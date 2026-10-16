package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestQoderBuildV3RequestAndTemperatureAndTools 覆盖设计 §6 用例 2, 3, 4, 5：模型归一（含大小写混合与未知模型拒绝）、Kimi-K3 默认温度 1、tools strict 剔除与多轮 tool_calls。
func TestQoderBuildV3RequestAndTemperatureAndTools(t *testing.T) {
	creds := &QoderCredentials{UID: "u-100", OrgID: "o-200"}

	// 用例 2：大小写混合别名与原生 ID
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"SONUS-FABLE5.1", "smodel"},
		{"Qwen3.8-Max", "qmodel_38max"},
		{"smodel", "smodel"},
		{"auto", "auto"},
	} {
		req, _ := json.Marshal(map[string]any{"model": tc.in, "messages": []map[string]any{{"role": "user", "content": "hi"}}})
		_, nat, _, err := BuildQoderV3Request(req, creds)
		if err != nil || nat != tc.want {
			t.Fatalf("BuildQoderV3Request(%s) = %s, %v; want %s", tc.in, nat, err, tc.want)
		}
	}

	// 用例 3：未知模型拒绝
	badReq, _ := json.Marshal(map[string]any{"model": "gpt-99-unknown", "messages": []map[string]any{{"role": "user", "content": "hi"}}})
	if _, _, _, err := BuildQoderV3Request(badReq, creds); err == nil || !strings.Contains(err.Error(), "unsupported model") {
		t.Fatalf("expected unsupported model error for gpt-99-unknown, got %v", err)
	}
}

