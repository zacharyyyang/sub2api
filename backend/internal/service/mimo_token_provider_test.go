package service

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeMimoAccountRepo struct {
	AccountRepository
	mu       sync.Mutex
	accounts map[int64]*Account
	updates  int
}

func (r *fakeMimoAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
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

func (r *fakeMimoAccountRepo) UpdateCredentials(_ context.Context, id int64, creds map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates++
	if acc, ok := r.accounts[id]; ok && acc != nil {
		acc.Credentials = make(map[string]any, len(creds))
		for k, v := range creds {
			acc.Credentials[k] = v
		}
	}
	return nil
}

func TestMimoTokenProviderSSOAndCache(t *testing.T) {
	const (
		fakeNonce     = "1234567890123456789"
		fakeSsecurity = "dGVzdF9zc2VjdXJpdHlfa2V5"
	)
	var phase1Calls, phase2Calls int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pass/serviceLogin":
			atomic.AddInt32(&phase1Calls, 1)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = fmt.Fprintf(w, "&&&START&&&{\"code\":0,\"location\":\"%s/sts?sid=mimopc\",\"nonce\":%s,\"ssecurity\":\"%s\",\"cUserId\":\"enc_uid_001\"}", srv.URL, fakeNonce, fakeSsecurity)
		case "/sts":
			atomic.AddInt32(&phase2Calls, 1)
			sum := sha1.Sum([]byte(fmt.Sprintf("nonce=%s&%s", fakeNonce, fakeSsecurity)))
			wantSign := base64.StdEncoding.EncodeToString(sum[:])
			if gotSign := r.URL.Query().Get("clientSign"); gotSign != wantSign {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "serviceToken", Value: "st_exchanged_001", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "cUserId", Value: "cuid_cookie_001", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "mimo_ph", Value: "ph_cookie_001", Path: "/"})
			http.Redirect(w, r, srv.URL+"/done", http.StatusFound)
		case "/done":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	acc := &Account{
		ID:       101,
		Platform: PlatformMimo,
		Credentials: map[string]any{
			"user_id":    "100000001",
			"pass_token": "V1:fake_pass_token==",
		},
	}
	repo := &fakeMimoAccountRepo{accounts: map[int64]*Account{101: acc}}
	provider := NewMimoTokenProvider(repo, srv.Client(), srv.URL+"/pass/serviceLogin?sid=mimopc&_json=true")
	now := time.Unix(1710000000, 0)
	provider.nowFn = func() time.Time { return now }

	// 1. First call executes two-phase SSO and persists to DB
	creds, err := provider.GetValidCredentials(context.Background(), acc, false)
	if err != nil {
		t.Fatalf("GetValidCredentials failed: %v", err)
	}
	if creds.ServiceToken != "st_exchanged_001" || creds.CUserID != "cuid_cookie_001" || creds.MimoPH != "ph_cookie_001" {
		t.Fatalf("unexpected exchanged creds: %+v", creds)
	}
	if atomic.LoadInt32(&phase1Calls) != 1 || atomic.LoadInt32(&phase2Calls) != 1 || repo.updates != 1 {
		t.Fatalf("expected 1 phase1, 1 phase2, 1 db update; got %d, %d, %d", phase1Calls, phase2Calls, repo.updates)
	}
	dbAcc, _ := repo.GetByID(context.Background(), 101)
	if dbAcc.Credentials["service_token"] != "st_exchanged_001" || dbAcc.Credentials["mimo_ph"] != "ph_cookie_001" {
		t.Fatalf("expected refreshed credentials persisted in repo, got %v", dbAcc.Credentials)
	}

	// 2. Second call within 5m hits cache without HTTP request
	now = now.Add(2 * time.Minute)
	creds2, err := provider.GetValidCredentials(context.Background(), acc, false)
	if err != nil || creds2.ServiceToken != "st_exchanged_001" || atomic.LoadInt32(&phase1Calls) != 1 {
		t.Fatalf("expected 5m cache hit, got creds=%+v err=%v calls=%d", creds2, err, phase1Calls)
	}

	// 3. Static service_token without pass_token: normal ok, forceRefresh returns error
	staticAcc := &Account{
		ID:       102,
		Platform: PlatformMimo,
		Credentials: map[string]any{
			"user_id":       "100000002",
			"service_token": "static_st_002",
		},
	}
	if staticCreds, err := provider.GetValidCredentials(context.Background(), staticAcc, false); err != nil || staticCreds.ServiceToken != "static_st_002" {
		t.Fatalf("expected static service_token to succeed when forceRefresh=false, got %+v, %v", staticCreds, err)
	}
	if _, err := provider.GetValidCredentials(context.Background(), staticAcc, true); err == nil {
		t.Fatalf("expected error when forceRefresh=true and pass_token is not configured")
	}
}

func TestMimoTokenProviderForceRefreshHonorsNegativeCache(t *testing.T) {
	var phase1Calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&phase1Calls, 1)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprint(w, `&&&START&&&{"code":70016,"desc":"passToken expired"}`)
	}))
	defer srv.Close()

	acc := &Account{
		ID:       202,
		Platform: PlatformMimo,
		Credentials: map[string]any{
			"user_id":    "100000202",
			"pass_token": "V1:expired_pass_token==",
		},
	}
	repo := &fakeMimoAccountRepo{accounts: map[int64]*Account{202: acc}}
	t.Cleanup(func() { sharedMimoTokenProviders.Delete(repo) })
	provider := newMimoTokenProviderFromAccountRepo(repo)
	provider.httpClient = srv.Client()
	provider.serviceLoginURL = srv.URL + "/pass/serviceLogin"
	now := time.Unix(1710000000, 0)
	provider.nowFn = func() time.Time { return now }

	// 1st call (forceRefresh=false) triggers HTTP and caches negative result for 60s
	if _, err := provider.GetValidCredentials(context.Background(), acc, false); err == nil {
		t.Fatalf("expected phase1 failure error on 1st call")
	}
	if atomic.LoadInt32(&phase1Calls) != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", phase1Calls)
	}

	// 2nd call within 60s with forceRefresh=true MUST still honor 60s negative cache
	now = now.Add(15 * time.Second)
	if _, err := provider.GetValidCredentials(context.Background(), acc, true); err == nil {
		t.Fatalf("expected negative cached error on 2nd call (forceRefresh=true)")
	}
	if atomic.LoadInt32(&phase1Calls) != 1 {
		t.Fatalf("expected forceRefresh=true within 60s to honor negative cache (1 call total), got %d", phase1Calls)
	}

	// Package-level InvalidateMimoTokenCache clears both token cache and mimoUsageCache
	mimoUsageCache.Store(int64(202), "cached_usage")
	InvalidateMimoTokenCache(202)
	if _, ok := mimoUsageCache.Load(int64(202)); ok {
		t.Fatalf("expected mimoUsageCache entry for 202 cleared by InvalidateMimoTokenCache")
	}

	// 3rd call after InvalidateMimoTokenCache sends a new HTTP request
	if _, err := provider.GetValidCredentials(context.Background(), acc, true); err == nil {
		t.Fatalf("expected phase1 failure error on 3rd call")
	}
	if atomic.LoadInt32(&phase1Calls) != 2 {
		t.Fatalf("expected 2nd HTTP call after InvalidateMimoTokenCache, got %d", phase1Calls)
	}
}
