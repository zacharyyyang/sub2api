package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
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
	// wbErrMixedToolPlanes：同一 assistant 事件里既有服务端代执行（A 平面）调用、又有
	// 客户端白名单（B 平面）调用。交接一旦终止，A 平面调用就永远不执行也拿不到结果，
	// 静默丢弃等于交付残缺结果——显式拒绝。
	wbErrMixedToolPlanes = "WB_MIXED_TOOL_PLANES"
	wbDoneFrame          = "data: [DONE]\n\n"
)

type wbUsage struct {
	InputTokens, OutputTokens, CacheCreation, CacheRead int
	hasCache                                            bool
}

// WBToolCall represents a client-executed tool call intercepted from a CLI assistant tool_use block.
type WBToolCall struct {
	ID        string
	Name      string
	Arguments string
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

// buildWbChunkFrame 构造一个 delta 帧。withRole 只在请求首帧为 true：
// OpenAI 兼容契约要求首帧携带 role:"assistant"，后续帧只带内容。
func buildWbChunkFrame(text, reasoning string, withRole bool) string {
	if text == "" && reasoning == "" {
		return ""
	}
	delta := map[string]any{}
	if withRole {
		delta["role"] = "assistant"
	}
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

func buildWbToolCallsFrame(calls []WBToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	items := make([]any, len(calls))
	for i, call := range calls {
		items[i] = map[string]any{
			"index": i,
			"id":    call.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		}
	}
	return wbSSE(map[string]any{
		"object":  "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": items}, "finish_reason": nil}},
	})
}

