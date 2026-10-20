package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	wbCLIDefaultTimeout  = 120 * time.Second
	wbCLIProbeTimeout    = 10 * time.Second
	wbCLIWorkDirName     = "wb-cli-workdir"
	wbCLIEnvToken        = "CODEBUDDY_AUTH_TOKEN"
	wbCLIEnvEnterpriseID = "CODEBUDDY_ENTERPRISE_ID"
	wbCLIEnvInternet     = "CODEBUDDY_INTERNET_ENVIRONMENT"
	wbCLIEnvInternetVal  = "internal"
	wbCLIEnvConfigDir    = "CODEBUDDY_CONFIG_DIR"
	wbCLIEnvPath         = "PATH"
	// wbCLIEnvServerPort pins the child's web-ui listener to a port we own.
	// Left unset, every child draws from the CLI's shared pool
	// (web-ui-port.json, 4 slots); once concurrent cold starts exhaust it the CLI
	// exits with rc=0 and no output at all — no EADDRINUSE, no stderr — which
	// surfaces upstream only as WB_STREAM_NO_COMPLETION.
	wbCLIEnvServerPort = "SERVER__PORT"
)

var (
	wbCLIKillGrace      = 3 * time.Second
	wbSignalProcessHook = wbSignalProcess
	wbWriteFileHook     = os.WriteFile
)