func TestQoderBuildV3KimiTemperatureAndToolStrict(t *testing.T) {
	creds := &QoderCredentials{UID: "u-100", OrgID: "o-200"}
	// 用例 4 & 5：Kimi-K3 默认温度 1、strict 剔除、assistant nil content 转 ""
	openAIReq, _ := json.Marshal(map[string]any{
		"model":  "Kimi-K3",
		"stream": false,
		"messages": []map[string]any{
			{"role": "user", "content": "read file"},
			{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []map[string]any{
					{"id": "call_1", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"path":"a.go"}`}},
				},
			},
			{"role": "tool", "tool_call_id": "call_1", "content": "package main"},
		},
		"tools": []map[string]any{
			{
				"type":   "function",
				"strict": true,
				"function": map[string]any{
					"name":   "read",
					"strict": true,
					"parameters": map[string]any{
						"type": "object",
					},
				},
			},
		},
	})
	v3Bytes, nativeModel, isStream, err := BuildQoderV3Request(openAIReq, creds)
	if err != nil {
		t.Fatalf("BuildQoderV3Request failed: %v", err)
	}
	if nativeModel != "kmodel_latest" || isStream {
		t.Fatalf("unexpected nativeModel=%s isStream=%v", nativeModel, isStream)
	}
	if strings.Contains(string(v3Bytes), `"strict"`) {
		t.Fatalf("expected strict field recursively stripped: %s", string(v3Bytes))
	}
	var parsed map[string]any
	if err := json.Unmarshal(v3Bytes, &parsed); err != nil {
		t.Fatalf("unmarshal v3Body: %v", err)
	}
	if parsed["version"] != "3" || parsed["chat_task"] != "FREE_INPUT" || parsed["temperature"] != float64(1.0) {
		t.Fatalf("expected version=3, chat_task=FREE_INPUT, temperature=1.0, got %v", parsed)
	}
	if parsed["user_id"] != "u-100" || parsed["organization_id"] != "o-200" || parsed["tool_choice"] != "auto" {
		t.Fatalf("expected user_id, organization_id and tool_choice=auto injected, got %v", parsed)
	}
	msgs, _ := parsed["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (user, assistant, tool), got %d", len(msgs))
	}
	assistantMsg, _ := msgs[1].(map[string]any)
	if assistantMsg["content"] != "" {
		t.Fatalf("expected assistant nil content converted to empty string, got %v", assistantMsg["content"])
	}
}

// TestQoderParseOuterFrameAndQueue10605 覆盖设计 §6 用例 5, 10：双层嵌套 10605 排队帧与扁平错误 provider_error/details 状态码候选链。
func TestQoderParseOuterFrameAndQueue10605(t *testing.T) {
	// 外层 403 内嵌双层 JSON 字符串 code="10605"，message="{\"retryAfterSeconds\":1}" -> 夹紧为 2s, Queue=true, Status=429
	innerBody, _ := json.Marshal(map[string]any{
		"code":    "10605",
		"message": `{"retryAfterSeconds":1}`,
	})
	outerQueue, _ := json.Marshal(map[string]any{
		"statusCodeValue": 403,
		"body":            string(innerBody),
	})
	res, err := ParseQoderOuterFrame(outerQueue)
	if err != nil {
		t.Fatalf("ParseQoderOuterFrame failed: %v", err)
	}
	if !res.Queue || res.Status != 429 || res.RetryAfter != 2*time.Second {
		t.Fatalf("expected Queue=true, Status=429, RetryAfter=2s, got %+v", res)
	}

	// 用例 10：外层 statusCodeValue=200，内层扁平错误 {"code":"provider_error","details":"{\"error\":{\"status\":429,\"message\":\"rate limit\"}}"}
	flatBody, _ := json.Marshal(map[string]any{
		"code":    "provider_error",
		"details": `{"error":{"status":429,"message":"rate limit"}}`,
	})
	outerFlat, _ := json.Marshal(map[string]any{
		"statusCodeValue": 200,
		"body":            string(flatBody),
	})
	resFlat, err := ParseQoderOuterFrame(outerFlat)
	if err != nil {
		t.Fatalf("ParseQoderOuterFrame flat error failed: %v", err)
	}
	if !resFlat.FatalError || resFlat.Status != 429 || resFlat.Message != "rate limit" {
		t.Fatalf("expected FatalError=true, Status=429, Message='rate limit', got %+v", resFlat)
	}

	// invalid temperature -> 400
	tempFrame, _ := json.Marshal(map[string]any{
		"statusCodeValue": 200,
		"body":            `{"code":1001,"message":"Invalid temperature for model"}`,
	})
	resTemp, err := ParseQoderOuterFrame(tempFrame)
	if err != nil || !resTemp.FatalError || resTemp.Status != 400 {
		t.Fatalf("expected Status=400 for invalid temperature, got %+v, err=%v", resTemp, err)
	}
}

// TestQoderNonStreamAggregation 覆盖设计 §6 用例 7：多帧 content、reasoning_content 与分段 tool_calls 聚合。
func TestQoderNonStreamAggregation(t *testing.T) {
	frames := []map[string]any{
		{"id": "cmpl-01", "delta": map[string]any{"reasoning_content": "think-1 ", "content": "Hello "}},
		{"id": "cmpl-01", "delta": map[string]any{"content": "\n"}},
		{"id": "cmpl-01", "delta": map[string]any{"reasoning_content": "think-2", "content": "Qoder!"}},
		{
			"id": "cmpl-01",
			"tool_calls": []any{
				map[string]any{"index": 2, "id": "tc_1", "function": map[string]any{"name": "ls", "arguments": `{"pa`}},
			},
		},
		{
			"id": "cmpl-01",
			"tool_calls": []any{
				map[string]any{"index": 2, "function": map[string]any{"arguments": `th":"."}`}},
			},
			"finish_reason": "tool_calls",
			"usage":         map[string]any{"prompt_tokens": 12, "completion_tokens": 8},
		},
	}
	respBytes, err := AggregateQoderNonStream("sonus-fable5.1", frames)
	if err != nil {
		t.Fatalf("AggregateQoderNonStream failed: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		t.Fatalf("unmarshal aggregated response: %v", err)
	}
	choices, _ := resp["choices"].([]any)
	ch0, _ := choices[0].(map[string]any)
	msg, _ := ch0["message"].(map[string]any)
	if msg["content"] != "Hello \nQoder!" || msg["reasoning_content"] != "think-1 think-2" {
		t.Fatalf("unexpected aggregated content=%q reasoning_content=%q", msg["content"], msg["reasoning_content"])
	}
	tcs, _ := msg["tool_calls"].([]any)
	tc0, _ := tcs[0].(map[string]any)
	fn, _ := tc0["function"].(map[string]any)
	if tc0["id"] != "tc_1" || fn["name"] != "ls" || fn["arguments"] != `{"path":"."}` {
		t.Fatalf("unexpected aggregated tool_call: %v", tc0)
	}
}
