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
	require.Equal(t, "System: s\n\nUser: u\n\nAssistant: a\n\nUser: v", prompt)
	for _, raw := range []string{
		`{"model":"glm-5.3-flash","messages":[{"role":"tool","content":"x"}]}`,
		`{"model":"glm-5.3-flash","messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
		`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"x","name":"n"}]}`,
		`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"x"}],"temperature":1}`,
	} {
		_, _, _, err := wbParseInbound([]byte(raw))
		require.Error(t, err)
	}
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
	_, err = wbMapTools(json.RawMessage(`[{"type":"function","function":{"name":"unknown"}}]`))
	require.ErrorContains(t, err, "unknown")
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
