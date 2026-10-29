package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// statsQueryPattern 把「确实按 api_key_id 分组」写进匹配条件：
// 只匹配得到 GROUP BY GROUPING SETS 的话，查询漏掉 (api_key_id) 时 mock 仍会
// 返回密钥行，测试就失去了发现该缺陷的能力。
const statsQueryPattern = `(?s)FROM usage_logs.*GROUP BY GROUPING SETS.*\(api_key_id\)`

// 按 API 密钥聚合：分组集 (api_key_id) 产出的行进入 stats.APIKeys，
// 名称用一次小查询回填（主聚合查询不 join api_keys，避免列名歧义）。
func TestUsageLogRepositoryGetStatsWithFiltersAggregatesAPIKeys(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	mock.ExpectQuery(statsQueryPattern).
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

	// 分组合计与总计一致（聚合正确性的自检）
	var perKeyRequests int64
	for _, k := range stats.APIKeys {
		perKeyRequests += k.Requests
	}
	require.Equal(t, stats.TotalRequests, perKeyRequests)
}

// 密钥已删除：名称查不到时留空（不写 "unknown"），id 保留以便展示层兜底。
func TestUsageLogRepositoryGetStatsWithFiltersDeletedAPIKeyLeavesNameEmpty(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	mock.ExpectQuery(statsQueryPattern).
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
	require.Empty(t, stats.APIKeys[0].Name)
}

// 名称查询本身失败：必须向上报错，不能被当成「密钥已删除」静默降级。
func TestUsageLogRepositoryGetStatsWithFiltersAPIKeyNameQueryFailureSurfaces(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	mock.ExpectQuery(statsQueryPattern).
		WillReturnRows(sqlmock.NewRows([]string{
			"inbound_grouped", "upstream_grouped", "key_grouped",
			"inbound_endpoint", "upstream_endpoint", "api_key_id",
			"requests", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens",
			"cost", "actual_cost", "account_cost", "avg_duration_ms",
		}).
			AddRow(1, 1, 0, nil, nil, int64(7), int64(1), int64(10), int64(5), int64(0), int64(0), 0.1, 0.1, 0.1, 5.0))

	mock.ExpectQuery("SELECT id, name FROM api_keys").
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(errors.New("api_keys lookup failed"))

	_, err := repo.GetStatsWithFilters(context.Background(), usagestats.UsageLogFilters{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "api_keys lookup failed")
	require.NoError(t, mock.ExpectationsWereMet())
}

// 名称扫描失败（列数不符等）：同样必须向上报错，不得落成缺失占位。
func TestUsageLogRepositoryGetStatsWithFiltersAPIKeyNameScanFailureSurfaces(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}

	mock.ExpectQuery(statsQueryPattern).
		WillReturnRows(sqlmock.NewRows([]string{
			"inbound_grouped", "upstream_grouped", "key_grouped",
			"inbound_endpoint", "upstream_endpoint", "api_key_id",
			"requests", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens",
			"cost", "actual_cost", "account_cost", "avg_duration_ms",
		}).
			AddRow(1, 1, 0, nil, nil, int64(7), int64(1), int64(10), int64(5), int64(0), int64(0), 0.1, 0.1, 0.1, 5.0))

	// 只有一列，扫描 id/name 时会失败
	mock.ExpectQuery("SELECT id, name FROM api_keys").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))

	_, err := repo.GetStatsWithFilters(context.Background(), usagestats.UsageLogFilters{})
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
