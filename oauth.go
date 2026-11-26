package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	appVersion     = "1.2.4660"
	personalTenant = "https://cell001.dp.appintegrations.us-east-1.prod.plato.ai.aws.dev"
	personalScope  = "quicksight:read_write"
	loginLifetime  = 10 * time.Minute
)

var regionPattern = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+){1,3}-[0-9]+$`)

type credential struct {
	Type         string    `json:"type"`
	Portal       string    `json:"oauth_provider"`
	Tenant       string    `json:"tenant"`
	TokenURL     string    `json:"token_url"`
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret,omitempty"`
	Access       string    `json:"access_token"`
	Refresh      string    `json:"refresh_token"`
	Identity     string    `json:"id_token,omitempty"`
	Expires      time.Time `json:"expires_at"`
	Actor        string    `json:"actor"`
	AWSAccountID string    `json:"aws_account_id,omitempty"`
	Region       string    `json:"region,omitempty"`
	Email        string    `json:"email,omitempty"`
	Label        string    `json:"label,omitempty"`
	Prefix       string    `json:"prefix,omitempty"`
	ProxyURL     string    `json:"proxy_url,omitempty"`
	Disabled     bool      `json:"disabled,omitempty"`
}

func (c credential) bearer() string {
	if c.Identity != "" {
		return c.Identity
	}
	return c.Access
}

func quickURL(raw string) (*url.URL, bool) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "https" || endpoint.User != nil || endpoint.Hostname() == "" || endpoint.Fragment != "" || endpoint.Opaque != "" || (endpoint.Port() != "" && endpoint.Port() != "443") {
		return nil, false
	}
	host := strings.ToLower(endpoint.Hostname())
	for _, allowed := range []string{"quick.aws.com", "quicksight.aws.amazon.com", "plato.ai.aws.dev", "signin.aws.amazon.com"} {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return endpoint, true
		}
	}
	if middle, ok := strings.CutPrefix(host, "oidc."); ok {
		if region, ok := strings.CutSuffix(middle, ".amazonaws.com"); ok && regionPattern.MatchString(region) {
			return endpoint, true
		}
	}
	return nil, false
}

func validTenant(raw string) (*url.URL, bool) {
	tenant, ok := quickURL(raw)
	if !ok || tenant.RawQuery != "" || (tenant.Path != "" && tenant.Path != "/") {
		return nil, false
	}
	host := strings.ToLower(tenant.Hostname())
	return tenant, strings.HasSuffix(host, ".prod.plato.ai.aws.dev") || host == "quicksight.aws.amazon.com" || strings.HasSuffix(host, ".quicksight.aws.amazon.com")
}

func badCredential() *pluginabi.Error {
	return pluginabi.NewError("reauth_required", "QuickWork requires a native OAuth credential; sign in using the CPA panel (bridge keys are no longer accepted)", 401)
}

func expiredCredential() *pluginabi.Error {
	return pluginabi.NewError("reauth_required", "QuickWork OAuth token has expired and could not be refreshed; please sign in again in the CPA panel", 401)
}

func readCredential(provider string, raw []byte) (credential, *pluginabi.Error) {
	var saved credential
	if provider != id || len(raw) > 128<<10 || json.Unmarshal(raw, &saved) != nil || saved.Type != id || (saved.Portal != personal && saved.Portal != enterprise) || saved.ClientID == "" || saved.Expires.IsZero() || saved.bearer() == "" || saved.Actor == "" {
		return credential{}, badCredential()
	}
	if _, ok := validTenant(saved.Tenant); !ok {
		return credential{}, badCredential()
	}
	if _, ok := quickURL(saved.TokenURL); !ok {
		return credential{}, badCredential()
	}
	return saved, nil
}

func parseAuth(req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, *pluginabi.Error) {
	var marker struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(req.RawJSON, &marker) != nil || marker.Type != id {
		return pluginapi.AuthParseResponse{}, nil
	}
	saved, failure := readCredential(id, req.RawJSON)
	if failure != nil {
		return pluginapi.AuthParseResponse{}, failure
	}
	result := saved.auth(req.RawJSON)
	result.FileName = req.FileName
	return pluginapi.AuthParseResponse{Handled: true, Auth: result}, nil
}

