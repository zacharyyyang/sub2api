package service

import (
	"context"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// TestMimoPoolEntryAndCompositeRouting 覆盖设计 §6 用例 9：
// ① 六层调度放行（isConcreteRequestPlatform、NormalizeOpenAICompatiblePlatform、isOpenAICompatibleAccountEligibleForRequest、粘性会话与负载均衡选号、SupportsOpenAIEndpointCapability）；
// ② DetectModelPlatform("mimo-v2.6-pro") 返 PlatformMimo, true 而 "mimo-v2.5" 返 "", false（绝不抢占 opencode-go）；
// ③ IsModelSupported 对 6 个白名单模型及思考后缀变体返回 true，未知模型返回 false。
func TestMimoPoolEntryAndCompositeRouting(t *testing.T) {
	ctx := context.Background()
	groupID := int64(9101)

	// ① 层 1 & 层 2：concrete 平台判定与 OpenAI 兼容归一化保真
	require.True(t, isConcreteRequestPlatform(PlatformMimo), "mimo 应是 concrete 请求平台")
	require.Equal(t, PlatformMimo, NormalizeOpenAICompatiblePlatform(PlatformMimo), "mimo 归一时必须保留平台值")

	// ② Composite 自动识别：6 个白名单模型与 mimo/ 前缀命中 PlatformMimo，mimo-v2.5 绝不抢占
	for _, m := range []string{"mimo-v2.6-pro", "mimo-v2.6-pro-thinking", "mimo-v2-omni", "mimo/mimo-v2.6-flash"} {
		plat, ok := DetectModelPlatform(m)
		require.True(t, ok, "model=%s", m)
		require.Equal(t, PlatformMimo, plat, "model=%s", m)
	}
	for _, nonMimo := range []string{"mimo-v2.5", "mimo-v2.5-pro", "unknown-model"} {
		plat, ok := DetectModelPlatform(nonMimo)
		require.False(t, ok, "model=%s must not be hijacked by PlatformMimo", nonMimo)
		require.Empty(t, plat, "model=%s", nonMimo)
	}

	// ③ 层 3 / 层 4 / 层 5：资格闸、粘性会话、负载均衡与端点能力闸门
	mimoAcc := Account{ID: 8001, Platform: PlatformMimo, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 2, GroupIDs: []int64{groupID}}
	require.False(t, mimoAcc.IsOpenAICompatible(), "mimo IsOpenAICompatible() 必须保持 false")
	require.True(t, mimoAcc.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions))
	require.True(t, mimoAcc.SupportsOpenAIEndpointCapability(""))
	require.False(t, mimoAcc.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses))
	require.False(t, mimoAcc.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityEmbeddings))
	require.True(t, isOpenAICompatibleAccountEligibleForRequest(ctx, &mimoAcc, PlatformMimo, "mimo-v2.6-pro-thinking", false, OpenAIEndpointCapabilityChatCompletions))
	require.False(t, isOpenAICompatibleAccountEligibleForRequest(ctx, &mimoAcc, PlatformMimo, "mimo-v2.6-pro", false, OpenAIEndpointCapabilityResponses))

	svc := &OpenAIGatewayService{
		accountRepo:        schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: []Account{mimoAcc}}},
		cache:              &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:mimo-sticky": mimoAcc.ID}},
		cfg:                &config.Config{},
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	scheduler := newDefaultOpenAIAccountScheduler(svc, nil)

	stickyRes, stickyDec, err := scheduler.Select(ctx, OpenAIAccountScheduleRequest{GroupID: &groupID, SessionHash: "mimo-sticky", RequestedModel: "mimo-v2.6-pro-thinking", RequiredTransport: OpenAIUpstreamTransportAny, RequiredCapability: OpenAIEndpointCapabilityChatCompletions, Platform: PlatformMimo})
	require.NoError(t, err)
	require.Equal(t, mimoAcc.ID, stickyRes.Account.ID)
	require.True(t, stickyDec.StickySessionHit)

	lbRes, lbDec, err := scheduler.Select(ctx, OpenAIAccountScheduleRequest{GroupID: &groupID, SessionHash: "mimo-fresh", RequestedModel: "mimo-v2-omni", RequiredTransport: OpenAIUpstreamTransportAny, RequiredCapability: OpenAIEndpointCapabilityChatCompletions, Platform: PlatformMimo})
	require.NoError(t, err)
	require.Equal(t, mimoAcc.ID, lbRes.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, lbDec.Layer)

	// ④ IsModelSupported：6 个官方模型及思考后缀、大小写变体均放行，mimo-v2.5 拒绝
	for _, id := range domain.MimoModelWhitelist {
		require.True(t, mimoAcc.IsModelSupported(id), "id=%s", id)
		require.True(t, mimoAcc.IsModelSupported(strings.ToUpper(id)), "upper=%s", id)
		require.True(t, mimoAcc.IsModelSupported(id+"-thinking"), "thinking=%s", id)
		require.True(t, mimoAcc.IsModelSupported(id+":high"), "colon=%s", id)
	}
	require.False(t, mimoAcc.IsModelSupported("mimo-v2.5"))
	require.False(t, mimoAcc.IsModelSupported("gpt-99-unknown"))

	mappedAcc := Account{ID: 8002, Platform: PlatformMimo, Credentials: map[string]any{"model_mapping": map[string]any{"claude-sonnet-4-6": "mimo-v2.6-pro"}}}
	require.True(t, mappedAcc.IsModelSupported("claude-sonnet-4-6"))
	require.Equal(t, "mimo-v2.6-pro", mappedAcc.GetMappedModel("claude-sonnet-4-6"))
}
