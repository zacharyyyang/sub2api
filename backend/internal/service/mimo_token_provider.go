package service

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	DefaultXiaomiServiceLoginURL = "https://account.xiaomi.com/pass/serviceLogin?sid=mimopc&_json=true&_snsNone=true"
	mimoTokenCacheTTL            = 5 * time.Minute
	mimoTokenNegativeCacheTTL    = 60 * time.Second
	mimoSSOHTTPTimeout           = 12 * time.Second
	mimoMaxRedirects             = 5
)

var (
	mimoPersistRetryDelays                   = []time.Duration{50 * time.Millisecond, 150 * time.Millisecond, 300 * time.Millisecond}
	sharedMimoTokenProviders, mimoUsageCache sync.Map
)

type cachedMimoAuth struct {
	Creds                    MimoCredentials
	Fingerprint              string
	FetchedAt, NegativeUntil time.Time
	LastErr                  error
}

type MimoTokenProvider struct {
	accountRepo     mimoAccountRepo
	httpClient      *http.Client
	serviceLoginURL string
	mu              sync.Mutex
	cache           map[int64]*cachedMimoAuth
	flight          singleflight.Group
	nowFn           func() time.Time
	sleepFn         func(d time.Duration)
}

func NewMimoTokenProvider(accountRepo mimoAccountRepo, httpClient *http.Client, serviceLoginURL string) *MimoTokenProvider {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: mimoSSOHTTPTimeout}
	}
	return &MimoTokenProvider{
		accountRepo: accountRepo, httpClient: httpClient, serviceLoginURL: qoderFirstTrimNonEmpty(serviceLoginURL, DefaultXiaomiServiceLoginURL),
		cache: make(map[int64]*cachedMimoAuth), nowFn: time.Now, sleepFn: time.Sleep,
	}
}

func newMimoTokenProviderFromAccountRepo(repo AccountRepository) *MimoTokenProvider {
	if repo == nil {
		return NewMimoTokenProvider(nil, nil, "")
	}
	mr, ok := repo.(mimoAccountRepo)
	if !ok {
		mr = &mimoAccountRepoAdapter{AccountRepository: repo}
	}
	p := NewMimoTokenProvider(mr, nil, "")
	if reflect.TypeOf(repo).Comparable() {
		actual, _ := sharedMimoTokenProviders.LoadOrStore(repo, p)
		return actual.(*MimoTokenProvider)
	}
	return p
}

func InvalidateMimoTokenCache(accountID int64) {
	mimoUsageCache.Delete(accountID)
	sharedMimoTokenProviders.Range(func(_, value any) bool {
		if p, ok := value.(*MimoTokenProvider); ok && p != nil {
			p.InvalidateMimoToken(accountID)
		}
		return true
	})
}

func (p *MimoTokenProvider) now() time.Time {
	if p != nil && p.nowFn != nil {
		return p.nowFn()
	}
	return time.Now()
}

func (p *MimoTokenProvider) InvalidateMimoToken(accountID int64) {
	mimoUsageCache.Delete(accountID)
	if p != nil {
		p.mu.Lock()
		delete(p.cache, accountID)
		p.mu.Unlock()
	}
}

func (p *MimoTokenProvider) checkCachedState(entry *cachedMimoAuth, fp string, forceRefresh bool) (MimoCredentials, bool, error) {
	now := p.now()
	if entry != nil && entry.Fingerprint == fp && now.Before(entry.NegativeUntil) && entry.LastErr != nil {
		return MimoCredentials{}, true, entry.LastErr
	}
	if !forceRefresh && entry != nil && entry.Fingerprint == fp && entry.Creds.ServiceToken != "" && !entry.FetchedAt.IsZero() && now.Sub(entry.FetchedAt) < mimoTokenCacheTTL {
		return entry.Creds, true, nil
	}
	return MimoCredentials{}, false, nil
}

