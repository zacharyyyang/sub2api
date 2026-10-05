//go:build unit

package service

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// bucket 便捷构造上游桶
func bucket(id string, rf *float64) antigravity.UserQuotaBucket {
	return antigravity.UserQuotaBucket{
		BucketID:          id,
		DisplayName:       "Display " + id,
		Window:            "5h",
		ResetTime:         "2026-10-05T12:00:00Z",
		RemainingFraction: rf,
	}
}

// C1 正常：实测形状 2 组 × 2 窗口
func TestBuildGoogleQuotaGroups_Normal(t *testing.T) {
	resp := &antigravity.UserQuotaSummaryResponse{
		Groups: []antigravity.UserQuotaGroup{
			{
				DisplayName: "Gemini Models",
				Buckets: []antigravity.UserQuotaBucket{
					bucket("gemini-5h", floatPtr(0.97)),
					bucket("gemini-weekly", floatPtr(0.6)),
				},
			},
			{
				DisplayName: "Claude & GPT",
				Buckets: []antigravity.UserQuotaBucket{
					bucket("3p-5h", floatPtr(1.0)),
					bucket("3p-weekly", floatPtr(0.0)),
				},
			},
		},
	}

	groups := buildGoogleQuotaGroups(resp)
	require.Len(t, groups, 2)

	g0 := groups[0]
	require.Equal(t, "gemini", g0.Kind)
	require.Equal(t, "Gemini Models", g0.Label)
	require.Len(t, g0.Windows, 2)
	// utilization = int((1-remainingFraction)*100) 截断取整：0.97 ⇒ 3、0.6 ⇒ 40
	require.Equal(t, "gemini-5h", g0.Windows[0].BucketID)
	require.Equal(t, "five_hour", g0.Windows[0].Kind)
	require.Equal(t, 3, g0.Windows[0].Utilization)
	require.Equal(t, "gemini-weekly", g0.Windows[1].BucketID)
	require.Equal(t, "seven_day", g0.Windows[1].Kind)
	require.Equal(t, 40, g0.Windows[1].Utilization)

	g1 := groups[1]
	require.Equal(t, "claude_gpt", g1.Kind)
	require.Equal(t, "Claude & GPT", g1.Label)
	require.Len(t, g1.Windows, 2)
	// 1.0 ⇒ 0、0.0 ⇒ 100（合法零值保留，额度已用尽）
	require.Equal(t, 0, g1.Windows[0].Utilization)
	require.Equal(t, 100, g1.Windows[1].Utilization)

	// resetTime 合法 ⇒ 保留原样
	require.Equal(t, "2026-10-05T12:00:00Z", g0.Windows[0].ResetTime)
}

// C2 正常：resp == nil ⇒ nil（字段缺席 ⇒ 无数据不显示）
func TestBuildGoogleQuotaGroups_NilResponse(t *testing.T) {
	require.Nil(t, buildGoogleQuotaGroups(nil))
}

// C3 边界：空 groups ⇒ nil（不输出空切片，omitempty 后字段整体缺席）
func TestBuildGoogleQuotaGroups_EmptyGroups(t *testing.T) {
	require.Nil(t, buildGoogleQuotaGroups(&antigravity.UserQuotaSummaryResponse{}))
}

// C4 边界：9 组 × 9 桶 ⇒ 截断为 8 组 × 8 桶 + Warn 一次，不报错
func TestBuildGoogleQuotaGroups_Truncate(t *testing.T) {
	resp := &antigravity.UserQuotaSummaryResponse{}
	for gi := 0; gi < 9; gi++ {
		grp := antigravity.UserQuotaGroup{DisplayName: "G"}
		for bi := 0; bi < 9; bi++ {
			grp.Buckets = append(grp.Buckets, bucket("gemini-5h", floatPtr(0.5)))
		}
		resp.Groups = append(resp.Groups, grp)
	}

	groups := buildGoogleQuotaGroups(resp)
	require.Len(t, groups, 8)
	for _, g := range groups {
		require.Len(t, g.Windows, 8)
	}
}

// C5 异常：越界 / NaN 桶丢弃，其余保留
func TestBuildGoogleQuotaGroups_OutOfRangeDropped(t *testing.T) {
	resp := &antigravity.UserQuotaSummaryResponse{
		Groups: []antigravity.UserQuotaGroup{
			{
				DisplayName: "Mixed",
				Buckets: []antigravity.UserQuotaBucket{
					bucket("gemini-5h", floatPtr(1.5)),      // >1 ⇒ 丢
					bucket("gemini-weekly", floatPtr(-0.1)), // <0 ⇒ 丢
					bucket("3p-5h", floatPtr(math.NaN())),   // NaN ⇒ 丢
					bucket("3p-weekly", floatPtr(0.5)),      // 合法 ⇒ 保留
				},
			},
		},
	}

	groups := buildGoogleQuotaGroups(resp)
	require.Len(t, groups, 1)
	require.Len(t, groups[0].Windows, 1)
	require.Equal(t, "3p-weekly", groups[0].Windows[0].BucketID)
	require.Equal(t, 50, groups[0].Windows[0].Utilization)
}

