package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	maxUserMessageBytes     = 63 * 1024
	attachmentPromptPrefix  = "prompt-"
	attachmentPromptSuffix  = ".txt"
	attachmentMimeType      = "text/plain"
	attachmentMinPromptSize = 8 * 1024
	attachmentPollAttempts  = 60
	textTrimMarker          = "\n\n[earlier conversation trimmed to fit the upstream 64 KB frame limit]\n\n"
)

var (
	attachmentPollInterval = 1500 * time.Millisecond
	errUserMessageTooLarge = errors.New("user_message exceeds the upstream 64 KB frame limit")
)

type attachmentRef struct {
	ID   string
	Path string
	Name string
	Size int
}

type pathEntry struct {
	Path string `json:"path"`
}

func attachmentPromptName(messageID string) string {
	suffix := strings.ReplaceAll(messageID, "-", "")
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	if suffix == "" {
		suffix = "00000000"
	}
	return attachmentPromptPrefix + suffix + attachmentPromptSuffix
}

func attachmentShortText(name string) string {
	return "The user's full message for this turn is in the attached file " + name + ". Read it and respond to it."
}

func userMessageBytes(payload map[string]any) (int, error) {
	body, err := json.Marshal(map[string]any{"type": "user_message", "payload": payload})
	if err != nil {
		return 0, err
	}
	return len(body), nil
}

func shouldAttachPrompt(promptSize int) bool {
	return promptSize >= attachmentMinPromptSize
}

func uploadPromptAttachment(ctx context.Context, tenantURL, bearer, threadID, name string, data []byte, transport oauthTransport, proxyRaw string) (attachmentRef, error) {
	ref := attachmentRef{Name: name, Size: len(data)}
	base := strings.TrimRight(strings.TrimSpace(tenantURL), "/")
	if _, ok := validTenant(base); !ok {
		return ref, errors.New("invalid tenant URL for attachment upload")
	}
	if strings.TrimSpace(bearer) == "" || len(data) == 0 {
		return ref, errors.New("missing token or prompt data for attachment upload")
	}

	// 1. Upload raw prompt bytes.
	q := url.Values{}
	q.Set("name", name)
	q.Set("contentType", attachmentMimeType)
	q.Set("contentLength", strconv.Itoa(len(data)))
	uploadHeaders := http.Header{
		"Authorization": {"Bearer " + bearer},
		"Origin":        {"https://quick.aws.com"},
		"Content-Type":  {"application/octet-stream"},
	}
	resp, err := doOutboundHTTP(ctx, transport, proxyRaw, 2*time.Minute, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     base + "/integration/quick-work/upload-attachment?" + q.Encode(),
		Headers: uploadHeaders,
		Body:    data,
	})
	if err != nil {
		return ref, fmt.Errorf("attachment upload: %w", err)
	}
	if resp.StatusCode >= 400 {
		return ref, fmt.Errorf("attachment upload HTTP %d", resp.StatusCode)
	}
	var up struct {
		AttachmentID string      `json:"attachmentId"`
		Path         string      `json:"path"`
		Files        []pathEntry `json:"files"`
	}
	if err := json.Unmarshal(resp.Body, &up); err != nil {
		return ref, fmt.Errorf("attachment upload decode: %w", err)
	}
	ref.ID = strings.TrimSpace(up.AttachmentID)
	ref.Path = firstAttachmentPath(up.Path, up.Files)
	if ref.ID == "" && ref.Path == "" {
		return ref, errors.New("attachment upload returned no attachment id or path")
	}

	// 2. Poll DLP scan verdict.
	if ref.ID != "" {
		if err := waitAttachmentDLP(ctx, base, bearer, ref.ID, transport, proxyRaw); err != nil {
			return ref, err
		}
	}
	if ref.Path != "" {
		return ref, nil
	}

	// 3. Deliver attachment to conversation workspace via qbs-event.
	path, err := deliverAttachment(ctx, base, bearer, threadID, ref.ID, transport, proxyRaw)
	if err != nil {
		return ref, err
	}
	ref.Path = path
	return ref, nil
}

