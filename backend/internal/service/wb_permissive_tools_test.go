//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// wbUnregisteredToolSet 生成 n 个未注册工具声明（真名 + 编号补齐 ≥60，不含 echo）。
func wbUnregisteredToolSet(n int) json.RawMessage {
	real := []string{"read", "write", "bash", "edit", "apply_patch", "glob", "grep", "ls", "tree", "execute", "lint", "code_search", "doc_search", "repo_outline", "memory", "fetch", "websearch"}
	decls := make([]any, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("client_tool_%d", i)
		if i < len(real) {
			name = real[i]
		}
		decls = append(decls, map[string]any{"type": "function", "function": map[string]any{"name": name, "parameters": map[string]any{"type": "object"}}})
	}
	raw, _ := json.Marshal(decls)
	return raw
}

// wbWithEcho 在未注册工具集前拼接一条合法 echo 声明（混合输入用；rest 须为非空数组）。
func wbWithEcho(rest json.RawMessage) json.RawMessage {
	echo := `{"type":"function","function":{"name":"echo","description":"Echo text","parameters":{"type":"object","properties":{}}}}`
	return json.RawMessage(`[` + echo + `,` + strings.TrimSuffix(strings.TrimPrefix(string(rest), "["), "]") + `]`)
}

type wbPermissiveTokenSource struct{}

func (wbPermissiveTokenSource) GetAccessToken(context.Context, *Account) (string, error) {
	return "__FAKE__:argv-sse", nil
}
func (wbPermissiveTokenSource) InvalidateAccountCache(int64) {}

// TestWBPermissiveMap：AC-1/2/5 —— 客户端自定义 function 工具与服务端注册工具双平面映射：
// 未注册合法 function 工具映射为 ServerExecuted:false（缺省 parameters 自动补齐默认 object schema），
// 注册工具 echo 保持 ServerExecuted:true 与 wbStrictJSON 严格校验，非 function 或无名条目静默剔除。
func TestWBPermissiveMap(t *testing.T) {
	cases := []struct {
		name     string
		raw      json.RawMessage
		wantN    int
		wantName string
		wantSrv  bool
		wantErr  string
	}{
		{"1 all unregistered 60+ mapped as client tools", wbUnregisteredToolSet(60), 60, "read", false, ""},
		{"2 mixed echo+unregistered", wbWithEcho(wbUnregisteredToolSet(60)), 61, "echo", true, ""},
		{"3 echo unchanged", json.RawMessage(`[{"type":"function","function":{"name":"echo","description":"Echo text","parameters":{"type":"object","properties":{}}}}]`), 1, "echo", true, ""},
		{"4 empty array", json.RawMessage(`[]`), 0, "", false, ""},
		{"5 unregistered strict:false mapped", json.RawMessage(`[{"type":"function","function":{"name":"read","parameters":{"type":"object"},"strict":false}}]`), 1, "read", false, ""},
		{"5b unregistered missing parameters gets default object schema", json.RawMessage(`[{"type":"function","function":{"name":"get_current_time"}}]`), 1, "get_current_time", false, ""},
		{"6 unregistered non-function type dropped", json.RawMessage(`[{"type":"file_search","name":"search"}]`), 0, "", false, ""},
		{"11b name missing/empty/non-string dropped", json.RawMessage(`[{"type":"function","function":{}},{"type":"function","function":{"name":""}},{"type":"function","function":{"name":123}},{"function":"echo"}]`), 0, "", false, ""},
		{"7 echo extra_field strict", json.RawMessage(`[{"type":"function","function":{"name":"echo","description":"E","parameters":{"type":"object","properties":{}},"extra_field":1}}]`), 0, "", false, "invalid tool declaration"},
		{"echo non-function type", json.RawMessage(`[{"type":"builtin","function":{"name":"echo","parameters":{"type":"object"}}}]`), 0, "", false, `unsupported tool type "builtin"`},
		{"8 echo duplicate", json.RawMessage(`[{"type":"function","function":{"name":"echo","parameters":{"type":"object"}}},{"type":"function","function":{"name":"echo","parameters":{"type":"object"}}}]`), 0, "", false, "duplicate tool"},
		{"8b client tool duplicate", json.RawMessage(`[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}},{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}]`), 0, "", false, "duplicate tool"},
		{"9 echo missing schema", json.RawMessage(`[{"type":"function","function":{"name":"echo","description":"E"}}]`), 0, "", false, "requires an object input schema"},
		{"11a null entry", json.RawMessage(`[null]`), 0, "", false, "invalid tool declaration"},
		{"11 non-object: string", json.RawMessage(`["s"]`), 0, "", false, "invalid tool declaration"},
		{"11 non-object: number", json.RawMessage(`[123]`), 0, "", false, "invalid tool declaration"},
		{"11 non-object: bool", json.RawMessage(`[true]`), 0, "", false, "invalid tool declaration"},
		{"11 non-object: array el", json.RawMessage(`[[1]]`), 0, "", false, "invalid tool declaration"},
		{"10 tools non-array or null", json.RawMessage(`{}`), 0, "", false, "tools must be an array"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools, err := wbMapTools(tc.raw)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, tools, tc.wantN)
			if tc.wantName != "" {
				require.Equal(t, tc.wantName, tools[0].Name)
				require.Equal(t, tc.wantSrv, tools[0].ServerExecuted)
				require.Equal(t, "object", tools[0].InputSchema["type"])
			}
		})
	}
}

