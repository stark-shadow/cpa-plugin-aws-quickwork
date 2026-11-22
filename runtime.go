package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	id             = "aws-quickwork"
	name           = "Amazon QuickWork"
	version        = "0.2.1"
	maxRPC         = 4 << 20
	personal       = id + "-personal"
	enterprise     = id + "-enterprise"
	discoveryRoute = "/v0/management/plugins/" + id + "/oauth/discovery"
)

var modes = []string{"smart", "fast", "balanced", "auto"}

type settings struct {
	Enabled bool
	Timeout time.Duration
}

func parseSettings(raw []byte) (settings, *pluginabi.Error) {
	cfg := settings{Enabled: true, Timeout: 2 * time.Minute}
	if len(bytes.TrimSpace(raw)) == 0 {
		return cfg, nil
	}
	var fields struct {
		Enabled  *bool     `yaml:"enabled"`
		Priority int       `yaml:"priority"`
		Store    yaml.Node `yaml:"store"`
		Timeout  string    `yaml:"request_timeout"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	bad := pluginabi.NewError("invalid_config", "Invalid QuickWork config; remove legacy bridge_origins", 400)
	if decoder.Decode(&fields) != nil || decoder.Decode(new(any)) != io.EOF {
		return settings{}, bad
	}
	if fields.Enabled != nil {
		cfg.Enabled = *fields.Enabled
	}
	if fields.Timeout != "" {
		span, err := time.ParseDuration(fields.Timeout)
		if err != nil || span < time.Second || span > 5*time.Minute {
			return settings{}, bad
		}
		cfg.Timeout = span
	}
	return cfg, nil
}

type engine struct {
	lifecycle  sync.Mutex
	mu         sync.Mutex
	generation *generation
}

type generation struct {
	settings
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	flows     sync.Map
	refreshMu sync.Mutex
	refreshes map[string]*refreshFlight
}

func (e *engine) swap(cfg *settings) {
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	e.mu.Lock()
	old := e.generation
	if old != nil && cfg != nil && old.settings == *cfg {
		e.mu.Unlock()
		return
	}
	e.generation = nil
	if cfg != nil {
		ctx, cancel := context.WithCancel(context.Background())
		e.generation = &generation{settings: *cfg, ctx: ctx, cancel: cancel}
	}
	e.mu.Unlock()
	if old != nil {
		old.cancel()
		old.wg.Wait()
	}
}

func (e *engine) stop() { e.swap(nil) }

type hostCallback func(string, any) bool
type hostHTTP func(string, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)

func decode(raw []byte, value any) *pluginabi.Error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(raw, value) != nil {
		return pluginabi.NewError("invalid_request", "Invalid QuickWork RPC payload", 400)
	}
	return nil
}

func envelope(value any, failure *pluginabi.Error) []byte {
	if failure == nil {
		result, err := json.Marshal(value)
		if err == nil {
			data, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: result})
			return data
		}
		failure = pluginabi.NewError("internal_error", "QuickWork response encoding failed", 500)
	}
	data, _ := json.Marshal(pluginabi.Envelope{Error: failure})
	return data
}

func models() pluginapi.ModelResponse {
	list := make([]pluginapi.ModelInfo, 0, len(modes))
	for _, mode := range modes {
		model := id + "-" + mode
		list = append(list, pluginapi.ModelInfo{ID: model, Object: "model", OwnedBy: id, Type: id, Name: model, DisplayName: name + " (" + mode + ")"})
	}
	return pluginapi.ModelResponse{Provider: id, Models: list}
}

func registration() any {
	return map[string]any{"schema_version": pluginabi.SchemaVersion,
		"metadata": pluginapi.Metadata{Name: name, Version: version, Author: "stark-shadow", GitHubRepository: "https://github.com/stark-shadow/cpa-plugin-aws-quickwork", ConfigFields: []pluginapi.ConfigField{
			{Name: "request_timeout", Type: pluginapi.ConfigFieldTypeString, Default: "2m", Description: "Total upstream chat deadline (1s to 5m)."},
		}},
		"capabilities": map[string]any{"auth_provider": true, "model_provider": true, "executor": true, "quota_provider": true, "management_api": true,
			"executor_model_scope": pluginapi.ExecutorModelScopeOAuth, "executor_input_formats": []string{"openai"}, "executor_output_formats": []string{"openai"}},
	}
}

func (e *engine) call(method string, raw []byte, host hostCallback, transport ...hostHTTP) (any, *pluginabi.Error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req struct {
			ConfigYAML    []byte `json:"config_yaml"`
			SchemaVersion uint32 `json:"schema_version"`
		}
		if failure := decode(raw, &req); failure != nil {
			return nil, failure
		}
		if req.SchemaVersion != pluginabi.SchemaVersion {
			return nil, pluginabi.NewError("unsupported_schema", "QuickWork requires CPA RPC schema 10", 400)
		}
		cfg, failure := parseSettings(req.ConfigYAML)
		if failure != nil {
			return nil, failure
		}
		e.swap(&cfg)
		return registration(), nil
	case pluginabi.MethodPluginShutdown, pluginabi.MethodPluginQuiesce:
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
	var sender hostHTTP
	if len(transport) != 0 {
		sender = transport[0]
	}
	var rpc struct {
		CallbackID string `json:"host_callback_id"`
	}
	if failure := decode(raw, &rpc); failure != nil {
		return nil, failure
	}
	authHTTP := oauthTransport{send: sender, callbackID: rpc.CallbackID}
	switch method {
	case pluginabi.MethodAuthIdentifier:
		return map[string]any{"identifier": personal, "identifiers": []string{personal, enterprise}, "credential_providers": []string{id}, "entries": []pluginapi.AuthProviderEntry{
			{Identifier: personal, Name: "QuickWork Personal", Logo: publicResourceBase + "/personal.svg"},
			{Identifier: enterprise, Name: "QuickWork Enterprise (SSO)", Logo: publicResourceBase + "/enterprise.svg"},
		}}, nil
	case pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier:
		return map[string]string{"identifier": id}, nil
	case pluginabi.MethodManagementRegister:
		return map[string]any{"routes": []map[string]string{{"method": "POST", "path": discoveryRoute}}, "resources": []map[string]string{{"path": "/omc.json"}, {"path": "/personal.svg"}, {"path": "/enterprise.svg"}}}, nil
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if failure := decode(raw, &req); failure != nil {
			return nil, failure
		}
		if req.Path == discoveryRoute {
			return run.remoteDiscovery(req, authHTTP), nil
		}
		return publicResource(req), nil
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if failure := decode(raw, &req); failure != nil {
			return nil, failure
		}
		return parseAuth(req)
	case pluginabi.MethodAuthLoginStart:
		var req pluginapi.AuthLoginStartRequest
		if failure := decode(raw, &req); failure != nil {
			return nil, failure
		}
		return run.startLogin(req)
	case pluginabi.MethodAuthLoginPoll:
		var req pluginapi.AuthLoginPollRequest
		if failure := decode(raw, &req); failure != nil {
			return nil, failure
		}
		return run.pollLogin(req, authHTTP)
	case pluginabi.MethodAuthRefresh:
		var req pluginapi.AuthRefreshRequest
		if failure := decode(raw, &req); failure != nil {
			return nil, failure
		}
		return run.refresh(req, authHTTP)
	case pluginabi.MethodModelStatic:
		return models(), nil
	case pluginabi.MethodModelForAuth:
		var req pluginapi.AuthModelRequest
		if failure := decode(raw, &req); failure != nil {
			return nil, failure
		}
		if _, failure := readCredential(req.AuthProvider, req.StorageJSON); failure != nil {
			return nil, failure
		}
		return models(), nil
	case pluginabi.MethodQuotaDescribe:
		return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{id}, DisplayName: name}, nil
	case pluginabi.MethodQuotaFetch:
		var req pluginapi.QuotaFetchRequest
		if failure := decode(raw, &req); failure != nil {
			return nil, failure
		}
		if strings.TrimSpace(req.AuthID) == "" || strings.TrimSpace(req.AuthIndex) == "" {
			return nil, badCredential()
		}
		if _, failure := readCredential(req.Provider, req.StorageJSON); failure != nil {
			return nil, failure
		}
		return nil, pluginabi.NewError("quota_unavailable", "QuickWork 没有向此登录凭据提供账户剩余额度；请在 Amazon Quick 中查看。此状态不代表额度为零。 / Account balance is not available to this OAuth credential.", 501)
	case pluginabi.MethodExecutorExecute, pluginabi.MethodExecutorExecuteStream:
		var req struct {
			pluginapi.ExecutorRequest
			StreamID string `json:"stream_id"`
		}
		if failure := decode(raw, &req); failure != nil {
			return nil, failure
		}
		if method == pluginabi.MethodExecutorExecuteStream {
			return run.executeStream(req.ExecutorRequest, req.StreamID, host)
		}
		return run.execute(req.ExecutorRequest)
	case pluginabi.MethodQuotaReset, pluginabi.MethodExecutorCountTokens, pluginabi.MethodExecutorHTTPRequest:
		return nil, pluginabi.NewError("unsupported_operation", "Operation not supported by QuickWork", 501)
	default:
		return nil, pluginabi.NewError("method_not_found", "Unknown QuickWork plugin method", 404)
	}
}