func (p *MimoTokenProvider) GetValidCredentials(ctx context.Context, account *Account, forceRefresh bool) (MimoCredentials, error) {
	if p == nil || account == nil {
		return MimoCredentials{}, errors.New("mimo token provider or account is nil")
	}
	inCreds, err := ParseMimoCredentials(account.Credentials)
	if err != nil {
		return MimoCredentials{}, err
	}
	reqStart := p.now()
	activeCreds, entry := p.resolveCachedOrLatest(ctx, account.ID, *inCreds)
	if cached, hit, err := p.checkCachedState(entry, MimoCredentialFingerprint(&activeCreds), forceRefresh); hit {
		return cached, err
	}
	if strings.TrimSpace(activeCreds.PassToken) == "" {
		if forceRefresh || strings.TrimSpace(activeCreds.ServiceToken) == "" {
			return MimoCredentials{}, errors.New("mimo service_token expired and pass_token is not configured")
		}
		p.storeCache(account.ID, activeCreds, reqStart, time.Time{}, nil)
		return activeCreds, nil
	}
	v, err, _ := p.flight.Do("mimo:"+strconv.FormatInt(account.ID, 10), func() (any, error) {
		curCreds, curEntry := p.resolveCachedOrLatest(ctx, account.ID, activeCreds)
		if cached, hit, err := p.checkCachedState(curEntry, MimoCredentialFingerprint(&curCreds), forceRefresh); hit {
			return cached, err
		}
		if forceRefresh && curEntry != nil && !curEntry.FetchedAt.IsZero() && !curEntry.FetchedAt.Before(reqStart) && curEntry.Creds.ServiceToken != inCreds.ServiceToken && curEntry.Creds.ServiceToken != "" {
			return curEntry.Creds, nil
		}
		return p.refreshAndPersist(ctx, account, curCreds)
	})
	if err != nil {
		return MimoCredentials{}, err
	}
	return v.(MimoCredentials), nil
}

func (p *MimoTokenProvider) resolveCachedOrLatest(ctx context.Context, accountID int64, incoming MimoCredentials) (MimoCredentials, *cachedMimoAuth) {
	inFP := MimoCredentialFingerprint(&incoming)
	p.mu.Lock()
	entry := p.cache[accountID]
	p.mu.Unlock()
	if entry != nil && entry.Fingerprint == inFP {
		return entry.Creds, entry
	}
	if entry != nil && p.accountRepo != nil && accountID > 0 {
		if latest, err := p.accountRepo.GetByID(ctx, accountID); err == nil && latest != nil {
			if dbCreds, parseErr := ParseMimoCredentials(latest.Credentials); parseErr == nil {
				if MimoCredentialFingerprint(dbCreds) == entry.Fingerprint {
					return entry.Creds, entry
				}
				return *dbCreds, nil
			}
		}
	}
	return incoming, nil
}

func (p *MimoTokenProvider) storeCache(accountID int64, creds MimoCredentials, fetchedAt, negUntil time.Time, lastErr error) {
	p.mu.Lock()
	p.cache[accountID] = &cachedMimoAuth{Creds: creds, Fingerprint: MimoCredentialFingerprint(&creds), FetchedAt: fetchedAt, NegativeUntil: negUntil, LastErr: lastErr}
	p.mu.Unlock()
}

func (p *MimoTokenProvider) refreshAndPersist(ctx context.Context, account *Account, creds MimoCredentials) (MimoCredentials, error) {
	bgCtx := context.WithoutCancel(ctx)
	prevPass, prevUser := creds.PassToken, creds.UserID
	refreshed, err := p.exchangeXiaomiSSO(bgCtx, creds)
	if err != nil {
		p.storeCache(account.ID, creds, time.Time{}, p.now().Add(mimoTokenNegativeCacheTTL), err)
		return MimoCredentials{}, err
	}
	var latest *Account
	if p.accountRepo != nil && account.ID > 0 {
		dbAcc, getErr := p.accountRepo.GetByID(bgCtx, account.ID)
		if errors.Is(getErr, ErrAccountNotFound) || (getErr == nil && dbAcc == nil) {
			p.InvalidateMimoToken(account.ID)
			return MimoCredentials{}, ErrAccountNotFound
		}
		if getErr == nil && dbAcc != nil {
			latest = dbAcc
			if dbCreds, parseErr := ParseMimoCredentials(dbAcc.Credentials); parseErr == nil && (dbCreds.PassToken != prevPass || dbCreds.UserID != prevUser) {
				if dbCreds.ServiceToken == "" && strings.TrimSpace(dbCreds.PassToken) != "" {
					return p.exchangeXiaomiSSO(bgCtx, *dbCreds)
				}
				p.storeCache(account.ID, *dbCreds, time.Time{}, time.Time{}, nil)
				return *dbCreds, nil
			}
		}
	}
	p.storeCache(account.ID, refreshed, p.now(), time.Time{}, nil)
	if p.accountRepo != nil && account.ID > 0 {
		saveCtx, cancel := context.WithTimeout(bgCtx, mimoSSOHTTPTimeout)
		defer cancel()
		updated := mergeMimoAccountCredentialsMap(account, latest, refreshed)
		var lastErr error
		for attempt := 0; attempt <= len(mimoPersistRetryDelays); attempt++ {
			if attempt > 0 && p.sleepFn != nil {
				p.sleepFn(mimoPersistRetryDelays[attempt-1])
			}
			if lastErr = p.accountRepo.UpdateCredentials(saveCtx, account.ID, updated); lastErr == nil {
				return refreshed, nil
			}
		}
		slog.Warn("mimo token refresh db persist failed", "account_id", account.ID, "error", lastErr)
	}
	return refreshed, nil
}

