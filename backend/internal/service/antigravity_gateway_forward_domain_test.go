//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// agDomainStubResponse 描述 stub 对某一 base URL 前缀的响应（优先级 = 声明顺序）。
type agDomainStubResponse struct {
	base       string // URL 前缀匹配
	statusCode int    // 0 时不设置状态码（配合 err 用）
	body       string
	err        error
}

// agDomainStubUpstream 按 base URL 前缀返回不同响应的转发桩（E1-E6 共用）。
type agDomainStubUpstream struct {
	responses []agDomainStubResponse
	calls     []string
}

var _ HTTPUpstream = (*agDomainStubUpstream)(nil)

func (s *agDomainStubUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	url := req.URL.String()
	s.calls = append(s.calls, url)
	for _, r := range s.responses {
		if strings.HasPrefix(url, r.base) {
			if r.err != nil {
				return nil, r.err
			}
			return &http.Response{
				StatusCode: r.statusCode,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(r.body)),
			}, nil
		}
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("ok")),
	}, nil
}

func (s *agDomainStubUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, accountConcurrency)
}

// antigravityDomainURLs 组装测试假 URL 序（oauth 序 = [prod, daily]，与 BaseURLs 全局一致）。
// resolveAntigravityForwardBaseURLs 对 paid 账号返回 [daily, prod]。
func antigravityDomainURLs() (dailyURL, prodURL string) {
	return "https://daily.example.test", "https://prod.example.test"
}

// setupAntigravityForwardDomainTest 保存/恢复 BaseURLs 与 DefaultURLAvailability，
// 并置空 forward base URL 环境变量（避免外部配置干扰域序解析）。
func setupAntigravityForwardDomainTest(t *testing.T, dailyURL, prodURL string) *stubAntigravityAccountRepo {
	t.Helper()
	t.Setenv(antigravityForwardBaseURLEnv, "")

	oldBaseURLs := append([]string(nil), antigravity.BaseURLs...)
	oldAvailability := antigravity.DefaultURLAvailability
	t.Cleanup(func() {
		antigravity.BaseURLs = oldBaseURLs
		antigravity.DefaultURLAvailability = oldAvailability
	})

	antigravity.BaseURLs = []string{prodURL, dailyURL}
	antigravity.DefaultURLAvailability = antigravity.NewURLAvailability(10 * time.Minute)

	return &stubAntigravityAccountRepo{}
}

// paidAntigravityAccount 返回 pro 档（paidTier）账号。
func paidAntigravityAccount() *Account {
	return &Account{
		ID:          1,
		Name:        "acc-1",
		Platform:    PlatformAntigravity,
		Schedulable: true,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{"plan_type": "pro"},
	}
}

// quotaExhausted429Body 个人配额真打满 429 响应（isPersonalQuotaExhausted429 命中）。
func quotaExhausted429Body() string {
	return `{"error":{"message":"Individual quota reached. Resets in 8m23.659899061s.","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"}]}}`
}

// urlLevel429Body URL 级泛化限流 429 响应（isURLLevelRateLimit 命中，无 capacity 文案）。
func urlLevel429Body() string {
	return `{"error":{"message":"Resource has been exhausted"}}`
}

// modelRateLimit429Body 模型级限流 429 响应（retryDelay >= 阈值 → shouldRateLimitModel → 账号切换信号）。
func modelRateLimit429Body() string {
	return `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","metadata":{"model":"gemini-3-pro"},"reason":"RATE_LIMIT_EXCEEDED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"60s"}]}}`
}

// connectionRefusedErr 构造可被 isAntigravityConnectionError 识别的连接错误。
func connectionRefusedErr() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
}

// noOpHandleError 记录是否被调用（用于断言 handleError 未被误触的用例）。
type noOpHandleError struct{ called bool }

func (h *noOpHandleError) fn(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
	h.called = true
	return nil
}

