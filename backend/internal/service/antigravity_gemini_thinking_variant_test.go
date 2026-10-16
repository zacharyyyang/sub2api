package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func newAntigravityAccountWithMapping(mapping map[string]string) *Account {
	mappingAny := make(map[string]any, len(mapping))
	for k, v := range mapping {
		mappingAny[k] = v
	}
	return &Account{
		ID:       1,
		Name:     "ag",
		Platform: PlatformAntigravity,
		Credentials: map[string]any{
			"model_mapping": mappingAny,
		},
	}
}

func TestGeminiThinkingLevelFromBody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"empty body", ``, "high"},
		{"no thinkingConfig", `{"contents":[]}`, "high"},
		{"budget -1 dynamic", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":-1}}}`, "high"},
		{"budget 0 off", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`, "low"},
		{"budget 1000 (agy low)", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":1000}}}`, "low"},
		{"budget 4000 (agy medium)", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":4000}}}`, "medium"},
		{"budget 24576", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":24576}}}`, "high"},
		{"thinkingLevel wins over budget", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":24576,"thinkingLevel":"low"}}}`, "low"},
		{"thinkingLevel medium", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"MEDIUM"}}}`, "medium"},
		{"thinkingLevel none", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"none"}}}`, "low"},
		{"thinkingLevel minimal", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"minimal"}}}`, "low"},
		{"invalid json", `{"generationConfig":`, "high"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, geminiThinkingLevelFromBody([]byte(tt.body)))
		})
	}
}

func TestResolveGeminiThinkingVariant(t *testing.T) {
	// 与生产 Antigravity 账号从上游同步来的目录一致：只有带后缀的变体，没有裸名。
	catalog := map[string]string{
		"gemini-3.8-flash-low":    "gemini-3.8-flash-low",
		"gemini-3.8-flash-medium": "gemini-3.8-flash-medium",
		"gemini-3.8-flash-high":   "gemini-3.8-flash-high",
		"gemini-3.8-flash-tiered": "gemini-3.8-flash-tiered",
		"gemini-3.6-flash-high":   "gemini-3.6-flash-high",
		"gemini-3.1-pro-high":     "gemini-3.1-pro-high",
		"gemini-2.5-flash":        "gemini-2.5-flash",
	}
	budget := func(v string) []byte {
		return []byte(`{"contents":[],"generationConfig":{"thinkingConfig":{"thinkingBudget":` + v + `}}}`)
	}

	tests := []struct {
		name      string
		mapping   map[string]string
		model     string
		body      []byte
		want      string
		wantMatch bool
	}{
		{"agy high (-1) -> -high", catalog, "gemini-3.8-flash", budget("-1"), "gemini-3.8-flash-high", true},
		{"agy medium (4000) -> -medium", catalog, "gemini-3.8-flash", budget("4000"), "gemini-3.8-flash-medium", true},
		{"agy low (1000) -> -low", catalog, "gemini-3.8-flash", budget("1000"), "gemini-3.8-flash-low", true},
		{"models/ prefix stripped", catalog, "models/gemini-3.8-flash", budget("1000"), "gemini-3.8-flash-low", true},
		{"no thinkingConfig -> -high", catalog, "gemini-3.8-flash", []byte(`{"contents":[]}`), "gemini-3.8-flash-high", true},
		{"only -high exists: low request degrades to high", map[string]string{
			"gemini-3.5-flash-high": "gemini-3.5-flash-high",
		}, "gemini-3.5-flash", budget("1000"), "gemini-3.5-flash-high", true},
		// gemini-3.6/3.7/3.8-flash 的四个变体会被 resolveModelMapping 自动补齐（存量显式映射账号的兼容透传）。
		{"injected default variants are usable", catalog, "gemini-3.6-flash", budget("1000"), "gemini-3.6-flash-low", true},
		{"already suffixed: untouched", catalog, "gemini-3.8-flash-low", budget("-1"), "", false},
		{"bare name explicitly mapped: untouched", catalog, "gemini-2.5-flash", budget("-1"), "", false},
		{"no variants in mapping: untouched", catalog, "gemini-9.9-flash", budget("-1"), "", false},
		{"non-gemini model: untouched", catalog, "claude-sonnet-4-6", budget("-1"), "", false},
		{"explicit bare alias wins over variants", map[string]string{
			"gemini-3.8-flash":      "gemini-3.8-flash-tiered",
			"gemini-3.8-flash-high": "gemini-3.8-flash-high",
		}, "gemini-3.8-flash", budget("-1"), "", false},
		{"variant value is itself a mapping target", map[string]string{
			"gemini-3.8-flash-high": "gemini-3.8-flash-tiered",
		}, "gemini-3.8-flash", budget("-1"), "gemini-3.8-flash-tiered", true},
		// 前端持久化的默认目录含裸名自映射；-low 由默认透传补齐，按预算命中。
		{"persisted default bare identity passthrough still resolves when variants exist", map[string]string{
			"gemini-3.8-flash":      "gemini-3.8-flash",
			"gemini-3.8-flash-low":  "gemini-3.8-flash-low",
			"gemini-3.8-flash-high": "gemini-3.8-flash-high",
		}, "gemini-3.8-flash", budget("1000"), "gemini-3.8-flash-low", true},
		{"bare-only mapping without suffixed keys: resolves built-in thinking variant", map[string]string{
			"gemini-4-argon": "gemini-4-argon-high",
			"gemini-3.1-pro": "gemini-pro-agent",
		}, "gemini-4-argon", budget("1000"), "gemini-4-argon-low", true},
		{"bare-only gemini-3.1-pro with low budget -> gemini-3.1-pro-low", map[string]string{
			"gemini-3.1-pro": "gemini-pro-agent",
		}, "gemini-3.1-pro", budget("1000"), "gemini-3.1-pro-low", true},
		{"bare-only gemini-3.1-pro with high budget -> gemini-pro-agent", map[string]string{
			"gemini-3.1-pro": "gemini-pro-agent",
		}, "gemini-3.1-pro", budget("24576"), "gemini-pro-agent", true},
		{"bare-only claude-opus-5-5 with low budget -> claude-opus-5-5-low", map[string]string{
			"claude-opus-5-5": "claude-opus-5-5-high",
		}, "claude-opus-5-5", budget("1000"), "claude-opus-5-5-low", true},
		{"bare-only claude-opus-5-5 mapped to another model (claude-opus-4-6-thinking): untouched", map[string]string{
			"claude-opus-5-5": "claude-opus-4-6-thinking",
		}, "claude-opus-5-5", budget("1000"), "", false},
		{"bare identity passthrough resolves a configured variant outside the default passthroughs", map[string]string{
			"gemini-3.5-flash":      "gemini-3.5-flash",
			"gemini-3.5-flash-high": "gemini-3.5-flash-high",
		}, "gemini-3.5-flash", budget("1000"), "gemini-3.5-flash-high", true},
		{"wildcard mapping does not fabricate variants for non-tiered bare model", map[string]string{
			"gemini-2.5*": "gemini-2.5-flash",
		}, "gemini-2.5-flash", budget("-1"), "", false},
		{"bare model absent from whitelist: configured variant still resolved", map[string]string{
			"gemini-3.7-flash-medium": "gemini-3.7-flash-medium",
		}, "gemini-3.7-flash", budget("4000"), "gemini-3.7-flash-medium", true},
		// 空 credentials 映射保留默认目录兼容行为。
		{"empty mapping falls back to default catalog", map[string]string{}, "gemini-3.8-flash", budget("-1"), "gemini-3.8-flash-high", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := newAntigravityAccountWithMapping(tt.mapping)
			got, matched := resolveGeminiThinkingVariant(account, tt.model, tt.body)
			require.Equal(t, tt.wantMatch, matched)
			require.Equal(t, tt.want, got)
		})
	}

	t.Run("nil account", func(t *testing.T) {
		got, matched := resolveGeminiThinkingVariant(nil, "gemini-3.8-flash", budget("-1"))
		require.False(t, matched)
		require.Empty(t, got)
	})
}

func TestGeminiThinkingLevelFromClaudeThinking(t *testing.T) {
	tests := []struct {
		name     string
		thinking *antigravity.ThinkingConfig
		want     string
	}{
		{"nil thinking", nil, "high"},
		{"disabled", &antigravity.ThinkingConfig{Type: "disabled"}, "low"},
		{"enabled without budget", &antigravity.ThinkingConfig{Type: "enabled"}, "high"},
		{"budget 1024 -> low", &antigravity.ThinkingConfig{Type: "enabled", BudgetTokens: 1024}, "low"},
		{"budget 4000 -> medium", &antigravity.ThinkingConfig{Type: "enabled", BudgetTokens: 4000}, "medium"},
		{"budget 20000 -> high", &antigravity.ThinkingConfig{Type: "enabled", BudgetTokens: 20000}, "high"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, geminiThinkingLevelFromClaudeThinking(tt.thinking))
		})
	}
}

func TestGeminiThinkingLevelFromClaudeRequest_OutputConfigEffort(t *testing.T) {
	tests := []struct {
		name         string
		thinking     *antigravity.ThinkingConfig
		outputConfig *antigravity.OutputConfig
		want         string
	}{
		{"output_config low overrides nil thinking", nil, &antigravity.OutputConfig{Effort: "low"}, "low"},
		{"output_config medium overrides adaptive thinking", &antigravity.ThinkingConfig{Type: "adaptive"}, &antigravity.OutputConfig{Effort: "medium"}, "medium"},
		{"output_config high", &antigravity.ThinkingConfig{Type: "adaptive"}, &antigravity.OutputConfig{Effort: "high"}, "high"},
		{"output_config max -> high", nil, &antigravity.OutputConfig{Effort: "max"}, "high"},
		{"thinking disabled wins over output_config", &antigravity.ThinkingConfig{Type: "disabled"}, &antigravity.OutputConfig{Effort: "high"}, "low"},
		{"empty output_config falls back to budget", &antigravity.ThinkingConfig{Type: "enabled", BudgetTokens: 1024}, &antigravity.OutputConfig{}, "low"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, geminiThinkingLevelFromClaudeRequest(tt.thinking, tt.outputConfig))
		})
	}
}

// 锁定线上 404 回归：Chat Completions / Messages 兼容层收到裸 Gemini 模型名时，
// 必须和 Gemini 原生路径一样解析到带后缀的上游变体，而不是把裸名透传给上游。
func TestResolveGeminiThinkingVariantForLevel_CompatPathResolvesBareModel(t *testing.T) {
	catalog := map[string]string{
		"gemini-3.8-flash-low":    "gemini-3.8-flash-low",
		"gemini-3.8-flash-medium": "gemini-3.8-flash-medium",
		"gemini-3.8-flash-high":   "gemini-3.8-flash-high",
	}
	account := newAntigravityAccountWithMapping(catalog)

	for _, tc := range []struct{ level, want string }{
		{"low", "gemini-3.8-flash-low"},
		{"medium", "gemini-3.8-flash-medium"},
		{"high", "gemini-3.8-flash-high"},
		{"", "gemini-3.8-flash-high"},
	} {
		t.Run("level="+tc.level, func(t *testing.T) {
			got, matched := resolveGeminiThinkingVariantForLevel(account, "gemini-3.8-flash", tc.level)
			require.True(t, matched)
			require.Equal(t, tc.want, got)
		})
	}

	t.Run("already suffixed model is untouched", func(t *testing.T) {
		got, matched := resolveGeminiThinkingVariantForLevel(account, "gemini-3.8-flash-high", "low")
		require.False(t, matched)
		require.Empty(t, got)
	})
}

// 同一请求无论走 Gemini 原生还是兼容层，都必须落到同一个上游变体。
func TestGeminiThinkingLevel_NativeAndCompatAgree(t *testing.T) {
	native := geminiThinkingLevelFromBody([]byte(`{"generationConfig":{"thinkingConfig":{"thinkingBudget":4000}}}`))
	compat := geminiThinkingLevelFromClaudeThinking(&antigravity.ThinkingConfig{Type: "enabled", BudgetTokens: 4000})
	require.Equal(t, native, compat)
}

// 回归锁：裸名解析必须发生在 getMappedModel 这一层，从而覆盖全部 Antigravity
// 转发入口（/v1beta 原生、/v1/chat/completions、/v1/messages）以及账号测试。
// 早前只在单个入口打补丁，导致 /v1/messages 仍把裸名透传给上游并触发
// 404 "Requested entity was not found."
func TestGetMappedModelResolvesBareGeminiModelForAllEntrypoints(t *testing.T) {
	svc := &AntigravityGatewayService{}
	account := newAntigravityAccountWithMapping(map[string]string{
		"gemini-3.8-flash-low":    "gemini-3.8-flash-low",
		"gemini-3.8-flash-medium": "gemini-3.8-flash-medium",
		"gemini-3.8-flash-high":   "gemini-3.8-flash-high",
	})

	t.Run("default entrypoint falls back to high", func(t *testing.T) {
		require.Equal(t, "gemini-3.8-flash-high", svc.getMappedModel(account, "gemini-3.8-flash"))
	})

	t.Run("caller supplied level wins", func(t *testing.T) {
		require.Equal(t, "gemini-3.8-flash-low",
			svc.getMappedModelForThinkingLevel(account, "gemini-3.8-flash", "low"))
		require.Equal(t, "gemini-3.8-flash-medium",
			svc.getMappedModelForThinkingLevel(account, "gemini-3.8-flash", "medium"))
	})

	t.Run("models/ prefix is handled", func(t *testing.T) {
		require.Equal(t, "gemini-3.8-flash-high", svc.getMappedModel(account, "models/gemini-3.8-flash"))
	})

	t.Run("suffixed model keeps normal mapping", func(t *testing.T) {
		require.Equal(t, "gemini-3.8-flash-low", svc.getMappedModel(account, "gemini-3.8-flash-low"))
	})

	t.Run("unsupported model still reports unsupported", func(t *testing.T) {
		require.Empty(t, svc.getMappedModel(account, "gemini-9.9-flash"))
	})
}

func TestGetAvailableModels_AntigravityUsesExplicitRawMappingWithoutAutoInjectedSuffixes(t *testing.T) {
	groupID := int64(16)
	repo := &modelsListAccountRepoStub{
		byGroup: map[int64][]Account{
			groupID: {
				*newAntigravityAccountWithMapping(map[string]string{
					"claude-opus-5-5":  "claude-opus-4-6-thinking",
					"gemini-3.1-pro":   "gemini-pro-agent",
					"gemini-3.8-flash": "gemini-3.8-flash-high",
					"gemini-4-argon":   "gemini-4-argon-high",
				}),
			},
		},
	}
	svc := &GatewayService{accountRepo: repo}
	models := svc.GetAvailableModels(context.Background(), &groupID, PlatformAntigravity)
	require.Equal(t, []string{
		"claude-opus-5-5",
		"gemini-3.1-pro",
		"gemini-3.8-flash",
		"gemini-4-argon",
	}, models)
}

func TestAntigravityConvergedBareMapping_SupportsBothBareAndSuffixedRequestsAcrossCompositeAndAllowlist(t *testing.T) {
	groupID := int64(6)
	convergedAccount := newAntigravityAccountWithMapping(map[string]string{
		"claude-opus-5-5":   "claude-opus-5-5-high",
		"claude-sonnet-5-5": "claude-sonnet-5-5-high",
		"claude-opus-4-6":   "claude-opus-4-6-thinking",
		"claude-sonnet-4-6": "claude-sonnet-4-6",
		"gemini-3.1-pro":    "gemini-pro-agent",
		"gemini-3.8-flash":  "gemini-3.8-flash-high",
		"gemini-4-argon":    "gemini-4-argon-high",
	})
	repo := &modelsListAccountRepoStub{
		byGroup: map[int64][]Account{
			groupID: {*convergedAccount},
		},
	}
	gatewaySvc := &GatewayService{accountRepo: repo}
	agSvc := &AntigravityGatewayService{}

	// 1. /v1/models 只返回收敛后的裸名
	models := gatewaySvc.GetAvailableModels(context.Background(), &groupID, PlatformAntigravity)
	require.Equal(t, []string{
		"claude-opus-4-6",
		"claude-opus-5-5",
		"claude-sonnet-4-6",
		"claude-sonnet-5-5",
		"gemini-3.1-pro",
		"gemini-3.8-flash",
		"gemini-4-argon",
	}, models)

	// 2. 复合分组 (composite) 模型归属判定：无论请求裸名还是后缀名，均认领为 antigravity（不会跌落到 gemini/anthropic 导致 503）
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(gatewaySvc.resolveCompositeModelOwnership)
	for _, reqModel := range []string{
		"gemini-4-argon",
		"gemini-4-argon-high",
		"gemini-4-argon-medium",
		"gemini-4-argon-low",
		"claude-opus-5-5",
		"claude-opus-5-5-high",
		"claude-opus-5-5-max",
		"claude-opus-5-5-medium",
		"claude-opus-5-5-low",
		"claude-sonnet-5-5-low",
		"gemini-3.8-flash-low",
		"gemini-3.1-pro-low",
		"gemini-3.1-pro-high",
		"claude-opus-4-6-high",
		"claude-sonnet-4-6-low",
	} {
		decision, err := resolver.Resolve(context.Background(), groupID, reqModel, CompositeRouteEndpointMessages)
		require.NoError(t, err, "model: %s", reqModel)
		require.True(t, decision.Matched, "model: %s", reqModel)
		require.Equal(t, PlatformAntigravity, decision.TargetPlatform, "model %s must resolve to antigravity, not %s", reqModel, decision.TargetPlatform)
		require.Equal(t, CompositeRouteSourceAccount, decision.Source, "model: %s", reqModel)

		ctx := WithCompositeRouteDecision(context.Background(), decision)
		require.True(t, gatewaySvc.isModelSupportedByAccountWithContext(ctx, convergedAccount, reqModel),
			"account must be schedulable for %s under composite route context", reqModel)
	}

	// 3. 转发层映射：带后缀请求映射到对应档位，裸名请求按 thinkingLevel 动态映射到对应档位
	require.Equal(t, "gemini-4-argon-high", agSvc.getMappedModelForThinkingLevel(convergedAccount, "gemini-4-argon-high", "low"))
	require.Equal(t, "gemini-4-argon-low", agSvc.getMappedModelForThinkingLevel(convergedAccount, "gemini-4-argon-low", "high"))
	require.Equal(t, "gemini-4-argon-low", agSvc.getMappedModelForThinkingLevel(convergedAccount, "gemini-4-argon", "low"))
	require.Equal(t, "gemini-4-argon-medium", agSvc.getMappedModelForThinkingLevel(convergedAccount, "gemini-4-argon", "medium"))
	require.Equal(t, "gemini-4-argon-high", agSvc.getMappedModelForThinkingLevel(convergedAccount, "gemini-4-argon", "high"))

	require.Equal(t, "claude-opus-5-5-high", agSvc.getMappedModelForThinkingLevel(convergedAccount, "claude-opus-5-5-max", "low"))
	require.Equal(t, "claude-opus-5-5-low", agSvc.getMappedModelForThinkingLevel(convergedAccount, "claude-opus-5-5-low", "high"))
	require.Equal(t, "claude-opus-5-5-low", agSvc.getMappedModelForThinkingLevel(convergedAccount, "claude-opus-5-5", "low"))
	require.Equal(t, "claude-opus-5-5-medium", agSvc.getMappedModelForThinkingLevel(convergedAccount, "claude-opus-5-5", "medium"))
	require.Equal(t, "claude-opus-5-5-high", agSvc.getMappedModelForThinkingLevel(convergedAccount, "claude-opus-5-5", "high"))

	require.Equal(t, "gemini-pro-agent", agSvc.getMappedModelForThinkingLevel(convergedAccount, "gemini-3.1-pro-high", "low"))
	require.Equal(t, "gemini-3.1-pro-low", agSvc.getMappedModelForThinkingLevel(convergedAccount, "gemini-3.1-pro-low", "high"))
	require.Equal(t, "claude-opus-4-6-thinking", agSvc.getMappedModelForThinkingLevel(convergedAccount, "claude-opus-4-6-high", "low"))
	require.Equal(t, "claude-sonnet-4-6", agSvc.getMappedModelForThinkingLevel(convergedAccount, "claude-sonnet-4-6-low", "high"))

	// 4. 分组白名单 (GroupModelAllowlist)：仅配置裸名时，列表只出裸名，准入同时放行裸名与后缀名
	allowlist := GroupModelAllowlist{
		Enabled: true,
		Models:  models,
	}
	require.Equal(t, models, allowlist.FilterForListing(models))
	for _, reqModel := range []string{
		"gemini-4-argon",
		"gemini-4-argon-high",
		"gemini-4-argon-low",
		"claude-opus-5-5",
		"claude-opus-5-5-high",
		"claude-opus-5-5-max",
		"claude-opus-5-5-low",
		"claude-opus-4-6-high",
		"gemini-3.8-flash-tiered",
	} {
		require.True(t, allowlist.Allows(reqModel), "allowlist with bare model must allow %s", reqModel)
	}
	require.False(t, allowlist.Allows("gemini-2.5-pro-high"))
}
