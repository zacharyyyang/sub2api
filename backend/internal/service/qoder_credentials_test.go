package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/model"
)

type fakeQoderAccountRepo struct {
	AccountRepository
	mu        sync.Mutex
	accounts  map[int64]*Account
	updateErr error
	updates   int
	setErrors []string
}

func (r *fakeQoderAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	acc, ok := r.accounts[id]
	if !ok || acc == nil {
		return nil, ErrAccountNotFound
	}
	cp := *acc
	cp.Credentials = make(map[string]any, len(acc.Credentials))
	for k, v := range acc.Credentials {
		cp.Credentials[k] = v
	}
	return &cp, nil
}

func (r *fakeQoderAccountRepo) UpdateCredentials(_ context.Context, id int64, creds map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates++
	if r.updateErr != nil {
		return r.updateErr
	}
	if acc, ok := r.accounts[id]; ok && acc != nil {
		acc.Credentials = make(map[string]any, len(creds))
		for k, v := range creds {
			acc.Credentials[k] = v
		}
	}
	return nil
}

func (r *fakeQoderAccountRepo) SetError(_ context.Context, _ int64, errorMsg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setErrors = append(r.setErrors, errorMsg)
	return nil
}

type qoderRoundTripFunc func(*http.Request) (*http.Response, error)