func waitAttachmentDLP(ctx context.Context, base, bearer, attachmentID string, transport oauthTransport, proxyRaw string) error {
	endpoint := base + "/integration/quick-work/attachment-dlp-status?attachmentId=" + url.QueryEscape(attachmentID)
	headers := http.Header{
		"Authorization": {"Bearer " + bearer},
		"Origin":        {"https://quick.aws.com"},
	}
	for attempt := range attachmentPollAttempts {
		if attempt > 0 {
			timer := time.NewTimer(attachmentPollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		resp, err := doOutboundHTTP(ctx, transport, proxyRaw, 30*time.Second, pluginapi.HTTPRequest{
			Method:  http.MethodGet,
			URL:     endpoint,
			Headers: headers,
		})
		if err != nil {
			return fmt.Errorf("attachment DLP: %w", err)
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("attachment DLP HTTP %d", resp.StatusCode)
		}
		var out struct {
			DLPJobStatus string `json:"dlpJobStatus"`
			PolicyAction string `json:"policyAction"`
		}
		if err := json.Unmarshal(resp.Body, &out); err != nil {
			return fmt.Errorf("attachment DLP decode: %w", err)
		}
		status := strings.ToUpper(strings.TrimSpace(out.DLPJobStatus))
		action := strings.ToUpper(strings.TrimSpace(out.PolicyAction))
		switch action {
		case "ALLOW":
			return nil
		case "BLOCK", "DENY", "REJECT", "WARN":
			return fmt.Errorf("attachment blocked by DLP (%s)", action)
		}
		switch status {
		case "SKIPPED", "NOT_APPLICABLE", "DISABLED":
			return nil
		case "FAILED", "ERROR":
			return fmt.Errorf("attachment DLP scan failed (%s)", status)
		case "COMPLETED":
			return errors.New("attachment DLP completed without an allow verdict")
		}
	}
	return errors.New("attachment DLP scan did not settle")
}

func deliverAttachment(ctx context.Context, base, bearer, threadID, attachmentID string, transport oauthTransport, proxyRaw string) (string, error) {
	event := map[string]any{
		"eventType": "invokeAgent",
		"eventBody": map[string]any{"invokeAgent": map[string]any{
			"path":        "files/upload/" + url.PathEscape(threadID),
			"queryParams": map[string]any{"_method": "POST"},
			"body":        map[string]any{"attachmentId": attachmentID},
		}},
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	headers := http.Header{
		"Authorization": {"Bearer " + bearer},
		"Origin":        {"https://quick.aws.com"},
		"Content-Type":  {"application/json"},
	}
	resp, err := doOutboundHTTP(ctx, transport, proxyRaw, 30*time.Second, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     base + "/integration/quick-work/qbs-event",
		Headers: headers,
		Body:    raw,
	})
	if err != nil {
		return "", fmt.Errorf("attachment deliver: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("attachment deliver HTTP %d", resp.StatusCode)
	}
	path := extractAttachmentPath(resp.Body)
	if path == "" {
		return "", errors.New("attachment deliver returned no path")
	}
	return path, nil
}

func firstAttachmentPath(path string, files []pathEntry) string {
	if strings.TrimSpace(path) != "" {
		return strings.TrimSpace(path)
	}
	if len(files) > 0 {
		return strings.TrimSpace(files[0].Path)
	}
	return ""
}

func extractAttachmentPath(raw []byte) string {
	var top struct {
		Path          string          `json:"path"`
		Files         []pathEntry     `json:"files"`
		EventResponse json.RawMessage `json:"eventResponse"`
	}
	if json.Unmarshal(raw, &top) != nil {
		return ""
	}
	if p := firstAttachmentPath(top.Path, top.Files); p != "" {
		return p
	}
	if len(top.EventResponse) == 0 {
		return extractFromAPIResponse(raw)
	}
	var ev struct {
		InvokeAgentResponse json.RawMessage `json:"invokeAgentResponse"`
	}
	if json.Unmarshal(top.EventResponse, &ev) != nil || len(ev.InvokeAgentResponse) == 0 {
		return extractFromAPIResponse(raw)
	}
	var encoded string
	if json.Unmarshal(ev.InvokeAgentResponse, &encoded) == nil {
		return extractFromAPIResponse([]byte(encoded))
	}
	return extractFromAPIResponse(ev.InvokeAgentResponse)
}

func extractFromAPIResponse(raw []byte) string {
	var api struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(raw, &api) == nil && api.Body != "" {
		if p := extractFromPathBody([]byte(api.Body)); p != "" {
			return p
		}
	}
	var withObj struct {
		Body json.RawMessage `json:"body"`
	}
	if json.Unmarshal(raw, &withObj) == nil && len(withObj.Body) > 0 {
		if p := extractFromPathBody(withObj.Body); p != "" {
			return p
		}
	}
	return extractFromPathBody(raw)
}

func extractFromPathBody(raw []byte) string {
	var v struct {
		Path  string      `json:"path"`
		Files []pathEntry `json:"files"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return firstAttachmentPath(v.Path, v.Files)
}

// fitUserMessage keeps the reassembled user_message JSON under the upstream
// 64 KB frame limit by trimming the middle of text while preserving its head
// and tail on valid UTF-8 rune boundaries.
func fitUserMessage(payload map[string]any) error {
	wrap := func() ([]byte, error) {
		return json.Marshal(map[string]any{"type": "user_message", "payload": payload})
	}
	body, err := wrap()
	if err != nil {
		return err
	}
	if len(body) <= maxUserMessageBytes {
		return nil
	}
	text, _ := payload["text"].(string)
	if len(text) > 4096 {
		payload["text"] = shrinkToLimit(text, func(s string) ([]byte, error) {
			payload["text"] = s
			return wrap()
		}, textTrimMarker)
		if body, err = wrap(); err != nil {
			return err
		}
	}
	if len(body) > maxUserMessageBytes {
		return fmt.Errorf("%w: %d bytes after trimming", errUserMessageTooLarge, len(body))
	}
	return nil
}

func shrinkToLimit(s string, wrap func(string) ([]byte, error), marker string) string {
	out := s
	for range 6 {
		body, err := wrap(out)
		if err != nil || len(body) <= maxUserMessageBytes {
			return out
		}
		over := len(body) - maxUserMessageBytes
		next := shrinkMiddle(out, over, marker)
		if next == out {
			return out
		}
		out = next
	}
	return out
}

func shrinkMiddle(s string, remove int, marker string) string {
	const minKeep = 4096
	target := len(s) - remove - len(marker)
	if target < minKeep {
		target = minKeep
	}
	if target >= len(s) {
		return s
	}
	head := target / 4
	if head > len(s) {
		head = len(s)
	}
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	tailStart := len(s) - (target - head)
	if tailStart < head {
		tailStart = head
	}
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	return s[:head] + marker + s[tailStart:]
}
