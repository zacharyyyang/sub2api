package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/google/uuid"
)

const (
	qoderQueueCode, qoderQueueDefaultRetry = 10605, 2 * time.Second
	qoderQueueMinRetry, qoderQueueMaxRetry = 2 * time.Second, 8 * time.Second
)

type QoderOuterFrameResult struct {
	Queue, FatalError, Skip, Corrupt bool
	RetryAfter                       time.Duration
	Status                           int
	Message                          string
	Data                             map[string]any
}

type QoderToolCallAccumulator struct {
	ID, Type, Name string
	Arguments      strings.Builder
}

type QoderNonStreamAggregator struct {
	ID, Model, FinishReason                 string
	Created                                 int64
	Content, ReasoningContent               strings.Builder
	ToolCalls                               map[int]*QoderToolCallAccumulator
	PromptTokens, OutputTokens, TotalTokens int
	CachedInputTokens, EffectiveCount       int
}

func BuildQoderV3Request(openAIBody []byte, creds *QoderCredentials) ([]byte, string, bool, error) {
	var raw map[string]any
	if err := json.Unmarshal(openAIBody, &raw); err != nil {
		return nil, "", false, fmt.Errorf("invalid openai request json: %w", err)
	}
	nativeModel, err := domain.NormalizeQoderModel(qoderAnyToStr(raw["model"]))
	if err != nil {
		return nil, "", false, err
	}
	isStream, _ := raw["stream"].(bool)
	rawMsgs, _ := raw["messages"].([]any)
	msgs, lastUserText := normalizeQoderMessages(rawMsgs)
	tools := normalizeQoderTools(raw["tools"])
	temp, maxTokens := resolveQoderTemperature(nativeModel, raw["temperature"]), qoderAnyToInt(raw["max_tokens"])
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	reqID, sessID := uuid.NewString(), uuid.NewString()
	params := map[string]any{"max_tokens": maxTokens, "temperature": temp}
	v3 := map[string]any{
		"version": "3", "request_id": reqID, "request_set_id": reqID, "chat_record_id": reqID, "session_id": sessID,
		"stream": true, "chat_task": "FREE_INPUT",
		"chat_context": map[string]any{
			"text": lastUserText, "features": []any{}, "chatPrompt": "", "imageUrls": nil,
			"extra": map[string]any{"context": []any{}, "modelConfig": map[string]any{"key": nativeModel, "is_reasoning": true}, "originalContent": lastUserText},
		},
		"is_reply": true, "is_retry": false, "source": 1, "agent_id": "agent_common", "task_id": "common", "session_type": "cli", "aliyun_user_type": "",
		"model_config": map[string]any{
			"key": nativeModel, "display_name": nativeModel, "model": "", "format": "openai",
			"is_vl": true, "is_reasoning": true, "api_key": "", "url": "", "source": "system", "max_input_tokens": 200000,
		},
		"system": []any{}, "messages": msgs, "tools": tools, "parameters": params,
		"model": nativeModel, "temperature": temp, "max_tokens": maxTokens,
	}
	if tc, exists := raw["tool_choice"]; exists && tc != nil {
		v3["tool_choice"], params["tool_choice"] = tc, tc
	} else if len(tools) > 0 {
		v3["tool_choice"], params["tool_choice"] = "auto", "auto"
	}
	if creds != nil {
		v3["user_id"], v3["organization_id"] = creds.UID, creds.OrgID
	}
	v3Body, err := json.Marshal(v3)
	return v3Body, nativeModel, isStream, err
}

func normalizeQoderMessages(rawMsgs []any) ([]map[string]any, string) {
	out, lastUser := make([]map[string]any, 0, len(rawMsgs)), ""
	for _, item := range rawMsgs {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		cloned := make(map[string]any, len(m))
		for k, v := range m {
			cloned[k] = v
		}
		role := qoderAnyToStr(cloned["role"])
		if role == "assistant" && cloned["content"] == nil {
			cloned["content"] = ""
		}
		if role == "user" {
			if s, ok := cloned["content"].(string); ok {
				lastUser = s
			} else if parts, ok := cloned["content"].([]any); ok {
				var sb strings.Builder
				for _, p := range parts {
					if pm, ok := p.(map[string]any); ok && qoderAnyToStr(pm["type"]) == "text" {
						sb.WriteString(qoderAnyToStr(pm["text"]))
					}
				}
				lastUser = sb.String()
			}
		}
		out = append(out, cloned)
	}
	return out, lastUser
}

func normalizeQoderTools(rawTools any) []map[string]any {
	arr, _ := rawTools.([]any)
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		tm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if qoderAnyToStr(tm["type"]) != "function" || !ok {
			out = append(out, tm)
			continue
		}
		cleanFn := map[string]any{"name": fn["name"]}
		if desc, exists := fn["description"]; exists && desc != nil {
			cleanFn["description"] = desc
		}
		if params, exists := fn["parameters"]; exists && params != nil {
			cleanFn["parameters"] = params
		}
		out = append(out, map[string]any{"type": "function", "function": cleanFn})
	}
	return out
}

func resolveQoderTemperature(nativeModel string, rawTemp any) float64 {
	if f, ok := qoderToFloat64(rawTemp); ok {
		return f
	}
	if strings.HasPrefix(strings.ToLower(nativeModel), "kmodel") {
		return 1.0
	}
	return 0.7
}

