package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	DefaultQoderPlanURL       = "https://openapi.qoder.sh/api/v2/user/plan"
	DefaultQoderQuotaUsageURL = "https://openapi.qoder.sh/api/v2/quota/usage"
	qoderQuotaHTTPTimeout     = 15 * time.Second
)

var (
	qoderPlanURL         = DefaultQoderPlanURL
	qoderQuotaUsageURL   = DefaultQoderQuotaUsageURL
	qoderQuotaHTTPClient = http.DefaultClient
	qoderUsageCache      sync.Map           // accountID(int64) -> *antigravityUsageCache
	qoderUsageFlight     singleflight.Group // 防止同一 Qoder 账号并发积分探测击穿缓存
)

// QoderCredits 表示 Qoder 账号的积分额度快照。
type QoderCredits struct {
	Remaining int64  `json:"remaining"`
	Total     int64  `json:"total"`
	Plan      string `json:"plan,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// FetchQoderQuota 是包级纯只读函数：直接接收调用方显式传入的 accessToken，
// 请求 /api/v2/user/plan 并按需补查 /api/v2/quota/usage，绝不调用 qoderTokenProvider 或触发 refresh。
func FetchQoderQuota(ctx context.Context, accessToken string) (*QoderCredits, error) {
	token := strings.TrimSpace(accessToken)
	if token == "" {
		return nil, errors.New("qoder access_token is required")
	}
	planObj, err := fetchQoderJSONMap(ctx, qoderPlanURL, token)
	if err != nil {
		return nil, err
	}
	quotaObj := planObj
	if extractQoderSubMap(planObj, "userQuota") == nil {
		if usageObj, uErr := fetchQoderJSONMap(ctx, qoderQuotaUsageURL, token); uErr == nil && usageObj != nil {
			quotaObj = usageObj
		} else if uErr != nil {
			slog.Warn("qoder quota usage endpoint unavailable", "error", uErr)
		}
	}
	return buildQoderCredits(planObj, quotaObj), nil
}

// getQoderUsage 获取 Qoder 账号积分额度（值通道旁路，缓存与 singleflight 下沉至本文件）：
// 先经 qoderTokenProvider 获取有效 access_token，再调用 FetchQoderQuota；
// 任何失败仅记录 slog.Warn，返回含 UpdatedAt 的空 UsageInfo（QoderCredits: nil），不写 Error/ErrorCode。
func (s *AccountUsageService) getQoderUsage(ctx context.Context, account *Account) (*UsageInfo, error) {
	now := time.Now()
	empty := &UsageInfo{UpdatedAt: &now}
	if account == nil {
		return empty, nil
	}
	if cached, ok := qoderUsageCache.Load(account.ID); ok {
		if cache, ok := cached.(*antigravityUsageCache); ok && time.Since(cache.timestamp) < apiCacheTTL {
			return cache.usageInfo, nil
		}
	}
	flightKey := fmt.Sprintf("qoder-usage:%d", account.ID)
	result, flightErr, _ := qoderUsageFlight.Do(flightKey, func() (any, error) {
		if cached, ok := qoderUsageCache.Load(account.ID); ok {
			if cache, ok := cached.(*antigravityUsageCache); ok && time.Since(cache.timestamp) < apiCacheTTL {
				return cache.usageInfo, nil
			}
		}
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), qoderQuotaHTTPTimeout)
		defer cancel()
		token, err := s.resolveQoderAccessTokenForUsage(fetchCtx, account)
		if err != nil {
			slog.Warn("qoder usage resolve access token failed", "account_id", account.ID, "error", err)
			return empty, nil
		}
		credits, err := FetchQoderQuota(fetchCtx, token)
		if err != nil {
			slog.Warn("qoder fetch quota failed", "account_id", account.ID, "error", err)
			return empty, nil
		}
		ts := time.Now()
		info := &UsageInfo{UpdatedAt: &ts, QoderCredits: credits}
		qoderUsageCache.Store(account.ID, &antigravityUsageCache{usageInfo: info, timestamp: ts})
		return info, nil
	})
	if flightErr != nil {
		return empty, nil
	}
	if usage, ok := result.(*UsageInfo); ok && usage != nil {
		return usage, nil
	}
	return empty, nil
}

func (s *AccountUsageService) resolveQoderAccessTokenForUsage(ctx context.Context, account *Account) (string, error) {
	var provider *QoderTokenProvider
	if s == nil || s.accountRepo == nil {
		provider = NewQoderTokenProvider(nil, qoderQuotaHTTPClient, "")
	} else {
		provider = newQoderTokenProviderFromAccountRepo(s.accountRepo)
	}
	creds, err := provider.GetValidCredentials(ctx, account, false)
	if err != nil {
		return "", err
	}
	return creds.AccessToken, nil
}

func fetchQoderJSONMap(ctx context.Context, url, token string) (map[string]any, error) {
	reqCtx, cancel := context.WithTimeout(ctx, qoderQuotaHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	client := qoderQuotaHTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s -> HTTP %d", url, resp.StatusCode)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	if data, ok := parsed["data"].(map[string]any); ok {
		return data, nil
	}
	return parsed, nil
}

func buildQoderCredits(planObj, quotaObj map[string]any) *QoderCredits {
	userQuota := extractQoderSubMap(quotaObj, "userQuota")
	addOnQuota := extractQoderSubMap(quotaObj, "addOnQuota")
	if addOnQuota == nil {
		addOnQuota = extractQoderSubMap(planObj, "addOnQuota")
	}
	if userQuota == nil && addOnQuota == nil {
		return nil
	}
	uRem, uHasRem := extractQoderQuotaInt64(userQuota, "remaining")
	aRem, aHasRem := extractQoderQuotaInt64(addOnQuota, "remaining")
	if !uHasRem && !aHasRem {
		return nil
	}
	uTot, _ := extractQoderQuotaInt64(userQuota, "total")
	aTot, _ := extractQoderQuotaInt64(addOnQuota, "total")
	rem := uRem + aRem
	tot := uTot + aTot
	if tot < rem {
		tot = rem
	}
	planName := qoderFirstTrimNonEmpty(
		qoderCredString(planObj, "planName"),
		qoderCredString(planObj, "plan_tier_name"),
		qoderCredString(planObj, "plan"),
		qoderCredString(planObj, "name"),
	)
	expRaw := firstNonNilQoderVal(
		planObj["expiresAt"], planObj["expireTime"], planObj["end_date"],
		quotaObj["expiresAt"], mapValOrNil(userQuota, "expiresAt"),
	)
	return &QoderCredits{
		Remaining: rem,
		Total:     tot,
		Plan:      planName,
		ExpiresAt: normalizeQoderExpiresAt(expRaw),
	}
}

func extractQoderSubMap(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	sub, _ := m[key].(map[string]any)
	return sub
}

func mapValOrNil(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	return m[key]
}

func firstNonNilQoderVal(vals ...any) any {
	for _, v := range vals {
		if v != nil && strings.TrimSpace(fmt.Sprint(v)) != "" {
			return v
		}
	}
	return nil
}

func extractQoderQuotaInt64(m map[string]any, key string) (int64, bool) {
	if m == nil {
		return 0, false
	}
	v, exists := m[key]
	if !exists || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		val, err := n.Int64()
		return val, err == nil
	case string:
		val, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return val, err == nil
	default:
		return 0, false
	}
}

func normalizeQoderExpiresAt(raw any) string {
	if raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case float64:
		return formatQoderUnixTimestamp(int64(v))
	case int64:
		return formatQoderUnixTimestamp(v)
	case int:
		return formatQoderUnixTimestamp(int64(v))
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return formatQoderUnixTimestamp(n)
		}
	case string:
		s := strings.TrimSpace(v)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return formatQoderUnixTimestamp(n)
		}
		if len(s) >= 10 && s[4] == '-' && s[7] == '-' {
			return s[:10]
		}
		return s
	}
	return ""
}

func formatQoderUnixTimestamp(ts int64) string {
	if ts <= 0 {
		return ""
	}
	if ts > 1e11 {
		return time.UnixMilli(ts).UTC().Format("2006-01-02")
	}
	return time.Unix(ts, 0).UTC().Format("2006-01-02")
}
