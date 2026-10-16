package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestQoderQuotaFetchAndTestCredentialsReadOnly 覆盖设计 §6 用例 14：
// ① plan 无 userQuota、usage 返回 3000+100=3100；
// ② 均无额度返回 QoderCredits == nil（不造 0、不写 ErrorCode）；
// ③ TestCredentials 仅调 FetchQoderQuota 与 WASM 签名，绝不调用 refresh。
func TestQoderQuotaFetchAndTestCredentialsReadOnly(t *testing.T) {
	var planCalls, usageCalls, refreshCalls int32
	var returnEmptyQuota bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/user/plan":
			atomic.AddInt32(&planCalls, 1)
			if r.Header.Get("Authorization") != "Bearer dt-test-quota" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"planName":  "Teams",
					"expiresAt": "2026-11-15T00:00:00Z",
				},
			})
		case "/api/v2/quota/usage":
			atomic.AddInt32(&usageCalls, 1)
			if returnEmptyQuota {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"userQuota":  map[string]any{"remaining": 3000, "total": 3000},
					"addOnQuota": map[string]any{"remaining": 100, "total": 100},
				},
			})
		case "/api/v1/deviceToken/refresh":
			atomic.AddInt32(&refreshCalls, 1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	origPlan, origUsage, origClient := qoderPlanURL, qoderQuotaUsageURL, qoderQuotaHTTPClient
	qoderPlanURL = srv.URL + "/api/v2/user/plan"
	qoderQuotaUsageURL = srv.URL + "/api/v2/quota/usage"
	qoderQuotaHTTPClient = srv.Client()
	defer func() {
		qoderPlanURL, qoderQuotaUsageURL, qoderQuotaHTTPClient = origPlan, origUsage, origClient
	}()

	// ① plan 无 userQuota，自动补查 /api/v2/quota/usage 聚合 3000 + 100 = 3100
	credits, err := FetchQoderQuota(context.Background(), "dt-test-quota")
	if err != nil {
		t.Fatalf("FetchQoderQuota failed: %v", err)
	}
	if credits == nil || credits.Remaining != 3100 || credits.Total != 3100 || credits.Plan != "Teams" || credits.ExpiresAt != "2026-11-15" {
		t.Fatalf("unexpected credits: %+v", credits)
	}

	// ② 均无额度对象时返回 nil（不造 0），且 getQoderUsage 失败降级仅 Warn、不写 Error/ErrorCode
	returnEmptyQuota = true
	nilCredits, err := FetchQoderQuota(context.Background(), "dt-test-quota")
	if err != nil || nilCredits != nil {
		t.Fatalf("expected nil credits when quota objects absent, got %+v, err=%v", nilCredits, err)
	}

	returnEmptyQuota = false
	qoderUsageCache.Delete(int64(401))
	repo := &fakeQoderAccountRepo{
		accounts: map[int64]*Account{
			401: {
				ID:       401,
				Platform: PlatformQoder,
				Credentials: map[string]any{
					"access_token":     "dt-test-quota",
					"refresh_token":    "drt-test-quota",
					"uid":              "u-401",
					"org_id":           "o-401",
					"token_expires_at": time.Now().Add(20 * 24 * time.Hour).UnixMilli(),
				},
			},
		},
	}
	usageSvc := &AccountUsageService{accountRepo: repo}
	acc401, _ := repo.GetByID(context.Background(), 401)
	usageInfo, err := usageSvc.getUsageForAccount(context.Background(), acc401, false)
	if err != nil || usageInfo == nil || usageInfo.QoderCredits == nil || usageInfo.QoderCredits.Remaining != 3100 || usageInfo.Error != "" || usageInfo.ErrorCode != "" {
		t.Fatalf("unexpected usageInfo from getQoderUsage: %+v, err=%v", usageInfo, err)
	}

	// ③ TestCredentials 只读验证：即使 token_expires_at 缺省（<=0），TestCredentials 也只调用 FetchQoderQuota 与 WASM 签名，绝不触发 refresh
	accNoExp := &Account{
		ID:       402,
		Platform: PlatformQoder,
		Credentials: map[string]any{
			"access_token":  "dt-test-quota",
			"refresh_token": "drt-never-rotate",
			"uid":           "u-402",
			"org_id":        "o-402",
			// 故意不设置 token_expires_at
		},
	}
	repo.accounts[402] = accNoExp
	sharedProvider := newQoderTokenProviderFromAccountRepo(repo)
	sharedProvider.refreshURL = srv.URL + "/api/v1/deviceToken/refresh"
	sharedProvider.httpClient = srv.Client()
	acctSvc := &AccountService{accountRepo: repo}
	if err := acctSvc.TestCredentials(context.Background(), 402); err != nil {
		t.Fatalf("TestCredentials failed: %v", err)
	}
	if atomic.LoadInt32(&refreshCalls) != 0 {
		t.Fatalf("TestCredentials must be read-only and never call refresh, got refreshCalls=%d", refreshCalls)
	}
	if newQoderTokenProviderFromAccountRepo(repo) != sharedProvider {
		t.Fatal("expected gateway and usage polling to share the same QoderTokenProvider per repo")
	}
}