// TestWBPermissiveConfigAndSpawn：AC-2/AC-3 —— 混合工具均映射进 MCP 配置与 --allowedTools（含 --tools ToolSearch,DeferExecuteTool）；
// 空工具集 → 空配置 → spawn argv 传 --tools "" 且不含 --mcp-config / --allowedTools / --permission-mode。
func TestWBPermissiveConfigAndSpawn(t *testing.T) {
	tools, err := wbMapTools(wbWithEcho(wbUnregisteredToolSet(60)))
	require.NoError(t, err)
	require.Len(t, tools, 61)
	cfg, err := wbBuildToolConfig(tools, wbFakeCLI(t))
	require.NoError(t, err)
	defer cfg.Close()
	require.Contains(t, cfg.AllowedTools, "mcp__wb__echo")
	require.Contains(t, cfg.AllowedTools, "mcp__wb__read")
	data, err := os.ReadFile(cfg.Path)
	require.NoError(t, err)
	var conf map[string]any
	require.NoError(t, json.Unmarshal(data, &conf))
	server := conf["mcpServers"].(map[string]any)["wb"].(map[string]any)
	args := server["args"].([]any)
	var decls []WBBridgeTool
	require.NoError(t, json.Unmarshal([]byte(args[1].(string)), &decls))
	require.Len(t, decls, 61)
	require.Equal(t, "echo", decls[0].Name)
	require.True(t, decls[0].ServerExecuted)
	require.Equal(t, "read", decls[1].Name)
	require.False(t, decls[1].ServerExecuted)

	empty, err := wbBuildToolConfig([]WBBridgeTool{}, "")
	require.NoError(t, err)
	require.Empty(t, empty.Path)
	p, err := spawnWBCLIProcess(wbSpawnConfig{CliPath: wbFakeCLI(t), Token: "__FAKE__:argv-sse",
		Model: "glm-5.3-flash", Prompt: "User: hi"})
	require.NoError(t, err)
	t.Cleanup(func() { p.Kill(); _ = p.Wait() })
	require.NoError(t, p.WritePrompt())
	out, err := io.ReadAll(p.stdout)
	require.NoError(t, err)
	require.NoError(t, p.Wait())
	argv := strings.SplitN(string(out), "\n", 2)[0]
	require.NotContains(t, argv, "--mcp-config")
	require.NotContains(t, argv, "--allowedTools")
	require.NotContains(t, argv, "--permission-mode")
	require.Contains(t, argv, `"--tools" ""`)
}

