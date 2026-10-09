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
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
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
		if wbBridgeRegistry[tool.Name] == nil {
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
		return nil, fmt.Errorf("tools must be an array")
	}
	var tools []WBBridgeTool
	var unsupported []string
	for _, entry := range entries {
		var t wbOpenAITool
		if err := wbStrictJSON(entry, &t); err != nil {
			return nil, fmt.Errorf("invalid tool declaration: %w", err)
		}
		if t.Type != "function" {
			return nil, fmt.Errorf("unsupported tool type %q", t.Type)
		}
		if wbBridgeRegistry[t.Function.Name] == nil {
			unsupported = append(unsupported, t.Function.Name)
		}
		tools = append(tools, WBBridgeTool{t.Function.Name, t.Function.Description, t.Function.Parameters})
	}
	if len(unsupported) != 0 {
		return nil, fmt.Errorf("%s: unsupported tools: %s", wbErrBridgeToolUnmatched, strings.Join(unsupported, ", "))
	}
	return tools, ValidateWBBridgeTools(tools)
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
