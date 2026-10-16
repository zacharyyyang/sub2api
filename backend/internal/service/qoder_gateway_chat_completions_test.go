package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
)

type fakeQoderUpstream struct {
	handler func(req *http.Request) (*http.Response, error)
}

func (u *fakeQoderUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.handler(req)
}

func (u *fakeQoderUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func newTestQoderGateway(t *testing.T, handler func(req *http.Request) (*http.Response, error), refreshHandler func(req *http.Request) (*http.Response, error)) (*OpenAIGatewayService, *Account, *fakeQoderAccountRepo) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	now := time.Now()
	acc := &Account{
		ID:       200,
		Platform: PlatformQoder,
		Credentials: map[string]any{
			"access_token":     "dt-valid-test",
			"refresh_token":    "drt-valid-test",
			"uid":              "uid-200",
			"org_id":           "org-200",
			"token_expires_at": now.Add(25 * 24 * time.Hour).UnixMilli(),
		},
	}
	repo := &fakeQoderAccountRepo{accounts: map[int64]*Account{200: acc}}
	if refreshHandler == nil {
		refreshHandler = func(_ *http.Request) (*http.Response, error) {
			b, _ := json.Marshal(map[string]any{"data": map[string]any{"token": "dt-refreshed-200", "refresh_token": "drt-refreshed-200"}})
			return &http.Response{StatusCode: http.StatusOK, Body: &readCloserBuffer{Reader: strings.NewReader(string(b))}, Header: make(http.Header)}, nil
		}
	}
	tp := NewQoderTokenProvider(repo, &http.Client{Transport: qoderRoundTripFunc(refreshHandler)}, "https://openapi.qoder.sh/api/v1/deviceToken/refresh")
	tp.sleepFn = func(time.Duration) {}
	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		httpUpstream:       &fakeQoderUpstream{handler: handler},
		qoderWasmSigner:    DefaultQoderWasmSigner(),
		qoderTokenProvider: tp,
		qoderSleep:         func(_ context.Context, _ time.Duration) error { return nil },
	}
	return svc, acc, repo
}

func makeQoderOuterFrame(status int, inner any) string {
	var bodyVal any = inner
	if m, ok := inner.(map[string]any); ok {
		b, _ := json.Marshal(m)
		bodyVal = string(b)
	}
	outer, _ := json.Marshal(map[string]any{"statusCodeValue": status, "body": bodyVal})
	return "data: " + string(outer) + "\n\n"
}

