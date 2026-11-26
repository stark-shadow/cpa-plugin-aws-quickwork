package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
)

func validateProxySetting(raw string) error {
	setting, err := proxyutil.Parse(raw)
	if err != nil {
		return err
	}
	if setting.Mode != proxyutil.ModeProxy || setting.URL == nil {
		return nil
	}
	u := setting.URL
	if u.Opaque != "" || u.Hostname() == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("expected a proxy URL without a path, query, or fragment")
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return errors.New("proxy port must be between 1 and 65535")
		}
	}
	if u.User != nil && (u.Scheme == "socks5" || u.Scheme == "socks5h") {
		password, _ := u.User.Password()
		if len(u.User.Username()) > 255 || len(password) > 255 {
			return errors.New("SOCKS5 credentials must not exceed 255 bytes each")
		}
	}
	return nil
}

func effectiveProxyURL(candidates ...string) string {
	for _, raw := range candidates {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func newWebSocketDialer(proxyRaw string) (websocket.Dialer, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second}
	setting, err := proxyutil.Parse(proxyRaw)
	if err != nil {
		return dialer, err
	}
	switch setting.Mode {
	case proxyutil.ModeInherit:
		dialer.Proxy = http.ProxyFromEnvironment
		return dialer, nil
	case proxyutil.ModeDirect:
		dialer.Proxy = nil
		return dialer, nil
	case proxyutil.ModeProxy:
		if err := validateProxySetting(proxyRaw); err != nil {
			return dialer, err
		}
		if setting.URL.Scheme == "http" {
			dialer.Proxy = http.ProxyURL(setting.URL)
			return dialer, nil
		}
		proxyDialer, _, err := proxyutil.BuildDialer(proxyRaw)
		if err != nil {
			return dialer, err
		}
		dialer.Proxy = nil
		if ctxDialer, ok := proxyDialer.(interface {
			DialContext(context.Context, string, string) (net.Conn, error)
		}); ok {
			dialer.NetDialContext = ctxDialer.DialContext
		} else {
			dialer.NetDialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
				return proxyDialer.Dial(network, addr)
			}
		}
		return dialer, nil
	default:
		return dialer, errors.New("invalid proxy setting")
	}
}

func outboundHTTPClient(proxyRaw string, timeout time.Duration) (*http.Client, error) {
	if strings.TrimSpace(proxyRaw) != "" {
		if err := validateProxySetting(proxyRaw); err != nil {
			return nil, err
		}
	}
	transport, mode, err := proxyutil.BuildHTTPTransport(proxyRaw)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if mode != proxyutil.ModeInherit && transport != nil {
		client.Transport = transport
	}
	return client, nil
}

func doOutboundHTTP(ctx context.Context, transport oauthTransport, proxyRaw string, timeout time.Duration, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if strings.TrimSpace(proxyRaw) == "" && transport.send != nil && transport.callbackID != "" {
		return transport.send(transport.callbackID, req)
	}
	client, err := outboundHTTPClient(proxyRaw, timeout)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	if req.Headers != nil {
		httpReq.Header = req.Headers.Clone()
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	defer resp.Body.Close()
	const maxResponseBytes = 2 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	if len(body) > maxResponseBytes {
		return pluginapi.HTTPResponse{}, errors.New("upstream HTTP response exceeded limit")
	}
	return pluginapi.HTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header.Clone(),
		Body:       body,
	}, nil
}
