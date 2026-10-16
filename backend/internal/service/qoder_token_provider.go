package service

import (
	"bytes"
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
	DefaultQoderRefreshURL   = "https://openapi.qoder.sh/api/v1/deviceToken/refresh"
	qoderTokenRefreshWindow  = 7 * 24 * time.Hour
	qoderTokenFallbackExpiry = 30 * 24 * time.Hour
	qoderRefreshHTTPTimeout  = 15 * time.Second
)

var qoderPersistRetryDelays = []time.Duration{50 * time.Millisecond, 150 * time.Millisecond, 300 * time.Millisecond}

type qoderAccountRepo interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
	UpdateCredentials(ctx context.Context, id int64, credentials map[string]any) error
	SetError(ctx context.Context, id int64, errorMsg string) error
}

type cachedQoderCreds struct {
	Creds         QoderCredentials
	Fingerprint   string
	LastRefreshAt time.Time
}

type QoderTokenProvider struct {
	accountRepo qoderAccountRepo
	httpClient  *http.Client
	refreshURL  string
	mu          sync.Mutex
	cache       map[int64]*cachedQoderCreds
	flight      singleflight.Group
	nowFn       func() time.Time
	sleepFn     func(d time.Duration)
}

func NewQoderTokenProvider(accountRepo qoderAccountRepo, httpClient *http.Client, refreshURL string) *QoderTokenProvider {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if refreshURL == "" {
		refreshURL = DefaultQoderRefreshURL
	}
	return &QoderTokenProvider{
		accountRepo: accountRepo, httpClient: httpClient, refreshURL: refreshURL,
		cache: make(map[int64]*cachedQoderCreds), nowFn: time.Now, sleepFn: time.Sleep,
	}
}

func (p *QoderTokenProvider) now() time.Time {
	if p != nil && p.nowFn != nil {
		return p.nowFn()
	}
	return time.Now()
}

func (p *QoderTokenProvider) InvalidateQoderToken(accountID int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.cache, accountID)
	p.mu.Unlock()
}

func (p *QoderTokenProvider) InvalidateAccountCache(accountID int64) {
	p.InvalidateQoderToken(accountID)
}

func (p *QoderTokenProvider) GetValidCredentials(ctx context.Context, account *Account, forceRefresh bool) (QoderCredentials, error) {
	if p == nil || account == nil {
		return QoderCredentials{}, errors.New("qoder token provider or account is nil")
	}
	inCreds, err := ParseQoderCredentials(account.Credentials)
	if err != nil {
		return QoderCredentials{}, err
	}
	reqStart := p.now()
	activeCreds, _ := p.resolveCachedOrLatest(ctx, account.ID, *inCreds)
	if !forceRefresh && !p.needsRefresh(activeCreds) {
		return activeCreds, nil
	}
	v, err, _ := p.flight.Do("qoder:"+strconv.FormatInt(account.ID, 10), func() (any, error) {
		curCreds, curLastRefresh := p.resolveCachedOrLatest(ctx, account.ID, activeCreds)
		if !forceRefresh && !p.needsRefresh(curCreds) {
			return curCreds, nil
		}
		if forceRefresh && !curLastRefresh.IsZero() && !curLastRefresh.Before(reqStart) && curCreds.AccessToken != inCreds.AccessToken {
			return curCreds, nil
		}
		return p.refreshAndPersist(ctx, account, curCreds)
	})
	if err != nil {
		return QoderCredentials{}, err
	}
	return v.(QoderCredentials), nil
}

