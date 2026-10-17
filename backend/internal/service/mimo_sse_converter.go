package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type MimoOpenAISSEToAnthropicSSEConverter struct {
	model, activeBlockType, stopReason, messageID string
	started, finished                             bool
	blockIndex                                    int
	toolIndexByKey                                map[string]int
	inputTokens, outputTokens, cacheReadTokens    int
}

func NewMimoOpenAISSEToAnthropicSSEConverter(model string) *MimoOpenAISSEToAnthropicSSEConverter {
	return &MimoOpenAISSEToAnthropicSSEConverter{
		model: model, blockIndex: -1, toolIndexByKey: make(map[string]int, 4), messageID: "msg_mimo",
	}
}

func (c *MimoOpenAISSEToAnthropicSSEConverter) PushLine(line string) []string {
	trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if !strings.HasPrefix(trimmed, "data:") {
		return nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" {
		return nil
	}
	if payload == "[DONE]" {
		return c.Finalize()
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return nil
	}
	return c.pushChunk(chunk)
}

func (c *MimoOpenAISSEToAnthropicSSEConverter) pushChunk(chunk map[string]any) []string {
	var out []string
	if id := strings.TrimSpace(qoderAnyToStr(chunk["id"])); id != "" {
		c.messageID = "msg_" + strings.TrimPrefix(id, "msg_")
	}
	if u := extractMimoChatUsage(chunk); u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadInputTokens > 0 {
		c.inputTokens, c.outputTokens, c.cacheReadTokens = u.InputTokens, u.OutputTokens, u.CacheReadInputTokens
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return out
	}
	ch, _ := choices[0].(map[string]any)
	if ch == nil {
		return out
	}
	switch strings.TrimSpace(qoderAnyToStr(ch["finish_reason"])) {
	case "tool_calls":
		c.stopReason = "tool_use"
	case "length":
		c.stopReason = "max_tokens"
	case "stop":
		c.stopReason = "end_turn"
	}
	delta, _ := ch["delta"].(map[string]any)
	if delta == nil {
		return out
	}
	out = append(out, c.ensureMessageStarted()...)
	if rc := qoderAnyToStr(delta["reasoning_content"]); rc != "" {
		out = append(out, c.ensureBlock("thinking")...)
		out = append(out, formatMimoAnthropicSSE("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": c.blockIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": rc},
		}))
	}
	if txt := qoderAnyToStr(delta["content"]); txt != "" {
		out = append(out, c.ensureBlock("text")...)
		out = append(out, formatMimoAnthropicSSE("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": c.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": txt},
		}))
	}
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, item := range tcs {
			if tc, ok := item.(map[string]any); ok {
				out = append(out, c.pushToolCallDelta(tc)...)
			}
		}
	}
	return out
}

func (c *MimoOpenAISSEToAnthropicSSEConverter) pushToolCallDelta(tc map[string]any) []string {
	var out []string
	fn, _ := tc["function"].(map[string]any)
	id := strings.TrimSpace(qoderAnyToStr(tc["id"]))
	idxKey := fmt.Sprintf("idx_%d", qoderAnyToInt(tc["index"]))
	idx, exists := c.toolIndexByKey[idxKey]
	if !exists && id != "" {
		idx, exists = c.toolIndexByKey[id]
	}
	if !exists {
		out = append(out, c.closeActiveBlock()...)
		c.blockIndex++
		idx = c.blockIndex
		c.toolIndexByKey[idxKey] = idx
		c.activeBlockType = "tool_use"
		toolID := qoderFirstTrimNonEmpty(id, fmt.Sprintf("toolu_mimo_%d", idx))
		out = append(out, formatMimoAnthropicSSE("content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "tool_use", "id": toolID, "name": qoderAnyToStr(fn["name"]), "input": map[string]any{}},
		}))
	}
	if id != "" {
		c.toolIndexByKey[id] = idx
	}
	if args := qoderAnyToStr(fn["arguments"]); args != "" {
		out = append(out, formatMimoAnthropicSSE("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		}))
	}
	return out
}

func (c *MimoOpenAISSEToAnthropicSSEConverter) ensureMessageStarted() []string {
	if c.started {
		return nil
	}
	c.started = true
	nonCached := max(c.inputTokens-c.cacheReadTokens, 0)
	return []string{formatMimoAnthropicSSE("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": c.messageID, "type": "message", "role": "assistant", "model": c.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": nonCached, "output_tokens": 0, "cache_read_input_tokens": c.cacheReadTokens},
		},
	})}
}

func (c *MimoOpenAISSEToAnthropicSSEConverter) ensureBlock(bType string) []string {
	if c.activeBlockType == bType {
		return nil
	}
	out := c.closeActiveBlock()
	c.blockIndex++
	c.activeBlockType = bType
	contentBlock := map[string]any{"type": bType, "text": ""}
	if bType == "thinking" {
		contentBlock = map[string]any{"type": "thinking", "thinking": ""}
	}
	return append(out, formatMimoAnthropicSSE("content_block_start", map[string]any{
		"type": "content_block_start", "index": c.blockIndex, "content_block": contentBlock,
	}))
}

func (c *MimoOpenAISSEToAnthropicSSEConverter) closeActiveBlock() []string {
	if c.activeBlockType == "" {
		return nil
	}
	c.activeBlockType = ""
	return []string{formatMimoAnthropicSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": c.blockIndex})}
}

func (c *MimoOpenAISSEToAnthropicSSEConverter) Finalize() []string {
	if c.finished {
		return nil
	}
	c.finished = true
	var out []string
	out = append(out, c.ensureMessageStarted()...)
	out = append(out, c.closeActiveBlock()...)
	stopReason, nonCached := qoderFirstTrimNonEmpty(c.stopReason, "end_turn"), max(c.inputTokens-c.cacheReadTokens, 0)
	out = append(out, formatMimoAnthropicSSE("message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": nonCached, "output_tokens": c.outputTokens, "cache_read_input_tokens": c.cacheReadTokens},
	}))
	return append(out, formatMimoAnthropicSSE("message_stop", map[string]any{"type": "message_stop"}))
}