// TestWBParseInboundPermissive：AC-1 —— 合法 body + 60+ 未注册 function tools 无错误、全部映射为客户端工具。
func TestWBParseInboundPermissive(t *testing.T) {
	body := []byte(`{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":` + string(wbUnregisteredToolSet(60)) + `}`)
	req, prompt, tools, err := wbParseInbound(body)
	require.NoError(t, err)
	require.True(t, req.Stream)
	require.Equal(t, "User: hi", prompt)
	require.Len(t, tools, 60)
	require.False(t, tools[0].ServerExecuted)
}

type wbClientToolUseTokenSource struct{}

func (wbClientToolUseTokenSource) GetAccessToken(context.Context, *Account) (string, error) {
	return "__FAKE__:client-tool-use", nil
}
func (wbClientToolUseTokenSource) InvalidateAccountCache(int64) {}

// TestWBForwardPermissive：AC-3/AC-4 —— 网关层端到端：
// (1) 仅含非 function 工具时映射为空，CLI 无 MCP 配置正常返回 200 流式文本；
// (2) 携带客户端 function 工具且 CLI 产出 mcp__wb__read tool_use 时，网关拦截并终止子进程，
// 流式与非流式均返回标准 OpenAI tool_calls 且 finish_reason="tool_calls"。
func TestWBForwardPermissive(t *testing.T) {
	t.Run("non-function tools dropped and CLI runs with empty tools", func(t *testing.T) {
		body := []byte(`{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"file_search","name":"search"}]}`)
		account := &Account{Credentials: map[string]any{"cli_path": wbFakeCLI(t)}}
		s := &OpenAIGatewayService{wbTokenProvider: wbPermissiveTokenSource{}}
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		result, err := s.forwardWBChatCompletions(context.Background(), c, account, body)
		require.NoError(t, err)
		require.Equal(t, 200, recorder.Code)
		require.Contains(t, recorder.Body.String(), `"content":"hello "`)
		require.Contains(t, recorder.Body.String(), `"content":"world"`)
		require.Contains(t, recorder.Body.String(), "[DONE]")
		require.Equal(t, 10, result.Usage.InputTokens)
	})

	t.Run("client tool_use intercepted as streaming tool_calls", func(t *testing.T) {
		oldGrace, oldHook := wbCLIKillGrace, wbSignalProcessHook
		wbCLIKillGrace = 50 * time.Millisecond
		var mu sync.Mutex
		var killSignals []bool
		var killedCmd *exec.Cmd
		wbSignalProcessHook = func(cmd *exec.Cmd, kill bool) error {
			mu.Lock()
			killSignals = append(killSignals, kill)
			if cmd != nil {
				killedCmd = cmd
			}
			mu.Unlock()
			return wbSignalProcess(cmd, kill)
		}
		t.Cleanup(func() {
			wbCLIKillGrace = oldGrace
			wbSignalProcessHook = oldHook
		})
		t.Setenv("WB_MCP_BRIDGE_PATH", wbFakeCLI(t))

		body := []byte(`{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"read README"}],"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"},"id_num":{"type":"integer"}}}}}]}`)
		account := &Account{Credentials: map[string]any{"cli_path": wbFakeCLI(t)}}
		s := &OpenAIGatewayService{wbTokenProvider: wbClientToolUseTokenSource{}}
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		result, err := s.forwardWBChatCompletions(context.Background(), c, account, body)
		require.NoError(t, err)
		require.Equal(t, 15, result.Usage.InputTokens)
		require.Equal(t, 6, result.Usage.OutputTokens)
		mu.Lock()
		require.Contains(t, killSignals, false, "expected soft kill signal upon client tool_use")
		require.Contains(t, killSignals, true, "expected forced tree kill upon client tool_use")
		require.NotNil(t, killedCmd)
		require.Contains(t, killedCmd.Args, "--mcp-config")
		require.Contains(t, killedCmd.Args, "mcp__wb__read")
		require.NotNil(t, killedCmd.ProcessState, "process must have exited after Kill()+Wait()")
		require.False(t, killedCmd.ProcessState.Success(), "process must exit non-zero due to Kill(), not natural os.Exit(0)")
		mu.Unlock()
		out := recorder.Body.String()
		require.Contains(t, out, `"content":"Let me read the file."`)
		require.Contains(t, out, "data: [DONE]\n\n")

		var foundToolCallChunk, foundTerminalChunk bool
		for _, line := range strings.Split(out, "\n") {
			if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
				continue
			}
			var chunk map[string]any
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk))
			choice := chunk["choices"].([]any)[0].(map[string]any)
			delta := choice["delta"].(map[string]any)
			if rawTCs, ok := delta["tool_calls"].([]any); ok && len(rawTCs) == 1 {
				foundToolCallChunk = true
				tc := rawTCs[0].(map[string]any)
				require.Equal(t, float64(0), tc["index"])
				require.Equal(t, "call_read_1", tc["id"])
				require.Equal(t, "function", tc["type"])
				fn := tc["function"].(map[string]any)
				require.Equal(t, "read", fn["name"])
				wbRequireToolArgs(t, fn["arguments"].(string))
			}
			if choice["finish_reason"] == "tool_calls" {
				foundTerminalChunk = true
			}
		}
		require.True(t, foundToolCallChunk, "expected SSE chunk with tool_calls delta")
		require.True(t, foundTerminalChunk, "expected SSE chunk with finish_reason=tool_calls")
	})

	t.Run("client tool_use intercepted as non-streaming tool_calls", func(t *testing.T) {
		oldGrace, oldHook := wbCLIKillGrace, wbSignalProcessHook
		wbCLIKillGrace = 50 * time.Millisecond
		var mu sync.Mutex
		var killSignals []bool
		var killedCmd *exec.Cmd
		wbSignalProcessHook = func(cmd *exec.Cmd, kill bool) error {
			mu.Lock()
			killSignals = append(killSignals, kill)
			if cmd != nil {
				killedCmd = cmd
			}
			mu.Unlock()
			return wbSignalProcess(cmd, kill)
		}
		t.Cleanup(func() {
			wbCLIKillGrace = oldGrace
			wbSignalProcessHook = oldHook
		})
		t.Setenv("WB_MCP_BRIDGE_PATH", wbFakeCLI(t))

		body := []byte(`{"model":"glm-5.3-flash","stream":false,"messages":[{"role":"user","content":"read README"}],"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"},"id_num":{"type":"integer"}}}}}]}`)
		account := &Account{Credentials: map[string]any{"cli_path": wbFakeCLI(t)}}
		s := &OpenAIGatewayService{wbTokenProvider: wbClientToolUseTokenSource{}}
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		result, err := s.forwardWBChatCompletions(context.Background(), c, account, body)
		require.NoError(t, err)
		require.Equal(t, 15, result.Usage.InputTokens)
		mu.Lock()
		require.Contains(t, killSignals, false, "expected soft kill signal upon client tool_use")
		require.Contains(t, killSignals, true, "expected forced tree kill upon client tool_use")
		require.NotNil(t, killedCmd)
		require.Contains(t, killedCmd.Args, "--mcp-config")
		require.Contains(t, killedCmd.Args, "mcp__wb__read")
		require.NotNil(t, killedCmd.ProcessState, "process must have exited after Kill()+Wait()")
		require.False(t, killedCmd.ProcessState.Success(), "process must exit non-zero due to Kill(), not natural os.Exit(0)")
		mu.Unlock()
		var resp map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
		choice := resp["choices"].([]any)[0].(map[string]any)
		require.Equal(t, "tool_calls", choice["finish_reason"])
		msg := choice["message"].(map[string]any)
		require.Equal(t, "Let me read the file.", msg["content"])
		tcs := msg["tool_calls"].([]any)
		require.Len(t, tcs, 1)
		tc := tcs[0].(map[string]any)
		require.Equal(t, "call_read_1", tc["id"])
		fn := tc["function"].(map[string]any)
		require.Equal(t, "read", fn["name"])
		wbRequireToolArgs(t, fn["arguments"].(string))
	})

	t.Run("round 2 trailing tool result synthesizes native jsonl session with --resume and retains all 5 tools", func(t *testing.T) {
		cfgDir := t.TempDir()
		t.Setenv("CODEBUDDY_CONFIG_DIR", cfgDir)
		t.Setenv("WB_MCP_BRIDGE_PATH", wbFakeCLI(t))
		body := []byte(`{"model":"glm-5.3-flash","stream":false,"messages":[{"role":"user","content":"read README and echo ping"},{"role":"assistant","content":"Let me run both tools.","tool_calls":[{"id":"call_read_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"README.md\"}"}},{"id":"call_echo_1","type":"function","function":{"name":"echo","arguments":"{\"text\":\"ping\"}"}}]},{"role":"tool","tool_call_id":"call_read_1","content":"# Sub2API"},{"role":"tool","tool_call_id":"call_echo_1","content":"ECHO: ping"}],"tools":[{"type":"function","function":{"name":"echo","parameters":{"type":"object","properties":{"text":{"type":"string"}}}}},{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}},{"type":"function","function":{"name":"write","parameters":{"type":"object"}}},{"type":"function","function":{"name":"bash","parameters":{"type":"object"}}},{"type":"function","function":{"name":"glob","parameters":{"type":"object"}}}]}`)
		account := &Account{Credentials: map[string]any{"cli_path": wbFakeCLI(t)}}
		s := &OpenAIGatewayService{wbTokenProvider: wbRound2ToolResultTokenSource{}}
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		result, err := s.forwardWBChatCompletions(context.Background(), c, account, body)
		require.NoError(t, err)
		require.Equal(t, 200, recorder.Code)
		require.Equal(t, 20, result.Usage.InputTokens)
		require.Equal(t, 5, result.Usage.OutputTokens)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
		choice := resp["choices"].([]any)[0].(map[string]any)
		require.Equal(t, "stop", choice["finish_reason"])
		msg := choice["message"].(map[string]any)
		require.Equal(t, "README Title: Sub2API", msg["content"])

		matches, globErr := filepath.Glob(filepath.Join(cfgDir, "projects", "*", "*.jsonl"))
		require.NoError(t, globErr)
		require.Empty(t, matches, "temporary session .jsonl file must be removed after round 2 forward completes")
	})

	t.Run("inbound message content array of text parts is concatenated while audio part is rejected", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":[{"type":"text","text":"You are "},{"type":"input_text","text":"helpful."}]},{"role":"user","content":[{"type":"text","text":"Hello "},{"type":"output_text","text":"world"}]}]}`)
		req, prompt, _, err := wbParseInbound(body)
		require.NoError(t, err)
		require.Len(t, req.Messages, 2)
		require.NotNil(t, req.Messages[0].Content)
		require.Equal(t, "You are helpful.", *req.Messages[0].Content)
		require.NotNil(t, req.Messages[1].Content)
		require.Equal(t, "Hello world", *req.Messages[1].Content)
		require.Contains(t, prompt, "You are helpful.")
		require.Contains(t, prompt, "Hello world")

		badBody := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA"}}]}]}`)
		_, _, _, badErr := wbParseInbound(badBody)
		require.Error(t, badErr)
		require.Contains(t, badErr.Error(), "unsupported message content part")
	})
}

type wbRound2ToolResultTokenSource struct{}

func (wbRound2ToolResultTokenSource) GetAccessToken(context.Context, *Account) (string, error) {
	return "__FAKE__:round2-tool-result", nil
}
func (wbRound2ToolResultTokenSource) InvalidateAccountCache(int64) {}

func wbRequireToolArgs(t *testing.T, raw string) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var parsed map[string]any
	require.NoError(t, dec.Decode(&parsed))
	require.Equal(t, "README.md", parsed["path"])
	require.Equal(t, json.Number("9007199254740993"), parsed["id_num"])
}