func (p *MimoTokenProvider) exchangeXiaomiSSO(ctx context.Context, creds MimoCredentials) (MimoCredentials, error) {
	reqCtx, cancel := context.WithTimeout(ctx, mimoSSOHTTPTimeout)
	defer cancel()
	req1, err := http.NewRequestWithContext(reqCtx, http.MethodGet, qoderFirstTrimNonEmpty(p.serviceLoginURL, DefaultXiaomiServiceLoginURL), nil)
	if err != nil {
		return MimoCredentials{}, err
	}
	req1.Header.Set("User-Agent", "Mozilla/5.0")
	req1.Header.Set("Cookie", fmt.Sprintf("userId=%s; cUserId=%s; passToken=%s; deviceId=%s; uLocale=zh_CN", creds.UserID, creds.CUserID, creds.PassToken, creds.DeviceID))
	resp1, err := cloneMimoHTTPClient(p.httpClient, func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }, nil).Do(req1)
	if err != nil {
		return MimoCredentials{}, fmt.Errorf("mimo sso phase1 request failed: %w", err)
	}
	defer func() { _ = resp1.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp1.Body, 1<<20))
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(body)), "&&&START&&&"))))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		return MimoCredentials{}, fmt.Errorf("mimo sso phase1 invalid json: %w", err)
	}
	location, ssecurity, nonceStr, cUserID := strings.TrimSpace(qoderFirstTrimNonEmpty(mimoCredString(payload, "location"), mimoCredString(payload, "loc"))), strings.TrimSpace(mimoCredString(payload, "ssecurity")), strings.TrimSpace(mimoCredString(payload, "nonce")), strings.TrimSpace(mimoCredString(payload, "cUserId"))
	if ssecurity == "" || location == "" || nonceStr == "" {
		return MimoCredentials{}, errors.New("mimo sso phase1 failed: missing ssecurity or location")
	}
	sum, sep := sha1.Sum([]byte(fmt.Sprintf("nonce=%s&%s", nonceStr, ssecurity))), "&"
	if !strings.Contains(location, "?") {
		sep = "?"
	}
	cookies, err := p.doPhase2STSCallback(reqCtx, location+sep+"clientSign="+url.QueryEscape(base64.StdEncoding.EncodeToString(sum[:])))
	if err != nil {
		return MimoCredentials{}, err
	}
	if strings.TrimSpace(cookies["serviceToken"]) == "" {
		return MimoCredentials{}, errors.New("mimo sso phase2 failed: serviceToken cookie not found")
	}
	out := creds
	out.ServiceToken, out.CUserID, out.MimoPH = strings.TrimSpace(cookies["serviceToken"]), qoderFirstTrimNonEmpty(cookies["cUserId"], cUserID, out.CUserID), qoderFirstTrimNonEmpty(cookies["mimo_ph"], out.MimoPH)
	EnsureMimoDerivedFields(&out)
	return out, nil
}

func (p *MimoTokenProvider) doPhase2STSCallback(ctx context.Context, stsURL string) (map[string]string, error) {
	collected := make(map[string]string, 6)
	jar, _ := cookiejar.New(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, stsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := cloneMimoHTTPClient(p.httpClient, func(r *http.Request, via []*http.Request) error {
		if r != nil && r.Response != nil {
			recordMimoHTTPCookies(collected, r.Response.Cookies())
		}
		if len(via) >= mimoMaxRedirects {
			return http.ErrUseLastResponse
		}
		return nil
	}, jar).Do(req)
	if err != nil {
		return nil, errors.New("mimo sso phase2 request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	recordMimoHTTPCookies(collected, resp.Cookies())
	if jar != nil && req.URL != nil {
		recordMimoHTTPCookies(collected, jar.Cookies(req.URL))
		if resp.Request != nil && resp.Request.URL != nil {
			recordMimoHTTPCookies(collected, jar.Cookies(resp.Request.URL))
		}
	}
	return collected, nil
}
