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
	require.Contains(t, buildWbChunkFrame("hi", "", false), `"content":"hi"`)
	require.NotContains(t, buildWbChunkFrame("hi", "", false), `"role"`)
	// 首帧带 role:"assistant"，后续帧只带内容（OpenAI 兼容契约）。
	require.Contains(t, buildWbChunkFrame("hi", "", true), `"role":"assistant"`)
	require.Contains(t, buildWbChunkFrame("", "reason", false), `"reasoning_content":"reason"`)
	require.True(t, strings.HasSuffix(frame, "\n\n"))
	require.Equal(t, "", buildWbChunkFrame("", "", true))
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
	// 同一事件里还带 A 平面调用（c_srv / c_defer_srv）⇒ 网关必须能识别出混合平面并显式拒绝。
	require.True(t, af.hasServerToolUse, "event carrying both planes must set hasServerToolUse")
	// 纯 B 平面事件不置标志，正常交接。
	bf := classifyWBLine(
		wbTestObject(t, `{"type":"assistant","message":{"id":"m2","content":[{"type":"tool_use","id":"c_only_cli","name":"mcp__wb__read","input":{"path":"b.go"}}]}}`),
		map[string]bool{"read": true},
	)
	require.Len(t, bf.toolCalls, 1)
	require.False(t, bf.hasServerToolUse)
	// 纯 A 平面事件既不透出调用，也不置混合标志。
	sf := classifyWBLine(
		wbTestObject(t, `{"type":"assistant","message":{"id":"m3","content":[{"type":"tool_use","id":"c_only_srv","name":"mcp__wb__echo","input":{"text":"x"}}]}}`),
		map[string]bool{"read": true},
	)
	require.Empty(t, sf.toolCalls)
	require.True(t, sf.hasServerToolUse)
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

// TestWBToolUseNameAndArgumentsNormalization 钉住 B 平面交接前的归一化：空白工具名
// 一律不当作可交接调用（否则 " " 这类名字会进到客户端）；参数分两条路径——`input` /
// `params` 是参数「值」，字符串就编成 JSON 字符串；只有 DeferExecuteTool 的
// `arguments` 是已编码参数串，要先证明是合法 JSON 再压缩。混用两条路径会把
// `params:"{}"` 这类字符串值悄悄重解释成对象，改变调用方收到的参数类型。
func TestWBToolUseNameAndArgumentsNormalization(t *testing.T) {
	for _, name := range []string{"", " ", "   "} {
		raw := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c1","name":"` + name + `","input":{}}]}}`
		f := classifyWBLine(wbTestObject(t, raw), map[string]bool{"read": true})
		require.Equal(t, wbLineAssistant, f.kind)
		require.Empty(t, f.toolCalls, "blank tool name %q must not be handed off", name)
	}

	// 直呼形态：input 是参数值，对象按对象编码。
	direct := classifyWBLine(wbTestObject(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c1","name":"mcp__wb__read","input":{"path":"a.go"}}]}}`), map[string]bool{"read": true})
	require.Len(t, direct.toolCalls, 1)
	require.Equal(t, `{"path":"a.go"}`, direct.toolCalls[0].Arguments)

	// 直呼形态的字符串 input 是字符串值，不得被当成已编码 JSON 重解释。
	directStr := classifyWBLine(wbTestObject(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c1b","name":"mcp__wb__read","input":" { \"path\" : \"a.go\" } "}]}}`), map[string]bool{"read": true})
	require.Len(t, directStr.toolCalls, 1)
	require.Equal(t, `" { \"path\" : \"a.go\" } "`, directStr.toolCalls[0].Arguments)

	// 包装形态：arguments 是已编码参数串，校验后压缩。
	valid := classifyWBLine(wbTestObject(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c2","name":"DeferExecuteTool","input":{"toolName":"mcp__wb__read","arguments":" { \"path\" : \"a.go\" } "}}]}}`), map[string]bool{"read": true})
	require.Len(t, valid.toolCalls, 1)
	require.Equal(t, `{"path":"a.go"}`, valid.toolCalls[0].Arguments)

	bad := classifyWBLine(wbTestObject(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c3","name":"DeferExecuteTool","input":{"toolName":"mcp__wb__read","arguments":"not-json"}}]}}`), map[string]bool{"read": true})
	require.Equal(t, wbLineError, bad.kind)
	require.Equal(t, wbErrCLIFailed, bad.code)

	// params 是值：null 与缺失都回退空对象；字符串原样编码成 JSON 字符串。
	missing := classifyWBLine(wbTestObject(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c4","name":"DeferExecuteTool","input":{"toolName":"mcp__wb__read","params":null}}]}}`), map[string]bool{"read": true})
	require.Len(t, missing.toolCalls, 1)
	require.Equal(t, "{}", missing.toolCalls[0].Arguments)

	strParams := classifyWBLine(wbTestObject(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c5","name":"DeferExecuteTool","input":{"toolName":"mcp__wb__read","params":"{}"}}]}}`), map[string]bool{"read": true})
	require.Len(t, strParams.toolCalls, 1)
	require.Equal(t, `"{}"`, strParams.toolCalls[0].Arguments)

	// 包装形态的空白主字段必须退回 tool_name，否则合法别名被空白串挡住、调用被静默丢弃。
	// 制表符用 JSON 转义 `\t` 写入：直接塞字面控制符进字符串是非法的 JSON。
	for _, blank := range []string{"", " ", `\t`} {
		raw := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c6","name":"DeferExecuteTool","input":{"toolName":"` + blank + `","tool_name":"mcp__wb__read","params":{"path":"a.go"}}}]}}`
		f := classifyWBLine(wbTestObject(t, raw), map[string]bool{"read": true})
		require.Len(t, f.toolCalls, 1, "blank toolName %q must fall back to tool_name", blank)
		require.Equal(t, "read", f.toolCalls[0].Name)
	}

	// 有效主字段优先：tool_name 存在也不覆盖非空 toolName。
	preferMain := classifyWBLine(wbTestObject(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"c7","name":"DeferExecuteTool","input":{"toolName":"mcp__wb__bash","tool_name":"mcp__wb__read","params":{}}}]}}`), map[string]bool{"read": true, "bash": true})
	require.Len(t, preferMain.toolCalls, 1)
	require.Equal(t, "bash", preferMain.toolCalls[0].Name)
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