func (p *QoderTokenProvider) resolveCachedOrLatest(ctx context.Context, accountID int64, incoming QoderCredentials) (QoderCredentials, time.Time) {
	inFP := CredentialFingerprint(&incoming)
	p.mu.Lock()
	entry := p.cache[accountID]
	p.mu.Unlock()
	if entry == nil {
		p.storeCache(accountID, incoming, time.Time{})
		return incoming, time.Time{}
	}
	if entry.Fingerprint == inFP {
		return entry.Creds, entry.LastRefreshAt
	}
	if p.accountRepo != nil && accountID > 0 {
		if latest, err := p.accountRepo.GetByID(ctx, accountID); err == nil && latest != nil {
			if dbCreds, err := ParseQoderCredentials(latest.Credentials); err == nil {
				if CredentialFingerprint(dbCreds) == entry.Fingerprint {
					return entry.Creds, entry.LastRefreshAt
				}
				p.storeCache(accountID, *dbCreds, time.Time{})
				return *dbCreds, time.Time{}
			}
		}
	}
	p.storeCache(accountID, incoming, time.Time{})
	return incoming, time.Time{}
}

func (p *QoderTokenProvider) needsRefresh(creds QoderCredentials) bool {
	if creds.TokenExpiresAt <= 0 {
		return true
	}
	return time.Duration(creds.TokenExpiresAt-p.now().UnixMilli())*time.Millisecond < qoderTokenRefreshWindow
}

func (p *QoderTokenProvider) storeCache(accountID int64, creds QoderCredentials, lastRefreshAt time.Time) {
	p.mu.Lock()
	p.cache[accountID] = &cachedQoderCreds{Creds: creds, Fingerprint: CredentialFingerprint(&creds), LastRefreshAt: lastRefreshAt}
	p.mu.Unlock()
}

func (p *QoderTokenProvider) refreshAndPersist(ctx context.Context, account *Account, creds QoderCredentials) (QoderCredentials, error) {
	bgCtx := context.WithoutCancel(ctx)
	prevRT, prevUID, prevOrg := creds.RefreshToken, creds.UID, creds.OrgID
	refreshed, err := p.callDeviceTokenRefresh(bgCtx, creds)
	if err != nil {
		return QoderCredentials{}, err
	}
	now := p.now()
	if p.accountRepo == nil || account.ID <= 0 {
		p.storeCache(account.ID, refreshed, now)
		return refreshed, nil
	}
	latest, err := p.accountRepo.GetByID(bgCtx, account.ID)
	if errors.Is(err, ErrAccountNotFound) || (err == nil && latest == nil) {
		p.InvalidateQoderToken(account.ID)
		return QoderCredentials{}, ErrAccountNotFound
	}
	if latest != nil {
		if dbCreds, parseErr := ParseQoderCredentials(latest.Credentials); parseErr == nil {
			if dbCreds.RefreshToken != prevRT || dbCreds.UID != prevUID || dbCreds.OrgID != prevOrg {
				p.storeCache(account.ID, *dbCreds, time.Time{})
				return *dbCreds, nil
			}
		}
	}
	p.storeCache(account.ID, refreshed, now)
	p.persistRefreshedCredentials(bgCtx, account, latest, refreshed)
	return refreshed, nil
}

func (p *QoderTokenProvider) callDeviceTokenRefresh(ctx context.Context, creds QoderCredentials) (QoderCredentials, error) {
	reqCtx, cancel := context.WithTimeout(ctx, qoderRefreshHTTPTimeout)
	defer cancel()
	payload, _ := json.Marshal(map[string]string{"refresh_token": creds.RefreshToken, "token": creds.AccessToken, "machine_id": creds.MachineID, "machine_type": creds.MachineType})
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, p.refreshURL, bytes.NewReader(payload))
	if err != nil {
		return QoderCredentials{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return QoderCredentials{}, fmt.Errorf("qoder token refresh failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return QoderCredentials{}, fmt.Errorf("qoder token refresh failed: HTTP %d", resp.StatusCode)
	}
	return p.parseRefreshResponse(body, creds)
}

func (p *QoderTokenProvider) parseRefreshResponse(body []byte, creds QoderCredentials) (QoderCredentials, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return QoderCredentials{}, fmt.Errorf("qoder token refresh invalid json: %w", err)
	}
	data := raw
	if d, ok := raw["data"].(map[string]any); ok {
		data = d
	}
	newToken, newRefresh := qoderFirstTrimNonEmpty(qoderCredString(data, "token"), qoderCredString(data, "access_token")), strings.TrimSpace(qoderCredString(data, "refresh_token"))
	if newToken == "" || newRefresh == "" {
		return QoderCredentials{}, errors.New("qoder token refresh malformed response")
	}
	expAt := qoderCredInt64(data, "expires_at")
	if v := qoderCredInt64(data, "token_expires_at"); v > expAt {
		expAt = v
	}
	if expAt <= p.now().UnixMilli() {
		expAt = p.now().Add(qoderTokenFallbackExpiry).UnixMilli()
	}
	rtExpAt := qoderCredInt64(data, "refresh_token_expires_at")
	if rtExpAt <= 0 {
		rtExpAt = creds.RefreshTokenExpiresAt
	}
	out := creds
	out.AccessToken, out.RefreshToken, out.TokenExpiresAt, out.RefreshTokenExpiresAt = newToken, newRefresh, expAt, rtExpAt
	EnsureDerivedFields(&out)
	return out, nil
}