// TestQoderGatewayStreamAndUnknownModel400 覆盖设计 §6 用例 6：流式 SSE 解包、元数据帧跳过、Usage 提取与未知模型 400。
func TestQoderGatewayStreamAndUnknownModel400(t *testing.T) {
	sseStream := strings.Join([]string{
		makeQoderOuterFrame(200, map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "Hello "}}}}),
		makeQoderOuterFrame(200, "[DONE]"),
		"event: finish\ndata: {\"statusCodeValue\":200,\"body\":null}\n\n",
		makeQoderOuterFrame(200, map[string]any{
			"id":      "c1",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "World"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 15, "completion_tokens": 5, "prompt_tokens_details": map[string]any{"cached_tokens": 4}},
		}),
	}, "")
	svc, acc, _ := newTestQoderGateway(t, func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: &readCloserBuffer{Reader: strings.NewReader(sseStream)}, Header: make(http.Header)}, nil
	}, nil)

	// 1. 正常流式转发
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	reqBody := []byte(`{"model":"sonus-fable5.1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	res, err := svc.forwardQoderChatCompletions(context.Background(), c, acc, reqBody)
	if err != nil {
		t.Fatalf("forwardQoderChatCompletions stream failed: %v", err)
	}
	out := w.Body.String()
	if !strings.Contains(out, `"Hello "`) || !strings.Contains(out, `"World"`) || !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("unexpected SSE output: %s", out)
	}
	if res.Usage.InputTokens != 15 || res.Usage.OutputTokens != 5 || res.Usage.CacheReadInputTokens != 4 || res.UpstreamModel != "smodel" {
		t.Fatalf("unexpected forward result: %+v", res)
	}

	// 2. 未知模型直接返回 HTTP 400
	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	_, err2 := svc.forwardQoderChatCompletions(context.Background(), c2, acc, []byte(`{"model":"gpt-99-unknown","messages":[{"role":"user","content":"hi"}]}`))
	if err2 == nil || w2.Code != http.StatusBadRequest || !strings.Contains(w2.Body.String(), "unsupported model") {
		t.Fatalf("expected 400 unsupported model, got code=%d body=%s err=%v", w2.Code, w2.Body.String(), err2)
	}
}

// TestQoderGatewayQueue10605RetryAndExhaust429 覆盖设计 §6 用例 8, 9：未开流 10605 退避重试成功与耗尽映射 429。
func TestQoderGatewayQueue10605RetryAndExhaust429(t *testing.T) {
	queueFrame := makeQoderOuterFrame(403, map[string]any{"code": "10605", "message": `{"retryAfterSeconds":1}`})
	okStream := makeQoderOuterFrame(200, map[string]any{
		"id":      "c2",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "ok"}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 4, "completion_tokens": 2},
	})

	// 用例 8：第 1 次 10605，第 2 次正常流
	var attempts int32
	var slept []time.Duration
	svc, acc, _ := newTestQoderGateway(t, func(_ *http.Request) (*http.Response, error) {
		n := atomic.AddInt32(&attempts, 1)
		body := okStream
		if n == 1 {
			body = queueFrame
		}
		return &http.Response{StatusCode: http.StatusOK, Body: &readCloserBuffer{Reader: strings.NewReader(body)}, Header: make(http.Header)}, nil
	}, nil)
	svc.qoderSleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	res, err := svc.forwardQoderChatCompletions(context.Background(), c, acc, []byte(`{"model":"auto","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil || w.Code != http.StatusOK || len(slept) != 1 || slept[0] != 2*time.Second || res == nil {
		t.Fatalf("expected retry success after 2s clamp, code=%d slept=%v err=%v", w.Code, slept, err)
	}

	// 用例 9：连续 3 次 10605 耗尽返回 HTTP 429（绝不返回 403）
	var exAttempts int32
	svcEx, accEx, _ := newTestQoderGateway(t, func(_ *http.Request) (*http.Response, error) {
		atomic.AddInt32(&exAttempts, 1)
		return &http.Response{StatusCode: http.StatusOK, Body: &readCloserBuffer{Reader: strings.NewReader(queueFrame)}, Header: make(http.Header)}, nil
	}, nil)
	wEx := httptest.NewRecorder()
	cEx, _ := gin.CreateTestContext(wEx)
	_, errEx := svcEx.forwardQoderChatCompletions(context.Background(), cEx, accEx, []byte(`{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if errEx == nil || wEx.Code != http.StatusTooManyRequests || atomic.LoadInt32(&exAttempts) != 3 {
		t.Fatalf("expected HTTP 429 after 3 attempts (initial+2 retries), got code=%d attempts=%d err=%v", wEx.Code, exAttempts, errEx)
	}
}

// TestQoderGatewayStreamThreeStatesAnd401Replay 覆盖设计 §6 用例 11, 12, 13：流式终止三态、未开流 401 重放与已开流 401 不重放。
func TestQoderGatewayStreamThreeStatesAnd401Replay(t *testing.T) {
	// 用例 11 ①：未开流 0 有效 chunk 即 EOF -> HTTP 502
	svcEmpty, accEmpty, _ := newTestQoderGateway(t, func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: &readCloserBuffer{Reader: strings.NewReader(makeQoderOuterFrame(200, "[DONE]"))}, Header: make(http.Header)}, nil
	}, nil)
	wEmpty := httptest.NewRecorder()
	cEmpty, _ := gin.CreateTestContext(wEmpty)
	_, errEmpty := svcEmpty.forwardQoderChatCompletions(context.Background(), cEmpty, accEmpty, []byte(`{"model":"smodel","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if errEmpty == nil || wEmpty.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 on empty stream, got code=%d err=%v", wEmpty.Code, errEmpty)
	}

	// 用例 11 ②：已开流但无 finish_reason 即 EOF -> 写出 upstream stream truncated 错误帧 + [DONE]
	truncStream := makeQoderOuterFrame(200, map[string]any{"id": "c3", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}}}})
	svcTrunc, accTrunc, _ := newTestQoderGateway(t, func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: &readCloserBuffer{Reader: strings.NewReader(truncStream)}, Header: make(http.Header)}, nil
	}, nil)
	wTrunc := httptest.NewRecorder()
	cTrunc, _ := gin.CreateTestContext(wTrunc)
	_, _ = svcTrunc.forwardQoderChatCompletions(context.Background(), cTrunc, accTrunc, []byte(`{"model":"smodel","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if !strings.Contains(wTrunc.Body.String(), "upstream stream truncated") || !strings.HasSuffix(wTrunc.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("expected truncated error frame + [DONE], got %s", wTrunc.Body.String())
	}

	// 用例 12：未开流 HTTP 401 -> 强制单飞刷新写库并重放 200
	var call401 int32
	okStream := makeQoderOuterFrame(200, map[string]any{
		"id":      "c4",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "replayed"}, "finish_reason": "stop"}},
	})
	svc401, acc401, repo401 := newTestQoderGateway(t, func(_ *http.Request) (*http.Response, error) {
		if atomic.AddInt32(&call401, 1) == 1 {
			return &http.Response{StatusCode: http.StatusUnauthorized, Body: &readCloserBuffer{Reader: strings.NewReader(`{"error":"expired"}`)}, Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: &readCloserBuffer{Reader: strings.NewReader(okStream)}, Header: make(http.Header)}, nil
	}, nil)
	w401 := httptest.NewRecorder()
	c401, _ := gin.CreateTestContext(w401)
	if _, err := svc401.forwardQoderChatCompletions(context.Background(), c401, acc401, []byte(`{"model":"smodel","stream":false,"messages":[{"role":"user","content":"hi"}]}`)); err != nil || w401.Code != http.StatusOK || repo401.updates != 1 {
		t.Fatalf("expected pre-stream 401 refresh+replay 200, got code=%d updates=%d err=%v", w401.Code, repo401.updates, err)
	}

	// 用例 13：已开流后第 2 帧为内层 401 错误 -> 写出错误帧 + [DONE]，禁止重放，后台触发一次异步刷新
	var postStreamCalls int32
	mid401Stream := fmt.Sprintf("%s%s",
		makeQoderOuterFrame(200, map[string]any{"id": "c5", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "first chunk"}}}}),
		makeQoderOuterFrame(401, map[string]any{"code": 401, "message": "token expired mid-stream"}),
	)
	svcPost, accPost, repoPost := newTestQoderGateway(t, func(_ *http.Request) (*http.Response, error) {
		atomic.AddInt32(&postStreamCalls, 1)
		return &http.Response{StatusCode: http.StatusOK, Body: &readCloserBuffer{Reader: strings.NewReader(mid401Stream)}, Header: make(http.Header)}, nil
	}, nil)
	doneCh := make(chan struct{}, 1)
	svcPost.qoderAsyncRefreshHook = func() { doneCh <- struct{}{} }
	wPost := httptest.NewRecorder()
	cPost, _ := gin.CreateTestContext(wPost)
	_, _ = svcPost.forwardQoderChatCompletions(context.Background(), cPost, accPost, []byte(`{"model":"smodel","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for qoderAsyncRefreshHook")
	}
	if atomic.LoadInt32(&postStreamCalls) != 1 || repoPost.updates != 1 || !strings.Contains(wPost.Body.String(), "token expired mid-stream") || !strings.HasSuffix(wPost.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("expected 1 upstream call (no replay), 1 async refresh, error frame+[DONE], got calls=%d updates=%d body=%s", postStreamCalls, repoPost.updates, wPost.Body.String())
	}
}