// C6 异常：某组所有桶越界 ⇒ 该组丢弃（不输出空 windows 的组）
func TestBuildGoogleQuotaGroups_GroupAllInvalidDropped(t *testing.T) {
	resp := &antigravity.UserQuotaSummaryResponse{
		Groups: []antigravity.UserQuotaGroup{
			{
				DisplayName: "All Bad",
				Buckets: []antigravity.UserQuotaBucket{
					bucket("gemini-5h", floatPtr(2.0)),
					bucket("gemini-weekly", floatPtr(-3.0)),
				},
			},
			{
				DisplayName: "Gemini Models",
				Buckets: []antigravity.UserQuotaBucket{
					bucket("gemini-5h", floatPtr(0.2)),
				},
			},
		},
	}

	groups := buildGoogleQuotaGroups(resp)
	require.Len(t, groups, 1)
	require.Equal(t, "Gemini Models", groups[0].Label)
	require.Len(t, groups[0].Windows, 1)
}

// C7 异常：resetTime 不可解析 / 空 ⇒ window 保留、reset_time 为空
func TestBuildGoogleQuotaGroups_InvalidResetTime(t *testing.T) {
	resp := &antigravity.UserQuotaSummaryResponse{
		Groups: []antigravity.UserQuotaGroup{
			{
				DisplayName: "Gemini Models",
				Buckets: []antigravity.UserQuotaBucket{
					func() antigravity.UserQuotaBucket {
						b := bucket("gemini-5h", floatPtr(0.3))
						b.ResetTime = "not-a-date"
						return b
					}(),
					func() antigravity.UserQuotaBucket {
						b := bucket("gemini-weekly", floatPtr(0.4))
						b.ResetTime = ""
						return b
					}(),
				},
			},
		},
	}

	groups := buildGoogleQuotaGroups(resp)
	require.Len(t, groups, 1)
	require.Len(t, groups[0].Windows, 2)
	require.Equal(t, "", groups[0].Windows[0].ResetTime)
	require.Equal(t, "", groups[0].Windows[1].ResetTime)
	// 窗口本身保留
	require.Equal(t, 70, groups[0].Windows[0].Utilization)
	require.Equal(t, 60, groups[0].Windows[1].Utilization)
}

// C8 异常：bucketId 未知 ⇒ 组 / 窗口 kind 均为 other，label 用上游 displayName（截断去换行）
func TestBuildGoogleQuotaGroups_UnknownBucketID(t *testing.T) {
	resp := &antigravity.UserQuotaSummaryResponse{
		Groups: []antigravity.UserQuotaGroup{
			{
				DisplayName: "Mystery Pool\nLine2",
				Buckets: []antigravity.UserQuotaBucket{
					func() antigravity.UserQuotaBucket {
						b := bucket("unknown-bucket", floatPtr(0.7))
						b.DisplayName = "Mystery Pool 5h\nCTRL\u0001chars"
						return b
					}(),
				},
			},
		},
	}

	groups := buildGoogleQuotaGroups(resp)
	require.Len(t, groups, 1)
	require.Equal(t, "other", groups[0].Kind)
	require.Equal(t, "Mystery PoolLine2", groups[0].Label) // 控制字符（换行）去除
	require.Equal(t, "other", groups[0].Windows[0].Kind)
	require.Equal(t, "Mystery Pool 5hCTRLchars", groups[0].Windows[0].Label)
}

