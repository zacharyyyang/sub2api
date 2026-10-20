package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const wbResumeContinuePrompt = "Please continue based on the tool execution results above."

func wbHasToolHistory(messages []wbInboundMessage) bool {
	for _, msg := range messages {
		if msg.Role == "tool" || msg.Role == "function" {
			return true
		}
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// wbMatchToolResult 在紧随当前 assistant 消息之后的连续 tool/function 段内优先按
// ToolCallID 或无 ID 的同名 Name 匹配回执；若连续段内未命中且 callID 非空，回退在全局
// 未消费的 tool/function 消息中按 callID 精确查找。
func wbMatchToolResult(messages []wbInboundMessage, limit, assistIdx int, callID, fnName string, consumed map[int]bool) string {
	segEnd := assistIdx + 1
	for segEnd < limit && (messages[segEnd].Role == "tool" || messages[segEnd].Role == "function") {
		segEnd++
	}
	if callID != "" {
		for j := assistIdx + 1; j < segEnd; j++ {
			if consumed[j] {
				continue
			}
			m := messages[j]
			if m.ToolCallID != nil && strings.TrimSpace(*m.ToolCallID) == callID {
				consumed[j] = true
				if m.Content != nil {
					return *m.Content
				}
				return ""
			}
		}
	}
	if fnName != "" {
		for j := assistIdx + 1; j < segEnd; j++ {
			if consumed[j] {
				continue
			}
			m := messages[j]
			hasID := m.ToolCallID != nil && strings.TrimSpace(*m.ToolCallID) != ""
			if !hasID && m.Name != nil && strings.TrimSpace(*m.Name) == fnName {
				consumed[j] = true
				if m.Content != nil {
					return *m.Content
				}
				return ""
			}
		}
	}
	if callID != "" {
		for j := segEnd; j < limit; j++ {
			m := messages[j]
			if m.Role == "assistant" {
				for _, tc := range m.ToolCalls {
					if strings.TrimSpace(tc.ID) == callID {
						return ""
					}
				}
				continue
			}
			if consumed[j] {
				continue
			}
			if (m.Role == "tool" || m.Role == "function") && m.ToolCallID != nil && strings.TrimSpace(*m.ToolCallID) == callID {
				consumed[j] = true
				if m.Content != nil {
					return *m.Content
				}
				return ""
			}
		}
	}
	return ""
}

func wbEmitDeferToolPair(emitNode func(map[string]any), callID, fnName, rawArgs, outText string) {
	var params any = map[string]any{}
	if trimmed := strings.TrimSpace(rawArgs); trimmed != "" {
		var parsed any
		if wbDecodeSingleJSONValue([]byte(trimmed), &parsed) == nil {
			params = parsed
		} else {
			params = map[string]any{"_raw": rawArgs}
		}
	}
	argBytes, _ := json.Marshal(map[string]any{
		"params":   params,
		"toolName": "mcp__wb__" + fnName,
	})
	emitNode(map[string]any{
		"type":      "function_call",
		"name":      "DeferExecuteTool",
		"callId":    callID,
		"arguments": string(argBytes),
	})
	emitNode(map[string]any{
		"type":   "function_call_result",
		"name":   "DeferExecuteTool",
		"callId": callID,
		"status": "completed",
		"output": map[string]any{"type": "text", "text": outText},
	})
}

func wbEmitToolSearchPreload(emitNode func(map[string]any), tools []WBBridgeTool, preloaded *bool) {
	if *preloaded || len(tools) == 0 {
		return
	}
	*preloaded = true
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = "mcp__wb__" + t.Name
	}
	qBytes, _ := json.Marshal(map[string]string{"query": "select:" + strings.Join(names, ",")})
	callID := "call_wb_preload"
	emitNode(map[string]any{
		"type":      "function_call",
		"name":      "ToolSearch",
		"callId":    callID,
		"arguments": string(qBytes),
	})
	emitNode(map[string]any{
		"type":   "function_call_result",
		"name":   "ToolSearch",
		"callId": callID,
		"status": "completed",
		"output": map[string]any{"type": "text", "text": "Tools: " + strings.Join(names, ", ")},
	})
}

// wbResumeWriter 承载会话合成过程中的共享状态：节点原子追加、系统前缀的一次性
// 注入，以及 ToolSearch 预热的一次性开关。各角色分支只描述自己该写哪些节点。
type wbResumeWriter struct {
	emitNode    func(map[string]any)
	tools       []WBBridgeTool
	sysPrefix   string
	sysInjected bool
	hasUserNode bool
	preloaded   bool
}

// flushSysPrefix 在没有 user 节点可承接系统前缀时，把它单独落成一个 user 节点。
func (w *wbResumeWriter) flushSysPrefix() {
	if w.sysInjected || w.sysPrefix == "" {
		return
	}
	w.emitNode(map[string]any{
		"type":    "message",
		"role":    "user",
		"content": []map[string]any{{"type": "input_text", "text": strings.TrimSpace(w.sysPrefix)}},
		"status":  "completed",
	})
	w.sysInjected = true
	w.hasUserNode = true
}

// writeUser 写一个 user 节点；首个 user 节点承接系统前缀。
func (w *wbResumeWriter) writeUser(text string) {
	if !w.sysInjected && w.sysPrefix != "" {
		text = w.sysPrefix + text
		w.sysInjected = true
	}
	w.hasUserNode = true
	w.emitNode(map[string]any{
		"type":    "message",
		"role":    "user",
		"content": []map[string]any{{"type": "input_text", "text": text}},
		"status":  "completed",
	})
}

// ensureUserNode 保证链上至少有一个 user 节点——孤立的 tool 回执没有父轮次时，
// CLI 会拒绝续接该会话。
func (w *wbResumeWriter) ensureUserNode() {
	if w.hasUserNode {
		return
	}
	if w.sysPrefix != "" {
		w.flushSysPrefix()
		if w.hasUserNode {
			return
		}
	}
	w.writeUser("Continue.")
}

// preload 在首个工具节点前注入 ToolSearch 预热对，让后续 DeferExecuteTool 调用
// 能解析到已声明的工具。
func (w *wbResumeWriter) preload() { wbEmitToolSearchPreload(w.emitNode, w.tools, &w.preloaded) }

// writeAssistant 写 assistant 文本节点，并把该消息声明的每个 tool_call 与其回执
// 配成 function_call / function_call_result 对。
func (w *wbResumeWriter) writeAssistant(messages []wbInboundMessage, idx, limit int, consumed map[int]bool) {
	msg := messages[idx]
	w.flushSysPrefix()
	if len(msg.ToolCalls) > 0 {
		// 工具调用节点必须挂在 user 轮次之后：历史直接以 assistant(tool_calls)
		// 开头、且无 system 前缀时，flushSysPrefix 不产生任何节点，紧随的
		// tool 回执里既有配对记录又会被 consumed 早退，合成链就没有 user 节点。
		w.ensureUserNode()
	}
	text := ""
	if msg.Content != nil {
		text = *msg.Content
	}
	if text != "" || len(msg.ToolCalls) == 0 {
		w.emitNode(map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": []map[string]any{{"type": "output_text", "text": text}},
			"status":  "completed",
		})
	}
	if len(msg.ToolCalls) == 0 {
		return
	}
	w.preload()
	for tcIdx, tc := range msg.ToolCalls {
		callID := strings.TrimSpace(tc.ID)
		if callID == "" {
			callID = fmt.Sprintf("call_%d_%d", idx+1, tcIdx+1)
		}
		fnName := strings.TrimSpace(tc.Function.Name)
		if fnName == "" {
			fnName = "unknown"
		}
		w.emitDeferPair(callID, fnName, tc.Function.Arguments, wbMatchToolResult(messages, limit, idx, callID, fnName, consumed))
	}
}

