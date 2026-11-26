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
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func raw(value any) []byte { result, _ := json.Marshal(value); return result }

func makeTestJWT(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString(raw(claims))
	return header + "." + payload + ".sig"
}

func TestNativeLoginAndRefresh(t *testing.T) {
	for _, portal := range []string{personal, enterprise} {
		t.Run(portal, func(t *testing.T) {
			var eng engine
			cfg := defaultSettings()
			cfg.Timeout = time.Second
			eng.swap(&cfg)
			t.Cleanup(eng.stop)
			g := eng.generation
			authDir := t.TempDir()
			start, failure := g.startLogin(pluginapi.AuthLoginStartRequest{Provider: portal, Host: pluginapi.HostConfigSummary{AuthDir: authDir}})
			if failure != nil {
				t.Fatal(failure)
			}
			entry, _ := g.flows.Load(start.State)
			flow := entry.(*loginFlow)
			initialIDToken := makeTestJWT(map[string]any{"exp": time.Now().Add(2 * time.Hour).Unix(), "sub": "arn:aws:quicksight:us-east-1:123456789012:user/default/jwt-fallback"})
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
						// Deliberately omit id_token to verify refresh preserves the previous id_token.
						content = map[string]any{"access_token": "new-test-access", "refresh_token": "new-test-refresh", "expires_in": 3600}
					} else {
						countExchange++
						if fields["code"] != "fixture-code" || (fields["codeVerifier"] != flow.verifier && fields["code_verifier"] != flow.verifier) {
							t.Fatal("wrong code/PKCE")
						}
						content = map[string]any{"access_token": "old-test-access", "id_token": initialIDToken, "refresh_token": "old-test-refresh", "expires_in": 3600}
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
			// Inject an extra CPA-managed field (like excluded_models) into StorageJSON before refresh.
			var storageMap map[string]any
			_ = json.Unmarshal(completed.Auth.StorageJSON, &storageMap)
			storageMap["excluded_models"] = []string{"aws-quickwork-auto"}
			reply, failure := g.refresh(pluginapi.AuthRefreshRequest{AuthID: completed.Auth.ID, AuthProvider: id, StorageJSON: raw(storageMap)}, oauth)
			if failure != nil {
				t.Fatal(failure)
			}
			saved, failure := readCredential(id, reply.Auth.StorageJSON)
			if failure != nil || saved.Refresh != "new-test-refresh" || saved.Access != "new-test-access" || saved.Identity != initialIDToken || saved.bearer() != initialIDToken || countRefresh != 1 {
				t.Fatalf("refresh did not persist rotated credentials and preserved id_token: %+v", saved)
			}
			var refreshedMap map[string]any
			if err := json.Unmarshal(reply.Auth.StorageJSON, &refreshedMap); err != nil || refreshedMap["excluded_models"] == nil {
				t.Fatalf("refresh dropped CPA-managed excluded_models field: %s", string(reply.Auth.StorageJSON))
			}
			if _, err := os.Stat(flow.callbackFile()); !os.IsNotExist(err) {
				t.Fatal("callback file was not cleared")
			}
		})
	}
}

func TestTokenExpiryUsesEarlierJWTExpAndConservativeFallback(t *testing.T) {
	earlierExp := time.Now().Add(15 * time.Minute).Unix()
	shortID := makeTestJWT(map[string]any{"exp": earlierExp})
	cred := credential{
		Type:     id,
		Portal:   personal,
		Tenant:   personalTenant,
		TokenURL: "https://oidc.us-east-1.amazonaws.com/token",
		ClientID: "fixture",
		Refresh:  "r1",
	}
	transport := oauthTransport{callbackID: "cb", send: func(_ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{
			StatusCode: 200,
			Body:       raw(map[string]any{"access_token": "a1", "id_token": shortID, "expires_in": 7200}),
		}, nil
	}}
	if failure := cred.tokens(transport, map[string]string{}); failure != nil {
		t.Fatal(failure)
	}
	if cred.Expires.Unix() != earlierExp {
		t.Fatalf("expected earlier JWT exp %d, got %d", earlierExp, cred.Expires.Unix())
	}

	// Missing expires_in and non-JWT token falls back conservatively to ~50m.
	cred2 := credential{
		Type:     id,
		Portal:   personal,
		Tenant:   personalTenant,
		TokenURL: "https://oidc.us-east-1.amazonaws.com/token",
		ClientID: "fixture",
		Refresh:  "r2",
	}
	transport2 := oauthTransport{callbackID: "cb", send: func(_ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{
			StatusCode: 200,
			Body:       raw(map[string]any{"access_token": "opaque-token"}),
		}, nil
	}}
	if failure := cred2.tokens(transport2, map[string]string{}); failure != nil {
		t.Fatal(failure)
	}
	remaining := time.Until(cred2.Expires)
	if remaining < 45*time.Minute || remaining > 55*time.Minute {
		t.Fatalf("expected ~50m conservative expiry fallback, got %v", remaining)
	}
}

func eventPacket(event []byte) []byte {
	return eventPacketWithHeaders(nil, raw(map[string]string{"payload": base64.StdEncoding.EncodeToString(event)}))
}