func buildWbToolCallsTerminalFrame(u wbUsage) string {
	return wbSSE(map[string]any{"object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}, "usage": wbUsageToOpenAI(u)})
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
	toolCalls                                []WBToolCall
	// hasServerToolUse 标记同一条 assistant 事件里还带有服务端代执行（A 平面）的
	// tool_use 块。这些块由 CLI 经桥执行、不透出给客户端；若同一事件又含客户端
	// 调用而触发交接终止，它们就会既没执行也没返回——调用方据此显式报错。
	hasServerToolUse bool
}

func frameStr(m map[string]any, key string) string { s, _ := m[key].(string); return s }

func classifyWBLine(obj map[string]any, allowedClientTools ...map[string]bool) wbLineFrame {
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
		u, _ := m["usage"].(map[string]any)
		if u == nil {
			u, _ = obj["usage"].(map[string]any)
		}
		var allowlist map[string]bool
		if len(allowedClientTools) > 0 {
			allowlist = allowedClientTools[0]
		}
		calls, serverUse, callErr := extractWBClientToolCalls(m["content"], allowlist)
		if callErr != nil {
			// CLI 产出的 tool_use 参数不是合法 JSON：透出给客户端等于塞一个它无法解析的
			// arguments，错误只会拖到客户端执行阶段才炸，这里就地拒绝。
			return wbLineFrame{kind: wbLineError, code: wbErrCLIFailed, message: callErr.Error()}
		}
		return wbLineFrame{
			kind:             wbLineAssistant,
			text:             extractWBText(m["content"]),
			messageID:        frameStr(m, "id"),
			usage:            parseWBUsage(u),
			toolCalls:        calls,
			hasServerToolUse: serverUse,
		}
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

// extractWBClientToolCalls 返回两类信息：命中客户端白名单的调用（B 平面，要透出给
// 客户端执行），以及同一事件里是否还带着服务端代执行（A 平面）的 tool_use。
// A 平面调用由 CLI 经桥自行执行、本不该透出；一旦同事件触发交接终止，它们就会
// 因进程被杀而永远不执行——调用方必须显式拒绝，不能静默丢弃。
func extractWBClientToolCalls(content any, allowlist map[string]bool) ([]WBToolCall, bool, error) {
	blocks, ok := content.([]any)
	if !ok {
		return nil, false, nil
	}
	var calls []WBToolCall
	serverUse := false
	for _, raw := range blocks {
		m, _ := raw.(map[string]any)
		if frameStr(m, "type") != "tool_use" {
			continue
		}
		rawName := frameStr(m, "name")
		// 缺失、非字符串或空白字符串一律视为未命名——只判空串会让 " " 绕过下面的
		// 前缀检查，把一条本该交接的客户端调用静默漏掉。
		if strings.TrimSpace(rawName) == "" || rawName == "ToolSearch" {
			continue
		}
		rawInput := m["input"]
		encodedArgs := false
		if rawName == "DeferExecuteTool" {
			inMap, ok := rawInput.(map[string]any)
			if !ok {
				continue
			}
			rawName = frameStr(inMap, "toolName")
			if strings.TrimSpace(rawName) == "" {
				rawName = frameStr(inMap, "tool_name")
			}
			if params, exists := inMap["params"]; exists {
				rawInput = params
			} else if args, exists := inMap["arguments"]; exists {
				// arguments 与 params 的语义不同：它是已编码的参数串（对应 OpenAI
				// function.arguments），要校验后再压缩；params 是参数值本身，字符串
				// 就是字符串。两者混用会把 params:"{}" 这类字符串值误重解释成对象。
				rawInput = args
				encodedArgs = true
			} else {
				rawInput = nil
			}
		}
		if !strings.HasPrefix(rawName, "mcp__wb__") {
			continue
		}
		name := strings.TrimPrefix(rawName, "mcp__wb__")
		if name == "" {
			continue
		}
		if wbBridgeRegistry[name] != nil {
			serverUse = true
			continue
		}
		if allowlist != nil && !allowlist[name] {
			continue
		}
		var args string
		var err error
		if encodedArgs {
			args, err = wbCompactToolArgumentsText(rawInput)
		} else {
			args, err = wbCompactToolArgumentsValue(rawInput)
		}
		if err != nil {
			return nil, false, fmt.Errorf("tool_use %q id %q: %w", name, frameStr(m, "id"), err)
		}
		calls = append(calls, WBToolCall{
			ID:        frameStr(m, "id"),
			Name:      name,
			Arguments: args,
		})
	}
	return calls, serverUse, nil
}

// wbCompactToolArgumentsValue 把参数值编码成契约要求的紧凑 JSON 字符串。
// 这里处理的是「值」：对象、数组、字符串、数字一律按原类型编码，字符串仍然是
// JSON 字符串——把 "{}" 解成对象会凭空改变调用方收到的参数类型。
func wbCompactToolArgumentsValue(v any) (string, error) {
	if v == nil {
		return "{}", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("arguments cannot be encoded: %w", err)
	}
	if len(b) == 0 || string(b) == "null" {
		return "{}", nil
	}
	return string(b), nil
}

// wbCompactToolArgumentsText 处理已编码参数串（DeferExecuteTool 的 arguments 字段）：
// 先证明是合法 JSON 再压缩。原样透出非 JSON 文本会变成客户端解析不了的
// arguments，而包一层引号又与调用方期望的结构不符。非字符串值按值编码兜底。
func wbCompactToolArgumentsText(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return wbCompactToolArgumentsValue(v)
	}
	if strings.TrimSpace(s) == "" {
		return "{}", nil
	}
	if !json.Valid([]byte(s)) {
		return "", fmt.Errorf("arguments is not valid JSON")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return "", fmt.Errorf("arguments is not valid JSON: %w", err)
	}
	return buf.String(), nil
}

func uInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		switch n := m[k].(type) {
		case json.Number:
			if v, err := n.Int64(); err == nil {
				return int(v)
			}
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
	seenToolCalls                         map[string]bool
	nextAutoCallID                        int
	clientToolCalls                       []WBToolCall
}

func newWbStreamAccumulator() *wbStreamAccumulator {
	return &wbStreamAccumulator{seen: map[string]bool{}, seenToolCalls: map[string]bool{}}
}
func (a *wbStreamAccumulator) AddClientToolCalls(calls []WBToolCall) {
	batchRealIDs := make(map[string]bool, len(calls))
	for _, call := range calls {
		if call.ID != "" {
			batchRealIDs[call.ID] = true
		}
	}
	for idx, call := range calls {
		if call.ID != "" {
			if a.seenToolCalls[call.ID] {
				continue
			}
			a.seenToolCalls[call.ID] = true
			a.clientToolCalls = append(a.clientToolCalls, call)
			continue
		}
		anonKey := "\x00anon:" + strconv.Itoa(idx) + ":" + call.Name + ":" + call.Arguments
		if a.seenToolCalls[anonKey] {
			continue
		}
		a.seenToolCalls[anonKey] = true
		for {
			a.nextAutoCallID++
			candidate := "call_" + strconv.Itoa(a.nextAutoCallID)
			if !a.seenToolCalls[candidate] && !batchRealIDs[candidate] {
				call.ID = candidate
				a.seenToolCalls[candidate] = true
				break
			}
		}
		a.clientToolCalls = append(a.clientToolCalls, call)
	}
}
func (a *wbStreamAccumulator) ClientToolCalls() []WBToolCall { return a.clientToolCalls }
func (a *wbStreamAccumulator) OnMessageStart()               { a.StartMessage("") }
func (a *wbStreamAccumulator) StartMessage(id string) {
	a.delta, a.scopeID, a.lastFull, a.backfilled = "", id, "", false
}
func (a *wbStreamAccumulator) OnTextDelta(s string)                { a.delta += s; a.requestText += s }
func (a *wbStreamAccumulator) OnThinkingDelta(_ string)            {}
func (a *wbStreamAccumulator) OnAssistant(s string) (string, bool) { return a.Assistant("", s) }
func (a *wbStreamAccumulator) Assistant(id, full string) (string, bool) {
	if full == "" {
		return "", false
	}
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