func nextRefreshAt(expires time.Time) time.Time {
	remaining := time.Until(expires)
	margin := 5 * time.Minute
	if remaining < 10*time.Minute {
		margin = 2 * time.Minute
	}
	return expires.Add(-margin)
}

func (c credential) auth(storage []byte) pluginapi.AuthData {
	if storage == nil {
		storage, _ = json.Marshal(c)
	}
	fingerprint := sha256.Sum256([]byte(c.Portal + "\x00" + c.Tenant + "\x00" + c.Actor))
	filename := id + "-" + hex.EncodeToString(fingerprint[:12]) + ".json"
	return pluginapi.AuthData{
		Provider:    id,
		ID:          filename,
		FileName:    filename,
		Label:       c.Label,
		Prefix:      c.Prefix,
		Disabled:    c.Disabled,
		ProxyURL:    c.ProxyURL,
		StorageJSON: storage,
		Metadata: map[string]any{
			"type":                     id,
			"oauth_provider":           c.Portal,
			"email":                    c.Email,
			"expires_at":               c.Expires.UTC().Format(time.RFC3339),
			"refresh_interval_seconds": 300,
		},
		Attributes: map[string]string{
			"refresh_interval_seconds": "300",
		},
		NextRefreshAfter: nextRefreshAt(c.Expires),
	}
}

type oauthTransport struct {
	send       hostHTTP
	callbackID string
	proxyURL   string
	ctx        context.Context
}

func (transport oauthTransport) withProxy(ctx context.Context, proxyURL string) oauthTransport {
	copy := transport
	copy.ctx = ctx
	copy.proxyURL = strings.TrimSpace(proxyURL)
	return copy
}

func (transport oauthTransport) post(endpoint string, body any, access string) (map[string]any, *pluginabi.Error) {
	failure := pluginabi.NewError("oauth_upstream", "QuickWork OAuth upstream request failed", 502)
	if _, ok := quickURL(endpoint); !ok {
		return nil, failure
	}
	if strings.TrimSpace(transport.proxyURL) == "" && (transport.send == nil || transport.callbackID == "") {
		return nil, failure
	}
	media := "application/json"
	var data []byte
	if form, ok := body.(url.Values); ok {
		media = "application/x-www-form-urlencoded"
		data = []byte(form.Encode())
	} else {
		data, _ = json.Marshal(body)
	}
	headers := http.Header{"Content-Type": {media}, "Accept": {"application/json"}}
	if access != "" {
		headers.Set("Authorization", "Bearer "+access)
	}
	ctx := transport.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	response, err := doOutboundHTTP(ctx, transport, transport.proxyURL, 30*time.Second, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     endpoint,
		Headers: headers,
		Body:    data,
	})
	if err != nil || len(response.Body) > 512<<10 {
		return nil, failure
	}
	var decoded map[string]any
	if json.Unmarshal(response.Body, &decoded) != nil || decoded == nil {
		return nil, failure
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		switch normalizeErrorCode(firstString(decoded, "error", "code", "__type")) {
		case "invalidgrant", "expiredtoken", "invalidclient", "unauthorizedclient", "accessdenied":
			return nil, expiredCredential()
		}
		if response.StatusCode == 401 {
			return nil, expiredCredential()
		}
		return nil, failure
	}
	return decoded, nil
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := values[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func firstInt64(values map[string]any, keys ...string) int64 {
	for _, key := range keys {
		switch v := values[key].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case int:
			return int64(v)
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return n
			}
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}

func decodeJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload := parts[1]
	padded := payload + strings.Repeat("=", (4-len(payload)%4)%4)
	body, err := base64.URLEncoding.DecodeString(padded)
	if err != nil {
		body, err = base64.RawURLEncoding.DecodeString(payload)
		if err != nil {
			return nil
		}
	}
	var claims map[string]any
	if json.Unmarshal(body, &claims) != nil {
		return nil
	}
	return claims
}

