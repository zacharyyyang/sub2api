//go:build unit

package service

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
	require.Contains(t, s, `"--tools" "ToolSearch,DeferExecuteTool"`)
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