// writeToolResult 写一个未被前面 assistant 配走的 tool / function 回执节点。
func (w *wbResumeWriter) writeToolResult(messages []wbInboundMessage, idx int, consumed map[int]bool) {
	if consumed[idx] {
		return
	}
	msg := messages[idx]
	w.ensureUserNode()
	w.preload()
	callID := fmt.Sprintf("call_orphan_%d", idx+1)
	if msg.ToolCallID != nil && strings.TrimSpace(*msg.ToolCallID) != "" {
		callID = strings.TrimSpace(*msg.ToolCallID)
	}
	fnName := "unknown"
	if msg.Name != nil && strings.TrimSpace(*msg.Name) != "" {
		fnName = strings.TrimSpace(*msg.Name)
	}
	content := ""
	if msg.Content != nil {
		content = *msg.Content
	}
	w.emitDeferPair(callID, fnName, "", content)
}

func (w *wbResumeWriter) emitDeferPair(callID, fnName, rawArgs, outText string) {
	wbEmitDeferToolPair(w.emitNode, callID, fnName, rawArgs, outText)
}

// wbResumeSysPrefix 把全部 system 消息拼成交接给会话的系统前缀。
func wbResumeSysPrefix(messages []wbInboundMessage) string {
	var sysParts []string
	for _, msg := range messages {
		if msg.Role == "system" && msg.Content != nil && strings.TrimSpace(*msg.Content) != "" {
			sysParts = append(sysParts, *msg.Content)
		}
	}
	if len(sysParts) == 0 {
		return ""
	}
	return strings.Join(sysParts, "\n\n") + "\n\n"
}

