package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	maxCompletion = 2 << 20
	maxEvent      = 512 << 10
	chatPath      = "/v1/chat/completions"
)

func bridgeClient() *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			// Keep the configured bridge origin a real boundary. Do not route
			// bridge keys or messages through inherited proxy settings.
			DialContext: d.DialContext, ForceAttemptHTTP2: true,
			MaxIdleConns: 16, MaxIdleConnsPerHost: 4, MaxConnsPerHost: 24,
			IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
			MaxResponseHeaderBytes: 64 << 10,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func providerMode(model string) (string, bool) {
	for _, mode := range modes {
		if model == id+"-"+mode {
			return mode, true
		}
	}
	return "", false
}

func preparePayload(req pluginapi.ExecutorRequest, stream bool) ([]byte, *pluginabi.Error) {
	mode, ok := providerMode(req.Model)
	if !ok || req.Format != "openai" || req.SourceFormat != "openai" {
		return nil, invalidInput()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(req.Payload, &fields) != nil || fields == nil {
		return nil, invalidInput()
	}
	var messages []json.RawMessage
	if json.Unmarshal(fields["messages"], &messages) != nil || len(messages) == 0 {
		return nil, invalidInput()
	}
	fields["model"], _ = json.Marshal(mode)
	fields["stream"], _ = json.Marshal(stream)
	payload, err := json.Marshal(fields)
	if err != nil {
		return nil, invalidInput()
	}
	return payload, nil
}

func upstreamProtocol() *pluginabi.Error {
	return pluginabi.NewError("upstream_protocol", "QuickWork bridge returned an invalid response", 502)
}

func bridgeFailure(ctx context.Context, err error) *pluginabi.Error {
	var timeout net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return pluginabi.NewError("upstream_timeout", "QuickWork bridge timed out", 504)
	}
	if ctx.Err() != nil {
		return pluginabi.NewError("unavailable", "QuickWork request canceled", 503)
	}
	return pluginabi.NewError("upstream_unavailable", "QuickWork bridge request failed", 502)
}

func retrySeconds(value string, now time.Time) int {
	const maxDelay = 24 * 60 * 60
	if v, err := strconv.ParseUint(value, 10, 64); err == nil {
		return int(min(maxDelay, v))
	}
	if t, err := http.ParseTime(value); err == nil && t.After(now) {
		return int(min(maxDelay, (t.Sub(now)-1)/time.Second+1))
	}
	return 0
}

func classifyStatus(status int, headers http.Header) *pluginabi.Error {
	switch status {
	case http.StatusTooManyRequests:
		return &pluginabi.Error{Code: pluginabi.ErrorCodeTransientRateLimit, Message: "QuickWork bridge is temporarily rate limited", HTTPStatus: status, RetryAfterSeconds: retrySeconds(headers.Get("Retry-After"), time.Now())}
	case http.StatusUnauthorized, http.StatusForbidden:
		return pluginabi.NewError("bridge_auth_rejected", "QuickWork bridge refused the credential", status)
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return pluginabi.NewError(pluginabi.ErrorCodeRequestScoped, "QuickWork bridge rejected the request", status)
	case http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return pluginabi.NewError("upstream_unavailable", "QuickWork bridge is temporarily unavailable", status)
	default:
		return pluginabi.NewError("upstream_failure", "QuickWork bridge could not complete the request", 502)
	}
}

