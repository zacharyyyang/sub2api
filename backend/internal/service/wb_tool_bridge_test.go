//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestWBInbound(t *testing.T) {
	req, prompt, _, err := wbParseInbound([]byte(`{"model":"glm-5.3-flash","messages":[{"role":"system","content":"s"},{"role":"user","content":"u"},{"role":"assistant","content":"a"},{"role":"user","content":"v"}]}`))
	require.NoError(t, err)
	require.False(t, req.Stream)
	require.Nil(t, req.StreamOptions)
	require.Equal(t, "System: s\n\nUser: u\n\nAssistant: a\n\nUser: v", prompt)
	// 解码器级拒绝：语法畸形 / 尾部多余 JSON / 字段类型不匹配。
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"trailing JSON", `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}{"trailing":true}`, "unexpected trailing JSON"},
		{"malformed JSON", `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}`, ""},
		{"non-string content", `{"model":"glm-5.3-flash","messages":[{"role":"user","content":[{"type":"image_url"}]}]}`, ""},
		// 业务校验级拒绝：硬约束全部保留。
		{"unsupported model", `{"model":"unsupported-model","messages":[{"role":"user","content":"hi"}]}`, "unsupported model"},
		{"empty messages", `{"model":"glm-5.3-flash","messages":[]}`, "messages must not be empty"},
		{"unsupported role", `{"model":"glm-5.3-flash","messages":[{"role":"developer","content":"x"}]}`, `unsupported role "developer"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := wbParseInbound([]byte(tc.raw))
			require.Error(t, err)
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

// TestWBInboundCompat：AC-1/AC-2 —— 入站放宽未知字段阻断后，标准 OpenAI
// 客户端默认携带的参数（stream_options、temperature、user、消息级 name）
// 与显式 stream_options:null 均被接受，不再触发 400。
func TestWBInboundCompat(t *testing.T) {
	req, _, _, err := wbParseInbound([]byte(`{"model":"glm-5.3-flash","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	require.True(t, req.Stream)
	require.NotNil(t, req.StreamOptions)
	require.True(t, req.StreamOptions.IncludeUsage)
	for _, raw := range []string{
		`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"user":"client-123"}`,
		`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"x","name":"n"}]}`,
		`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"stream_options":null}`,
	} {
		req, _, _, err := wbParseInbound([]byte(raw))
		require.NoError(t, err)
		require.Nil(t, req.StreamOptions)
	}
}

// TestWBInboundToolHistory：设计档 §6 用例表（16 组）+ 5 组补强（17-21：
// assistant content 空串判定、system/user content 缺失与显式 null 的
// fail-closed 边界）—— 工具历史（role:tool / function）与 assistant
// content:null（摘要 / 空串兜底）折叠为确定性文本、不再 400；未变分支（未知
// 角色、system/user content 缺失、解码层拒绝）保持原行为。
func TestWBInboundToolHistory(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		want        string // 期望折叠 prompt（errContains 为空时）
		errContains string // 期望错误片段（非空时 = 期望 400）
	}{
		// 1 纯文本四段（既有断言回归）
		{"1 纯文本四段", `{"model":"glm-5.3-flash","messages":[{"role":"system","content":"s"},{"role":"user","content":"u"},{"role":"assistant","content":"a"},{"role":"user","content":"v"}]}`, "System: s\n\nUser: u\n\nAssistant: a\n\nUser: v", ""},
		// 2 tool 带 tool_call_id
		{"2 tool 带 tool_call_id", `{"model":"glm-5.3-flash","messages":[{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`, "Tool[call_1]: ok", ""},
		// 3 function 带 name（legacy）
		{"3 function 带 name", `{"model":"glm-5.3-flash","messages":[{"role":"function","name":"get_weather","content":"sunny"}]}`, "Tool[get_weather]: sunny", ""},
		// 4 tool 无 id 无 name
		{"4 tool 无 id 无 name", `{"model":"glm-5.3-flash","messages":[{"role":"tool","content":"bare"}]}`, "Tool: bare", ""},
			// 5 assistant content:null 带 tool_calls → 摘要（含 id / name / arguments）
			{"5 assistant null+tool_calls 摘要", `{"model":"glm-5.3-flash","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c1","function":{"name":"create_file","arguments":"{\"path\":\"a.txt\"}"}},{"id":"c2","function":{"name":"get_weather","arguments":"{}"}}]}]}`, `Assistant: [tool_calls: id=c1 name=create_file arguments={"path":"a.txt"}; id=c2 name=get_weather arguments={}]`, ""},
			// 6 assistant content:null 无 tool_calls → 空串兜底
			{"6 assistant null 无 tool_calls 兜底", `{"model":"glm-5.3-flash","messages":[{"role":"assistant","content":null}]}`, "Assistant: ", ""},
			// 7 tool content:null → 空内容不 400
			{"7 tool content null 空内容", `{"model":"glm-5.3-flash","messages":[{"role":"tool","tool_call_id":"c9","content":null}]}`, "Tool[c9]: ", ""},
			// 8 tool_calls 无 function.name 与 arguments → id 降级 + {} 兜底
			{"8 assistant null tool_calls 无 name id 降级", `{"model":"glm-5.3-flash","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c9"}]}]}`, "Assistant: [tool_calls: id=c9 name=c9 arguments={}]", ""},
			// 9 完整多轮回合（user → assistant[tool_calls] → tool → assistant），四段顺序折叠
			{"9 完整多轮回合", `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"请创建文件并查询天气"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","function":{"name":"create_file","arguments":"{\"path\":\"a.txt\"}"}},{"id":"c2","function":{"name":"get_weather","arguments":"{\"city\":\"bj\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"文件已创建"},{"role":"assistant","content":"已创建完成"}]}`, "User: 请创建文件并查询天气\n\nAssistant: [tool_calls: id=c1 name=create_file arguments={\"path\":\"a.txt\"}; id=c2 name=get_weather arguments={\"city\":\"bj\"}]\n\nTool[call_1]: 文件已创建\n\nAssistant: 已创建完成", ""},
			// 10 未知角色仍 400
			{"10 未知角色", `{"model":"glm-5.3-flash","messages":[{"role":"developer","content":"x"}]}`, "", `unsupported role "developer"`},
			// 11 user content 缺失仍 400
			{"11 user content 缺失", `{"model":"glm-5.3-flash","messages":[{"role":"user"}]}`, "", "messages[0].content must be a string"},
			// 12 content 数组 → 解码层类型错误 400（无稳定错误文本断言）
			{"12 content 数组", `{"model":"glm-5.3-flash","messages":[{"role":"user","content":[{"type":"image_url"}]}]}`, "", ""},
			// 13 尾部多余 JSON → 解码层拒绝（行为不变）
			{"13 尾部多余 JSON", `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}{"trailing":true}`, "", "unexpected trailing JSON"},
			// 14 tool_calls name 与 ID 均空 → call_1 + unknown + {} 兜底
			{"14 摘要 name 与 ID 皆空 unknown 兜底", `{"model":"glm-5.3-flash","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"","function":{"name":""}}]}]}`, "Assistant: [tool_calls: id=call_1 name=unknown arguments={}]", ""},
			// 15 tool 消息 tool_call_id 与 name 并存 → ToolCallID 优先
			{"15 tool id 与 name 并存 ID 优先", `{"model":"glm-5.3-flash","messages":[{"role":"tool","tool_call_id":"call_1","name":"get_weather","content":"sunny"}]}`, "Tool[call_1]: sunny", ""},
			// 16 assistant content 非空与 tool_calls 并存 → content 与 tool_calls 摘要换行并存
			{"16 assistant content 非空与 tool_calls 并存", `{"model":"glm-5.3-flash","messages":[{"role":"assistant","content":"已调用工具，先做总结","tool_calls":[{"id":"c1","function":{"name":"create_file","arguments":"{\"path\":\"a.txt\"}"}}]}]}`, "Assistant: 已调用工具，先做总结\n[tool_calls: id=c1 name=create_file arguments={\"path\":\"a.txt\"}]", ""},
			// 17 assistant content 空串（非 nil）带 tool_calls → 仅输出 tool_calls 摘要
			{"17 assistant content 空串带 tool_calls", `{"model":"glm-5.3-flash","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c1","function":{"name":"create_file"}}]}]}`, "Assistant: [tool_calls: id=c1 name=create_file arguments={}]", ""},
		// 18 assistant content 空串（非 nil）无 tool_calls → 原样拼接（输出与空串兜底同形）
		{"18 assistant content 空串无 tool_calls", `{"model":"glm-5.3-flash","messages":[{"role":"assistant","content":""}]}`, "Assistant: ", ""},
		// 19 system content 缺失 → 400（fail-closed 边界，设计 §6 行 11 的 system 侧补齐）
		{"19 system content 缺失", `{"model":"glm-5.3-flash","messages":[{"role":"system"}]}`, "", "messages[0].content must be a string"},
		// 20 user content 显式 null → 400
		{"20 user content 显式 null", `{"model":"glm-5.3-flash","messages":[{"role":"user","content":null}]}`, "", "messages[0].content must be a string"},
		// 21 system content 显式 null → 400
		{"21 system content 显式 null", `{"model":"glm-5.3-flash","messages":[{"role":"system","content":null}]}`, "", "messages[0].content must be a string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, prompt, _, err := wbParseInbound([]byte(tc.raw))
			if tc.errContains != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.errContains)
				return
			}
			if tc.want == "" {
				// 解码层类型错误：仅需 Error，不断言具体文本（与既有用例一致）
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, prompt)
			// 折叠-装配结构不变量：每条消息恰好折叠为一个 segment，Join 结果与
			// wbParseInbound 输出逐字节一致（不依赖分隔符计数，正文含 \n\n 亦不受影响）。
			var segs []string
			for i, m := range req.Messages {
				seg, err := wbFoldInboundMessage(i, m)
				require.NoError(t, err)
				segs = append(segs, seg)
			}
			require.Equal(t, prompt, strings.Join(segs, "\n\n"))
		})
	}
}

