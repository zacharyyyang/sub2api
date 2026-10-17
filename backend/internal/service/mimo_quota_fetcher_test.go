package service

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMimoQuotaFetcherAndTestCredentials 覆盖设计 §6 用例 8（操作包级共享缓存，禁止加 t.Parallel()）：
// ① /api/user/usage 返 percent:82.5 + 订阅信息；
// ② TestCredentials 按需 SSO 换票后拉取配额；
// ③ 调 InvalidateMimoToken(id) 清除共享正负缓存与 mimoUsageCache。
func TestMimoQuotaFetcherAndTestCredentials(t *testing.T) {
	const (
		fakeNonce     = "9876543210123456789"
		fakeSsecurity = "dGVzdF9zc2VjdXJpdHlfa2V5"
	)
	var usageCalls, subCalls, ssoCalls int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/usage":
			atomic.AddInt32(&usageCalls, 1)
			if !strings.Contains(r.Header.Get("Cookie"), "serviceToken=st_valid_quota") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"percent":            82.5,
					"fiveHourUsageRatio": 0.175,
					"weeklyUsageRatio":   0.35,
					"resetSec":           3600,
				},
			})
		case "/api/user/xiaomi/subscription/self":
			atomic.AddInt32(&subCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"current": map[string]any{
						"title":         "MiMo Pro 会员",
						"planCode":      "mimo_pro",
						"tier":          2,
						"nextResetTime": "2026-10-18T00:00:00Z",
					},
				},
			})
		case "/pass/serviceLogin":
			atomic.AddInt32(&ssoCalls, 1)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = fmt.Fprintf(w, "&&&START&&&{\"code\":0,\"location\":\"%s/sts?sid=mimopc\",\"nonce\":%s,\"ssecurity\":\"%s\",\"cUserId\":\"cuid_q\"}", srv.URL, fakeNonce, fakeSsecurity)
		case "/sts":
			sum := sha1.Sum([]byte(fmt.Sprintf("nonce=%s&%s", fakeNonce, fakeSsecurity)))
			if r.URL.Query().Get("clientSign") != base64.StdEncoding.EncodeToString(sum[:]) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "serviceToken", Value: "st_valid_quota", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "cUserId", Value: "cuid_q", Path: "/"})
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	origClient := mimoQuotaHTTPClient
	mimoQuotaHTTPClient = srv.Client()
	defer func() { mimoQuotaHTTPClient = origClient }()

	// ① FetchMimoQuota 直接解析 percent:82.5 -> RemainingPercent==82.5, UsedPercent==17.5
	quota, err := FetchMimoQuota(context.Background(), MimoCredentials{UserID: "100000001", ServiceToken: "st_valid_quota", BaseURL: srv.URL})
	if err != nil || quota == nil || quota.RemainingPercent != 82.5 || quota.UsedPercent != 17.5 || quota.PlanTitle != "MiMo Pro 会员" || quota.PlanCode != "mimo_pro" || quota.PlanTier != 2 || quota.ResetAt != "2026-10-18T00:00:00Z" || quota.FiveHourUsedPct == nil || *quota.FiveHourUsedPct != 17.5 || quota.WeeklyUsedPct == nil || *quota.WeeklyUsedPct != 35.0 {
		t.Fatalf("unexpected FetchMimoQuota result: %+v err=%v", quota, err)
	}

	// ② TestCredentials & ③ InvalidateMimoToken(id) 缓存清除
	verifyMimoAccountServiceAndInvalidation(t, srv, &ssoCalls)
}

func verifyMimoAccountServiceAndInvalidation(t *testing.T, srv *httptest.Server, ssoCalls *int32) {
	t.Helper()
	acc := &Account{ID: 401, Platform: PlatformMimo, Credentials: map[string]any{"user_id": "100000001", "pass_token": "V1:fake_pass_token==", "base_url": srv.URL}}
	repo := &fakeMimoAccountRepo{accounts: map[int64]*Account{401: acc}}
	t.Cleanup(func() {
		sharedMimoTokenProviders.Delete(repo)
		InvalidateMimoTokenCache(401)
	})
	sharedProvider := newMimoTokenProviderFromAccountRepo(repo)
	sharedProvider.httpClient, sharedProvider.serviceLoginURL = srv.Client(), srv.URL+"/pass/serviceLogin"

	acctSvc := &AccountService{accountRepo: repo}
	if err := acctSvc.TestCredentials(context.Background(), 401); err != nil {
		t.Fatalf("TestCredentials failed: %v", err)
	}
	if atomic.LoadInt32(ssoCalls) != 1 || repo.updates != 1 {
		t.Fatalf("expected 1 SSO exchange and 1 DB update during TestCredentials, got sso=%d updates=%d", *ssoCalls, repo.updates)
	}

	usageSvc := &AccountUsageService{accountRepo: repo}
	dbAcc, _ := repo.GetByID(context.Background(), 401)
	if info, err := usageSvc.getUsageForAccount(context.Background(), dbAcc, false); err != nil || info == nil || info.MimoQuota == nil || info.MimoQuota.RemainingPercent != 82.5 {
		t.Fatalf("unexpected getUsageForAccount result: %+v err=%v", info, err)
	}
	if _, ok := mimoUsageCache.Load(int64(401)); !ok {
		t.Fatalf("expected mimoUsageCache populated for account 401")
	}

	sharedProvider.mu.Lock()
	sharedProvider.cache[401] = &cachedMimoAuth{NegativeUntil: time.Now().Add(time.Minute)}
	sharedProvider.mu.Unlock()
	gwSvc := &OpenAIGatewayService{accountRepo: repo, mimoTokenProvider: sharedProvider}
	gwSvc.InvalidateMimoToken(401)
	if _, ok := mimoUsageCache.Load(int64(401)); ok {
		t.Fatalf("expected mimoUsageCache cleared by InvalidateMimoToken")
	}
	sharedProvider.mu.Lock()
	entryAfter, hasTokenCache := sharedProvider.cache[401]
	sharedProvider.mu.Unlock()
	if hasTokenCache || entryAfter != nil {
		t.Fatalf("expected provider positive and negative cache cleared by InvalidateMimoToken, got has=%v entry=%+v", hasTokenCache, entryAfter)
	}
}
