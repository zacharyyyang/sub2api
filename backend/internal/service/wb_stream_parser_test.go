//go:build unit

package service

import (
	"encoding/json"
	"strings"
	"testing"

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
}