// C11 / C12 异常：缺字段 与 显式 null 各自成例（语义 = 该桶丢弃、同组其余保留）
func TestBuildGoogleQuotaGroups_RemainingFractionMissingOrNull(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{
			name: "缺字段",
			json: `{"groups":[{"displayName":"Gemini Models","buckets":[
				{"bucketId":"gemini-5h","displayName":"Gemini 5h","window":"5h","resetTime":"2026-10-05T12:00:00Z"},
				{"bucketId":"gemini-weekly","displayName":"Gemini 7d","window":"7d","resetTime":"2026-10-05T12:00:00Z","remainingFraction":0.5}
			]}]}`,
		},
		{
			name: "显式 null",
			json: `{"groups":[{"displayName":"Gemini Models","buckets":[
				{"bucketId":"gemini-5h","displayName":"Gemini 5h","window":"5h","resetTime":"2026-10-05T12:00:00Z","remainingFraction":null},
				{"bucketId":"gemini-weekly","displayName":"Gemini 7d","window":"7d","resetTime":"2026-10-05T12:00:00Z","remainingFraction":0.5}
			]}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp antigravity.UserQuotaSummaryResponse
			require.NoError(t, json.Unmarshal([]byte(tt.json), &resp))

			groups := buildGoogleQuotaGroups(&resp)
			require.Len(t, groups, 1)
			require.Len(t, groups[0].Windows, 1, "nil 剩余比例桶应被丢弃")
			require.Equal(t, "gemini-weekly", groups[0].Windows[0].BucketID)
			require.Equal(t, 50, groups[0].Windows[0].Utilization)
		})
	}

	// C13 合法 0：与 nil 同组对照 —— 0 保留且 utilization = 100
	respZero := &antigravity.UserQuotaSummaryResponse{
		Groups: []antigravity.UserQuotaGroup{
			{
				DisplayName: "Claude & GPT",
				Buckets: []antigravity.UserQuotaBucket{
					bucket("3p-weekly", floatPtr(0.0)),
				},
			},
		},
	}
	groupsZero := buildGoogleQuotaGroups(respZero)
	require.Len(t, groupsZero, 1)
	require.Len(t, groupsZero[0].Windows, 1)
	require.Equal(t, 100, groupsZero[0].Windows[0].Utilization, "合法 0 ⇒ 已用尽 = 100，不得与无数据同形")
}

// allInvalid 全部无效 ⇒ nil（字段整体缺席）
func TestBuildGoogleQuotaGroups_AllInvalidNil(t *testing.T) {
	resp := &antigravity.UserQuotaSummaryResponse{
		Groups: []antigravity.UserQuotaGroup{
			{
				DisplayName: "Gemini Models",
				Buckets: []antigravity.UserQuotaBucket{
					bucket("gemini-5h", nil),
					bucket("gemini-weekly", nil),
				},
			},
		},
	}
	require.Nil(t, buildGoogleQuotaGroups(resp))
}

// S9 表驱动：≥9 例覆盖 C4-C7 + C11-C13 输入面
func TestBuildGoogleQuotaGroups_TableDriven(t *testing.T) {
	tests := []struct {
		name        string
		groups      []antigravity.UserQuotaGroup
		wantNil     bool
		wantCount   int
		wantUtil    map[string]int  // bucketID → utilization
		wantDropped map[string]bool // bucketID → 应被丢弃
	}{
		{name: "NaN 丢弃", groups: gOne("gemini-5h", math.NaN()), wantNil: true},
		{name: "1.5 丢弃", groups: gOne("gemini-5h", 1.5), wantNil: true},
		{name: "-0.1 丢弃", groups: gOne("gemini-5h", -0.1), wantNil: true},
		{name: "合法 0 保留", groups: gOne("3p-weekly", 0.0), wantNil: false, wantCount: 1, wantUtil: map[string]int{"3p-weekly": 100}},
		{name: "0.5 保留", groups: gOne("gemini-weekly", 0.5), wantNil: false, wantCount: 1, wantUtil: map[string]int{"gemini-weekly": 50}},
		{name: "1.0 保留 用 0", groups: gOne("3p-5h", 1.0), wantNil: false, wantCount: 1, wantUtil: map[string]int{"3p-5h": 0}},
		{name: "not-a-date", groups: gOneRF("gemini-5h", 0.3, "not-a-date"), wantNil: false, wantCount: 1, wantUtil: map[string]int{"gemini-5h": 70}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildGoogleQuotaGroups(&antigravity.UserQuotaSummaryResponse{Groups: tt.groups})
			if tt.wantNil {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			total := 0
			for _, g := range got {
				for _, w := range g.Windows {
					total++
					if u, ok := tt.wantUtil[w.BucketID]; ok {
						require.Equal(t, u, w.Utilization, "bucket %s utilization", w.BucketID)
					}
					require.False(t, tt.wantDropped[w.BucketID], "bucket %s 应被丢弃", w.BucketID)
				}
			}
			require.Equal(t, tt.wantCount, total)
		})
	}
}

// gOne 构造单组单桶
func gOne(id string, rf float64) []antigravity.UserQuotaGroup {
	return []antigravity.UserQuotaGroup{{DisplayName: "G", Buckets: []antigravity.UserQuotaBucket{bucket(id, floatPtr(rf))}}}
}

// gOneRF 构造单组单桶（自定 resetTime）
func gOneRF(id string, rf float64, resetTime string) []antigravity.UserQuotaGroup {
	b := bucket(id, floatPtr(rf))
	b.ResetTime = resetTime
	return []antigravity.UserQuotaGroup{{DisplayName: "G", Buckets: []antigravity.UserQuotaBucket{b}}}
}

// label 超长截断至 80 字符
func TestBuildGoogleQuotaGroups_LongLabelTruncated(t *testing.T) {
	long := make([]rune, 120)
	for i := range long {
		long[i] = '字'
	}
	resp := &antigravity.UserQuotaSummaryResponse{
		Groups: []antigravity.UserQuotaGroup{
			{
				DisplayName: string(long),
				Buckets: []antigravity.UserQuotaBucket{
					bucket("gemini-5h", floatPtr(0.5)),
				},
			},
		},
	}
	groups := buildGoogleQuotaGroups(resp)
	require.Len(t, groups, 1)
	require.Equal(t, 80, len([]rune(groups[0].Label)))
}
