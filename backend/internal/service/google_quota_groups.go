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

// buildGoogleQuotaGroups 将 retrieveUserQuotaSummary 响应清洗为展示层结构。
// 语义（§2.3 / §4 决策 3、4、5、6）：
//   - remainingFraction 缺字段 / null（*float64 解出 nil）⇒ 该桶丢弃（视同无数据，不落 0）。
//     nil 判空必须先于 [0,1] 范围检查，否则对 nil 指针取值即 panic。
//   - 越界（<0 / >1 / NaN）⇒ 丢弃，不做钳位。
//   - 合法 0 ⇒ 保留，utilization = 100（额度已用尽是一条真实数据，与「无数据」必须可区分）。
//   - 组内所有桶被丢 ⇒ 该组不输出；所有组都不有效 ⇒ 返回 nil（JSON 字段缺席，前端不渲染）。
//   - 组 / 桶超 8 ⇒ 截断保留前 N 条并 Warn 一次（截断不报错）。
func buildGoogleQuotaGroups(resp *antigravity.UserQuotaSummaryResponse) []GoogleQuotaGroup {
	if resp == nil {
		return nil
	}

	var groups []GoogleQuotaGroup
	truncated := false
	for gi, grp := range resp.Groups {
		if gi >= maxGoogleQuotaGroups {
			truncated = true
			break
		}

		var windows []GoogleQuotaWindow
		for bi, bucket := range grp.Buckets {
			if bi >= maxGoogleQuotaBuckets {
				truncated = true
				break
			}
			if w, ok := buildGoogleQuotaWindow(bucket); ok {
				windows = append(windows, w)
			}
		}
		if len(windows) == 0 {
			// 组内所有桶都缺值 / 越界 ⇒ 该组不输出（不输出空 windows 的组）
			continue
		}

		groups = append(groups, GoogleQuotaGroup{
			Kind:    bucketIDGroupKind(windows[0].BucketID),
			Label:   cleanLabel(grp.DisplayName),
			Windows: windows,
		})
	}

	if truncated {
		slog.Warn("google quota groups truncated",
			"maxGroups", maxGoogleQuotaGroups,
			"maxBuckets", maxGoogleQuotaBuckets,
		)
	}
	return groups
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
