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

type wbInbound struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string  `json:"role"`
		Content *string `json:"content"`
	} `json:"messages"`
	Stream bool            `json:"stream"`
	Tools  json.RawMessage `json:"tools"`
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

func wbParseInbound(body []byte) (wbInbound, string, []WBBridgeTool, error) {
	var req wbInbound
	if err := wbStrictJSON(body, &req); err != nil {
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
		prefix := map[string]string{"system": "System", "user": "User", "assistant": "Assistant"}[message.Role]
		if prefix == "" {
			return req, "", nil, fmt.Errorf("unsupported role %q: server-executed tools do not expose tool_calls", message.Role)
		}
		if message.Content == nil {
			return req, "", nil, fmt.Errorf("messages[%d].content must be a string", i)
		}
		parts[i] = prefix + ": " + *message.Content
	}
	tools, err := wbMapTools(req.Tools)
	return req, strings.Join(parts, "\n\n"), tools, err
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