var wbDefaultProbe = func(path string) bool { return wbProbeContext(context.Background(), path) }
var wbProbeContext = func(parent context.Context, path string) bool {
	ctx, cancel := context.WithTimeout(parent, wbCLIProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = wbCLIEnv(path, "", "", "")
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

// wbDiscoveryNow 让负缓存的过期判定可以在测试里确定性地推进，
// 不必靠真实 sleep 去赌时序窗口。
var wbDiscoveryNow = time.Now

// wbDiscoveryNegativeTTL：未命中的探测结果只负缓存这么久。候选集固定不变
// （账号 cli_path / env / 安装位 / PATH），永久负缓存会让「先报 CLI 未安装、
// 事后装上 CLI」的部署永远无法自愈，只能重启进程。命中结果不设过期——
// 已发现的路径不会因时间失效，真失效时探测本身会在 spawn 阶段报错。
var wbDiscoveryNegativeTTL = 30 * time.Second

type wbDiscoveryEntry struct {
	path      string
	expiresAt time.Time
}

// wbDiscoveryLookup 读缓存；负缓存条目过期即作废，重新走探测。
func wbDiscoveryLookup(key string) (path string, hit bool) {
	v, ok := wbDiscoveryCache.Load(key)
	if !ok {
		return "", false
	}
	e := v.(wbDiscoveryEntry)
	if e.path == "" && !e.expiresAt.IsZero() && wbDiscoveryNow().After(e.expiresAt) {
		return "", false
	}
	return e.path, true
}

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
	if p, ok := wbDiscoveryLookup(key); ok {
		if p != "" {
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
			wbDiscoveryCache.Store(key, wbDiscoveryEntry{path: absolute})
			return absolute, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// 未命中只负缓存一段时间：候选集不变时，永久负缓存会让事后安装的 CLI
	// 永远探测不到，只能重启进程。
	wbDiscoveryCache.Store(key, wbDiscoveryEntry{expiresAt: wbDiscoveryNow().Add(wbDiscoveryNegativeTTL)})
	return "", fmt.Errorf("%s: CLI 未安装", wbErrCLINotFound)
}

func wbCLIEnv(path, token, enterprise, port string) []string {
	env := []string{wbCLIEnvToken + "=" + token, wbCLIEnvEnterpriseID + "=" + enterprise,
		wbCLIEnvInternet + "=" + wbCLIEnvInternetVal,
		wbCLIEnvConfigDir + "=" + wbCLIHomeDir(),
		"PATH=" + filepath.Dir(path) + string(os.PathListSeparator) + os.Getenv("PATH")}
	if port != "" {
		env = append(env, wbCLIEnvServerPort+"="+port)
	}
	return env
}

var wbPortMu sync.Mutex
var wbPortTaken = map[int]struct{}{}

// wbReservePort asks the kernel for a free loopback port and marks it taken for
// this gateway, so two concurrent spawns never receive the same port number.
// The registry only excludes collisions between our own spawns — it cannot stop
// an unrelated process from grabbing the port in the window between Close and
// the child's bind.
func wbReservePort() (int, error) {
	wbPortMu.Lock()
	defer wbPortMu.Unlock()
	for attempt := 0; attempt < 32; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			continue
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		if _, taken := wbPortTaken[port]; taken {
			continue
		}
		wbPortTaken[port] = struct{}{}
		return port, nil
	}
	return 0, fmt.Errorf("wb CLI 无法分配空闲端口")
}

func wbReleasePort(port int) {
	if port == 0 {
		return
	}
	wbPortMu.Lock()
	delete(wbPortTaken, port)
	wbPortMu.Unlock()
}

// wbCLIHomeDir 返回父进程合成会话文件、子进程 --resume 查找共用的配置目录。
// 自定义的相对路径必须在这里转成绝对路径：子进程的 cwd 是临时工作目录
// （spawnWBCLIProcess 的 cmd.Dir），父进程的 cwd 是网关进程目录，同一个相对值
// 在两侧指向不同目录，会话文件就会合成到子进程找不到的地方。
func wbCLIHomeDir() string {
	if custom := strings.TrimSpace(os.Getenv(wbCLIEnvConfigDir)); custom != "" {
		if abs, err := filepath.Abs(custom); err == nil {
			return abs
		}
		return custom
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".codebuddy")
	}
	if u, err := user.Current(); err == nil && u != nil && u.HomeDir != "" {
		return filepath.Join(u.HomeDir, ".codebuddy")
	}
	return filepath.Join(os.TempDir(), ".codebuddy")
}

func wbDJB2Base36(s string) string {
	var h uint32 = 5381
	for _, r := range s {
		h = ((h << 5) + h) + uint32(r)
	}
	return strconv.FormatUint(uint64(h), 36)
}

func wbCompressWorkDir(workDir string) string {
	resolved, err := filepath.EvalSymlinks(workDir)
	if err != nil || resolved == "" {
		resolved = filepath.Clean(workDir)
	}
	var b strings.Builder
	prevDash := false
	for _, r := range resolved {
		if r == '/' || r == '\\' || r == ':' || r == '-' {
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
			continue
		}
		b.WriteRune(r)
		prevDash = false
	}
	cleaned := strings.Trim(b.String(), "-")
	if cleaned == "" {
		cleaned = "root"
	}
	if len(cleaned) <= 255 {
		return cleaned
	}
	prefix := cleaned[:180]
	for len(prefix) > 0 && !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return strings.TrimRight(prefix, "-") + "-" + wbDJB2Base36(resolved)
}

func wbSessionFilePath(workDir, sessionID string) string {
	return filepath.Join(wbCLIHomeDir(), "projects", wbCompressWorkDir(workDir), sessionID+".jsonl")
}

func wbWriteSessionFile(sessionPath string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o700); err != nil {
		return err
	}
	tmpPath := sessionPath + ".tmp"
	if err := wbWriteFileHook(tmpPath, data, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		_ = os.Remove(sessionPath)
		return err
	}
	if err := os.Rename(tmpPath, sessionPath); err != nil {
		_ = os.Remove(tmpPath)
		_ = os.Remove(sessionPath)
		return err
	}
	return nil
}

type wbSpawnConfig struct {
	CliPath, Model, Prompt, Token, EnterpriseID, MCPConfigPath, AllowedTools string
	ResumeSessionID                                                          string
	SessionJSONL                                                             []byte
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
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	stdout      io.ReadCloser
	stderr      *wbStderrBuffer
	prompt      string
	sessionPath string
	done        chan struct{}
	waitErr     error
	killed      atomic.Bool
	killDone    chan struct{}
	port        int
}

