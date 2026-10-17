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
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	mimoUpstreamEndpoint  = "/api/route/chat/completions"
	mimoSourceHeader      = "mimocode-cli"
	mimoUserAgent         = "Mozilla/5.0"
	mimoErrUpstream       = "upstream_error"
	mimoErrInvalidRequest = "invalid_request_error"
	mimoEmptyStreamMsg    = "upstream empty or truncated stream"
	mimoTruncatedStream   = "upstream stream truncated"
)

func (s *OpenAIGatewayService) forwardMimoChatCompletions(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	started := time.Now()
	SetActualOpenAIUpstreamEndpoint(c, mimoUpstreamEndpoint)
	reqModel, upstreamModel, reasoningEffort, isStream, outBody, err := prepareMimoChatRequestBody(account, body)
	if err != nil {
		return nil, mimoWriteErrorJSON(c, http.StatusBadRequest, mimoErrInvalidRequest, err.Error(), false)
	}
	SetOpsUpstreamModel(c, upstreamModel)
	resp, err := s.doMimoChatRequestWithRetry(ctx, account, outBody, isStream)
	if err != nil {
		return nil, mimoWriteErrorJSON(c, http.StatusBadGateway, mimoErrUpstream, err.Error(), false)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return s.handleMimoUpstreamNon2xx(ctx, c, account, resp, upstreamModel, false)
	}
	defer func() { _ = resp.Body.Close() }()
	var effPtr *string
	if reasoningEffort != "" {
		effPtr = &reasoningEffort
	}
	if isStream {
		return s.streamMimoChatCompletions(c, resp, reqModel, upstreamModel, effPtr, started)
	}
	return s.bufferMimoChatCompletions(c, resp, reqModel, upstreamModel, effPtr, started)
}

func resolveMimoMappedModelAndEffort(account *Account, reqModel, defaultMappedModel string) (upstreamModel, suffixEffort string, err error) {
	_, reqEffort, _ := domain.NormalizeMimoModel(reqModel)
	mappedModel := reqModel
	if account != nil {
		if m, matched := account.ResolveMappedModel(reqModel); matched && strings.TrimSpace(m) != "" {
			mappedModel = strings.TrimSpace(m)
		} else if strings.TrimSpace(defaultMappedModel) != "" {
			mappedModel = strings.TrimSpace(defaultMappedModel)
		}
	}
	upstreamModel, mappedEffort, err := domain.NormalizeMimoModel(mappedModel)
	if err != nil {
		return "", "", err
	}
	return upstreamModel, qoderFirstTrimNonEmpty(mappedEffort, reqEffort), nil
}

func prepareMimoChatRequestBody(account *Account, body []byte) (reqModel, upstreamModel, reasoningEffort string, isStream bool, outBody []byte, err error) {
	reqModel = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if reqModel == "" {
		return "", "", "", false, nil, errors.New("model is required")
	}
	upstreamModel, suffixEffort, normErr := resolveMimoMappedModelAndEffort(account, reqModel, "")
	if normErr != nil {
		return "", "", "", false, nil, normErr
	}
	outBody, _ = sjson.SetBytes(body, "model", upstreamModel)
	reasoningEffort = strings.TrimSpace(gjson.GetBytes(outBody, "reasoning_effort").String())
	if reasoningEffort == "" && suffixEffort != "" {
		reasoningEffort = suffixEffort
		outBody, _ = sjson.SetBytes(outBody, "reasoning_effort", suffixEffort)
	}
	isStream = gjson.GetBytes(outBody, "stream").Bool()
	if isStream && !gjson.GetBytes(outBody, "stream_options.include_usage").Exists() {
		outBody, _ = sjson.SetBytes(outBody, "stream_options.include_usage", true)
	}
	return reqModel, upstreamModel, reasoningEffort, isStream, outBody, nil
}

func (s *OpenAIGatewayService) resolveMimoAccountCredentials(ctx context.Context, account *Account, forceRefresh bool) (MimoCredentials, error) {
	if account == nil {
		return MimoCredentials{}, errors.New("mimo account is nil")
	}
	if s != nil && s.mimoTokenProvider != nil {
		return s.mimoTokenProvider.GetValidCredentials(ctx, account, forceRefresh)
	}
	if s != nil && s.accountRepo != nil {
		return newMimoTokenProviderFromAccountRepo(s.accountRepo).GetValidCredentials(ctx, account, forceRefresh)
	}
	if forceRefresh {
		return MimoCredentials{}, errors.New("mimo token provider is not configured for forceRefresh")
	}
	creds, err := ParseMimoCredentials(account.Credentials)
	if err != nil || strings.TrimSpace(creds.ServiceToken) == "" {
		return MimoCredentials{}, errors.New("mimo service_token is empty and token provider is not configured")
	}
	return *creds, nil
}

