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
	"testing"

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

// TestWBPermissiveMap：AC-1/2/5/8 —— 宽容放行映射：全未注册 / 混合 / 形状与名称异常边界，均按设计 §2.1 两阶段处置（对象性检查 + 名称归属探针；注册工具严格解码 fail-closed）。
func TestWBPermissiveMap(t *testing.T) {
	cases := []struct {
		name     string
		raw      json.RawMessage
		wantN    int
		wantName string
		wantErr  string
	}{
		{"1 all unregistered 60+", wbUnregisteredToolSet(60), 0, "", ""},
		{"2 mixed echo+unregistered", wbWithEcho(wbUnregisteredToolSet(60)), 1, "echo", ""},
		{"3 echo unchanged", json.RawMessage(`[{"type":"function","function":{"name":"echo","description":"Echo text","parameters":{"type":"object","properties":{}}}}]`), 1, "echo", ""},
		{"4 empty array", json.RawMessage(`[]`), 0, "", ""},
		{"5 unregistered strict:false dropped", json.RawMessage(`[{"type":"function","function":{"name":"read","parameters":{"type":"object"},"strict":false}}]`), 0, "", ""},
		{"6 unregistered non-function type dropped", json.RawMessage(`[{"type":"file_search","name":"search"}]`), 0, "", ""},
		{"11b name missing/empty/non-string dropped", json.RawMessage(`[{"type":"function","function":{}},{"type":"function","function":{"name":""}},{"type":"function","function":{"name":123}},{"function":"echo"}]`), 0, "", ""},
		{"7 echo extra_field strict", json.RawMessage(`[{"type":"function","function":{"name":"echo","description":"E","parameters":{"type":"object","properties":{}},"extra_field":1}}]`), 0, "", "invalid tool declaration"},
		{"echo non-function type", json.RawMessage(`[{"type":"builtin","function":{"name":"echo","parameters":{"type":"object"}}}]`), 0, "", `unsupported tool type "builtin"`},
		{"8 echo duplicate", json.RawMessage(`[{"type":"function","function":{"name":"echo","parameters":{"type":"object"}}},{"type":"function","function":{"name":"echo","parameters":{"type":"object"}}}]`), 0, "", "duplicate tool"},
		{"9 echo missing schema", json.RawMessage(`[{"type":"function","function":{"name":"echo","description":"E"}}]`), 0, "", "requires an object input schema"},
		{"11a null entry", json.RawMessage(`[null]`), 0, "", "invalid tool declaration"},
		{"11 non-object: string", json.RawMessage(`["s"]`), 0, "", "invalid tool declaration"},
		{"11 non-object: number", json.RawMessage(`[123]`), 0, "", "invalid tool declaration"},
		{"11 non-object: bool", json.RawMessage(`[true]`), 0, "", "invalid tool declaration"},
		{"11 non-object: array el", json.RawMessage(`[[1]]`), 0, "", "invalid tool declaration"},
		{"10 tools non-array or null", json.RawMessage(`{}`), 0, "", "tools must be an array"},
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
			}
		})
	}
}

// TestWBPermissiveConfigAndSpawn：AC-2/AC-3 —— 注册工具正常映射 MCP 配置（仅 echo 单声明）；全未注册 → 空配置 → spawn argv 不含 --mcp-config / --allowedTools / --permission-mode。
func TestWBPermissiveConfigAndSpawn(t *testing.T) {
	tools, err := wbMapTools(wbWithEcho(wbUnregisteredToolSet(60)))
	require.NoError(t, err)
	require.Len(t, tools, 1)
	cfg, err := wbBuildToolConfig(tools, wbFakeCLI(t))
	require.NoError(t, err)
	defer cfg.Close()
	require.Equal(t, "mcp__wb__echo", cfg.AllowedTools)
	data, err := os.ReadFile(cfg.Path)
	require.NoError(t, err)
	var conf map[string]any
	require.NoError(t, json.Unmarshal(data, &conf))
	server := conf["mcpServers"].(map[string]any)["wb"].(map[string]any)
	args := server["args"].([]any)
	var decls []WBBridgeTool
	require.NoError(t, json.Unmarshal([]byte(args[1].(string)), &decls))
	require.Len(t, decls, 1)
	require.Equal(t, "echo", decls[0].Name)

	empty, err := wbBuildToolConfig([]WBBridgeTool{}, "")
	require.NoError(t, err)
	require.Empty(t, empty.Path)
	p, err := spawnWBCLIProcess(wbSpawnConfig{CliPath: wbFakeCLI(t), Token: "__FAKE__:argv-sse",
		Model: "glm-5.3-flash", Prompt: "User: hi"})
	require.NoError(t, err)
	require.NoError(t, p.WritePrompt())
	out, err := io.ReadAll(p.stdout)
	require.NoError(t, err)
	require.NoError(t, p.Wait())
	argv := strings.SplitN(string(out), "\n", 2)[0]
	require.NotContains(t, argv, "--mcp-config")
	require.NotContains(t, argv, "--allowedTools")
	require.NotContains(t, argv, "--permission-mode")
	require.Contains(t, argv, `"--tools" "ToolSearch,DeferExecuteTool"`)
}

// TestWBParseInboundPermissive：AC-4 / 用例 13 —— 合法 body + 60+ 未注册 tools 无错误、tools 为空、prompt 折叠正常（请求准入放行）。
func TestWBParseInboundPermissive(t *testing.T) {
	body := []byte(`{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":` + string(wbUnregisteredToolSet(60)) + `}`)
	req, prompt, tools, err := wbParseInbound(body)
	require.NoError(t, err)
	require.True(t, req.Stream)
	require.Equal(t, "User: hi", prompt)
	require.Empty(t, tools)
}

// TestWBForwardPermissive：AC-9 / 用例 14 —— 网关层端到端：60+ 纯未注册 tools 完整请求 → CLI 发现与 spawn 成功、预置 SSE 被网关消费、向客户端返回 200 流式（数据块 + [DONE] + usage）。
func TestWBForwardPermissive(t *testing.T) {
	body := []byte(`{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":` + string(wbUnregisteredToolSet(60)) + `}`)
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
}
