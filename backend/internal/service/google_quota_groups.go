package service

import (
	"log/slog"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// 清洗护栏：与参照实现一致，组 / 桶各 ≤8，超出截断保留前 N 条（防御性护栏，实测上游仅 2 组 4 桶）。
const (
	maxGoogleQuotaGroups  = 8
	maxGoogleQuotaBuckets = 8
	maxQuotaGroupLabelLen = 80
)

// buildGoogleQuotaGroups 将 prod / daily 两域 retrieveUserQuotaSummary 响应合并清洗为展示层结构。
// 合并语义（§2.3 / §4 决策 13-16）：清洗规则沿用 v1（nil 判空先于范围检查 / 越界丢弃 / 合法 0 保留 / 全无效不输出）；
// 合并键 = (kind, bucketId)：gemini / other 账号级（Domain=""）两域并集归为一条、同窗异值取 daily；
// claude_gpt 每域各自一组（Domain = prod / daily），异值并存不跨域合并；单域失败 ⇒ 只用另一域；双 nil ⇒ nil。
// 组 / 桶超 8 截断保留前 N 条 + Warn 一次（合并并集桶数可能超 8，防御）；输出序 = [gemini, other, claude(prod), claude(daily)]。
func buildGoogleQuotaGroups(prodResp, dailyResp *antigravity.UserQuotaSummaryResponse) []GoogleQuotaGroup {
	prod := buildDomainQuota(prodResp)
	daily := buildDomainQuota(dailyResp)

	var groups []GoogleQuotaGroup
	truncated := false

	// 账号级（gemini / other）：两域并集，daily 桶在前 ⇒ 同 (kind, bucketId) 异值取 daily（§4 决策 13）
	for _, kind := range []string{"gemini", "other"} {
		dailyKQ, prodKQ := daily.byKind[kind], prod.byKind[kind]
		if dailyKQ == nil && prodKQ == nil {
			continue
		}
		windows := mergeKindWindows(dailyKQ, prodKQ, &truncated)
		if len(windows) == 0 {
			continue
		}
		if len(groups) >= maxGoogleQuotaGroups {
			truncated = true
			break
		}
		label := ""
		if dailyKQ != nil {
			label = dailyKQ.label
		} else {
			label = prodKQ.label
		}
		groups = append(groups, GoogleQuotaGroup{Kind: kind, Label: label, Windows: windows})
	}

	// claude_gpt：每域各自一组（§2.3，不跨域合并），输出序固定 prod 在前
	for _, dc := range []struct {
		domain string
		kq     *kindQuota
	}{{"prod", prod.byKind["claude_gpt"]}, {"daily", daily.byKind["claude_gpt"]}} {
		windows := mergeKindWindows(nil, dc.kq, &truncated)
		if len(windows) == 0 {
			continue
		}
		if len(groups) >= maxGoogleQuotaGroups {
			truncated = true
			break
		}
		groups = append(groups, GoogleQuotaGroup{Kind: "claude_gpt", Label: dc.kq.label, Domain: dc.domain, Windows: windows})
	}

	if truncated {
		slog.Warn("google quota groups truncated",
			"maxGroups", maxGoogleQuotaGroups,
			"maxBuckets", maxGoogleQuotaBuckets,
		)
	}
	return groups
}

// kindQuota 单一 kind 的有效桶集合（已按 bucketId 去重）+ 该 kind 首个组的标签。
type kindQuota struct {
	buckets []antigravity.UserQuotaBucket
	seen    map[string]bool // bucketId 去重：(kind, bucketId) 为合并键，跨组同桶只取首个
	label   string
}

// domainQuota 单域清洗中间形态：kind → kindQuota。
type domainQuota struct {
	byKind map[string]*kindQuota
}

// buildDomainQuota 过滤单域响应为「kind → 有效桶」中间形态。
// 组分类沿用 v1：组 kind 由首个有效桶的 bucketId 前缀定；组内全无效 ⇒ 该组不接入。
// 不做组 / 桶截断——截断由合并后的最终输出统一承担（§2.3「截断保留前 N 条」）。
func buildDomainQuota(resp *antigravity.UserQuotaSummaryResponse) domainQuota {
	q := domainQuota{byKind: map[string]*kindQuota{}}
	if resp == nil {
		return q
	}
	for _, grp := range resp.Groups {
		// 首个有效桶（判空先于范围检查，v1 同款防 panic）
		var first *antigravity.UserQuotaBucket
		for i := range grp.Buckets {
			b := &grp.Buckets[i]
			if b.RemainingFraction == nil || !validFraction(*b.RemainingFraction) {
				continue
			}
			first = b
			break
		}
		if first == nil {
			continue // 组内所有桶无效 ⇒ 该组不输出（v1 同款）
		}

		kind := bucketIDGroupKind(first.BucketID)
		kq := q.byKind[kind]
		if kq == nil {
			kq = &kindQuota{seen: map[string]bool{}, label: cleanLabel(grp.DisplayName)}
			q.byKind[kind] = kq
		}
		for i := range grp.Buckets {
			b := grp.Buckets[i]
			if b.RemainingFraction == nil || !validFraction(*b.RemainingFraction) || kq.seen[b.BucketID] {
				continue
			}
			kq.seen[b.BucketID] = true
			kq.buckets = append(kq.buckets, b)
		}
	}
	return q
}

// validFraction 判断 remainingFraction 是否在合法 [0,1] 闭区间内（NaN 恒 false）。
func validFraction(rf float64) bool {
	return rf == rf && rf >= 0 && rf <= 1
}

// mergeKindWindows 将若干已去重 kindQuota 的桶按序并成窗口：daily 在前 ⇒ 同 (kind, bucketId) 取 daily 值；
// 超桶上限截断并置 truncated（并集可能超 8，防御）。入桶均已过 nil / 越界过滤，无再丢。
func mergeKindWindows(dailyKQ, prodKQ *kindQuota, truncated *bool) []GoogleQuotaWindow {
	var out []GoogleQuotaWindow
	seen := map[string]bool{}
	appendFrom := func(src *kindQuota) {
		if src == nil {
			return
		}
		for i := range src.buckets {
			b := src.buckets[i]
			if seen[b.BucketID] {
				continue
			}
			seen[b.BucketID] = true
			if len(out) >= maxGoogleQuotaBuckets {
				*truncated = true
				break
			}
			w, _ := buildGoogleQuotaWindow(b)
			out = append(out, w)
		}
	}
	appendFrom(dailyKQ)
	appendFrom(prodKQ)
	return out
}

// buildGoogleQuotaWindow 将单个上游桶清洗为一个展示窗口。
// ok=false ⇒ 该桶缺值 / 越界，调用方丢弃（不落 0、不钳位）。
func buildGoogleQuotaWindow(bucket antigravity.UserQuotaBucket) (GoogleQuotaWindow, bool) {
	// 判空先于范围检查：nil ⇒ 缺字段 / JSON null，视同无数据，防 nil 取值 panic
	if bucket.RemainingFraction == nil {
		return GoogleQuotaWindow{}, false
	}
	rf := *bucket.RemainingFraction
	if rf != rf || rf < 0 || rf > 1 { // NaN（rf!=rf）/ 越界 ⇒ 丢弃
		return GoogleQuotaWindow{}, false
	}

	// 与既有逐模型同一算法、同一方向：利用例 = int((1-remainingFraction)*100)，截断取整
	utilization := int((1.0 - rf) * 100)

	// resetTime 校验：仅在上游值为合法 RFC3339 时保留，否则留空（前端不显示重置时间，仍显示额度）
	resetTime := ""
	if bucket.ResetTime != "" {
		if _, err := time.Parse(time.RFC3339, bucket.ResetTime); err == nil {
			resetTime = bucket.ResetTime
		}
	}

	return GoogleQuotaWindow{
		BucketID:    bucket.BucketID,
		Kind:        bucketIDWindowKind(bucket.BucketID),
		Label:       cleanLabel(bucket.DisplayName),
		Utilization: utilization,
		ResetTime:   resetTime,
	}, true
}

// bucketIDGroupKind 按 bucketId 前缀归类组：gemini* ⇒ gemini、3p* ⇒ claude_gpt、其余 ⇒ other。
// 用 bucketId 而非 displayName / window：bucketId 是稳定 ID，文案随上游改版会变（§4 决策 6）。
func bucketIDGroupKind(bucketID string) string {
	switch {
	case strings.HasPrefix(bucketID, "gemini"):
		return "gemini"
	case strings.HasPrefix(bucketID, "3p"):
		return "claude_gpt"
	default:
		return "other"
	}
}

// bucketIDWindowKind 按 bucketId 后缀归类窗口：*-5h ⇒ five_hour、*-weekly ⇒ seven_day、其余 ⇒ other。
func bucketIDWindowKind(bucketID string) string {
	switch {
	case strings.HasSuffix(bucketID, "-5h"):
		return "five_hour"
	case strings.HasSuffix(bucketID, "-weekly"):
		return "seven_day"
	default:
		return "other"
	}
}

// cleanLabel 去掉控制字符（含换行）并截断至 80 字符（rune 计宽，避免 CJK 被字节截坏）。
func cleanLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	runes := []rune(s)
	if len(runes) > maxQuotaGroupLabelLen {
		runes = runes[:maxQuotaGroupLabelLen]
	}
	return string(runes)
}
