package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	// DefaultWbTokenURL 是 wb 企业 access_token 的换发端点（client_credentials）。
	DefaultWbTokenURL = "https://copilot.tencent.com/oauth2/token"

	// DefaultWbOverviewURL 是 wb 企业资源面板端点前缀，拼上 /{enterprise_id}/openapi/resources/overview。
	DefaultWbOverviewURL = "https://api.copilot.tencent.com/api/v1/enterprises"

	// wbTokenRefreshLead 是到期前触发主动换发的提前量。
	wbTokenRefreshLead = time.Hour

	// wbTokenExchangeTimeout 是单次换发的独立超时（不随任一等待方 ctx 取消而中断）。
	wbTokenExchangeTimeout = 15 * time.Second

	// wb 凭证键：四件套必填（client_id / client_secret / pt_key / enterprise_id），
	// cli_path 供 CLI 发现链使用（派遣②）。凭证值永不落入日志与错误文本。
	wbCredentialClientID     = "client_id"
	wbCredentialClientSecret = "client_secret"
	wbCredentialPTKey        = "pt_key"
	wbCredentialEnterpriseID = "enterprise_id"
	wbCredentialCLIPath      = "cli_path"
)

var (
	errWBNotConfigured      = errors.New("wb token provider is not configured")
	errWBNotWBAccount       = errors.New("not a wb account")
	errWBCredentialsMissing = errors.New("wb client credentials are missing")
	errWBCredentialsEdited  = errors.New("wb credentials changed during token exchange, in-flight token discarded")
)

// ValidateWBRequiredCredentials 校验 wb 企业账号的必填凭证四件套
// （client_id / client_secret / pt_key / enterprise_id）。cli_path 为可选键（发现链用）。
// 凭证校验函数本体在此（凭证形状的拥有者），创建/更新路径调用。
func ValidateWBRequiredCredentials(credentials map[string]any) error {
	for _, key := range []string{wbCredentialClientID, wbCredentialClientSecret, wbCredentialPTKey, wbCredentialEnterpriseID} {
		if strings.TrimSpace(wbCredentialString(credentials, key)) == "" {
			return fmt.Errorf("wb account credential %q is required", key)
		}
	}
	return nil
}

// wbCredentialString 从凭证 map 读取字符串值，兼容 string / json.Number / 数字类型
// （与 Account.GetCredential 的读取语义一致，见 account.go GetCredential）。
func wbCredentialString(credentials map[string]any, key string) string {
	if credentials == nil {
		return ""
	}
	switch v := credentials[key].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	default:
		return ""
	}
}

// wbCredentialFingerprint 计算 wb 凭证四件套的规范化指纹：
// 四件套以固定键序序列化为 JSON → SHA-256 → 前 16 位十六进制。
// json.Marshal 对 map 键排序，序列化结果稳定；map[string]string 序列化无失败路径。
// 指纹失配 = 凭证已编辑（或换人绑定），新旧凭证的 token 缓存天然隔离。
func wbCredentialFingerprint(clientID, clientSecret, ptKey, enterpriseID string) string {
	canonical, err := json.Marshal(map[string]string{
		"client_id":     clientID,
		"client_secret": clientSecret,
		"pt_key":        ptKey,
		"enterprise_id": enterpriseID,
	})
	if err != nil {
		// map[string]string 序列化不可能失败；防御性兜底（固定哨兵串，不泄露凭证）。
		return "wb-fingerprint-unavailable"
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])[:16]
}

// wbTokenCacheKey 是 token 缓存与单飞键的公共编码：绑定（账号 id, 凭证指纹）。
func wbTokenCacheKey(accountID int64, fingerprint string) string {
	return "wb:" + strconv.FormatInt(accountID, 10) + ":" + fingerprint
}

// mergeWBCredentials 按仓库 BulkUpdate 的 JSONB `||` 合并语义构造「写入后形状」：
// incoming 覆盖同键、缺失键保留 existing（该语义见 account_repo.go BulkUpdate 的
// credentials = COALESCE(credentials, '{}'::jsonb) || $payload）。仅用于批量校验的
// 合并后形状判定；不修改任何入参。
func mergeWBCredentials(existing, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(existing)+len(incoming))
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range incoming {
		out[k] = v
	}
	return out
}

// wbTokenEntry 是 wb 企业 access_token 的进程内缓存条目。
// 只缓 token 本体与到期时刻，绝不缓存凭证值。
type wbTokenEntry struct {
	token     string
	expiresAt time.Time
}

// wbAccountRepo 是 WbTokenProvider 需要的账号仓最小面（consumer-side 窄接口）：
// 只需 GetByID 复核当前凭证指纹；任何提供 GetByID 的账号仓实现天然满足。
type wbAccountRepo interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
}

