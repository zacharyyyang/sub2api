package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/gin-gonic/gin"
)

type wbTokenSource interface {
	GetAccessToken(context.Context, *Account) (string, error)
	InvalidateAccountCache(int64)
}

func (s *OpenAIGatewayService) InvalidateWBToken(id int64) {
	if s.wbTokenProvider != nil {
		s.wbTokenProvider.InvalidateAccountCache(id)
	}
}

// wbStreamOptions 是 OpenAI 兼容入站 stream_options 参数的结构化声明。
// WB 网关在 CLI 流式结束时已默认输出含 usage 的终端 chunk，因此仅需解析
// include_usage，无需将参数映射传递给下游 CLI。
type wbStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// wbInboundFunctionCall 是入站 tool_calls 内 function 对象的结构化声明
// （仅用于入站解析与折叠，不映射给下游 CLI）。
type wbInboundFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// wbInboundToolCall 是入站 assistant 消息 tool_calls 数组元素的结构化声明。
type wbInboundToolCall struct {
	ID       string                `json:"id"`
	Type     string                `json:"type"`
	Function wbInboundFunctionCall `json:"function"`
}

// wbInboundMessage 是入站 messages 数组元素的具名结构体。Content 保持 *string
// （与现有解码行为一致）；ToolCallID / Name / ToolCalls 为可选字段，仅用于
// 工具历史折叠，零值 = 缺省。
type wbInboundMessage struct {
	Role       string              `json:"role"`
	Content    *string             `json:"content"`
	ToolCallID *string             `json:"tool_call_id,omitempty"`
	Name       *string             `json:"name,omitempty"`
	ToolCalls  []wbInboundToolCall `json:"tool_calls,omitempty"`
}

type wbInbound struct {
	Model         string             `json:"model"`
	Messages      []wbInboundMessage `json:"messages"`
	Stream        bool               `json:"stream"`
	Tools         json.RawMessage    `json:"tools"`
	StreamOptions *wbStreamOptions   `json:"stream_options"`
}

