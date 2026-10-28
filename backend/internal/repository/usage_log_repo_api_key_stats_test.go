package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// 按 API 密钥聚合：分组集 (api_key_id) 产出的行进入 stats.APIKeys，
// 名称用一次小查询回填（主聚合查询不 join api_keys，避免列名歧义）。
func TestUsageLogRepositoryGetStatsWithFiltersAggregatesAPIKeys(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	mock.ExpectQuery("(?s)FROM usage_logs.*GROUP BY GROUPING SETS").
		WillReturnRows(sqlmock.NewRows([]string{
			"inbound_grouped", "upstream_grouped", "key_grouped",
			"inbound_endpoint", "upstream_endpoint", "api_key_id",
			"requests", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens",
			"cost", "actual_cost", "account_cost", "avg_duration_ms",
		}).
			// 总计行（三个维度都卷起）
			AddRow(1, 1, 1, nil, nil, nil, int64(10), int64(1000), int64(500), int64(0), int64(2000), 3.0, 2.0, 2.0, 50.0).
			// 仅按密钥聚合的行
			AddRow(1, 1, 0, nil, nil, int64(7), int64(6), int64(600), int64(300), int64(0), int64(1200), 2.0, 1.2, 1.2, 40.0).
			AddRow(1, 1, 0, nil, nil, int64(9), int64(4), int64(400), int64(200), int64(0), int64(800), 1.0, 0.8, 0.8, 30.0))

	mock.ExpectQuery("SELECT id, name FROM api_keys").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).
			AddRow(int64(7), "wb-beta-雪清").
			AddRow(int64(9), "wb-alpha-思雨"))

	stats, err := repo.GetStatsWithFilters(context.Background(), usagestats.UsageLogFilters{})
	require.NoError(t, err)
	require.Len(t, stats.APIKeys, 2, "key-grouped rows must land in stats.APIKeys")
	require.NoError(t, mock.ExpectationsWereMet())

	// 总计不受影响
	require.Equal(t, int64(10), stats.TotalRequests)
	require.InDelta(t, 3.0, stats.TotalCost, 1e-9)

	require.Len(t, stats.APIKeys, 2)
	// 请求数降序：id=7(6) 在 id=9(4) 之前
	require.Equal(t, int64(7), stats.APIKeys[0].ID)
	require.Equal(t, "wb-beta-雪清", stats.APIKeys[0].Name)
	require.Equal(t, int64(6), stats.APIKeys[0].Requests)
	require.Equal(t, int64(600+300+1200), stats.APIKeys[0].TotalTokens)
	require.InDelta(t, 2.0, stats.APIKeys[0].Cost, 1e-9)
	require.InDelta(t, 1.2, stats.APIKeys[0].ActualCost, 1e-9)

	require.Equal(t, int64(9), stats.APIKeys[1].ID)
	require.Equal(t, "wb-alpha-思雨", stats.APIKeys[1].Name)
}

// 已删除的密钥：名称回填不到时回退 unknown，但 id 保留（以便排查来源）。
func TestUsageLogRepositoryGetStatsWithFiltersDeletedAPIKeyFallsBackToUnknown(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	mock.ExpectQuery("(?s)FROM usage_logs.*GROUP BY GROUPING SETS").
		WillReturnRows(sqlmock.NewRows([]string{
			"inbound_grouped", "upstream_grouped", "key_grouped",
			"inbound_endpoint", "upstream_endpoint", "api_key_id",
			"requests", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens",
			"cost", "actual_cost", "account_cost", "avg_duration_ms",
		}).
			AddRow(1, 1, 0, nil, nil, int64(42), int64(3), int64(300), int64(100), int64(0), int64(0), 0.6, 0.6, 0.6, 10.0))

	mock.ExpectQuery("SELECT id, name FROM api_keys").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))

	stats, err := repo.GetStatsWithFilters(context.Background(), usagestats.UsageLogFilters{})
	require.NoError(t, err)
	require.Len(t, stats.APIKeys, 1)
	require.Equal(t, int64(42), stats.APIKeys[0].ID)
	require.Equal(t, "unknown", stats.APIKeys[0].Name)
}