// TestAntigravityRetryLoop_PersonalQuotaExhausted_FallbackWithoutMarkUnavailable — E1：
// daily 真打满 429（个人配额耗尽）⇒ 换 prod 重试成功且 daily **不** MarkUnavailable
// （S15 ①：账号×域消耗非 URL 级故障，标记会跨账号污染可用性记忆）。
func TestAntigravityRetryLoop_PersonalQuotaExhausted_FallbackWithoutMarkUnavailable(t *testing.T) {
	dailyURL, prodURL := antigravityDomainURLs()
	repo := setupAntigravityForwardDomainTest(t, dailyURL, prodURL)

	upstream := &agDomainStubUpstream{responses: []agDomainStubResponse{
		{base: dailyURL, statusCode: http.StatusTooManyRequests, body: quotaExhausted429Body()}, // daily 真打满
		// prod 未命中 → 默认 200 OK
	}}

	handleErr := &noOpHandleError{}
	svc := &AntigravityGatewayService{accountRepo: repo}
	result, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
		prefix:       "[test]",
		ctx:          context.Background(),
		account:      paidAntigravityAccount(),
		proxyURL:     "",
		accessToken:  "token",
		action:       "generateContent",
		body:         []byte(`{"input":"test"}`),
		httpUpstream: upstream,
		accountRepo:  repo,
		handleError:  handleErr.fn,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.resp)
	defer func() { _ = result.resp.Body.Close() }()
	require.Equal(t, http.StatusOK, result.resp.StatusCode)

	// 换 prod 成功：先 daily 后 prod
	require.Len(t, upstream.calls, 2)
	require.True(t, strings.HasPrefix(upstream.calls[0], dailyURL))
	require.True(t, strings.HasPrefix(upstream.calls[1], prodURL))

	// E1 核心断言：daily **无**不可用标记（判据①不 MarkUnavailable）
	require.True(t, antigravity.DefaultURLAvailability.IsAvailable(dailyURL), "daily should NOT be marked unavailable after personal quota exhausted")
	require.False(t, handleErr.called)
}

// TestAntigravityRetryLoop_URLLevelRateLimit_FallbackWithMarkUnavailable — E2：
// daily 域级泛化 429（"Resource has been exhausted"）⇒ 换 prod 成功且 daily **被** MarkUnavailable
// （S15 ②：URL 级限流信号）。
func TestAntigravityRetryLoop_URLLevelRateLimit_FallbackWithMarkUnavailable(t *testing.T) {
	dailyURL, prodURL := antigravityDomainURLs()
	repo := setupAntigravityForwardDomainTest(t, dailyURL, prodURL)

	upstream := &agDomainStubUpstream{responses: []agDomainStubResponse{
		{base: dailyURL, statusCode: http.StatusTooManyRequests, body: urlLevel429Body()}, // daily URL 级限流
	}}

	svc := &AntigravityGatewayService{accountRepo: repo}
	result, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
		prefix:       "[test]",
		ctx:          context.Background(),
		account:      paidAntigravityAccount(),
		proxyURL:     "",
		accessToken:  "token",
		action:       "generateContent",
		body:         []byte(`{"input":"test"}`),
		httpUpstream: upstream,
		accountRepo:  repo,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.resp)
	defer func() { _ = result.resp.Body.Close() }()
	require.Equal(t, http.StatusOK, result.resp.StatusCode)

	// 换 prod 成功
	require.Len(t, upstream.calls, 2)
	require.True(t, strings.HasPrefix(upstream.calls[0], dailyURL))
	require.True(t, strings.HasPrefix(upstream.calls[1], prodURL))

	// E2 核心断言：daily **被** MarkUnavailable（判据②）
	require.False(t, antigravity.DefaultURLAvailability.IsAvailable(dailyURL), "daily should be marked unavailable after URL-level rate limit")
	require.True(t, antigravity.DefaultURLAvailability.IsAvailable(prodURL))
}