func wbStrictJSON(raw []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

// wbDecodeInboundJSON 是 HTTP 入站专用解码器：不启用 DisallowUnknownFields，
// 兼容标准 OpenAI SDK / 兼容客户端默认携带的常规参数（stream_options、
// temperature、user 等）；同时保留严格 JSON 语法与尾部多余数据校验，拒绝
// 畸形 JSON 与多段粘包。内部协议解析（wbMapTools 工具声明）仍走 wbStrictJSON
// 严格模式，两者互不影响。
func wbDecodeInboundJSON(raw []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

func wbParseInbound(body []byte) (wbInbound, string, []WBBridgeTool, error) {
	var req wbInbound
	if err := wbDecodeInboundJSON(body, &req); err != nil {
		return req, "", nil, err
	}
	valid := false
	for _, model := range domain.WbModelWhitelist {
		if req.Model == model {
			valid = true
			break
		}
	}
	if !valid {
		return req, "", nil, fmt.Errorf("unsupported model %q; supported: %s", req.Model, strings.Join(domain.WbModelWhitelist, ", "))
	}
	if len(req.Messages) == 0 {
		return req, "", nil, fmt.Errorf("messages must not be empty")
	}
	parts := make([]string, len(req.Messages))
	for i, message := range req.Messages {
		segment, err := wbFoldInboundMessage(i, message)
		if err != nil {
			return req, "", nil, err
		}
		parts[i] = segment
	}
	tools, err := wbMapTools(req.Tools)
	return req, strings.Join(parts, "\n\n"), tools, err
}

// wbFoldInboundMessage 将单条入站消息折叠为确定性 prompt segment（每条消息
// 恰好一个 segment）。system / user 保持 System:/User: 纯文本折叠；tool /
// function 按 Tool[<id|name>]: <content> 折叠（均无则 Tool: <content>，
// content 为 nil 视为空）；assistant 仅在 content 为 nil（含 tool_calls 摘要
// Assistant: [calls: <name>, ...] / 无则空串兜底 Assistant: ）时特殊折叠，
// content 非 nil（含空串）一律原样拼接；未知角色仍 400。
func wbFoldInboundMessage(i int, message wbInboundMessage) (string, error) {
	switch message.Role {
	case "system", "user":
		if message.Content == nil {
			return "", fmt.Errorf("messages[%d].content must be a string", i)
		}
		prefix := "System"
		if message.Role == "user" {
			prefix = "User"
		}
		return prefix + ": " + *message.Content, nil
	case "assistant":
		if message.Content != nil {
			return "Assistant: " + *message.Content, nil
		}
		if len(message.ToolCalls) > 0 {
			return "Assistant: [calls: " + wbToolCallSummary(message.ToolCalls) + "]", nil
		}
		return "Assistant: ", nil
	case "tool", "function":
		content := ""
		if message.Content != nil {
			content = *message.Content
		}
		return "Tool" + wbToolBracket(message) + ": " + content, nil
	default:
		return "", fmt.Errorf("unsupported role %q", message.Role)
	}
}

// wbToolBracket 计算 tool / function 消息折叠时的方括号括注：ToolCallID 优先，
// 为空则降级取 Name；两者皆空返回空串（不输出方括号），对应契约
// Tool[<id|name>]: <content> / Tool: <content>。
func wbToolBracket(message wbInboundMessage) string {
	switch {
	case message.ToolCallID != nil && *message.ToolCallID != "":
		return "[" + *message.ToolCallID + "]"
	case message.Name != nil && *message.Name != "":
		return "[" + *message.Name + "]"
	default:
		return ""
	}
}

// wbToolCallSummary 将 assistant 消息的 tool_calls 折叠为摘要列表：每个调用取
// Function.Name，为空则取 ID，两者皆空取 unknown；逗号 + 空格连接（按入站
// 顺序），arguments 不进入摘要（KDR-3）。
func wbToolCallSummary(calls []wbInboundToolCall) string {
	names := make([]string, len(calls))
	for i, call := range calls {
		switch {
		case call.Function.Name != "":
			names[i] = call.Function.Name
		case call.ID != "":
			names[i] = call.ID
		default:
			names[i] = "unknown"
		}
	}
	return strings.Join(names, ", ")
}

func wbWriteError(c *gin.Context, status int, code, message string, streamed bool) error {
	payload := gin.H{"error": gin.H{"code": code, "type": "upstream_error", "message": message}}
	if streamed {
		_, _ = c.Writer.WriteString(wbSSE(payload))
		c.Writer.Flush()
	} else {
		c.JSON(status, payload)
	}
	return fmt.Errorf("%s: %s", code, message)
}

func (s *OpenAIGatewayService) forwardWBChatCompletions(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	started := time.Now()
	req, prompt, tools, err := wbParseInbound(body)
	if err != nil {
		code := wbErrUnsupportedInbound
		if strings.Contains(err.Error(), wbErrBridgeToolUnmatched) {
			code = wbErrBridgeToolUnmatched
		}
		return nil, wbWriteError(c, 400, code, err.Error(), false)
	}
	ctx, cancel := context.WithTimeout(ctx, wbCLIDefaultTimeout)
	defer cancel()
	if s.wbTokenProvider == nil {
		return nil, wbWriteError(c, 502, wbErrTokenFailed, "wb token provider unavailable", false)
	}
	token, err := s.wbTokenProvider.GetAccessToken(ctx, account)
	if ctx.Err() != nil {
		return nil, wbWriteError(c, 504, wbErrCLITimeout, ctx.Err().Error(), false)
	}
	if err != nil {
		return nil, wbWriteError(c, 502, wbErrTokenFailed, "wb token exchange failed", false)
	}
	cli, err := resolveWBCLIContext(ctx, account.GetCredential("cli_path"))
	if ctx.Err() != nil {
		return nil, wbWriteError(c, 504, wbErrCLITimeout, ctx.Err().Error(), false)
	}
	if err != nil {
		return nil, wbWriteError(c, 502, wbErrCLINotFound, err.Error(), false)
	}
	config, err := wbBuildToolConfig(tools, "")
	if err != nil {
		return nil, wbWriteError(c, 502, wbErrBridgeToolUnmatched, err.Error(), false)
	}
	defer config.Close()
	if ctx.Err() != nil {
		return nil, wbWriteError(c, 504, wbErrCLITimeout, ctx.Err().Error(), false)
	}
	process, err := spawnWBCLIProcess(wbSpawnConfig{CliPath: cli, Model: req.Model, Prompt: prompt, Token: token,
		EnterpriseID: account.GetCredential("enterprise_id"), MCPConfigPath: config.Path, AllowedTools: config.AllowedTools})
	if err != nil {
		return nil, wbWriteError(c, 502, wbErrCLIFailed, err.Error(), false)
	}
	defer func() { process.Kill(); _ = process.Wait() }()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			process.Kill()
		case <-stop:
		}
	}()
	if err := process.WritePrompt(); err != nil {
		return nil, wbWriteError(c, 502, wbErrCLIFailed, maskSecret(err.Error(), token), false)
	}
	return wbConsumeCLI(ctx, c, process, req, token, started, account.GetCredential("enterprise_id"), account.GetCredential("client_secret"), account.GetCredential("pt_key"), account.GetCredential("client_id"))
}

