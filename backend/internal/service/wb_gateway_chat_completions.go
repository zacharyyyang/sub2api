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
	clientTools   map[string]bool    `json:"-"`
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
// 畸形 JSON 与多段粘包。wbMapTools 的 wbStrictJSON 严格模式仅覆盖命中注册表
// 的工具声明；未注册工具声明宽容解码后静默剔除，两者互不影响。
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
	if err != nil {
		return req, "", nil, err
	}
	tools = wbFilterExecutedClientTools(tools, wbTrailingExecutedTools(req.Messages))
	req.clientTools = make(map[string]bool, len(tools))
	for _, tool := range tools {
		if !tool.ServerExecuted {
			req.clientTools[tool.Name] = true
		}
	}
	return req, strings.Join(parts, "\n\n"), tools, nil
}

// wbTrailingExecutedTools 检查入站消息末尾是否为连续的 tool / function 消息段。
// 若是，则提取该尾部连续段涉及的全部工具名集合（自身 name + 通过 tool_call_id 反查前序
// assistant.tool_calls 的 function.name）。若无法识别任何工具名，返回 nil（退化为不剔除）。
func wbTrailingExecutedTools(messages []wbInboundMessage) map[string]bool {
	n := len(messages)
	if n == 0 {
		return nil
	}
	tailStart := n
	for tailStart > 0 {
		role := messages[tailStart-1].Role
		if role != "tool" && role != "function" {
			break
		}
		tailStart--
	}
	if tailStart == n {
		return nil
	}
	callIDToName := make(map[string]string)
	for i := 0; i < tailStart; i++ {
		if messages[i].Role != "assistant" {
			continue
		}
		for _, tc := range messages[i].ToolCalls {
			id := strings.TrimSpace(tc.ID)
			fnName := strings.TrimSpace(tc.Function.Name)
			if id != "" && fnName != "" {
				callIDToName[id] = fnName
			}
		}
	}
	var executed map[string]bool
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if executed == nil {
			executed = make(map[string]bool)
		}
		executed[name] = true
	}
	for i := tailStart; i < n; i++ {
		msg := messages[i]
		if msg.Name != nil {
			add(*msg.Name)
		}
		if msg.ToolCallID != nil {
			if fnName := callIDToName[strings.TrimSpace(*msg.ToolCallID)]; fnName != "" {
				add(fnName)
			}
		}
	}
	return executed
}

// wbFilterExecutedClientTools 从本轮 tools 中剔除刚在入站末尾连续 tool/function 段执行完的
// B 平面客户端工具（ServerExecuted == false），防止无状态 CLI 在结果回传轮次重复调用同一工具；
// A 平面服务端工具（ServerExecuted == true）与未执行的其余客户端工具保留不变。
func wbFilterExecutedClientTools(tools []WBBridgeTool, executed map[string]bool) []WBBridgeTool {
	if len(tools) == 0 || len(executed) == 0 {
		return tools
	}
	filtered := make([]WBBridgeTool, 0, len(tools))
	for _, tool := range tools {
		if !tool.ServerExecuted && executed[tool.Name] {
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered
}

// wbFoldInboundMessage 将单条入站消息折叠为确定性 prompt segment（每条消息
// 恰好一个 segment）。system / user 保持 System:/User: 纯文本折叠；tool /
// function 按 Tool[<id|name>]: <content> 折叠（均无则 Tool: <content>，
// content 为 nil 视为空）；assistant 支持 content 与 tool_calls 并存
// （[tool_calls: id=<id> name=<name> arguments=<args>; ...]），无 tool_calls 且
// content 为 nil 时空串兜底 Assistant: ；未知角色仍 400。
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
		text := ""
		if message.Content != nil {
			text = *message.Content
		}
		if len(message.ToolCalls) > 0 {
			summary := "[tool_calls: " + wbToolCallSummary(message.ToolCalls) + "]"
			if text != "" {
				return "Assistant: " + text + "\n" + summary, nil
			}
			return "Assistant: " + summary, nil
		}
		return "Assistant: " + text, nil
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

// wbToolCallSummary 将 assistant 消息的 tool_calls 折叠为结构化摘要列表：
// 每个调用输出 id=<id> name=<name> arguments=<args>（id 缺省为 call_<i> 1-based，
// name 缺省取 ID 再空取 unknown，arguments 缺省为 {}），以 "; " 连接。
func wbToolCallSummary(calls []wbInboundToolCall) string {
	items := make([]string, len(calls))
	for i, call := range calls {
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i+1)
		}
		name := call.Function.Name
		if name == "" {
			if call.ID != "" {
				name = call.ID
			} else {
				name = "unknown"
			}
		}
		args := strings.TrimSpace(call.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		items[i] = fmt.Sprintf("id=%s name=%s arguments=%s", id, name, args)
	}
	return strings.Join(items, "; ")
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
		return nil, wbWriteError(c, 400, wbErrUnsupportedInbound, err.Error(), false)
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
	var lastUsage wbUsage
scan:
	for scanner.Scan() {
		if ctx.Err() != nil {
			break
		}
		var obj map[string]any
		dec := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		dec.UseNumber()
		if dec.Decode(&obj) != nil {
			continue
		}
		frame := classifyWBLine(obj, req.clientTools)
		switch frame.kind {
		case wbLineMessageStart:
			accumulator.StartMessage(frame.messageID)
		case wbLineTextDelta:
			accumulator.OnTextDelta(frame.text)
			emit(buildWbChunkFrame(frame.text, ""))
		case wbLineThinkingDelta:
			emit(buildWbChunkFrame("", frame.thinking))
		case wbLineAssistant:
			if frame.usage.InputTokens > 0 || frame.usage.OutputTokens > 0 || frame.usage.hasCache {
				lastUsage = frame.usage
			}
			if suffix, ok := accumulator.Assistant(frame.messageID, frame.text); ok {
				emit(buildWbChunkFrame(suffix, ""))
			}
			if len(frame.toolCalls) > 0 {
				accumulator.AddClientToolCalls(frame.toolCalls)
				process.Kill()
				break scan
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
	if calls := accumulator.ClientToolCalls(); len(calls) > 0 {
		if req.Stream {
			emit(buildWbToolCallsFrame(calls))
			emit(buildWbToolCallsTerminalFrame(lastUsage))
			emit(wbDoneFrame)
		} else {
			var contentVal any
			if text := accumulator.RequestText(); text != "" {
				contentVal = text
			}
			items := make([]any, len(calls))
			for i, call := range calls {
				items[i] = gin.H{
					"id":   call.ID,
					"type": "function",
					"function": gin.H{
						"name":      call.Name,
						"arguments": call.Arguments,
					},
				}
			}
			c.JSON(http.StatusOK, gin.H{
				"object": "chat.completion",
				"model":  req.Model,
				"choices": []any{gin.H{
					"index": 0,
					"message": gin.H{
						"role":       "assistant",
						"content":    contentVal,
						"tool_calls": items,
					},
					"finish_reason": "tool_calls",
				}},
				"usage": wbUsageToOpenAI(lastUsage),
			})
		}
		return &OpenAIForwardResult{
			Model: req.Model, Stream: req.Stream, Duration: time.Since(started), FirstTokenMs: firstToken,
			Usage: OpenAIUsage{InputTokens: lastUsage.InputTokens, OutputTokens: lastUsage.OutputTokens, CacheCreationInputTokens: lastUsage.CacheCreation, CacheReadInputTokens: lastUsage.CacheRead},
		}, nil
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