// TestWBToolStrictModeUnchanged：回归 —— 注册工具声明仍走 wbStrictJSON
// 严格模式，未知字段必须被拒绝（宽容仅作用于未注册工具声明，注册工具 =
// 服务端代执行契约，fail-closed 不变）。
func TestWBToolStrictModeUnchanged(t *testing.T) {
	_, err := wbMapTools(json.RawMessage(`[{"type":"function","function":{"name":"echo","description":"Echo text","parameters":{"type":"object","properties":{}},"extra_field":1}}]`))
	require.ErrorContains(t, err, "invalid tool declaration")
}

func TestWBTools(t *testing.T) {
	tools, err := wbMapTools(json.RawMessage(`[{"type":"function","function":{"name":"echo","description":"Echo text","parameters":{"type":"object","properties":{"text":{"type":"string"}}}}}]`))
	require.NoError(t, err)
	cfg, err := wbBuildToolConfig(tools, wbFakeCLI(t))
	require.NoError(t, err)
	defer cfg.Close()
	require.Equal(t, "mcp__wb__echo", cfg.AllowedTools)
	p, err := spawnWBCLIProcess(wbSpawnConfig{CliPath: wbFakeCLI(t), Token: "__FAKE__:echo", MCPConfigPath: cfg.Path, AllowedTools: cfg.AllowedTools})
	require.NoError(t, err)
	t.Cleanup(func() { p.Kill(); _ = p.Wait() })
	require.NoError(t, p.WritePrompt())
	output, err := io.ReadAll(p.stdout)
	require.NoError(t, err)
	require.NoError(t, p.Wait())
	argv := strings.SplitN(string(output), "\n", 2)[0]
	require.Contains(t, argv, `"--allowedTools" "mcp__wb__echo" "--permission-mode" "bypassPermissions"`)
	require.Contains(t, argv, `"--mcp-config"`)
	require.Contains(t, argv, `"--tools" "ToolSearch,DeferExecuteTool"`)
	require.NotContains(t, argv, "Glob")
	require.NotContains(t, argv, "*")
	data, err := os.ReadFile(cfg.Path)
	require.NoError(t, err)
	var config map[string]any
	require.NoError(t, json.Unmarshal(data, &config))
	server := config["mcpServers"].(map[string]any)["wb"].(map[string]any)
	args := server["args"].([]any)
	var declarations []WBBridgeTool
	require.NoError(t, json.Unmarshal([]byte(args[1].(string)), &declarations))
	require.Equal(t, tools, declarations)
	text, err := ExecuteWBBridgeTool(declarations, "echo", json.RawMessage(`{"text":"hello"}`))
	require.NoError(t, err)
	require.Equal(t, "ECHO: hello", text)
	_, err = ExecuteWBBridgeTool(declarations, "Glob", json.RawMessage(`{}`))
	require.ErrorContains(t, err, wbErrBridgeToolUnmatched)
		tools, err = wbMapTools(json.RawMessage(`[{"type":"function","function":{"name":"unknown"}}]`))
		require.NoError(t, err)
		require.Len(t, tools, 1)
		require.Equal(t, "unknown", tools[0].Name)
		require.False(t, tools[0].ServerExecuted)
		require.Equal(t, "object", tools[0].InputSchema["type"])
	cfg.Close()
	_, err = os.Stat(cfg.Path)
	require.True(t, os.IsNotExist(err))
}

