package service

import (
	"encoding/json"
	"strings"
)

const (
	wbErrUnsupportedInbound  = "WB_UNSUPPORTED_INBOUND"
	wbErrCLINotFound         = "WB_CLI_NOT_FOUND"
	wbErrCLIFailed           = "WB_CLI_FAILED"
	wbErrCLITimeout          = "WB_CLI_TIMEOUT"
	wbErrStreamNoCompletion  = "WB_STREAM_NO_COMPLETION"
	wbErrFulltextMismatch    = "WB_STREAM_FULLTEXT_MISMATCH"
	wbErrTokenFailed         = "WB_TOKEN_FAILED"
	wbErrBridgeToolUnmatched = "WB_BRIDGE_TOOL_UNMATCHED"
	wbDoneFrame              = "data: [DONE]\n\n"
)

type wbUsage struct {
	InputTokens, OutputTokens, CacheCreation, CacheRead int
	hasCache                                            bool
}

type wbOpenAIUsage struct {
	PromptTokens             int `json:"prompt_tokens"`
	CompletionTokens         int `json:"completion_tokens"`
	TotalTokens              int `json:"total_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

func wbUsageToOpenAI(u wbUsage) *wbOpenAIUsage {
	return &wbOpenAIUsage{u.InputTokens, u.OutputTokens, u.InputTokens + u.OutputTokens, u.CacheCreation, u.CacheRead}
}

func buildWbChunkFrame(text, reasoning string) string {
	if text == "" && reasoning == "" {
		return ""
	}
	delta := map[string]string{}
	if text != "" {
		delta["content"] = text
	}
	if reasoning != "" {
		delta["reasoning_content"] = reasoning
	}
	return wbSSE(map[string]any{"object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}})
}

func buildWbTerminalFrame(u wbUsage) string {
	return wbSSE(map[string]any{"object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": wbUsageToOpenAI(u)})
}

func wbSSE(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n\n"
}

type wbLineKind int

const (
	wbLineIgnore wbLineKind = iota
	wbLineMessageStart
	wbLineTextDelta
	wbLineThinkingDelta
	wbLineAssistant
	wbLineCompletion
	wbLineError
)

type wbLineFrame struct {
	kind                                     wbLineKind
	text, thinking, code, message, messageID string
	usage                                    wbUsage
}

func frameStr(m map[string]any, key string) string { s, _ := m[key].(string); return s }

func classifyWBLine(obj map[string]any) wbLineFrame {
	if frameStr(obj, "type") == "error" {
		return makeErrorFrame(obj)
	}
	if e, ok := obj["error"].(map[string]any); ok {
		return makeErrorFrame(e)
	}
	if e, ok := obj["error"].(string); ok && e != "" {
		return wbLineFrame{kind: wbLineError, code: wbErrCLIFailed, message: e}
	}
	switch frameStr(obj, "type") {
	case "stream_event":
		e, _ := obj["event"].(map[string]any)
		if frameStr(e, "type") == "message_start" {
			m, _ := e["message"].(map[string]any)
			return wbLineFrame{kind: wbLineMessageStart, messageID: frameStr(m, "id")}
		}
		if frameStr(e, "type") != "content_block_delta" {
			break
		}
		d, _ := e["delta"].(map[string]any)
		switch frameStr(d, "type") {
		case "text_delta":
			if s := frameStr(d, "text"); s != "" {
				return wbLineFrame{kind: wbLineTextDelta, text: s}
			}
		case "thinking_delta":
			if s := frameStr(d, "thinking"); s != "" {
				return wbLineFrame{kind: wbLineThinkingDelta, thinking: s}
			}
		}
	case "assistant":
		m, ok := obj["message"].(map[string]any)
		if !ok {
			m = obj
		}
		return wbLineFrame{kind: wbLineAssistant, text: extractWBText(m["content"]), messageID: frameStr(m, "id")}
	case "result":
		// CLI result is a top-level string; accept the older nested envelope too.
		r := obj
		if nested, ok := obj["result"].(map[string]any); ok {
			r = nested
		}
		if frameStr(r, "status") == "error" || r["is_error"] == true || strings.HasPrefix(frameStr(r, "subtype"), "error") || r["error"] != nil {
			return makeErrorFrame(r)
		}
		if frameStr(r, "subtype") != "success" && frameStr(r, "status") != "success" {
			break
		}
		text, ok := r["result"].(string)
		if !ok {
			break
		}
		u, _ := r["usage"].(map[string]any)
		if u == nil {
			u, _ = obj["usage"].(map[string]any)
		}
		return wbLineFrame{kind: wbLineCompletion, text: text, usage: parseWBUsage(u)}
	}
	return wbLineFrame{kind: wbLineIgnore}
}

func makeErrorFrame(m map[string]any) wbLineFrame {
	e := m
	if nested, ok := m["error"].(map[string]any); ok {
		e = nested
	}
	code, msg := frameStr(e, "code"), frameStr(e, "message")
	if code == "" {
		code = wbErrCLIFailed
	}
	if msg == "" {
		msg = frameStr(m, "error")
	}
	if msg == "" {
		msg = frameStr(m, "result")
	}
	if msg == "" {
		msg = "wb CLI reported an error"
	}
	return wbLineFrame{kind: wbLineError, code: code, message: msg}
}

func extractWBText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	blocks, _ := content.([]any)
	var b strings.Builder
	for _, raw := range blocks {
		m, _ := raw.(map[string]any)
		if frameStr(m, "type") == "text" {
			b.WriteString(frameStr(m, "text"))
		}
	}
	return b.String()
}

func uInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		switch n := m[k].(type) {
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return 0
}

func parseWBUsage(m map[string]any) wbUsage {
	return wbUsage{InputTokens: uInt(m, "input_tokens"), OutputTokens: uInt(m, "output_tokens"),
		CacheCreation: uInt(m, "cache_creation_input_tokens", "cache_creation"), CacheRead: uInt(m, "cache_read_input_tokens", "cache_read"),
		hasCache: m["cache_creation_input_tokens"] != nil || m["cache_creation"] != nil || m["cache_read_input_tokens"] != nil || m["cache_read"] != nil}
}

// Each message owns its delta prefix. Full-only messages can arrive without a start event.
// IDs distinguish equal text in distinct messages; absent IDs, identical repeats are idempotent.
type wbStreamAccumulator struct {
	delta, requestText, lastFull, scopeID string
	backfilled                            bool
	seen                                  map[string]bool
}

func newWbStreamAccumulator() *wbStreamAccumulator {
	return &wbStreamAccumulator{seen: map[string]bool{}}
}
func (a *wbStreamAccumulator) OnMessageStart() { a.StartMessage("") }
func (a *wbStreamAccumulator) StartMessage(id string) {
	a.delta, a.scopeID, a.lastFull, a.backfilled = "", id, "", false
}
func (a *wbStreamAccumulator) OnTextDelta(s string)                { a.delta += s; a.requestText += s }
func (a *wbStreamAccumulator) OnThinkingDelta(_ string)            {}
func (a *wbStreamAccumulator) OnAssistant(s string) (string, bool) { return a.Assistant("", s) }
func (a *wbStreamAccumulator) Assistant(id, full string) (string, bool) {
	if id != "" && a.seen[id] {
		return "", false
	}
	if a.backfilled {
		if (id == "" || id == a.scopeID) && full == a.lastFull {
			return "", false
		}
		a.StartMessage(id)
	} else if id != "" && a.scopeID != "" && id != a.scopeID {
		a.StartMessage(id)
	}
	a.backfilled, a.lastFull = true, full
	if id != "" {
		a.seen[id] = true
		a.scopeID = id
	}
	if !strings.HasPrefix(full, a.delta) {
		return "", false
	}
	suffix := full[len(a.delta):]
	a.requestText += suffix
	return suffix, suffix != ""
}
func (a *wbStreamAccumulator) RequestText() string         { return a.requestText }
func (a *wbStreamAccumulator) CheckFulltext(s string) bool { return a.requestText == s }