// TestAntigravityRetryLoop_ConnectionError_Fallback — E3：
// daily 连接错误 ⇒ 既有连接错误分支（:578-581）换 prod 成功（不新增分支、不 MarkUnavailable）。
func TestAntigravityRetryLoop_ConnectionError_Fallback(t *testing.T) {
	dailyURL, prodURL := antigravityDomainURLs()
	repo := setupAntigravityForwardDomainTest(t, dailyURL, prodURL)

	upstream := &agDomainStubUpstream{responses: []agDomainStubResponse{
		{base: dailyURL, err: connectionRefusedErr()}, // daily 连接错误
		// prod 未命中 → 默认 200 OK
	}}

	svc := &AntigravityGatewayService{accountRepo: repo}
	result, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
		prefix:       "[test]",
		ctx:          context.Background(),
		account:      paidAntigravityAccount(),
		proxyURL:     "",
		accessToken:  "token",
		action:       "generateContent",
		body:         []byte(`{"input":"test"}`),
		httpUpstream: upstream,
		accountRepo:  repo,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.resp)
	defer func() { _ = result.resp.Body.Close() }()
	require.Equal(t, http.StatusOK, result.resp.StatusCode)

	// 连接错误分支换 prod 成功
	require.Len(t, upstream.calls, 2)
	require.True(t, strings.HasPrefix(upstream.calls[0], dailyURL))
	require.True(t, strings.HasPrefix(upstream.calls[1], prodURL))

	// 连接错误分支不产生不可用记忆（既有行为）
	require.True(t, antigravity.DefaultURLAvailability.IsAvailable(dailyURL))
}

// TestAntigravityRetryLoop_BothDomainsFail_SwitchAccount — E4：
// paid 两域尝试序内均 429（daily 真打满 → 换 prod；prod 模型限流，最后一域）⇒
// prod 不命中判据①②（urlIdx == len-1）→ 落既有 default → 账号切换信号（原语义）。
func TestAntigravityRetryLoop_BothDomainsFail_SwitchAccount(t *testing.T) {
	dailyURL, prodURL := antigravityDomainURLs()
	repo := setupAntigravityForwardDomainTest(t, dailyURL, prodURL)

	upstream := &agDomainStubUpstream{responses: []agDomainStubResponse{
		{base: dailyURL, statusCode: http.StatusTooManyRequests, body: quotaExhausted429Body()}, // daily 真打满 → 换 prod
		{base: prodURL, statusCode: http.StatusTooManyRequests, body: modelRateLimit429Body()},  // prod 模型限流（最后一域）
	}}

	handleErr := &noOpHandleError{}
	svc := &AntigravityGatewayService{accountRepo: repo}
	result, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
		prefix:       "[test]",
		ctx:          context.Background(),
		account:      paidAntigravityAccount(),
		proxyURL:     "",
		accessToken:  "token",
		action:       "generateContent",
		body:         []byte(`{"input":"test"}`),
		httpUpstream: upstream,
		accountRepo:  repo,
		handleError:  handleErr.fn,
	})

	// 两域耗尽 → 账号切换信号（既有语义）
	require.Error(t, err)
	var switchErr *AntigravityAccountSwitchError
	require.ErrorAs(t, err, &switchErr)
	require.Nil(t, result)

	// 两域各被尝试一次
	require.Len(t, upstream.calls, 2)
	require.True(t, strings.HasPrefix(upstream.calls[0], dailyURL))
	require.True(t, strings.HasPrefix(upstream.calls[1], prodURL))

	// prod 最后一域走模型限流分支 → repo 收到 SetModelRateLimit 调用，handleError 不触发
	require.GreaterOrEqual(t, len(repo.modelRateLimitCalls), 1)
	require.False(t, handleErr.called)
}