func (p *QoderTokenProvider) persistRefreshedCredentials(ctx context.Context, account *Account, latest *Account, refreshed QoderCredentials) {
	saveCtx, cancel := context.WithTimeout(ctx, qoderRefreshHTTPTimeout)
	defer cancel()
	updated := make(map[string]any, 12)
	for k, v := range account.Credentials {
		updated[k] = v
	}
	if latest != nil {
		for k, v := range latest.Credentials {
			updated[k] = v
		}
	}
	delete(updated, qoderCredTokenAlias)
	updated[qoderCredAccessToken], updated[qoderCredRefreshToken], updated[qoderCredUID], updated[qoderCredOrgID] = refreshed.AccessToken, refreshed.RefreshToken, refreshed.UID, refreshed.OrgID
	updated[qoderCredMachineID], updated[qoderCredUMIDToken], updated[qoderCredMachineType], updated[qoderCredTokenExpiresAt] = refreshed.MachineID, refreshed.UMIDToken, refreshed.MachineType, refreshed.TokenExpiresAt
	if refreshed.RefreshTokenExpiresAt > 0 {
		updated[qoderCredRefreshTokenExpiresAt] = refreshed.RefreshTokenExpiresAt
	}
	var lastErr error
	for attempt := 0; attempt <= len(qoderPersistRetryDelays); attempt++ {
		if attempt > 0 && p.sleepFn != nil {
			p.sleepFn(qoderPersistRetryDelays[attempt-1])
		}
		if lastErr = p.accountRepo.UpdateCredentials(saveCtx, account.ID, updated); lastErr == nil {
			return
		}
	}
	msg := "qoder token refresh db persist failed: " + lastErr.Error()
	slog.Error(msg, "account_id", account.ID)
	_ = p.accountRepo.SetError(saveCtx, account.ID, msg)
}

func extractQoderDeltaToolCalls(data map[string]any, acc map[int]*QoderToolCallAccumulator) []any {
	rawTCs, _ := data["tool_calls"].([]any)
	if len(rawTCs) == 0 {
		if choices, ok := data["choices"].([]any); ok && len(choices) > 0 {
			ch, _ := choices[0].(map[string]any)
			d, _ := ch["delta"].(map[string]any)
			rawTCs, _ = d["tool_calls"].([]any)
		}
	}
	for i, item := range rawTCs {
		tcMap, ok := item.(map[string]any)
		if !ok || tcMap == nil {
			continue
		}
		idx := qoderAnyToInt(tcMap["index"])
		if _, hasIdx := tcMap["index"]; !hasIdx && i > 0 {
			idx = i
		}
		if acc[idx] == nil {
			acc[idx] = &QoderToolCallAccumulator{Type: "function"}
		}
		if id := qoderAnyToStr(tcMap["id"]); id != "" {
			acc[idx].ID = id
		}
		if fn, ok := tcMap["function"].(map[string]any); ok {
			if name := qoderAnyToStr(fn["name"]); name != "" {
				acc[idx].Name = name
			}
			acc[idx].Arguments.WriteString(qoderAnyToStr(fn["arguments"]))
		}
	}
	return rawTCs
}