func (c *MimoOpenAISSEToAnthropicSSEConverter) Usage() OpenAIUsage {
	return OpenAIUsage{InputTokens: c.inputTokens, OutputTokens: c.outputTokens, CacheReadInputTokens: c.cacheReadTokens}
}

func formatMimoAnthropicSSE(event string, data any) string {
	b, _ := json.Marshal(data)
	return "event: " + event + "\ndata: " + string(b) + "\n\n"
}

func (s *OpenAIGatewayService) forwardMimoAnthropicMessages(ctx context.Context, c *gin.Context, account *Account, body []byte, defaultMappedModel string) (*OpenAIForwardResult, error) {
	started := time.Now()
	SetActualOpenAIUpstreamEndpoint(c, mimoUpstreamEndpoint)
	reqModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if reqModel == "" {
		return nil, mimoWriteErrorJSON(c, http.StatusBadRequest, mimoErrInvalidRequest, "model is required", true)
	}
	upstreamModel, modelEffort, err := resolveMimoMappedModelAndEffort(account, reqModel, defaultMappedModel)
	if err != nil {
		return nil, mimoWriteErrorJSON(c, http.StatusBadRequest, mimoErrInvalidRequest, err.Error(), true)
	}
	SetOpsUpstreamModel(c, upstreamModel)
	chatPayload, err := AnthropicMessagesToMimoChat(body, upstreamModel, modelEffort)
	if err != nil {
		var compatErr *MimoMessagesCompatError
		if errors.As(err, &compatErr) && compatErr != nil {
			return nil, mimoWriteErrorJSON(c, compatErr.Status, compatErr.Code, compatErr.Message, true)
		}
		return nil, mimoWriteErrorJSON(c, http.StatusBadRequest, mimoErrInvalidRequest, err.Error(), true)
	}
	isStream := gjson.GetBytes(body, "stream").Bool()
	resp, err := s.doMimoChatRequestWithRetry(ctx, account, chatPayload, isStream)
	if err != nil {
		return nil, mimoWriteErrorJSON(c, http.StatusBadGateway, mimoErrUpstream, err.Error(), true)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return s.handleMimoUpstreamNon2xx(ctx, c, account, resp, upstreamModel, true)
	}
	defer func() { _ = resp.Body.Close() }()
	var effPtr *string
	if eff := strings.TrimSpace(gjson.GetBytes(chatPayload, "reasoning_effort").String()); eff != "" {
		effPtr = &eff
	}
	if isStream {
		return s.streamMimoOpenAIToAnthropicSSE(c, resp, reqModel, upstreamModel, effPtr, started)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	if err != nil {
		return nil, mimoWriteErrorJSON(c, http.StatusBadGateway, mimoErrUpstream, err.Error(), true)
	}
	anthBytes, usage, err := MimoChatToAnthropicMessage(raw, reqModel)
	if err != nil {
		return nil, mimoWriteErrorJSON(c, http.StatusBadGateway, mimoErrUpstream, err.Error(), true)
	}
	if c != nil {
		c.Data(http.StatusOK, "application/json; charset=utf-8", anthBytes)
	}
	return newMimoForwardResult(resp, reqModel, upstreamModel, effPtr, false, started, nil, usage), nil
}

func (s *OpenAIGatewayService) streamMimoOpenAIToAnthropicSSE(c *gin.Context, resp *http.Response, reqModel, upstreamModel string, effPtr *string, started time.Time) (*OpenAIForwardResult, error) {
	conv := NewMimoOpenAISSEToAnthropicSSEConverter(reqModel)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	var firstTokenMs *int
	headersWritten, sawData := false, false
	writeEvents := func(events []string) {
		if len(events) == 0 || c == nil {
			return
		}
		if !headersWritten {
			c.Header("Content-Type", "text/event-stream; charset=utf-8")
			c.Header("Cache-Control", "no-cache")
			headersWritten = true
		}
		if firstTokenMs == nil {
			ms := int(time.Since(started).Milliseconds())
			firstTokenMs = &ms
		}
		for _, ev := range events {
			_, _ = c.Writer.WriteString(ev)
		}
		c.Writer.Flush()
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "data:") {
			sawData = true
		}
		writeEvents(conv.PushLine(line))
	}
	if scanErr := scanner.Err(); scanErr != nil {
		if headersWritten {
			writeEvents([]string{formatMimoAnthropicSSE("error", map[string]any{"type": "error", "error": map[string]any{"type": mimoErrUpstream, "message": mimoTruncatedStream}})})
			return newMimoForwardResult(resp, reqModel, upstreamModel, effPtr, true, started, firstTokenMs, conv.Usage()), fmt.Errorf("stream usage incomplete: %w", scanErr)
		}
		return nil, mimoWriteErrorJSON(c, http.StatusBadGateway, mimoErrUpstream, mimoEmptyStreamMsg, true)
	}
	if !sawData && !conv.started {
		return nil, mimoWriteErrorJSON(c, http.StatusBadGateway, mimoErrUpstream, mimoEmptyStreamMsg, true)
	}
	writeEvents(conv.Finalize())
	return newMimoForwardResult(resp, reqModel, upstreamModel, effPtr, true, started, firstTokenMs, conv.Usage()), nil
}
