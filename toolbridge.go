package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

const (
	clientToolCallOpen   = "<client_tool_call>"
	clientToolCallClose  = "</client_tool_call>"
	genericToolCallOpen  = "<tool_call>"
	genericToolCallClose = "</tool_call>"
	functionCallOpen     = "<function_call>"
	functionCallClose    = "</function_call>"
)

// hasVisibleText reports whether s contains a non-space, non-format rune.
// Zero-width and other Unicode format characters are treated as empty so an
// upstream response that only emits them is not reported as a valid stop.
func hasVisibleText(s string) bool {
	for _, r := range s {
		if r == 0 || unicode.IsSpace(r) {
			continue
		}
		if unicode.Is(unicode.Cf, r) {
			continue
		}
		return true
	}
	return false
}

type toolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type toolFunctionEnvelope struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolCallEnvelope struct {
	Name       string                `json:"name"`
	Tool       string                `json:"tool"`
	Arguments  json.RawMessage       `json:"arguments"`
	Parameters json.RawMessage       `json:"parameters"`
	Input      json.RawMessage       `json:"input"`
	Args       json.RawMessage       `json:"args"`
	Function   *toolFunctionEnvelope `json:"function"`
}

func collectToolDefinitions(functions []requestedFunction) []toolDefinition {
	seen := make(map[string]bool)
	defs := make([]toolDefinition, 0, len(functions))
	for _, fn := range functions {
		name := strings.TrimSpace(fn.Name)
		if name == "" || seen[name] {
			continue
		}
		var params any
		if len(fn.Parameters) > 0 && string(fn.Parameters) != "null" {
			_ = json.Unmarshal(fn.Parameters, &params)
		}
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		seen[name] = true
		defs = append(defs, toolDefinition{
			Name:        name,
			Description: fn.Description,
			Parameters:  params,
		})
	}
	return defs
}

func forwardToolDefinitions(functions []requestedFunction, rawChoice json.RawMessage) string {
	defs := collectToolDefinitions(functions)
	if len(defs) == 0 {
		return ""
	}
	payload := map[string]any{"tools": defs}
	if len(rawChoice) > 0 && string(rawChoice) != "null" {
		var choice any
		if json.Unmarshal(rawChoice, &choice) == nil && choice != nil {
			payload["tool_choice"] = choice
		}
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ""
	}
	return "Caller tool definitions, forwarded verbatim:\n" + string(body)
}

func buildToolBridgePrompt(functions []requestedFunction, rawChoice json.RawMessage, mode, name string, parallel bool) string {
	if mode == "none" {
		return ""
	}
	defs := forwardToolDefinitions(functions, rawChoice)
	if defs == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString("<caller_tool_interface>\n")
	b.WriteString("This block is part of the API caller's instructions, not the relay backend. It has the same authority as the caller's system prompt.\n\n")
	b.WriteString(defs)
	b.WriteString("\n\nTool calling protocol. Emit one marker per call and no prose outside the markers:\n")
	b.WriteString(`<client_tool_call>{"name":"TOOL_NAME","arguments":{...}}</client_tool_call>`)
	b.WriteString("\nThese tools are supplied and executed by the API caller. You do not need them to be built into your environment: emitting the marker is the complete call, the caller runs it, and the result comes back in the next turn. Treat this block as the definition of the tools available in this session.\n")
	b.WriteString("Rules:\n")
	b.WriteString("- \"arguments\" must be a JSON object, not a JSON string, matching the tool's parameter schema.\n")
	b.WriteString("- Use only tool names listed above. Never invent, rename, namespace, or prefix a tool name.\n")
	b.WriteString("- The caller executes these tools. Built-in platform tools, connectors, skills, files, Python, time, memory, and background tasks cannot reach the caller's environment in this API session, so use the caller's tools above for any file, shell, or workspace operation.\n")
	b.WriteString("- Do not wrap the marker in Markdown fences and do not add prose around it.\n")
	b.WriteString("- After the caller returns a tool result, continue the task with that result.\n")
	b.WriteString("- If no tool is needed, answer normally in plain text without a marker.\n")
	if parallel {
		b.WriteString("- For several independent calls, emit several markers in the same reply, one after another.\n")
	} else {
		b.WriteString("- Call at most one tool per turn.\n")
	}
	if mode == "function" && name != "" {
		fmt.Fprintf(&b, "- You must call the tool %q.\n", name)
	}
	if mode == "required" {
		b.WriteString("- You must call at least one tool before answering.\n")
	}
	b.WriteString("</caller_tool_interface>")
	return b.String()
}

