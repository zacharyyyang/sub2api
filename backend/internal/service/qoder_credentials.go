package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	qoderCredAccessToken, qoderCredTokenAlias = "access_token", "token"
	qoderCredRefreshToken, qoderCredUID       = "refresh_token", "uid"
	qoderCredOrgID, qoderCredMachineID        = "org_id", "machine_id"
	qoderCredUMIDToken, qoderCredMachineType  = "umid_token", "machine_type"
	qoderCredTokenExpiresAt                   = "token_expires_at"
	qoderCredRefreshTokenExpiresAt            = "refresh_token_expires_at"
	defaultQoderMachineType                   = "win32"
)

// QoderCredentials 表示 Qoder 原生平台的完整凭据结构（9 字段）。
type QoderCredentials struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	UID                   string `json:"uid"`
	OrgID                 string `json:"org_id"`
	MachineID             string `json:"machine_id"`
	UMIDToken             string `json:"umid_token"`
	MachineType           string `json:"machine_type"`
	TokenExpiresAt        int64  `json:"token_expires_at"`
	RefreshTokenExpiresAt int64  `json:"refresh_token_expires_at"`
}

// ValidateQoderRequiredCredentials 校验 Qoder 账号必填四键（access_token/token、refresh_token、uid、org_id）。
func ValidateQoderRequiredCredentials(credentials map[string]any) error {
	if len(credentials) == 0 {
		return errors.New("qoder credentials are required")
	}
	if qoderFirstTrimNonEmpty(qoderCredString(credentials, qoderCredAccessToken), qoderCredString(credentials, qoderCredTokenAlias)) == "" {
		return fmt.Errorf("missing required qoder credential: %s", qoderCredAccessToken)
	}
	for _, key := range []string{qoderCredRefreshToken, qoderCredUID, qoderCredOrgID} {
		if strings.TrimSpace(qoderCredString(credentials, key)) == "" {
			return fmt.Errorf("missing required qoder credential: %s", key)
		}
	}
	return nil
}

// EnsureDerivedFields 按 uid 与 org_id 确定性补齐缺省的 machine_id、umid_token 与 machine_type。
func EnsureDerivedFields(creds *QoderCredentials) {
	if creds == nil {
		return
	}
	creds.AccessToken, creds.RefreshToken = strings.TrimSpace(creds.AccessToken), strings.TrimSpace(creds.RefreshToken)
	creds.UID, creds.OrgID = strings.TrimSpace(creds.UID), strings.TrimSpace(creds.OrgID)
	creds.MachineID, creds.UMIDToken = strings.TrimSpace(creds.MachineID), strings.TrimSpace(creds.UMIDToken)
	creds.MachineType = strings.TrimSpace(creds.MachineType)
	if creds.MachineID == "" {
		sum := sha256.Sum256([]byte("qoder-machine:" + creds.UID + ":" + creds.OrgID))
		creds.MachineID = hex.EncodeToString(sum[:])
	}
	if creds.UMIDToken == "" {
		sum := sha256.Sum256([]byte("qoder-umid:" + creds.UID + ":" + creds.OrgID))
		creds.UMIDToken = "B42" + strings.ToUpper(hex.EncodeToString(sum[:])[:32])
	}
	if creds.MachineType == "" {
		creds.MachineType = defaultQoderMachineType
	}
}

// ParseQoderCredentials 从 credentials map 解析并归一化 QoderCredentials。
func ParseQoderCredentials(credentials map[string]any) (*QoderCredentials, error) {
	if err := ValidateQoderRequiredCredentials(credentials); err != nil {
		return nil, err
	}
	creds := &QoderCredentials{
		AccessToken:           qoderFirstTrimNonEmpty(qoderCredString(credentials, qoderCredAccessToken), qoderCredString(credentials, qoderCredTokenAlias)),
		RefreshToken:          qoderCredString(credentials, qoderCredRefreshToken),
		UID:                   qoderCredString(credentials, qoderCredUID),
		OrgID:                 qoderCredString(credentials, qoderCredOrgID),
		MachineID:             qoderCredString(credentials, qoderCredMachineID),
		UMIDToken:             qoderCredString(credentials, qoderCredUMIDToken),
		MachineType:           qoderCredString(credentials, qoderCredMachineType),
		TokenExpiresAt:        qoderCredInt64(credentials, qoderCredTokenExpiresAt),
		RefreshTokenExpiresAt: qoderCredInt64(credentials, qoderCredRefreshTokenExpiresAt),
	}
	EnsureDerivedFields(creds)
	return creds, nil
}

// NormalizeQoderCredentials 解析凭据并将归一化及派生后的字段写回 credentials map（同时剥离兼容别名键 token）。
func NormalizeQoderCredentials(credentials map[string]any) (*QoderCredentials, error) {
	creds, err := ParseQoderCredentials(credentials)
	if err != nil {
		return nil, err
	}
	delete(credentials, qoderCredTokenAlias)
	credentials[qoderCredAccessToken], credentials[qoderCredRefreshToken] = creds.AccessToken, creds.RefreshToken
	credentials[qoderCredUID], credentials[qoderCredOrgID] = creds.UID, creds.OrgID
	credentials[qoderCredMachineID], credentials[qoderCredUMIDToken] = creds.MachineID, creds.UMIDToken
	credentials[qoderCredMachineType] = creds.MachineType
	if creds.TokenExpiresAt > 0 {
		credentials[qoderCredTokenExpiresAt] = creds.TokenExpiresAt
	}
	if creds.RefreshTokenExpiresAt > 0 {
		credentials[qoderCredRefreshTokenExpiresAt] = creds.RefreshTokenExpiresAt
	}
	return creds, nil
}