func ParseQoderOuterFrame(raw []byte) (QoderOuterFrameResult, error) {
	var outer map[string]any
	if err := json.Unmarshal(raw, &outer); err != nil {
		return QoderOuterFrameResult{Corrupt: true}, err
	}
	status := 200
	if n := qoderAnyToInt(outer["statusCodeValue"]); n != 0 {
		status = n
	}
	inner, skip, corrupt := extractQoderInnerMap(outer)
	if skip {
		if status == 200 {
			return QoderOuterFrameResult{Skip: true, Status: 200}, nil
		}
		return QoderOuterFrameResult{FatalError: true, Status: status, Message: fmt.Sprintf("upstream status %d", status)}, nil
	}
	if corrupt {
		return QoderOuterFrameResult{Corrupt: true}, errors.New("corrupt inner body")
	}
	choices, hasChoices := inner["choices"].([]any)
	flatInner := !hasChoices && (inner["code"] != nil || inner["message"] != nil || inner["status"] != nil || inner["statusCode"] != nil)
	if status != 200 || inner["error"] != nil || flatInner {
		return buildQoderErrorFrameResult(status, inner), nil
	}
	if !hasChoices || !validateQoderChoices(choices) {
		return QoderOuterFrameResult{Corrupt: true}, errors.New("missing or invalid choices array")
	}
	return QoderOuterFrameResult{Data: inner, Status: 200}, nil
}

func validateQoderChoices(choices []any) bool {
	for _, item := range choices {
		ch, ok := item.(map[string]any)
		if !ok || ch == nil {
			return false
		}
		if d, ok := ch["delta"].(map[string]any); ok && d["tool_calls"] != nil {
			tcs, ok := d["tool_calls"].([]any)
			if !ok {
				return false
			}
			for _, t := range tcs {
				if _, ok := t.(map[string]any); !ok || t == nil {
					return false
				}
			}
		}
	}
	return true
}

func extractQoderInnerMap(outer map[string]any) (inner map[string]any, skip, corrupt bool) {
	rawBody, hasBody := outer["body"]
	if !hasBody || rawBody == nil {
		return nil, true, false
	}
	if s, ok := rawBody.(string); ok {
		s = strings.TrimSpace(s)
		if s == "" || s == "[DONE]" {
			return nil, true, false
		}
		if err := json.Unmarshal([]byte(s), &inner); err != nil {
			return nil, false, true
		}
		return inner, false, false
	}
	if m, ok := rawBody.(map[string]any); ok {
		return m, false, false
	}
	return nil, false, true
}

func buildQoderErrorFrameResult(outerStatus int, inner map[string]any) QoderOuterFrameResult {
	var dErr, mObj, mInner map[string]any
	if ds := strings.TrimSpace(qoderAnyToStr(inner["details"])); strings.HasPrefix(ds, "{") && json.Unmarshal([]byte(ds), &dErr) == nil {
		if e, ok := dErr["error"].(map[string]any); ok {
			dErr = e
		}
	}
	errObj, ok := inner["error"].(map[string]any)
	if !ok {
		errObj = inner
	}
	msg := qoderFirstTrimNonEmpty(qoderAnyToStr(errObj["message"]), qoderAnyToStr(dErr["message"]), qoderAnyToStr(inner["message"]), qoderAnyToStr(inner["msg"]))
	if strings.HasPrefix(strings.TrimSpace(msg), "{") && json.Unmarshal([]byte(msg), &mObj) == nil {
		if ms := strings.TrimSpace(qoderAnyToStr(mObj["message"])); strings.HasPrefix(ms, "{") {
			_ = json.Unmarshal([]byte(ms), &mInner)
		}
	}
	if qoderAnyToInt(errObj["code"]) == qoderQueueCode || qoderAnyToInt(mObj["code"]) == qoderQueueCode || qoderAnyToInt(inner["code"]) == qoderQueueCode {
		return QoderOuterFrameResult{Queue: true, Status: 429, RetryAfter: resolveQueueRetryAfter(errObj, mObj, mInner, inner), Message: qoderFirstTrimNonEmpty(msg, "qoder request queued (10605)")}
	}
	cand := 0
	for _, v := range []any{errObj["status"], errObj["statusCode"], errObj["code"], dErr["status"], dErr["statusCode"], dErr["code"], mObj["status"], mObj["statusCode"], inner["status"], inner["statusCode"]} {
		if n := qoderAnyToInt(v); n >= 400 && n <= 599 {
			cand = n
			break
		}
	}
	if cand == 0 {
		cand = 502
		if strings.Contains(strings.ToLower(msg), "invalid temperature") {
			cand = 400
		}
	}
	finalStatus := cand
	if outerStatus != 200 && cand == 502 {
		finalStatus = outerStatus
	}
	if msg == "" {
		msg = fmt.Sprintf("upstream status %d", finalStatus)
	}
	return QoderOuterFrameResult{FatalError: true, Status: finalStatus, Message: msg}
}

func resolveQueueRetryAfter(maps ...map[string]any) time.Duration {
	for _, m := range maps {
		if m == nil {
			continue
		}
		pm, _ := m["param"].(map[string]any)
		for _, src := range []map[string]any{pm, m} {
			for _, k := range []string{"retryAfterSeconds", "retry_after_seconds", "retry_after"} {
				if sec, ok := qoderToFloat64(src[k]); ok && sec > 0 {
					return time.Duration(min(max(2000, int(sec*1000)), 8000)) * time.Millisecond
				}
			}
		}
	}
	return qoderQueueDefaultRetry
}

func NewQoderNonStreamAggregator(model string) *QoderNonStreamAggregator {
	return &QoderNonStreamAggregator{ID: "chatcmpl-" + uuid.NewString(), Model: model, Created: time.Now().Unix(), ToolCalls: make(map[int]*QoderToolCallAccumulator)}
}

func AggregateQoderNonStream(model string, frames []map[string]any) ([]byte, error) {
	agg := NewQoderNonStreamAggregator(model)
	for _, f := range frames {
		if _, err := agg.FeedDataFrame(f); err != nil {
			return nil, err
		}
	}
	return agg.BuildResponse()
}