func (f qoderRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestQoderModelWhitelistAndConstants 覆盖设计 §6 用例 2, 3：32 个合法模型归一与解析及未知模型拒绝。
func TestQoderModelWhitelistAndConstants(t *testing.T) {
	if domain.PlatformQoder != "qoder" || PlatformQoder != "qoder" {
		t.Fatalf("unexpected PlatformQoder: %s / %s", domain.PlatformQoder, PlatformQoder)
	}
	if len(domain.QoderModelWhitelist) != 14 || len(domain.QoderNativeModels) != 18 {
		t.Fatalf("unexpected model list lengths: %d / %d", len(domain.QoderModelWhitelist), len(domain.QoderNativeModels))
	}
	rule := &model.ErrorPassthroughRule{
		Name: "qoder-rule", ErrorCodes: []int{429}, MatchMode: model.MatchModeAny,
		Platforms: []string{"qoder"}, PassthroughCode: true, PassthroughBody: true,
	}
	if err := rule.Validate(); err != nil {
		t.Fatalf("expected error passthrough rule with platform=qoder to validate: %v", err)
	}

	for _, alias := range domain.QoderModelWhitelist {
		native, err := domain.NormalizeQoderModel(strings.ToUpper(alias))
		if err != nil || native != domain.QoderModelAliasToNative[alias] {
			t.Fatalf("NormalizeQoderModel(%s) = %s, %v", alias, native, err)
		}
		resKey, ok := domain.ResolveQoderModel(strings.ToUpper(alias))
		if !ok || resKey != alias {
			t.Fatalf("ResolveQoderModel(%s) = %s, %v; want %s, true", alias, resKey, ok, alias)
		}
		resKey2, ok2 := domain.ResolveQoderModel(native)
		if !ok2 || resKey2 != alias {
			t.Fatalf("ResolveQoderModel(native=%s) = %s, %v; want %s, true", native, resKey2, ok2, alias)
		}
	}
	for _, tier := range []string{"auto", "fast", "performance", "efficient"} {
		nat, err := domain.NormalizeQoderModel(strings.ToUpper(tier))
		if err != nil || nat != tier {
			t.Fatalf("NormalizeQoderModel(%s) = %s, %v", tier, nat, err)
		}
		rk, ok := domain.ResolveQoderModel(strings.ToUpper(tier))
		if !ok || rk != tier {
			t.Fatalf("ResolveQoderModel(%s) = %s, %v", tier, rk, ok)
		}
	}
	if _, err := domain.NormalizeQoderModel("gpt-99-unknown"); err == nil {
		t.Fatalf("expected error for unknown model gpt-99-unknown")
	}
	if _, ok := domain.ResolveQoderModel("gpt-99-unknown"); ok {
		t.Fatalf("expected false for unknown model gpt-99-unknown")
	}
}

// TestQoderCredentialsDeriveAndRedact 覆盖设计 §6 用例 17：必填校验、确定性派生、RedactCredentials 脱敏与编辑保留。
func TestQoderCredentialsDeriveAndRedact(t *testing.T) {
	if err := ValidateQoderRequiredCredentials(map[string]any{"access_token": "a"}); err == nil {
		t.Fatalf("expected missing required fields error")
	}
	raw := map[string]any{
		"token":         "tok-123",
		"refresh_token": "ref-456",
		"uid":           "u-001",
		"org_id":        "o-001",
	}
	creds, err := NormalizeQoderCredentials(raw)
	if err != nil {
		t.Fatalf("NormalizeQoderCredentials failed: %v", err)
	}
	if creds.AccessToken != "tok-123" || len(creds.MachineID) != 64 || !strings.HasPrefix(creds.UMIDToken, "B42") || len(creds.UMIDToken) != 35 || creds.MachineType != "win32" {
		t.Fatalf("unexpected derived fields: %+v", creds)
	}
	creds2, _ := ParseQoderCredentials(map[string]any{"access_token": "x", "refresh_token": "y", "uid": "u-001", "org_id": "o-001"})
	if creds.MachineID != creds2.MachineID || creds.UMIDToken != creds2.UMIDToken {
		t.Fatalf("derived fields not deterministic")
	}
	redacted, status := RedactCredentials(raw)
	if _, exists := redacted["umid_token"]; exists {
		t.Fatalf("expected umid_token stripped by RedactCredentials, got %v", redacted)
	}
	if _, exists := redacted["token"]; exists {
		t.Fatalf("expected legacy token stripped by NormalizeQoderCredentials and absent in redacted output, got %v", redacted)
	}
	if !status["has_umid_token"] || !status["has_access_token"] || !status["has_refresh_token"] {
		t.Fatalf("expected has_umid_token/has_access_token/has_refresh_token=true, got %v", status)
	}
	merged := MergePreservingSensitiveCreds(
		map[string]any{"access_token": "a", "refresh_token": "r", "umid_token": creds.UMIDToken, "uid": "u-001"},
		map[string]any{"uid": "u-002"},
	)
	if merged["umid_token"] != creds.UMIDToken || merged["uid"] != "u-002" {
		t.Fatalf("expected MergePreservingSensitiveCreds to preserve umid_token and update uid, got %v", merged)
	}
}

// TestQoderTokenProviderRefreshAndCache 覆盖设计 §6 用例 5：单飞刷新、缺省 expires_at 30d 回退、指纹复核、并发编辑防护与缓存失效。
func TestQoderTokenProviderRefreshAndCache(t *testing.T) {
	now := time.Unix(1700000000, 0)
	var httpCalls int32
	client := &http.Client{
		Transport: qoderRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			atomic.AddInt32(&httpCalls, 1)
			body, _ := json.Marshal(map[string]any{
				"data": map[string]any{"token": "new-acc-token", "refresh_token": "new-ref-token"},
			})
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       &readCloserBuffer{Reader: strings.NewReader(string(body))},
				Header:     make(http.Header),
			}, nil
		}),
	}
	repo := &fakeQoderAccountRepo{
		accounts: map[int64]*Account{
			10: {
				ID:       10,
				Platform: PlatformQoder,
				Credentials: map[string]any{
					"access_token":  "old-acc",
					"refresh_token": "old-ref",
					"uid":           "u-10",
					"org_id":        "o-10",
				},
			},
		},
	}
	provider := NewQoderTokenProvider(repo, client, "https://openapi.qoder.sh/api/v1/deviceToken/refresh")
	provider.nowFn = func() time.Time { return now }
	provider.sleepFn = func(time.Duration) {}
	acc, _ := repo.GetByID(context.Background(), 10)
	got, err := provider.GetValidCredentials(context.Background(), acc, false)
	if err != nil {
		t.Fatalf("GetValidCredentials failed: %v", err)
	}
	if got.AccessToken != "new-acc-token" || got.RefreshToken != "new-ref-token" || atomic.LoadInt32(&httpCalls) != 1 {
		t.Fatalf("unexpected refreshed creds: %+v, calls=%d", got, httpCalls)
	}
	wantExp := now.Add(30 * 24 * time.Hour).UnixMilli()
	if got.TokenExpiresAt != wantExp {
		t.Fatalf("expected 30d fallback expiry %d, got %d", wantExp, got.TokenExpiresAt)
	}
	// 紧接第二次调用（即使传入陈旧快照 old-ref，因 DB 已是 new-ref-token 与缓存指纹一致）：直接命中缓存不重刷
	staleSnapshot := &Account{ID: 10, Platform: PlatformQoder, Credentials: map[string]any{"access_token": "old-acc", "refresh_token": "old-ref", "uid": "u-10", "org_id": "o-10"}}
	got2, err := provider.GetValidCredentials(context.Background(), staleSnapshot, false)
	if err != nil || atomic.LoadInt32(&httpCalls) != 1 || got2.AccessToken != "new-acc-token" {
		t.Fatalf("expected cache hit via GetByID verification without second HTTP call, calls=%d, got2=%+v, err=%v", httpCalls, got2, err)
	}
	// 模拟后台管理员修改了 DB 凭据（有效期充足），传入旧快照时通过 GetByID 重建缓存并不触发 refresh
	repo.accounts[10].Credentials["access_token"] = "admin-acc"
	repo.accounts[10].Credentials["refresh_token"] = "admin-ref"
	repo.accounts[10].Credentials["token_expires_at"] = now.Add(20 * 24 * time.Hour).UnixMilli()
	got3, err := provider.GetValidCredentials(context.Background(), staleSnapshot, false)
	if err != nil || atomic.LoadInt32(&httpCalls) != 1 || got3.AccessToken != "admin-acc" {
		t.Fatalf("expected DB rebuild without HTTP refresh, calls=%d, got3=%+v, err=%v", httpCalls, got3, err)
	}
	provider.InvalidateQoderToken(10)
	provider.InvalidateAccountCache(10)
}

type readCloserBuffer struct{ *strings.Reader }

func (b *readCloserBuffer) Close() error { return nil }