func TestWBConsumeTerminal(t *testing.T) {
	for _, tc := range []struct {
		name, input, code string
		stream            bool
	}{
		{"success", `{"type":"assistant","message":{"content":"ok"}}
{"type":"result","subtype":"success","result":"ok"}`, "", true},
		{"nonstream", `{"type":"assistant","message":{"content":"ok"}}
{"type":"result","subtype":"success","result":"ok"}`, "", false},
		{"missing", `{"type":"system"}`, wbErrStreamNoCompletion, false},
		{"mismatch", `{"type":"assistant","message":{"content":"bad"}}
{"type":"result","subtype":"success","result":"ok"}`, wbErrFulltextMismatch, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := wbTestSpawn(t, "__FAKE__:echo")
			_, _ = io.Copy(io.Discard, p.stdout)
			require.NoError(t, p.Wait())
			_ = p.stdout.Close()
			p.stdout = io.NopCloser(strings.NewReader(tc.input))
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			result, err := wbConsumeCLI(context.Background(), c, p, wbInbound{Model: "glm-5.3-flash", Stream: tc.stream}, "", time.Now())
			if tc.code != "" {
				require.ErrorContains(t, err, tc.code)
				require.NotContains(t, recorder.Body.String(), "[DONE]")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			if tc.stream {
				require.Contains(t, recorder.Body.String(), "[DONE]")
			} else {
				require.Contains(t, recorder.Body.String(), `"content":"ok"`)
			}
		})
	}
}

