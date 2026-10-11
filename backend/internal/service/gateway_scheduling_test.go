//go:build unit

package service

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件覆盖 #9：全部门控后仍无可用账号（空集）时的诊断日志。
// 注意：这些用例捕获全局 slog（slog.SetDefault），因此不 t.Parallel()——
// 若并行执行，全局 logger 的捕获会让其他用例的日志也落入缓冲区，破坏计数断言。
// 顺序执行保证本用例时序段内只有本用例产生日志。

// 普通空集（排除列表覆盖全部账号，无渠道限制）→ 触发新增 Warn 一次。
func TestSelectAccountWithLoadAwareness_EmptySetLogsWarn(t *testing.T) {
	f := newLoadAwareRestrictionFixture(t, false, nil, nil)
	excluded := map[int64]struct{}{1: {}, 2: {}}

	var buf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(origLogger)

	result, err := f.svc.SelectAccountWithLoadAwareness(f.ctx, &f.groupID, "", "claude-fable-5-1", excluded, "", 0)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Nil(t, result)

	got := buf.String()
	require.Equal(t, 1, strings.Count(got, "no schedulable account after all gates"), "普通空集结产生的诊断日志应恰好一次")
	require.Contains(t, got, "group_id=10")
	require.Contains(t, got, "model=claude-fable-5-1")
	require.Contains(t, got, "platform=anthropic")
	require.Contains(t, got, "total_accounts=2")
	require.Contains(t, got, "total=2")
	require.Contains(t, got, "excluded=2")
}

// 渠道限制导致的空集（:703 分支先返回）→ 不触发新增 Warn，既有渠道限制 Warn 保留一次。
func TestSelectAccountWithLoadAwareness_ChannelRestrictedEmptyKeepsOldWarn(t *testing.T) {
	f := newLoadAwareRestrictionFixture(t, true, nil, nil)
	// 账号 2 也改为映射到不在定价列表的模型，使全部账号渠道受限 → 候选为空
	f.svc.accountRepo.(*mockAccountRepoForPlatform).accounts[1].Credentials = map[string]any{
		"model_mapping": map[string]any{"claude-fable-5-1": "claude-fable-5-1"},
	}

	var buf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(origLogger)

	result, err := f.svc.SelectAccountWithLoadAwareness(f.ctx, &f.groupID, "", "claude-fable-5-1", nil, "", 0)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.ErrorContains(t, err, "channel pricing restriction")
	require.Nil(t, result)

	got := buf.String()
	require.Equal(t, 0, strings.Count(got, "no schedulable account after all gates"), "渠道限制分支不应触发新增诊断日志")
	require.Equal(t, 1, strings.Count(got, "channel pricing restriction blocked request"), "既有渠道限制 Warn 应恰好一次")
}