// CredentialFingerprint 计算 Qoder 凭据的冲突比对指纹：SHA256(uid + "|" + org_id + "|" + refresh_token) 的 64 位小写 hex。
func CredentialFingerprint(creds *QoderCredentials) string {
	if creds == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(creds.UID) + "|" + strings.TrimSpace(creds.OrgID) + "|" + strings.TrimSpace(creds.RefreshToken)))
	return hex.EncodeToString(sum[:])
}

func qoderCredString(credentials map[string]any, key string) string {
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

func qoderCredInt64(credentials map[string]any, key string) int64 {
	switch v := credentials[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	default:
		return 0
	}
}

func qoderAnyToStr(v any) string { s, _ := v.(string); return s }

func qoderAnyToInt(v any) int {
	if f, ok := qoderToFloat64(v); ok {
		return int(f)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(qoderAnyToStr(v)))
	return n
}

func qoderToFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func qoderFirstNonZeroInt(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func qoderFirstTrimNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
func qoderFirstNonEmptyRaw(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (a *QoderNonStreamAggregator) FeedDataFrame(data map[string]any) ([]byte, error) {
	if a == nil || data == nil {
		return nil, nil
	}
	if id := qoderAnyToStr(data["id"]); id != "" {
		a.ID = id
	}
	delta, dm := map[string]any{}, map[string]any{}
	if d, ok := data["delta"].(map[string]any); ok {
		dm = d
	}
	if c := qoderFirstNonEmptyRaw(qoderAnyToStr(dm["content"]), qoderAnyToStr(data["content"]), extractQoderChoiceRaw(data, "content")); c != "" {
		a.Content.WriteString(c)
		delta["content"], a.EffectiveCount = c, a.EffectiveCount+1
	}
	if rc := qoderFirstNonEmptyRaw(qoderAnyToStr(dm["reasoning_content"]), qoderAnyToStr(data["reasoning_content"]), extractQoderChoiceRaw(data, "reasoning_content")); rc != "" {
		a.ReasoningContent.WriteString(rc)
		delta["reasoning_content"], a.EffectiveCount = rc, a.EffectiveCount+1
	}
	if tcs := extractQoderDeltaToolCalls(data, a.ToolCalls); len(tcs) > 0 {
		delta["tool_calls"], a.EffectiveCount = tcs, a.EffectiveCount+1
	}
	if fr := qoderFirstTrimNonEmpty(qoderAnyToStr(data["finish_reason"]), extractQoderChoiceRaw(data, "finish_reason")); fr != "" {
		a.FinishReason = fr
	}
	if u, ok := data["usage"].(map[string]any); ok {
		if p := qoderFirstNonZeroInt(qoderAnyToInt(u["prompt_tokens"]), qoderAnyToInt(u["input_tokens"])); p > 0 {
			a.PromptTokens = p
		}
		if c := qoderFirstNonZeroInt(qoderAnyToInt(u["completion_tokens"]), qoderAnyToInt(u["output_tokens"])); c > 0 {
			a.OutputTokens = c
		}
		if pd, ok := u["prompt_tokens_details"].(map[string]any); ok && qoderAnyToInt(pd["cached_tokens"]) > 0 {
			a.CachedInputTokens = qoderAnyToInt(pd["cached_tokens"])
		}
		a.TotalTokens = a.PromptTokens + a.OutputTokens
	}
	var frVal any
	if a.FinishReason != "" {
		frVal = a.FinishReason
	}
	return json.Marshal(map[string]any{"id": a.ID, "object": "chat.completion.chunk", "created": a.Created, "model": a.Model, "choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": frVal}}})
}

func (a *QoderNonStreamAggregator) BuildResponse() ([]byte, error) {
	msg := map[string]any{"role": "assistant", "content": a.Content.String()}
	if rc := a.ReasoningContent.String(); rc != "" {
		msg["reasoning_content"] = rc
	}
	if len(a.ToolCalls) > 0 {
		indices := make([]int, 0, len(a.ToolCalls))
		for idx := range a.ToolCalls {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		list := make([]map[string]any, 0, len(indices))
		for _, idx := range indices {
			if tc := a.ToolCalls[idx]; tc != nil {
				list = append(list, map[string]any{"id": tc.ID, "type": "function", "function": map[string]any{"name": tc.Name, "arguments": tc.Arguments.String()}})
			}
		}
		msg["tool_calls"] = list
		if a.FinishReason == "" {
			a.FinishReason = "tool_calls"
		}
	}
	if a.FinishReason == "" {
		a.FinishReason = "stop"
	}
	return json.Marshal(map[string]any{
		"id": a.ID, "object": "chat.completion", "created": a.Created, "model": a.Model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": a.FinishReason}},
		"usage":   map[string]int{"prompt_tokens": a.PromptTokens, "completion_tokens": a.OutputTokens, "total_tokens": a.TotalTokens},
	})
}

func extractQoderChoiceRaw(data map[string]any, field string) string {
	if choices, ok := data["choices"].([]any); ok && len(choices) > 0 {
		ch, _ := choices[0].(map[string]any)
		d, _ := ch["delta"].(map[string]any)
		m, _ := ch["message"].(map[string]any)
		return qoderFirstNonEmptyRaw(qoderAnyToStr(ch[field]), qoderAnyToStr(d[field]), qoderAnyToStr(m[field]))
	}
	return ""
}
