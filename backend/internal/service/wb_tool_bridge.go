package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// WBBridgeTool is the MCP tools/list descriptor passed unchanged from validated OpenAI declarations.
type WBBridgeTool struct {
	Name           string         `json:"name"`
	Description    string         `json:"description,omitempty"`
	InputSchema    map[string]any `json:"inputSchema"`
	ServerExecuted bool           `json:"serverExecuted,omitempty"`
}

type wbOpenAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// The registry contains implementations, not arbitrary names supplied by a caller.
var wbBridgeRegistry = map[string]func(json.RawMessage) (string, error){"echo": wbEcho}

func wbEcho(raw json.RawMessage) (string, error) {
	var args struct {
		Text *string `json:"text"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	if args.Text == nil {
		return "", fmt.Errorf("echo requires a string text argument")
	}
	return "ECHO: " + *args.Text, nil
}

func ExecuteWBBridgeTool(allowed []WBBridgeTool, name string, args json.RawMessage) (string, error) {
	for _, tool := range allowed {
		if tool.Name == name {
			if run := wbBridgeRegistry[name]; run != nil {
				return run(args)
			}
		}
	}
	return "", fmt.Errorf("%s: unsupported tool %q", wbErrBridgeToolUnmatched, name)
}

func ValidateWBBridgeTools(tools []WBBridgeTool) error {
	seen := map[string]bool{}
	for _, tool := range tools {
		if tool.Name == "" {
			return fmt.Errorf("invalid tool declaration: tool name must not be empty")
		}
		if tool.ServerExecuted && wbBridgeRegistry[tool.Name] == nil {
			return fmt.Errorf("%s: unsupported tool %q", wbErrBridgeToolUnmatched, tool.Name)
		}
		if seen[tool.Name] {
			return fmt.Errorf("duplicate tool %q", tool.Name)
		}
		seen[tool.Name] = true
		if tool.InputSchema == nil || tool.InputSchema["type"] != "object" {
			return fmt.Errorf("tool %q requires an object input schema", tool.Name)
		}
	}
	return nil
}

func wbMapTools(raw json.RawMessage) ([]WBBridgeTool, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var entries []json.RawMessage
	if string(raw) == "null" || json.Unmarshal(raw, &entries) != nil {
		return nil, fmt.Errorf("tools must be an array") // 顶层形态校验：不变
	}
	var tools []WBBridgeTool
	for _, entry := range entries {
		// ── 阶段一 · 对象性检查 + 名称归属探针（宽容面：仅判定归属，不校验声明形状）──
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(entry, &obj); err != nil || obj == nil {
			// 条目为 null 或非对象（数字/字符串/布尔/数组）→ 结构性畸形，归属不可判定 → 报错
			return nil, fmt.Errorf("invalid tool declaration: tool entry must be an object")
		}
		name := wbProbeToolName(obj) // 仅提取 function.name（string）；其余字段一律不触碰
		if name == "" {
			continue // 名称缺失 / 为空 / 非字符串 → 无法归属 → 静默剔除
		}
		// ── 阶段二 A · 注册工具严格契约解码（服务端代执行 = 契约面，fail-closed）──
		if wbBridgeRegistry[name] != nil {
			var t wbOpenAITool
			if err := wbStrictJSON(entry, &t); err != nil { // 注册工具 = 服务端契约：严格重解码
				return nil, fmt.Errorf("invalid tool declaration: %w", err)
			}
			if t.Type != "function" {
				return nil, fmt.Errorf("unsupported tool type %q", t.Type)
			}
			tools = append(tools, WBBridgeTool{
				Name:           t.Function.Name,
				Description:    t.Function.Description,
				InputSchema:    t.Function.Parameters,
				ServerExecuted: true,
			})
			continue
		}
		// ── 阶段二 B · 客户端自定义工具宽容映射（B 平面：注入 MCP 声明，拦截 tool_use 透出）──
		clientTool, ok := wbMapClientTool(obj, name)
		if !ok {
			continue
		}
		tools = append(tools, clientTool)
	}
	return tools, ValidateWBBridgeTools(tools) // 去重 / schema / 注册表防御
}

func wbMapClientTool(obj map[string]json.RawMessage, name string) (WBBridgeTool, bool) {
	if typeRaw, ok := obj["type"]; ok {
		var toolType string
		if json.Unmarshal(typeRaw, &toolType) != nil || (toolType != "" && toolType != "function") {
			return WBBridgeTool{}, false
		}
	}
	var fn struct {
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	}
	if fnRaw, ok := obj["function"]; ok {
		_ = json.Unmarshal(fnRaw, &fn)
	}
	schema := fn.Parameters
	if schema == nil || schema["type"] != "object" {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return WBBridgeTool{
		Name:           name,
		Description:    fn.Description,
		InputSchema:    schema,
		ServerExecuted: false,
	}, true
}

// wbProbeToolName：宽容名称探针——只取 function.name 字符串作为归属键。
// function 缺失 / function 非对象 / name 缺失 / name 为空 / name 非字符串 → 一律返回 ""，
// 由调用方按「未注册」处置（静默剔除）；探针不校验任何其他字段（未知字段照常忽略）。
func wbProbeToolName(obj map[string]json.RawMessage) string {
	fnRaw, ok := obj["function"]
	if !ok {
		return ""
	}
	var fn struct{ Name string }
	if err := json.Unmarshal(fnRaw, &fn); err != nil {
		return "" // 无法提取 string name（function 非对象 / name 类型异常）→ 未注册处置
	}
	return fn.Name
}

type wbToolConfig struct {
	Path, AllowedTools string
	cleanup            func()
}

func (c *wbToolConfig) Close() {
	if c != nil && c.cleanup != nil {
		c.cleanup()
	}
}

func wbBuildToolConfig(tools []WBBridgeTool, bridgePath string) (*wbToolConfig, error) {
	if err := ValidateWBBridgeTools(tools); err != nil {
		return nil, err
	}
	if len(tools) == 0 {
		return &wbToolConfig{}, nil
	}
	if bridgePath == "" {
		bridgePath = os.Getenv("WB_MCP_BRIDGE_PATH")
	}
	if bridgePath == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		name := "wb-mcp-bridge"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		bridgePath = filepath.Join(filepath.Dir(exe), name)
	}
	bridgePath, err := filepath.Abs(bridgePath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(bridgePath)
	if err != nil || info.IsDir() {
		return nil, fmt.Errorf("wb MCP bridge executable unavailable: %s", bridgePath)
	}
	declarations, err := json.Marshal(tools)
	if err != nil {
		return nil, err
	}
	config := map[string]any{"mcpServers": map[string]any{"wb": map[string]any{
		"command": bridgePath, "args": []string{"--tools-json", string(declarations)},
	}}}
	data, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "wb-mcp-*.json")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return nil, err
	}
	if err = f.Close(); err != nil {
		cleanup()
		return nil, err
	}
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = "mcp__wb__" + tool.Name
	}
	return &wbToolConfig{Path: f.Name(), AllowedTools: strings.Join(names, ","), cleanup: cleanup}, nil
}