func wbBuildResumeSession(messages []wbInboundMessage, tools []WBBridgeTool) (string, []byte, string) {
	sessionID := uuid.NewString()
	now := time.Now().UTC()
	step := 0
	var buf bytes.Buffer
	var prevID any
	w := &wbResumeWriter{tools: tools, sysPrefix: wbResumeSysPrefix(messages)}
	w.emitNode = func(node map[string]any) {
		id := uuid.NewString()
		node["id"] = id
		node["parentId"] = prevID
		node["sessionId"] = sessionID
		node["timestamp"] = now.Add(time.Duration(step) * time.Millisecond).Format(time.RFC3339Nano)
		step++
		prevID = id
		raw, _ := json.Marshal(node)
		buf.Write(raw)
		buf.WriteByte('\n')
	}

	limit := len(messages)
	resumePrompt := wbResumeContinuePrompt
	if limit > 0 && messages[limit-1].Role == "user" && messages[limit-1].Content != nil {
		resumePrompt = *messages[limit-1].Content
		limit--
	}

	consumed := make(map[int]bool)
	for idx := 0; idx < limit; idx++ {
		switch messages[idx].Role {
		case "user":
			text := ""
			if messages[idx].Content != nil {
				text = *messages[idx].Content
			}
			w.writeUser(text)
		case "assistant":
			w.writeAssistant(messages, idx, limit, consumed)
		case "tool", "function":
			w.writeToolResult(messages, idx, consumed)
		}
	}
	if !w.sysInjected && w.sysPrefix != "" {
		resumePrompt = w.sysPrefix + resumePrompt
	}
	if strings.TrimSpace(resumePrompt) == "" {
		resumePrompt = wbResumeContinuePrompt
	}
	return sessionID, buf.Bytes(), resumePrompt
}

// wbFoldInboundMessage 将单条入站消息折叠为确定性 prompt segment（每条消息
// 恰好一个 segment）。system / user 保持 System:/User: 纯文本折叠；tool /
// function 按 Tool[<id|name>]: <content> 折叠（均无则 Tool: <content>，
// content 为 nil 视为空）；assistant 支持 content 与 tool_calls 并存
// （[tool_calls: id=<id> name=<name> arguments=<args>; ...]），无 tool_calls 且
// content 为 nil 时空串兜底 Assistant: ；未知角色仍 400。
func wbFoldInboundMessage(i int, message wbInboundMessage) (string, error) {
	switch message.Role {
	case "system", "user":
		if message.Content == nil {
			return "", fmt.Errorf("messages[%d].content must be a string", i)
		}
		prefix := "System"
		if message.Role == "user" {
			prefix = "User"
		}
		return prefix + ": " + *message.Content, nil
	case "assistant":
		text := ""
		if message.Content != nil {
			text = *message.Content
		}
		if len(message.ToolCalls) > 0 {
			summary := "[tool_calls: " + wbToolCallSummary(message.ToolCalls) + "]"
			if text != "" {
				return "Assistant: " + text + "\n" + summary, nil
			}
			return "Assistant: " + summary, nil
		}
		return "Assistant: " + text, nil
	case "tool", "function":
		content := ""
		if message.Content != nil {
			content = *message.Content
		}
		return "Tool" + wbToolBracket(message) + ": " + content, nil
	default:
		return "", fmt.Errorf("unsupported role %q", message.Role)
	}
}

// wbToolBracket 计算 tool / function 消息折叠时的方括号括注：ToolCallID 优先，
// 为空则降级取 Name；两者皆空返回空串（不输出方括号），对应契约
// Tool[<id|name>]: <content> / Tool: <content>。
func wbToolBracket(message wbInboundMessage) string {
	switch {
	case message.ToolCallID != nil && *message.ToolCallID != "":
		return "[" + *message.ToolCallID + "]"
	case message.Name != nil && *message.Name != "":
		return "[" + *message.Name + "]"
	default:
		return ""
	}
}

// wbToolCallSummary 将 assistant 消息的 tool_calls 折叠为结构化摘要列表：
// 每个调用输出 id=<id> name=<name> arguments=<args>（id 缺省为 call_<i> 1-based，
// name 缺省取 ID 再空取 unknown，arguments 缺省为 {}），以 "; " 连接。
func wbToolCallSummary(calls []wbInboundToolCall) string {
	items := make([]string, len(calls))
	for i, call := range calls {
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i+1)
		}
		name := call.Function.Name
		if name == "" {
			if call.ID != "" {
				name = call.ID
			} else {
				name = "unknown"
			}
		}
		args := strings.TrimSpace(call.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		items[i] = fmt.Sprintf("id=%s name=%s arguments=%s", id, name, args)
	}
	return strings.Join(items, "; ")
}
