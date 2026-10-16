package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	maxQoderQueueRetries   = 2
	qoderUpstreamEndpoint  = "/algo/api/v2/chat/litellm"
	qoderEmptyStreamMsg    = "upstream empty or truncated stream"
	qoderTruncatedStream   = "upstream stream truncated"
	qoderUnauthorizedMsg   = "upstream unauthorized; token refresh failed"
	qoderErrUpstream       = "upstream_error"
	qoderErrInvalidRequest = "invalid_request_error"
)

type qoderConsumeOutcome struct {
	action, errType, errMsg string
	retryAfter              time.Duration
	status                  int
	respBody                []byte
	firstToken              *int
	usage                   OpenAIUsage
}

func (s *OpenAIGatewayService) forwardQoderChatCompletions(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	started := time.Now()
	SetActualOpenAIUpstreamEndpoint(c, qoderUpstreamEndpoint)
	reqModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if _, err := domain.NormalizeQoderModel(reqModel); err != nil {
		return nil, qoderWriteErrorJSON(c, http.StatusBadRequest, qoderErrInvalidRequest, err.Error())
	}
	creds, err := s.resolveQoderAccountCredentials(ctx, account, false)
	if err != nil {
		return nil, qoderWriteErrorJSON(c, http.StatusUnauthorized, qoderErrUpstream, err.Error())
	}
	v3Body, nativeModel, isStream, err := BuildQoderV3Request(body, &creds)
	if err != nil {
		return nil, qoderWriteErrorJSON(c, http.StatusBadRequest, qoderErrInvalidRequest, err.Error())
	}
	return s.executeQoderWithRetry(ctx, c, account, creds, v3Body, reqModel, nativeModel, isStream, started)
}

func (s *OpenAIGatewayService) executeQoderWithRetry(ctx context.Context, c *gin.Context, account *Account, creds QoderCredentials, v3Body []byte, reqModel, nativeModel string, isStream bool, started time.Time) (*OpenAIForwardResult, error) {
	queueRetries, replayed401 := 0, false
	for {
		signed, err := s.signQoderV3Payload(ctx, &creds, v3Body)
		if err != nil {
			return nil, qoderWriteErrorJSON(c, http.StatusBadGateway, qoderErrUpstream, err.Error())
		}
		resp, err := s.doQoderUpstreamRequest(ctx, account, signed)
		if err != nil {
			return nil, qoderWriteErrorJSON(c, http.StatusBadGateway, qoderErrUpstream, err.Error())
		}
		if resp.StatusCode == http.StatusUnauthorized && !replayed401 {
			_ = resp.Body.Close()
			if refreshed, refErr := s.resolveQoderAccountCredentials(ctx, account, true); refErr == nil {
				creds, replayed401, v3Body = refreshed, true, refreshQoderV3RequestID(v3Body)
				continue
			}
			return nil, qoderWriteErrorJSON(c, http.StatusUnauthorized, qoderErrUpstream, qoderUnauthorizedMsg)
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, handleQoderHTTPNon2xx(c, resp)
		}
		outcome := s.consumeQoderSSEStream(ctx, c, account, resp.Body, reqModel, isStream, queueRetries, started)
		_ = resp.Body.Close()
		if outcome.action == "refresh401" && !replayed401 {
			if refreshed, refErr := s.resolveQoderAccountCredentials(ctx, account, true); refErr == nil {
				creds, replayed401, v3Body = refreshed, true, refreshQoderV3RequestID(v3Body)
				continue
			}
			return nil, qoderWriteErrorJSON(c, http.StatusUnauthorized, qoderErrUpstream, qoderUnauthorizedMsg)
		}
		if outcome.action == "retry" {
			queueRetries++
			if err := s.sleepQoderRetry(ctx, outcome.retryAfter); err != nil {
				return nil, qoderWriteErrorJSON(c, http.StatusGatewayTimeout, qoderErrUpstream, err.Error())
			}
			v3Body = refreshQoderV3RequestID(v3Body)
			continue
		}
		if outcome.action == "error" || outcome.action == "refresh401" {
			return nil, qoderWriteErrorJSON(c, outcome.status, outcome.errType, outcome.errMsg)
		}
		if !isStream {
			c.Data(http.StatusOK, "application/json; charset=utf-8", outcome.respBody)
		}
		return &OpenAIForwardResult{
			Model: reqModel, UpstreamModel: nativeModel, UpstreamEndpoint: qoderUpstreamEndpoint,
			Stream: isStream, Duration: time.Since(started), FirstTokenMs: outcome.firstToken, Usage: outcome.usage,
		}, nil
	}
}

