//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func wbTestObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &m))
	return m
}

func TestWBClassify(t *testing.T) {
	cases := []struct {
		raw  string
		kind wbLineKind
		text string
	}{
		{`{"type":"system","subtype":"init"}`, wbLineIgnore, ""},
		{`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m1"}}}`, wbLineMessageStart, ""},
		{`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}}`, wbLineTextDelta, "hi"},
		{`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"reason"}}}`, wbLineThinkingDelta, ""},
		{`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"echo"}]}}`, wbLineAssistant, "hi"},
		{`{"type":"result","subtype":"success","result":"hi","usage":{"input_tokens":10,"output_tokens":2}}`, wbLineCompletion, "hi"},
		{`{"type":"result","result":{"status":"success","result":"hi"}}`, wbLineCompletion, "hi"},
		{`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom"}`, wbLineError, ""},
		{`{"type":"result","status":"error","message":"boom"}`, wbLineError, ""},
		{`{"type":"error","message":"boom"}`, wbLineError, ""},
		{`{"error":"boom"}`, wbLineError, ""},
		{`{"error":{"message":"boom"}}`, wbLineError, ""},
		{`{"type":"result","subtype":"success"}`, wbLineIgnore, ""},
		{`{"type":"result","subtype":"unknown","result":"hi"}`, wbLineIgnore, ""},
	}
	for _, tc := range cases {
		f := classifyWBLine(wbTestObject(t, tc.raw))
		require.Equal(t, tc.kind, f.kind, tc.raw)
		require.Equal(t, tc.text, f.text, tc.raw)
	}
}

func TestWBMessageScope(t *testing.T) {
	t.Run("equal length different full-only messages without start", func(t *testing.T) {
		a := newWbStreamAccumulator()
		x, ok := a.OnAssistant("AAAA")
		require.True(t, ok)
		require.Equal(t, "AAAA", x)
		x, ok = a.OnAssistant("BBBB")
		require.True(t, ok)
		require.Equal(t, "BBBB", x)
		_, ok = a.OnAssistant("BBBB")
		require.False(t, ok)
		require.True(t, a.CheckFulltext("AAAABBBB"))
	})
	t.Run("partial prefix duplicate and tool round", func(t *testing.T) {
		a := newWbStreamAccumulator()
		a.StartMessage("m1")
		a.OnTextDelta("hello ")
		// Thinking-only or tool_use-only partial assistant frame (empty text) must not poison seen[m1].
		_, ok := a.Assistant("m1", "")
		require.False(t, ok)
		x, ok := a.Assistant("m1", "hello world")
		require.True(t, ok)
		require.Equal(t, "world", x)
		_, ok = a.Assistant("m1", "hello world")
		require.False(t, ok)
		a.StartMessage("m2")
		a.OnTextDelta(" answer")
		_, ok = a.Assistant("m2", " answer")
		require.False(t, ok)
		require.Equal(t, "hello world answer", a.RequestText())
		require.True(t, a.CheckFulltext("hello world answer"))
	})
	t.Run("distinct message IDs may carry equal text", func(t *testing.T) {
		a := newWbStreamAccumulator()
		a.Assistant("a", "same")
		a.Assistant("b", "same")
		a.Assistant("a", "same")
		require.Equal(t, "samesame", a.RequestText())
	})
	t.Run("nonprefix preserves emitted delta", func(t *testing.T) {
		a := newWbStreamAccumulator()
		a.OnTextDelta("hello ")
		_, ok := a.OnAssistant("world")
		require.False(t, ok)
		require.Equal(t, "hello ", a.RequestText())
		require.False(t, a.CheckFulltext("hello world"))
	})
}

