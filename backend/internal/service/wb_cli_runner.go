package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	wbCLIDefaultTimeout  = 120 * time.Second
	wbCLIProbeTimeout    = 10 * time.Second
	wbCLIKillGrace       = 3 * time.Second
	wbCLIWorkDirName     = "wb-cli-workdir"
	wbCLIEnvToken        = "CODEBUDDY_AUTH_TOKEN"
	wbCLIEnvEnterpriseID = "CODEBUDDY_ENTERPRISE_ID"
	wbCLIEnvInternet     = "CODEBUDDY_INTERNET_ENVIRONMENT"
	wbCLIEnvInternetVal  = "internal"
	wbCLIEnvPath         = "PATH"
)

var wbDefaultProbe = func(path string) bool { return wbProbeContext(context.Background(), path) }
var wbProbeContext = func(parent context.Context, path string) bool {
	ctx, cancel := context.WithTimeout(parent, wbCLIProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = wbCLIEnv(path, "", "")
	cmd.Dir = filepath.Join(os.TempDir(), wbCLIWorkDirName)
	if os.MkdirAll(cmd.Dir, 0o700) != nil {
		return false
	}
	wbPrepareProcess(cmd)
	cmd.Cancel = func() error { return wbSignalProcess(cmd, true) }
	cmd.WaitDelay = wbCLIKillGrace
	return cmd.Run() == nil
}
var wbProbeCLI = wbDefaultProbe
var wbCommonInstallDirs = func() []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	dirs := []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin"}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local/bin"), filepath.Join(home, ".npm-global/bin"))
	}
	return dirs
}
var wbDiscoveryCache sync.Map

func wbCLICandidates(accountPath string) []string {
	candidates := []string{accountPath, os.Getenv("WORKBUDDY_CLI_PATH")}
	for _, dir := range wbCommonInstallDirs() {
		for _, name := range []string{"codebuddy", "cbc"} {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}
	for _, name := range []string{"codebuddy", "cbc"} {
		if p, err := exec.LookPath(name); err == nil {
			candidates = append(candidates, p)
		}
	}
	return candidates
}
func resolveWBCLI(accountPath string) (string, error) {
	return resolveWBCLIWithProbe(context.Background(), accountPath, wbProbeCLI)
}
func resolveWBCLIContext(ctx context.Context, accountPath string) (string, error) {
	return resolveWBCLIWithProbe(ctx, accountPath, func(path string) bool { return wbProbeContext(ctx, path) })
}
func resolveWBCLIWithProbe(ctx context.Context, accountPath string, probe func(string) bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	candidates := wbCLICandidates(accountPath)
	key := strings.Join(candidates, "\x00")
	if v, ok := wbDiscoveryCache.Load(key); ok {
		if p := v.(string); p != "" {
			return p, nil
		}
		return "", fmt.Errorf("%s: CLI 未安装", wbErrCLINotFound)
	}
	seen := map[string]bool{}
	for _, p := range candidates {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if probe(p) {
			absolute, err := filepath.Abs(p)
			if err != nil {
				return "", err
			}
			wbDiscoveryCache.Store(key, absolute)
			return absolute, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	wbDiscoveryCache.Store(key, "")
	return "", fmt.Errorf("%s: CLI 未安装", wbErrCLINotFound)
}

func wbCLIEnv(path, token, enterprise string) []string {
	return []string{wbCLIEnvToken + "=" + token, wbCLIEnvEnterpriseID + "=" + enterprise,
		wbCLIEnvInternet + "=" + wbCLIEnvInternetVal,
		"PATH=" + filepath.Dir(path) + string(os.PathListSeparator) + os.Getenv("PATH")}
}

type wbSpawnConfig struct {
	CliPath, Model, Prompt, Token, EnterpriseID, MCPConfigPath, AllowedTools string
}

// A bounded, synchronized stderr tail avoids races and unbounded upstream output retention.
type wbStderrBuffer struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *wbStderrBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.data = append(b.data, p...)
	if len(b.data) > 16384 {
		b.data = append([]byte(nil), b.data[len(b.data)-16384:]...)
		b.truncated = true
	}
	return n, nil
}
func (b *wbStderrBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.data) }

type wbCLIProcess struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   *wbStderrBuffer
	prompt   string
	done     chan struct{}
	waitErr  error
	killed   atomic.Bool
	killDone chan struct{}
}

