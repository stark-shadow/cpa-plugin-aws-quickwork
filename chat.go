package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	chatSocketPath = "/streaming/quickwork-chat-bidirectional-stream"
	surfaceType    = "QUICK_WORK_DESKTOP_WINDOWS"
	toolStart      = "<client_tool_call>"
	toolEnd        = "</client_tool_call>"
	maxChatText    = 512 << 10
)

type chatInput struct {
	model, mode, prompt, system, choice string
	tools                               map[string]bool
	parallel, includeUsage              bool
}

type requestedFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func textContent(raw json.RawMessage) (string, bool) {
	var text string
	if len(raw) == 0 || json.Unmarshal(raw, &text) == nil {
		return text, true
	}
	var parts []struct {
		Type string
		Text string
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", false
	}
	var result strings.Builder
	for _, part := range parts {
		if part.Type != "text" {
			return "", false
		}
		result.WriteString(part.Text)
	}
	return result.String(), true
}

func invalidInput() *pluginabi.Error {
	return pluginabi.NewError(pluginabi.ErrorCodeRequestScoped, "Invalid or unsupported QuickWork text Chat Completions request", 400)
}

func prepareChat(req pluginapi.ExecutorRequest) (chatInput, *pluginabi.Error) {
	result := chatInput{model: req.Model, tools: make(map[string]bool), parallel: true}
	for _, mode := range modes {
		if req.Model == id+"-"+mode {
			result.mode = mode
			break
		}
	}
	if result.mode == "" || req.Format != "openai" || req.SourceFormat != "openai" {
		return result, invalidInput()
	}
	var body struct {
		N        int
		Messages []struct {
			Role         string
			Content      json.RawMessage
			ToolCalls    json.RawMessage `json:"tool_calls"`
			ToolCallID   string          `json:"tool_call_id"`
			FunctionCall json.RawMessage `json:"function_call"`
			Name         string
		}
		Tools []struct {
			Type     string
			Function requestedFunction
		}
		Functions      []requestedFunction
		ToolChoice     json.RawMessage `json:"tool_choice"`
		FunctionChoice json.RawMessage `json:"function_call"`
		Parallel       *bool           `json:"parallel_tool_calls"`
		StreamOptions  struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if json.Unmarshal(req.Payload, &body) != nil || body.N > 1 || len(body.Messages) == 0 {
		return result, invalidInput()
	}
	result.includeUsage = body.StreamOptions.IncludeUsage
	if body.Parallel != nil {
		result.parallel = *body.Parallel
	}
	var dialogue, system []string
	oneUser := ""
	for _, message := range body.Messages {
		text, ok := textContent(message.Content)
		if !ok {
			return result, invalidInput()
		}
		switch message.Role {
		case "system", "developer":
			if text != "" {
				system = append(system, text)
			}
		case "user":
			oneUser = text
			dialogue = append(dialogue, "User: "+text)
		case "assistant":
			if text != "" {
				dialogue = append(dialogue, "Assistant: "+text)
			}
			if len(message.ToolCalls) > 0 {
				dialogue = append(dialogue, "Assistant tool calls: "+string(message.ToolCalls))
			}
			if len(message.FunctionCall) > 0 {
				dialogue = append(dialogue, "Assistant function call: "+string(message.FunctionCall))
			}
		case "tool", "function":
			if message.Role == "tool" && message.ToolCallID == "" {
				return result, invalidInput()
			}
			data, _ := json.Marshal(map[string]string{"id": message.ToolCallID, "name": message.Name, "content": text})
			dialogue = append(dialogue, "Client tool result: "+string(data))
		default:
			return result, invalidInput()
		}
	}
	result.prompt = strings.Join(dialogue, "\n\n")
	if len(dialogue) == 1 && oneUser != "" {
		result.prompt = oneUser
	}
	if strings.TrimSpace(result.prompt) == "" {
		return result, invalidInput()
	}
	functions := body.Functions
	for _, tool := range body.Tools {
		if tool.Type != "function" {
			return result, invalidInput()
		}
		functions = append(functions, tool.Function)
	}
	choice := body.ToolChoice
	if len(choice) == 0 {
		choice = body.FunctionChoice
	}
	if len(choice) != 0 && string(choice) != "null" {
		if json.Unmarshal(choice, &result.choice) != nil {
			var specific struct {
				Name     string
				Function struct{ Name string }
			}
			if json.Unmarshal(choice, &specific) != nil {
				return result, invalidInput()
			}
			result.choice = specific.Function.Name
			if result.choice == "" {
				result.choice = specific.Name
			}
			if result.choice == "" {
				return result, invalidInput()
			}
		}
	}
	if result.choice != "none" {
		for _, function := range functions {
			if function.Name == "" {
				return result, invalidInput()
			}
			result.tools[function.Name] = true
		}
		if result.choice != "" && result.choice != "auto" && result.choice != "required" && !result.tools[result.choice] {
			return result, invalidInput()
		}
		if result.choice == "required" && len(result.tools) == 0 {
			return result, invalidInput()
		}
		if len(result.tools) > 0 {
			definitions, _ := json.Marshal(functions)
			instruction := "These tools are executed only by the API client. To request one, emit " + toolStart + `{"name":"function_name","arguments":{}}` + toolEnd + ". Do not execute client tools yourself. Available functions: " + string(definitions)
			if result.choice == "required" {
				instruction += " You must request a function."
			} else if result.tools[result.choice] {
				instruction += " Request only " + result.choice + "."
			}
			if !result.parallel {
				instruction += " Request at most one function per response."
			}
			system = append(system, instruction)
		}
	}
	result.system = strings.Join(system, "\n\n")
	return result, nil
}

type toolInvocation struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatUsage struct {
	Input  int `json:"input_tokens"`
	Output int `json:"output_tokens"`
	Total  int `json:"total_tokens"`
	Cached int `json:"cache_read_tokens"`
}

func (usage chatUsage) openAI() any {
	return map[string]any{"prompt_tokens": usage.Input, "completion_tokens": usage.Output, "total_tokens": max(usage.Total, usage.Input+usage.Output), "prompt_tokens_details": map[string]int{"cached_tokens": usage.Cached}}
}

type chatResult struct {
	Text  string
	Tools []toolInvocation
	Usage chatUsage
}
type chatEvent struct {
	Type      string
	Payload   json.RawMessage
	ErrorCode string `json:"error_code"`
}

func aidpOutput(frame []byte) ([][]byte, error) {
	if json.Valid(frame) {
		var wrapper struct{ Type, Payload string }
		if json.Unmarshal(frame, &wrapper) == nil && wrapper.Type == "" && wrapper.Payload != "" {
			decoded, err := base64.StdEncoding.DecodeString(wrapper.Payload)
			return [][]byte{decoded}, err
		}
		return [][]byte{frame}, nil
	}
	var result [][]byte
	for len(frame) > 0 {
		if len(frame) < 16 {
			return nil, io.ErrUnexpectedEOF
		}
		size, headers := int(binary.BigEndian.Uint32(frame)), int(binary.BigEndian.Uint32(frame[4:]))
		if size < 16 || size > len(frame) || headers > size-16 || crc32.ChecksumIEEE(frame[:8]) != binary.BigEndian.Uint32(frame[8:]) || crc32.ChecksumIEEE(frame[:size-4]) != binary.BigEndian.Uint32(frame[size-4:]) {
			return nil, errors.New("invalid event frame")
		}
		if bytes.Contains(frame[12:12+headers], []byte(":error-code")) || bytes.Contains(frame[12:12+headers], []byte(":exception-type")) {
			return nil, errors.New("upstream event exception")
		}
		var wrapper struct{ Payload string }
		if json.Unmarshal(frame[12+headers:size-4], &wrapper) != nil || wrapper.Payload == "" {
			return nil, errors.New("invalid event payload")
		}
		data, err := base64.StdEncoding.DecodeString(wrapper.Payload)
		if err != nil {
			return nil, err
		}
		result = append(result, data)
		frame = frame[size:]
	}
	return result, nil
}

func uid() string {
	secret := make([]byte, 16)
	_, _ = rand.Read(secret)
	secret[6] = (secret[6] & 15) | 64
	secret[8] = (secret[8] & 63) | 128
	raw := hex.EncodeToString(secret)
	return raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:]
}