func TestWBFramesAndUsage(t *testing.T) {
	f := classifyWBLine(wbTestObject(t, `{"type":"result","subtype":"success","result":"hi","usage":{"input_tokens":10,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4}}`))
	require.Equal(t, wbUsage{10, 2, 3, 4, true}, f.usage)
	frame := buildWbTerminalFrame(f.usage)
	require.Contains(t, frame, `"finish_reason":"stop"`)
	require.Contains(t, frame, `"total_tokens":12`)
	require.Contains(t, buildWbChunkFrame("hi", ""), `"content":"hi"`)
	require.Contains(t, buildWbChunkFrame("", "reason"), `"reasoning_content":"reason"`)
	require.True(t, strings.HasSuffix(frame, "\n\n"))
	require.Equal(t, "", buildWbChunkFrame("", ""))
	require.Contains(t, buildWbTerminalFrame(wbUsage{}), `"prompt_tokens":0`)
	require.Equal(t, "data: [DONE]\n\n", wbDoneFrame)

	af := classifyWBLine(
		wbTestObject(t, `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":5,"output_tokens":3},"content":[{"type":"tool_use","id":"c_meta","name":"ToolSearch","input":{"tool_names":["mcp__wb__read"]}},{"type":"tool_use","id":"c_other_ns","name":"mcp__other__read","input":{}},{"type":"tool_use","id":"c_undeclared","name":"mcp__wb__bash","input":{}},{"type":"tool_use","id":"c_srv","name":"mcp__wb__echo","input":{"text":"x"}},{"type":"tool_use","id":"c_defer_srv","name":"DeferExecuteTool","input":{"toolName":"mcp__wb__echo","params":{"text":"x"}}},{"type":"tool_use","id":"c_defer_undecl","name":"DeferExecuteTool","input":{"toolName":"mcp__wb__bash","params":{}}},{"type":"tool_use","id":"c_cli","name":"mcp__wb__read","input":{"path":"a.go"}},{"type":"tool_use","id":"c_defer_cli","name":"DeferExecuteTool","input":{"toolName":"mcp__wb__get_local_weather","params":{"city":"Shenzhen"}}}]}}`),
		map[string]bool{"read": true, "get_local_weather": true},
	)
	require.Equal(t, wbLineAssistant, af.kind)
	require.Equal(t, wbUsage{5, 3, 0, 0, false}, af.usage)
	require.Len(t, af.toolCalls, 2)
	require.Equal(t, WBToolCall{ID: "c_cli", Name: "read", Arguments: `{"path":"a.go"}`}, af.toolCalls[0])
	require.Equal(t, WBToolCall{ID: "c_defer_cli", Name: "get_local_weather", Arguments: `{"city":"Shenzhen"}`}, af.toolCalls[1])
	acc := newWbStreamAccumulator()
	acc.AddClientToolCalls(af.toolCalls)
	acc.AddClientToolCalls(af.toolCalls)
	require.Len(t, acc.ClientToolCalls(), 2)
	require.Contains(t, buildWbToolCallsFrame(af.toolCalls), `"name":"read"`)
	require.Contains(t, buildWbToolCallsFrame(af.toolCalls), `"name":"get_local_weather"`)
	require.Contains(t, buildWbToolCallsTerminalFrame(af.usage), `"finish_reason":"tool_calls"`)

	// Missing ID fallback avoids colliding with real IDs (e.g. call_2) and preserves distinct anonymous calls.
	accCollision := newWbStreamAccumulator()
	batch := []WBToolCall{
		{ID: "call_2", Name: "read", Arguments: `{"path":"a"}`},
		{ID: "", Name: "write", Arguments: `{"path":"b"}`},
		{ID: "", Name: "edit", Arguments: `{"path":"c"}`},
	}
	accCollision.AddClientToolCalls(batch)
	accCollision.AddClientToolCalls(batch) // idempotent on duplicate assistant event
	gotCalls := accCollision.ClientToolCalls()
	require.Len(t, gotCalls, 3)
	require.Equal(t, "call_2", gotCalls[0].ID)
	require.Equal(t, "call_1", gotCalls[1].ID)
	require.Equal(t, "call_3", gotCalls[2].ID)
}

func TestWBConsumeCLI_FulltextMismatchRetainsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	// 构造包含流式 delta 与完成帧的输出，但完成帧的全文与流式 text 不一致
	streamLines := []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"streamed text"}}}`,
		`{"type":"result","subtype":"success","result":"mismatched completion text","usage":{"input_tokens":120,"output_tokens":45,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}`,
	}
	var stdout bytes.Buffer
	for _, line := range streamLines {
		stdout.WriteString(line + "\n")
	}

	cmd := exec.Command("true")
	process := &wbCLIProcess{
		cmd:    cmd,
		stdout: io.NopCloser(&stdout),
		stderr: &wbStderrBuffer{},
	}

	req := wbInbound{Model: "claude-sonnet-4.5", Stream: false}
	started := time.Now()
	res, err := wbConsumeCLI(context.Background(), c, process, req, "test-token", started)
	// 1. 验证错误语义：返回 502 全文不一致错误
	require.Error(t, err)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), wbErrFulltextMismatch)

	// 2. 验证用量保留：生产路径保留并返回完成帧中的 Usage 及模型信息，供 submitChatUsage 入账
	require.NotNil(t, res)
	require.Equal(t, "claude-sonnet-4.5", res.Model)
	require.Equal(t, 120, res.Usage.InputTokens)
	require.Equal(t, 45, res.Usage.OutputTokens)
	require.Equal(t, 10, res.Usage.CacheCreationInputTokens)
	require.Equal(t, 5, res.Usage.CacheReadInputTokens)
}