func (g *generation) open(ctx context.Context, req pluginapi.ExecutorRequest, stream bool) (*http.Response, *pluginabi.Error) {
	if !g.Enabled {
		return nil, pluginabi.NewError("unavailable", "QuickWork plugin is disabled", 503)
	}
	if req.AuthID == "" || req.AuthProvider != id {
		return nil, invalidAuth()
	}
	cred, err := g.parseCredential(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	body, err := preparePayload(req, stream)
	if err != nil {
		return nil, err
	}
	msg, e := http.NewRequestWithContext(ctx, http.MethodPost, cred.Origin+chatPath, bytes.NewReader(body))
	if e != nil {
		return nil, invalidAuth()
	}
	msg.Header.Set("Authorization", "Bearer "+cred.Key)
	msg.Header.Set("Content-Type", "application/json")
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	msg.Header.Set("Accept", accept)
	response, e := g.client.Do(msg)
	if e != nil {
		return nil, bridgeFailure(ctx, e)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, classifyStatus(response.StatusCode, response.Header)
	}
	media, _, e := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if e != nil || media != accept {
		response.Body.Close()
		return nil, upstreamProtocol()
	}
	return response, nil
}

// Change only the model name, keeping choices, tool calls and usage intact.
func normalizeOutput(raw []byte, model, object string) ([]byte, *pluginabi.Error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || (len(fields["error"]) > 0 && string(fields["error"]) != "null") {
		return nil, upstreamProtocol()
	}
	var kind string
	var choices []json.RawMessage
	if json.Unmarshal(fields["object"], &kind) != nil || kind != object || json.Unmarshal(fields["choices"], &choices) != nil || choices == nil {
		return nil, upstreamProtocol()
	}
	fields["model"], _ = json.Marshal(model)
	result, err := json.Marshal(fields)
	if err != nil {
		return nil, upstreamProtocol()
	}
	return result, nil
}

func (g *generation) execute(req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, *pluginabi.Error) {
	ctx, cancel := context.WithTimeout(g.ctx, g.Timeout)
	defer cancel()
	var result pluginapi.ExecutorResponse
	response, err := g.open(ctx, req, false)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, maxCompletion+1))
	if e != nil {
		return result, bridgeFailure(ctx, e)
	}
	if len(raw) > maxCompletion {
		return result, upstreamProtocol()
	}
	result.Payload, err = normalizeOutput(raw, req.Model, "chat.completion")
	return result, err
}

type streamEvent struct {
	StreamID string           `json:"stream_id"`
	Payload  []byte           `json:"payload,omitempty"`
	Failure  *pluginabi.Error `json:"error_detail,omitempty"`
}

func (g *generation) executeStream(req pluginapi.ExecutorRequest, streamID string, host hostCallback) (any, *pluginabi.Error) {
	if streamID == "" || host == nil {
		return nil, invalidInput()
	}
	ctx, cancel := context.WithTimeout(g.ctx, g.Timeout)
	response, err := g.open(ctx, req, true)
	if err != nil {
		cancel()
		return nil, err
	}
	// The dispatch call still holds a reference to this generation here.
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer cancel()
		defer response.Body.Close()
		var failure *pluginabi.Error
		defer func() {
			if recover() != nil {
				failure = upstreamProtocol()
			}
			host(pluginabi.MethodHostStreamClose, streamEvent{StreamID: streamID, Failure: failure})
		}()
		failure = pumpSSE(ctx, response.Body, req.Model, func(payload []byte) bool {
			return host(pluginabi.MethodHostStreamEmit, streamEvent{StreamID: streamID, Payload: payload})
		})
	}()
	return map[string]any{"headers": http.Header{"Content-Type": {"text/event-stream"}}}, nil
}

func pumpSSE(ctx context.Context, body io.Reader, model string, emit func([]byte) bool) *pluginabi.Error {
	scan := bufio.NewScanner(body)
	scan.Buffer(make([]byte, 4096), maxEvent)
	var data []byte
	var sawData, sawChunk bool
	first := true
	for scan.Scan() {
		if ctx.Err() != nil {
			return bridgeFailure(ctx, ctx.Err())
		}
		line := scan.Bytes()
		if first {
			line = bytes.TrimPrefix(line, []byte("\ufeff"))
			first = false
		}
		if len(line) == 0 {
			if !sawData {
				continue
			}
			if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
				if sawChunk {
					return nil
				}
				return upstreamProtocol()
			}
			chunk, err := normalizeOutput(data, model, "chat.completion.chunk")
			if err != nil {
				return err
			}
			if !emit(chunk) {
				// The host may have already canceled the output stream.
				return nil
			}
			sawChunk = true
			data, sawData = data[:0], false
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		if !bytes.Equal(field, []byte("data")) {
			continue
		}
		value = bytes.TrimPrefix(value, []byte(" "))
		if len(data)+len(value)+1 > maxEvent {
			return upstreamProtocol()
		}
		if sawData {
			data = append(data, '\n')
		}
		data = append(data, value...)
		sawData = true
	}
	// Never turn a truncated stream into a successful completion.
	if ctx.Err() != nil {
		return bridgeFailure(ctx, ctx.Err())
	}
	return upstreamProtocol()
}