func TestWBRealPipeFixture(t *testing.T) {
	p := wbTestSpawn(t, "__FAKE__:fixture")
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	result, err := wbConsumeCLI(context.Background(), c, p, wbInbound{Model: "glm-5.3-flash", Stream: true}, "", time.Now())
	require.NoError(t, err)
	require.Equal(t, 10, result.Usage.InputTokens)
	require.Contains(t, recorder.Body.String(), `"content":"hello "`)
	require.Contains(t, recorder.Body.String(), `"content":"world"`)
	require.Contains(t, recorder.Body.String(), "[DONE]")
}

// Synthetic wire events exercise the pipe; live CLI provenance is verified at Studio closeout.
func TestWBResultDoesNotWaitForExit(t *testing.T) {
	p := wbTestSpawn(t, "__FAKE__:result-sleep")
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	done := make(chan error, 1)
	go func() {
		_, err := wbConsumeCLI(context.Background(), c, p, wbInbound{Stream: true}, "", time.Now())
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("completion waited for CLI exit/EOF")
	}
	select {
	case <-p.done:
		t.Fatal("fixture must still be running after result")
	default:
	}
	require.Contains(t, recorder.Body.String(), "[DONE]")
}

func TestWBConsumeCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%t", deadline), func(t *testing.T) {
			p := wbTestSpawn(t, "__FAKE__:sleep")
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			}
			defer cancel()
			killed := make(chan struct{})
			go func() { <-ctx.Done(); p.Kill(); close(killed) }()
			if !deadline {
				cancel()
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			done := make(chan error, 1)
			go func() { _, err := wbConsumeCLI(ctx, c, p, wbInbound{}, "", time.Now()); done <- err }()
			select {
			case err := <-done:
				require.ErrorContains(t, err, wbErrCLITimeout)
			case <-time.After(10 * time.Second):
				t.Fatal("cancellation failed to unblock pipe")
			}
			<-killed
			require.Equal(t, 504, recorder.Code)
			require.NotContains(t, recorder.Body.String(), "[DONE]")
			require.Error(t, p.Wait())
		})
	}
}

