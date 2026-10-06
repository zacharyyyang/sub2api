//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// TestAttachGoogleQuotaGroups_Degraded 降级四例（设计 §5「降级用例细则」）：
// ① httptest 返 500；② 已关闭服务（连接拒绝）；③ 200 {}（空响应）；④ 已取消 ctx（超时注入，零 sleep）。
// 每例独立 t.Run，互不代偿。旁路失败 ⇒ *UsageInfo 整包不变（含 AntigravityQuota /
// SubscriptionTier / AICredits / UpdatedAt）、GoogleQuotaGroups 缺席、Error/ErrorCode 仍空。
func TestAttachGoogleQuotaGroups_Degraded(t *testing.T) {
	// 构造一个已装配好的 UsageInfo（逐模型额度 / 订阅档 / AI Credits / UpdatedAt 齐全）
	buildBaseline := func() (*UsageInfo, UsageInfo) {
		now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
		info := &UsageInfo{
			UpdatedAt:        &now,
			SubscriptionTier: "PRO",
			AntigravityQuota: map[string]*AntigravityModelQuota{
				"model-a": {Utilization: 40, ResetTime: "2026-10-05T00:00:00Z"},
			},
			AICredits: []AICredit{{CreditType: "GOOGLE_ONE_AI", Amount: 25, MinimumBalance: 5}},
		}
		return info, *info
	}

	// 公共断言：旁路前后整包快照比较 + GoogleQuotaGroups 缺席 + Error/ErrorCode 空（细则第 4 条）
	assertUntouched := func(t *testing.T, preSnapshot UsageInfo, info *UsageInfo) {
		t.Helper()
		require.Equal(t, preSnapshot, *info, "旁路失败时 *UsageInfo 必须整包不变")
		require.Nil(t, info.GoogleQuotaGroups, "google_quota_groups 必须缺席")
		require.Empty(t, info.Error, "不得写 Error")
		require.Empty(t, info.ErrorCode, "不得写 ErrorCode")
	}

	// 指向给定 URL 的直调环境：BaseURLs 置双元素（两探针同指 targetURL；对位 attach 固定序访问 fetcher.go:128/:132）；手法沿用 antigravity_quota_fetcher_test.go:398-426
	setupClient := func(t *testing.T, targetURL string) (*AntigravityQuotaFetcher, *antigravity.Client) {
		t.Helper()
		oldBaseURLs := append([]string(nil), antigravity.BaseURLs...)
		oldAvailability := antigravity.DefaultURLAvailability
		t.Cleanup(func() {
			antigravity.BaseURLs = oldBaseURLs
			antigravity.DefaultURLAvailability = oldAvailability
		})
		antigravity.BaseURLs = []string{targetURL, targetURL}
		antigravity.DefaultURLAvailability = antigravity.NewURLAvailability(time.Minute)

		client, err := antigravity.NewClient("")
		require.NoError(t, err)
		cfg := &config.Config{}
		return NewAntigravityQuotaFetcher(nil, cfg), client
	}

	callAttach := func(fetcher *AntigravityQuotaFetcher, ctx context.Context, client *antigravity.Client, info *UsageInfo) {
		fetcher.attachGoogleQuotaGroups(ctx, client, "token", resolveModelsListReadLimit(fetcher.cfg), info)
	}

	t.Run("① 上游返回 500", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		fetcher, client := setupClient(t, server.URL)
		info, preSnapshot := buildBaseline()
		callAttach(fetcher, context.Background(), client, info)
		assertUntouched(t, preSnapshot, info)
	})

	t.Run("② 已关闭服务（连接拒绝）", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
		deadURL := server.URL
		server.Close() // 连接拒绝：*net.OpError 支路

		fetcher, client := setupClient(t, deadURL)
		info, preSnapshot := buildBaseline()
		callAttach(fetcher, context.Background(), client, info)
		assertUntouched(t, preSnapshot, info)
	})

	t.Run("③ 空响应 200 {}", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}))
		defer server.Close()

		fetcher, client := setupClient(t, server.URL)
		info, preSnapshot := buildBaseline()
		callAttach(fetcher, context.Background(), client, info)
		assertUntouched(t, preSnapshot, info)
	})

	t.Run("④ 已取消 ctx（超时注入，零 sleep）", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}))
		defer server.Close()

		cctx, cancel := context.WithCancel(context.Background())
		cancel() // 已取消的 ctx 使 Client.Do 即刻失败，不等 clientTimeout（细则第 2 条）

		fetcher, client := setupClient(t, server.URL)
		info, preSnapshot := buildBaseline()
		callAttach(fetcher, cctx, client, info)
		assertUntouched(t, preSnapshot, info)
	})
}
