package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var (
	errUsageUnavailable  = errors.New("official account usage is unavailable")
	errUsageUnauthorized = errors.New("official account usage authorization failed")
)

type usageBucket struct {
	Used     *float64 `json:"used"`
	Limit    *float64 `json:"limit"`
	ResetsAt int64    `json:"resets_at"`
	Warning  string   `json:"warning,omitempty"`
}

type usageSnapshot struct {
	Source           string       `json:"source"`
	Unit             string       `json:"unit"`
	Monthly          *usageBucket `json:"monthly"`
	OverageMonthly   *usageBucket `json:"overage_monthly"`
	SubscriptionType string       `json:"subscription_type,omitempty"`
}

type usageFetchError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *usageFetchError) Error() string {
	switch {
	case e.StatusCode == http.StatusTooManyRequests:
		return "QuickWork official usage endpoint is temporarily rate limited"
	case e.StatusCode >= 500:
		return "QuickWork official usage service is temporarily unavailable"
	case e.StatusCode > 0:
		return "QuickWork official usage request failed"
	default:
		return "QuickWork official usage transport failed"
	}
}

func quotaUnavailableError() *pluginabi.Error {
	return pluginabi.NewError("quota_unavailable", "QuickWork 没有向此登录凭据或套餐提供官方额度信息；请在 Amazon Quick 中查看。此状态不代表额度为零。 / Official quota information is not available for this QuickWork credential or plan.", 501)
}

func (g *generation) fetchQuota(req pluginapi.QuotaFetchRequest, host hostCallback, transport oauthTransport) (pluginapi.QuotaFetchResponse, *pluginabi.Error) {
	var empty pluginapi.QuotaFetchResponse
	if strings.TrimSpace(req.AuthID) == "" || strings.TrimSpace(req.AuthIndex) == "" {
		return empty, badCredential()
	}
	saved, refreshed, failure := g.ensureFreshCredential(req.AuthID, req.Provider, req.StorageJSON, req.Metadata, req.Attributes, transport, host)
	if failure != nil {
		return empty, failure
	}
	endpoint := strings.TrimRight(saved.Tenant, "/") + "/integration/quick-work/qbs-event"
	if _, ok := quickURL(endpoint); !ok {
		return empty, quotaUnavailableError()
	}
	query := map[string]string{
		"_method":     http.MethodGet,
		"surfaceType": surfaceType,
		"appVersion":  appVersion,
	}
	invocation := map[string]any{
		"path":        "profile/usage",
		"queryParams": query,
		"body":        map[string]any{},
	}
	if g.WorkingSessionID != "" {
		invocation["workingSessionId"] = g.WorkingSessionID
	}
	if g.OutpostID != "" {
		invocation["outpostId"] = g.OutpostID
	}
	body, err := json.Marshal(map[string]any{
		"eventType": "invokeAgent",
		"eventBody": map[string]any{"invokeAgent": invocation},
	})
	if err != nil {
		return empty, pluginabi.NewError("quota_failed", "Failed to encode QuickWork quota request", 500)
	}
	proxyRaw := effectiveProxyURL(saved.ProxyURL, firstString(req.Metadata, "proxy_url"), g.ProxyURL)
	ctx, cancel := context.WithTimeout(g.ctx, 30*time.Second)
	defer cancel()
	for attempt := range 2 {
		headers := make(http.Header)
		headers.Set("Content-Type", "application/json")
		headers.Set("Accept", "application/json")
		headers.Set("Authorization", "Bearer "+saved.bearer())
		headers.Set("x-amzn-qs-internal-trace-id", uid())
		resp, err := doOutboundHTTP(ctx, transport, proxyRaw, 30*time.Second, pluginapi.HTTPRequest{
			Method:  http.MethodPost,
			URL:     endpoint,
			Headers: headers,
			Body:    body,
		})
		if err != nil {
			return empty, &pluginabi.Error{Code: pluginabi.ErrorCodeTransientTransport, Message: "QuickWork quota request failed", Retryable: true, HTTPStatus: 502}
		}
		if err := usageStatusError(resp.StatusCode, resp.Headers); err != nil {
			if errors.Is(err, errUsageUnauthorized) && !refreshed && attempt == 0 && strings.TrimSpace(saved.Refresh) != "" {
				refreshed = true
				next, refreshErr := g.refreshAndPersist(req.AuthID, req.Provider, req.StorageJSON, req.Metadata, req.Attributes, transport, host)
				if refreshErr != nil {
					return empty, refreshErr
				}
				saved = next
				continue
			}
			return empty, mapUsageError(err)
		}
		if len(resp.Body) > 1<<20 {
			return empty, pluginabi.NewError("quota_failed", "QuickWork quota response exceeded size limit", 502)
		}
		snapshot, err := parseUsageSnapshot(resp.Body)
		if err != nil {
			if errors.Is(err, errUsageUnauthorized) && !refreshed && attempt == 0 && strings.TrimSpace(saved.Refresh) != "" {
				refreshed = true
				next, refreshErr := g.refreshAndPersist(req.AuthID, req.Provider, req.StorageJSON, req.Metadata, req.Attributes, transport, host)
				if refreshErr != nil {
					return empty, refreshErr
				}
				saved = next
				continue
			}
			return empty, mapUsageError(err)
		}
		return usageSnapshotToQuota(snapshot, saved.Portal), nil
	}
	return empty, expiredCredential()
}

