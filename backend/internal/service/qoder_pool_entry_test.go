package service

import (
	"context"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// TestQoderPoolEntryLayer1And2 验证 qoder 平台进入 OpenAI 兼容调度池的层 1 / 层 2 放行：
//   - isConcreteRequestPlatform(qoder) == true（层 1，composite_platform.go:208）
//   - NormalizeOpenAICompatiblePlatform(qoder) == qoder（层 2，openai_gateway_scheduling.go:295）
func TestQoderPoolEntryLayer1And2(t *testing.T) {
	require.True(t, isConcreteRequestPlatform(PlatformQoder), "qoder 应是 concrete 请求平台")

	normalized := NormalizeOpenAICompatiblePlatform(PlatformQoder)
	require.Equal(t, PlatformQoder, normalized, "qoder 归一时必须保留平台值，绝不回落为 openai")
}

// TestQoderPoolEntryLayer3And4Scheduling 验证层 3（资格闸）与层 4（粘性会话闸 + 候选过滤闸）：
//   - isOpenAICompatibleAccountEligibleForRequest 对 qoder × chat_completions 返回 true（含 UI 预填 14 别名映射账号请求 smodel / auto）
//   - defaultOpenAIAccountScheduler.Select 在粘性会话与负载均衡两条路径均能选中 qoder 账号
func TestQoderPoolEntryLayer3And4Scheduling(t *testing.T) {
	ctx := context.Background()
	groupID := int64(9001)
	uiPreset14 := make(map[string]any, len(domain.QoderModelWhitelist))
	for _, alias := range domain.QoderModelWhitelist {
		uiPreset14[alias] = alias
	}
	qoderAcc := Account{
		ID:          7001,
		Platform:    PlatformQoder,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 2,
		GroupIDs:    []int64{groupID},
		Credentials: map[string]any{"model_mapping": uiPreset14},
	}

	require.True(t, isOpenAICompatibleAccountEligibleForRequest(
		ctx, &qoderAcc, PlatformQoder, "sonus-fable5.1", false, OpenAIEndpointCapabilityChatCompletions,
	))
	require.False(t, isOpenAICompatibleAccountEligibleForRequest(
		ctx, &qoderAcc, PlatformQoder, "sonus-fable5.1", false, OpenAIEndpointCapabilityResponses,
	))

	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{
			"openai:qoder-sticky-session": qoderAcc.ID,
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: []Account{qoderAcc}}},
		cache:              cache,
		cfg:                &config.Config{},
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	scheduler := newDefaultOpenAIAccountScheduler(svc, nil)

	// 层 4 粘性会话命中（selectBySessionHash）
	stickyRes, stickyDec, err := scheduler.Select(ctx, OpenAIAccountScheduleRequest{
		GroupID:            &groupID,
		SessionHash:        "qoder-sticky-session",
		RequestedModel:     "smodel",
		RequiredTransport:  OpenAIUpstreamTransportAny,
		RequiredCapability: OpenAIEndpointCapabilityChatCompletions,
		Platform:           PlatformQoder,
	})
	require.NoError(t, err)
	require.NotNil(t, stickyRes)
	require.Equal(t, qoderAcc.ID, stickyRes.Account.ID)
	require.True(t, stickyDec.StickySessionHit)

	// 层 4 负载均衡候选过滤命中（selectByLoadBalance）
	lbRes, lbDec, err := scheduler.Select(ctx, OpenAIAccountScheduleRequest{
		GroupID:            &groupID,
		SessionHash:        "qoder-new-session",
		RequestedModel:     "auto",
		RequiredTransport:  OpenAIUpstreamTransportAny,
		RequiredCapability: OpenAIEndpointCapabilityChatCompletions,
		Platform:           PlatformQoder,
	})
	require.NoError(t, err)
	require.NotNil(t, lbRes)
	require.Equal(t, qoderAcc.ID, lbRes.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, lbDec.Layer)
}

// TestQoderPoolEntrySupportsOpenAIEndpointCapability 验证层 5 能力谓词与 IsOpenAICompatible 保持 false：
//   - qoder × chat_completions → true
//   - qoder × responses / embeddings / live / seedance → false
//   - IsOpenAICompatible() → false（防止通用 401/400 冷却误伤 Qoder 账号）
func TestQoderPoolEntrySupportsOpenAIEndpointCapability(t *testing.T) {
	qoder := &Account{Platform: PlatformQoder, Type: AccountTypeAPIKey}

	require.False(t, qoder.IsOpenAICompatible(), "qoder 账号 IsOpenAICompatible() 必须保持 false")
	require.True(t, qoder.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions))
	require.True(t, qoder.SupportsOpenAIEndpointCapability(""))
	require.False(t, qoder.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses))
	require.False(t, qoder.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityEmbeddings))
	require.False(t, qoder.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityLive))
	require.False(t, qoder.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance))
}

// TestQoderPoolEntryIsModelSupportedAll32ModelsAndUI14Whitelist 覆盖设计 §6 用例 15 ①：
// 验证未配 model_mapping 与 UI 仅预填 14 个可读别名（qoderModels）两种场景下，
// account.IsModelSupported 对全部 14 个可读别名（含大小写混合如 SONUS-FABLE5.1）
// 与 18 个原生模型 ID（含 smodel、auto、fast、performance、efficient）共 32 个合法模型均返回 true，
// 对未知模型 "unknown" 返回 false。
func TestQoderPoolEntryIsModelSupportedAll32ModelsAndUI14Whitelist(t *testing.T) {
	uiPreset14 := make(map[string]any, len(domain.QoderModelWhitelist))
	for _, alias := range domain.QoderModelWhitelist {
		uiPreset14[alias] = alias
	}

	accounts := []*Account{
		{ID: 1, Platform: PlatformQoder},
		{ID: 2, Platform: PlatformQoder, Credentials: map[string]any{}},
		{ID: 3, Platform: PlatformQoder, Credentials: map[string]any{"model_mapping": uiPreset14}},
	}

	for _, acc := range accounts {
		for _, alias := range domain.QoderModelWhitelist {
			require.True(t, acc.IsModelSupported(alias), "acc=%d alias=%s", acc.ID, alias)
			require.True(t, acc.IsModelSupported(strings.ToUpper(alias)), "acc=%d upper=%s", acc.ID, strings.ToUpper(alias))
		}
		for _, nativeID := range domain.QoderNativeModels {
			require.True(t, acc.IsModelSupported(nativeID), "acc=%d native=%s", acc.ID, nativeID)
			require.True(t, acc.IsModelSupported(strings.ToUpper(nativeID)), "acc=%d upperNative=%s", acc.ID, strings.ToUpper(nativeID))
		}
		require.True(t, acc.IsModelSupported("SONUS-FABLE5.1"))
		require.True(t, acc.IsModelSupported("smodel"))
		require.True(t, acc.IsModelSupported("auto"))
		require.False(t, acc.IsModelSupported("unknown"))
		require.False(t, acc.IsModelSupported("gpt-99-unknown"))
	}
}
