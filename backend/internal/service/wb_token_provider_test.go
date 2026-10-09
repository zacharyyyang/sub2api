//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// wbTokenRepoStub 以可变账号指针模拟账号库存取（编辑 = setAccount 换新账号对象）。
// 内嵌 accountRepoStub 满足 AccountRepository 全部方法（未用方法 panic），
// GetByID 在本层重写（外层方法遮蔽内嵌 panic 版）。
type wbTokenRepoStub struct {
	accountRepoStub
	mu       sync.RWMutex
	account  *Account
	notFound bool
}

func (r *wbTokenRepoStub) GetByID(ctx context.Context, id int64) (*Account, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.notFound {
		return nil, ErrAccountNotFound
	}
	if r.account == nil || r.account.ID != id {
		return nil, nil
	}
	return r.account, nil
}

func (r *wbTokenRepoStub) setAccount(account *Account) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.account = account
}

func (r *wbTokenRepoStub) setNotFound() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notFound = true
}

func wbTestAccount(id int64, clientID, clientSecret, ptKey, enterpriseID string) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformWB,
		Credentials: map[string]any{
			wbCredentialClientID:     clientID,
			wbCredentialClientSecret: clientSecret,
			wbCredentialPTKey:        ptKey,
			wbCredentialEnterpriseID: enterpriseID,
		},
	}
}

// wbTokenTestServer 提供可阻塞的换发端点，并统计调用次数。
type wbTokenTestServer struct {
	server      *httptest.Server
	started     chan struct{}
	startedOnce sync.Once
	release     chan struct{}
	calls       atomic.Int32
	expiresIn   int64
	status      int
}

func newWbTokenTestServer(t *testing.T, expiresIn int64) *wbTokenTestServer {
	t.Helper()
	ts := &wbTokenTestServer{
		started:   make(chan struct{}),
		release:   make(chan struct{}),
		expiresIn: expiresIn,
		status:    http.StatusOK,
	}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.calls.Add(1)
		ts.startedOnce.Do(func() { close(ts.started) })
		<-ts.release
		w.Header().Set("Content-Type", "application/json")
		if ts.status != http.StatusOK {
			w.WriteHeader(ts.status)
			fmt.Fprintf(w, `{"error":"invalid_client"}`)
			return
		}
		fmt.Fprintf(w, `{"access_token":"tok-%d","expires_in":%d}`, ts.calls.Load(), ts.expiresIn)
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func (ts *wbTokenTestServer) url() string { return ts.server.URL }

func TestWbTokenProvider_GetAccessToken(t *testing.T) {
	ts := newWbTokenTestServer(t, 86400)
	repo := &wbTokenRepoStub{account: wbTestAccount(1001, "cid-1", "csec-1", "ptk-1", "eid-1")}
	provider := NewWbTokenProvider(repo, ts.server.Client(), ts.url())
	close(ts.release)

	token, err := provider.GetAccessToken(context.Background(), repo.account)
	require.NoError(t, err)
	require.Equal(t, "tok-1", token)
	require.Equal(t, int32(1), ts.calls.Load())

	// 二次调用：同指纹 + 未进入刷新窗口 → 直接命中缓存，不再换发
	token2, err := provider.GetAccessToken(context.Background(), repo.account)
	require.NoError(t, err)
	require.Equal(t, "tok-1", token2)
	require.Equal(t, int32(1), ts.calls.Load())
}

func TestWbTokenProvider_RefreshWindow(t *testing.T) {
	// expires_in = 3600s：换发后立即处于「到期前 1h」刷新窗口 → 二次调用触发重换发
	ts := newWbTokenTestServer(t, 3600)
	repo := &wbTokenRepoStub{account: wbTestAccount(1002, "cid-1", "csec-1", "ptk-1", "eid-1")}
	provider := NewWbTokenProvider(repo, ts.server.Client(), ts.url())
	close(ts.release)

	_, err := provider.GetAccessToken(context.Background(), repo.account)
	require.NoError(t, err)
	_, err = provider.GetAccessToken(context.Background(), repo.account)
	require.NoError(t, err)
	require.Equal(t, int32(2), ts.calls.Load())
}

// TestWbTokenProvider_SingleflightConcurrentRefresh AC1：到期前 1h 刷新窗口内
// 并发 N 请求仅 1 次换发（后续请求复用同一单飞结果；窗口内缓存不满足直用）。
func TestWbTokenProvider_SingleflightConcurrentRefresh(t *testing.T) {
	ts := newWbTokenTestServer(t, 3600) // 换发后立即处于刷新窗口
	repo := &wbTokenRepoStub{account: wbTestAccount(1002, "cid-1", "csec-1", "ptk-1", "eid-1")}
	provider := NewWbTokenProvider(repo, ts.server.Client(), ts.url())

	// 首个请求先进 handler（单飞键已占用）；其余请求并发到达时等待同一结果
	var firstToken string
	var firstErr error
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		firstToken, firstErr = provider.GetAccessToken(context.Background(), repo.account)
	}()
	<-ts.started // 换发在途（handler 阻塞）→ flight.Do 键已占用

	const n = 4
	tokens := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tokens[i], errs[i] = provider.GetAccessToken(context.Background(), repo.account)
		}(i)
	}
	// 给并发请求时间挂到同一单飞键上，再放行换发。50ms 足够稳定：release
	// 在 sleep 后才关闭，waiters 只要在 release 前到达 Do 即全部落入同一飞
	// （换发在 handler 内阻塞，主测试始终先于 release 完成挂起）。
	time.Sleep(50 * time.Millisecond)
	close(ts.release)
	<-firstDone
	wg.Wait()

	require.NoError(t, firstErr)
	require.Equal(t, "tok-1", firstToken)
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, "tok-1", tokens[i])
	}
	require.Equal(t, int32(1), ts.calls.Load()) // 并发 N 请求仅 1 次换发
}