func makeTool(identifier, tool string, arguments json.RawMessage) (toolInvocation, bool) {
	if identifier == "" {
		identifier = "call_" + strings.ReplaceAll(uid(), "-", "")
	}
	if len(arguments) == 0 || string(arguments) == "null" {
		arguments = json.RawMessage("{}")
	}
	var args string
	if json.Unmarshal(arguments, &args) != nil {
		args = string(arguments)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &object) != nil || object == nil {
		return toolInvocation{}, false
	}
	call := toolInvocation{ID: identifier, Type: "function"}
	call.Function.Name = tool
	call.Function.Arguments = args
	return call, true
}

type toolEvent struct {
	ID        string          `json:"id"`
	ToolID    string          `json:"tool_id"`
	CallID    string          `json:"tool_call_id"`
	Name      string          `json:"name"`
	ToolName  string          `json:"tool_name"`
	Arguments json.RawMessage `json:"arguments"`
	Function  struct {
		Name      string
		Arguments json.RawMessage
	} `json:"function"`
}

func collectChat(ctx context.Context, input chatInput, read func() ([]byte, error), emit func(string) bool, grace func(bool)) (chatResult, *pluginabi.Error) {
	var result chatResult
	var content strings.Builder
	emitted := 0
	tools := make(map[string]toolEvent)
	var order []string
	record := func(item toolEvent) {
		key := item.ID
		if key == "" {
			key = item.ToolID
		}
		if key == "" {
			key = item.CallID
		}
		if key == "" {
			key = "call_" + uid()
		}
		entry, exists := tools[key]
		if !exists {
			order = append(order, key)
		}
		entry.ID = key
		for _, label := range []string{item.Name, item.ToolName, item.Function.Name} {
			if label != "" {
				entry.Name = label
				break
			}
		}
		if len(item.Arguments) > 0 {
			entry.Arguments = item.Arguments
		} else if len(item.Function.Arguments) > 0 {
			entry.Arguments = item.Function.Arguments
		}
		tools[key] = entry
	}
	finish := func() (chatResult, *pluginabi.Error) {
		result.Text = content.String()
		if len(input.tools) > 0 {
			var cleaned strings.Builder
			for remaining := result.Text; remaining != ""; {
				before, after, found := strings.Cut(remaining, toolStart)
				cleaned.WriteString(before)
				if !found {
					break
				}
				data, rest, closed := strings.Cut(after, toolEnd)
				if !closed {
					return result, submittedFailure("QuickWork returned an incomplete tool call", 502)
				}
				var item toolEvent
				if json.Unmarshal([]byte(data), &item) != nil || !input.tools[item.Name] {
					return result, submittedFailure("QuickWork returned an invalid client tool call", 502)
				}
				record(item)
				remaining = rest
			}
			result.Text = cleaned.String()
		}
		for _, key := range order {
			item := tools[key]
			if !input.tools[item.Name] || (input.tools[input.choice] && item.Name != input.choice) {
				continue
			}
			if call, ok := makeTool(item.ID, item.Name, item.Arguments); ok {
				result.Tools = append(result.Tools, call)
				if !input.parallel {
					break
				}
			}
		}
		if (input.choice == "required" || input.tools[input.choice]) && len(result.Tools) == 0 {
			return result, submittedFailure("QuickWork did not return the required tool call", 502)
		}
		if strings.TrimSpace(result.Text) == "" && len(result.Tools) == 0 {
			return result, submittedFailure("QuickWork returned no text or tools", 502)
		}
		if len(input.tools) > 0 && len(result.Text) > emitted {
			emit(result.Text[emitted:])
		}
		return result, nil
	}
	complete := false
	for {
		frame, err := read()
		if err != nil {
			var timeout net.Error
			var closed *websocket.CloseError
			if complete && ctx.Err() == nil && ((errors.As(err, &timeout) && timeout.Timeout()) || (errors.As(err, &closed) && closed.Code == websocket.CloseNormalClosure)) {
				return finish()
			}
			return result, pluginabi.NewError(pluginabi.ErrorCodeStreamTruncated, "QuickWork stream ended before completion", 502)
		}
		events, err := aidpOutput(frame)
		if err != nil {
			return result, submittedFailure("Invalid QuickWork event frame", 502)
		}
		for _, data := range events {
			var event chatEvent
			if json.Unmarshal(data, &event) != nil {
				return result, submittedFailure("Invalid QuickWork event", 502)
			}
			switch event.Type {
			case "text_delta", "text_complete":
				if event.Type == "text_complete" && content.Len() > 0 {
					continue
				}
				var text string
				if json.Unmarshal(event.Payload, &text) != nil {
					var delta struct {
						Text  string
						Delta string
					}
					_ = json.Unmarshal(event.Payload, &delta)
					text = delta.Delta
					if text == "" {
						text = delta.Text
					}
				}
				if content.Len()+len(text) > maxChatText {
					return result, submittedFailure("QuickWork response exceeds the configured limit", 502)
				}
				if text != "" {
					content.WriteString(text)
					if len(input.tools) == 0 {
						if !emit(text) {
							return result, nil
						}
					} else {
						buffer := content.String()
						limit := strings.Index(buffer, toolStart)
						if limit < 0 {
							limit = len(buffer)
							for width := min(len(toolStart)-1, len(buffer)); width > 0; width-- {
								if strings.HasSuffix(buffer, toolStart[:width]) {
									limit -= width
									break
								}
							}
						}
						if limit > emitted {
							if !emit(buffer[emitted:limit]) {
								return result, nil
							}
							emitted = limit
						}
					}
				}
			case "turn_usage":
				var usage chatUsage
				if json.Unmarshal(event.Payload, &usage) == nil {
					result.Usage.Input += usage.Input
					result.Usage.Output += usage.Output
					result.Usage.Total += usage.Total
					result.Usage.Cached += usage.Cached
				}
			case "tool_start", "tool_arguments", "tool_arguments_edited", "tool_result", "tool_complete", "tool_call", "tool_call_update":
				if len(input.tools) > 0 {
					var item toolEvent
					if json.Unmarshal(event.Payload, &item) == nil {
						record(item)
					}
				}
			case "message_complete":
				var end struct {
					Continuation bool        `json:"continuation_expected"`
					Tools        []toolEvent `json:"tool_calls"`
				}
				_ = json.Unmarshal(event.Payload, &end)
				for _, item := range end.Tools {
					record(item)
				}
				complete = !end.Continuation
				if grace != nil {
					grace(complete)
				}
			case "turn_completed", "turn_ended", "stream_ended", "chat_complete":
				return finish()
			case "error", "session_error", "stream_error", "turn_failed", "turn_aborted":
				var failure struct {
					Code      string `json:"code"`
					ErrorCode string `json:"error_code"`
				}
				_ = json.Unmarshal(event.Payload, &failure)
				for _, code := range []string{event.ErrorCode, failure.Code, failure.ErrorCode} {
					switch strings.ToLower(code) {
					case "429", "throttlingexception", "throttled", "too_many_requests":
						return result, submittedFailure("QuickWork is rate limited", 429)
					}
				}
				return result, submittedFailure("QuickWork could not complete the turn", 502)
			}
		}
	}
}