func (s *OpenAIGatewayService) consumeQoderSSEStream(ctx context.Context, c *gin.Context, account *Account, streamBody io.Reader, reqModel string, isStream bool, queueRetries int, started time.Time) qoderConsumeOutcome {
	agg := NewQoderNonStreamAggregator(reqModel)
	scanner := bufio.NewScanner(streamBody)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	streamStarted, finishSeen := false, false
	var firstToken *int
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimSuffix(scanner.Text(), "\r"))
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		ev, _ := ParseQoderOuterFrame([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))))
		if ev.Skip {
			continue
		}
		if ev.Corrupt || ev.FatalError || ev.Queue {
			return s.handleQoderAbnormalEvent(ctx, c, account, ev, streamStarted, queueRetries, agg, firstToken)
		}
		prev := agg.EffectiveCount
		chunkBytes, _ := agg.FeedDataFrame(ev.Data)
		if agg.EffectiveCount == prev && extractQoderChoiceRaw(ev.Data, "role") != "" {
			agg.EffectiveCount++
		}
		if agg.FinishReason != "" {
			finishSeen = true
		}
		if isStream && (streamStarted || agg.EffectiveCount > prev) {
			if !streamStarted {
				c.Header("Content-Type", "text/event-stream; charset=utf-8")
				c.Header("Cache-Control", "no-cache")
				ms := int(time.Since(started).Milliseconds())
				firstToken, streamStarted = &ms, true
			}
			_, _ = c.Writer.WriteString("data: " + string(chunkBytes) + "\n\n")
			c.Writer.Flush()
		}
	}
	if scanner.Err() != nil {
		if streamStarted {
			return finishQoderStartedStream(c, qoderTruncatedStream, agg, firstToken)
		}
		return qoderConsumeOutcome{action: "error", status: http.StatusBadGateway, errType: qoderErrUpstream, errMsg: qoderEmptyStreamMsg}
	}
	return finalizeQoderStreamEOF(c, agg, isStream, streamStarted, finishSeen, firstToken)
}

func (s *OpenAIGatewayService) handleQoderAbnormalEvent(ctx context.Context, c *gin.Context, account *Account, ev QoderOuterFrameResult, streamStarted bool, queueRetries int, agg *QoderNonStreamAggregator, firstToken *int) qoderConsumeOutcome {
	if ev.Corrupt {
		if streamStarted {
			return finishQoderStartedStream(c, qoderTruncatedStream, agg, firstToken)
		}
		return qoderConsumeOutcome{action: "error", status: http.StatusBadGateway, errType: qoderErrUpstream, errMsg: qoderEmptyStreamMsg}
	}
	if streamStarted {
		if ev.Status == http.StatusUnauthorized {
			s.triggerQoderAsyncRefresh(ctx, account)
		}
		return finishQoderStartedStream(c, ev.Message, agg, firstToken)
	}
	if ev.Queue && queueRetries < maxQoderQueueRetries {
		return qoderConsumeOutcome{action: "retry", retryAfter: ev.RetryAfter}
	}
	if ev.Status == http.StatusUnauthorized {
		return qoderConsumeOutcome{action: "refresh401", status: ev.Status, errType: qoderErrUpstream, errMsg: ev.Message}
	}
	return qoderConsumeOutcome{action: "error", status: ev.Status, errType: qoderErrUpstream, errMsg: ev.Message}
}

func finalizeQoderStreamEOF(c *gin.Context, agg *QoderNonStreamAggregator, isStream, streamStarted, finishSeen bool, firstToken *int) qoderConsumeOutcome {
	usage := OpenAIUsage{InputTokens: agg.PromptTokens, OutputTokens: agg.OutputTokens, CacheReadInputTokens: agg.CachedInputTokens}
	if !isStream {
		if agg.EffectiveCount == 0 || !finishSeen {
			return qoderConsumeOutcome{action: "error", status: http.StatusBadGateway, errType: qoderErrUpstream, errMsg: qoderEmptyStreamMsg}
		}
		respBytes, err := agg.BuildResponse()
		if err != nil {
			return qoderConsumeOutcome{action: "error", status: http.StatusBadGateway, errType: qoderErrUpstream, errMsg: err.Error()}
		}
		return qoderConsumeOutcome{action: "done", status: http.StatusOK, respBody: respBytes, usage: usage}
	}
	if !streamStarted {
		return qoderConsumeOutcome{action: "error", status: http.StatusBadGateway, errType: qoderErrUpstream, errMsg: qoderEmptyStreamMsg}
	}
	if !finishSeen {
		return finishQoderStartedStream(c, qoderTruncatedStream, agg, firstToken)
	}
	return finishQoderStartedStream(c, "", agg, firstToken)
}

