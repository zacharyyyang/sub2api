package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

func AnthropicMessagesToMimoChat(rawBody []byte, upstreamModel string, modelEffort string) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(rawBody, &req); err != nil {
		return nil, fmt.Errorf("invalid anthropic messages json: %w", err)
	}
	isVision := domain.IsMimoVisionModel(upstreamModel)
	outMsgs := make([]map[string]any, 0, 8)
	if sysMsg, err := convertMimoSystemPrompt(req["system"], upstreamModel, isVision); err != nil {
		return nil, err
	} else if sysMsg != nil {
		outMsgs = append(outMsgs, sysMsg)
	}
	inMsgs, _ := req["messages"].([]any)
	for _, item := range inMsgs {
		if msgMap, ok := item.(map[string]any); ok {
			converted, err := convertMimoAnthropicMessage(msgMap, upstreamModel, isVision)
			if err != nil {
				return nil, err
			}
			outMsgs = append(outMsgs, converted...)
		}
	}
	out := map[string]any{"model": upstreamModel, "messages": outMsgs}
	if tools, err := convertMimoTools(req["tools"]); err != nil {
		return nil, err
	} else if len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := convertMimoToolChoice(req["tool_choice"]); tc != nil {
		out["tool_choice"] = tc
	}
	if effort := resolveMimoAnthropicReasoningEffort(req, modelEffort); effort != "" {
		out["reasoning_effort"] = effort
	}
	for _, k := range []string{"max_tokens", "temperature", "top_p", "stream"} {
		if v, ok := req[k]; ok && v != nil {
			out[k] = v
		}
	}
	if stopSeq, ok := req["stop_sequences"]; ok && stopSeq != nil {
		out["stop"] = stopSeq
	}
	if stream, _ := req["stream"].(bool); stream {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	return json.Marshal(out)
}

func convertMimoSystemPrompt(sys any, upstreamModel string, _ bool) (map[string]any, error) {
	switch v := sys.(type) {
	case string:
		if v != "" {
			return map[string]any{"role": "system", "content": v}, nil
		}
	case []any:
		var sb strings.Builder
		for _, item := range v {
			bm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			bType := strings.TrimSpace(qoderAnyToStr(bm["type"]))
			if bType == "image" || bType == "document" {
				return nil, newMimoModalityError(upstreamModel, bType)
			}
			if bType != "" && bType != "text" {
				return nil, &MimoMessagesCompatError{Status: 422, Code: "UNKNOWN_FIELD", Message: "unsupported system block type: " + bType}
			}
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(qoderAnyToStr(bm["text"]))
		}
		if sb.Len() > 0 {
			return map[string]any{"role": "system", "content": sb.String()}, nil
		}
	}
	return nil, nil
}

func convertMimoAnthropicMessage(msg map[string]any, upstreamModel string, isVision bool) ([]map[string]any, error) {
	role := strings.TrimSpace(qoderAnyToStr(msg["role"]))
	if s, ok := msg["content"].(string); ok {
		return []map[string]any{{"role": role, "content": s}}, nil
	}
	blocks, _ := msg["content"].([]any)
	if role == "assistant" {
		return convertMimoAssistantBlocks(blocks)
	}
	return convertMimoUserBlocks(blocks, upstreamModel, isVision)
}

func convertMimoAssistantBlocks(blocks []any) ([]map[string]any, error) {
	var textParts []string
	var toolCalls []map[string]any
	for i, item := range blocks {
		bm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		bType := strings.TrimSpace(qoderAnyToStr(bm["type"]))
		switch bType {
		case "thinking", "redacted_thinking":
			continue
		case "text":
			if t := qoderAnyToStr(bm["text"]); t != "" {
				textParts = append(textParts, t)
			}
		case "tool_use":
			id := qoderFirstTrimNonEmpty(qoderAnyToStr(bm["id"]), fmt.Sprintf("call_%d", i))
			argsBytes, _ := json.Marshal(bm["input"])
			if len(argsBytes) == 0 || string(argsBytes) == "null" {
				argsBytes = []byte("{}")
			}
			toolCalls = append(toolCalls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": qoderAnyToStr(bm["name"]), "arguments": string(argsBytes)}})
		default:
			return nil, &MimoMessagesCompatError{Status: 422, Code: "UNKNOWN_FIELD", Message: "unsupported assistant block type: " + bType}
		}
	}
	out := map[string]any{"role": "assistant", "content": strings.Join(textParts, "")}
	if len(toolCalls) > 0 {
		out["tool_calls"] = toolCalls
	}
	return []map[string]any{out}, nil
}

func convertMimoUserBlocks(blocks []any, upstreamModel string, isVision bool) ([]map[string]any, error) {
	var out, contentParts []map[string]any
	var textOnly []string
	hasImage := false
	for _, item := range blocks {
		bm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		bType := strings.TrimSpace(qoderAnyToStr(bm["type"]))
		switch bType {
		case "tool_result":
			payload, imgParts, err := extractMimoToolResultContent(bm["content"], upstreamModel, isVision)
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": qoderAnyToStr(bm["tool_use_id"]), "content": payload})
			if len(imgParts) > 0 {
				hasImage, contentParts = true, append(contentParts, imgParts...)
			}
		case "text":
			txt := qoderAnyToStr(bm["text"])
			textOnly, contentParts = append(textOnly, txt), append(contentParts, map[string]any{"type": "text", "text": txt})
		case "image":
			imgURL := convertAnthropicImageBlockToURL(bm)
			if !isVision || imgURL == "" {
				return nil, newMimoModalityError(upstreamModel, "image")
			}
			hasImage, contentParts = true, append(contentParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imgURL}})
		case "document":
			return nil, newMimoModalityError(upstreamModel, "document")
		default:
			return nil, &MimoMessagesCompatError{Status: 422, Code: "UNKNOWN_FIELD", Message: "unsupported user block type: " + bType}
		}
	}
	if hasImage {
		out = append(out, map[string]any{"role": "user", "content": contentParts})
	} else if len(textOnly) > 0 || len(out) == 0 {
		out = append(out, map[string]any{"role": "user", "content": strings.Join(textOnly, "\n")})
	}
	return out, nil
}

