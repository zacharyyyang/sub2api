package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWbPoolEntryLayer1L2 验证 wb 平台进入 OpenAI 兼容调度池的层 1/层 2 放行：
//   - isConcreteRequestPlatform(wb)==true：wb 是 concrete 请求平台，允许走 OpenAI
//     兼容链路（层 1，composite_platform.go）
//   - NormalizeOpenAICompatiblePlatform(wb)==wb：归一保留 wb，不回落 PlatformOpenAI
//     （层 2，openai_gateway_scheduling.go）
func TestWbPoolEntryLayer1L2(t *testing.T) {
	require.True(t, isConcreteRequestPlatform(PlatformWB), "wb 应是 concrete 请求平台")

	normalized := NormalizeOpenAICompatiblePlatform(PlatformWB)
	require.Equal(t, PlatformWB, normalized, "wb 归一时必须保留平台值（池查询命中依赖归一保留）")
}

// TestWbPoolEntrySupportsOpenAIEndpointCapability 验证层 5 能力闸对 wb 的定点放行
// （account.go SupportsOpenAIEndpointCapability）：
//   - wb × chat_completions → true（wb 网关腿 = OpenAI 兼容 chat 接口）
//   - wb × 其他非空能力 → false（seedance 分支在前，wb×seedance 必 false，顺序无需调整）
//   - wb × 空能力 → true（空 value 的无能力约束入口提前放行，既有行为延续）
func TestWbPoolEntrySupportsOpenAIEndpointCapability(t *testing.T) {
	wb := &Account{Platform: PlatformWB}

	require.True(t, wb.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions),
		"wb×chat_completions 必须放行")
	require.False(t, wb.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance),
		"wb×seedance（非 CC 非空能力）必须拒绝")
	require.True(t, wb.SupportsOpenAIEndpointCapability(""),
		"wb×空能力必须放行（无能力约束入口的既有行为延续）")
}

// TestWbPoolEntryIsOpenAICompatibleFalse 验证 IsOpenAICompatible()(wb)==false 保持不变：
// wb 401 错误语义独立（KDR-8），不能被通用 openai 兼容谓词误判；调度池内的 wb
// 放行靠层 3/层 4/层 5 的定点分支完成，谓词本体一律不动。
func TestWbPoolEntryIsOpenAICompatibleFalse(t *testing.T) {
	wb := &Account{Platform: PlatformWB}

	require.False(t, wb.IsOpenAICompatible(), "wb 账号永不通过 IsOpenAICompatible()")
}
