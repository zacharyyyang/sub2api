package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"golang.org/x/sync/singleflight"
)

const (
	mimoQuotaHTTPTimeout = 12 * time.Second
	mimoUsageCacheTTL    = 3 * time.Minute
	mimoUsageNegCacheTTL = 30 * time.Second
	mimoDefaultPlanTitle = "MiMo 会员"
)

var (
	mimoQuotaHTTPClient      = http.DefaultClient
	mimoUsageFlight          singleflight.Group
	errMimoQuotaUnauthorized = errors.New("mimo quota endpoint returned HTTP 401")
)

type mimoUsageCacheEntry struct {
	usageInfo *UsageInfo
	timestamp time.Time
	isFailure bool
}

type MimoQuotaInfo struct {
	PlanTitle        string   `json:"plan_title,omitempty"`
	PlanCode         string   `json:"plan_code,omitempty"`
	PlanTier         int      `json:"plan_tier,omitempty"`
	RemainingPercent float64  `json:"remaining_percent"`
	UsedPercent      float64  `json:"used_percent"`
	FiveHourUsedPct  *float64 `json:"five_hour_used_pct,omitempty"`
	WeeklyUsedPct    *float64 `json:"weekly_used_pct,omitempty"`
	ResetAt          string   `json:"reset_at,omitempty"`
	ResetSec         *int     `json:"reset_sec,omitempty"`
	CooldownEndTime  string   `json:"cooldown_end_time,omitempty"`
}

func FetchMimoQuota(ctx context.Context, creds MimoCredentials, proxyURL ...string) (*MimoQuotaInfo, error) {
	EnsureMimoDerivedFields(&creds)
	if strings.TrimSpace(creds.UserID) == "" || strings.TrimSpace(creds.ServiceToken) == "" {
		return nil, errors.New("mimo user_id and service_token are required for quota fetch")
	}
	pURL := qoderFirstTrimNonEmpty(proxyURL...)
	usageData, status, err := fetchMimoQuotaJSON(ctx, creds, creds.BaseURL+"/api/user/usage", pURL)
	if status == http.StatusUnauthorized {
		return nil, errMimoQuotaUnauthorized
	}
	if err != nil {
		return nil, err
	}
	subData, _, _ := fetchMimoQuotaJSON(ctx, creds, creds.BaseURL+"/api/user/xiaomi/subscription/self", pURL)
	return buildMimoQuotaInfo(usageData, subData), nil
}

