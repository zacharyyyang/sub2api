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
// resolveAntigravityForwardBaseURLs 对任意消费级账号返回 [daily] 单域。
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

// TestAntigravityRetryLoop_PersonalQuotaExhausted_DirectSwitch — E1（TC-10）：
// 消费级账号默认序 [daily] 单域，daily 真打满 429（个人配额耗尽）⇒ 不换域、不 MarkUnavailable，
// 直接返回账号切换信号（S15 ①：账号×域消耗非 URL 级故障，标记会跨账号污染可用性记忆）。
func TestAntigravityRetryLoop_PersonalQuotaExhausted_DirectSwitch(t *testing.T) {
	dailyURL, prodURL := antigravityDomainURLs()
	repo := setupAntigravityForwardDomainTest(t, dailyURL, prodURL)

	upstream := &agDomainStubUpstream{responses: []agDomainStubResponse{
		{base: dailyURL, statusCode: http.StatusTooManyRequests, body: quotaExhausted429Body()}, // daily 真打满
		// prod 不应被访问（单域序无第二跳）
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

	// 单元素序 429 直切：账号切换信号，无响应对象
	require.Error(t, err)
	var switchErr *AntigravityAccountSwitchError
	require.ErrorAs(t, err, &switchErr)
	require.Equal(t, int64(1), switchErr.OriginalAccountID)
	require.Nil(t, result)

	// ① 目的域：仅 daily 一次调用；② prod 调用次数 = 0
	require.Len(t, upstream.calls, 1)
	require.True(t, strings.HasPrefix(upstream.calls[0], dailyURL))
	require.False(t, strings.HasPrefix(upstream.calls[0], prodURL))

	// ④ daily **无**不可用标记（账号级消耗非 URL 级故障，判据①不 MarkUnavailable）
	require.True(t, antigravity.DefaultURLAvailability.IsAvailable(dailyURL), "daily should NOT be marked unavailable after personal quota exhausted")
	require.False(t, handleErr.called)
}

// TestAntigravityRetryLoop_URLLevelRateLimit_DirectSwitch — E2（TC-11）：
// 消费级账号默认序 [daily] 单域，daily 域级泛化 429（"Resource has been exhausted"）⇒
// 单元素序无下一域（判据②换域条件 urlIdx < len-1 不成立）⇒ 不换域、不 MarkUnavailable，直切账号。
func TestAntigravityRetryLoop_URLLevelRateLimit_DirectSwitch(t *testing.T) {
	dailyURL, prodURL := antigravityDomainURLs()
	repo := setupAntigravityForwardDomainTest(t, dailyURL, prodURL)

	upstream := &agDomainStubUpstream{responses: []agDomainStubResponse{
		{base: dailyURL, statusCode: http.StatusTooManyRequests, body: urlLevel429Body()}, // daily URL 级限流
		// prod 不应被访问（单域序无第二跳）
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

	// 单元素序 429 直切：账号切换信号，无响应对象
	require.Error(t, err)
	var switchErr *AntigravityAccountSwitchError
	require.ErrorAs(t, err, &switchErr)
	require.Equal(t, int64(1), switchErr.OriginalAccountID)
	require.Nil(t, result)

	// ① 目的域：仅 daily 一次调用；② prod 调用次数 = 0
	require.Len(t, upstream.calls, 1)
	require.True(t, strings.HasPrefix(upstream.calls[0], dailyURL))
	require.False(t, strings.HasPrefix(upstream.calls[0], prodURL))

	// ④ daily **无**不可用标记（判据②的标记需 urlIdx < len-1，单域下不标记）
	require.True(t, antigravity.DefaultURLAvailability.IsAvailable(dailyURL), "daily should NOT be marked unavailable (single-domain, no next URL)")
}

// TestAntigravityRetryLoop_ConnectionError_SingleDomainRetriesExhausted — E3：
// 消费级账号默认序 [daily] 单域，daily 连接错误 ⇒ 无下一域可换（urlIdx < len-1 不成立），
// 走既有单域退避重试（antigravityMaxRetries=3，约 3s：1s + 2s），耗尽后返回重试失败硬错
// （设计 §2.2：503 / 连接错误 → 走既有单域退避重试或失败转移，单域下即退避重试）。
func TestAntigravityRetryLoop_ConnectionError_SingleDomainRetriesExhausted(t *testing.T) {
	dailyURL, prodURL := antigravityDomainURLs()
	repo := setupAntigravityForwardDomainTest(t, dailyURL, prodURL)

	upstream := &agDomainStubUpstream{responses: []agDomainStubResponse{
		{base: dailyURL, err: connectionRefusedErr()}, // daily 连接错误
		// prod 不应被访问（单域序无第二跳）
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

	// 单域退避重试耗尽 → 重试失败硬错
	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream request failed after retries")
	require.Nil(t, result)

	// 仅 daily 三次尝试（antigravityMaxRetries）；② prod 调用次数 = 0
	require.Len(t, upstream.calls, 3)
	for _, callURL := range upstream.calls {
		require.True(t, strings.HasPrefix(callURL, dailyURL), "single domain must only hit daily, got %s", callURL)
	}
	require.False(t, strings.HasPrefix(upstream.calls[0], prodURL))

	// 连接错误分支不产生不可用记忆（既有行为）
	require.True(t, antigravity.DefaultURLAvailability.IsAvailable(dailyURL))
}

// TestAntigravityRetryLoop_ModelRateLimit_DirectSwitch — E4：
// 消费级账号默认序 [daily] 单域，daily 模型级限流（retryDelay 60s ⇒ 情况1 限流模型并切换账号）：
// 不命中判据①②③（非个人配额 / URL 级），单域下同样落既有模型限流分支（原语义）。
func TestAntigravityRetryLoop_ModelRateLimit_DirectSwitch(t *testing.T) {
	dailyURL, prodURL := antigravityDomainURLs()
	repo := setupAntigravityForwardDomainTest(t, dailyURL, prodURL)

	upstream := &agDomainStubUpstream{responses: []agDomainStubResponse{
		{base: dailyURL, statusCode: http.StatusTooManyRequests, body: modelRateLimit429Body()}, // daily 模型级限流
		// prod 不应被访问（单域序无第二跳）
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

	// 模型级限流（非判据①②③）→ 账号切换信号（既有语义）
	require.Error(t, err)
	var switchErr *AntigravityAccountSwitchError
	require.ErrorAs(t, err, &switchErr)
	require.Nil(t, result)

	// ① 目的域：仅 daily 一次调用；② prod 调用次数 = 0
	require.Len(t, upstream.calls, 1)
	require.True(t, strings.HasPrefix(upstream.calls[0], dailyURL))
	require.False(t, strings.HasPrefix(upstream.calls[0], prodURL))

	// 模型限流分支 → repo 收到 SetModelRateLimit 调用，handleError 不触发；daily 无不可用标记
	require.GreaterOrEqual(t, len(repo.modelRateLimitCalls), 1)
	require.False(t, handleErr.called)
	require.True(t, antigravity.DefaultURLAvailability.IsAvailable(dailyURL))
}

// TestAntigravityRetryLoop_FreeTier_SingleDaily_SwitchAccount — E5：
// free 账号默认序同样坍缩为 [daily] 单域（TC-03）⇒ daily 模型级限流走既有模型限流线切号，
// prod 零访问（free 档同样不做双域）。
func TestAntigravityRetryLoop_FreeTier_SingleDaily_SwitchAccount(t *testing.T) {
	dailyURL, prodURL := antigravityDomainURLs()
	repo := setupAntigravityForwardDomainTest(t, dailyURL, prodURL)

	upstream := &agDomainStubUpstream{responses: []agDomainStubResponse{
		{base: dailyURL, statusCode: http.StatusTooManyRequests, body: modelRateLimit429Body()}, // daily 模型限流
		// prod 不应被访问
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

	// 模型级限流 → 账号切换信号（既有语义）
	require.Error(t, err)
	var switchErr *AntigravityAccountSwitchError
	require.ErrorAs(t, err, &switchErr)
	require.Nil(t, result)

	// 只访问 daily，prod 零访问（free 档不做双域，默认序同为 daily）
	require.NotEmpty(t, upstream.calls)
	for _, callURL := range upstream.calls {
		require.True(t, strings.HasPrefix(callURL, dailyURL), "free tier must only hit daily, got %s", callURL)
		require.False(t, strings.HasPrefix(callURL, prodURL))
	}
}

// TestAntigravityRetryLoop_AllMarkedUnavailable_GuardFallsBackToUnfiltered — E6：
// daily + prod 双标记并存（探测记忆过滤结果为空）⇒ 空集守卫回退未过滤序 [daily] 续试，
// 不再零次尝试 / 无 {resp: nil} 解引用；daily 真打满 429 ⇒ 单域直切账号切换信号。
func TestAntigravityRetryLoop_AllMarkedUnavailable_GuardFallsBackToUnfiltered(t *testing.T) {
	dailyURL, prodURL := antigravityDomainURLs()
	repo := setupAntigravityForwardDomainTest(t, dailyURL, prodURL)

	// 预置双标记：daily + prod 均 MarkUnavailable（TTL 未过 ⇒ 过滤结果为空）
	antigravity.DefaultURLAvailability.MarkUnavailable(dailyURL)
	antigravity.DefaultURLAvailability.MarkUnavailable(prodURL)
	require.Empty(t, antigravity.DefaultURLAvailability.GetAvailableURLsWithBase([]string{dailyURL, prodURL}))
	require.Empty(t, antigravity.DefaultURLAvailability.GetAvailableURLsWithBase([]string{dailyURL}))

	upstream := &agDomainStubUpstream{responses: []agDomainStubResponse{
		{base: dailyURL, statusCode: http.StatusTooManyRequests, body: quotaExhausted429Body()}, // daily 真打满
		// prod 不应被访问（单域序无第二跳）
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

	// 单域直切：仅 daily 一次尝试（② prod 调用次数 = 0）→ 账号切换信号
	require.Len(t, upstream.calls, 1)
	require.False(t, strings.HasPrefix(upstream.calls[0], prodURL))
	require.Error(t, err)
	var switchErr *AntigravityAccountSwitchError
	require.ErrorAs(t, err, &switchErr)
	require.Nil(t, result)
}
