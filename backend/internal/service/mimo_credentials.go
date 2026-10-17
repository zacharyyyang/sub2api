package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	DefaultMimoBaseURL = "https://mimo-server-cn.xiaomimimo.com"

	mimoCredUserID       = "user_id"
	mimoCredPassToken    = "pass_token"
	mimoCredCUserID      = "c_user_id"
	mimoCredServiceToken = "service_token"
	mimoCredMimoPH       = "mimo_ph"
	mimoCredDeviceID     = "device_id"
	mimoCredBaseURL      = "base_url"
)

type MimoCredentials struct {
	UserID       string `json:"user_id"`
	PassToken    string `json:"pass_token,omitempty"`
	CUserID      string `json:"c_user_id,omitempty"`
	ServiceToken string `json:"service_token,omitempty"`
	MimoPH       string `json:"mimo_ph,omitempty"`
	DeviceID     string `json:"device_id"`
	BaseURL      string `json:"base_url"`
}

func ValidateMimoRequiredCredentials(credentials map[string]any) error {
	if len(credentials) == 0 {
		return errors.New("mimo credentials are required")
	}
	if strings.TrimSpace(mimoCredString(credentials, mimoCredUserID)) == "" {
		return fmt.Errorf("missing required mimo credential: %s", mimoCredUserID)
	}
	if strings.TrimSpace(mimoCredString(credentials, mimoCredPassToken)) == "" && strings.TrimSpace(mimoCredString(credentials, mimoCredServiceToken)) == "" {
		return errors.New("missing required mimo credential: pass_token or service_token")
	}
	if rawBaseURL := strings.TrimSpace(mimoCredString(credentials, mimoCredBaseURL)); rawBaseURL != "" {
		return validateMimoBaseURL(rawBaseURL)
	}
	return nil
}

func FormatUUIDFromSHA256(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	var b [16]byte
	copy(b[:], sum[:16])
	b[6], b[8] = (b[6]&0x0f)|0x40, (b[8]&0x3f)|0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func EnsureMimoDerivedFields(creds *MimoCredentials) {
	if creds == nil {
		return
	}
	creds.UserID, creds.PassToken = strings.TrimSpace(creds.UserID), strings.TrimSpace(creds.PassToken)
	creds.CUserID, creds.ServiceToken = strings.TrimSpace(creds.CUserID), strings.TrimSpace(creds.ServiceToken)
	creds.MimoPH, creds.DeviceID = strings.TrimSpace(creds.MimoPH), strings.TrimSpace(creds.DeviceID)
	creds.BaseURL = strings.TrimRight(strings.TrimSpace(creds.BaseURL), "/")
	if creds.BaseURL == "" {
		creds.BaseURL = DefaultMimoBaseURL
	}
	if creds.DeviceID == "" {
		creds.DeviceID = "wb_" + FormatUUIDFromSHA256("mimo-device:"+creds.UserID)
	}
}

func ParseMimoCredentials(credentials map[string]any) (*MimoCredentials, error) {
	if err := ValidateMimoRequiredCredentials(credentials); err != nil {
		return nil, err
	}
	creds := &MimoCredentials{
		UserID: mimoCredString(credentials, mimoCredUserID), PassToken: mimoCredString(credentials, mimoCredPassToken),
		CUserID: mimoCredString(credentials, mimoCredCUserID), ServiceToken: mimoCredString(credentials, mimoCredServiceToken),
		MimoPH: mimoCredString(credentials, mimoCredMimoPH), DeviceID: mimoCredString(credentials, mimoCredDeviceID),
		BaseURL: mimoCredString(credentials, mimoCredBaseURL),
	}
	EnsureMimoDerivedFields(creds)
	return creds, nil
}

func NormalizeMimoCredentials(credentials map[string]any) (*MimoCredentials, error) {
	creds, err := ParseMimoCredentials(credentials)
	if err != nil {
		return nil, err
	}
	credentials[mimoCredUserID], credentials[mimoCredDeviceID], credentials[mimoCredBaseURL] = creds.UserID, creds.DeviceID, creds.BaseURL
	for k, v := range map[string]string{mimoCredPassToken: creds.PassToken, mimoCredCUserID: creds.CUserID, mimoCredServiceToken: creds.ServiceToken, mimoCredMimoPH: creds.MimoPH} {
		if v != "" {
			credentials[k] = v
		}
	}
	return creds, nil
}

func MimoCredentialFingerprint(creds *MimoCredentials) string {
	if creds == nil {
		return ""
	}
	tokenPart := qoderFirstTrimNonEmpty(creds.PassToken, creds.ServiceToken)
	baseURL := qoderFirstTrimNonEmpty(strings.TrimRight(strings.TrimSpace(creds.BaseURL), "/"), DefaultMimoBaseURL)
	sum := sha256.Sum256([]byte(strings.TrimSpace(creds.UserID) + "|" + tokenPart + "|" + baseURL))
	return hex.EncodeToString(sum[:])
}

func BuildMimoInferenceCookie(creds MimoCredentials) string {
	var parts []string
	for _, kv := range [][2]string{{"userId", creds.UserID}, {"cUserId", creds.CUserID}, {"serviceToken", creds.ServiceToken}, {"mimo_ph", creds.MimoPH}} {
		if v := strings.TrimSpace(kv[1]); v != "" {
			parts = append(parts, kv[0]+"="+v)
		}
	}
	return strings.Join(parts, "; ")
}

func validateMimoBaseURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("invalid mimo base_url: %s", raw)
	}
	return nil
}