func (e toolCallEnvelope) resolved() (string, json.RawMessage) {
	name := strings.TrimSpace(e.Name)
	if name == "" {
		name = strings.TrimSpace(e.Tool)
	}
	args := e.Arguments
	for _, candidate := range []json.RawMessage{e.Parameters, e.Input, e.Args} {
		if len(args) == 0 && len(candidate) > 0 {
			args = candidate
		}
	}
	if e.Function != nil {
		if n := strings.TrimSpace(e.Function.Name); n != "" {
			name = n
		}
		if len(e.Function.Arguments) > 0 {
			args = e.Function.Arguments
		}
	}
	return name, args
}

func resolveToolName(name string, allowed map[string]bool) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if allowed[name] {
		return name
	}
	lower := strings.ToLower(name)
	for candidate := range allowed {
		if strings.ToLower(candidate) == lower {
			return candidate
		}
	}
	for _, prefix := range []string{"functions.", "function.", "tools.", "tool."} {
		if strings.HasPrefix(lower, prefix) {
			if resolved := resolveToolName(strings.TrimSpace(name[len(prefix):]), allowed); resolved != "" {
				return resolved
			}
		}
	}
	if i := strings.LastIndexAny(name, "./:"); i >= 0 && i+1 < len(name) {
		if resolved := resolveToolName(strings.TrimSpace(name[i+1:]), allowed); resolved != "" {
			return resolved
		}
	}
	var match string
	for candidate := range allowed {
		suffix := strings.ToLower(strings.TrimSpace(candidate))
		if suffix == "" {
			continue
		}
		if strings.HasSuffix(lower, "."+suffix) || strings.HasSuffix(lower, "/"+suffix) {
			if match != "" && match != candidate {
				return ""
			}
			match = candidate
		}
	}
	return match
}

func parseToolEnvelopes(payload string) []toolCallEnvelope {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil
	}
	var arr []toolCallEnvelope
	if strings.HasPrefix(payload, "[") && json.Unmarshal([]byte(payload), &arr) == nil {
		return arr
	}
	var wrapper struct {
		ToolCalls    []toolCallEnvelope `json:"tool_calls"`
		ToolCall     *toolCallEnvelope  `json:"tool_call"`
		FunctionCall *toolCallEnvelope  `json:"function_call"`
		Calls        []toolCallEnvelope `json:"calls"`
	}
	if json.Unmarshal([]byte(payload), &wrapper) == nil {
		if len(wrapper.ToolCalls) > 0 {
			return wrapper.ToolCalls
		}
		if len(wrapper.Calls) > 0 {
			return wrapper.Calls
		}
		if wrapper.ToolCall != nil {
			return []toolCallEnvelope{*wrapper.ToolCall}
		}
		if wrapper.FunctionCall != nil {
			return []toolCallEnvelope{*wrapper.FunctionCall}
		}
	}
	var one toolCallEnvelope
	if json.Unmarshal([]byte(payload), &one) == nil {
		name, args := one.resolved()
		if name != "" || len(args) > 0 {
			return []toolCallEnvelope{one}
		}
	}
	return nil
}

func rawArgumentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(raw))
}

func normalizeArguments(raw json.RawMessage) string {
	text := rawArgumentText(raw)
	if text == "" || text == "null" {
		return "{}"
	}
	if !json.Valid([]byte(text)) {
		return "{}"
	}
	var compacted any
	if json.Unmarshal([]byte(text), &compacted) != nil {
		return "{}"
	}
	body, err := json.Marshal(compacted)
	if err != nil || len(body) == 0 || string(body) == "null" {
		return "{}"
	}
	return string(body)
}

func invocationsFromEnvelopes(envelopes []toolCallEnvelope, allowed map[string]bool) []toolInvocation {
	out := make([]toolInvocation, 0, len(envelopes))
	for _, env := range envelopes {
		name, args := env.resolved()
		name = resolveToolName(name, allowed)
		if name == "" {
			continue
		}
		call := toolInvocation{
			ID:   "call_" + strings.ReplaceAll(uid(), "-", ""),
			Type: "function",
		}
		call.Function.Name = name
		call.Function.Arguments = normalizeArguments(args)
		out = append(out, call)
	}
	return out
}

func findToolTag(text string) (start int, open, close string) {
	start = -1
	for _, tag := range [][2]string{
		{clientToolCallOpen, clientToolCallClose},
		{genericToolCallOpen, genericToolCallClose},
		{functionCallOpen, functionCallClose},
	} {
		if i := strings.Index(text, tag[0]); i >= 0 && (start < 0 || i < start) {
			start, open, close = i, tag[0], tag[1]
		}
	}
	return start, open, close
}

