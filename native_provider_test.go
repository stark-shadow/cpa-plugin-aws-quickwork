package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func raw(value any) []byte { result, _ := json.Marshal(value); return result }

func TestNativeLoginAndRefresh(t *testing.T) {
	for _, portal := range []string{personal, enterprise} {
		t.Run(portal, func(t *testing.T) {
			var engine engine
			engine.swap(&settings{Enabled: true, Timeout: time.Second})
			t.Cleanup(engine.stop)
			g := engine.generation
			authDir := t.TempDir()
			start, failure := g.startLogin(pluginapi.AuthLoginStartRequest{Provider: portal, Host: pluginapi.HostConfigSummary{AuthDir: authDir}})
			if failure != nil {
				t.Fatal(failure)
			}
			entry, _ := g.flows.Load(start.State)
			flow := entry.(*loginFlow)
			var countRegister, countExchange, countRefresh int
			oauth := oauthTransport{callbackID: "fixture", send: func(callback string, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				if callback != "fixture" || request.Method != http.MethodPost {
					t.Fatal("missing host callback context")
				}
				endpoint, _ := url.Parse(request.URL)
				var content any
				switch endpoint.Path {
				case "/client/register":
					countRegister++
					content = map[string]string{"clientId": "fixture-client", "clientSecret": "fixture-secret"}
				case "/token":
					fields := map[string]string{}
					if portal == personal {
						_ = json.Unmarshal(request.Body, &fields)
					} else {
						form, _ := url.ParseQuery(string(request.Body))
						for key := range form {
							fields[key] = form.Get(key)
						}
					}
					if fields["grantType"] == "refresh_token" || fields["grant_type"] == "refresh_token" {
						countRefresh++
						content = map[string]any{"access_token": "new-test-access", "refresh_token": "new-test-refresh", "expires_in": 3600}
					} else {
						countExchange++
						if fields["code"] != "fixture-code" || (fields["codeVerifier"] != flow.verifier && fields["code_verifier"] != flow.verifier) {
							t.Fatal("wrong code/PKCE")
						}
						content = map[string]any{"access_token": "old-test-access", "refresh_token": "old-test-refresh", "expires_in": 3600}
					}
				case "/integration/quick-work/user-info":
					if request.Headers.Get("Authorization") != "Bearer old-test-access" {
						t.Fatal("wrong user-info bearer")
					}
					content = map[string]string{"email": "fixture@example.invalid", "userArn": "arn:aws:quicksight:us-east-1:000000000000:user/default/test"}
				default:
					t.Fatalf("unexpected OAuth path: %s", endpoint.Path)
				}
				return pluginapi.HTTPResponse{StatusCode: 200, Body: raw(content)}, nil
			}}
			poll := pluginapi.AuthLoginPollRequest{Provider: portal, State: start.State}
			query := url.Values{"nonce": {flow.nonce}, "tenantUrl": {personalTenant}, "issuerUrl": {"https://example.awsapps.com/start"}}
			if portal == enterprise {
				query.Set("authEndpoint", "https://signin.aws.amazon.com/authorize")
				query.Set("tokenEndpoint", "https://signin.aws.amazon.com/token")
				query.Set("clientId", "fixture-client")
				body := func(query url.Values) []byte {
					return raw(map[string]string{"state": start.State, "redirect_url": flow.redirect + "/callback?" + query.Encode()})
				}
				query.Set("nonce", "wrong")
				if got := g.remoteDiscovery(pluginapi.ManagementRequest{Method: "POST", Body: body(query)}, oauth); got.StatusCode != 400 {
					t.Fatal(got.StatusCode)
				}
				query.Set("nonce", flow.nonce)
				if got := g.remoteDiscovery(pluginapi.ManagementRequest{Method: "POST", Body: body(query)}, oauth); got.StatusCode != 200 {
					t.Fatal(got.StatusCode)
				}
				if err := os.WriteFile(flow.callbackFile(), raw(map[string]string{"state": start.State, "code": "fixture-code"}), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				response := httptest.NewRecorder()
				flow.localCallback(response, httptest.NewRequest("GET", "/callback?"+query.Encode(), nil))
				if response.Code != 200 || response.Header().Get("Refresh") == "" {
					t.Fatal(response.Code)
				}
				if _, failure := g.pollLogin(poll, oauth); failure != nil {
					t.Fatal(failure)
				}
				response = httptest.NewRecorder()
				flow.localCallback(response, httptest.NewRequest("GET", "/continue?nonce="+flow.nonce, nil))
				if response.Code != 303 || !strings.Contains(response.Header().Get("Location"), "code_challenge=") {
					t.Fatal(response.Code)
				}
				response = httptest.NewRecorder()
				flow.localCallback(response, httptest.NewRequest("GET", "/?state="+start.State+"&code=fixture-code", nil))
				if response.Code != 200 || countRegister != 1 {
					t.Fatalf("local callback %d; registrations %d", response.Code, countRegister)
				}
			}
			completed, failure := g.pollLogin(poll, oauth)
			if failure != nil || completed.Status != pluginapi.AuthLoginStatusSuccess {
				t.Fatalf("poll %s: %v", completed.Status, failure)
			}
			if _, failure := g.pollLogin(poll, oauth); failure != nil || countExchange != 1 {
				t.Fatal("a duplicate poll repeated token exchange")
			}
			reply, failure := g.refresh(pluginapi.AuthRefreshRequest{AuthID: completed.Auth.ID, AuthProvider: id, StorageJSON: completed.Auth.StorageJSON}, oauth)
			if failure != nil {
				t.Fatal(failure)
			}
			saved, failure := readCredential(id, reply.Auth.StorageJSON)
			if failure != nil || saved.Refresh != "new-test-refresh" || saved.bearer() != "new-test-access" || countRefresh != 1 {
				t.Fatal("refresh did not persist rotated credentials")
			}
			if _, err := os.Stat(flow.callbackFile()); !os.IsNotExist(err) {
				t.Fatal("callback file was not cleared")
			}
		})
	}
}

func eventPacket(event []byte) []byte {
	data := raw(map[string]string{"payload": base64.StdEncoding.EncodeToString(event)})
	frame := make([]byte, 16+len(data))
	binary.BigEndian.PutUint32(frame, uint32(len(frame)))
	binary.BigEndian.PutUint32(frame[8:], crc32.ChecksumIEEE(frame[:8]))
	copy(frame[12:], data)
	binary.BigEndian.PutUint32(frame[len(frame)-4:], crc32.ChecksumIEEE(frame[:len(frame)-4]))
	return frame
}

func TestDirectWebSocketTurn(t *testing.T) {
	completed := make(chan []string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		var sequence []string
		for range 5 {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				t.Error(err)
				return
			}
			var wrapped struct{ Payload struct{ Payload string } }
			if json.Unmarshal(frame, &wrapped) != nil {
				t.Error("invalid outbound frame")
				return
			}
			data, _ := base64.StdEncoding.DecodeString(wrapped.Payload.Payload)
			var event chatEvent
			if json.Unmarshal(data, &event) != nil {
				t.Error("invalid outbound event")
				return
			}
			sequence = append(sequence, event.Type)
			if event.Type != "user_message" {
				continue
			}
			if !strings.Contains(string(event.Payload), "system_prompt") || !strings.Contains(string(event.Payload), "call_prior") {
				t.Error("message history/system prompt not forwarded")
			}
			for _, event := range []string{
				`{"type":"text_delta","payload":"checking <client_"}`,
				`{"type":"text_delta","payload":{"delta":"tool_call>{\"name\":\"lookup\",\"arguments\":{\"id\":42}}</client_tool_call>"}}`,
				`{"type":"turn_usage","payload":{"input_tokens":10,"output_tokens":2,"cache_read_tokens":3}}`,
				`{"type":"turn_completed","payload":{}}`,
			} {
				if err := conn.WriteMessage(websocket.BinaryMessage, eventPacket([]byte(event))); err != nil {
					t.Error(err)
					return
				}
			}
		}
		completed <- sequence
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	input, failure := prepareChat(pluginapi.ExecutorRequest{Model: id + "-smart", Format: "openai", SourceFormat: "openai", Payload: []byte(`{"messages":[{"role":"system","content":"Use concise answers"},{"role":"assistant","tool_calls":[{"id":"call_prior"}]},{"role":"tool","tool_call_id":"call_prior","content":"result"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`)})
	if failure != nil {
		t.Fatal(failure)
	}
	var text strings.Builder
	result, failure := performChat(t.Context(), conn, input, credential{Actor: "fixture"}, func(delta string) bool { text.WriteString(delta); return true })
	if failure != nil || len(result.Tools) != 1 || result.Tools[0].Function.Arguments != `{"id":42}` || text.String() != "checking " || result.Usage.Cached != 3 {
		t.Fatalf("unexpected result: %+v; %q; %v", result, text.String(), failure)
	}
	select {
	case sequence := <-completed:
		if !reflect.DeepEqual(sequence, []string{"session_connect", "federate_status", "session_create", "user_message", "session_delete"}) {
			t.Fatal(sequence)
		}
	case <-time.After(time.Second):
		t.Fatal("session was not deleted")
	}
}

func TestRejectLegacyAndTruncatedTurn(t *testing.T) {
	if _, failure := parseSettings([]byte("bridge_origins: [http://127.0.0.1]")); failure == nil {
		t.Fatal("old settings accepted")
	}
	if _, failure := readCredential(id, []byte(`{"type":"aws-quickwork","bridge_key":"old"}`)); failure == nil {
		t.Fatal("old key accepted")
	}
	_, failure := collectChat(t.Context(), chatInput{}, func() ([]byte, error) { return nil, io.EOF }, func(string) bool { return true }, nil)
	if failure == nil || failure.Code != pluginabi.ErrorCodeStreamTruncated {
		t.Fatal("truncated stream treated as complete")
	}
	frame := eventPacket([]byte(`{"type":"turn_completed"}`))
	frame[len(frame)-1] ^= 1
	if _, err := aidpOutput(frame); err == nil {
		t.Fatal("corrupt frame accepted")
	}
}

func TestQuotaIsExplicitlyUnavailableWithoutUpstreamCalls(t *testing.T) {
	var runtime engine
	runtime.swap(&settings{Enabled: true})
	t.Cleanup(runtime.stop)
	saved := credential{Type: id, Portal: personal, Tenant: personalTenant, TokenURL: "https://oidc.us-east-1.amazonaws.com/token", ClientID: "fixture", Access: "fixture", Expires: time.Now().Add(time.Hour), Actor: "fixture"}
	request := pluginapi.QuotaFetchRequest{AuthID: "fixture", AuthIndex: "fixture", Provider: id, StorageJSON: raw(saved)}
	value, failure := runtime.call(pluginabi.MethodQuotaFetch, raw(request), nil, func(string, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		t.Fatal("an unsupported quota lookup must not call upstream")
		return pluginapi.HTTPResponse{}, nil
	})
	if value != nil || failure == nil || failure.Code != "quota_unavailable" || !strings.Contains(failure.Message, "不代表额度为零") {
		t.Fatalf("unexpected quota response: %v, %v", value, failure)
	}
}

func TestBrowserCallbackPageUsesLanguageAndDoesNotExposeAuthorizationCode(t *testing.T) {
	flow := loginFlow{state: "known-state", nonce: "known-nonce", authorize: "https://quick.aws.com/sn", until: time.Now().Add(time.Minute)}
	for _, test := range []struct {
		path, language, expected string
		status                   int
	}{
		{"/?state=known-state&code=do-not-display", "zh-CN,zh;q=0.9", "已收到授权", 200},
		{"/?state=known-state&code=do-not-display", "en-US", "Authorization received", 200},
		{"/?state=known-state", "zh-CN", "此登录链接已失效", 400},
		{"/?state=known-state&code=first&code=second", "en-US", "sign-in link is no longer valid", 400},
	} {
		request := httptest.NewRequest("GET", test.path, nil)
		request.Header.Set("Accept-Language", test.language)
		response := httptest.NewRecorder()
		flow.localCallback(response, request)
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.expected) || strings.Contains(response.Body.String(), "do-not-display") || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unexpected callback status/body: %s (%d)", test.expected, response.Code)
		}
	}
}