func TestWBDiscoveryCancellation(t *testing.T) {
	oldProbe, oldDirs := wbProbeContext, wbCommonInstallDirs
	t.Cleanup(func() { wbProbeContext, wbCommonInstallDirs = oldProbe, oldDirs; wbDiscoveryCache = sync.Map{} })
	wbCommonInstallDirs = func() []string { return nil }
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%t", deadline), func(t *testing.T) {
			wbDiscoveryCache = sync.Map{}
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			}
			defer cancel()
			entered := make(chan struct{})
			wbProbeContext = func(parent context.Context, _ string) bool { close(entered); <-parent.Done(); return false }
			done := make(chan error, 1)
			go func() { _, err := resolveWBCLIContext(ctx, "blocked-probe"); done <- err }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("probe not entered")
			}
			if !deadline {
				cancel()
			}
			select {
			case err := <-done:
				require.ErrorIs(t, err, ctx.Err())
			case <-time.After(5 * time.Second):
				t.Fatal("discovery ignored request context")
			}
			entries := 0
			wbDiscoveryCache.Range(func(_, _ any) bool { entries++; return true })
			require.Zero(t, entries, "cancelled discovery must not cache a missing CLI")
		})
	}
}

// TestWBInboundTrailingToolResultSuppressesClientTools：验证方案 A —— 当入站消息末尾为连续
// role:"tool" / role:"function" 消息段时，自动从本轮 tools 与 req.clientTools 中剔除该段
// 已执行的 B 平面客户端工具，A 平面服务端工具与未执行的其余客户端工具保留；当后续追加
// 新 user 消息后，工具自动恢复。
func TestWBInboundTrailingToolResultSuppressesClientTools(t *testing.T) {
	t.Run("single trailing tool_call_id suppresses matching client tool to empty", func(t *testing.T) {
		raw := `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"weather in Shanghai?"},{"role":"assistant","content":"Checking...","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Shanghai\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"{\"temp\":\"23C\"}"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
		req, prompt, tools, err := wbParseInbound([]byte(raw))
		require.NoError(t, err)
		require.Empty(t, tools)
		require.Empty(t, req.clientTools)
		require.Contains(t, prompt, `Assistant: Checking...`+"\n"+`[tool_calls: id=call_1 name=get_weather arguments={"city":"Shanghai"}]`)
		require.Contains(t, prompt, `Tool[call_1]: {"temp":"23C"}`)
	})

	t.Run("multiple contiguous trailing tool messages suppress all executed client tools while keeping unexecuted and server echo", func(t *testing.T) {
		raw := `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"read and weather"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a.txt\"}"}},{"id":"c2","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"BJ\"}"}},{"id":"c3","type":"function","function":{"name":"echo","arguments":"{\"text\":\"hi\"}"}}]},{"role":"tool","tool_call_id":"c1","content":"file content"},{"role":"tool","tool_call_id":"c2","content":"sunny"},{"role":"tool","tool_call_id":"c3","content":"ECHO: hi"}],"tools":[{"type":"function","function":{"name":"echo","description":"E","parameters":{"type":"object","properties":{}}}},{"type":"function","function":{"name":"read","parameters":{"type":"object"}}},{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}},{"type":"function","function":{"name":"write","parameters":{"type":"object"}}}]}`
		req, _, tools, err := wbParseInbound([]byte(raw))
		require.NoError(t, err)
		require.Len(t, tools, 2)
		require.Equal(t, "echo", tools[0].Name)
		require.True(t, tools[0].ServerExecuted)
		require.Equal(t, "write", tools[1].Name)
		require.False(t, tools[1].ServerExecuted)
		require.Equal(t, map[string]bool{"write": true}, req.clientTools)
	})

	t.Run("legacy function role or tool with explicit name suppresses by name", func(t *testing.T) {
		raw := `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"weather"},{"role":"function","name":"get_weather","content":"25C"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
		req, _, tools, err := wbParseInbound([]byte(raw))
		require.NoError(t, err)
		require.Empty(t, tools)
		require.Empty(t, req.clientTools)
	})

	t.Run("unmatched tool_call_id without name degrades gracefully without dropping tools", func(t *testing.T) {
		raw := `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"weather"},{"role":"tool","tool_call_id":"orphan_id","content":"25C"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
		req, _, tools, err := wbParseInbound([]byte(raw))
		require.NoError(t, err)
		require.Len(t, tools, 1)
		require.Equal(t, "get_weather", tools[0].Name)
		require.True(t, req.clientTools["get_weather"])
	})

	t.Run("subsequent user message restores previously executed client tool", func(t *testing.T) {
		raw := `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"weather in Shanghai?"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Shanghai\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"23C"},{"role":"assistant","content":"It is 23C."},{"role":"user","content":"Now check Beijing"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
		req, _, tools, err := wbParseInbound([]byte(raw))
		require.NoError(t, err)
		require.Len(t, tools, 1)
		require.Equal(t, "get_weather", tools[0].Name)
		require.True(t, req.clientTools["get_weather"])
	})
}