func mapUsageError(err error) *pluginabi.Error {
	if errors.Is(err, errUsageUnauthorized) {
		return expiredCredential()
	}
	if errors.Is(err, errUsageUnavailable) {
		return quotaUnavailableError()
	}
	var fetchErr *usageFetchError
	if errors.As(err, &fetchErr) {
		if fetchErr.StatusCode == http.StatusTooManyRequests {
			return &pluginabi.Error{
				Code:              pluginabi.ErrorCodeTransientRateLimit,
				Message:           fetchErr.Error(),
				Retryable:         true,
				HTTPStatus:        429,
				RetryAfterSeconds: int(fetchErr.RetryAfter / time.Second),
			}
		}
		return &pluginabi.Error{
			Code:       "quota_failed",
			Message:    fetchErr.Error(),
			Retryable:  fetchErr.StatusCode == 0 || fetchErr.StatusCode == http.StatusRequestTimeout || fetchErr.StatusCode >= 500,
			HTTPStatus: 502,
		}
	}
	return pluginabi.NewError("quota_failed", "QuickWork quota lookup failed", 502)
}

func usageStatusError(status int, headers http.Header) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return errUsageUnauthorized
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented:
		return errUsageUnavailable
	case status >= 200 && status < 300:
		return nil
	default:
		return &usageFetchError{StatusCode: status, RetryAfter: usageRetryAfter(headers.Get("Retry-After"))}
	}
}