// TestAntigravityRetryLoop_FreeTier_SingleProd_NoDomainFallback — E5：
// free（非 paid 档）账号 ⇒ 域序坍缩为 [prod] 单元素 ⇒ 429 不换域，直接落既有账号切换线（零回归）。
func TestAntigravityRetryLoop_FreeTier_SingleProd_NoDomainFallback(t *testing.T) {
	dailyURL, prodURL := antigravityDomainURLs()
	repo := setupAntigravityForwardDomainTest(t, dailyURL, prodURL)

	upstream := &agDomainStubUpstream{responses: []agDomainStubResponse{
		{base: prodURL, statusCode: http.StatusTooManyRequests, body: modelRateLimit429Body()}, // prod 模型限流
		// daily 不应被访问
	}}

	freeAccount := &Account{
		ID:          2,
		Name:        "acc-free",
		Platform:    PlatformAntigravity,
		Schedulable: true,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{"plan_type": "free"},
	}

	svc := &AntigravityGatewayService{accountRepo: repo}
	result, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
		prefix:       "[test]",
		ctx:          context.Background(),
		account:      freeAccount,
		proxyURL:     "",
		accessToken:  "token",
		action:       "generateContent",
		body:         []byte(`{"input":"test"}`),
		httpUpstream: upstream,
		accountRepo:  repo,
	})

	// 单元素序内 429 → 账号切换信号（既有语义，不换域）
	require.Error(t, err)
	var switchErr *AntigravityAccountSwitchError
	require.ErrorAs(t, err, &switchErr)
	require.Nil(t, result)

	// 只访问 prod，daily 零访问（零回归：free 档不做双域）
	require.NotEmpty(t, upstream.calls)
	for _, callURL := range upstream.calls {
		require.True(t, strings.HasPrefix(callURL, prodURL), "free tier must only hit prod, got %s", callURL)
		require.False(t, strings.HasPrefix(callURL, dailyURL))
	}
}

// TestAntigravityRetryLoop_AllMarkedUnavailable_GuardFallsBackToUnfiltered — E6：
// daily + prod 双标记并存（探测记忆过滤结果为空）⇒ 空集守卫回退未过滤序 [daily, prod] 续试，
// 不再零次尝试 / 无 {resp: nil} 解引用；两域仍败 ⇒ 落既有账号切换线。
func TestAntigravityRetryLoop_AllMarkedUnavailable_GuardFallsBackToUnfiltered(t *testing.T) {
	dailyURL, prodURL := antigravityDomainURLs()
	repo := setupAntigravityForwardDomainTest(t, dailyURL, prodURL)

	// 预置双标记：daily + prod 均 MarkUnavailable（TTL 未过 ⇒ 过滤结果为空）
	antigravity.DefaultURLAvailability.MarkUnavailable(dailyURL)
	antigravity.DefaultURLAvailability.MarkUnavailable(prodURL)
	require.Empty(t, antigravity.DefaultURLAvailability.GetAvailableURLsWithBase([]string{dailyURL, prodURL}))

	upstream := &agDomainStubUpstream{responses: []agDomainStubResponse{
		{base: dailyURL, statusCode: http.StatusTooManyRequests, body: quotaExhausted429Body()}, // daily 真打满 → 换 prod
		{base: prodURL, statusCode: http.StatusTooManyRequests, body: modelRateLimit429Body()},  // prod 模型限流（最后一域）
	}}

	svc := &AntigravityGatewayService{accountRepo: repo}
	result, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
		prefix:       "[test]",
		ctx:          context.Background(),
		account:      paidAntigravityAccount(),
		proxyURL:     "",
		accessToken:  "token",
		action:       "generateContent",
		body:         []byte(`{"input":"test"}`),
		httpUpstream: upstream,
		accountRepo:  repo,
	})

	// 过滤为空时仍至少尝试一轮（守卫回退未过滤序）→ 无 {resp: nil} 解引用
	require.NotEmpty(t, upstream.calls, "guard fallback must still attempt requests")
	require.True(t, strings.HasPrefix(upstream.calls[0], dailyURL))

	// 两域仍败 → 账号切换信号（既有语义）
	require.Error(t, err)
	var switchErr *AntigravityAccountSwitchError
	require.ErrorAs(t, err, &switchErr)
	require.Nil(t, result)
}