func mimoCredString(credentials map[string]any, key string) string {
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

type mimoAccountRepo interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
	UpdateCredentials(ctx context.Context, id int64, credentials map[string]any) error
}

type mimoAccountRepoAdapter struct{ AccountRepository }

func (a *mimoAccountRepoAdapter) UpdateCredentials(ctx context.Context, id int64, credentials map[string]any) error {
	if u, ok := a.AccountRepository.(interface {
		UpdateCredentials(ctx context.Context, id int64, credentials map[string]any) error
	}); ok {
		return u.UpdateCredentials(ctx, id, credentials)
	}
	_, err := a.AccountRepository.BulkUpdate(ctx, []int64{id}, AccountBulkUpdate{Credentials: credentials})
	return err
}

func mergeMimoAccountCredentialsMap(account, latest *Account, refreshed MimoCredentials) map[string]any {
	updated := make(map[string]any, 10)
	for _, src := range []*Account{account, latest} {
		if src != nil {
			for k, v := range src.Credentials {
				updated[k] = v
			}
		}
	}
	for k, v := range map[string]string{
		mimoCredUserID: refreshed.UserID, mimoCredServiceToken: refreshed.ServiceToken,
		mimoCredDeviceID: refreshed.DeviceID, mimoCredBaseURL: refreshed.BaseURL,
		mimoCredPassToken: refreshed.PassToken, mimoCredCUserID: refreshed.CUserID, mimoCredMimoPH: refreshed.MimoPH,
	} {
		if v != "" {
			updated[k] = v
		}
	}
	return updated
}

type MimoMessagesCompatError struct {
	Status  int
	Code    string
	Message string
}

func (e *MimoMessagesCompatError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func newMimoModalityError(model, modality string) *MimoMessagesCompatError {
	return &MimoMessagesCompatError{Status: 422, Code: "MODEL_MODALITY_UNSUPPORTED", Message: fmt.Sprintf("model %s does not support %s blocks", model, modality)}
}

func extractMimoChatUsage(data map[string]any) OpenAIUsage {
	var u OpenAIUsage
	if um, ok := data["usage"].(map[string]any); ok {
		u.InputTokens = qoderFirstNonZeroInt(qoderAnyToInt(um["prompt_tokens"]), qoderAnyToInt(um["input_tokens"]))
		u.OutputTokens = qoderFirstNonZeroInt(qoderAnyToInt(um["completion_tokens"]), qoderAnyToInt(um["output_tokens"]))
		if pd, ok := um["prompt_tokens_details"].(map[string]any); ok {
			u.CacheReadInputTokens = qoderAnyToInt(pd["cached_tokens"])
		}
	}
	return u
}

func convertMimoToolChoice(raw any) any {
	tm, _ := raw.(map[string]any)
	switch strings.TrimSpace(qoderAnyToStr(tm["type"])) {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": qoderAnyToStr(tm["name"])}}
	}
	return nil
}

func resolveMimoAnthropicReasoningEffort(req map[string]any, modelEffort string) string {
	if th, ok := req["thinking"].(map[string]any); ok {
		switch strings.ToLower(strings.TrimSpace(qoderAnyToStr(th["type"]))) {
		case "disabled":
			return ""
		case "enabled", "adaptive":
			if budget := qoderAnyToInt(th["budget_tokens"]); budget >= 16000 || budget <= 0 {
				return "high"
			} else if budget >= 4000 {
				return "medium"
			}
			return "low"
		}
	}
	rawEffort := strings.ToLower(strings.TrimSpace(qoderAnyToStr(req["effort"])))
	if oc, ok := req["output_config"].(map[string]any); ok && rawEffort == "" {
		rawEffort = strings.ToLower(strings.TrimSpace(qoderAnyToStr(oc["effort"])))
	}
	switch rawEffort {
	case "high", "xhigh", "max":
		return "high"
	case "medium", "low":
		return rawEffort
	}
	return modelEffort
}

func convertAnthropicImageBlockToURL(bm map[string]any) string {
	src, _ := bm["source"].(map[string]any)
	if src == nil {
		return strings.TrimSpace(qoderAnyToStr(bm["url"]))
	}
	if strings.TrimSpace(qoderAnyToStr(src["type"])) == "url" {
		return strings.TrimSpace(qoderAnyToStr(src["url"]))
	}
	if strings.TrimSpace(qoderAnyToStr(src["type"])) == "base64" {
		if data := strings.TrimSpace(qoderAnyToStr(src["data"])); data != "" {
			return fmt.Sprintf("data:%s;base64,%s", qoderFirstTrimNonEmpty(qoderAnyToStr(src["media_type"]), "image/png"), data)
		}
	}
	return ""
}

func recordMimoHTTPCookies(dst map[string]string, cs []*http.Cookie) {
	for _, ck := range cs {
		if ck != nil && ck.Name != "" && strings.TrimSpace(ck.Value) != "" {
			dst[ck.Name] = strings.TrimSpace(ck.Value)
		}
	}
}

func cloneMimoHTTPClient(base *http.Client, checkRedirect func(*http.Request, []*http.Request) error, jar http.CookieJar) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	clone := *base
	if clone.Timeout <= 0 {
		clone.Timeout = mimoSSOHTTPTimeout
	}
	clone.CheckRedirect, clone.Jar = checkRedirect, jar
	return &clone
}