func (c *credential) tokens(transport oauthTransport, exchange any) *pluginabi.Error {
	data, failure := transport.post(c.TokenURL, exchange, "")
	if failure != nil {
		return failure
	}
	c.Access = firstString(data, "access_token", "accessToken")
	// Refresh responses are not guaranteed to repeat id_token; preserve the
	// previous one so WebSocket auth does not silently fall back to access_token.
	if nextID := firstString(data, "id_token", "idToken"); nextID != "" {
		c.Identity = nextID
	}
	if nextRefresh := firstString(data, "refresh_token", "refreshToken"); nextRefresh != "" {
		c.Refresh = nextRefresh
	}
	if c.Access == "" || c.Refresh == "" {
		return badCredential()
	}

	now := time.Now()
	c.Expires = time.Time{}
	if seconds := firstInt64(data, "expires_in", "expiresIn"); seconds > 0 && seconds < 86400*30 {
		c.Expires = now.Add(time.Duration(seconds) * time.Second)
	}
	// The WebSocket uses the ID token when present; refresh before whichever
	// token expires first.
	if claims := decodeJWTClaims(c.bearer()); claims != nil {
		if exp := firstInt64(claims, "exp"); exp > now.Unix() {
			jwtExpiry := time.Unix(exp, 0)
			if c.Expires.IsZero() || jwtExpiry.Before(c.Expires) {
				c.Expires = jwtExpiry
			}
		}
	}
	if c.Expires.IsZero() && c.Refresh != "" {
		c.Expires = now.Add(50 * time.Minute)
	}
	if c.Expires.IsZero() {
		return pluginabi.NewError("oauth_protocol", "QuickWork did not return a token expiry", 502)
	}
	return nil
}

func (c *credential) loadIdentity(transport oauthTransport) *pluginabi.Error {
	origin := c.Tenant
	if c.Portal == personal && !strings.Contains(origin, ".dp.appintegrations.") {
		origin = personalTenant
	}
	data, failure := transport.post(strings.TrimRight(origin, "/")+"/integration/quick-work/user-info", map[string]any{}, c.Access)
	if failure != nil {
		return failure
	}
	if inner, ok := data["userInfo"].(map[string]any); ok {
		data = inner
	}
	claims := decodeJWTClaims(c.Identity)
	if claims == nil {
		claims = decodeJWTClaims(c.Access)
	}
	c.Actor = firstString(data, "userArn", "user_arn", "quicksightExternalUserArn", "quicksight_external_user_arn")
	if c.Actor == "" {
		if sub := firstString(claims, "sub"); strings.HasPrefix(sub, "arn:") {
			c.Actor = sub
		}
	}
	c.Email = firstString(data, "email", "emailAddress")
	c.Label = firstString(data, "displayName", "display_name", "email", "emailAddress", "userName", "user_name", "username")
	if acctID := firstString(data, "awsAccountId", "aws_account_id", "accountId"); acctID != "" {
		c.AWSAccountID = acctID
	} else if acctID := firstString(claims, "aws_account_id"); acctID != "" {
		c.AWSAccountID = acctID
	}
	if c.Region == "" {
		if reg := firstString(claims, "region"); regionPattern.MatchString(reg) {
			c.Region = reg
		}
	}
	if c.Actor == "" {
		return pluginabi.NewError("oauth_protocol", "QuickWork did not identify the signed-in account", 502)
	}
	return nil
}

type loginFlow struct {
	mu                                                        sync.Mutex
	portal, state, nonce, verifier, redirect, region, authDir string
	until                                                     time.Time
	discovered                                                url.Values
	code                                                      string
	denied                                                    bool
	authorize                                                 string
	saved                                                     credential
	terminal                                                  *pluginabi.Error
	completed                                                 *pluginapi.AuthData
}

func loginInput() *pluginabi.Error {
	return pluginabi.NewError("invalid_oauth_callback", "Invalid or expired QuickWork login callback", 400)
}