func fetchMimoQuotaJSON(ctx context.Context, creds MimoCredentials, urlStr, proxyURL string) (map[string]any, int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, mimoQuotaHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", mimoUserAgent)
	req.Header.Set("x-mimo-source", mimoSourceHeader)
	req.Header.Set("Cookie", BuildMimoInferenceCookie(creds))
	client := mimoQuotaHTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	if proxyURL != "" && client == http.DefaultClient {
		if proxyClient, pErr := httpclient.GetClient(httpclient.Options{ProxyURL: proxyURL, Timeout: mimoQuotaHTTPTimeout}); pErr == nil && proxyClient != nil {
			client = proxyClient
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, resp.StatusCode, fmt.Errorf("%s -> HTTP %d", urlStr, resp.StatusCode)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, resp.StatusCode, err
	}
	if data, ok := parsed["data"].(map[string]any); ok {
		return data, resp.StatusCode, nil
	}
	return parsed, resp.StatusCode, nil
}

func buildMimoQuotaInfo(usageObj, subObj map[string]any) *MimoQuotaInfo {
	if len(usageObj) == 0 && len(subObj) == 0 {
		return nil
	}
	currentSub, _ := subObj["current"].(map[string]any)
	if currentSub == nil {
		currentSub = subObj
	}
	planTitle := qoderFirstTrimNonEmpty(mimoCredString(currentSub, "title"), mimoCredString(currentSub, "planTitle"), mimoDefaultPlanTitle)
	planCode := qoderFirstTrimNonEmpty(mimoCredString(currentSub, "planCode"), mimoCredString(currentSub, "plan_code"))
	planTier := qoderAnyToInt(firstNonNilQoderVal(mapValOrNil(currentSub, "tier"), mapValOrNil(currentSub, "planTier")))
	var fiveHourPtr, weeklyPtr *float64
	if fh, ok := parseMimoRatioOrPercent(usageObj, "fiveHourUsageRatio", "five_hour_used_pct"); ok {
		fiveHourPtr = &fh
	}
	if wk, ok := parseMimoRatioOrPercent(usageObj, "weeklyUsageRatio", "weekly_used_pct"); ok {
		weeklyPtr = &wk
	}
	var remPct, usedPct float64
	if pct, ok := extractMimoFloat64(usageObj, "percent"); ok {
		remPct, usedPct = clampMimoPercent(pct), clampMimoPercent(100-clampMimoPercent(pct))
	} else if fiveHourPtr != nil {
		usedPct, remPct = clampMimoPercent(*fiveHourPtr), clampMimoPercent(100-clampMimoPercent(*fiveHourPtr))
	} else if remRaw, ok := extractMimoFloat64(usageObj, "remaining_percent"); ok {
		remPct, usedPct = clampMimoPercent(remRaw), clampMimoPercent(100-clampMimoPercent(remRaw))
	}
	resetAt := normalizeMimoResetAt(firstNonNilQoderVal(mapValOrNil(currentSub, "nextResetTime"), mapValOrNil(usageObj, "resetAt"), mapValOrNil(usageObj, "resetDate")))
	var resetSecPtr *int
	if sec, ok := extractQoderQuotaInt64(usageObj, "resetSec"); ok && sec >= 0 {
		s := int(sec)
		resetSecPtr = &s
	}
	return &MimoQuotaInfo{
		PlanTitle: planTitle, PlanCode: planCode, PlanTier: planTier,
		RemainingPercent: remPct, UsedPercent: usedPct,
		FiveHourUsedPct: fiveHourPtr, WeeklyUsedPct: weeklyPtr,
		ResetAt: resetAt, ResetSec: resetSecPtr,
		CooldownEndTime: strings.TrimSpace(mimoCredString(usageObj, "cooldownEndTime")),
	}
}

func (s *AccountUsageService) getMimoUsage(ctx context.Context, account *Account) (*UsageInfo, error) {
	now := time.Now()
	empty := &UsageInfo{UpdatedAt: &now}
	if account == nil {
		return empty, nil
	}
	if cached, hit := checkMimoUsageCache(account.ID); hit {
		return cached, nil
	}
	result, flightErr, _ := mimoUsageFlight.Do(fmt.Sprintf("mimo-usage:%d", account.ID), func() (any, error) {
		if cached, hit := checkMimoUsageCache(account.ID); hit {
			return cached, nil
		}
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mimoQuotaHTTPTimeout)
		defer cancel()
		provider := NewMimoTokenProvider(nil, mimoQuotaHTTPClient, "")
		if s != nil && s.accountRepo != nil {
			provider = newMimoTokenProviderFromAccountRepo(s.accountRepo)
		}
		creds, err := provider.GetValidCredentials(fetchCtx, account, false)
		if err != nil {
			slog.Warn("mimo usage resolve credentials failed", "account_id", account.ID, "error", err)
			mimoUsageCache.Store(account.ID, &mimoUsageCacheEntry{usageInfo: empty, timestamp: time.Now(), isFailure: true})
			return empty, nil
		}
		quota, err := fetchMimoQuotaWith401Retry(fetchCtx, provider, account, creds)
		if err != nil || quota == nil {
			slog.Warn("mimo fetch quota failed", "account_id", account.ID, "error", err)
			mimoUsageCache.Store(account.ID, &mimoUsageCacheEntry{usageInfo: empty, timestamp: time.Now(), isFailure: true})
			return empty, nil
		}
		info := buildMimoUsageInfoSnapshot(quota, time.Now())
		mimoUsageCache.Store(account.ID, &mimoUsageCacheEntry{usageInfo: info, timestamp: *info.UpdatedAt})
		return info, nil
	})
	if flightErr == nil {
		if usage, ok := result.(*UsageInfo); ok && usage != nil {
			return usage, nil
		}
	}
	return empty, nil
}