func wbConsumeCLI(ctx context.Context, c *gin.Context, process *wbCLIProcess, req wbInbound, token string, started time.Time, secrets ...string) (*OpenAIForwardResult, error) {
	accumulator := newWbStreamAccumulator()
	emitted := false
	var firstToken *int
	emit := func(frame string) {
		if frame == "" || !req.Stream {
			return
		}
		if !emitted {
			c.Header("Content-Type", "text/event-stream; charset=utf-8")
			c.Header("Cache-Control", "no-cache")
			ms := int(time.Since(started).Milliseconds())
			firstToken = &ms
		}
		emitted = true
		_, _ = c.Writer.WriteString(frame)
		c.Writer.Flush()
	}
	fail := func(status int, code, message string, partialResult ...*OpenAIForwardResult) (*OpenAIForwardResult, error) {
		for _, secret := range append(secrets, token) {
			message = maskSecret(message, secret)
			code = maskSecret(code, secret)
		}
		var res *OpenAIForwardResult
		if len(partialResult) > 0 {
			res = partialResult[0]
		}
		return res, wbWriteError(c, status, code, message, emitted)
	}
	scanner := bufio.NewScanner(process.stdout)
	scanner.Buffer(make([]byte, 4096), 16*1024*1024)
	var completion *wbLineFrame
scan:
	for scanner.Scan() {
		if ctx.Err() != nil {
			break
		}
		var obj map[string]any
		if json.Unmarshal(scanner.Bytes(), &obj) != nil {
			continue
		}
		frame := classifyWBLine(obj)
		switch frame.kind {
		case wbLineMessageStart:
			accumulator.StartMessage(frame.messageID)
		case wbLineTextDelta:
			accumulator.OnTextDelta(frame.text)
			emit(buildWbChunkFrame(frame.text, ""))
		case wbLineThinkingDelta:
			emit(buildWbChunkFrame("", frame.thinking))
		case wbLineAssistant:
			if suffix, ok := accumulator.Assistant(frame.messageID, frame.text); ok {
				emit(buildWbChunkFrame(suffix, ""))
			}
		case wbLineError:
			return fail(502, frame.code, frame.message)
		case wbLineCompletion:
			completion = &frame
			break scan
		}
	}
	if ctx.Err() != nil {
		return fail(http.StatusGatewayTimeout, wbErrCLITimeout, ctx.Err().Error())
	}
	if err := scanner.Err(); err != nil {
		return fail(502, wbErrCLIFailed, err.Error())
	}
	if completion == nil {
		if err := process.Wait(); err != nil {
			return fail(502, wbErrCLIFailed, fmt.Sprintf("exit %d: %s", process.ExitCode(), process.StderrSummary(token, secrets...)))
		}
		return fail(502, wbErrStreamNoCompletion, "wb CLI exited without result/success")
	}
	if !accumulator.CheckFulltext(completion.text) {
		u := completion.usage
		partial := &OpenAIForwardResult{
			Model: req.Model, Stream: req.Stream, Duration: time.Since(started), FirstTokenMs: firstToken,
			Usage: OpenAIUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheCreation, CacheReadInputTokens: u.CacheRead},
		}
		return fail(502, wbErrFulltextMismatch, "wb streamed text differs from completion result", partial)
	}
	if req.Stream {
		emit(buildWbTerminalFrame(completion.usage))
		emit(wbDoneFrame)
	} else {
		c.JSON(http.StatusOK, gin.H{"object": "chat.completion", "model": req.Model,
			"choices": []any{gin.H{"index": 0, "message": gin.H{"role": "assistant", "content": completion.text}, "finish_reason": "stop"}},
			"usage":   wbUsageToOpenAI(completion.usage)})
	}
	u := completion.usage
	return &OpenAIForwardResult{Model: req.Model, Stream: req.Stream, Duration: time.Since(started), FirstTokenMs: firstToken,
		Usage: OpenAIUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheCreation, CacheReadInputTokens: u.CacheRead}}, nil
}