func TestWbTokenProvider_InvalidateAccountCache(t *testing.T) {
	ts := newWbTokenTestServer(t, 86400)
	repo := &wbTokenRepoStub{account: wbTestAccount(1003, "cid-1", "csec-1", "ptk-1", "eid-1")}
	provider := NewWbTokenProvider(repo, ts.server.Client(), ts.url())
	close(ts.release)

	token, err := provider.GetAccessToken(context.Background(), repo.account)
	require.NoError(t, err)
	require.Equal(t, "tok-1", token)

	// 主动失效 → 缓存清空 → 重换发（AC1 失效口径）
	provider.InvalidateAccountCache(repo.account.ID)
	token2, err := provider.GetAccessToken(context.Background(), repo.account)
	require.NoError(t, err)
	require.Equal(t, "tok-2", token2)
	require.Equal(t, int32(2), ts.calls.Load())
}

func TestWbTokenProvider_ExchangeFailure(t *testing.T) {
	ts := newWbTokenTestServer(t, 86400)
	ts.status = http.StatusUnauthorized
	repo := &wbTokenRepoStub{account: wbTestAccount(1004, "cid-1", "csec-1", "ptk-1", "eid-1")}
	provider := NewWbTokenProvider(repo, ts.server.Client(), ts.url())
	close(ts.release)

	_, err := provider.GetAccessToken(context.Background(), repo.account)
	require.Error(t, err)

	// 失败不落负缓存：重试会再次尝试换发
	_, err = provider.GetAccessToken(context.Background(), repo.account)
	require.Error(t, err)
	require.Equal(t, int32(2), ts.calls.Load())
}

// TestWbTokenProvider_CredentialsEdited AC1：换发在途期间凭证被编辑 →
// 旧指纹的换发结果整体丢弃（不写缓存、返回明确错误）。
func TestWbTokenProvider_CredentialsEdited(t *testing.T) {
	ts := newWbTokenTestServer(t, 86400)
	repo := &wbTokenRepoStub{account: wbTestAccount(1005, "cid-1", "csec-1", "ptk-1", "eid-1")}
	provider := NewWbTokenProvider(repo, ts.server.Client(), ts.url())

	accountA := repo.account
	oldFingerprint := wbCredentialFingerprint("cid-1", "csec-1", "ptk-1", "eid-1")
	oldKey := wbTokenCacheKey(accountA.ID, oldFingerprint)

	var gotToken string
	var gotErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		gotToken, gotErr = provider.GetAccessToken(context.Background(), accountA)
	}()

	// 换发在途（handler 阻塞）时编辑凭证
	<-ts.started
	repo.setAccount(wbTestAccount(1005, "cid-edited", "csec-edited", "ptk-1", "eid-1"))
	close(ts.release)
	<-done

	require.Error(t, gotErr)
	require.True(t, errors.Is(gotErr, errWBCredentialsEdited), "got: %v", gotErr)
	require.Equal(t, "", gotToken)
	// AC1：缓存中不存在与旧凭证指纹匹配的条目
	_, ok := provider.cache.Load(oldKey)
	require.False(t, ok, "stale fingerprint entry must not be cached")
	// AC1：编辑后的新凭证首请求正常换发，且缓存指纹 == 当前指纹（新键）
	token2, err := provider.GetAccessToken(context.Background(), repo.account)
	require.NoError(t, err)
	require.Equal(t, "tok-2", token2)
	newFingerprint := wbCredentialFingerprint("cid-edited", "csec-edited", "ptk-1", "eid-1")
	newKey := wbTokenCacheKey(accountA.ID, newFingerprint)
	entry, ok := provider.cache.Load(newKey)
	require.True(t, ok, "new fingerprint entry must be cached")
	we, ok := entry.(*wbTokenEntry)
	require.True(t, ok)
	require.Equal(t, token2, we.token)
	require.Equal(t, int32(2), ts.calls.Load())
}

