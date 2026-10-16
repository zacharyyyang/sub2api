package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type qoderAdminRepoStub struct {
	fakeQoderAccountRepo
}

func (r *qoderAdminRepoStub) ListShadowsByParent(_ context.Context, _ int64) ([]*Account, error) {
	return nil, nil
}

func (r *qoderAdminRepoStub) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.accounts, id)
	return nil
}

func (r *qoderAdminRepoStub) Update(_ context.Context, account *Account) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *account
	cp.Credentials = make(map[string]any, len(account.Credentials))
	for k, v := range account.Credentials {
		cp.Credentials[k] = v
	}
	r.accounts[account.ID] = &cp
	return nil
}

func (r *qoderAdminRepoStub) GetByIDs(_ context.Context, ids []int64) ([]*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Account, 0, len(ids))
	for _, id := range ids {
		if acc, ok := r.accounts[id]; ok && acc != nil {
			cp := *acc
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *qoderAdminRepoStub) BulkUpdate(_ context.Context, _ []int64, _ AccountBulkUpdate) (int64, error) {
	return 1, nil
}

type qoderBlockerRecorder struct {
	mu             sync.Mutex
	invalidatedIDs []int64
}

func (b *qoderBlockerRecorder) BlockAccountScheduling(_ *Account, _ time.Time, _ string) {}
func (b *qoderBlockerRecorder) ClearAccountSchedulingBlock(_ int64)                      {}

func (b *qoderBlockerRecorder) InvalidateQoderToken(id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.invalidatedIDs = append(b.invalidatedIDs, id)
}

// TestQoderTokenProviderSingleflightAndConflictAndRetry 覆盖设计 §6 用例 5, 12：
// <7d 预刷新、单飞合并、并发编辑冲突放弃覆盖写库、UpdateCredentials 3 次退避重试与失败 SetError 标错。
func TestQoderTokenProviderSingleflightAndConflictAndRetry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	var httpCalls int32
	client := &http.Client{
		Transport: qoderRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
			n := atomic.AddInt32(&httpCalls, 1)
			body, _ := json.Marshal(map[string]any{
				"data": map[string]any{
					"token":            "rot-acc-" + string(rune('0'+n)),
					"refresh_token":    "rot-ref-" + string(rune('0'+n)),
					"token_expires_at": now.Add(30 * 24 * time.Hour).UnixMilli(),
				},
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
			20: {
				ID:       20,
				Platform: PlatformQoder,
				Credentials: map[string]any{
					"access_token":     "init-acc",
					"refresh_token":    "init-ref",
					"uid":              "u-20",
					"org_id":           "o-20",
					"token_expires_at": now.Add(3 * 24 * time.Hour).UnixMilli(), // < 7d 触发预刷新
				},
			},
		},
	}
	provider := NewQoderTokenProvider(repo, client, "https://openapi.qoder.sh/api/v1/deviceToken/refresh")
	provider.nowFn = func() time.Time { return now }
	var sleepCalls int
	provider.sleepFn = func(time.Duration) { sleepCalls++ }

	// 1. <7d 预刷新 + UpdateCredentials 失败 3 次重试后调用 SetError，但当次请求仍返回新 token
	repo.updateErr = errors.New("db write transient failure")
	acc, _ := repo.GetByID(context.Background(), 20)
	got, err := provider.GetValidCredentials(context.Background(), acc, false)
	if err != nil {
		t.Fatalf("expected refresh to succeed despite db persist error: %v", err)
	}
	if got.AccessToken != "rot-acc-1" || repo.updates != 4 || sleepCalls != 3 || len(repo.setErrors) != 1 {
		t.Fatalf("expected 4 update attempts (initial+3 retries) and 1 SetError, got updates=%d sleeps=%d errs=%v", repo.updates, sleepCalls, repo.setErrors)
	}

	// 2. 并发编辑冲突：刷新期间管理员修改了数据库中的 refresh_token，放弃覆盖写库并采用管理员新凭据
	repo.updateErr = nil
	repo.updates = 0
	conflictClient := &http.Client{
		Transport: qoderRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
			// 模拟刷新 HTTP 期间管理员在 DB 修改了凭据
			repo.accounts[20].Credentials["access_token"] = "manual-acc"
			repo.accounts[20].Credentials["refresh_token"] = "manual-ref"
			repo.accounts[20].Credentials["token_expires_at"] = now.Add(25 * 24 * time.Hour).UnixMilli()
			body, _ := json.Marshal(map[string]any{"data": map[string]any{"token": "stale-rot-acc", "refresh_token": "stale-rot-ref"}})
			return &http.Response{StatusCode: http.StatusOK, Body: &readCloserBuffer{Reader: strings.NewReader(string(body))}, Header: make(http.Header)}, nil
		}),
	}
	provider2 := NewQoderTokenProvider(repo, conflictClient, "https://openapi.qoder.sh/api/v1/deviceToken/refresh")
	provider2.nowFn = func() time.Time { return now }
	gotConflict, err := provider2.GetValidCredentials(context.Background(), acc, true)
	if err != nil {
		t.Fatalf("GetValidCredentials on concurrent edit failed: %v", err)
	}
	if gotConflict.AccessToken != "manual-acc" || repo.updates != 0 {
		t.Fatalf("expected conflict check to adopt manual-acc and skip UpdateCredentials, got %+v updates=%d", gotConflict, repo.updates)
	}

	// 3. 并发 singleflight 合并验证：两个 goroutine 同时强制刷新只触发一次上游 HTTP 请求
	var sfCalls int32
	sfClient := &http.Client{
		Transport: qoderRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
			atomic.AddInt32(&sfCalls, 1)
			time.Sleep(15 * time.Millisecond)
			body, _ := json.Marshal(map[string]any{"data": map[string]any{"token": "sf-acc", "refresh_token": "sf-ref"}})
			return &http.Response{StatusCode: http.StatusOK, Body: &readCloserBuffer{Reader: strings.NewReader(string(body))}, Header: make(http.Header)}, nil
		}),
	}
	sfProvider := NewQoderTokenProvider(repo, sfClient, "https://openapi.qoder.sh/api/v1/deviceToken/refresh")
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, e := sfProvider.GetValidCredentials(context.Background(), acc, true)
			errCh <- e
		}()
	}
	if err1, err2 := <-errCh, <-errCh; err1 != nil || err2 != nil || atomic.LoadInt32(&sfCalls) != 1 {
		t.Fatalf("expected singleflight to merge 2 concurrent refreshes into 1 HTTP call, got calls=%d err1=%v err2=%v", sfCalls, err1, err2)
	}
}