func eventPacketWithHeaders(headers map[string]string, payload []byte) []byte {
	var hb []byte
	for k, v := range headers {
		hb = append(hb, byte(len(k)))
		hb = append(hb, k...)
		hb = append(hb, hvString, byte(len(v)>>8), byte(len(v)))
		hb = append(hb, v...)
	}
	total := 16 + len(hb) + len(payload)
	frame := make([]byte, total)
	binary.BigEndian.PutUint32(frame[0:4], uint32(total))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(hb)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	copy(frame[12:], hb)
	copy(frame[12+len(hb):], payload)
	binary.BigEndian.PutUint32(frame[total-4:], crc32.ChecksumIEEE(frame[:total-4]))
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
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Error(err)
				return
			}
			if _, hasSystem := payload["system_prompt"]; hasSystem {
				t.Error("deprecated system_prompt field must not be sent")
			}
			textField, _ := payload["text"].(string)
			if !strings.Contains(textField, "[System instructions from the API caller]") ||
				!strings.Contains(textField, "Use concise answers") ||
				!strings.Contains(textField, "<caller_tool_interface>") ||
				!strings.Contains(textField, "call_prior") {
				t.Errorf("caller instructions or tool history missing from relayed user text: %q", textField)
			}
			if payload["thinking_effort"] != "medium" || payload["chat_agent_id"] != "agent-custom" {
				t.Errorf("thinking_effort or chat_agent_id not forwarded: %+v", payload)
			}
			// Pack two event-stream messages into a single WebSocket binary frame to test multi-message decoding.
			combined := append(
				eventPacket([]byte(`{"type":"text_delta","payload":"checking <client_"}`)),
				eventPacket([]byte(`{"type":"text_delta","payload":{"delta":"tool_call>{\"name\":\"lookup\",\"arguments\":{\"id\":42}}</client_tool_call>"}}`))...,
			)
			if err := conn.WriteMessage(websocket.BinaryMessage, combined); err != nil {
				t.Error(err)
				return
			}
			for _, ev := range []string{
				`{"type":"turn_usage","payload":{"input_tokens":10,"output_tokens":2,"cache_read_tokens":3,"cache_write_tokens":4}}`,
				`{"type":"turn_completed","payload":{}}`,
			} {
				if err := conn.WriteMessage(websocket.BinaryMessage, eventPacket([]byte(ev))); err != nil {
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
	input, failure := prepareChat(pluginapi.ExecutorRequest{
		Model:        id + "-smart",
		Format:       "openai",
		SourceFormat: "openai",
		Payload:      []byte(`{"messages":[{"role":"system","content":"Use concise answers"},{"role":"assistant","tool_calls":[{"id":"call_prior","function":{"name":"lookup","arguments":"{\"id\":1}"}}]},{"role":"tool","tool_call_id":"call_prior","content":"result"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`),
	})
	if failure != nil {
		t.Fatal(failure)
	}
	var text strings.Builder
	opts := chatOptions{timezone: "UTC", locale: "en-US", thinkingEffort: "medium", chatAgentID: "agent-custom"}
	result, failure := performChat(t.Context(), conn, input, credential{Actor: "fixture"}, opts, func(delta string) bool { text.WriteString(delta); return true })
	if failure != nil || len(result.Tools) != 1 || result.Tools[0].Function.Name != "lookup" || result.Tools[0].Function.Arguments != `{"id":42}` || text.String() != "checking" || result.Usage.Cached != 3 || result.Usage.CacheWrite != 4 {
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

func TestToolBridgeHardeningAndNativeProgressFallback(t *testing.T) {
	allowed := map[string]bool{"read_file": true, "run_cmd": true}

	// Fenced JSON with alternate key "parameters", namespace prefix, and parallel array.
	fenced := "Let me check.\n```json\n[{\"name\":\"functions.read_file\",\"parameters\":{\"path\":\"a.go\"}},{\"tool\":\"mcp/run_cmd\",\"input\":{\"cmd\":\"ls\"}}]\n```"
	clean, calls := parseClientToolCalls(fenced, allowed)
	if clean != "Let me check." || len(calls) != 2 || calls[0].Function.Name != "read_file" || calls[0].Function.Arguments != `{"path":"a.go"}` || calls[1].Function.Name != "run_cmd" {
		t.Fatalf("fenced parse failed: clean=%q calls=%+v", clean, calls)
	}

	// Unknown tool tag must remain untouched as ordinary prose.
	unknown := "See <tool_call>{\"name\":\"not_declared\",\"arguments\":{}}</tool_call> for details."
	clean, calls = parseClientToolCalls(unknown, allowed)
	if clean != unknown || len(calls) != 0 {
		t.Fatalf("unknown tool altered prose: clean=%q calls=%+v", clean, calls)
	}

	// Native tool_start + tool_progress fallback when text contains no markers.
	frames := [][]byte{
		eventPacket([]byte(`{"type":"tool_start","payload":{"tool_id":"t1","tool_name":"functions.read_file"}}`)),
		eventPacket([]byte(`{"type":"tool_progress","payload":{"tool_id":"t1","input_delta":"{\"path\":"}}`)),
		eventPacket([]byte(`{"type":"tool_progress","payload":{"tool_id":"t1","progress":{"input_delta":"\"README.md\"}"}}}`)),
		eventPacket([]byte(`{"type":"turn_completed","payload":{}}`)),
	}
	idx := 0
	input := chatInput{bridgeActive: true, tools: allowed, choiceMode: "auto", parallel: true}
	res, failure := collectChat(t.Context(), input, func() ([]byte, error) {
		if idx >= len(frames) {
			return nil, io.EOF
		}
		f := frames[idx]
		idx++
		return f, nil
	}, func(string) bool { return true }, nil, nil)
	if failure != nil || len(res.Tools) != 1 || res.Tools[0].Function.Name != "read_file" || res.Tools[0].Function.Arguments != `{"path":"README.md"}` {
		t.Fatalf("native tool_progress fallback failed: res=%+v err=%v", res, failure)
	}

	// Zero-width/whitespace-only response must fail as empty_response.
	emptyFrames := [][]byte{
		eventPacket([]byte(`{"type":"text_delta","payload":" \u200b\u200c\n\t "}`)),
		eventPacket([]byte(`{"type":"turn_usage","payload":{"output_tokens":5}}`)),
		eventPacket([]byte(`{"type":"turn_completed","payload":{}}`)),
	}
	idx = 0
	_, failure = collectChat(t.Context(), chatInput{}, func() ([]byte, error) {
		if idx >= len(emptyFrames) {
			return nil, io.EOF
		}
		f := emptyFrames[idx]
		idx++
		return f, nil
	}, func(string) bool { return true }, nil, nil)
	if failure == nil || failure.Code != "empty_response" || !strings.Contains(failure.Message, "completion_tokens=5") {
		t.Fatalf("expected empty_response error for zero-width output, got %v", failure)
	}
}

func TestOversizedPromptAttachmentAndFallbackTrimming(t *testing.T) {
	origInterval := attachmentPollInterval
	attachmentPollInterval = time.Millisecond
	t.Cleanup(func() { attachmentPollInterval = origInterval })

	dlpCalls := 0
	transport := oauthTransport{callbackID: "cb", send: func(_ string, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		u, _ := url.Parse(req.URL)
		switch u.Path {
		case "/integration/quick-work/upload-attachment":
			if req.Headers.Get("Origin") != "https://quick.aws.com" || req.Headers.Get("Content-Type") != "application/octet-stream" {
				t.Fatalf("unexpected upload headers: %v", req.Headers)
			}
			return pluginapi.HTTPResponse{StatusCode: 200, Body: raw(map[string]any{"attachmentId": "att-123"})}, nil
		case "/integration/quick-work/attachment-dlp-status":
			dlpCalls++
			if dlpCalls == 1 {
				return pluginapi.HTTPResponse{StatusCode: 200, Body: raw(map[string]any{"dlpJobStatus": "IN_PROGRESS"})}, nil
			}
			return pluginapi.HTTPResponse{StatusCode: 200, Body: raw(map[string]any{"dlpJobStatus": "COMPLETED", "policyAction": "ALLOW"})}, nil
		case "/integration/quick-work/qbs-event":
			innerBody := string(raw(map[string]any{"files": []map[string]string{{"path": "/workspace/prompt-123.txt"}}}))
			apiResp := string(raw(map[string]any{"type": "api_response", "body": innerBody}))
			return pluginapi.HTTPResponse{StatusCode: 200, Body: raw(map[string]any{"eventResponse": map[string]any{"invokeAgentResponse": apiResp}})}, nil
		default:
			t.Fatalf("unexpected attachment path: %s", u.Path)
			return pluginapi.HTTPResponse{StatusCode: 404}, nil
		}
	}}

	bigPrompt := strings.Repeat("hello world! ", 6000) // ~78 KB > 63 KB
	ref, err := uploadPromptAttachment(t.Context(), personalTenant, "jwt-token", "thread-1", "prompt-12345678.txt", []byte(bigPrompt), transport, "")
	if err != nil || ref.ID != "att-123" || ref.Path != "/workspace/prompt-123.txt" || dlpCalls != 2 {
		t.Fatalf("attachment upload failed: ref=%+v dlpCalls=%d err=%v", ref, dlpCalls, err)
	}

	// Test UTF-8-safe middle trimming when attachment upload is unavailable.
	utf8Payload := map[string]any{"text": strings.Repeat("你好世界", 10000)} // 120 KB
	if err := fitUserMessage(utf8Payload); err != nil {
		t.Fatalf("fitUserMessage failed: %v", err)
	}
	trimmedSize, err := userMessageBytes(utf8Payload)
	if err != nil || trimmedSize > maxUserMessageBytes {
		t.Fatalf("trimmed payload still oversized: %d > %d (err=%v)", trimmedSize, maxUserMessageBytes, err)
	}
	if !strings.Contains(utf8Payload["text"].(string), textTrimMarker) {
		t.Fatal("trim marker missing from trimmed payload")
	}
}

func TestSessionReuseAndMultiAccountIsolation(t *testing.T) {
	store := newConversationStore(30*time.Minute, 16)
	m1 := []chatMessage{{Role: "user", Content: "first question"}}
	m2 := []chatMessage{{Role: "user", Content: "first question"}, {Role: "assistant", Content: "first answer"}, {Role: "user", Content: "follow up"}}

	sys := buildSystemPrompt(m1)
	keyA := conversationKey("auth-A", nil, m1, sys)
	keyA2 := conversationKey("auth-A", nil, m2, sys)
	keyB := conversationKey("auth-B", nil, m2, sys)
	if keyA != keyA2 {
		t.Fatal("conversation key changed when messages grew")
	}
	if keyA == keyB {
		t.Fatal("different CPA credentials collided on conversation key")
	}

	conv := store.acquire(keyA)
	conv.threadID = "thread-A"
	conv.authID = "auth-A"
	conv.messages = append([]chatMessage(nil), m1...)
	conv.systemPrompt = sys
	store.release(conv)

	conv2 := store.acquire(keyA2)
	delta, ok := conv2.delta(m2)
	store.release(conv2)
	if !ok || len(delta) != 2 {
		t.Fatalf("expected 2 delta messages on reuse, got ok=%v len=%d", ok, len(delta))
	}
	deltaPrompt, err := formatPrompt(delta)
	if err != nil || !strings.Contains(deltaPrompt, "follow up") || strings.Contains(deltaPrompt, "first question") {
		t.Fatalf("unexpected delta prompt: %q (err=%v)", deltaPrompt, err)
	}
}

func TestOfficialQuotaFetchAndErrorStates(t *testing.T) {
	var rt engine
	cfg := defaultSettings()
	rt.swap(&cfg)
	t.Cleanup(rt.stop)
	saved := credential{
		Type:     id,
		Portal:   personal,
		Tenant:   personalTenant,
		TokenURL: "https://oidc.us-east-1.amazonaws.com/token",
		ClientID: "fixture",
		Access:   "fixture-jwt",
		Expires:  time.Now().Add(time.Hour),
		Actor:    "fixture-actor",
	}
	req := pluginapi.QuotaFetchRequest{AuthID: "auth-1", AuthIndex: "idx-1", Provider: id, StorageJSON: raw(saved)}

	t.Run("success_json_envelope", func(t *testing.T) {
		usageJSON := `{"monthly":{"used":25,"limit":100,"resets_at":1800000000,"warning":{"severity":"info"}},"overage_monthly":{"used":5,"resets_at":1800000000},"subscription":{"subscription_type":"PRO"}}`
		apiResp := raw(map[string]any{"type": "api_response", "status": 200, "body": usageJSON, "body_encoding": "json"})
		outer := raw(map[string]any{"eventResponse": map[string]any{"invokeAgentResponse": string(apiResp)}})
		value, failure := rt.call(pluginabi.MethodQuotaFetch, raw(map[string]any{
			"auth_index":       req.AuthIndex,
			"auth_id":          req.AuthID,
			"provider":         req.Provider,
			"storage_json":     req.StorageJSON,
			"host_callback_id": "cb-quota",
		}), nil, func(_ string, httpReq pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			if httpReq.Method != http.MethodPost || !strings.HasSuffix(httpReq.URL, "/integration/quick-work/qbs-event") {
				t.Fatalf("unexpected quota request: %s %s", httpReq.Method, httpReq.URL)
			}
			if httpReq.Headers.Get("Authorization") != "Bearer fixture-jwt" || httpReq.Headers.Get("x-amzn-qs-internal-trace-id") == "" {
				t.Fatalf("missing quota auth or trace headers: %v", httpReq.Headers)
			}
			return pluginapi.HTTPResponse{StatusCode: 200, Body: outer}, nil
		})
		if failure != nil {
			t.Fatalf("quota fetch failed: %v", failure)
		}
		resp := value.(pluginapi.QuotaFetchResponse)
		if resp.Subscription == nil || resp.Subscription.Plan != "PRO" {
			t.Fatalf("unexpected subscription: %+v", resp.Subscription)
		}
		if len(resp.Groups) != 1 || len(resp.Groups[0].Buckets) != 1 {
			t.Fatalf("expected 1 capped monthly bucket without fabricating a bar for uncapped overage, got %+v", resp.Groups)
		}
		monthly := resp.Groups[0].Buckets[0]
		if monthly.RemainingFraction != 0.75 || monthly.Description != "75 / 100 min left [info]" {
			t.Fatalf("unexpected monthly bucket: %+v", monthly)
		}
		if len(resp.Summary) != 1 || resp.Summary[0].Key != "overage_monthly_used" || resp.Summary[0].Value != 5 {
			t.Fatalf("expected only uncapped overage metric in summary (no duplicate capped monthly rows), got %+v", resp.Summary)
		}
	})

	t.Run("success_base64_body", func(t *testing.T) {
		usageB64 := base64.StdEncoding.EncodeToString([]byte(`{"monthly":{"used":10,"limit":40}}`))
		apiResp := raw(map[string]any{"type": "api_response", "status": 200, "body": usageB64, "body_encoding": "base64"})
		snap, err := parseUsageSnapshot(apiResp)
		if err != nil || snap.Monthly == nil || *snap.Monthly.Used != 10 || *snap.Monthly.Limit != 40 {
			t.Fatalf("base64 usage parse failed: snap=%+v err=%v", snap, err)
		}
	})

	t.Run("unavailable_404", func(t *testing.T) {
		_, failure := rt.call(pluginabi.MethodQuotaFetch, raw(map[string]any{
			"auth_index":       req.AuthIndex,
			"auth_id":          req.AuthID,
			"provider":         req.Provider,
			"storage_json":     req.StorageJSON,
			"host_callback_id": "cb-quota",
		}), nil, func(_ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			return pluginapi.HTTPResponse{StatusCode: 404, Body: []byte(`{}`)}, nil
		})
		if failure == nil || failure.Code != "quota_unavailable" || failure.HTTPStatus != 501 || !strings.Contains(failure.Message, "不代表额度为零") {
			t.Fatalf("expected 501 quota_unavailable, got %v", failure)
		}
	})

	t.Run("rate_limited_429", func(t *testing.T) {
		_, failure := rt.call(pluginabi.MethodQuotaFetch, raw(map[string]any{
			"auth_index":       req.AuthIndex,
			"auth_id":          req.AuthID,
			"provider":         req.Provider,
			"storage_json":     req.StorageJSON,
			"host_callback_id": "cb-quota",
		}), nil, func(_ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			return pluginapi.HTTPResponse{StatusCode: 429, Headers: http.Header{"Retry-After": {"42"}}, Body: []byte(`{}`)}, nil
		})
		if failure == nil || failure.Code != pluginabi.ErrorCodeTransientRateLimit || failure.HTTPStatus != 429 || failure.RetryAfterSeconds != 42 {
			t.Fatalf("expected 429 with retry-after 42, got %+v", failure)
		}
	})
}

func TestWireErrorAndClassification(t *testing.T) {
	errFrame := eventPacketWithHeaders(map[string]string{
		":message-type":  "exception",
		":error-code":    "ThrottlingException",
		":error-message": "Rate exceeded",
	}, []byte(`{"message":"Rate exceeded"}`))
	_, err := aidpOutput(errFrame)
	if err == nil || !strings.Contains(err.Error(), "ThrottlingException") || !strings.Contains(err.Error(), "Rate exceeded") {
		t.Fatalf("expected structured wireError, got %v", err)
	}

	if got := classifyUpstreamError("InsufficientCreditsException", 0, "out of credits"); got.Code != pluginabi.ErrorCodeCredentialQuota || !got.CredentialScoped || got.HTTPStatus != 429 {
		t.Fatalf("unexpected quota classification: %+v", got)
	}
	if got := classifyUpstreamError("FrameTooLarge", 0, "frame > 64KB"); got.Code != pluginabi.ErrorCodeRequestScoped || got.HTTPStatus != 400 {
		t.Fatalf("unexpected frame-too-large classification: %+v", got)
	}
	if got := classifyUpstreamError("ExpiredTokenException", 0, "expired"); got.Code != "reauth_required" || got.HTTPStatus != 401 {
		t.Fatalf("unexpected expired-token classification: %+v", got)
	}
	if got := classifyUpstreamError("SessionNotFound", 0, "missing thread"); got.Code != errCodeSessionNotFound {
		t.Fatalf("unexpected session-not-found classification: %+v", got)
	}
}

func TestConfigAndProxyValidation(t *testing.T) {
	if _, failure := parseSettings([]byte("bridge_origins: [http://127.0.0.1]")); failure == nil {
		t.Fatal("old settings accepted")
	}
	cfg, failure := parseSettings([]byte(`
enabled: true
request_timeout: 6m
session_reuse: false
session_ttl: 45m
keep_sessions: true
thinking_effort: high
chat_agent_id: custom-agent
timezone: Asia/Shanghai
locale: zh-CN
proxy_url: socks5://user:pass@127.0.0.1:1080
`))
	if failure != nil {
		t.Fatalf("valid settings rejected: %v", failure)
	}
	if cfg.Timeout != 6*time.Minute || cfg.SessionReuse || cfg.SessionTTL != 45*time.Minute || !cfg.KeepSessions || cfg.ThinkingEffort != "high" || cfg.ChatAgentID != "custom-agent" || cfg.Timezone != "Asia/Shanghai" || cfg.Locale != "zh-CN" || cfg.ProxyURL != "socks5://user:pass@127.0.0.1:1080" {
		t.Fatalf("unexpected parsed settings: %+v", cfg)
	}
	for _, valid := range []string{"", "direct", "http://127.0.0.1:8080", "https://proxy.example.com:8443", "socks5://127.0.0.1:1080", "socks5h://u:p@127.0.0.1:1080"} {
		if _, err := newWebSocketDialer(valid); err != nil {
			t.Fatalf("expected valid proxy %q, got %v", valid, err)
		}
	}
	for _, invalid := range []string{"ftp://127.0.0.1:21", "http://127.0.0.1:99999", "http://127.0.0.1:8080/path", "socks5://" + strings.Repeat("a", 256) + ":p@127.0.0.1:1080"} {
		if err := validateProxySetting(invalid); err == nil {
			t.Fatalf("expected invalid proxy %q to fail", invalid)
		}
	}
}

func TestRejectLegacyAndTruncatedTurn(t *testing.T) {
	if _, failure := readCredential(id, []byte(`{"type":"aws-quickwork","bridge_key":"old"}`)); failure == nil {
		t.Fatal("old key accepted")
	}
	_, failure := collectChat(t.Context(), chatInput{}, func() ([]byte, error) { return nil, io.EOF }, func(string) bool { return true }, nil, nil)
	if failure == nil || failure.Code != pluginabi.ErrorCodeStreamTruncated {
		t.Fatal("truncated stream treated as complete")
	}
	frame := eventPacket([]byte(`{"type":"turn_completed"}`))
	frame[len(frame)-1] ^= 1
	if _, err := aidpOutput(frame); err == nil {
		t.Fatal("corrupt frame accepted")
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

func TestSessionReuseEndToEndRecovery(t *testing.T) {
	var mu sync.Mutex
	var turns []struct {
		created bool
		thread  string
		text    string
	}
	turnIndex := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		var created bool
		for {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var wrapped struct{ Payload struct{ Payload string } }
			if json.Unmarshal(frame, &wrapped) != nil {
				return
			}
			data, _ := base64.StdEncoding.DecodeString(wrapped.Payload.Payload)
			var event chatEvent
			if json.Unmarshal(data, &event) != nil {
				return
			}
			switch event.Type {
			case "session_create":
				created = true
			case "user_message":
				var payload struct {
					ThreadID string `json:"thread_id"`
					Text     string `json:"text"`
				}
				_ = json.Unmarshal(event.Payload, &payload)
				mu.Lock()
				turnIndex++
				current := turnIndex
				turns = append(turns, struct {
					created bool
					thread  string
					text    string
				}{created: created, thread: payload.ThreadID, text: payload.Text})
				mu.Unlock()
				if current == 2 {
					_ = conn.WriteMessage(websocket.BinaryMessage, eventPacket([]byte(`{"type":"error","code":"SessionNotFound","payload":{"message":"expired thread"}}`)))
					return
				}
				_ = conn.WriteMessage(websocket.BinaryMessage, eventPacket([]byte(`{"type":"text_delta","payload":"ok"}`)))
				_ = conn.WriteMessage(websocket.BinaryMessage, eventPacket([]byte(`{"type":"turn_completed","payload":{}}`)))
				return
			}
		}
	}))
	defer server.Close()

	// Exercise performChat + conversation store reuse & reset directly against mock server.
	store := newConversationStore(30*time.Minute, 16)
	dial := func() *websocket.Conn {
		c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	req1 := pluginapi.ExecutorRequest{AuthID: "a1", Model: id + "-smart", Format: "openai", SourceFormat: "openai", Payload: []byte(`{"messages":[{"role":"user","content":"turn one"}]}`)}
	in1, failure := prepareChat(req1)
	if failure != nil {
		t.Fatal(failure)
	}
	in1.deleteSession = false
	conv := store.acquire(conversationKey("a1", nil, in1.messages, in1.systemPromptText))
	res1, failure := performChat(t.Context(), dial(), in1, credential{Actor: "actor"}, chatOptions{}, func(string) bool { return true })
	if failure != nil {
		t.Fatal(failure)
	}
	conv.threadID = res1.ThreadID
	conv.authID = "a1"
	conv.messages = in1.messages
	conv.systemPrompt = in1.systemPromptText
	store.release(conv)

	req2 := pluginapi.ExecutorRequest{AuthID: "a1", Model: id + "-smart", Format: "openai", SourceFormat: "openai", Payload: []byte(`{"messages":[{"role":"user","content":"turn one"},{"role":"assistant","content":"ok"},{"role":"user","content":"turn two"}]}`)}
	in2, failure := prepareChat(req2)
	if failure != nil {
		t.Fatal(failure)
	}
	in2.deleteSession = false
	fullPrompt := in2.prompt
	conv = store.acquire(conversationKey("a1", nil, in2.messages, in2.systemPromptText))
	delta, ok := conv.delta(in2.messages)
	if !ok {
		t.Fatal("expected delta for turn 2")
	}
	in2.prompt, _ = formatPrompt(delta)
	in2.threadID = conv.threadID
	in2.createSession = false
	_, failure = performChat(t.Context(), dial(), in2, credential{Actor: "actor"}, chatOptions{}, func(string) bool { return true })
	if failure == nil || failure.Code != errCodeSessionNotFound {
		t.Fatalf("expected session_not_found on expired reused session, got %v", failure)
	}
	// Reset and retry with full transcript.
	conv.reset()
	in2.prompt = fullPrompt
	in2.threadID = ""
	in2.createSession = true
	res2, failure := performChat(t.Context(), dial(), in2, credential{Actor: "actor"}, chatOptions{}, func(string) bool { return true })
	store.release(conv)
	if failure != nil || res2.Text != "ok" {
		t.Fatalf("recovery turn failed: %+v, %v", res2, failure)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(turns))
	}
	if !turns[0].created || turns[1].created || turns[1].thread != turns[0].thread || !turns[2].created || turns[2].thread == turns[0].thread {
		t.Fatalf("unexpected session lifecycle: %+v", turns)
	}
	if strings.Contains(turns[1].text, "turn one") || !strings.Contains(turns[2].text, "turn one") {
		t.Fatalf("unexpected delta/full prompt contents: turn2=%q turn3=%q", turns[1].text, turns[2].text)
	}
}

func TestAuthMetadataIncludesAutoRefreshHints(t *testing.T) {
	exp := time.Now().Add(45 * time.Minute).UTC().Truncate(time.Second)
	saved := credential{
		Type:     id,
		Portal:   personal,
		Tenant:   personalTenant,
		TokenURL: "https://oidc.us-east-1.amazonaws.com/token",
		ClientID: "fixture",
		Access:   "fixture-access",
		Refresh:  "fixture-refresh",
		Expires:  exp,
		Actor:    "fixture-actor",
		Email:    "user@example.invalid",
	}
	parsed, failure := parseAuth(pluginapi.AuthParseRequest{
		FileName: "aws-quickwork-test.json",
		RawJSON:  raw(saved),
	})
	if failure != nil || !parsed.Handled {
		t.Fatalf("parseAuth failed: handled=%v err=%v", parsed.Handled, failure)
	}
	if parsed.Auth.Metadata["expires_at"] != exp.Format(time.RFC3339) {
		t.Fatalf("expected expires_at %q in metadata, got %v", exp.Format(time.RFC3339), parsed.Auth.Metadata["expires_at"])
	}
	if parsed.Auth.Metadata["refresh_interval_seconds"] != 300 {
		t.Fatalf("expected refresh_interval_seconds=300 in metadata, got %v", parsed.Auth.Metadata["refresh_interval_seconds"])
	}
	if parsed.Auth.Attributes["refresh_interval_seconds"] != "300" {
		t.Fatalf("expected refresh_interval_seconds=300 in attributes, got %v", parsed.Auth.Attributes["refresh_interval_seconds"])
	}
	if !parsed.Auth.NextRefreshAfter.Before(exp) {
		t.Fatalf("expected NextRefreshAfter before expiry, got %v vs %v", parsed.Auth.NextRefreshAfter, exp)
	}
}

func TestOnDemandRefreshPersistsAndRetries(t *testing.T) {
	var rt engine
	cfg := defaultSettings()
	rt.swap(&cfg)
	t.Cleanup(rt.stop)

	usageJSON := `{"monthly":{"used":12,"limit":100,"resets_at":1800000000},"subscription":{"subscription_type":"PRO"}}`
	apiResp := raw(map[string]any{"type": "api_response", "status": 200, "body": usageJSON, "body_encoding": "json"})
	usageOuter := raw(map[string]any{"eventResponse": map[string]any{"invokeAgentResponse": string(apiResp)}})

	makeStorageWithExcluded := func(expires time.Time, access, refresh, idToken string) []byte {
		cred := credential{
			Type:     id,
			Portal:   personal,
			Tenant:   personalTenant,
			TokenURL: "https://oidc.us-east-1.amazonaws.com/token",
			ClientID: "fixture-client",
			Access:   access,
			Refresh:  refresh,
			Identity: idToken,
			Expires:  expires,
			Actor:    "fixture-actor",
		}
		var m map[string]any
		_ = json.Unmarshal(raw(cred), &m)
		m["excluded_models"] = []string{"aws-quickwork-auto", "aws-quickwork-balanced"}
		return raw(m)
	}

	t.Run("proactive_refresh_on_expired_quota", func(t *testing.T) {
		expiredStorage := makeStorageWithExcluded(time.Now().Add(-2*time.Hour), "stale-access", "old-refresh", "")
		var savedReq *pluginapi.HostAuthSaveRequest
		var tokenCalls, quotaCalls int

		value, failure := rt.call(pluginabi.MethodQuotaFetch, raw(map[string]any{
			"auth_index":       "idx-expired",
			"auth_id":          "aws-quickwork-test.json",
			"provider":         id,
			"storage_json":     expiredStorage,
			"attributes":       map[string]string{"path": "/tmp/oauth/aws-quickwork-test.json"},
			"host_callback_id": "cb-refresh",
		}), func(method string, payload any) bool {
			if method == pluginabi.MethodHostAuthSave {
				req := payload.(pluginapi.HostAuthSaveRequest)
				savedReq = &req
				return true
			}
			return false
		}, func(_ string, httpReq pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			u, _ := url.Parse(httpReq.URL)
			switch u.Path {
			case "/token":
				tokenCalls++
				return pluginapi.HTTPResponse{
					StatusCode: 200,
					Body:       raw(map[string]any{"access_token": "fresh-access", "refresh_token": "fresh-refresh", "expires_in": 3600}),
				}, nil
			case "/integration/quick-work/qbs-event":
				quotaCalls++
				if httpReq.Headers.Get("Authorization") != "Bearer fresh-access" {
					t.Fatalf("expected refreshed bearer token, got %q", httpReq.Headers.Get("Authorization"))
				}
				return pluginapi.HTTPResponse{StatusCode: 200, Body: usageOuter}, nil
			default:
				t.Fatalf("unexpected path: %s", u.Path)
				return pluginapi.HTTPResponse{StatusCode: 404}, nil
			}
		})
		if failure != nil {
			t.Fatalf("proactive quota refresh failed: %v", failure)
		}
		resp := value.(pluginapi.QuotaFetchResponse)
		if tokenCalls != 1 || quotaCalls != 1 || resp.Subscription == nil || resp.Subscription.Plan != "PRO" {
			t.Fatalf("unexpected call counts or response: tokens=%d quota=%d resp=%+v", tokenCalls, quotaCalls, resp)
		}
		if savedReq == nil || savedReq.Name != "aws-quickwork-test.json" {
			t.Fatalf("expected HostAuthSave for aws-quickwork-test.json, got %+v", savedReq)
		}
		var persisted map[string]any
		if err := json.Unmarshal(savedReq.JSON, &persisted); err != nil {
			t.Fatalf("invalid persisted json: %v", err)
		}
		if persisted["access_token"] != "fresh-access" || persisted["refresh_token"] != "fresh-refresh" || persisted["excluded_models"] == nil {
			t.Fatalf("persisted credential missing rotated tokens or excluded_models: %s", string(savedReq.JSON))
		}
	})

	t.Run("reactive_refresh_on_401_quota", func(t *testing.T) {
		validStorage := makeStorageWithExcluded(time.Now().Add(30*time.Minute), "revoked-access", "old-refresh", "")
		var savedCount, tokenCalls, quotaCalls int

		value, failure := rt.call(pluginabi.MethodQuotaFetch, raw(map[string]any{
			"auth_index":       "idx-401",
			"auth_id":          "aws-quickwork-401.json",
			"provider":         id,
			"storage_json":     validStorage,
			"host_callback_id": "cb-401",
		}), func(method string, _ any) bool {
			if method == pluginabi.MethodHostAuthSave {
				savedCount++
				return true
			}
			return false
		}, func(_ string, httpReq pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			u, _ := url.Parse(httpReq.URL)
			switch u.Path {
			case "/token":
				tokenCalls++
				return pluginapi.HTTPResponse{
					StatusCode: 200,
					Body:       raw(map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh", "expires_in": 3600}),
				}, nil
			case "/integration/quick-work/qbs-event":
				quotaCalls++
				if quotaCalls == 1 {
					return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte(`{"message":"expired"}`)}, nil
				}
				if httpReq.Headers.Get("Authorization") != "Bearer rotated-access" {
					t.Fatalf("retry did not use rotated token: %q", httpReq.Headers.Get("Authorization"))
				}
				return pluginapi.HTTPResponse{StatusCode: 200, Body: usageOuter}, nil
			default:
				t.Fatalf("unexpected path: %s", u.Path)
				return pluginapi.HTTPResponse{StatusCode: 404}, nil
			}
		})
		if failure != nil {
			t.Fatalf("reactive 401 refresh failed: %v", failure)
		}
		if tokenCalls != 1 || quotaCalls != 2 || savedCount != 1 || value.(pluginapi.QuotaFetchResponse).Subscription.Plan != "PRO" {
			t.Fatalf("unexpected reactive refresh state: tokens=%d quota=%d saved=%d", tokenCalls, quotaCalls, savedCount)
		}
	})

	t.Run("expired_refresh_token_returns_clear_reauth_error", func(t *testing.T) {
		expiredStorage := makeStorageWithExcluded(time.Now().Add(-2*time.Hour), "stale-access", "dead-refresh", "")
		for _, body := range []string{`{"error":"invalid_grant"}`, `{"__type":"ExpiredTokenException"}`} {
			_, failure := rt.call(pluginabi.MethodQuotaFetch, raw(map[string]any{
				"auth_index":       "idx-dead",
				"auth_id":          "aws-quickwork-dead.json",
				"provider":         id,
				"storage_json":     expiredStorage,
				"host_callback_id": "cb-dead",
			}), nil, func(_ string, _ pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
				return pluginapi.HTTPResponse{StatusCode: 400, Body: []byte(body)}, nil
			})
			if failure == nil || failure.Code != "reauth_required" || failure.HTTPStatus != 401 {
				t.Fatalf("expected 401 reauth_required for %s, got %+v", body, failure)
			}
			if !strings.Contains(failure.Message, "expired and could not be refreshed") || strings.Contains(failure.Message, "bridge keys") {
				t.Fatalf("misleading error message for %s: %q", body, failure.Message)
			}
		}
	})

	t.Run("proactive_refresh_on_chat_validateRequest", func(t *testing.T) {
		expiredStorage := makeStorageWithExcluded(time.Now().Add(-time.Hour), "old-access", "old-refresh", "")
		var savedReq *pluginapi.HostAuthSaveRequest
		transport := oauthTransport{callbackID: "cb-chat", send: func(_ string, httpReq pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			if !strings.HasSuffix(httpReq.URL, "/token") {
				t.Fatalf("unexpected URL: %s", httpReq.URL)
			}
			return pluginapi.HTTPResponse{
				StatusCode: 200,
				Body:       raw(map[string]any{"access_token": "chat-fresh-access", "refresh_token": "chat-fresh-refresh", "expires_in": 3600}),
			}, nil
		}}
		_, saved, _, refreshed, failure := rt.generation.validateRequest(pluginapi.ExecutorRequest{
			AuthID:       "aws-quickwork-chat.json",
			AuthProvider: id,
			Model:        id + "-fast",
			Format:       "openai",
			SourceFormat: "openai",
			StorageJSON:  expiredStorage,
			Payload:      []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		}, func(method string, payload any) bool {
			if method == pluginabi.MethodHostAuthSave {
				req := payload.(pluginapi.HostAuthSaveRequest)
				savedReq = &req
				return true
			}
			return false
		}, transport)
		if failure != nil || !refreshed || saved.Access != "chat-fresh-access" || savedReq == nil || savedReq.Name != "aws-quickwork-chat.json" {
			t.Fatalf("chat validateRequest proactive refresh failed: refreshed=%v access=%q saved=%+v err=%v", refreshed, saved.Access, savedReq, failure)
		}
	})
}