// TestWbTokenProvider_InvalidateDuringExchange AC1：换发在途发生编辑 + 主动失效 →
// 在途结果丢弃且失效条目不被重填（指纹复核 + 失效代次屏障双保险）。
func TestWbTokenProvider_InvalidateDuringExchange(t *testing.T) {
	ts := newWbTokenTestServer(t, 86400)
	repo := &wbTokenRepoStub{account: wbTestAccount(1006, "cid-1", "csec-1", "ptk-1", "eid-1")}
	provider := NewWbTokenProvider(repo, ts.server.Client(), ts.url())
	accountA := repo.account

	var gotToken string
	var gotErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		gotToken, gotErr = provider.GetAccessToken(context.Background(), accountA)
	}()

	// 换发在途（handler 阻塞）：编辑凭证并主动失效（编辑路径的标准顺序）
	<-ts.started
	repo.setAccount(wbTestAccount(1006, "cid-2", "csec-2", "ptk-2", "eid-2"))
	provider.InvalidateAccountCache(accountA.ID)
	close(ts.release)
	<-done

	require.Error(t, gotErr)
	require.True(t, errors.Is(gotErr, errWBCredentialsEdited), "got: %v", gotErr)
	require.Equal(t, "", gotToken)
	// AC1：失效后旧指纹条目不被重填（失效代次已推高，写回被拦截）
	_, ok := provider.cache.Load(wbTokenCacheKey(accountA.ID, wbCredentialFingerprint("cid-1", "csec-1", "ptk-1", "eid-1")))
	require.False(t, ok, "invalidated fingerprint entry must not be resurrected")
}

// TestWbTokenProvider_AccountDeletedDuringExchange AC1：换发在途账号被删除
// （GetByID 明确返回 ErrAccountNotFound，非瞬时故障）→ 在途结果整体丢弃、不写缓存。
func TestWbTokenProvider_AccountDeletedDuringExchange(t *testing.T) {
	ts := newWbTokenTestServer(t, 86400)
	repo := &wbTokenRepoStub{account: wbTestAccount(1007, "cid-1", "csec-1", "ptk-1", "eid-1")}
	provider := NewWbTokenProvider(repo, ts.server.Client(), ts.url())
	accountA := repo.account

	var gotToken string
	var gotErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		gotToken, gotErr = provider.GetAccessToken(context.Background(), accountA)
	}()

	<-ts.started
	repo.setNotFound() // 账号已删除：GetByID 返回 ErrAccountNotFound
	close(ts.release)
	<-done

	require.Error(t, gotErr)
	require.True(t, errors.Is(gotErr, errWBCredentialsEdited), "got: %v", gotErr)
	require.Equal(t, "", gotToken)
	_, ok := provider.cache.Load(wbTokenCacheKey(accountA.ID, wbCredentialFingerprint("cid-1", "csec-1", "ptk-1", "eid-1")))
	require.False(t, ok, "deleted account's in-flight token must not be cached")
}

func TestWbTokenProvider_NotWBAccount(t *testing.T) {
	provider := NewWbTokenProvider(nil, nil, "")
	account := &Account{ID: 2001, Platform: PlatformGrok}
	_, err := provider.GetAccessToken(context.Background(), account)
	require.Error(t, err)
	require.True(t, errors.Is(err, errWBNotWBAccount))
}

func TestValidateWBRequiredCredentials(t *testing.T) {
	require.NoError(t, ValidateWBRequiredCredentials(map[string]any{
		"client_id": "cid", "client_secret": "csec", "pt_key": "ptk", "enterprise_id": "eid",
	}))
	require.Error(t, ValidateWBRequiredCredentials(map[string]any{
		"client_id": "cid", "client_secret": "", "pt_key": "ptk", "enterprise_id": "eid",
	}))
	require.Error(t, ValidateWBRequiredCredentials(map[string]any{"client_id": "cid"}))
	require.Error(t, ValidateWBRequiredCredentials(nil))
}