// WbTokenProvider 为 wb 企业账号换发并缓存 access_token（client_credentials 流）。
// 缓存键绑定（账号 id, 凭证指纹）：指纹失配 = 凭证已编辑 → 自然 miss 触发重换发；
// 写回前复核 DB 当前指纹，在途期间凭证被编辑则整体丢弃在途结果（设计 AC1）。
// 凭证编辑/账号删除的主动失效由 InvalidateAccountCache 提供，装配接线在派遣②。
type WbTokenProvider struct {
	accountRepo   wbAccountRepo
	httpClient    *http.Client
	tokenURL      string
	cache         sync.Map // key: wbTokenCacheKey(accountID, fingerprint) → *wbTokenEntry
	flight        singleflight.Group
	invalidateGen sync.Map // accountID → uint64：失效代次（编辑/删除失效时推高），换发写回前复核
	entryLockMu   sync.Map // accountID → *sync.Mutex：换发写回（代次复核+Store）与失效（bump+Delete）的互斥屏障
}

// NewWbTokenProvider 创建 WbTokenProvider。httpClient 为空用 http.DefaultClient；
// tokenURL 为空用常量默认端点（测试可注入 httptest 地址）。
func NewWbTokenProvider(accountRepo wbAccountRepo, httpClient *http.Client, tokenURL string) *WbTokenProvider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if tokenURL == "" {
		tokenURL = DefaultWbTokenURL
	}
	return &WbTokenProvider{accountRepo: accountRepo, httpClient: httpClient, tokenURL: tokenURL}
}

// GetAccessToken 返回 wb 企业账号的有效 access_token：
//  1. 缓存命中（同指纹 + 距到期 > 1h）直接返回；
//  2. 未缓存 / 指纹失配 / 进入到期前 1h 刷新窗口 → 单飞换发（键绑定 账号id+指纹）；
//  3. 换发成功后、写回前，重读 DB 复核当前指纹——在途期间凭证被编辑 → 丢弃结果并报错；
//  4. 换发失败 → 传播错误，绝不回退过期 token。
func (p *WbTokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	if p == nil {
		return "", errWBNotConfigured
	}
	if account == nil {
		return "", errors.New("account is nil")
	}
	if account.Platform != PlatformWB {
		return "", errWBNotWBAccount
	}
	clientID := strings.TrimSpace(account.GetCredential(wbCredentialClientID))
	clientSecret := strings.TrimSpace(account.GetCredential(wbCredentialClientSecret))
	if clientID == "" || clientSecret == "" {
		return "", errWBCredentialsMissing
	}
	ptKey := strings.TrimSpace(account.GetCredential(wbCredentialPTKey))
	enterpriseID := strings.TrimSpace(account.GetCredential(wbCredentialEnterpriseID))
	fingerprint := wbCredentialFingerprint(clientID, clientSecret, ptKey, enterpriseID)
	cacheKey := wbTokenCacheKey(account.ID, fingerprint)

	// 1. 缓存命中（同指纹 + 距到期 > 1h）
	if entry, ok := p.cache.Load(cacheKey); ok {
		if e, ok := entry.(*wbTokenEntry); ok && p.tokenStillFresh(e.expiresAt) {
			return e.token, nil
		}
	}

	// 2. 单飞换发（键绑定 账号id+指纹）：未缓存 / 指纹失配 / 进入刷新窗口
	v, err, _ := p.flight.Do(cacheKey, func() (any, error) {
		// 换发开始时记录失效代次；写回前复核，闭合「指纹复核与缓存写回之间」的失效竞态
		gen, _ := p.invalidateGen.LoadOrStore(account.ID, uint64(0))
		genAtExchange := gen.(uint64)
		// 等锁期间可能已被其他请求换发：再查一次
		if entry, ok := p.cache.Load(cacheKey); ok {
			if e, ok := entry.(*wbTokenEntry); ok && p.tokenStillFresh(e.expiresAt) {
				return e.token, nil
			}
		}
		// 独立超时上下文：换发不随任一等待方取消而中断
		exchangeCtx, cancel := context.WithTimeout(context.Background(), wbTokenExchangeTimeout)
		defer cancel()
		token, ttl, err := wbExchangeClientCredentials(exchangeCtx, p.httpClient, p.tokenURL, clientID, clientSecret)
		if err != nil {
			return nil, err
		}
		// 3. 写回前复核 DB 当前指纹——在途期间凭证被编辑 → 整体丢弃
		if !p.fingerprintStillCurrent(exchangeCtx, account.ID, fingerprint) {
			return nil, errWBCredentialsEdited
		}
		// 失效代次屏障：复核之后若发生编辑/删除失效（键未变但条目已被清），同样丢弃写回。
		// storeIfCurrent 与失效侧共享 account 锁：复核+Store 与 bump+Delete 是互斥临界区，
		// 任一交错下都不会出现「复核通过后失效插入、旧条目复活」（TOCTOU 闭合）。
		if !p.storeIfCurrent(account.ID, cacheKey, &wbTokenEntry{token: token, expiresAt: time.Now().Add(ttl)}, genAtExchange) {
			return nil, errWBCredentialsEdited
		}
		return token, nil
	})
	if err != nil {
		return "", err
	}
	token, ok := v.(string)
	if !ok || token == "" {
		return "", errors.New("wb token exchange returned an empty token")
	}
	return token, nil
}

