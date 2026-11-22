package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	id      = "aws-quickwork"
	name    = "Amazon QuickWork"
	version = "0.1.0"
	maxRPC  = 4 << 20
)

type settings struct {
	Enabled bool
	Origins map[string]struct{}
	Timeout time.Duration
}

func invalidConfig() *pluginabi.Error {
	return pluginabi.NewError("invalid_config", "Invalid QuickWork plugin configuration", 400)
}
func invalidAuth() *pluginabi.Error {
	return pluginabi.NewError("invalid_credential", "Invalid QuickWork bridge credential or origin not enabled", 401)
}
func invalidInput() *pluginabi.Error {
	return pluginabi.NewError(pluginabi.ErrorCodeRequestScoped, "Invalid QuickWork Chat Completions request", 400)
}

func canonicalOrigin(input string) (string, bool) {
	u, err := url.Parse(input)
	if err != nil || u.Hostname() == "" || u.User != nil || u.ForceQuery || u.RawQuery != "" || strings.Contains(input, "#") || u.Opaque != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return "", false
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", false
		}
		port = strconv.Itoa(n)
	} else if strings.HasSuffix(u.Host, ":") {
		return "", false
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if ip != nil {
		host = ip.String()
	}
	switch {
	case port != "":
		host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host, true
}

func parseSettings(raw []byte) (settings, *pluginabi.Error) {
	cfg := settings{Enabled: true, Origins: make(map[string]struct{}), Timeout: 2 * time.Minute}
	if len(bytes.TrimSpace(raw)) == 0 {
		return cfg, nil
	}
	var fields struct {
		Enabled  *bool     `yaml:"enabled"`
		Priority int       `yaml:"priority"`
		Store    yaml.Node `yaml:"store"`
		Origins  []string  `yaml:"bridge_origins"`
		Timeout  *string   `yaml:"request_timeout"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if decoder.Decode(&fields) != nil || decoder.Decode(new(any)) != io.EOF || len(fields.Origins) > 64 {
		return settings{}, invalidConfig()
	}
	if fields.Enabled != nil {
		cfg.Enabled = *fields.Enabled
	}
	if fields.Timeout != nil {
		t, err := time.ParseDuration(*fields.Timeout)
		if err != nil || t < time.Second || t > 5*time.Minute {
			return settings{}, invalidConfig()
		}
		cfg.Timeout = t
	}
	for _, rawOrigin := range fields.Origins {
		origin, ok := canonicalOrigin(rawOrigin)
		if !ok {
			return settings{}, invalidConfig()
		}
		if _, exists := cfg.Origins[origin]; exists {
			return settings{}, invalidConfig()
		}
		cfg.Origins[origin] = struct{}{}
	}
	return cfg, nil
}

type credential struct {
	Type   string `json:"type"`
	Origin string `json:"bridge_origin"`
	Key    string `json:"bridge_key"`
}

func (cfg settings) parseCredential(raw []byte) (credential, *pluginabi.Error) {
	var c credential
	if len(raw) > 32<<10 || json.Unmarshal(raw, &c) != nil || c.Type != id || len(c.Key) == 0 || len(c.Key) > 4096 {
		return credential{}, invalidAuth()
	}
	for _, ch := range c.Key {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("-._~+/=", ch)) {
			return credential{}, invalidAuth()
		}
	}
	origin, ok := canonicalOrigin(c.Origin)
	if !ok {
		return credential{}, invalidAuth()
	}
	if _, exists := cfg.Origins[origin]; !exists {
		return credential{}, invalidAuth()
	}
	c.Origin = origin
	return c, nil
}

func (cfg settings) parseAuth(req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, *pluginabi.Error) {
	var marker struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(req.RawJSON, &marker) != nil || marker.Type != id {
		return pluginapi.AuthParseResponse{}, nil
	}
	if _, err := cfg.parseCredential(req.RawJSON); err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	var fields struct {
		Label    string `json:"label"`
		Prefix   string `json:"prefix"`
		Disabled bool   `json:"disabled"`
	}
	if json.Unmarshal(req.RawJSON, &fields) != nil {
		return pluginapi.AuthParseResponse{}, invalidAuth()
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: pluginapi.AuthData{
		Provider: id, FileName: req.FileName, Label: fields.Label, Prefix: fields.Prefix, Disabled: fields.Disabled, StorageJSON: req.RawJSON,
		Metadata: map[string]any{"type": id}, NextRefreshAfter: time.Now().Add(24 * time.Hour),
	}}, nil
}

var modes = [...]string{"smart", "fast", "balanced", "auto"}

func models() pluginapi.ModelResponse {
	list := make([]pluginapi.ModelInfo, 0, len(modes))
	for _, mode := range modes {
		model := id + "-" + mode
		list = append(list, pluginapi.ModelInfo{ID: model, Object: "model", OwnedBy: id, Type: id, Name: model, DisplayName: name + " (" + mode + ")"})
	}
	return pluginapi.ModelResponse{Provider: id, Models: list}
}

func registration() any {
	return map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"metadata": pluginapi.Metadata{Name: name, Version: version, Author: "stark-shadow", GitHubRepository: "https://github.com/stark-shadow/cpa-plugin-aws-quickwork", ConfigFields: []pluginapi.ConfigField{
			{Name: "bridge_origins", Type: pluginapi.ConfigFieldTypeArray, Default: []string{}, Description: "Exact origins of authorized QuickWork Chat Completions bridges."},
			{Name: "request_timeout", Type: pluginapi.ConfigFieldTypeString, Default: "2m", Description: "Total bridge request deadline (1s to 5m)."},
		}},
		"capabilities": map[string]any{
			"auth_provider": true, "model_provider": true, "executor": true, "quota_provider": true,
			"executor_model_scope": pluginapi.ExecutorModelScopeOAuth, "executor_input_formats": []string{"openai"}, "executor_output_formats": []string{"openai"},
		},
	}
}

// The documented Chat Completions bridge has no account quota endpoint. The
// routing cooldown is owned by the host; it is not an account quota reading.
func (cfg settings) fetchQuota(req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, *pluginabi.Error) {
	if req.Provider != id || strings.TrimSpace(req.AuthID) == "" || strings.TrimSpace(req.AuthIndex) == "" {
		return pluginapi.QuotaFetchResponse{}, invalidAuth()
	}
	if _, err := cfg.parseCredential(req.StorageJSON); err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	return pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{DisplayName: "Amazon QuickWork — quota unknown"}}}, nil
}

type engine struct {
	lifecycle  sync.Mutex
	mu         sync.Mutex
	generation *generation
}

type generation struct {
	settings
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	client *http.Client
}

func (e *engine) swap(cfg *settings) {
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	e.mu.Lock()
	old := e.generation
	if cfg != nil && old != nil && old.Enabled == cfg.Enabled && old.Timeout == cfg.Timeout && maps.Equal(old.Origins, cfg.Origins) {
		// CPA may reconfigure during unrelated changes, such as auth-file imports.
		e.mu.Unlock()
		return
	}
	e.generation = nil
	if cfg != nil {
		ctx, cancel := context.WithCancel(context.Background())
		e.generation = &generation{settings: *cfg, ctx: ctx, cancel: cancel, client: bridgeClient()}
	}
	e.mu.Unlock()
	if old != nil {
		old.cancel()
		old.wg.Wait()
		old.client.CloseIdleConnections()
	}
}

func (e *engine) stop() { e.swap(nil) }

type hostCallback func(string, any) bool

func decode(raw []byte, v any) *pluginabi.Error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if p := bytes.TrimSpace(raw); len(p) == 0 || p[0] != '{' || json.Unmarshal(raw, v) != nil {
		return pluginabi.NewError("invalid_request", "Invalid QuickWork RPC payload", 400)
	}
	return nil
}

func envelope(value any, failure *pluginabi.Error) []byte {
	if failure == nil {
		encoded, err := json.Marshal(value)
		if err == nil {
			result, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: encoded})
			return result
		}
		failure = pluginabi.NewError("internal_error", "QuickWork response encoding failed", 500)
	}
	encoded, _ := json.Marshal(pluginabi.Envelope{Error: failure})
	return encoded
}

func (e *engine) call(method string, raw []byte, host hostCallback) (any, *pluginabi.Error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var request struct {
			ConfigYAML    []byte `json:"config_yaml"`
			SchemaVersion uint32 `json:"schema_version"`
		}
		if err := decode(raw, &request); err != nil {
			return nil, err
		}
		if request.SchemaVersion != pluginabi.SchemaVersion {
			return nil, pluginabi.NewError("unsupported_schema", "QuickWork plugin requires the current CPA RPC schema", 400)
		}
		cfg, err := parseSettings(request.ConfigYAML)
		if err != nil {
			return nil, err
		}
		e.swap(&cfg)
		return registration(), nil
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		e.stop()
		return struct{}{}, nil
	}
	e.mu.Lock()
	run := e.generation
	if run != nil {
		run.wg.Add(1)
	}
	e.mu.Unlock()
	if run == nil {
		return nil, pluginabi.NewError("unavailable", "QuickWork plugin unavailable", 503)
	}
	defer run.wg.Done()

	switch method {
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier:
		return map[string]any{"identifier": id, "credential_providers": []string{id}}, nil
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		return run.parseAuth(req)
	case pluginabi.MethodAuthRefresh:
		var req pluginapi.AuthRefreshRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		if req.AuthProvider != id || req.AuthID == "" {
			return nil, invalidAuth()
		}
		if _, err := run.parseCredential(req.StorageJSON); err != nil {
			return nil, err
		}
		return pluginapi.AuthRefreshResponse{NextRefreshAfter: time.Now().Add(24 * time.Hour)}, nil
	case pluginabi.MethodModelStatic:
		return models(), nil
	case pluginabi.MethodModelForAuth:
		var req pluginapi.AuthModelRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		if req.AuthProvider != id {
			return nil, invalidAuth()
		}
		if _, err := run.parseCredential(req.StorageJSON); err != nil {
			return nil, err
		}
		return models(), nil
	case pluginabi.MethodQuotaDescribe:
		return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{id}, DisplayName: name}, nil
	case pluginabi.MethodQuotaFetch:
		var req pluginapi.QuotaFetchRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		return run.fetchQuota(req)
	case pluginabi.MethodExecutorExecute, pluginabi.MethodExecutorExecuteStream:
		var req struct {
			pluginapi.ExecutorRequest
			StreamID string `json:"stream_id"`
		}
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		if method == pluginabi.MethodExecutorExecute {
			return run.execute(req.ExecutorRequest)
		}
		return run.executeStream(req.ExecutorRequest, req.StreamID, host)
	case pluginabi.MethodAuthLoginStart, pluginabi.MethodAuthLoginPoll, pluginabi.MethodQuotaReset, pluginabi.MethodExecutorCountTokens, pluginabi.MethodExecutorHTTPRequest:
		return nil, pluginabi.NewError("unsupported_operation", "Operation not supported by the QuickWork bridge plugin", 501)
	default:
		return nil, pluginabi.NewError("method_not_found", "Unknown QuickWork plugin method", 404)
	}
}