func submittedFailure(message string, status int) *pluginabi.Error {
	return pluginabi.NewError(pluginabi.ErrorCodeRequestScoped, message, status)
}

func (g *generation) connect(ctx context.Context, req pluginapi.ExecutorRequest) (*websocket.Conn, chatInput, credential, *pluginabi.Error) {
	var input chatInput
	var saved credential
	if !g.Enabled {
		return nil, input, saved, pluginabi.NewError("unavailable", "QuickWork is disabled", 503)
	}
	if req.AuthID == "" {
		return nil, input, saved, badCredential()
	}
	saved, failure := readCredential(req.AuthProvider, req.StorageJSON)
	if failure != nil {
		return nil, input, saved, failure
	}
	if time.Now().After(saved.Expires) {
		return nil, input, saved, badCredential()
	}
	input, failure = prepareChat(req)
	if failure != nil {
		return nil, input, saved, failure
	}
	tenant, _ := validTenant(saved.Tenant)
	address := url.URL{Scheme: "wss", Host: tenant.Host, Path: chatSocketPath}
	query := url.Values{"surfaceType": {surfaceType}}
	if saved.Portal == personal {
		query.Set("token", saved.bearer())
	}
	address.RawQuery = query.Encode()
	headers := http.Header{"Authorization": {"Bearer " + saved.bearer()}, "Origin": {"file://"}, "X-Surface-Type": {surfaceType}, "X-Agent-Service-Type": {"UNIFIED_QUICK"}}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, response, err := dialer.DialContext(ctx, address.String(), headers)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
			switch response.StatusCode {
			case 429:
				retry, _ := strconv.Atoi(response.Header.Get("Retry-After"))
				return nil, input, saved, &pluginabi.Error{Code: pluginabi.ErrorCodeTransientRateLimit, Message: "QuickWork is rate limited", HTTPStatus: 429, RetryAfterSeconds: min(max(retry, 0), 86400)}
			case 401, 403:
				return nil, input, saved, badCredential()
			}
		}
		return nil, input, saved, pluginabi.NewError(pluginabi.ErrorCodeTransientTransport, "QuickWork WebSocket connection failed", 502)
	}
	conn.SetReadLimit(2 << 20)
	return conn, input, saved, nil
}