func usageRetryAfter(raw string) time.Duration {
	const maximum = 5 * time.Minute
	seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds >= int64(maximum/time.Second) {
			return maximum
		}
		return time.Duration(seconds) * time.Second
	}
	deadline, err := http.ParseTime(raw)
	if err != nil {
		return 0
	}
	delay := time.Until(deadline)
	if delay < 0 {
		return 0
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func parseUsageSnapshot(data []byte) (*usageSnapshot, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, errUsageUnavailable
	}
	if bytes.HasPrefix(trimmed, []byte{'"'}) {
		var encoded string
		if err := json.Unmarshal(trimmed, &encoded); err != nil {
			return nil, &usageFetchError{StatusCode: http.StatusBadGateway}
		}
		data = []byte(encoded)
	}
	var outer struct {
		EventResponse *struct {
			InvokeAgentResponse json.RawMessage `json:"invokeAgentResponse"`
		} `json:"eventResponse"`
	}
	if err := json.Unmarshal(data, &outer); err != nil {
		return nil, &usageFetchError{StatusCode: http.StatusBadGateway}
	}
	if outer.EventResponse != nil && len(outer.EventResponse.InvokeAgentResponse) > 0 {
		data = outer.EventResponse.InvokeAgentResponse
	}
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte{'"'}) {
		var encoded string
		if err := json.Unmarshal(data, &encoded); err != nil {
			return nil, &usageFetchError{StatusCode: http.StatusBadGateway}
		}
		data = []byte(encoded)
	}
	var env struct {
		Type         string            `json:"type"`
		Status       *int              `json:"status"`
		Headers      map[string]string `json:"headers"`
		Body         json.RawMessage   `json:"body"`
		BodyEncoding string            `json:"body_encoding"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, &usageFetchError{StatusCode: http.StatusBadGateway}
	}
	if env.Type == "api_response" {
		status := http.StatusOK
		if env.Status != nil {
			status = *env.Status
		}
		headers := make(http.Header, len(env.Headers))
		for k, v := range env.Headers {
			headers.Set(k, v)
		}
		if err := usageStatusError(status, headers); err != nil {
			return nil, err
		}
		data = env.Body
		if bytes.HasPrefix(bytes.TrimSpace(data), []byte{'"'}) {
			var encoded string
			if err := json.Unmarshal(data, &encoded); err != nil {
				return nil, &usageFetchError{StatusCode: http.StatusBadGateway}
			}
			data = []byte(encoded)
		}
		if env.BodyEncoding == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(string(data))
			if err != nil {
				return nil, &usageFetchError{StatusCode: http.StatusBadGateway}
			}
			data = decoded
		} else if env.BodyEncoding != "" && env.BodyEncoding != "json" {
			return nil, &usageFetchError{StatusCode: http.StatusBadGateway}
		}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errUsageUnavailable
	}
	var payload struct {
		Monthly        *usageWireBucket `json:"monthly"`
		OverageMonthly *usageWireBucket `json:"overage_monthly"`
		Subscription   *struct {
			SubscriptionType string `json:"subscription_type"`
		} `json:"subscription"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, &usageFetchError{StatusCode: http.StatusBadGateway}
	}
	monthly, err := normalizeUsageBucket(payload.Monthly)
	if err != nil {
		return nil, err
	}
	overage, err := normalizeUsageBucket(payload.OverageMonthly)
	if err != nil {
		return nil, err
	}
	if monthly == nil && overage == nil {
		return nil, errUsageUnavailable
	}
	snapshot := &usageSnapshot{
		Source:         "quickwork_profile_usage",
		Unit:           "minutes",
		Monthly:        monthly,
		OverageMonthly: overage,
	}
	if payload.Subscription != nil {
		plan := payload.Subscription.SubscriptionType
		if len(plan) <= 128 && strings.Trim(plan, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") == "" {
			snapshot.SubscriptionType = plan
		}
	}
	return snapshot, nil
}

type usageWireBucket struct {
	Used     *float64 `json:"used"`
	Limit    *float64 `json:"limit"`
	ResetsAt float64  `json:"resets_at"`
	Warning  *struct {
		Severity string `json:"severity"`
	} `json:"warning"`
}