func spawnWBCLIProcess(s wbSpawnConfig) (*wbCLIProcess, error) {
	argv := []string{"-p", "-", "--model", s.Model, "--output-format", "stream-json", "--include-partial-messages"}
	// Remove builtins except the MCP discovery meta-tools: with bypassPermissions the
	// --allowedTools whitelist does NOT block builtins (real-machine evidence), and
	// --tools "" also kills ToolSearch/DeferExecuteTool, making MCP tools unreachable.
	argv = append(argv, "--tools", "ToolSearch,DeferExecuteTool")
	if s.MCPConfigPath != "" {
		argv = append(argv, "--mcp-config", s.MCPConfigPath)
	}
	if s.AllowedTools != "" {
		for _, name := range strings.Split(s.AllowedTools, ",") {
			if !strings.HasPrefix(name, "mcp__wb__") || strings.ContainsAny(name, "*? []\t\r\n") {
				return nil, fmt.Errorf("invalid wb tool allowlist")
			}
		}
		argv = append(argv, "--allowedTools", s.AllowedTools, "--permission-mode", "bypassPermissions")
	}
	cmd := exec.Command(s.CliPath, argv...)
	cmd.Dir = filepath.Join(os.TempDir(), wbCLIWorkDirName)
	if err := os.MkdirAll(cmd.Dir, 0o700); err != nil {
		return nil, err
	}
	cmd.Env = wbCLIEnv(s.CliPath, s.Token, s.EnterpriseID)
	wbPrepareProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Own the pipe: Cmd.Wait must not close unread stdout while the scanner drains it.
	reader, writer, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	cmd.Stdout = writer
	stderr := &wbStderrBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = reader.Close()
		_ = writer.Close()
		return nil, fmt.Errorf("无法启动 wb CLI: %s", maskSecret(err.Error(), s.Token))
	}
	_ = writer.Close()
	p := &wbCLIProcess{cmd: cmd, stdin: stdin, stdout: reader, stderr: stderr, prompt: s.Prompt, done: make(chan struct{}), killDone: make(chan struct{})}
	go func() { p.waitErr = cmd.Wait(); close(p.done) }()
	return p, nil
}

func (p *wbCLIProcess) WritePrompt() error {
	defer p.stdin.Close()
	_, err := io.Copy(p.stdin, bytes.NewBufferString(p.prompt))
	return err
}

// Broadcast completion allows Kill and Wait to observe exit without stealing a result.
// Kill targets the process group so the MCP child cannot outlive the request.
func (p *wbCLIProcess) Kill() {
	if p == nil {
		return
	}
	if !p.killed.CompareAndSwap(false, true) {
		<-p.killDone
		return
	}
	defer close(p.killDone)
	_ = wbSignalProcess(p.cmd, false)
	timer := time.NewTimer(wbCLIKillGrace)
	defer timer.Stop()
	<-timer.C
	_ = wbSignalProcess(p.cmd, true)
	_ = p.stdin.Close()
	_ = p.stdout.Close()
}
func (p *wbCLIProcess) Wait() error { <-p.done; return p.waitErr }
func (p *wbCLIProcess) ExitCode() int {
	select {
	case <-p.done:
		return p.cmd.ProcessState.ExitCode()
	default:
		return -1
	}
}
func (p *wbCLIProcess) StderrSummary(token string, secrets ...string) string {
	if p == nil || p.stderr == nil {
		return ""
	}
	p.stderr.mu.Lock()
	text, truncated := string(p.stderr.data), p.stderr.truncated
	p.stderr.mu.Unlock()
	allSecrets := append([]string{token}, secrets...)
	// A bounded tail may begin inside a secret. Mask that fragment before trimming.
	if truncated {
		cut := 0
		for _, secret := range allSecrets {
			for i := 1; i < len(secret); i++ {
				if suffix := secret[i:]; len(suffix) > cut && strings.HasPrefix(text, suffix) {
					cut = len(suffix)
				}
			}
		}
		if cut > 0 {
			text = "****" + text[cut:]
		}
	}
	for _, secret := range allSecrets {
		text = maskSecret(text, secret)
	}
	s := []rune(strings.TrimSpace(text))
	if len(s) > 400 {
		s = s[:400]
	}
	return string(s)
}
func maskSecret(text, secret string) string {
	if secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, "****")
}