func (s *OpenAIGatewayService) doMimoChatRequestWithRetry(ctx context.Context, account *Account, payload []byte, isStream bool) (*http.Response, error) {
	creds, err := s.resolveMimoAccountCredentials(ctx, account, false)
	if err != nil {
		return nil, err
	}
	resp, err := s.sendMimoHTTPRequest(ctx, account, creds, payload, isStream)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if refreshed, refErr := s.resolveMimoAccountCredentials(ctx, account, true); refErr == nil && strings.TrimSpace(refreshed.ServiceToken) != "" {
			_ = resp.Body.Close()
			return s.sendMimoHTTPRequest(ctx, account, refreshed, payload, isStream)
		}
	}
	return resp, nil
}

func (s *OpenAIGatewayService) sendMimoHTTPRequest(ctx context.Context, account *Account, creds MimoCredentials, payload []byte, isStream bool) (*http.Response, error) {
	baseURL := qoderFirstTrimNonEmpty(strings.TrimRight(strings.TrimSpace(creds.BaseURL), "/"), DefaultMimoBaseURL)
	if s != nil && s.cfg != nil {
		validated, vErr := s.validateUpstreamBaseURL(baseURL)
		if vErr != nil {
			return nil, vErr
		}
		if validated != "" {
			baseURL = strings.TrimRight(validated, "/")
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+mimoUpstreamEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", map[bool]string{true: "text/event-stream", false: "application/json"}[isStream])
	req.Header.Set("User-Agent", mimoUserAgent)
	req.Header.Set("x-mimo-source", mimoSourceHeader)
	req.Header.Set("Cookie", BuildMimoInferenceCookie(creds))
	if s != nil && s.httpUpstream != nil {
		var proxyURL string
		var accID int64
		var conc int
		if account != nil {
			accID, conc = account.ID, account.Concurrency
			if account.Proxy != nil {
				proxyURL = account.Proxy.URL()
			}
		}
		return s.httpUpstream.Do(req, proxyURL, accID, conc)
	}
	return (&http.Client{Timeout: 2 * time.Minute}).Do(req)
}

func (s *OpenAIGatewayService) bufferMimoChatCompletions(c *gin.Context, resp *http.Response, reqModel, upstreamModel string, reasoningEffort *string, started time.Time) (*OpenAIForwardResult, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	if err != nil {
		return nil, mimoWriteErrorJSON(c, http.StatusBadGateway, mimoErrUpstream, err.Error(), false)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, mimoWriteErrorJSON(c, http.StatusBadGateway, mimoErrUpstream, "invalid upstream json response", false)
	}
	if _, hasChoices := parsed["choices"]; !hasChoices {
		if errMsg := qoderFirstTrimNonEmpty(gjson.GetBytes(raw, "error.message").String(), gjson.GetBytes(raw, "message").String()); errMsg != "" {
			return nil, mimoWriteErrorJSON(c, http.StatusBadGateway, mimoErrUpstream, truncateString(errMsg, 512), false)
		}
	}
	usage, outBytes := extractMimoChatUsage(parsed), raw
	if reqModel != "" && reqModel != upstreamModel {
		if replaced, sErr := sjson.SetBytes(raw, "model", reqModel); sErr == nil {
			outBytes = replaced
		}
	}
	if c != nil {
		c.Data(http.StatusOK, "application/json; charset=utf-8", outBytes)
	}
	return newMimoForwardResult(resp, reqModel, upstreamModel, reasoningEffort, false, started, nil, usage), nil
}

func (s *OpenAIGatewayService) streamMimoChatCompletions(c *gin.Context, resp *http.Response, reqModel, upstreamModel string, reasoningEffort *string, started time.Time) (*OpenAIForwardResult, error) {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	var usage OpenAIUsage
	var firstTokenMs *int
	streamStarted := false
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			dataStr := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if dataStr != "" && dataStr != "[DONE]" {
				var chunk map[string]any
				if json.Unmarshal([]byte(dataStr), &chunk) == nil {
					if u := extractMimoChatUsage(chunk); u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheReadInputTokens > 0 {
						usage = u
					}
				}
				if firstTokenMs == nil {
					ms := int(time.Since(started).Milliseconds())
					firstTokenMs = &ms
				}
				if reqModel != "" && reqModel != upstreamModel && json.Valid([]byte(dataStr)) {
					if replaced, sErr := sjson.SetBytes([]byte(dataStr), "model", reqModel); sErr == nil {
						line = "data: " + string(replaced)
					}
				}
			}
			if !streamStarted && c != nil {
				c.Header("Content-Type", "text/event-stream; charset=utf-8")
				c.Header("Cache-Control", "no-cache")
				streamStarted = true
			}
		}
		if streamStarted && c != nil {
			_, _ = c.Writer.WriteString(line + "\n")
			if trimmed == "" {
				c.Writer.Flush()
			}
		}
	}
	if scanErr := scanner.Err(); scanErr != nil && streamStarted && c != nil {
		errFrame, _ := json.Marshal(map[string]any{"error": map[string]any{"message": mimoTruncatedStream, "type": mimoErrUpstream}})
		_, _ = c.Writer.WriteString("data: " + string(errFrame) + "\n\ndata: [DONE]\n\n")
		c.Writer.Flush()
		return newMimoForwardResult(resp, reqModel, upstreamModel, reasoningEffort, true, started, firstTokenMs, usage), fmt.Errorf("stream usage incomplete: %w", scanErr)
	}
	if scanner.Err() != nil || !streamStarted {
		return nil, mimoWriteErrorJSON(c, http.StatusBadGateway, mimoErrUpstream, mimoEmptyStreamMsg, false)
	}
	if c != nil {
		c.Writer.Flush()
	}
	return newMimoForwardResult(resp, reqModel, upstreamModel, reasoningEffort, true, started, firstTokenMs, usage), nil
}