func normalizeUsageBucket(bucket *usageWireBucket) (*usageBucket, error) {
	if bucket == nil || (bucket.Used == nil && bucket.Limit == nil) {
		return nil, nil
	}
	if (bucket.Used != nil && (*bucket.Used < 0 || math.IsNaN(*bucket.Used) || math.IsInf(*bucket.Used, 0))) ||
		(bucket.Limit != nil && (*bucket.Limit < 0 || math.IsNaN(*bucket.Limit) || math.IsInf(*bucket.Limit, 0))) ||
		bucket.ResetsAt < 0 || bucket.ResetsAt > 253402300799 || math.IsNaN(bucket.ResetsAt) || math.IsInf(bucket.ResetsAt, 0) {
		return nil, &usageFetchError{StatusCode: http.StatusBadGateway}
	}
	normalized := &usageBucket{
		Used:     bucket.Used,
		Limit:    bucket.Limit,
		ResetsAt: int64(bucket.ResetsAt),
	}
	if bucket.Warning != nil {
		switch bucket.Warning.Severity {
		case "info", "warning", "error":
			normalized.Warning = bucket.Warning.Severity
		}
	}
	return normalized, nil
}

func usageSnapshotToQuota(snapshot *usageSnapshot, portal string) pluginapi.QuotaFetchResponse {
	tier := "QuickWork Personal"
	if portal == enterprise {
		tier = "QuickWork Enterprise"
	}
	plan := snapshot.SubscriptionType
	if plan == "" {
		plan = tier
	}
	out := pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     plan,
			TierName: tier,
		},
	}
	group := pluginapi.QuotaGroup{DisplayName: name}
	appendBucket := func(window, keyPrefix, labelPrefix string, bucket *usageBucket) {
		if bucket == nil {
			return
		}
		reset := ""
		if bucket.ResetsAt > 0 {
			reset = time.Unix(bucket.ResetsAt, 0).UTC().Format(time.RFC3339)
		}
		switch {
		case bucket.Used != nil && bucket.Limit != nil && *bucket.Limit > 0:
			remaining := math.Max(0, *bucket.Limit-*bucket.Used)
			frac := clamp01(remaining / *bucket.Limit)
			desc := fmt.Sprintf("%s / %s min left (%s used)", formatQuotaNumber(remaining), formatQuotaNumber(*bucket.Limit), formatQuotaNumber(*bucket.Used))
			if bucket.Warning != "" {
				desc += " [" + bucket.Warning + "]"
			}
			group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{
				Window:            window,
				RemainingFraction: frac,
				ResetTime:         reset,
				Description:       desc,
			})
		case bucket.Limit != nil && *bucket.Limit == 0:
			used := 0.0
			if bucket.Used != nil {
				used = *bucket.Used
			}
			desc := fmt.Sprintf("%s / 0 min", formatQuotaNumber(used))
			if bucket.Warning != "" {
				desc += " [" + bucket.Warning + "]"
			}
			group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{
				Window:            window,
				RemainingFraction: 0,
				ResetTime:         reset,
				Description:       desc,
			})
		case bucket.Used != nil:
			// Uncapped usage (e.g. pay-as-you-go overage without a reported cap):
			// report as a plain summary metric instead of fabricating a 100% bar.
			out.Summary = append(out.Summary, pluginapi.QuotaMetric{
				Key:    keyPrefix + "_used",
				Label:  labelPrefix + " Used",
				Value:  roundQuota(*bucket.Used),
				Unit:   "minutes",
				Format: "number",
			})
		case bucket.Limit != nil:
			out.Summary = append(out.Summary, pluginapi.QuotaMetric{
				Key:    keyPrefix + "_limit",
				Label:  labelPrefix + " Limit",
				Value:  roundQuota(*bucket.Limit),
				Unit:   "minutes",
				Format: "number",
			})
		}
	}
	appendBucket("Monthly", "monthly", "Monthly", snapshot.Monthly)
	appendBucket("Monthly Overage", "overage_monthly", "Monthly Overage", snapshot.OverageMonthly)
	if len(group.Buckets) > 0 {
		out.Groups = append(out.Groups, group)
	}
	return out
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}

func roundQuota(v float64) float64 {
	return math.Round(v*100) / 100
}

func formatQuotaNumber(v float64) string {
	rounded := roundQuota(v)
	if rounded == math.Trunc(rounded) {
		return fmt.Sprintf("%.0f", rounded)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", rounded), "0"), ".")
}