func finishQoderStartedStream(c *gin.Context, errMsg string, agg *QoderNonStreamAggregator, firstToken *int) qoderConsumeOutcome {
	if errMsg != "" {
		errFrame, _ := json.Marshal(map[string]any{"error": map[string]any{"message": errMsg, "type": qoderErrUpstream}})
		_, _ = c.Writer.WriteString("data: " + string(errFrame) + "\n\n")
	}
	_, _ = c.Writer.WriteString("data: [DONE]\n\n")
	c.Writer.Flush()
	return qoderConsumeOutcome{action: "done", status: http.StatusOK, firstToken: firstToken, usage: OpenAIUsage{InputTokens: agg.PromptTokens, OutputTokens: agg.OutputTokens, CacheReadInputTokens: agg.CachedInputTokens}}
}

func (s *OpenAIGatewayService) resolveQoderAccountCredentials(ctx context.Context, account *Account, forceRefresh bool) (QoderCredentials, error) {
	if account == nil {
		return QoderCredentials{}, errors.New("qoder account is nil")
	}
	if s != nil && s.qoderTokenProvider != nil {
		return s.qoderTokenProvider.GetValidCredentials(ctx, account, forceRefresh)
	}
	creds, err := ParseQoderCredentials(account.Credentials)
	if err != nil {
		return QoderCredentials{}, err
	}
	return *creds, nil
}

func (s *OpenAIGatewayService) signQoderV3Payload(ctx context.Context, creds *QoderCredentials, v3Body []byte) (*QoderSignedRequest, error) {
	if s != nil && s.qoderWasmSigner != nil {
		return s.qoderWasmSigner.SignAndEncrypt(ctx, creds, v3Body, nil)
	}
	return DefaultQoderWasmSigner().SignAndEncrypt(ctx, creds, v3Body, nil)
}

func (s *OpenAIGatewayService) doQoderUpstreamRequest(ctx context.Context, account *Account, signed *QoderSignedRequest) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, signed.URL, bytes.NewReader(signed.CiphertextBody))
	if err != nil {
		return nil, err
	}
	for k, v := range signed.Headers {
		req.Header.Set(k, v)
	}
	if s != nil && s.httpUpstream != nil {
		var proxyURL string
		if account != nil && account.Proxy != nil {
			proxyURL = account.Proxy.URL()
		}
		return s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	}
	return http.DefaultClient.Do(req)
}

func (s *OpenAIGatewayService) sleepQoderRetry(ctx context.Context, d time.Duration) error {
	if s != nil && s.qoderSleep != nil {
		return s.qoderSleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *OpenAIGatewayService) triggerQoderAsyncRefresh(ctx context.Context, account *Account) {
	if s == nil {
		return
	}
	go func() {
		if s.qoderTokenProvider != nil && account != nil {
			_, _ = s.qoderTokenProvider.GetValidCredentials(context.WithoutCancel(ctx), account, true)
		}
		if s.qoderAsyncRefreshHook != nil {
			s.qoderAsyncRefreshHook()
		}
	}()
}

func refreshQoderV3RequestID(v3Body []byte) []byte {
	id := uuid.NewString()
	out, _ := sjson.SetBytes(v3Body, "request_id", id)
	out, _ = sjson.SetBytes(out, "request_set_id", id)
	out, _ = sjson.SetBytes(out, "chat_record_id", id)
	return out
}

func handleQoderHTTPNon2xx(c *gin.Context, resp *http.Response) error {
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if ev, _ := ParseQoderOuterFrame(raw); ev.Queue {
		return qoderWriteErrorJSON(c, http.StatusTooManyRequests, qoderErrUpstream, ev.Message)
	}
	return qoderWriteErrorJSON(c, resp.StatusCode, qoderErrUpstream, qoderFirstTrimNonEmpty(gjson.GetBytes(raw, "error.message").String(), string(raw), fmt.Sprintf("upstream HTTP %d", resp.StatusCode)))
}

func qoderWriteErrorJSON(c *gin.Context, status int, errType, message string) error {
	if c != nil {
		c.JSON(status, gin.H{"error": gin.H{"message": message, "type": errType}})
	}
	return fmt.Errorf("%s: %s (status=%d)", errType, message, status)
}