func sendEvent(conn *websocket.Conn, kind string, payload map[string]any) error {
	data, _ := json.Marshal(map[string]any{"type": kind, "payload": payload})
	frame, _ := json.Marshal(map[string]any{"eventType": "rawInputEvent", "payload": map[string]string{"payload": base64.StdEncoding.EncodeToString(data)}})
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteMessage(websocket.BinaryMessage, frame)
}

func performChat(ctx context.Context, conn *websocket.Conn, input chatInput, saved credential, emit func(string) bool) (chatResult, *pluginabi.Error) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-finished:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(3*time.Second))
			}
		}
	}()
	thread := uid()
	if sendEvent(conn, "session_connect", map[string]any{"actor_id": saved.Actor, "surfaceType": surfaceType, "appVersion": appVersion, "outpostId": "", "external_agent_sign_in": 1, "recents_promotion": 1}) != nil || sendEvent(conn, "federate_status", map[string]any{}) != nil || sendEvent(conn, "session_create", map[string]any{"session_id": thread, "title": "QuickWork API chat"}) != nil {
		return chatResult{}, pluginabi.NewError("upstream_unavailable", "Could not establish QuickWork chat session", 502)
	}
	defer func() { _ = sendEvent(conn, "session_delete", map[string]any{"session_id": thread}) }()
	turn := map[string]any{"text": input.prompt, "send_origin": "programmatic", "had_physical_input": false, "surfaceType": surfaceType, "outpostId": "", "appVersion": appVersion, "traceId": uid(), "message_id": uid(), "thread_id": thread, "agent_mode": input.mode, "user_timezone": "UTC", "user_locale": "en-US", "chat_agent_id": nil, "auto_routing": false, "web_search_enabled": false, "pre_approved_tools": []string{}, "preload_skills": []string{}, "panel_edits": []string{}, "custom_prompt": nil}
	if input.system != "" {
		turn["system_prompt"] = input.system
	}
	if sendEvent(conn, "user_message", turn) != nil {
		return chatResult{}, submittedFailure("QuickWork could not submit the turn", 502)
	}
	read := func() ([]byte, error) { _, frame, err := conn.ReadMessage(); return frame, err }
	return collectChat(ctx, input, read, emit, func(complete bool) {
		deadline := time.Time{}
		if complete {
			deadline = time.Now().Add(5 * time.Second)
		}
		_ = conn.SetReadDeadline(deadline)
	})
}