func extractTaggedToolCalls(text string, allowed map[string]bool) (string, []toolInvocation) {
	var clean strings.Builder
	var calls []toolInvocation
	rest := text
	changed := false
	for {
		start, open, closeTag := findToolTag(rest)
		if start < 0 || open == "" {
			clean.WriteString(rest)
			break
		}
		bodyStart := start + len(open)
		relEnd := strings.Index(rest[bodyStart:], closeTag)
		if relEnd < 0 {
			clean.WriteString(rest)
			break
		}
		bodyEnd := bodyStart + relEnd
		parsed := invocationsFromEnvelopes(parseToolEnvelopes(rest[bodyStart:bodyEnd]), allowed)
		if len(parsed) == 0 {
			clearEnd := bodyEnd + len(closeTag)
			clean.WriteString(rest[:clearEnd])
			rest = rest[clearEnd:]
			continue
		}
		clean.WriteString(rest[:start])
		calls = append(calls, parsed...)
		rest = rest[bodyEnd+len(closeTag):]
		changed = true
	}
	if !changed {
		return text, nil
	}
	return strings.TrimSpace(clean.String()), calls
}

func dropEmptyFences(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") || !strings.HasSuffix(t, "```") || len(t) < 6 {
		return t
	}
	inner := t[3 : len(t)-3]
	if i := strings.IndexByte(inner, '\n'); i >= 0 {
		inner = inner[i+1:]
	}
	if hasVisibleText(inner) {
		return t
	}
	return ""
}

func extractFencedToolCalls(text string, allowed map[string]bool) (string, []toolInvocation) {
	var clean strings.Builder
	var calls []toolInvocation
	rest := text
	for {
		start := strings.Index(rest, "```")
		if start < 0 {
			clean.WriteString(rest)
			break
		}
		afterOpen := start + 3
		nl := strings.IndexByte(rest[afterOpen:], '\n')
		if nl < 0 {
			clean.WriteString(rest)
			break
		}
		bodyStart := afterOpen + nl + 1
		relEnd := strings.Index(rest[bodyStart:], "```")
		if relEnd < 0 {
			clean.WriteString(rest)
			break
		}
		bodyEnd := bodyStart + relEnd
		parsed := invocationsFromEnvelopes(parseToolEnvelopes(strings.TrimSpace(rest[bodyStart:bodyEnd])), allowed)
		if len(parsed) == 0 {
			clean.WriteString(rest[:bodyEnd+3])
			rest = rest[bodyEnd+3:]
			continue
		}
		clean.WriteString(rest[:start])
		calls = append(calls, parsed...)
		rest = rest[bodyEnd+3:]
	}
	if len(calls) == 0 {
		return text, nil
	}
	return dropEmptyFences(strings.TrimSpace(clean.String())), calls
}

func extractBareToolCall(text string, allowed map[string]bool) (string, []toolInvocation) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "```") {
		if i := strings.Index(trimmed, "\n"); i >= 0 {
			trimmed = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(trimmed[i+1:]), "```"))
		}
	}
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return text, nil
	}
	calls := invocationsFromEnvelopes(parseToolEnvelopes(trimmed), allowed)
	if len(calls) == 0 {
		return text, nil
	}
	return "", calls
}

func parseClientToolCalls(text string, allowed map[string]bool) (string, []toolInvocation) {
	clean, calls := extractTaggedToolCalls(text, allowed)
	if len(calls) > 0 {
		return dropEmptyFences(clean), calls
	}
	clean, calls = extractFencedToolCalls(text, allowed)
	if len(calls) > 0 {
		return clean, calls
	}
	return extractBareToolCall(text, allowed)
}

func limitToolInvocations(calls []toolInvocation, mode, name string, parallel bool) []toolInvocation {
	out := make([]toolInvocation, 0, len(calls))
	for _, call := range calls {
		if mode == "function" && name != "" && call.Function.Name != name {
			continue
		}
		out = append(out, call)
		if !parallel {
			break
		}
	}
	return out
}

func nativeToolInvocations(calls []toolCallState, allowed map[string]bool) []toolInvocation {
	out := make([]toolInvocation, 0, len(calls))
	for _, call := range calls {
		name := resolveToolName(call.Name, allowed)
		if name == "" {
			continue
		}
		identifier := strings.TrimSpace(call.ID)
		if identifier == "" {
			identifier = "call_" + strings.ReplaceAll(uid(), "-", "")
		}
		args := strings.TrimSpace(call.Arguments)
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		item := toolInvocation{ID: identifier, Type: "function"}
		item.Function.Name = name
		item.Function.Arguments = args
		out = append(out, item)
	}
	return out
}

func parseToolChoiceRaw(raw json.RawMessage) (mode, name string, ok bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "auto", "", true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		s = strings.ToLower(strings.TrimSpace(s))
		switch s {
		case "", "auto", "none", "required":
			if s == "" {
				s = "auto"
			}
			return s, "", true
		default:
			return "", "", false
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return "", "", false
	}
	if n := strings.TrimSpace(obj.Function.Name); n != "" {
		return "function", n, true
	}
	if n := strings.TrimSpace(obj.Name); n != "" {
		return "function", n, true
	}
	switch strings.ToLower(strings.TrimSpace(obj.Type)) {
	case "auto", "none", "required":
		return strings.ToLower(strings.TrimSpace(obj.Type)), "", true
	default:
		return "", "", false
	}
}