func extractMimoToolResultContent(raw any, upstreamModel string, isVision bool) (string, []map[string]any, error) {
	if s, ok := raw.(string); ok {
		return s, nil, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		if raw == nil {
			return "", nil, nil
		}
		b, _ := json.Marshal(raw)
		return string(b), nil, nil
	}
	var parts []string
	var imgParts []map[string]any
	for _, item := range arr {
		bm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		bType := strings.TrimSpace(qoderAnyToStr(bm["type"]))
		switch bType {
		case "", "text":
			parts = append(parts, qoderAnyToStr(bm["text"]))
		case "image":
			imgURL := convertAnthropicImageBlockToURL(bm)
			if !isVision || imgURL == "" {
				return "", nil, newMimoModalityError(upstreamModel, "image")
			}
			imgParts = append(imgParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imgURL}})
		case "document":
			return "", nil, newMimoModalityError(upstreamModel, "document")
		default:
			return "", nil, &MimoMessagesCompatError{Status: 422, Code: "UNKNOWN_FIELD", Message: "unsupported tool_result block type: " + bType}
		}
	}
	return strings.Join(parts, "\n"), imgParts, nil
}

func convertMimoTools(raw any) ([]map[string]any, error) {
	arr, _ := raw.([]any)
	var out []map[string]any
	for _, item := range arr {
		tm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if tType := strings.TrimSpace(qoderAnyToStr(tm["type"])); tType != "" && tType != "tool" && tType != "custom" {
			return nil, &MimoMessagesCompatError{Status: 422, Code: "UNKNOWN_FIELD", Message: "unsupported tool type: " + tType}
		}
		if strings.TrimSpace(qoderAnyToStr(tm["name"])) == "" {
			continue
		}
		params := tm["input_schema"]
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": map[string]any{"name": strings.TrimSpace(qoderAnyToStr(tm["name"])), "description": qoderAnyToStr(tm["description"]), "parameters": params}})
	}
	return out, nil
}

func MimoChatToAnthropicMessage(rawResp []byte, requestedModel string) ([]byte, OpenAIUsage, error) {
	var data map[string]any
	if err := json.Unmarshal(rawResp, &data); err != nil {
		return nil, OpenAIUsage{}, fmt.Errorf("invalid mimo chat response json: %w", err)
	}
	blocks, stopReason := buildMimoAnthropicContentBlocks(data)
	usage := extractMimoChatUsage(data)
	nonCachedInput := max(usage.InputTokens-usage.CacheReadInputTokens, 0)
	out := map[string]any{
		"id": qoderFirstTrimNonEmpty(qoderAnyToStr(data["id"]), "msg_mimo"), "type": "message", "role": "assistant", "model": requestedModel,
		"content": blocks, "stop_reason": stopReason, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": nonCachedInput, "output_tokens": usage.OutputTokens, "cache_read_input_tokens": usage.CacheReadInputTokens},
	}
	b, err := json.Marshal(out)
	return b, usage, err
}

func buildMimoAnthropicContentBlocks(data map[string]any) ([]map[string]any, string) {
	choices, _ := data["choices"].([]any)
	var msg map[string]any
	finishReason := "stop"
	if len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			msg, _ = ch["message"].(map[string]any)
			finishReason = qoderFirstTrimNonEmpty(qoderAnyToStr(ch["finish_reason"]), "stop")
		}
	}
	var blocks []map[string]any
	if msg != nil {
		if rc := qoderAnyToStr(msg["reasoning_content"]); rc != "" {
			blocks = append(blocks, map[string]any{"type": "thinking", "thinking": rc})
		}
		if c := qoderAnyToStr(msg["content"]); c != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": c})
		}
		if tcs, ok := msg["tool_calls"].([]any); ok {
			for i, item := range tcs {
				if tc, ok := item.(map[string]any); ok {
					fn, _ := tc["function"].(map[string]any)
					var inputObj any = map[string]any{}
					if rawArgs := strings.TrimSpace(qoderAnyToStr(fn["arguments"])); rawArgs != "" {
						_ = json.Unmarshal([]byte(rawArgs), &inputObj)
					}
					blocks = append(blocks, map[string]any{"type": "tool_use", "id": qoderFirstTrimNonEmpty(qoderAnyToStr(tc["id"]), fmt.Sprintf("toolu_%d", i)), "name": qoderAnyToStr(fn["name"]), "input": inputObj})
				}
			}
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	if strings.TrimSpace(finishReason) == "tool_calls" {
		return blocks, "tool_use"
	} else if strings.TrimSpace(finishReason) == "length" {
		return blocks, "max_tokens"
	}
	return blocks, "end_turn"
}
