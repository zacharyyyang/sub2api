package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// wbQuotaFetchTimeout 是积分面板探测的独立超时上限（旁路取值，不阻塞监控循环）。
	wbQuotaFetchTimeout = 10 * time.Second
)

// WbEnterpriseCredits 是 wb 企业账号的积分（credit）口径余额。
// 设计只认 credit 项的 remaining 口径；expires_at 字符串直通。
type WbEnterpriseCredits struct {
	Remaining int64  `json:"remaining"`
	Total     int64  `json:"total"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// WbQuotaFetcher 是 wb 企业账号的积分额度获取器（管理面直连，值通道旁路）：
// 失败仅 slog.Warn、字段 nil、无数据显示、不造 0、绝不写 Error / ErrorCode。
type WbQuotaFetcher struct {
	httpClient        *http.Client
	overviewURLPrefix string // 默认 DefaultWbOverviewURL；测试可注入
}

// NewWbQuotaFetcher 创建 WbQuotaFetcher。httpClient 为空用 http.DefaultClient
// （尊重环境代理，即「直连」）；overviewURLPrefix 为空用常量默认前缀。
func NewWbQuotaFetcher(httpClient *http.Client, overviewURLPrefix string) *WbQuotaFetcher {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if overviewURLPrefix == "" {
		overviewURLPrefix = DefaultWbOverviewURL
	}
	return &WbQuotaFetcher{httpClient: httpClient, overviewURLPrefix: overviewURLPrefix}
}

// CanFetch 检查此账户是否可获取 wb 积分额度：wb 平台 + 已配 pt_key。
func (f *WbQuotaFetcher) CanFetch(account *Account) bool {
	return account != nil && account.Platform == PlatformWB &&
		strings.TrimSpace(account.GetCredential(wbCredentialPTKey)) != ""
}

// FetchQuota 获取 wb 企业积分面板。proxyURL 忽略（管理面直连，设计未给 wb 配代理链路）。
// 任何失败（HTTP 非 2xx / 响应畸形 / credit 项不可定位）→ 值通道 Warn：
// 返回含 UpdatedAt 的空 UsageInfo，WbEnterpriseCredits 为 nil，不返回 error。
func (f *WbQuotaFetcher) FetchQuota(ctx context.Context, account *Account, proxyURL string) (*QuotaResult, error) {
	now := time.Now()
	empty := &QuotaResult{UsageInfo: &UsageInfo{UpdatedAt: &now}}
	if f == nil || account == nil || account.Platform != PlatformWB {
		return empty, nil
	}
	ptKey := strings.TrimSpace(account.GetCredential(wbCredentialPTKey))
	enterpriseID := strings.TrimSpace(account.GetCredential(wbCredentialEnterpriseID))
	if ptKey == "" || enterpriseID == "" {
		slog.Warn("wb quota fetch skipped: missing pt_key or enterprise_id", "account_id", account.ID)
		return empty, nil
	}

	client := f.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	prefix := f.overviewURLPrefix
	if prefix == "" {
		prefix = DefaultWbOverviewURL
	}
	endpoint := prefix + "/" + url.PathEscape(enterpriseID) + "/openapi/resources/overview"

	fetchCtx, cancel := context.WithTimeout(ctx, wbQuotaFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		slog.Warn("wb quota request construction failed", "account_id", account.ID, "error", err)
		return empty, nil
	}
	req.Header.Set("Authorization", "Bearer "+ptKey)

	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("wb quota fetch failed", "account_id", account.ID, "error", err)
		return empty, nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		slog.Warn("wb quota response read failed", "account_id", account.ID, "error", err)
		return empty, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		slog.Warn("wb quota endpoint returned non-2xx", "account_id", account.ID, "status", resp.StatusCode)
		return empty, nil
	}

	var root map[string]any
	// UseNumber 保留 JSON 整数精度（默认 float64 会在 >2^53 时舍入）；
	// 数值统一经 wbNumeric 读取，json.Number 分支按原值解析，>int64 上界拒绝。
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		slog.Warn("wb quota response is not valid JSON", "account_id", account.ID, "error", err)
		return empty, nil
	}

	info := &UsageInfo{UpdatedAt: &now}
	node, ok := findWbCreditNode(root)
	if !ok {
		slog.Warn("wb quota response has no recognizable credit item", "account_id", account.ID)
		return &QuotaResult{UsageInfo: info}, nil
	}
	info.WbEnterpriseCredits = parseWbEnterpriseCredits(node)
	if info.WbEnterpriseCredits == nil {
		// credit 项存在但 remaining/total 缺失或畸形 → 无数据显示
		slog.Warn("wb credit item is malformed", "account_id", account.ID)
	}
	return &QuotaResult{UsageInfo: info}, nil
}

// findWbCreditNode 在响应 JSON 中宽松定位 credit 项。
// 顶层 JSON 形状已实证（2026-10-08 AC5 真机）：data.items[] 元素以 resourceType="credit" 标记。
// 先走固定候选路径（顶层 credit / data.credit /
// data.credits|resources|items 列表中带 credit 类型标记的项），再无命中走 DFS 兜底：
// 任何同时含数字 remaining 与 total 且类型标记（type/kind/resource/resourceType/name）含 "credit" 的节点。
func findWbCreditNode(root map[string]any) (map[string]any, bool) {
	if node, ok := wbObjMap(root["credit"]); ok && wbHasRemainingTotal(node) {
		return node, true
	}
	if data, ok := wbObjMap(root["data"]); ok {
		if node, ok := wbObjMap(data["credit"]); ok && wbHasRemainingTotal(node) {
			return node, true
		}
		for _, listKey := range []string{"credits", "resources", "items"} {
			if list, ok := data[listKey].([]any); ok {
				for _, item := range list {
					if node, ok := wbObjMap(item); ok && wbIsCreditNode(node) {
						return node, true
					}
				}
			}
		}
	}
	return wbFindCreditNodeDFS(root)
}

// wbFindCreditNodeDFS 深度优先遍历，寻找带 credit 类型标记且含 remaining/total 的节点。
func wbFindCreditNodeDFS(node any) (map[string]any, bool) {
	switch v := node.(type) {
	case map[string]any:
		if wbIsCreditNode(v) {
			return v, true
		}
		for _, child := range v {
			if found, ok := wbFindCreditNodeDFS(child); ok {
				return found, true
			}
		}
	case []any:
		for _, child := range v {
			if found, ok := wbFindCreditNodeDFS(child); ok {
				return found, true
			}
		}
	}
	return nil, false
}

// wbObjMap 将任意值断言为 map[string]any。
func wbObjMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// wbHasRemainingTotal 宽松判定节点是 credit 项：同时含可解析的非负数字 remaining 与 total。
// remaining 为 0 是合法数据（余额用尽），不等于「造 0 兜底」。
func wbHasRemainingTotal(node map[string]any) bool {
	_, okR := wbNumeric(node["remaining"])
	_, okT := wbNumeric(node["total"])
	return okR && okT
}

// wbIsCreditNode 判定节点是 credit 项：含 remaining/total 且类型标记含 "credit"。
// 类型标记键含真实 API 实证键 resourceType（2026-10-08 AC5 真机：items[] 元素标记键为 resourceType）。
func wbIsCreditNode(node map[string]any) bool {
	if !wbHasRemainingTotal(node) {
		return false
	}
	for _, key := range []string{"type", "kind", "resource", "resourceType", "name"} {
		if s, ok := node[key].(string); ok && strings.Contains(strings.ToLower(s), "credit") {
			return true
		}
	}
	return false
}

// wbNumeric 将 JSON 数值（float64 / json.Number / 数字字符串）解析为非负 int64。
// 负数、畸形或缺失 → 不可用。
func wbNumeric(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		// 整数契约：仅接受可精确表示的非负整数（小数截断会制造 0/缩水，必须拒绝）。
		// 上界判定：float64 精度 2^53 之后的跳变使 float64(math.MaxInt64) == 2^63，
		// 按 n > math.MaxInt64 比较会漏掉 n == 2^63（int64 转换回绕成负数），
		// 故用 n >= 1<<63（即 >= 2^63）拒绝——2^63 起一律不可表示。
		if n < 0 || n != n || n != math.Trunc(n) || n >= 1<<63 {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < 0 {
			return 0, false
		}
		return i, true
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil || i < 0 {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}

// parseWbEnterpriseCredits 从 credit 节点提取三项。remaining/total 缺失或畸形 → 整体 nil
// （无数据显示）；expiresAt/expires_at 字符串直通、秒级数字规格化为日期。
func parseWbEnterpriseCredits(node map[string]any) *WbEnterpriseCredits {
	remaining, okR := wbNumeric(node["remaining"])
	total, okT := wbNumeric(node["total"])
	if !okR || !okT {
		return nil
	}
	credits := &WbEnterpriseCredits{Remaining: remaining, Total: total}
	if raw, ok := node["expiresAt"]; ok {
		credits.ExpiresAt = wbExpiresAtString(raw)
	}
	if credits.ExpiresAt == "" {
		if raw, ok := node["expires_at"]; ok {
			credits.ExpiresAt = wbExpiresAtString(raw)
		}
	}
	return credits
}

// wbExpiresAtString 将 expires 值规格化为展示字符串：字符串直通；秒级数字 → YYYY-MM-DD。
func wbExpiresAtString(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		if v >= 0 {
			return time.Unix(int64(v), 0).UTC().Format("2006-01-02")
		}
	case json.Number:
		if i, err := v.Int64(); err == nil && i >= 0 {
			return time.Unix(i, 0).UTC().Format("2006-01-02")
		}
	}
	return ""
}
