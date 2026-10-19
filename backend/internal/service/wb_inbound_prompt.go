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

func wbBuildResumeSession(messages []wbInboundMessage, tools []WBBridgeTool) (string, []byte, string) {
	sessionID := uuid.NewString()
	now := time.Now().UTC()
	step := 0
	var buf bytes.Buffer
	var prevID any
	emitNode := func(node map[string]any) {
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

	var sysParts []string
	for _, msg := range messages {
		if msg.Role == "system" && msg.Content != nil && strings.TrimSpace(*msg.Content) != "" {
			sysParts = append(sysParts, *msg.Content)
		}
	}
	sysPrefix := ""
	if len(sysParts) > 0 {
		sysPrefix = strings.Join(sysParts, "\n\n") + "\n\n"
	}

	limit := len(messages)
	resumePrompt := wbResumeContinuePrompt
	if limit > 0 && messages[limit-1].Role == "user" && messages[limit-1].Content != nil {
		resumePrompt = *messages[limit-1].Content
		limit--
	}

	consumedToolIdx := make(map[int]bool)
	preloadedTools := false
	sysInjected := false
	hasUserNode := false
	for idx := 0; idx < limit; idx++ {
		msg := messages[idx]
		switch msg.Role {
		case "system":
			continue
		case "user":
			text := ""
			if msg.Content != nil {
				text = *msg.Content
			}
			if !sysInjected && sysPrefix != "" {
				text = sysPrefix + text
				sysInjected = true
			}
			hasUserNode = true
			emitNode(map[string]any{
				"type":    "message",
				"role":    "user",
				"content": []map[string]any{{"type": "input_text", "text": text}},
				"status":  "completed",
			})
		case "assistant":
			if !sysInjected && sysPrefix != "" {
				emitNode(map[string]any{
					"type":    "message",
					"role":    "user",
					"content": []map[string]any{{"type": "input_text", "text": strings.TrimSpace(sysPrefix)}},
					"status":  "completed",
				})
				sysInjected = true
				hasUserNode = true
			}
			text := ""
			if msg.Content != nil {
				text = *msg.Content
			}
			if text != "" || len(msg.ToolCalls) == 0 {
				emitNode(map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": []map[string]any{{"type": "output_text", "text": text}},
					"status":  "completed",
				})
			}
			if len(msg.ToolCalls) > 0 {
				wbEmitToolSearchPreload(emitNode, tools, &preloadedTools)
				for tcIdx, tc := range msg.ToolCalls {
					callID := strings.TrimSpace(tc.ID)
					if callID == "" {
						callID = fmt.Sprintf("call_%d_%d", idx+1, tcIdx+1)
					}
					fnName := strings.TrimSpace(tc.Function.Name)
					if fnName == "" {
						fnName = "unknown"
					}
					outText := wbMatchToolResult(messages, limit, idx, callID, fnName, consumedToolIdx)
					wbEmitDeferToolPair(emitNode, callID, fnName, tc.Function.Arguments, outText)
				}
			}
		case "tool", "function":
			if consumedToolIdx[idx] {
				continue
			}
			if !hasUserNode {
				seed := "Continue."
				if sysPrefix != "" {
					seed = strings.TrimSpace(sysPrefix)
					sysInjected = true
				}
				emitNode(map[string]any{
					"type":    "message",
					"role":    "user",
					"content": []map[string]any{{"type": "input_text", "text": seed}},
					"status":  "completed",
				})
				hasUserNode = true
			}
			wbEmitToolSearchPreload(emitNode, tools, &preloadedTools)
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
			wbEmitDeferToolPair(emitNode, callID, fnName, "", content)
		}
	}
	if !sysInjected && sysPrefix != "" {
		resumePrompt = sysPrefix + resumePrompt
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