func spawnWBCLIProcess(s wbSpawnConfig) (*wbCLIProcess, error) {
	workDir := filepath.Join(os.TempDir(), wbCLIWorkDirName)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, err
	}
	port, err := wbReservePort()
	if err != nil {
		return nil, err
	}
	portKept := false
	defer func() {
		if !portKept {
			wbReleasePort(port)
		}
	}()
	argv := []string{"-p", "-", "--model", s.Model, "--output-format", "stream-json", "--include-partial-messages",
		"--port", strconv.Itoa(port)}
	var sessionPath string
	if s.ResumeSessionID != "" && len(s.SessionJSONL) > 0 {
		sessionPath = wbSessionFilePath(workDir, s.ResumeSessionID)
		if err := wbWriteSessionFile(sessionPath, s.SessionJSONL); err != nil {
			return nil, err
		}
		argv = append(argv, "--resume", s.ResumeSessionID)
	}
	// Remove builtins except the MCP discovery meta-tools when MCP tools are configured:
	// with bypassPermissions the --allowedTools whitelist does NOT block builtins (real-machine evidence),
	// and --tools "" kills ToolSearch/DeferExecuteTool. When no MCP tools are configured, pass --tools ""
	// so the model cannot spin on builtin ToolSearch without an MCP bridge.
	if s.MCPConfigPath != "" {
		argv = append(argv, "--tools", "ToolSearch,DeferExecuteTool", "--mcp-config", s.MCPConfigPath)
	} else {
		argv = append(argv, "--tools", "")
	}
	if s.AllowedTools != "" {
		for _, name := range strings.Split(s.AllowedTools, ",") {
			if !strings.HasPrefix(name, "mcp__wb__") || strings.ContainsAny(name, "*? []\t\r\n") {
				if sessionPath != "" {
					_ = os.Remove(sessionPath)
				}
				return nil, fmt.Errorf("invalid wb tool allowlist")
			}
		}
		argv = append(argv, "--allowedTools", s.AllowedTools, "--permission-mode", "bypassPermissions")
	}
	cmd := exec.Command(s.CliPath, argv...)
	cmd.Dir = workDir
	cmd.Env = wbCLIEnv(s.CliPath, s.Token, s.EnterpriseID, strconv.Itoa(port))
	wbPrepareProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		if sessionPath != "" {
			_ = os.Remove(sessionPath)
		}
		return nil, err
	}
	// Own the pipe: Cmd.Wait must not close unread stdout while the scanner drains it.
	reader, writer, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		if sessionPath != "" {
			_ = os.Remove(sessionPath)
		}
		return nil, err
	}
	cmd.Stdout = writer
	stderr := &wbStderrBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = reader.Close()
		_ = writer.Close()
		if sessionPath != "" {
			_ = os.Remove(sessionPath)
		}
		return nil, fmt.Errorf("无法启动 wb CLI: %s", maskSecret(err.Error(), s.Token))
	}
	_ = writer.Close()
	p := &wbCLIProcess{cmd: cmd, stdin: stdin, stdout: reader, stderr: stderr, prompt: s.Prompt, sessionPath: sessionPath, port: port, done: make(chan struct{}), killDone: make(chan struct{})}
	portKept = true
	go func() {
		p.waitErr = cmd.Wait()
		if p.sessionPath != "" {
			_ = os.Remove(p.sessionPath)
		}
		wbReleasePort(p.port)
		close(p.done)
	}()
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
	select {
	case <-p.done:
	default:
		_ = wbSignalProcessHook(p.cmd, false)
		timer := time.NewTimer(wbCLIKillGrace)
		defer timer.Stop()
		select {
		case <-p.done:
		case <-timer.C:
		}
	}
	// Always force-terminate the process group/tree so MCP descendants are
	// cleaned up even when the parent CLI process exited first.
	_ = wbSignalProcessHook(p.cmd, true)
	_ = p.stdin.Close()
	_ = p.stdout.Close()
	if p.sessionPath != "" {
		_ = os.Remove(p.sessionPath)
	}
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