// TestQoderAdminInvalidationAndTimestampPreservation 覆盖设计 §6 用例 12：
// 单账号更新（含保留 token_expires_at / refresh_token_expires_at）、批量更新、DeleteAccount 三处触发 InvalidateQoderToken。
func TestQoderAdminInvalidationAndTimestampPreservation(t *testing.T) {
	repo := &qoderAdminRepoStub{
		fakeQoderAccountRepo: fakeQoderAccountRepo{
			accounts: map[int64]*Account{
				30: {
					ID:       30,
					Name:     "qoder-prod",
					Platform: PlatformQoder,
					Type:     AccountTypeAPIKey,
					Credentials: map[string]any{
						"access_token":             "acc-30",
						"refresh_token":            "ref-30",
						"uid":                      "u-30",
						"org_id":                   "o-30",
						"token_expires_at":         int64(1790000000000),
						"refresh_token_expires_at": int64(1820000000000),
					},
				},
			},
		},
	}
	blocker := &qoderBlockerRecorder{}
	svc := &adminServiceImpl{accountRepo: repo, runtimeBlocker: blocker}

	// 1. 单账号编辑（未传敏感键与过期时间戳）：保留原有 token 与两个 expires_at，并触发 InvalidateQoderToken(30)
	updated, err := svc.UpdateAccount(context.Background(), 30, &UpdateAccountInput{
		Name:        "qoder-prod-renamed",
		Credentials: map[string]any{"uid": "u-30", "org_id": "o-30"},
	})
	if err != nil {
		t.Fatalf("UpdateAccount failed: %v", err)
	}
	if updated.Credentials["access_token"] != "acc-30" ||
		qoderCredInt64(updated.Credentials, "token_expires_at") != 1790000000000 ||
		qoderCredInt64(updated.Credentials, "refresh_token_expires_at") != 1820000000000 {
		t.Fatalf("expected UpdateAccount to preserve tokens and timestamps, got %v", updated.Credentials)
	}

	// 2. 批量更新凭据：触发 InvalidateQoderToken(30)
	_, err = svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs:  []int64{30},
		Credentials: map[string]any{"machine_type": "darwin"},
	})
	if err != nil {
		t.Fatalf("BulkUpdateAccounts failed: %v", err)
	}

	// 3. 删除账号：触发 InvalidateQoderToken(30)
	if err := svc.DeleteAccount(context.Background(), 30); err != nil {
		t.Fatalf("DeleteAccount failed: %v", err)
	}
	if len(blocker.invalidatedIDs) != 3 || blocker.invalidatedIDs[0] != 30 || blocker.invalidatedIDs[1] != 30 || blocker.invalidatedIDs[2] != 30 {
		t.Fatalf("expected 3 InvalidateQoderToken(30) calls on update/bulk/delete, got %v", blocker.invalidatedIDs)
	}
}