func newMimoForwardResult(resp *http.Response, reqModel, upstreamModel string, eff *string, stream bool, started time.Time, firstToken *int, usage OpenAIUsage) *OpenAIForwardResult {
	var hdr http.Header
	var reqID string
	if resp != nil {
		hdr, reqID = resp.Header, resp.Header.Get("x-request-id")
	}
	return &OpenAIForwardResult{
		RequestID: reqID, UpstreamHeaders: hdr, Model: reqModel, BillingModel: upstreamModel,
		UpstreamModel: upstreamModel, UpstreamEndpoint: mimoUpstreamEndpoint, ReasoningEffort: eff,
		Stream: stream, Duration: time.Since(started), FirstTokenMs: firstToken, Usage: usage,
	}
}

func (s *OpenAIGatewayService) handleMimoUpstreamNon2xx(ctx context.Context, c *gin.Context, account *Account, resp *http.Response, _ string, anthropicFormat bool) (*OpenAIForwardResult, error) {
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	msg := truncateString(qoderFirstTrimNonEmpty(gjson.GetBytes(raw, "error.message").String(), gjson.GetBytes(raw, "message").String(), gjson.GetBytes(raw, "msg").String(), strings.TrimSpace(string(raw)), fmt.Sprintf("mimo upstream HTTP %d", resp.StatusCode)), 512)
	if account != nil {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account), Platform: account.Platform, AccountID: account.ID, AccountName: account.Name, UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id"), Kind: "http_error", Message: msg})
	}
	if s != nil && account != nil {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusPaymentRequired {
			account.Status, account.ErrorMessage = StatusError, msg
			if s.accountRepo != nil {
				_ = s.accountRepo.SetError(ctx, account.ID, msg)
			}
			s.BlockAccountScheduling(account, time.Time{}, "mimo_auth_or_balance_error")
		} else if resp.StatusCode == http.StatusTooManyRequests && s.rateLimitService != nil {
			_ = s.rateLimitService.HandleUpstreamError(ctx, account, resp.StatusCode, resp.Header, raw)
		}
	}
	errType := map[int]string{http.StatusBadRequest: mimoErrInvalidRequest, http.StatusTooManyRequests: "rate_limit_error"}[resp.StatusCode]
	return nil, mimoWriteErrorJSON(c, resp.StatusCode, qoderFirstTrimNonEmpty(errType, mimoErrUpstream), msg, anthropicFormat)
}

func mimoWriteErrorJSON(c *gin.Context, status int, errType, message string, anthropicFormat bool) error {
	if c != nil {
		MarkResponseCommitted(c)
		if anthropicFormat {
			c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": errType, "message": message}})
		} else {
			c.JSON(status, gin.H{"error": gin.H{"type": errType, "message": message}})
		}
	}
	return fmt.Errorf("%s: %s (status=%d)", errType, message, status)
}