// TestWbTokenProvider_StoreIfCurrent_GenerationBarrier 确定性验证换发写回与失效的同步屏障：
// 失效（代次推高 + 条目删除）之后，旧代次的写回必须被拦截（TOCTOU 窗口闭合——
// storeIfCurrent 与 InvalidateAccountCache 共享账号锁，复核+Store 与 bump+Delete 互斥原子）。
func TestWbTokenProvider_StoreIfCurrent_GenerationBarrier(t *testing.T) {
	provider := NewWbTokenProvider(nil, nil, "")
	const acctID = int64(2007)
	key := wbTokenCacheKey(acctID, "fp-1")
	first := &wbTokenEntry{token: "tok-1", expiresAt: time.Now().Add(time.Hour)}

	// 代次未变：写回成功
	require.True(t, provider.storeIfCurrent(acctID, key, first, 0))
	got, ok := provider.cache.Load(key)
	require.True(t, ok)
	require.Equal(t, "tok-1", got.(*wbTokenEntry).token)

	// 失效（bump 代次 + 删条目）后，旧代次写回被拦截：条目不复活
	provider.InvalidateAccountCache(acctID)
	second := &wbTokenEntry{token: "tok-2", expiresAt: time.Now().Add(time.Hour)}
	require.False(t, provider.storeIfCurrent(acctID, key, second, 0))
	_, ok = provider.cache.Load(key)
	require.False(t, ok, "stale-generation write-back must not resurrect the entry")

	// 以当前代次写回：恢复成功（失效后首次换发不被屏障卡死）
	gen, _ := provider.invalidateGen.Load(acctID)
	require.True(t, provider.storeIfCurrent(acctID, key, second, gen.(uint64)))
	got, ok = provider.cache.Load(key)
	require.True(t, ok)
	require.Equal(t, "tok-2", got.(*wbTokenEntry).token)
}

// TestMergeWBCredentials_用于批量校验的合并后形状 验证 BulkUpdate 的 JSONB `||` 合并语义：
// incoming 覆盖同键、缺失键保留 existing；合并后四件套校验与仓库最终存储形状一致，
// 局部更新不误拒、空 secret 仍拒绝。
func TestMergeWBCredentials_ForBulkValidationShape(t *testing.T) {
	complete := map[string]any{"client_id": "cid", "client_secret": "csec", "pt_key": "ptk", "enterprise_id": "eid"}

	// 局部更新：existing 四件套齐全、incoming 只改 pt_key → 合并后仍齐全，必须通过
	merged := mergeWBCredentials(complete, map[string]any{"pt_key": "ptk-2"})
	require.Equal(t, "ptk-2", merged["pt_key"])
	require.Equal(t, "csec", merged["client_secret"])
	require.NoError(t, ValidateWBRequiredCredentials(merged))

	// 新建（无 existing）携带四件套 → 通过
	require.NoError(t, ValidateWBRequiredCredentials(mergeWBCredentials(nil, complete)))

	// existing 缺 client_secret、incoming 也不含 → 合并后仍缺 → 拒绝（新建/补全不可行）
	partial := map[string]any{"client_id": "cid", "pt_key": "ptk", "enterprise_id": "eid"}
	require.Error(t, ValidateWBRequiredCredentials(mergeWBCredentials(partial, map[string]any{"pt_key": "ptk-2"})))

	// incoming 显式置空 client_secret → 合并后为空 → 拒绝（第 1 轮核心漏洞场景）
	require.Error(t, ValidateWBRequiredCredentials(mergeWBCredentials(complete, map[string]any{"client_secret": ""})))

	// 入参不被修改
	require.Equal(t, "csec", complete["client_secret"])
	require.Equal(t, "ptk", complete["pt_key"])
}

func TestWbExchangeClientCredentials(t *testing.T) {
	var gotForm string
	var gotAuthHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		require.NoError(t, r.ParseForm())
		gotForm = r.Form.Encode()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"abc","token_type":"Bearer","expires_in":31536000,"scope":"profile email"}`)
	}))
	defer server.Close()

	token, ttl, err := wbExchangeClientCredentials(context.Background(), server.Client(), server.URL, "cid-1", "csec-1")
	require.NoError(t, err)
	require.Equal(t, "abc", token)
	require.Equal(t, 31536000*time.Second, ttl)
	require.Contains(t, gotForm, "grant_type=client_credentials")
	require.Contains(t, gotForm, "client_id=cid-1")
	require.Contains(t, gotForm, "client_secret=csec-1")
	// 凭证换发要求无 Authorization 头（client_credentials 走表单）
	require.Equal(t, "", gotAuthHeader)
}

func TestWbExchangeClientCredentials_Non2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":"invalid_client","error_description":"oops"}`)
	}))
	defer server.Close()

	_, _, err := wbExchangeClientCredentials(context.Background(), server.Client(), server.URL, "cid-1", "csec-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "401")
	// 凭证值绝不出现在错误文本
	require.NotContains(t, err.Error(), "cid-1")
	require.NotContains(t, err.Error(), "csec-1")
}

func TestWbExchangeClientCredentials_MissingFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token_type":"Bearer"}`) // 缺 access_token / expires_in
	}))
	defer server.Close()

	_, _, err := wbExchangeClientCredentials(context.Background(), server.Client(), server.URL, "cid-1", "csec-1")
	require.Error(t, err)
}