func keyMatches(actual, expected string) bool {
	return actual != "" && subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

func (g *generation) startLogin(req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, *pluginabi.Error) {
	var empty pluginapi.AuthLoginStartResponse
	if !g.Enabled || (req.Provider != personal && req.Provider != enterprise) || req.Host.AuthDir == "" {
		return empty, loginInput()
	}
	region := firstString(req.Metadata, "region")
	if region == "" {
		region = "us-east-1"
	}
	if !regionPattern.MatchString(region) {
		return empty, loginInput()
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return empty, pluginabi.NewError("callback_listener", "Unable to open QuickWork loopback callback", 503)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	flow := &loginFlow{portal: req.Provider, state: rand.Text(), nonce: rand.Text(), verifier: rand.Text() + rand.Text(), region: region, authDir: req.Host.AuthDir, until: time.Now().Add(loginLifetime)}
	flow.redirect = "http://" + listener.Addr().String()
	portal := "https://quick.aws.com/sn"
	if flow.portal == enterprise {
		flow.redirect = "http://" + net.JoinHostPort("localhost", strconv.Itoa(port))
		prefix := ""
		if region != "us-east-1" {
			prefix = region + "."
		}
		portal = "https://" + prefix + "quicksight.aws.amazon.com/sn/start/app-integrations/quickwork-enterprise-login"
	}
	surface := "desktop:" + flow.nonce + ":" + strconv.Itoa(port) + ":" + appVersion
	loginURL := portal + "?" + url.Values{"qwSurface": {surface}}.Encode()
	g.flows.Store(flow.state, flow)
	server := &http.Server{Handler: http.HandlerFunc(flow.localCallback), ReadHeaderTimeout: 3 * time.Second, WriteTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	stop := context.AfterFunc(g.ctx, func() { _ = server.Close() })
	deadline := time.AfterFunc(loginLifetime, func() { _ = server.Close() })
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer stop()
		defer deadline.Stop()
		defer g.flows.Delete(flow.state)
		defer flow.removeCallback()
		_ = server.Serve(listener)
	}()
	return pluginapi.AuthLoginStartResponse{Provider: flow.portal, URL: loginURL, State: flow.state, ExpiresAt: flow.until}, nil
}

func (f *loginFlow) localCallback(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if req.Method != http.MethodGet || len(req.URL.RawQuery) > 16<<10 {
		callbackPage(w, req, http.StatusBadRequest, "invalid", "")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	query := req.URL.Query()
	if time.Now().After(f.until) || f.terminal != nil {
		callbackPage(w, req, http.StatusBadRequest, "invalid", "")
		return
	}
	switch req.URL.Path {
	case "/callback":
		if !keyMatches(query.Get("nonce"), f.nonce) || len(query["nonce"]) != 1 {
			callbackPage(w, req, http.StatusBadRequest, "invalid", "")
			return
		}
		if f.discovered == nil {
			f.discovered = query
		}
		fallthrough
	case "/continue":
		if !keyMatches(query.Get("nonce"), f.nonce) {
			callbackPage(w, req, http.StatusBadRequest, "invalid", "")
			return
		}
		if f.authorize != "" {
			http.Redirect(w, req, f.authorize, http.StatusSeeOther)
			return
		}
		next := "/continue?" + url.Values{"nonce": {f.nonce}}.Encode()
		w.Header().Set("Refresh", "1; url="+next)
		callbackPage(w, req, http.StatusOK, "discover", next)
	case "/":
		if !keyMatches(query.Get("state"), f.state) || f.authorize == "" || len(query["state"]) != 1 || len(query["code"]) > 1 || len(query["error"]) > 1 || (query.Get("code") == "" && query.Get("error") == "") || (query.Get("code") != "" && query.Get("error") != "") {
			callbackPage(w, req, http.StatusBadRequest, "invalid", "")
			return
		}
		if f.completed == nil && f.code == "" && !f.denied {
			f.code = query.Get("code")
			f.denied = query.Get("error") != ""
		}
		if f.denied {
			callbackPage(w, req, http.StatusOK, "denied", "")
			return
		}
		callbackPage(w, req, http.StatusOK, "received", "")
	default:
		http.NotFound(w, req)
	}
}

func (f *loginFlow) processDiscovery(query url.Values, transport oauthTransport) *pluginabi.Error {
	if !keyMatches(query.Get("nonce"), f.nonce) || time.Now().After(f.until) || len(query["nonce"]) != 1 {
		return loginInput()
	}
	if f.authorize != "" {
		return nil
	}
	if query.Get("error") != "" {
		return loginInput()
	}
	saved := credential{Type: id, Portal: f.portal, Region: f.region}
	field := func(keys ...string) string {
		for _, key := range keys {
			if value := strings.TrimSpace(query.Get(key)); value != "" {
				return value
			}
		}
		return ""
	}
	saved.Tenant = field("tenantUrl", "tenant_url")
	if saved.Tenant == "" && f.portal == personal {
		saved.Tenant = personalTenant
	}
	if saved.Tenant != "" && !strings.Contains(saved.Tenant, "://") {
		saved.Tenant = "https://" + saved.Tenant
	}
	if _, ok := validTenant(saved.Tenant); !ok {
		return loginInput()
	}
	if discoveredRegion := field("region"); regionPattern.MatchString(discoveredRegion) {
		saved.Region = discoveredRegion
	}
	var authorize string
	scope := personalScope
	extra := url.Values{}
	if f.portal == enterprise {
		authorize = field("authEndpoint", "auth_endpoint")
		saved.TokenURL = field("tokenEndpoint", "token_endpoint")
		saved.ClientID = field("clientId", "client_id")
		saved.ClientSecret = field("clientSecret", "client_secret")
		scope = "openid profile email"
		extra.Set("access_type", "offline")
		extra.Set("prompt", "consent")
	} else {
		region := field("idcRegion", "idc_region")
		if region == "" {
			region = f.region
		}
		if !regionPattern.MatchString(region) {
			return loginInput()
		}
		issuer := field("issuerUrl", "issuer_url", "issuer")
		issuerURL, err := url.Parse(issuer)
		if err != nil || issuerURL.Scheme != "https" || issuerURL.Hostname() == "" || issuerURL.User != nil {
			return loginInput()
		}
		base := "https://oidc." + region + ".amazonaws.com"
		authorize, saved.TokenURL = base+"/authorize", base+"/token"
		data, failure := transport.post(base+"/client/register", map[string]any{"clientName": "AmazonQuick-Desktop", "clientType": "public", "scopes": []string{scope}, "grantTypes": []string{"authorization_code", "refresh_token"}, "redirectUris": []string{f.redirect}, "issuerUrl": issuer}, "")
		if failure != nil {
			return failure
		}
		saved.ClientID = firstString(data, "clientId", "client_id")
		saved.ClientSecret = firstString(data, "clientSecret", "client_secret")
	}
	endpoint, ok := quickURL(authorize)
	if !ok || saved.ClientID == "" {
		return loginInput()
	}
	if _, ok := quickURL(saved.TokenURL); !ok {
		return loginInput()
	}
	challenge := sha256.Sum256([]byte(f.verifier))
	for key, value := range endpoint.Query() {
		extra[key] = value
	}
	for key, value := range map[string]string{"response_type": "code", "client_id": saved.ClientID, "redirect_uri": f.redirect, "scope": scope, "state": f.state, "code_challenge": base64.RawURLEncoding.EncodeToString(challenge[:]), "code_challenge_method": "S256"} {
		extra.Set(key, value)
	}
	endpoint.RawQuery = extra.Encode()
	f.saved = saved
	f.authorize = endpoint.String()
	return nil
}

func (f *loginFlow) callbackFile() string {
	return filepath.Join(f.authDir, ".oauth-"+f.portal+"-"+f.state+".oauth")
}

func (f *loginFlow) removeCallback() { _ = os.Remove(f.callbackFile()) }

func (f *loginFlow) acceptNativeCallback() {
	file, err := os.Open(f.callbackFile())
	if err != nil {
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, 16<<10))
	_ = file.Close()
	f.removeCallback()
	var callback struct {
		Code  string `json:"code"`
		State string `json:"state"`
		Error string `json:"error"`
	}
	if err == nil && json.Unmarshal(data, &callback) == nil && keyMatches(callback.State, f.state) {
		f.code = callback.Code
		f.denied = callback.Error != ""
	}
}

func (g *generation) pollLogin(req pluginapi.AuthLoginPollRequest, transport oauthTransport) (pluginapi.AuthLoginPollResponse, *pluginabi.Error) {
	result := pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}
	item, ok := g.flows.Load(req.State)
	if !ok {
		return result, loginInput()
	}
	f := item.(*loginFlow)
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Provider != f.portal || time.Now().After(f.until) {
		return result, loginInput()
	}
	if f.completed != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: *f.completed}, nil
	}
	if f.terminal != nil {
		return result, f.terminal
	}
	if f.discovered != nil && f.authorize == "" {
		if failure := f.processDiscovery(f.discovered, transport); failure != nil {
			f.terminal = failure
			return result, failure
		}
	}
	if f.authorize == "" {
		return result, nil
	}
	if f.code == "" && !f.denied {
		f.acceptNativeCallback()
	}
	if f.denied {
		f.terminal = loginInput()
		return result, f.terminal
	}
	if f.code == "" {
		return result, nil
	}
	var exchange any
	if f.portal == enterprise {
		form := url.Values{"grant_type": {"authorization_code"}, "client_id": {f.saved.ClientID}, "code": {f.code}, "redirect_uri": {f.redirect}, "code_verifier": {f.verifier}}
		if f.saved.ClientSecret != "" {
			form.Set("client_secret", f.saved.ClientSecret)
		}
		exchange = form
	} else {
		exchange = map[string]string{"grantType": "authorization_code", "clientId": f.saved.ClientID, "clientSecret": f.saved.ClientSecret, "code": f.code, "redirectUri": f.redirect, "codeVerifier": f.verifier}
	}
	f.code = ""
	if failure := f.saved.tokens(transport, exchange); failure != nil {
		f.terminal = failure
		return result, failure
	}
	if failure := f.saved.loadIdentity(transport); failure != nil {
		f.terminal = failure
		return result, failure
	}
	auth := f.saved.auth(nil)
	f.completed = &auth
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: auth}, nil
}

