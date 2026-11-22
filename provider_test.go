package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func raw(v any) []byte { b, _ := json.Marshal(v); return b }

func TestConfigAndCredentialBoundary(t *testing.T) {
	good, fail := parseSettings([]byte("enabled: true\npriority: 2\nstore: {source: fixture}\nrequest_timeout: 7s\nbridge_origins: [http://127.0.0.1:1234/, 'https://FIXTURE.TEST:443']\n"))
	if fail != nil || good.Timeout != 7*time.Second || len(good.Origins) != 2 {
		t.Fatal("valid settings rejected")
	}
	for _, v := range []string{
		"bridge_origins: [http://fixture.test]", "bridge_origins: [https://u:p@fixture.test]",
		"bridge_origins: [https://fixture.test/v1]", "bridge_origins: ['https://fixture.test/?secret']",
		"bridge_origins: ['https://fixture.test#']", "bridge_origins: ['https://fixture.test:0']",
		"bridge_origins: ['https://fixture.test:', 'https://fixture.test:443']",
		"bridge_origins: ['https://fixture.test', 'https://FIXTURE.TEST:443/']",
		"bridge_origins: ['http://[::2]:80']", "bridge_origins: ['https://fixture.test/%2f']",
		"bridge_origins: true", "request_timeout: 0.5s", "request_timeout: 6m", "keys: []", "{}\n---\n{}",
	} {
		if _, err := parseSettings([]byte(v)); err == nil {
			t.Errorf("invalid settings accepted: %s", v)
		}
	}
	if _, ok := canonicalOrigin("http://[::1]:8080"); !ok {
		t.Fatal("IPv6 loopback rejected")
	}
	cred := credential{Type: id, Origin: "https://fixture.test/", Key: "fixture-key"}
	if c, err := good.parseCredential(raw(cred)); err != nil || c.Origin != "https://fixture.test" {
		t.Fatal("allowlisted credential rejected")
	}
	for _, bad := range []credential{
		{id, "https://other.test", "fixture-key"}, {id, "https://fixture.test", "x\r\ny"},
		{id, "https://fixture.test", ""}, {"other", cred.Origin, cred.Key},
	} {
		if _, err := good.parseCredential(raw(bad)); err == nil || strings.Contains(err.Message, "fixture-key") {
			t.Fatal("unsafe credential handling")
		}
	}
	if auth, err := good.parseAuth(pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"unrelated"}`)}); err != nil || auth.Handled {
		t.Fatal("foreign credential claimed")
	}
}

func TestQuotaIsUnknownAndVerifiesIdentity(t *testing.T) {
	cfg, fail := parseSettings([]byte("bridge_origins: [https://fixture.test]"))
	if fail != nil {
		t.Fatal(fail)
	}
	req := pluginapi.QuotaFetchRequest{Provider: id, AuthIndex: "index-a", AuthID: "a.json", StorageJSON: raw(credential{id, "https://fixture.test", "fixture-key"})}
	result, err := cfg.fetchQuota(req)
	if err != nil || result.Subscription != nil || len(result.Summary) != 0 || len(result.Groups) != 1 || len(result.Groups[0].Buckets) != 0 || !strings.Contains(result.Groups[0].DisplayName, "unknown") {
		t.Fatal("unknown must not expose a zero, total or reset")
	}
	for index, check := range []func(*pluginapi.QuotaFetchRequest){
		func(q *pluginapi.QuotaFetchRequest) { q.Provider = "another" },
		func(q *pluginapi.QuotaFetchRequest) { q.AuthID = "" },
		func(q *pluginapi.QuotaFetchRequest) { q.AuthIndex = "" },
		func(q *pluginapi.QuotaFetchRequest) { q.StorageJSON = nil },
		func(q *pluginapi.QuotaFetchRequest) {
			q.StorageJSON = raw(credential{id, "https://not-allowed.test", "fixture-key"})
		},
	} {
		q := req
		check(&q)
		if _, e := cfg.fetchQuota(q); e == nil {
			t.Errorf("bad identity %d accepted", index)
		}
	}
}

func TestRateLimitIsNotCredentialQuota(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 500000000, time.UTC)
	for in, want := range map[string]int{"12": 12, "bad": 0, "-2": 0, "9999999": 86400, now.Add(3 * time.Second).Format(http.TimeFormat): 3, now.Add(-time.Second).Format(http.TimeFormat): 0} {
		if got := retrySeconds(in, now); got != want {
			t.Errorf("delay %s: got %d want %d", in, got, want)
		}
	}
	h := http.Header{"Retry-After": {"12"}, "Authorization": {"private-fixture"}}
	e := classifyStatus(429, h)
	if e.Code != pluginabi.ErrorCodeTransientRateLimit || e.IsCredentialScoped() || e.HTTPStatus != 429 || e.RetryAfterSeconds != 12 || strings.Contains(e.Error(), "private-fixture") {
		t.Fatal("unsafe rate limit classification")
	}
	if e := classifyStatus(502, h); e.HTTPStatus != 502 || e.RetryAfterSeconds != 0 {
		t.Fatal("inferred quota from a bridge failure")
	}
}

func rpcOptions(origin string) []byte {
	return raw(map[string]any{"schema_version": pluginabi.SchemaVersion, "config_yaml": []byte(fmt.Sprintf("bridge_origins: [%s]\nrequest_timeout: 3s\n", origin))})
}

func requestFor(origin string) pluginapi.ExecutorRequest {
	return pluginapi.ExecutorRequest{AuthID: "fixture.json", AuthProvider: id, Model: id + "-smart", Format: "openai", SourceFormat: "openai",
		StorageJSON: raw(credential{id, origin, "fixture-key"}),
		Payload:     []byte(`{"model":"public-alias","messages":[{"role":"assistant","tool_calls":[{"id":"call_fixture"}]},{"role":"tool","tool_call_id":"call_fixture","content":"42"}]}`),
		Headers:     http.Header{"Authorization": {"untrusted"}, "X-Thread-Id": {"foreign-thread"}},
	}
}

func TestNoRedirectOrCallerCredentialForwarding(t *testing.T) {
	var count atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { count.Add(1) }))
	defer other.Close()
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-key" || r.Header.Get("X-Thread-Id") != "" || r.URL.Path != chatPath {
			t.Error("incorrect routing or forwarded headers")
		}
		http.Redirect(w, r, other.URL, 307)
	}))
	defer bridge.Close()
	var eng engine
	defer eng.stop()
	if _, err := eng.call(pluginabi.MethodPluginRegister, rpcOptions(bridge.URL), nil); err != nil {
		t.Fatal(err)
	}
	request := requestFor(bridge.URL)
	_, err := eng.call(pluginabi.MethodExecutorExecute, raw(request), nil)
	if err == nil || err.HTTPStatus != 502 || count.Load() != 0 {
		t.Fatal("redirect escaped the authorized bridge")
	}
	request.SourceFormat = "openai-response"
	if _, err := eng.call(pluginabi.MethodExecutorExecute, raw(request), nil); err == nil || err.HTTPStatus != 400 {
		t.Fatal("unsupported source format accepted")
	}
}

func TestSSEHandlesToolsUsageAndFailure(t *testing.T) {
	event := `{"object":"chat.completion.chunk","choices":[{"delta":{"tool_calls":[{"id":"call_fixture","function":{"arguments":"{\"number\":7}"}}]}}]}`
	input := "\ufeff: heartbeat\r\n\r\ndata: " + event + "\r\n\r\ndata: {\r\ndata: \"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"total_tokens\":12}}\r\n\r\ndata: [DONE]\r\n\r\n"
	var chunks [][]byte
	if e := pumpSSE(t.Context(), strings.NewReader(input), id+"-fast", func(b []byte) bool { chunks = append(chunks, b); return true }); e != nil {
		t.Fatal(e)
	}
	if len(chunks) != 2 || !bytes.Contains(chunks[0], []byte(`"id":"call_fixture"`)) || !bytes.Contains(chunks[1], []byte(`"total_tokens":12`)) || !bytes.Contains(chunks[0], []byte(id+"-fast")) {
		t.Fatal("stream lost tool, model or usage")
	}
	for _, bad := range []string{
		"data: [DONE]\n\n", "data: " + event + "\n\n", "data: {\"error\":\"private-fixture\"}\n\ndata: [DONE]\n\n",
		"data: " + strings.Repeat("x", maxEvent) + "\n\n", "data: {\"object\":\"chat.completion.chunk\",\"choices\":null}\n\n",
	} {
		if e := pumpSSE(t.Context(), strings.NewReader(bad), id+"-fast", func([]byte) bool { return true }); e == nil || e.HTTPStatus != 502 || strings.Contains(e.Error(), "private-fixture") {
			t.Fatal("broken stream was not safely rejected")
		}
	}
	if e := pumpSSE(t.Context(), strings.NewReader(input), id+"-fast", func([]byte) bool { return false }); e != nil {
		t.Fatal("canceled host should stop stream")
	}
}

func TestShutdownCancelsActiveStreamingGeneration(t *testing.T) {
	disconnected := make(chan struct{})
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(disconnected)
		case <-time.After(5 * time.Second):
			t.Error("request kept alive after shutdown")
		}
	}))
	defer bridge.Close()
	var eng engine
	if _, e := eng.call(pluginabi.MethodPluginRegister, rpcOptions(bridge.URL), nil); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(eng.stop)
	req := struct {
		pluginapi.ExecutorRequest
		StreamID string `json:"stream_id"`
	}{requestFor(bridge.URL), "fixture-stream"}
	result := make(chan streamEvent, 1)
	if _, e := eng.call(pluginabi.MethodExecutorExecuteStream, raw(req), func(method string, value any) bool {
		if method == pluginabi.MethodHostStreamClose {
			result <- value.(streamEvent)
		}
		return true
	}); e != nil {
		t.Fatal(e)
	}
	if _, err := eng.call(pluginabi.MethodPluginReconfigure, rpcOptions(bridge.URL), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-disconnected:
		t.Fatal("unchanged reconfiguration canceled the stream")
	default:
	}
	eng.stop()
	select {
	case event := <-result:
		if event.Failure == nil || event.StreamID != "fixture-stream" {
			t.Fatal("missing structured cancellation")
		}
	default:
		t.Fatal("shutdown returned before the worker")
	}
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("upstream stream not canceled")
	}
}

func TestManifestMatchesABI(t *testing.T) {
	var m struct {
		ID, Version string
		ABI         uint32 `json:"abi_version"`
		Schema      uint32 `json:"schema_version"`
	}
	b, e := os.ReadFile("manifest.json")
	if e != nil || json.Unmarshal(b, &m) != nil || m.ID != id || m.Version != version || m.ABI != pluginabi.ABIVersion || m.Schema != pluginabi.SchemaVersion {
		t.Fatal("manifest mismatch")
	}
}

func TestUnaryDeadline(t *testing.T) {
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read headers of a valid response but never complete the body.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
			t.Error("deadline was not propagated")
		}
	}))
	defer bridge.Close()
	cfg, err := parseSettings([]byte("bridge_origins: [" + bridge.URL + "]"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Timeout = 250 * time.Millisecond
	g := generation{settings: cfg, ctx: context.Background(), client: bridgeClient()}
	defer g.client.CloseIdleConnections()
	if _, e := g.execute(requestFor(bridge.URL)); e == nil || e.HTTPStatus != 504 {
		t.Fatal("deadline did not produce a safe error")
	}
}
