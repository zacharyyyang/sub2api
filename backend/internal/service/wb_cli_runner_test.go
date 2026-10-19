//go:build unit

package service

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		os.Exit(0)
	}
	if tok := os.Getenv(wbCLIEnvToken); strings.HasPrefix(tok, "__FAKE__") {
		body, _ := io.ReadAll(os.Stdin)
		if tok == "__FAKE__:sleep" {
			time.Sleep(time.Minute)
			os.Exit(0)
		}
		if tok == "__FAKE__:stderr" {
			fmt.Fprintln(os.Stderr, tok+" failure")
			os.Exit(7)
		}
		// EOF 后仍存活：先关掉 stdout 让扫描器见到 EOF，再挂着等被取消/超时杀掉。
		if tok == "__FAKE__:eof-sleep" {
			fmt.Println(`{"type":"system"}`)
			_ = os.Stdout.Close()
			time.Sleep(time.Minute)
			os.Exit(0)
		}
		if tok == "__FAKE__:fixture" || tok == "__FAKE__:result-sleep" {
			fmt.Println(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"a"}}}`)
			fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello "}}}`)
			fmt.Println(`{"type":"assistant","message":{"id":"a","content":[{"type":"text","text":"hello world"}]}}`)
			fmt.Println(`{"type":"result","subtype":"success","result":"hello world","usage":{"input_tokens":10,"output_tokens":2}}`)
			if tok == "__FAKE__:result-sleep" {
				time.Sleep(time.Minute)
			}
			os.Exit(0)
		}
		if tok == "__FAKE__:argv-sse" {
			fmt.Printf("ARGS:%q\n", os.Args[1:])
			fmt.Println(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"a"}}}`)
			fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello "}}}`)
			fmt.Println(`{"type":"assistant","message":{"id":"a","content":[{"type":"text","text":"hello world"}]}}`)
			fmt.Println(`{"type":"result","subtype":"success","result":"hello world","usage":{"input_tokens":10,"output_tokens":2}}`)
			os.Exit(0)
		}
		if tok == "__FAKE__:client-tool-use" {
			var mcpPath, allowed string
			for i := 1; i < len(os.Args)-1; i++ {
				if os.Args[i] == "--mcp-config" {
					mcpPath = os.Args[i+1]
				}
				if os.Args[i] == "--allowedTools" {
					allowed = os.Args[i+1]
				}
			}
			cfgBytes, cfgErr := os.ReadFile(mcpPath)
			if cfgErr != nil || !strings.Contains(string(cfgBytes), "read") || !strings.Contains(allowed, "mcp__wb__read") {
				fmt.Fprintf(os.Stderr, "missing client tool injection: err=%v allowed=%q cfg=%s\n", cfgErr, allowed, string(cfgBytes))
				os.Exit(2)
			}
			fmt.Println(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m_search"}}}`)
			fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Let me read the file."}}}`)
			fmt.Println(`{"type":"assistant","message":{"id":"m_search","usage":{"input_tokens":0,"output_tokens":0},"content":[{"type":"text","text":"Let me read the file."}]}}`)
			fmt.Println(`{"type":"assistant","message":{"id":"m_search","usage":{"input_tokens":10,"output_tokens":4},"content":[{"type":"tool_use","id":"call_search_1","name":"ToolSearch","input":{"tool_names":["mcp__wb__read"]}}]}}`)
			fmt.Println(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m_tool"}}}`)
			fmt.Println(`{"type":"assistant","message":{"id":"m_tool","usage":{"input_tokens":15,"output_tokens":6},"content":[{"type":"tool_use","id":"call_read_1","name":"DeferExecuteTool","input":{"toolName":"mcp__wb__read","params":{"path":"README.md","id_num":9007199254740993}}}]}}`)
			time.Sleep(time.Minute)
			fmt.Println(`{"type":"assistant","message":{"id":"m_after","content":[{"type":"text","text":"SHOULD_NOT_APPEAR"}]}}`)
			os.Exit(0)
		}
		if tok == "__FAKE__:round2-tool-result" {
			if err := wbVerifyFakeRound2Resume(body); err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				os.Exit(3)
			}
			fmt.Println(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m_r2"}}}`)
			fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"README Title: Sub2API"}}}`)
			fmt.Println(`{"type":"assistant","message":{"id":"m_r2","usage":{"input_tokens":20,"output_tokens":5},"content":[{"type":"text","text":"README Title: Sub2API"}]}}`)
			fmt.Println(`{"type":"result","subtype":"success","result":"README Title: Sub2API","usage":{"input_tokens":20,"output_tokens":5}}`)
			os.Exit(0)
		}
		wd, _ := os.Getwd()
		fmt.Printf("ARGS:%q\nENV:%s\nCWD:%s\nPROMPT:%s\n", os.Args[1:], strings.Join(os.Environ(), "|"), wd, body)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func wbFakeCLI(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	require.NoError(t, err)
	return p
}

func wbVerifyFakeRound2Resume(stdinBody []byte) error {
	var mcpPath, allowed, resumeID string
	for i := 1; i < len(os.Args)-1; i++ {
		switch os.Args[i] {
		case "--mcp-config":
			mcpPath = os.Args[i+1]
		case "--allowedTools":
			allowed = os.Args[i+1]
		case "--resume":
			resumeID = os.Args[i+1]
		}
	}
	if resumeID == "" {
		return fmt.Errorf("missing --resume sessionID")
	}
	cfgBytes, err := os.ReadFile(mcpPath)
	if err != nil {
		return fmt.Errorf("read mcp config: %w", err)
	}
	var mcpConf struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(cfgBytes, &mcpConf); err != nil {
		return fmt.Errorf("unmarshal mcp config: %w", err)
	}
	srv, ok := mcpConf.MCPServers["wb"]
	if !ok || len(srv.Args) < 2 || srv.Args[0] != "--tools-json" {
		return fmt.Errorf("invalid wb server in mcp config: %+v", srv)
	}
	var specs []WBBridgeTool
	if err := json.Unmarshal([]byte(srv.Args[1]), &specs); err != nil {
		return fmt.Errorf("unmarshal --tools-json: %w", err)
	}
	wantServerExec := map[string]bool{"echo": true, "read": false, "write": false, "bash": false, "glob": false}
	if len(specs) != len(wantServerExec) {
		return fmt.Errorf("expected %d MCP tool specs, got %d", len(wantServerExec), len(specs))
	}
	seenSpecNames := map[string]bool{}
	for _, sp := range specs {
		exp, exists := wantServerExec[sp.Name]
		if !exists || seenSpecNames[sp.Name] || sp.ServerExecuted != exp || sp.InputSchema == nil {
			return fmt.Errorf("unexpected or duplicate tool spec %+v", sp)
		}
		seenSpecNames[sp.Name] = true
	}
	if len(seenSpecNames) != len(wantServerExec) {
		return fmt.Errorf("expected %d unique MCP tool specs, got %d", len(wantServerExec), len(seenSpecNames))
	}
	wantAllowed := "mcp__wb__echo,mcp__wb__read,mcp__wb__write,mcp__wb__bash,mcp__wb__glob"
	if allowed != wantAllowed {
		return fmt.Errorf("allowedTools=%q, want %q", allowed, wantAllowed)
	}
	wd, _ := os.Getwd()
	sessionPath := wbSessionFilePath(wd, resumeID)
	jsonlBytes, err := os.ReadFile(sessionPath)
	if err != nil {
		return fmt.Errorf("read session jsonl %s: %w", sessionPath, err)
	}
	lines := strings.Split(strings.TrimSpace(string(jsonlBytes)), "\n")
	callToolByID := map[string]string{}
	resultOutByID := map[string]string{}
	var prevID string
	for idx, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return fmt.Errorf("line %d invalid json: %w", idx, err)
		}
		id, _ := entry["id"].(string)
		if idx == 0 && entry["parentId"] != nil {
			return fmt.Errorf("root node parentId=%v, want nil", entry["parentId"])
		}
		if idx > 0 && entry["parentId"] != prevID {
			return fmt.Errorf("line %d parentId=%v, want %q", idx, entry["parentId"], prevID)
		}
		prevID = id
		switch entry["type"] {
		case "function_call":
			cid, _ := entry["callId"].(string)
			name, _ := entry["name"].(string)
			argsStr, _ := entry["arguments"].(string)
			if name == "DeferExecuteTool" {
				var wrap map[string]any
				_ = json.Unmarshal([]byte(argsStr), &wrap)
				tn, _ := wrap["toolName"].(string)
				callToolByID[cid] = tn
			}
		case "function_call_result":
			cid, _ := entry["callId"].(string)
			if out, ok := entry["output"].(map[string]any); ok {
				txt, _ := out["text"].(string)
				resultOutByID[cid] = txt
			}
		}
	}
	if callToolByID["call_read_1"] != "mcp__wb__read" || resultOutByID["call_read_1"] != "# Sub2API" {
		return fmt.Errorf("call_read_1 mismatch: tool=%q out=%q", callToolByID["call_read_1"], resultOutByID["call_read_1"])
	}
	if callToolByID["call_echo_1"] != "mcp__wb__echo" || resultOutByID["call_echo_1"] != "ECHO: ping" {
		return fmt.Errorf("call_echo_1 mismatch: tool=%q out=%q", callToolByID["call_echo_1"], resultOutByID["call_echo_1"])
	}
	if strings.TrimSpace(string(stdinBody)) != wbResumeContinuePrompt {
		return fmt.Errorf("unexpected stdin prompt in resume round 2: %q", string(stdinBody))
	}
	return nil
}
func wbTestSpawn(t *testing.T, token string) *wbCLIProcess {
	t.Helper()
	p, err := spawnWBCLIProcess(wbSpawnConfig{CliPath: wbFakeCLI(t), Model: "glm-5.3-flash", Prompt: "User: hello", Token: token})
	require.NoError(t, err)
	t.Cleanup(func() { p.Kill(); _ = p.Wait() })
	require.NoError(t, p.WritePrompt())
	return p
}

func TestWBDiscovery(t *testing.T) {
	oldProbe, oldDirs := wbProbeCLI, wbCommonInstallDirs
	t.Cleanup(func() { wbProbeCLI, wbCommonInstallDirs = oldProbe, oldDirs; wbDiscoveryCache = sync.Map{} })
	wbCommonInstallDirs = func() []string { return []string{t.TempDir()} }
	for _, source := range []string{"account", "env", "install", "path"} {
		t.Run(source, func(t *testing.T) {
			wbDiscoveryCache = sync.Map{}
			dir := t.TempDir()
			want := filepath.Join(dir, "codebuddy")
			account := ""
			t.Setenv("WORKBUDDY_CLI_PATH", "")
			wbCommonInstallDirs = func() []string { return nil }
			switch source {
			case "account":
				account = want
			case "env":
				t.Setenv("WORKBUDDY_CLI_PATH", want)
			case "install":
				wbCommonInstallDirs = func() []string { return []string{dir} }
			}
			if source == "path" {
				if runtime.GOOS == "windows" {
					want += ".exe"
					t.Setenv("PATHEXT", ".EXE")
				}
				require.NoError(t, os.WriteFile(want, []byte("probe is stubbed"), 0o700))
				t.Setenv("PATH", dir)
			}
			calls := 0
			wbProbeCLI = func(p string) bool { calls++; return p == want }
			got, err := resolveWBCLI(account)
			require.NoError(t, err)
			require.Equal(t, want, got)
			n := calls
			_, err = resolveWBCLI(account)
			require.NoError(t, err)
			require.Equal(t, n, calls)
		})
	}
	wbDiscoveryCache = sync.Map{}
	wbProbeCLI = func(string) bool { return false }
	_, err := resolveWBCLI("")
	require.ErrorContains(t, err, wbErrCLINotFound)
	require.True(t, wbDefaultProbe(wbFakeCLI(t)))
}

func TestWBSpawnBoundary(t *testing.T) {
	t.Setenv("WB_SHOULD_NOT_LEAK", "secret-host-value")
	p := wbTestSpawn(t, "__FAKE__:echo")
	out, err := io.ReadAll(p.stdout)
	require.NoError(t, err)
	require.NoError(t, p.Wait())
	s := string(out)
	require.Contains(t, s, `"--tools" ""`)
	require.Contains(t, s, "PROMPT:User: hello")
	require.Contains(t, s, "CWD:"+filepath.Join(os.TempDir(), wbCLIWorkDirName))
	require.Contains(t, s, "CODEBUDDY_INTERNET_ENVIRONMENT=internal")
	require.NotContains(t, s, "secret-host-value")
	require.NotContains(t, s, "WB_MCP_BRIDGE_PATH")
	_, err = spawnWBCLIProcess(wbSpawnConfig{CliPath: wbFakeCLI(t), AllowedTools: "Glob"})
	require.Error(t, err)
	_, err = spawnWBCLIProcess(wbSpawnConfig{CliPath: filepath.Join(t.TempDir(), "missing")})
	require.ErrorContains(t, err, "无法启动")
}

// TestWBPortPinning guards the fix for concurrent cold-start 502s: every child
// must get its own web-ui port instead of racing for the CLI's 4-slot pool.
func TestWBPortPinning(t *testing.T) {
	seen := map[int]bool{}
	reserved := make([]int, 0, 16)
	// Release on the way out so a mid-test failure cannot leave the registry dirty
	// for the rest of the package run.
	t.Cleanup(func() {
		for _, port := range reserved {
			wbReleasePort(port)
		}
	})
	for i := 0; i < 16; i++ {
		port, err := wbReservePort()
		require.NoError(t, err)
		require.False(t, seen[port], "port %d handed out twice", port)
		seen[port] = true
		reserved = append(reserved, port)
	}
	for _, port := range reserved {
		wbReleasePort(port)
	}

	p := wbTestSpawn(t, "__FAKE__:echo")
	out, err := io.ReadAll(p.stdout)
	require.NoError(t, err)
	require.NoError(t, p.Wait())
	s := string(out)
	argvPort := regexp.MustCompile(`"--port" "(\d+)"`).FindStringSubmatch(s)
	require.NotNil(t, argvPort, "missing --port in argv: %s", s)
	envPort := regexp.MustCompile(`SERVER__PORT=(\d+)`).FindStringSubmatch(s)
	require.NotNil(t, envPort, "missing SERVER__PORT in env: %s", s)
	require.Equal(t, argvPort[1], envPort[1], "argv port and env port diverged")
	n, convErr := strconv.Atoi(argvPort[1])
	require.NoError(t, convErr)
	require.Greater(t, n, 0)
	// The port is handed back once the child exits, so a later spawn can reuse it.
	wbPortMu.Lock()
	_, stillHeld := wbPortTaken[n]
	wbPortMu.Unlock()
	require.False(t, stillHeld, "port %d not released after child exit", n)

	// The --version probe must stay on the default path (no port pinning).
	for _, kv := range wbCLIEnv("/usr/bin/codebuddy", "t", "e", "") {
		require.NotContains(t, kv, wbCLIEnvServerPort+"=")
	}
}

// TestWBPortReleaseOnSpawnFailure guards the failure path: a child that never
// starts must hand its reserved port straight back, leaving the registry as it was.
func TestWBPortReleaseOnSpawnFailure(t *testing.T) {
	wbPortMu.Lock()
	before := len(wbPortTaken)
	wbPortMu.Unlock()

	_, err := spawnWBCLIProcess(wbSpawnConfig{CliPath: filepath.Join(t.TempDir(), "missing")})
	require.ErrorContains(t, err, "无法启动")

	wbPortMu.Lock()
	after := len(wbPortTaken)
	wbPortMu.Unlock()
	require.Equal(t, before, after, "reserved port leaked on spawn failure")
}

// TestWBPortReleaseOnCancel guards the cancel path: killing an in-flight child
// must return its port to the registry once the wait goroutine completes.
func TestWBPortReleaseOnCancel(t *testing.T) {
	p := wbTestSpawn(t, "__FAKE__:sleep")
	require.Greater(t, p.port, 0, "child did not receive a pinned port")

	wbPortMu.Lock()
	_, held := wbPortTaken[p.port]
	wbPortMu.Unlock()
	require.True(t, held, "port %d not registered while child runs", p.port)

	p.Kill()
	_ = p.Wait()

	require.Eventually(t, func() bool {
		wbPortMu.Lock()
		defer wbPortMu.Unlock()
		_, stillHeld := wbPortTaken[p.port]
		return !stillHeld
	}, 15*time.Second, 50*time.Millisecond, "port %d not released after cancel", p.port)
}

// TestWBPortExclusiveAcrossLiveChildren guards the real contention case: several
// children alive at the same moment must each hold a distinct port, and every one
// of those ports must be gone from the registry once the children are killed.
func TestWBPortExclusiveAcrossLiveChildren(t *testing.T) {
	const n = 4
	procs := make([]*wbCLIProcess, 0, n)
	for i := 0; i < n; i++ {
		procs = append(procs, wbTestSpawn(t, "__FAKE__:sleep"))
	}

	ports := map[int]bool{}
	for _, p := range procs {
		require.Greater(t, p.port, 0, "child did not receive a pinned port")
		require.False(t, ports[p.port], "two live children share port %d", p.port)
		ports[p.port] = true
	}

	var unregistered []int
	wbPortMu.Lock()
	for port := range ports {
		if _, held := wbPortTaken[port]; !held {
			unregistered = append(unregistered, port)
		}
	}
	wbPortMu.Unlock()
	// 不在锁内断言：require 失败会直接结束测试，显式解锁走不到，
	// 而 Cleanup 里的 Kill/Wait 又要等 runner 拿到同一把锁才能关 done。
	require.Empty(t, unregistered, "ports %v not registered while their children run", unregistered)

	for _, p := range procs {
		p.Kill()
		_ = p.Wait()
	}
	require.Eventually(t, func() bool {
		wbPortMu.Lock()
		defer wbPortMu.Unlock()
		for port := range ports {
			if _, stillHeld := wbPortTaken[port]; stillHeld {
				return false
			}
		}
		return true
	}, 15*time.Second, 50*time.Millisecond, "ports not released after all children exited")
}

func TestWBWaitBroadcastAndRedaction(t *testing.T) {
	p := wbTestSpawn(t, "__FAKE__:sleep")
	go p.Kill()
	for i := 0; i < 2; i++ {
		wait := make(chan error, 1)
		go func() { wait <- p.Wait() }()
		select {
		case err := <-wait:
			require.Error(t, err)
		case <-time.After(6 * time.Second):
			t.Fatal("Wait blocked")
		}
	}
	p2 := wbTestSpawn(t, "__FAKE__:stderr")
	_, _ = io.Copy(io.Discard, p2.stdout)
	require.Error(t, p2.Wait())
	require.Equal(t, 7, p2.ExitCode())
	require.NotContains(t, p2.StderrSummary("__FAKE__:stderr"), "__FAKE__")
	require.Equal(t, "****", maskSecret("secret with spaces", "secret with spaces"))
	b := &wbStderrBuffer{}
	_, _ = b.Write([]byte(strings.Repeat("字", 600)))
	require.Len(t, []rune((&wbCLIProcess{stderr: b}).StderrSummary("")), 400)
}

func TestWBConcurrentKillBarrier(t *testing.T) {
	p := wbTestSpawn(t, "__FAKE__:sleep")
	start := make(chan struct{})
	returned := make(chan bool, 8)
	for i := 0; i < cap(returned); i++ {
		go func() {
			<-start
			p.Kill()
			select {
			case <-p.killDone:
				returned <- true
			default:
				returned <- false
			}
		}()
	}
	close(start)
	for i := 0; i < cap(returned); i++ {
		select {
		case complete := <-returned:
			require.True(t, complete, "Kill returned before cleanup barrier")
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent Kill blocked")
		}
	}
	require.Error(t, p.Wait())
	_, err := p.stdout.Read(make([]byte, 1))
	require.ErrorIs(t, err, os.ErrClosed)
}

func TestWBStderrRedactionBeforeTruncation(t *testing.T) {
	secrets := []string{"client-secret-1234567890", "pt-key-1234567890", "enterprise-1234567890", "client-id-1234567890"}
	for _, secret := range secrets {
		t.Run(secret, func(t *testing.T) {
			b := &wbStderrBuffer{}
			_, _ = b.Write([]byte(strings.Repeat("x", 390) + secret))
			p := &wbCLIProcess{stderr: b}
			require.Equal(t, strings.Repeat("x", 390)+"****", p.StderrSummary("token", secrets...))
			b = &wbStderrBuffer{}
			p.stderr = b
			tail := strings.Repeat("x", 16384-len(secret)+5)
			_, _ = b.Write([]byte(secret[:8]))
			_, _ = b.Write([]byte(secret[8:] + tail))
			require.Equal(t, "****"+strings.Repeat("x", 396), p.StderrSummary("token", secrets...))
		})
	}
}

func TestWBKillAlwaysForcesTreeCleanup(t *testing.T) {
	oldGrace, oldHook := wbCLIKillGrace, wbSignalProcessHook
	wbCLIKillGrace = 50 * time.Millisecond
	t.Cleanup(func() {
		wbCLIKillGrace = oldGrace
		wbSignalProcessHook = oldHook
	})

	var mu sync.Mutex
	var signals []bool
	wbSignalProcessHook = func(cmd *exec.Cmd, kill bool) error {
		mu.Lock()
		signals = append(signals, kill)
		mu.Unlock()
		return wbSignalProcess(cmd, kill)
	}

	// Case 1: parent process already exited before Kill() — must still issue forced tree kill (kill=true) for MCP descendants.
	pExited := wbTestSpawn(t, "__FAKE__:fixture")
	_, _ = io.Copy(io.Discard, pExited.stdout)
	require.NoError(t, pExited.Wait())
	mu.Lock()
	signals = nil
	mu.Unlock()
	pExited.Kill()
	mu.Lock()
	require.Equal(t, []bool{true}, signals, "exited parent must still trigger forced tree cleanup")
	mu.Unlock()

	// Case 2: running process receives soft signal then forced tree kill.
	pRunning := wbTestSpawn(t, "__FAKE__:sleep")
	mu.Lock()
	signals = nil
	mu.Unlock()
	pRunning.Kill()
	mu.Lock()
	require.Contains(t, signals, false)
	require.Contains(t, signals, true)
	mu.Unlock()
}

func TestWBSessionFileLifecycleAndCompressWorkDir(t *testing.T) {
	require.Equal(t, "root", wbCompressWorkDir("/"))
	require.Equal(t, "tmp-wb-cli-workdir", wbCompressWorkDir("/tmp/wb-cli-workdir"))
	require.Equal(t, "tmp-a-b-c-wb-cli-workdir", wbCompressWorkDir("/tmp/a--b/-c-/wb-cli-workdir"))
	longDir := "/" + strings.Repeat("segment/", 40)
	compressed := wbCompressWorkDir(longDir)
	require.LessOrEqual(t, len(compressed), 200)
	require.Contains(t, compressed, "-")

	customCfgDir := t.TempDir()
	t.Setenv("CODEBUDDY_CONFIG_DIR", customCfgDir)
	t.Setenv("HOME", t.TempDir())
	require.Equal(t, customCfgDir, wbCLIHomeDir())

	sessionID := "test-session-cleanup-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	p, err := spawnWBCLIProcess(wbSpawnConfig{
		CliPath:         wbFakeCLI(t),
		Model:           "glm-5.3-flash",
		Prompt:          wbResumeContinuePrompt,
		Token:           "__FAKE__:echo",
		ResumeSessionID: sessionID,
		SessionJSONL:    []byte(`{"type":"message","id":"1","parentId":null}` + "\n"),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		p.Kill()
		_ = p.Wait()
		if p.sessionPath != "" {
			_ = os.Remove(p.sessionPath)
		}
	})
	require.NotEmpty(t, p.sessionPath)
	require.True(t, strings.HasPrefix(p.sessionPath, customCfgDir), "session file must be under custom CODEBUDDY_CONFIG_DIR")
	_, statErr := os.Stat(p.sessionPath)
	require.NoError(t, statErr, "session jsonl file must exist while process is alive")
	require.NoError(t, p.WritePrompt())
	out, err := io.ReadAll(p.stdout)
	require.NoError(t, err)
	require.NoError(t, p.Wait())
	require.Contains(t, string(out), `"--resume" "`+sessionID+`"`)
	require.Contains(t, string(out), "CODEBUDDY_CONFIG_DIR="+customCfgDir)
	_, statAfterWait := os.Stat(p.sessionPath)
	require.True(t, os.IsNotExist(statAfterWait), "session jsonl file must be removed immediately after process exits")

	// Fault injection: write failure leaves zero residual .jsonl or .tmp files.
	origWriteHook := wbWriteFileHook
	t.Cleanup(func() { wbWriteFileHook = origWriteHook })
	wbWriteFileHook = func(name string, data []byte, perm os.FileMode) error {
		_ = os.WriteFile(name, data[:4], perm)
		return fmt.Errorf("injected disk full error")
	}
	failID := "test-write-fail-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	_, err = spawnWBCLIProcess(wbSpawnConfig{
		CliPath:         wbFakeCLI(t),
		Model:           "glm-5.3-flash",
		Prompt:          wbResumeContinuePrompt,
		Token:           "__FAKE__:echo",
		ResumeSessionID: failID,
		SessionJSONL:    []byte(`{"type":"message","id":"1","parentId":null}` + "\n"),
	})
	require.ErrorContains(t, err, "injected disk full error")
	workDir := filepath.Join(os.TempDir(), wbCLIWorkDirName)
	failPath := wbSessionFilePath(workDir, failID)
	_, statFail := os.Stat(failPath)
	require.True(t, os.IsNotExist(statFail), "failed write must not leave session file")
	_, statTmp := os.Stat(failPath + ".tmp")
	require.True(t, os.IsNotExist(statTmp), "failed write must not leave .tmp file")
}