func (g *generation) remoteDiscovery(req pluginapi.ManagementRequest, transport oauthTransport) pluginapi.ManagementResponse {
	reply := func(status int, fields map[string]string) pluginapi.ManagementResponse {
		body, _ := json.Marshal(fields)
		return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: body}
	}
	bad := func() pluginapi.ManagementResponse {
		return reply(400, map[string]string{"error": loginInput().Message})
	}
	if req.Method != http.MethodPost {
		return reply(405, map[string]string{"error": "method not allowed"})
	}
	var submitted struct {
		State       string `json:"state"`
		RedirectURL string `json:"redirect_url"`
	}
	if len(req.Body) > 16<<10 || json.Unmarshal(req.Body, &submitted) != nil {
		return bad()
	}
	item, ok := g.flows.Load(submitted.State)
	if !ok {
		return bad()
	}
	f := item.(*loginFlow)
	rawURL := strings.TrimSpace(submitted.RedirectURL)
	rawURL = strings.Trim(rawURL, "\"'<>`")
	rawURL = strings.ReplaceAll(rawURL, "&amp;", "&")
	rawURL = strings.ReplaceAll(rawURL, `\&`, "&")
	callback, err := url.Parse(rawURL)
	if err != nil || callback.Scheme != "http" || callback.User != nil || callback.Path != "/callback" {
		return bad()
	}
	expected, _ := url.Parse(f.redirect)
	if (callback.Hostname() != "localhost" && callback.Hostname() != "127.0.0.1") || callback.Port() != expected.Port() {
		return bad()
	}
	values := callback.Query()
	if callback.Fragment != "" {
		if fragment, err := url.ParseQuery(callback.Fragment); err == nil {
			for key, vals := range fragment {
				if values.Get(key) == "" {
					values[key] = vals
				}
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.terminal != nil || f.completed != nil {
		return bad()
	}
	if failure := f.processDiscovery(values, transport); failure != nil {
		return reply(failure.HTTPStatus, map[string]string{"error": failure.Message})
	}
	return reply(200, map[string]string{"authorize_url": f.authorize})
}

type refreshFlight struct {
	done    chan struct{}
	result  pluginapi.AuthRefreshResponse
	failure *pluginabi.Error
}

func mergeStorageJSON(base []byte, c credential) []byte {
	fresh, err := json.Marshal(c)
	if err != nil {
		return fresh
	}
	if len(base) == 0 {
		return fresh
	}
	var merged map[string]any
	if json.Unmarshal(base, &merged) != nil || merged == nil {
		return fresh
	}
	var overlay map[string]any
	if json.Unmarshal(fresh, &overlay) != nil || overlay == nil {
		return fresh
	}
	for k, v := range overlay {
		merged[k] = v
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return fresh
	}
	return out
}

func (g *generation) refresh(req pluginapi.AuthRefreshRequest, transport oauthTransport) (pluginapi.AuthRefreshResponse, *pluginabi.Error) {
	var empty pluginapi.AuthRefreshResponse
	if req.AuthID == "" {
		return empty, badCredential()
	}
	saved, failure := readCredential(req.AuthProvider, req.StorageJSON)
	if failure != nil || saved.Refresh == "" {
		return empty, badCredential()
	}
	digest := sha256.Sum256(req.StorageJSON)
	key := req.AuthID + "/" + hex.EncodeToString(digest[:])
	g.refreshMu.Lock()
	if g.refreshes == nil {
		g.refreshes = make(map[string]*refreshFlight)
	}
	if pending := g.refreshes[key]; pending != nil {
		g.refreshMu.Unlock()
		select {
		case <-pending.done:
			return pending.result, pending.failure
		case <-g.ctx.Done():
			return empty, pluginabi.NewError("unavailable", "QuickWork is shutting down", 503)
		}
	}
	pending := &refreshFlight{done: make(chan struct{})}
	g.refreshes[key] = pending
	g.refreshMu.Unlock()
	defer func() { close(pending.done); g.refreshMu.Lock(); delete(g.refreshes, key); g.refreshMu.Unlock() }()
	var exchange any
	if saved.Portal == enterprise {
		form := url.Values{"grant_type": {"refresh_token"}, "client_id": {saved.ClientID}, "refresh_token": {saved.Refresh}}
		if saved.ClientSecret != "" {
			form.Set("client_secret", saved.ClientSecret)
		}
		exchange = form
	} else {
		exchange = map[string]string{"grantType": "refresh_token", "clientId": saved.ClientID, "clientSecret": saved.ClientSecret, "refreshToken": saved.Refresh}
	}
	ctx, cancel := context.WithTimeout(g.ctx, 30*time.Second)
	defer cancel()
	proxyRaw := effectiveProxyURL(saved.ProxyURL, firstString(req.Metadata, "proxy_url"), g.ProxyURL)
	if pending.failure = saved.tokens(transport.withProxy(ctx, proxyRaw), exchange); pending.failure != nil {
		return empty, pending.failure
	}
	updated := saved.auth(mergeStorageJSON(req.StorageJSON, saved))
	updated.ID = req.AuthID
	for key, value := range req.Metadata {
		switch key {
		case "access_token", "refresh_token", "id_token", "expires_at", "client_secret", "type", "email", "oauth_provider":
			continue
		}
		updated.Metadata[key] = value
	}
	pending.result = pluginapi.AuthRefreshResponse{Auth: updated, NextRefreshAfter: updated.NextRefreshAfter}
	return pending.result, nil
}

func authFileNameForSave(authID string, attrs map[string]string, fallback string) string {
	if attrs != nil {
		if path := strings.TrimSpace(attrs["path"]); path != "" {
			if base := filepath.Base(path); strings.HasSuffix(strings.ToLower(base), ".json") {
				return base
			}
		}
	}
	if base := filepath.Base(strings.TrimSpace(authID)); strings.HasSuffix(strings.ToLower(base), ".json") {
		return base
	}
	return fallback
}

func (g *generation) refreshAndPersist(authID, authProvider string, storageJSON []byte, authMeta map[string]any, authAttrs map[string]string, transport oauthTransport, host hostCallback) (credential, *pluginabi.Error) {
	refreshed, failure := g.refresh(pluginapi.AuthRefreshRequest{
		AuthID:       authID,
		AuthProvider: authProvider,
		StorageJSON:  storageJSON,
		Metadata:     authMeta,
		Attributes:   authAttrs,
	}, transport)
	if failure != nil {
		if failure.HTTPStatus == 401 {
			return credential{}, expiredCredential()
		}
		return credential{}, failure
	}
	next, failure := readCredential(authProvider, refreshed.Auth.StorageJSON)
	if failure != nil {
		return credential{}, expiredCredential()
	}
	if host != nil && len(refreshed.Auth.StorageJSON) > 0 {
		if name := authFileNameForSave(authID, authAttrs, refreshed.Auth.FileName); name != "" {
			host(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
				Name: name,
				JSON: json.RawMessage(refreshed.Auth.StorageJSON),
			})
		}
	}
	return next, nil
}

func (g *generation) ensureFreshCredential(authID, authProvider string, storageJSON []byte, authMeta map[string]any, authAttrs map[string]string, transport oauthTransport, host hostCallback) (credential, bool, *pluginabi.Error) {
	saved, failure := readCredential(authProvider, storageJSON)
	if failure != nil {
		return credential{}, false, failure
	}
	if time.Now().Add(time.Minute).Before(saved.Expires) {
		return saved, false, nil
	}
	if strings.TrimSpace(saved.Refresh) == "" {
		return credential{}, false, expiredCredential()
	}
	refreshed, failure := g.refreshAndPersist(authID, authProvider, storageJSON, authMeta, authAttrs, transport, host)
	if failure != nil {
		return credential{}, false, failure
	}
	return refreshed, true, nil
}