// entryLock 返回账号的写回/失效互斥锁（惰性建，账号粒度）。
func (p *WbTokenProvider) entryLock(accountID int64) *sync.Mutex {
	v, _ := p.entryLockMu.LoadOrStore(accountID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// storeIfCurrent 在账号锁内复核失效代次后写回缓存条目。代次已推进（期间发生
// 编辑/删除失效）→ 不写回、返回 false；代次未变 → 写回、返回 true。
// 与 InvalidateAccountCache 的 bump+Delete 同锁互斥：检查与写回原子，
// 失效不可能插在二者之间（TOCTOU 闭合）。
func (p *WbTokenProvider) storeIfCurrent(accountID int64, cacheKey string, entry *wbTokenEntry, wantGen uint64) bool {
	mu := p.entryLock(accountID)
	mu.Lock()
	defer mu.Unlock()
	gen, _ := p.invalidateGen.LoadOrStore(accountID, uint64(0))
	if gen.(uint64) != wantGen {
		return false
	}
	p.cache.Store(cacheKey, entry)
	return true
}

// tokenStillFresh 判定缓存条目仍可直接使用：未到期且未进入到期前 1h 刷新窗口。
func (p *WbTokenProvider) tokenStillFresh(expiresAt time.Time) bool {
	return time.Now().Before(expiresAt.Add(-wbTokenRefreshLead))
}

// fingerprintStillCurrent 复核 DB 中账号的当前凭证指纹是否仍是换发所用的指纹。
// 查询失败 → 允许写回（与 CheckTokenVersion 先例一致：DB 瞬时故障不惩罚请求线程）；
// 账号已删除或指纹失配 → 丢弃（false）。
func (p *WbTokenProvider) fingerprintStillCurrent(ctx context.Context, accountID int64, fingerprint string) bool {
	if p.accountRepo == nil {
		return true
	}
	latest, err := p.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		if errors.Is(err, ErrAccountNotFound) {
			// 账号已删除是明确证据，不属于瞬时故障放行面——在途结果必须丢弃
			return false
		}
		return true
	}
	if latest == nil {
		return false
	}
	cid := strings.TrimSpace(latest.GetCredential(wbCredentialClientID))
	csec := strings.TrimSpace(latest.GetCredential(wbCredentialClientSecret))
	ptk := strings.TrimSpace(latest.GetCredential(wbCredentialPTKey))
	eid := strings.TrimSpace(latest.GetCredential(wbCredentialEnterpriseID))
	return wbCredentialFingerprint(cid, csec, ptk, eid) == fingerprint
}

// InvalidateAccountCache 立即失效指定账号的全部 token 缓存条目（所有指纹）。
// 供凭证编辑 / 账号删除后的主动失效调用；装配接线在派遣②（wire 层）。
func (p *WbTokenProvider) InvalidateAccountCache(accountID int64) {
	if p == nil {
		return
	}
	// bump 与 Delete 同一账号锁内原子，与换发写回（storeIfCurrent）互斥：
	// 写回先完成则本次删除清掉它；本锁先完成则写回侧代次复核失败丢弃。
	mu := p.entryLock(accountID)
	mu.Lock()
	defer mu.Unlock()
	gen, _ := p.invalidateGen.LoadOrStore(accountID, uint64(0))
	p.invalidateGen.Store(accountID, gen.(uint64)+1)
	prefix := "wb:" + strconv.FormatInt(accountID, 10) + ":"
	p.cache.Range(func(key, _ any) bool {
		if k, ok := key.(string); ok && strings.HasPrefix(k, prefix) {
			p.cache.Delete(key)
		}
		return true
	})
}

// wbExchangeClientCredentials 是 wb 企业 access_token 的换发单源实现
// （client_credentials 流，POST token 端点）。WbTokenProvider 与
// TestCredentials 冒烟共用。凭证值绝不出现在错误文本 / 日志中；
// 错误响应正文不随错误传播（上游可能回显 client_id，属凭证纪律禁止范围）。
// 成功返回 access_token 与生效时长；失败返回可读错误。
func wbExchangeClientCredentials(ctx context.Context, client *http.Client, tokenURL, clientID, clientSecret string) (string, time.Duration, error) {
	if client == nil {
		client = http.DefaultClient
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("wb token request construction failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("wb token exchange failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("wb token response read failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", 0, fmt.Errorf("wb token endpoint returned HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, fmt.Errorf("wb token response is not valid JSON: %w", err)
	}
	if parsed.AccessToken == "" {
		return "", 0, errors.New("wb token response is missing access_token")
	}
	if parsed.ExpiresIn <= 0 {
		return "", 0, errors.New("wb token response is missing a valid expires_in")
	}
	return parsed.AccessToken, time.Duration(parsed.ExpiresIn) * time.Second, nil
}