type streamEvent struct {
	StreamID string           `json:"stream_id"`
	Payload  []byte           `json:"payload,omitempty"`
	Failure  *pluginabi.Error `json:"error_detail,omitempty"`
	Error    string           `json:"error,omitempty"`
}

func (g *generation) execute(req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, *pluginabi.Error) {
	ctx, cancel := context.WithTimeout(g.ctx, g.Timeout)
	defer cancel()
	var response pluginapi.ExecutorResponse
	conn, input, saved, failure := g.connect(ctx, req)
	if failure != nil {
		return response, failure
	}
	result, failure := performChat(ctx, conn, input, saved, func(string) bool { return true })
	if failure != nil {
		return response, failure
	}
	message := map[string]any{"role": "assistant", "content": result.Text}
	reason := "stop"
	if len(result.Tools) > 0 {
		message["tool_calls"] = result.Tools
		reason = "tool_calls"
		if result.Text == "" {
			message["content"] = nil
		}
	}
	response.Payload, _ = json.Marshal(map[string]any{"id": "chatcmpl-" + uid(), "object": "chat.completion", "created": time.Now().Unix(), "model": input.model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": reason}}, "usage": result.Usage.openAI()})
	return response, nil
}

func (g *generation) executeStream(req pluginapi.ExecutorRequest, streamID string, host hostCallback) (any, *pluginabi.Error) {
	if streamID == "" || host == nil {
		return nil, invalidInput()
	}
	ctx, cancel := context.WithTimeout(g.ctx, g.Timeout)
	conn, input, saved, failure := g.connect(ctx, req)
	if failure != nil {
		cancel()
		return nil, failure
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer cancel()
		var failure *pluginabi.Error
		defer func() {
			if recover() != nil {
				failure = submittedFailure("QuickWork stream failed", 502)
			}
			end := streamEvent{StreamID: streamID, Failure: failure}
			if failure != nil {
				end.Error = failure.Message
			}
			host(pluginabi.MethodHostStreamClose, end)
		}()
		created, requestID := time.Now().Unix(), "chatcmpl-"+uid()
		stopped := false
		emit := func(choices []any, usage any) bool {
			if stopped {
				return false
			}
			payload := map[string]any{"id": requestID, "object": "chat.completion.chunk", "created": created, "model": input.model, "choices": choices}
			if usage != nil {
				payload["usage"] = usage
			}
			encoded, _ := json.Marshal(payload)
			stopped = !host(pluginabi.MethodHostStreamEmit, streamEvent{StreamID: streamID, Payload: encoded})
			if stopped {
				cancel()
			}
			return !stopped
		}
		started := false
		chunk := func(delta any, reason any) bool {
			return emit([]any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}, nil)
		}
		start := func() bool {
			if started {
				return !stopped
			}
			started = true
			return chunk(map[string]string{"role": "assistant"}, nil)
		}
		var result chatResult
		result, failure = performChat(ctx, conn, input, saved, func(text string) bool { return start() && chunk(map[string]string{"content": text}, nil) })
		if failure != nil || stopped || !start() {
			return
		}
		reason := "stop"
		if len(result.Tools) > 0 {
			reason = "tool_calls"
			calls := make([]any, 0, len(result.Tools))
			for index, call := range result.Tools {
				calls = append(calls, map[string]any{"index": index, "id": call.ID, "type": call.Type, "function": call.Function})
			}
			if !chunk(map[string]any{"tool_calls": calls}, nil) {
				return
			}
		}
		if chunk(map[string]any{}, reason) && input.includeUsage {
			emit([]any{}, result.Usage.openAI())
		}
	}()
	return map[string]any{"headers": http.Header{"Content-Type": {"text/event-stream"}}}, nil
}