func fetchMimoQuotaWith401Retry(ctx context.Context, provider *MimoTokenProvider, account *Account, creds MimoCredentials) (*MimoQuotaInfo, error) {
	var proxyURL string
	if account != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	quota, err := FetchMimoQuota(ctx, creds, proxyURL)
	if errors.Is(err, errMimoQuotaUnauthorized) && strings.TrimSpace(creds.PassToken) != "" {
		refreshed, refErr := provider.GetValidCredentials(ctx, account, true)
		if refErr != nil {
			return nil, fmt.Errorf("mimo sso token refresh failed: %w", refErr)
		}
		return FetchMimoQuota(ctx, refreshed, proxyURL)
	}
	return quota, err
}

func checkMimoUsageCache(accountID int64) (*UsageInfo, bool) {
	if cached, ok := mimoUsageCache.Load(accountID); ok {
		if entry, ok := cached.(*mimoUsageCacheEntry); ok && entry != nil {
			ttl := map[bool]time.Duration{false: mimoUsageCacheTTL, true: mimoUsageNegCacheTTL}[entry.isFailure]
			if time.Since(entry.timestamp) < ttl {
				return entry.usageInfo, true
			}
		}
	}
	return nil, false
}

func buildMimoUsageInfoSnapshot(quota *MimoQuotaInfo, ts time.Time) *UsageInfo {
	info := &UsageInfo{UpdatedAt: &ts, MimoQuota: quota, SubscriptionTierRaw: quota.PlanTitle}
	var resetTimePtr *time.Time
	if quota.ResetAt != "" {
		if t, err := time.Parse(time.RFC3339, quota.ResetAt); err == nil {
			resetTimePtr = &t
		}
	}
	used5h := quota.UsedPercent
	if quota.FiveHourUsedPct != nil {
		used5h = *quota.FiveHourUsedPct
	}
	info.FiveHour = &UsageProgress{Utilization: used5h, ResetsAt: resetTimePtr}
	if quota.ResetSec != nil {
		info.FiveHour.RemainingSeconds = *quota.ResetSec
	}
	if quota.WeeklyUsedPct != nil {
		info.SevenDay = &UsageProgress{Utilization: *quota.WeeklyUsedPct, ResetsAt: resetTimePtr}
	}
	return info
}

func (s *AccountService) testMimoAccountConnection(ctx context.Context, account *Account) error {
	creds, err := ParseMimoCredentials(account.Credentials)
	if err != nil {
		return err
	}
	smokeCtx, cancel := context.WithTimeout(ctx, mimoQuotaHTTPTimeout)
	defer cancel()
	provider := NewMimoTokenProvider(nil, mimoQuotaHTTPClient, "")
	if s != nil && s.accountRepo != nil {
		provider = newMimoTokenProviderFromAccountRepo(s.accountRepo)
	}
	activeCreds := *creds
	if strings.TrimSpace(activeCreds.ServiceToken) == "" {
		exchanged, exErr := provider.GetValidCredentials(smokeCtx, account, true)
		if exErr != nil {
			return fmt.Errorf("mimo sso token exchange failed: %w", exErr)
		}
		activeCreds = exchanged
	}
	if _, err := fetchMimoQuotaWith401Retry(smokeCtx, provider, account, activeCreds); err != nil {
		return fmt.Errorf("mimo quota smoke test failed: %w", err)
	}
	return nil
}

func extractMimoFloat64(m map[string]any, key string) (float64, bool) {
	if m == nil || m[key] == nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(fmt.Sprint(m[key])), 64)
	return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

func parseMimoRatioOrPercent(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if v, ok := extractMimoFloat64(m, k); ok {
			if v > 0 && v <= 1.0 && strings.Contains(strings.ToLower(k), "ratio") {
				v *= 100
			}
			return clampMimoPercent(v), true
		}
	}
	return 0, false
}

func clampMimoPercent(v float64) float64 { return math.Min(math.Max(math.Round(v*10)/10, 0), 100) }

func normalizeMimoResetAt(raw any) string {
	s := strings.TrimSpace(mimoCredString(map[string]any{"v": raw}, "v"))
	if ts, err := strconv.ParseInt(s, 10, 64); err == nil && ts > 0 {
		return map[bool]time.Time{true: time.UnixMilli(ts), false: time.Unix(ts, 0)}[ts > 1e11].UTC().Format(time.RFC3339)
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return s
}
