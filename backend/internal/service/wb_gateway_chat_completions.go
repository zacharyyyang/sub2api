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
// （与现有解码行为一致），在 UnmarshalJSON 中同时兼容 string、null 与 OpenAI
// Content Parts 数组：文本块（type = "text" | "input_text" | "output_text"）
// 按序拼接进 Content；图像块（type = "image_url" | "input_image"）按序收进
// Images，交给会话合成写成 input_image 节点。其余块类型（如 input_audio）仍返回
// 反序列化错误并映射为 400 WB_UNSUPPORTED_INBOUND——会话节点没有音频承载形态，
// 静默丢弃会让模型在不知情的前提下丢掉输入。
// ToolCallID / Name / ToolCalls 为可选字段，仅用于工具历史折叠与临时会话合成，零值 = 缺省。
type wbInboundMessage struct {
	Role       string              `json:"role"`
	Content    *string             `json:"content"`
	Images     []string            `json:"-"`
	ToolCallID *string             `json:"tool_call_id,omitempty"`
	Name       *string             `json:"name,omitempty"`
	ToolCalls  []wbInboundToolCall `json:"tool_calls,omitempty"`
}

type wbInboundContentPart struct {
	Type     string          `json:"type"`
	Text     *string         `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
	Image    *string         `json:"image"`
}

// wbInboundImageURL 解出图像块的 url：兼容 OpenAI 的 {url: "..."} 对象形态、
// 部分客户端直发的字符串形态，以及内部 input_image 块的 image 字段（data URL）。
func wbInboundImageURL(part wbInboundContentPart) (string, error) {
	trimmed := bytes.TrimSpace(part.ImageURL)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if trimmed[0] == '"' {
			var s string
			if err := json.Unmarshal(trimmed, &s); err != nil {
				return "", err
			}
			return strings.TrimSpace(s), nil
		}
		var obj struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return "", err
		}
		return strings.TrimSpace(obj.URL), nil
	}
	if part.Image != nil {
		return strings.TrimSpace(*part.Image), nil
	}
	return "", nil
}

// wbValidateImageURL 对图像 url 做 scheme 白名单 + 基础结构校验：只接受 data URL
// （CLI 原生载体）与 http(s) URL（OpenAI 兼容客户端常用形态），其余 scheme 一律拒绝——
// 未知 scheme 透传下去只会让 CLI 在无法诊断的位置失败，错误必须在这里明确。
// data URL 要求媒体类型非空（`data:image/png;base64,...` 通过，`data:;base64,...` 与
// `data:,` 拒绝）；http(s) URL 要求 scheme 之后有非空主体（裸 `https://` 拒绝）。
// 这里不做完整 URL 语法校验，也不验图片字节有效性——职责边界是「明显畸形值不进下游」。
func wbValidateImageURL(rawURL string) error {
	lower := strings.ToLower(rawURL)
	switch {
	case strings.HasPrefix(lower, "data:"):
		rest := rawURL[len("data:"):]
		comma := strings.IndexByte(rest, ',')
		if comma < 0 {
			return fmt.Errorf("malformed data URL: missing ',' separator")
		}
		// 媒体类型到第一个 ';' 为止（如 image/png;base64），必须非空。
		mediaType := rest[:comma]
		if semi := strings.IndexByte(mediaType, ';'); semi >= 0 {
			mediaType = mediaType[:semi]
		}
		if strings.TrimSpace(mediaType) == "" {
			return fmt.Errorf("malformed data URL: empty media type")
		}
		return nil
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		// scheme 之后必须还有主体，裸 `https://` 不可能是可用图片地址。
		rest := lower[strings.Index(lower, "://")+len("://"):]
		if strings.TrimSpace(rest) == "" {
			return fmt.Errorf("malformed image url: missing host after scheme")
		}
		return nil
	default:
		return fmt.Errorf("unsupported image url scheme; only data: and http(s) are supported")
	}
}

func (m *wbInboundMessage) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role       string              `json:"role"`
		Content    json.RawMessage     `json:"content"`
		ToolCallID *string             `json:"tool_call_id,omitempty"`
		Name       *string             `json:"name,omitempty"`
		ToolCalls  []wbInboundToolCall `json:"tool_calls,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.ToolCallID = raw.ToolCallID
	m.Name = raw.Name
	m.ToolCalls = raw.ToolCalls

	trimmed := bytes.TrimSpace(raw.Content)
	// Images 必须在每个成功分支里重置：UnmarshalJSON 可能被复用在同一个接收者上，
	// 残留的旧图片会让纯文本消息被 wbHasImageContent 误判为带图。
	m.Images = nil
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		m.Content = nil
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		m.Content = &s
		return nil
	}
	if trimmed[0] == '[' {
		var parts []wbInboundContentPart
		if err := json.Unmarshal(trimmed, &parts); err != nil {
			return fmt.Errorf("cannot unmarshal message content array: %w", err)
		}
		var sb strings.Builder
		var images []string
		for idx, part := range parts {
			partType := strings.ToLower(strings.TrimSpace(part.Type))
			switch partType {
			case "text", "input_text", "output_text":
				if part.Text == nil {
					return fmt.Errorf("message content part[%d] of type %q is missing text string", idx, part.Type)
				}
				sb.WriteString(*part.Text)
			case "image_url", "input_image":
				url, err := wbInboundImageURL(part)
				if err != nil {
					return fmt.Errorf("message content part[%d] of type %q has malformed image_url: %w", idx, part.Type, err)
				}
				if url == "" {
					return fmt.Errorf("message content part[%d] of type %q is missing image url", idx, part.Type)
				}
				if err := wbValidateImageURL(url); err != nil {
					return fmt.Errorf("message content part[%d] of type %q has invalid image url: %w", idx, part.Type, err)
				}
				images = append(images, url)
			default:
				return fmt.Errorf("unsupported message content part[%d] type %q; only text and image content parts are supported", idx, part.Type)
			}
		}
		if len(images) > 0 {
			// 带图像但无文本时 Content 仍需非 nil 空串：角色校验与折叠都以「Content 非 nil」
			// 判定 user 合法，置 nil 会让纯图片轮次被当成 content 缺失而 400。
			joined := sb.String()
			m.Content = &joined
			m.Images = images
			return nil
		}
		joined := sb.String()
		m.Content = &joined
		return nil
	}
	var s string
	return json.Unmarshal(trimmed, &s)
}

type wbInbound struct {
	Model           string             `json:"model"`
	Messages        []wbInboundMessage `json:"messages"`
	Stream          bool               `json:"stream"`
	Tools           json.RawMessage    `json:"tools"`
	StreamOptions   *wbStreamOptions   `json:"stream_options"`
	clientTools     map[string]bool    `json:"-"`
	resumeSessionID string             `json:"-"`
	sessionJSONL    []byte             `json:"-"`
	resumePrompt    string             `json:"-"`
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
// 畸形 JSON 与多段粘包。wbMapTools 的 wbStrictJSON 严格模式仅覆盖命中服务端
// 注册表（wbBridgeRegistry）的工具声明；未注册合法 function 工具映射为客户端
// 工具（ServerExecuted: false），非 function 或无名条目静默剔除。
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

// wbDecodeSingleJSONValue 解出唯一一个 JSON 值并要求其后无残余内容，与入站解码
// 保持同一严格度。只解首个值会把 `{} trailing` 当成合法 `{}`——CLI 事件行会因此
// 把噪音行误判成完成事件，tool_calls.arguments 会因此静默改写历史参数。
func wbDecodeSingleJSONValue(raw []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
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
	// 图像块只能落在 user 节点上：会话合成只为 user 角色写 content 数组，
	// system 走文本前缀、assistant 走 output_text、tool 走纯文本回执。放行
	// 非 user 的图像块等于静默吞掉用户输入，故在解码后按角色 fail-closed。
	for i, message := range req.Messages {
		if len(message.Images) > 0 && message.Role != "user" {
			return req, "", nil, fmt.Errorf("messages[%d].content: image content parts are only supported on user messages, got role %q", i, message.Role)
		}
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
	req.clientTools = make(map[string]bool, len(tools))
	for _, tool := range tools {
		if !tool.ServerExecuted {
			req.clientTools[tool.Name] = true
		}
	}
	prompt := strings.Join(parts, "\n\n")
	if wbNeedsResumeSession(req.Messages) {
		sessionID, jsonlData, resumePrompt := wbBuildResumeSession(req.Messages, tools)
		req.resumeSessionID = sessionID
		req.sessionJSONL = jsonlData
		req.resumePrompt = resumePrompt
	}
	return req, prompt, tools, nil
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
	if req.resumeSessionID != "" {
		prompt = req.resumePrompt
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
	process, err := spawnWBCLIProcess(wbSpawnConfig{
		CliPath:         cli,
		Model:           req.Model,
		Prompt:          prompt,
		Token:           token,
		EnterpriseID:    account.GetCredential("enterprise_id"),
		MCPConfigPath:   config.Path,
		AllowedTools:    config.AllowedTools,
		ResumeSessionID: req.resumeSessionID,
		SessionJSONL:    req.sessionJSONL,
	})
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
		if ctx.Err() != nil {
			return nil, wbWriteError(c, 504, wbErrCLITimeout, ctx.Err().Error(), false)
		}
		return nil, wbWriteError(c, 502, wbErrCLIFailed, maskSecret(err.Error(), token), false)
	}
	return wbConsumeCLI(ctx, c, process, req, token, started, account.GetCredential("enterprise_id"), account.GetCredential("client_secret"), account.GetCredential("pt_key"), account.GetCredential("client_id"))
}

// wbWaitHook, when non-nil, fires immediately before the post-EOF process.Wait().
// The cancellation test uses it to pin that instant: the hook runs after the
// pre-Wait cancellation check has already passed, so a cancel raised at the hook
// can only be answered by the re-check after Wait. Nil in production.
var wbWaitHook func()

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
		if wbDecodeSingleJSONValue(scanner.Bytes(), &obj) != nil {
			continue
		}
		frame := classifyWBLine(obj, req.clientTools)
		switch frame.kind {
		case wbLineMessageStart:
			accumulator.StartMessage(frame.messageID)
		case wbLineTextDelta:
			accumulator.OnTextDelta(frame.text)
			emit(buildWbChunkFrame(frame.text, "", !emitted))
		case wbLineThinkingDelta:
			emit(buildWbChunkFrame("", frame.thinking, !emitted))
		case wbLineAssistant:
			if frame.usage.InputTokens > 0 || frame.usage.OutputTokens > 0 || frame.usage.hasCache {
				lastUsage = frame.usage
			}
			if suffix, ok := accumulator.Assistant(frame.messageID, frame.text); ok {
				emit(buildWbChunkFrame(suffix, "", !emitted))
			}
			if len(frame.toolCalls) > 0 {
				// A 平面调用由 CLI 经桥执行，交接一终止就永远不执行且不返回结果。
				// 同一事件里 A/B 两平面并存时无法既执行 A 又透出 B（A 的结果模型
				// 再也看不到），只能显式拒绝，不静默丢弃。
				if frame.hasServerToolUse {
					return fail(502, wbErrMixedToolPlanes, "assistant event mixes server-executed and client-executed tool calls")
				}
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
			c.JSON(http.StatusOK, wbToolCallsPayload(req.Model, calls, contentVal, lastUsage))
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
		if wbWaitHook != nil {
			wbWaitHook()
		}
		waitErr := process.Wait()
		// 取消可能落在 stdout EOF 之后、进程退出之前：此时 Wait 才返回，
		// 上下文已结束但退出码来自 Kill——按设计档超时/取消一律 504，不报 502。
		if ctx.Err() != nil {
			return fail(http.StatusGatewayTimeout, wbErrCLITimeout, ctx.Err().Error())
		}
		if waitErr != nil {
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
		c.JSON(http.StatusOK, wbTextPayload(req.Model, completion.text, completion.usage))
	}
	u := completion.usage
	return &OpenAIForwardResult{Model: req.Model, Stream: req.Stream, Duration: time.Since(started), FirstTokenMs: firstToken,
		Usage: OpenAIUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheCreation, CacheReadInputTokens: u.CacheRead}}, nil
}
