package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type gatewayModelsAccountRepoStub struct {
	service.AccountRepository

	byGroup map[int64][]service.Account
}

type gatewayModelsRouteRepoStub struct {
	service.CompositeModelRouteRepository
	routes []service.CompositeModelRoute
	err    error
}

func (s *gatewayModelsRouteRepoStub) ListByGroup(_ context.Context, _ int64, _ bool) ([]service.CompositeModelRoute, error) {
	return s.routes, s.err
}

type gatewayModelsResponseForTest struct {
	Object string                    `json:"object"`
	Data   []gatewayModelItemForTest `json:"data"`
}

type codexModelsResponseForTest struct {
	Models []struct {
		Slug                     string                       `json:"slug"`
		SupportedReasoningLevels []codexReasoningLevelForTest `json:"supported_reasoning_levels"`
		InputModalities          []string                     `json:"input_modalities"`
		ModelMessages            map[string]json.RawMessage   `json:"model_messages"`
		TruncationPolicy         map[string]json.RawMessage   `json:"truncation_policy"`
		AvailabilityNUX          json.RawMessage              `json:"availability_nux"`
		Upgrade                  json.RawMessage              `json:"upgrade"`
	} `json:"models"`
}

type codexReasoningLevelForTest struct {
	Effort string `json:"effort"`
}

type gatewayModelItemForTest struct {
	ID                      string                                `json:"id"`
	Object                  string                                `json:"object"`
	Created                 int64                                 `json:"created"`
	OwnedBy                 string                                `json:"owned_by"`
	CreatedAt               string                                `json:"created_at"`
	SupportsReasoningEffort bool                                  `json:"supportsReasoningEffort"`
	ReasoningEffort         string                                `json:"reasoningEffort"`
	ReasoningEfforts        []gatewayReasoningEffortOptionForTest `json:"reasoningEfforts"`
}

type gatewayReasoningEffortOptionForTest struct {
	Value   string `json:"value"`
	Label   string `json:"label"`
	Default bool   `json:"default"`
}

func (s *gatewayModelsAccountRepoStub) ListSchedulableByGroupID(ctx context.Context, groupID int64) ([]service.Account, error) {
	accounts, ok := s.byGroup[groupID]
	if !ok {
		return nil, nil
	}
	out := make([]service.Account, len(accounts))
	copy(out, accounts)
	return out, nil
}

func (s *gatewayModelsAccountRepoStub) ListByGroup(ctx context.Context, groupID int64) ([]service.Account, error) {
	return s.ListSchedulableByGroupID(ctx, groupID)
}

func (s *gatewayModelsAccountRepoStub) ListModelAvailabilityCandidates(ctx context.Context, groupID *int64, _ []string, _ bool) ([]service.Account, error) {
	if groupID == nil {
		return nil, nil
	}
	return s.ListSchedulableByGroupID(ctx, *groupID)
}

func newGatewayModelsHandlerForTest(repo service.AccountRepository, routeRepo ...service.CompositeModelRouteRepository) *GatewayHandler {
	var resolver *service.CompositeRouteResolver
	if len(routeRepo) > 0 {
		resolver = service.NewCompositeRouteResolver(routeRepo[0])
	}
	return &GatewayHandler{
		gatewayService: service.NewGatewayService(
			repo,
			nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
			nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, resolver, nil, nil,
		),
	}
}

func TestGatewayCompositeRouteModelsAppearInBothCatalogs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 7784
	accounts := &gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{
		groupID: {{ID: 1, Platform: service.PlatformOpenAI, Credentials: map[string]any{
			"model_mapping": map[string]any{"mapped-model": "upstream"},
		}}},
	}}
	routes := &gatewayModelsRouteRepoStub{routes: []service.CompositeModelRoute{
		{PublicModel: "route-only", MatchType: service.CompositeRouteMatchExact, Enabled: true},
		{PublicModel: "route-only", MatchType: service.CompositeRouteMatchExact, Enabled: true},
		{PublicModel: "mapped-model", MatchType: service.CompositeRouteMatchExact, Enabled: true},
		{PublicModel: "image-only", MatchType: service.CompositeRouteMatchExact, Endpoint: service.CompositeRouteEndpointImages, Enabled: true},
		{PublicModel: "prefix-", MatchType: service.CompositeRouteMatchPrefix, Enabled: true},
		{PublicModel: "disabled", MatchType: service.CompositeRouteMatchExact, Enabled: false},
	}}
	for _, tc := range []struct {
		name      string
		repo      service.CompositeModelRouteRepository
		allowlist []string
		want      []string
	}{
		{"routes", routes, nil, []string{"mapped-model", "route-only", "image-only"}},
		{"allowlist", routes, []string{"route-only", "disabled", "unknown"}, []string{"route-only"}},
		{"lookup failure", &gatewayModelsRouteRepoStub{err: errors.New("unavailable")}, nil, []string{"mapped-model"}},
		{"allowlisted lookup failure", &gatewayModelsRouteRepoStub{err: errors.New("unavailable")}, []string{"mapped-model", "unknown"}, []string{"mapped-model"}},
		{"no route repo", nil, nil, []string{"mapped-model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newGatewayModelsHandlerForTest(accounts, tc.repo)
			group := &service.Group{ID: groupID, Platform: service.PlatformComposite}
			if tc.allowlist != nil {
				group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: tc.allowlist}
			}
			for _, endpoint := range []string{"models", "codex"} {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
				c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: group})
				if endpoint == "models" {
					h.Models(c)
					var got gatewayModelsResponseForTest
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
					require.Equal(t, tc.want, modelIDsForTest(got.Data))
				} else {
					h.CodexModels(c)
					var got codexModelsResponseForTest
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
					want := tc.want
					if tc.name == "routes" {
						want = []string{"mapped-model", "route-only"}
					}
					require.ElementsMatch(t, want, codexModelSlugsForTest(got.Models))
				}
				require.Equal(t, http.StatusOK, rec.Code)
			}
		})
	}
}

func TestDefaultModelIDsForCompositeIncludesAntigravityDefaults(t *testing.T) {
	antigravityIDs := defaultModelIDsForPlatform(service.PlatformAntigravity)
	require.NotEmpty(t, antigravityIDs)

	compositeIDs := defaultModelIDsForPlatform(service.PlatformComposite)
	require.Contains(t, compositeIDs, antigravityIDs[0])
}

// Scenario: Anthropic defaults contain only Claude while Antigravity keeps its own Gemini models.
func TestDefaultModelIDsForAnthropicExcludeAntigravityGemini(t *testing.T) {
	anthropicIDs := defaultModelIDsForPlatform(service.PlatformAnthropic)
	require.Contains(t, anthropicIDs, "claude-opus-4-6")
	require.NotContains(t, anthropicIDs, "gemini-2.5-flash")

	antigravityIDs := defaultModelIDsForPlatform(service.PlatformAntigravity)
	require.Contains(t, antigravityIDs, "gemini-2.5-flash")
}

// Scenario: non-OpenAI groups return a Codex manifest instead of a standard model list.
func TestGatewayCodexModels_NonOpenAIGroupsUseMappedModels(t *testing.T) {
	tests := []struct {
		name       string
		platform   string
		model      string
		efforts    []string
		modalities []string
	}{
		{
			name:       "Grok",
			platform:   service.PlatformGrok,
			model:      "grok-4.6",
			efforts:    []string{"low", "medium", "high", "xhigh"},
			modalities: []string{"text", "image"},
		},
		{
			name:       "DeepSeek",
			platform:   service.PlatformDeepseek,
			model:      "deepseek-v4-pro",
			efforts:    []string{"low", "high", "max"},
			modalities: []string{"text"},
		},
		{
			name:       "provider-qualified Claude",
			platform:   service.PlatformAnthropic,
			model:      "anthropic/claude-sonnet-4-6",
			efforts:    []string{"low", "medium", "high", "max"},
			modalities: []string{"text"},
		},
	}

	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			groupID := int64(100 + index)
			h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
				byGroup: map[int64][]service.Account{
					groupID: {
						{
							ID:       1,
							Platform: tt.platform,
							Credentials: map[string]any{
								"model_mapping": map[string]any{tt.model: tt.model},
							},
						},
					},
				},
			})

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodGet, "/models?client_version=0.147.0", nil)
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
				Group: &service.Group{ID: groupID, Platform: tt.platform},
			})

			h.CodexModels(c)

			require.Equal(t, http.StatusOK, rec.Code)
			var got codexModelsResponseForTest
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			require.Len(t, got.Models, 1)
			require.Equal(t, tt.model, got.Models[0].Slug)
			require.NotEmpty(t, got.Models[0].ModelMessages)
			require.NotEmpty(t, got.Models[0].TruncationPolicy)
			require.NotNil(t, got.Models[0].AvailabilityNUX)
			require.NotNil(t, got.Models[0].Upgrade)
			require.Equal(t, tt.efforts, codexReasoningEffortsForTest(got.Models[0].SupportedReasoningLevels))
			require.Equal(t, tt.modalities, got.Models[0].InputModalities)
		})
	}
}

// Composite Codex manifests include only mapping keys: unmapped accounts contribute no defaults.
func TestGatewayCodexModels_CompositeUsesCompleteEffectiveModelList(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 120
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {
				{
					ID:          3,
					Platform:    service.PlatformOpenAI,
					Status:      service.StatusActive,
					Schedulable: true,
					Credentials: map[string]any{},
				},
				{
					ID:       1,
					Platform: service.PlatformOpenAI,
					Credentials: map[string]any{
						"model_mapping": map[string]any{"gpt-5.5": "gpt-5.5"},
					},
				},
				{
					ID:       2,
					Platform: service.PlatformGrok,
					Credentials: map[string]any{
						"model_mapping": map[string]any{"grok-4.6": "grok-4.6"},
					},
				},
			},
		},
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/models?client_version=0.147.0", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformComposite},
	})

	h.CodexModels(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got codexModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	// Unmapped OpenAI accounts contribute no default models; only account mapping keys survive.
	require.ElementsMatch(t, []string{"gpt-5.5", "grok-4.6"}, codexModelSlugsForTest(got.Models))
}

func TestGatewayModels_UnmappedOpenAIAccountsAddNoDefaultModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 28
	const sparkModel = "gpt-5.3-codex-spark"
	const alias = "team-coder"
	parentID := int64(1)
	accounts := []service.Account{
		{ID: parentID, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth},
		{
			ID: 2, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			ParentAccountID: &parentID, QuotaDimension: "spark",
			Credentials: map[string]any{"model_mapping": map[string]any{sparkModel: sparkModel}},
		},
		{
			ID: 3, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{"model_mapping": map[string]any{alias: "gpt-5.6-sol"}},
		},
	}
	tests := []struct {
		name     string
		accounts []service.Account
		config   service.GroupModelAllowlist
		want     []string
	}{
		{
			name:     "unmapped parent and Spark shadow keep mapped keys only",
			accounts: accounts,
			want:     []string{sparkModel, alias},
		},
		{
			name:     "unmapped API key account adds no default models",
			accounts: append([]service.Account{{ID: 4, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}}, accounts[1:]...),
			want:     []string{sparkModel, alias},
		},
		{
			name:     "unmapped accounts alone retain default response shape",
			accounts: accounts[:1],
			want:     openai.DefaultModelIDs(),
		},
		{
			name:     "custom list selects mapped keys only",
			accounts: accounts,
			config:   service.GroupModelAllowlist{Enabled: true, Models: []string{alias, "gpt-5.6-sol", sparkModel, "unknown-model"}},
			want:     []string{alias, sparkModel},
		},
		{
			name:     "unavailable custom selection remains empty",
			accounts: accounts,
			config:   service.GroupModelAllowlist{Enabled: true, Models: []string{"unknown-model"}},
			want:     []string{},
		},
		{
			name:     "mapped accounts alone do not gain defaults",
			accounts: accounts[1:],
			want:     []string{sparkModel, alias},
		},
		{
			// A passthrough account with a stale mapping behaves like an unmapped
			// one: it adds the defaults but never its own mapping keys, and it no
			// longer hides the aliases declared on ordinary accounts.
			name: "passthrough account adds no defaults and no stale mapping keys",
			accounts: append([]service.Account{{
				ID: 5, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Credentials: map[string]any{"model_mapping": map[string]any{"stale-model": "stale-model"}},
				Extra:       map[string]any{"openai_passthrough": true},
			}}, accounts[1:]...),
			want: []string{sparkModel, alias},
		},
		{
			name:     "unmapped accounts from another platform do not add defaults",
			accounts: append([]service.Account{{ID: 4, Platform: service.PlatformAnthropic}}, accounts[1:]...),
			want:     []string{sparkModel, alias},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
				byGroup: map[int64][]service.Account{groupID: tt.accounts},
			})
			for range 2 {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
				c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
					Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, ModelAllowlist: tt.config},
				})
				h.Models(c)
				require.Equal(t, http.StatusOK, rec.Code)
				var got gatewayModelsResponseForTest
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
				require.Equal(t, "list", got.Object)
				require.ElementsMatch(t, tt.want, modelIDsForTest(got.Data))
				for _, model := range got.Data {
					require.Equal(t, "model", model.Object, model.ID)
					require.Positive(t, model.Created, model.ID)
					require.Equal(t, "openai", model.OwnedBy, model.ID)
					require.Empty(t, model.CreatedAt, model.ID)
				}
				if tt.config.Enabled {
					require.Equal(t, tt.want, modelIDsForTest(got.Data))
				}
			}
		})
	}
	require.Empty(t, accounts[0].GetModelMapping())
	require.True(t, accounts[0].IsModelSupported("gpt-future-model"))
	require.False(t, accounts[1].IsModelSupported("gpt-5.6-sol"))
}

func TestGatewayCodexModels_GeneratedManifestUsesFinalBodyETag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 122
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {{
				ID:       1,
				Platform: service.PlatformDeepseek,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"deepseek-v4-pro": "deepseek-v4-pro"},
				},
			}},
		},
	})
	group := &service.Group{ID: groupID, Platform: service.PlatformDeepseek}

	first := httptest.NewRecorder()
	firstContext, _ := gin.CreateTestContext(first)
	firstContext.Request = httptest.NewRequest(http.MethodGet, "/models?client_version=0.147.0", nil)
	firstContext.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: group})
	h.CodexModels(firstContext)

	require.Equal(t, http.StatusOK, first.Code)
	etag := first.Header().Get("ETag")
	require.NotEmpty(t, etag)
	require.Equal(t, service.CodexModelsManifestETag(first.Body.Bytes()), etag)

	second := httptest.NewRecorder()
	secondContext, _ := gin.CreateTestContext(second)
	secondContext.Request = httptest.NewRequest(http.MethodGet, "/models?client_version=0.147.0", nil)
	secondContext.Request.Header.Set("If-None-Match", "W/"+etag)
	secondContext.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: group})
	h.CodexModels(secondContext)

	require.Equal(t, http.StatusNotModified, second.Code)
	require.Empty(t, second.Body.Bytes())
	require.Equal(t, etag, second.Header().Get("ETag"))
}

// Scenario: group model_allowlist limits the generated Codex manifest.
func TestGatewayCodexModels_CustomModelsListFiltersCompositeManifest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 121
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {
				{
					ID:       1,
					Platform: service.PlatformOpenAI,
					Credentials: map[string]any{
						"model_mapping": map[string]any{"gpt-5.5": "gpt-5.5"},
					},
				},
				{
					ID:       2,
					Platform: service.PlatformGrok,
					Credentials: map[string]any{
						"model_mapping": map[string]any{"grok-4.6": "grok-4.6"},
					},
				},
			},
		},
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/models?client_version=0.147.0", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformComposite,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: true,
				Models:  []string{"grok-4.6"},
			},
		},
	})

	h.CodexModels(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got codexModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []string{"grok-4.6"}, codexModelSlugsForTest(got.Models))
}

func codexModelSlugsForTest(models []struct {
	Slug                     string                       `json:"slug"`
	SupportedReasoningLevels []codexReasoningLevelForTest `json:"supported_reasoning_levels"`
	InputModalities          []string                     `json:"input_modalities"`
	ModelMessages            map[string]json.RawMessage   `json:"model_messages"`
	TruncationPolicy         map[string]json.RawMessage   `json:"truncation_policy"`
	AvailabilityNUX          json.RawMessage              `json:"availability_nux"`
	Upgrade                  json.RawMessage              `json:"upgrade"`
}) []string {
	slugs := make([]string, 0, len(models))
	for _, model := range models {
		slugs = append(slugs, model.Slug)
	}
	return slugs
}

func codexReasoningEffortsForTest(levels []codexReasoningLevelForTest) []string {
	efforts := make([]string, 0, len(levels))
	for _, level := range levels {
		efforts = append(efforts, level.Effort)
	}
	return efforts
}

func TestGatewayModels_GeminiGroupFallsBackToGeminiModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(20)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{ID: 1, Platform: service.PlatformGemini},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformGemini},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "list", got.Object)
	require.Contains(t, modelIDsForTest(got.Data), "gemini-2.5-flash")
	require.NotContains(t, modelIDsForTest(got.Data), "claude-sonnet-4-6")
}

func TestGatewayModels_Grok45AdvertisesReasoningEffortForGrokBuild(t *testing.T) {
	assertGrokGatewayReasoningEfforts(t, 4409, "grok-4.5", []gatewayReasoningEffortOptionForTest{
		{Value: "low", Label: "Low"},
		{Value: "medium", Label: "Medium"},
		{Value: "high", Label: "High", Default: true},
	})
}

func TestGatewayModels_Grok46AdvertisesXHighReasoningEffortForGrokBuild(t *testing.T) {
	xhighEfforts := []gatewayReasoningEffortOptionForTest{
		{Value: "low", Label: "Low"},
		{Value: "medium", Label: "Medium"},
		{Value: "high", Label: "High", Default: true},
		{Value: "xhigh", Label: "xHigh"},
	}
	tests := []struct {
		groupID int64
		model   string
	}{
		{groupID: 4410, model: "grok-4.6"},
		{groupID: 4411, model: "grok-4.6-latest"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			assertGrokGatewayReasoningEfforts(t, tt.groupID, tt.model, xhighEfforts)
		})
	}
}

func assertGrokGatewayReasoningEfforts(t *testing.T, groupID int64, modelID string, want []gatewayReasoningEffortOptionForTest) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{
						ID:       1,
						Platform: service.PlatformGrok,
						Credentials: map[string]any{
							"model_mapping": map[string]any{modelID: modelID},
						},
					},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformGrok},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Data, 1)
	model := got.Data[0]
	require.Equal(t, modelID, model.ID)
	require.True(t, model.SupportsReasoningEffort)
	require.Equal(t, "high", model.ReasoningEffort)
	require.Equal(t, want, model.ReasoningEfforts)
}

func TestGatewayModels_GeminiGroupFiltersMappedModelsByPlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(21)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{
						ID:       1,
						Platform: service.PlatformAnthropic,
						Credentials: map[string]any{
							"model_mapping": map[string]any{
								"claude-sonnet-4-6": "claude-sonnet-4-6",
							},
						},
					},
					{
						ID:       2,
						Platform: service.PlatformGemini,
						Credentials: map[string]any{
							"model_mapping": map[string]any{
								"gemini-2.5-flash": "gemini-2.5-flash",
							},
						},
					},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformGemini},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []string{"gemini-2.5-flash"}, modelIDsForTest(got.Data))
}

// Scenario: a Composite group with only Anthropic accounts must not inherit Antigravity Gemini defaults.
func TestGatewayCodexModels_CompositeAnthropicDoesNotAdvertiseAntigravityDefaults(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(64)
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {{ID: 1, Platform: service.PlatformAnthropic}},
		},
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/models?client_version=0.147.0", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformComposite},
	})

	h.CodexModels(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got codexModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	slugs := codexModelSlugsForTest(got.Models)
	require.Contains(t, slugs, "claude-opus-4-6")
	require.NotContains(t, slugs, "gemini-2.5-flash")
}

// Scenario: Antigravity retains its own Claude and Gemini defaults inside Composite groups.
func TestGatewayModels_CompositeAntigravityAdvertisesAntigravityDefaults(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(65)
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {{ID: 1, Platform: service.PlatformAntigravity}},
		},
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformComposite},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	ids := modelIDsForTest(got.Data)
	require.Contains(t, ids, "claude-opus-4-6")
	require.Contains(t, ids, "gemini-2.5-flash")
}

func TestGatewayModels_CustomModelsListDisabledKeepsOriginalModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(22)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{
						ID:       1,
						Platform: service.PlatformOpenAI,
						Credentials: map[string]any{
							"model_mapping": map[string]any{
								"gpt-5.5": "gpt-5.5",
								"gpt-5.4": "gpt-5.4",
							},
						},
					},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformOpenAI,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: false,
				Models:  []string{"gpt-5.5"},
			},
		},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []string{"gpt-5.4", "gpt-5.5"}, modelIDsForTest(got.Data))
}

func TestGatewayModels_CustomModelsListFiltersAndOrdersMappedModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(23)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{
						ID:       1,
						Platform: service.PlatformOpenAI,
						Credentials: map[string]any{
							"model_mapping": map[string]any{
								"gpt-5.4":         "gpt-5.4",
								"gpt-5.5":         "gpt-5.5",
								"legacy-gpt-2024": "legacy-gpt-2024",
							},
						},
					},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformOpenAI,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: true,
				Models:  []string{"gpt-5.5", "missing-model", "gpt-5.4"},
			},
		},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []string{"gpt-5.5", "gpt-5.4"}, modelIDsForTest(got.Data))
}

func TestGatewayModels_CompositeCustomModelsListFiltersAcrossConcretePlatforms(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(33)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{
						ID:       1,
						Platform: service.PlatformOpenAI,
						Credentials: map[string]any{
							"model_mapping": map[string]any{
								"gpt-5.4": "gpt-5.4",
								"gpt-5.5": "gpt-5.5",
							},
						},
					},
					{
						ID:       2,
						Platform: service.PlatformGemini,
						Credentials: map[string]any{
							"model_mapping": map[string]any{
								"gemini-2.5-flash": "gemini-2.5-flash",
							},
						},
					},
					{
						ID:       3,
						Platform: service.PlatformAntigravity,
						Credentials: map[string]any{
							"model_mapping": map[string]any{
								"ag-custom-model": "ag-custom-model",
							},
						},
					},
					{
						ID:       4,
						Platform: service.PlatformKimi,
						Credentials: map[string]any{
							"model_mapping": map[string]any{"kimi-custom": "kimi-upstream"},
						},
					},
					{
						ID:       5,
						Platform: service.PlatformZhipu,
						Credentials: map[string]any{
							"model_mapping": map[string]any{"glm-custom": "glm-upstream"},
						},
					},
					{
						ID:       6,
						Platform: service.PlatformDeepseek,
						Credentials: map[string]any{
							"model_mapping": map[string]any{"deepseek-custom": "deepseek-upstream"},
						},
					},
					{
						ID:       7,
						Platform: service.PlatformMiniMax,
						Credentials: map[string]any{
							"model_mapping": map[string]any{"minimax-custom": "MiniMax-M3"},
						},
					},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformComposite,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: true,
				Models:  []string{"gemini-2.5-flash", "missing-model", "ag-custom-model", "gpt-5.5", "kimi-custom", "glm-custom", "deepseek-custom", "minimax-custom"},
			},
		},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []string{"gemini-2.5-flash", "ag-custom-model", "gpt-5.5", "kimi-custom", "glm-custom", "deepseek-custom", "minimax-custom"}, modelIDsForTest(got.Data))
}

func TestGatewayModels_CompositeGrokImplicitMappingBlocksOpenAIDefaultFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(34)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{ID: 1, Platform: service.PlatformOpenAI},
					{ID: 2, Platform: service.PlatformGrok},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformComposite},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	ids := modelIDsForTest(got.Data)
	// grok 无映射账号按隐式映射（account.go:636-637）贡献非空模型集，allEmpty=false，
	// openai 默认模型不再注入。
	require.NotContains(t, ids, "gpt-5.5")
	require.Contains(t, ids, "grok-4.3")
	require.NotContains(t, ids, "claude-sonnet-4-6")
	require.NotContains(t, ids, "gemini-2.5-flash")
}

// CN 供应商没有静态默认模型列表：composite 下无映射的可调度 CN 账号不得把
// defaultModelIDsForPlatform default 分支的 Claude 列表挂到 CN 平台名下。
func TestGatewayModels_CompositeUnmappedCNAccountsContributeNoDefaults(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(35)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{ID: 1, Platform: service.PlatformOpenAI},
					{ID: 2, Platform: service.PlatformKimi},
					{ID: 3, Platform: service.PlatformZhipu},
					{ID: 4, Platform: service.PlatformDeepseek},
					{ID: 5, Platform: service.PlatformMiniMax},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformComposite},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	ids := modelIDsForTest(got.Data)
	require.Contains(t, ids, "gpt-5.5")
	require.NotContains(t, ids, "claude-sonnet-4-6")
}

// 独立 CN 分组沿用 default 分支的 Claude 默认列表（Claude Code 客户端请求的
// 就是这些模型名并经账号 model_mapping 转换），composite 支持不得改变该回退。
func TestDefaultModelIDsForPlatform_CNProvidersKeepClaudeDefaults(t *testing.T) {
	want := make([]string, 0, len(claude.DefaultModels))
	for _, model := range claude.DefaultModels {
		want = append(want, model.ID)
	}
	for _, platform := range []string{service.PlatformKimi, service.PlatformZhipu, service.PlatformDeepseek, service.PlatformMiniMax} {
		require.Equal(t, want, defaultModelIDsForPlatform(platform), "platform=%s", platform)
	}
}

func TestDefaultCodexModelIDsForPlatform_DeepSeekUsesDeepSeekModels(t *testing.T) {
	require.Equal(t, []string{"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-flash"}, defaultCodexModelIDsForPlatform(service.PlatformDeepseek))
	require.Equal(t, []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.5"}, defaultCodexModelIDsForPlatform(service.PlatformMiniMax))
	require.Equal(t, defaultModelIDsForPlatform(service.PlatformAnthropic), defaultCodexModelIDsForPlatform(service.PlatformAnthropic))
}

func TestDefaultModelIDsForPlatform_TypeSafeUsesJev(t *testing.T) {
	require.Equal(t, []string{"jev-latest"}, defaultModelIDsForPlatform(service.PlatformTypeSafe))
}

func TestGatewayCodexModels_DeepSeekWithoutMappingUsesDeepSeekDefaults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 130
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {
				{
					ID:          1,
					Platform:    service.PlatformDeepseek,
					Status:      service.StatusActive,
					Schedulable: true,
					Credentials: map[string]any{},
				},
			},
		},
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/models?client_version=0.150.0", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformDeepseek},
	})

	h.CodexModels(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got codexModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	slugs := make([]string, 0, len(got.Models))
	for _, model := range got.Models {
		slugs = append(slugs, model.Slug)
	}
	require.Contains(t, slugs, "deepseek-v4-pro")
	require.Contains(t, slugs, "deepseek-v4-flash")
	require.NotContains(t, slugs, "claude-sonnet-4-6")
	require.NotContains(t, slugs, "claude-opus-4-6")
}

func TestGatewayCodexModels_OmitsWildcardMappingKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 131
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {
				{
					ID:       1,
					Platform: service.PlatformDeepseek,
					Credentials: map[string]any{
						"model_mapping": map[string]any{
							"foo-*":           "deepseek-v4-pro",
							"deepseek-v4-pro": "deepseek-v4-pro",
						},
					},
				},
			},
		},
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/models?client_version=0.150.0", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformDeepseek},
	})

	h.CodexModels(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got codexModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	slugs := make([]string, 0, len(got.Models))
	for _, model := range got.Models {
		slugs = append(slugs, model.Slug)
	}
	require.Equal(t, []string{"deepseek-v4-pro"}, slugs)
}

func TestGatewayModels_CustomModelsListKeepsConcreteModelAllowedByWildcardMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(26)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{
						ID:       1,
						Platform: service.PlatformAnthropic,
						Credentials: map[string]any{
							"model_mapping": map[string]any{
								"claude-*": "claude-sonnet-4-6",
							},
						},
					},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformAnthropic,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: true,
				Models:  []string{"claude-sonnet-4-6"},
			},
		},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []string{"claude-sonnet-4-6"}, modelIDsForTest(got.Data))
}

func TestGatewayModels_AnthropicCustomModelsListKeepsMappedModelsOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(28)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{
						ID:       1,
						Platform: service.PlatformAnthropic,
						Type:     service.AccountTypeOAuth,
					},
					{
						ID:       2,
						Platform: service.PlatformAnthropic,
						Type:     service.AccountTypeAPIKey,
						Credentials: map[string]any{
							"model_mapping": map[string]any{
								"deepseek-v4-pro": "deepseek-v4-pro",
							},
						},
					},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformAnthropic,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: true,
				Models:  []string{"claude-fable-5", "claude-opus-4-8", "deepseek-v4-pro"},
			},
		},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []string{"deepseek-v4-pro"}, modelIDsForTest(got.Data))
}

func TestGatewayModels_AnthropicCustomModelsListDisabledKeepsMappedModelList(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(29)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{
						ID:       1,
						Platform: service.PlatformAnthropic,
						Type:     service.AccountTypeOAuth,
					},
					{
						ID:       2,
						Platform: service.PlatformAnthropic,
						Type:     service.AccountTypeAPIKey,
						Credentials: map[string]any{
							"model_mapping": map[string]any{
								"deepseek-v4-pro": "deepseek-v4-pro",
							},
						},
					},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformAnthropic,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: false,
				Models:  []string{"claude-fable-5", "deepseek-v4-pro"},
			},
		},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []string{"deepseek-v4-pro"}, modelIDsForTest(got.Data))
}

func TestGatewayModels_AnthropicAllowlistWithoutMappingsStaysEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(30)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{
						ID:       1,
						Platform: service.PlatformAnthropic,
						Type:     service.AccountTypeOAuth,
					},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformAnthropic,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: true,
				Models:  []string{"claude-opus-4-6-thinking", "claude-sonnet-4-5"},
			},
		},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Empty(t, modelIDsForTest(got.Data))
}

func TestGatewayModels_AllowlistEmptyIntersectionReturnsEmptyList(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(24)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{
						ID:       1,
						Platform: service.PlatformOpenAI,
						Credentials: map[string]any{
							"model_mapping": map[string]any{
								"gpt-5.4": "gpt-5.4",
							},
						},
					},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformOpenAI,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: true,
				Models:  []string{"gpt-5.5"},
			},
		},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Empty(t, modelIDsForTest(got.Data))
}

func TestGatewayModels_AllowlistEmptySourceKeepsEmptyListing(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(25)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{ID: 1, Platform: service.PlatformOpenAI},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformOpenAI,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: true,
				Models:  []string{"gpt-5.5", "legacy-gpt-2024", "gpt-5.4"},
			},
		},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Empty(t, modelIDsForTest(got.Data))
}

// Scenario: WB groups with unmapped accounts (no credentials or credentials
// without model_mapping) advertise exactly the WB whitelist on /v1/models —
// never Claude defaults (AC-26-01) — and GetModelMapping resolves to the
// id→id identity mapping over the whitelist (AC-26-04①).
func TestGatewayModels_WBUnmappedAccountsListsWhitelist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 7701
	accounts := []service.Account{
		{ID: 1, Platform: service.PlatformWB},
		{ID: 2, Platform: service.PlatformWB, Credentials: map[string]any{}},
	}
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: accounts},
	})

	for range 2 {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
			Group: &service.Group{ID: groupID, Platform: service.PlatformWB},
		})
		h.Models(c)
		require.Equal(t, http.StatusOK, rec.Code)
		var got gatewayModelsResponseForTest
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.Equal(t, "list", got.Object)
		ids := modelIDsForTest(got.Data)
		require.ElementsMatch(t, domain.WbModelWhitelist, ids)
		for _, id := range ids {
			require.NotContains(t, id, "claude-")
		}
	}

	mapping := accounts[0].GetModelMapping()
	require.Len(t, mapping, len(domain.WbModelWhitelist))
	for _, id := range domain.WbModelWhitelist {
		require.Equal(t, id, mapping[id])
	}
}

// Scenario: an explicit model_mapping on a WB account is intersected with the
// whitelist — off-whitelist keys are dropped, and an all-invalid mapping yields
// an empty list with 200 (AC-26-01 hard boundary).
func TestGatewayModels_WBExplicitMappingIntersectWhitelist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name    string
		mapping map[string]any
		want    []string
	}{
		{"mixed mapping keeps whitelist keys only", map[string]any{
			"gpt-5.5":           "gpt-5.5",
			"claude-sonnet-4-5": "claude-sonnet-4-5",
		}, []string{"gpt-5.5"}},
		{"all-invalid mapping returns empty list", map[string]any{
			"claude-sonnet-4-5": "claude-sonnet-4-5",
		}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const groupID int64 = 7702
			h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
				byGroup: map[int64][]service.Account{
					groupID: {{
						ID:          1,
						Platform:    service.PlatformWB,
						Credentials: map[string]any{"model_mapping": tt.mapping},
					}},
				},
			})
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
				Group: &service.Group{ID: groupID, Platform: service.PlatformWB},
			})
			h.Models(c)
			require.Equal(t, http.StatusOK, rec.Code)
			var got gatewayModelsResponseForTest
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			require.Equal(t, tt.want, modelIDsForTest(got.Data))
		})
	}
}

// Scenario: a WB group with no accounts falls back to the whitelist — never to
// Claude defaults (KDR-8 fallback leg).
func TestGatewayModels_WBNoAccountsFallsBackToWhitelist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 7703
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{},
	})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformWB},
	})
	h.Models(c)
	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	ids := modelIDsForTest(got.Data)
	require.ElementsMatch(t, domain.WbModelWhitelist, ids)
	for _, id := range ids {
		require.NotContains(t, id, "claude-")
	}
}

// Scenario: default model IDs for WB come from the whitelist, and the Codex
// default delegates to the same source (AC-26-02).
func TestDefaultModelIDsForPlatform_WBUsesWhitelist(t *testing.T) {
	wbIDs := defaultModelIDsForPlatform(service.PlatformWB)
	require.Equal(t, domain.WbModelWhitelist, wbIDs)
	require.Equal(t, wbIDs, defaultCodexModelIDsForPlatform(service.PlatformWB))
}

// Scenario: a WB group with a model allowlist filters the whitelist source
// down to the allowed models.
func TestGatewayModels_WBAllowlistFiltersWithinWhitelist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 7704
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {{ID: 1, Platform: service.PlatformWB}},
		},
	})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformWB,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: true,
				Models:  []string{"gpt-5.5"},
			},
		},
	})
	h.Models(c)
	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []string{"gpt-5.5"}, modelIDsForTest(got.Data))
}

// Scenario: a WB group with a model allowlist that intersects nothing returns
// an empty list — both for a mapped account whose keys are all filtered out and
// for a group with no accounts at all.
func TestGatewayModels_WBAllowlistEmptyIntersectionReturnsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name     string
		accounts []service.Account
	}{
		{"mapped keys filtered out", []service.Account{{
			ID:          1,
			Platform:    service.PlatformWB,
			Credentials: map[string]any{"model_mapping": map[string]any{"gpt-5.5": "gpt-5.5"}},
		}}},
		{"no accounts", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groupID := int64(7705)
			h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
				byGroup: map[int64][]service.Account{groupID: tt.accounts},
			})
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
				Group: &service.Group{
					ID:       groupID,
					Platform: service.PlatformWB,
					ModelAllowlist: service.GroupModelAllowlist{
						Enabled: true,
						Models:  []string{"claude-sonnet-4-5"},
					},
				},
			})
			h.Models(c)
			require.Equal(t, http.StatusOK, rec.Code)
			var got gatewayModelsResponseForTest
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			require.Empty(t, modelIDsForTest(got.Data))
		})
	}
}

// Scenario: a WB account with non-nil credentials but no model_mapping resolves
// to the whitelist identity mapping too (resolveModelMapping branch ②,
// AC-26-04①).
func TestGatewayModels_WBAccountCredentialsNoMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 7706
	account := service.Account{
		ID:          1,
		Platform:    service.PlatformWB,
		Credentials: map[string]any{},
	}
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {account}},
	})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformWB},
	})
	h.Models(c)
	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.ElementsMatch(t, domain.WbModelWhitelist, modelIDsForTest(got.Data))

	mapping := account.GetModelMapping()
	require.Len(t, mapping, len(domain.WbModelWhitelist))
	for _, id := range domain.WbModelWhitelist {
		require.Equal(t, id, mapping[id])
	}
}

// Codex 发现链路（/models?client_version=）对 WB 分组同样执行白名单钉死：账号显式
// mapping 全为白名单外键时，manifest 模型列表为空（非空源被求交清空 = 终态，
// AC-26-01 硬边界，绝不回退白名单默认）。
func TestGatewayCodexModels_WBAllOffWhitelistMappingReturnsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 7707
	account := service.Account{
		ID:       1,
		Platform: service.PlatformWB,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"claude-sonnet-4-5": "claude-sonnet-4-5",
				"gpt-5.6-sol-x":     "gpt-5.6-sol-x",
			},
		},
	}
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {account}},
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/models?client_version=0.147.0", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformWB},
	})

	h.CodexModels(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got codexModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Empty(t, codexModelSlugsForTest(got.Models))
}

// Codex 发现链路 allowlist 分支的出口求交（KDR-8 出口门）：allowlist 配置原文
// gpt-5.6-sol-thinking 经 -thinking 归一命中白名单键 gpt-5.6-sol，FilterForListing
// 写出配置原文；出口再与白名单求交将该变体剔除，不留泄露口子。
func TestGatewayCodexModels_WBAllowlistThinkingVariantExcluded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 7708
	account := service.Account{
		ID:       1,
		Platform: service.PlatformWB,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"gpt-5.6-sol": "gpt-5.6-sol",
			},
		},
	}
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {account}},
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/models?client_version=0.147.0", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformWB,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: true,
				Models:  []string{"gpt-5.6-sol-thinking"},
			},
		},
	})

	h.CodexModels(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got codexModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Empty(t, codexModelSlugsForTest(got.Models))
}

func modelIDsForTest(models []gatewayModelItemForTest) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

// gemini 分组混合调度：组内 antigravity 账号的 gemini-* 映射也应出现在 /v1/models，
// 与路由层（listSchedulableAccountsOnce 的 useMixedScheduling）同源；claude-* 不带入，
// 非混合调度的异平台账号（anthropic）仍被过滤。
func TestGatewayModels_GeminiGroupIncludesAntigravityGeminiMappings(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(22)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{
						ID:       1,
						Platform: service.PlatformAntigravity,
						Extra:    map[string]any{"mixed_scheduling": true},
						Credentials: map[string]any{
							"model_mapping": map[string]any{
								"gemini-3.8-flash-high": "gemini-3.8-flash-high",
								"gemini-synced-custom":  "gemini-3.8-flash-high",
								"gemini-3.8-flash-low":  "gemini-3.8-flash-low",
								"claude-sonnet-4-6":     "claude-sonnet-4-6",
							},
						},
					},
					{ID: 3, Platform: service.PlatformAntigravity,
						Extra:       map[string]any{"mixed_scheduling": false},
						Credentials: map[string]any{"model_mapping": map[string]any{"gemini-disabled-only": "gemini-3.8-flash-high"}}},
					{ID: 4, Platform: service.PlatformAntigravity,
						Credentials: map[string]any{"model_mapping": map[string]any{"gemini-unset-only": "gemini-3.8-flash-high"}}},
					{ID: 5, Platform: service.PlatformGemini,
						Credentials: map[string]any{"model_mapping": map[string]any{"gemini-native-only": "gemini-2.5-pro"}}},
					{
						ID:       2,
						Platform: service.PlatformAnthropic,
						Credentials: map[string]any{
							"model_mapping": map[string]any{
								"claude-opus-4-6": "claude-opus-4-6",
							},
						},
					},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformGemini},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	ids := modelIDsForTest(got.Data)
	// 账号显式配置的 gemini-* 映射必须出现（resolveModelMapping 还会补一组 antigravity
	// 默认透传别名，这里只断言显式条目与排除项，不锁定补齐后的完整集合）。
	require.Contains(t, ids, "gemini-3.8-flash-high")
	require.Contains(t, ids, "gemini-synced-custom")
	require.Contains(t, ids, "gemini-3.8-flash-low")
	require.Contains(t, ids, "gemini-native-only")
	require.NotContains(t, ids, "gemini-disabled-only")
	require.NotContains(t, ids, "gemini-unset-only")
	// antigravity 账号的 claude-* 映射与 anthropic 账号的映射都不应出现在 gemini 分组。
	require.NotContains(t, ids, "claude-sonnet-4-6")
	require.NotContains(t, ids, "claude-opus-4-6")
	for _, id := range ids {
		require.True(t, strings.HasPrefix(id, "gemini-"), "unexpected non-gemini model on gemini group: %s", id)
	}
	// 没有回落到 geminicli 静态表。
	require.NotContains(t, ids, "gemini-2.0-flash")
}

// antigravity 账号未配置 model_mapping 时使用 DefaultAntigravityModelMapping：
// gemini 分组应列出其中的 gemini-* 条目，而不是回落到 geminicli 静态表；claude-* 仍不出现。
func TestGatewayModels_GeminiGroupUsesAntigravityDefaultMappingWhenUnset(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(23)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{ID: 1, Platform: service.PlatformAntigravity, Extra: map[string]any{"mixed_scheduling": true}, Credentials: map[string]any{}},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformGemini},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	ids := modelIDsForTest(got.Data)
	require.Contains(t, ids, "gemini-3.8-flash-high")
	for _, id := range ids {
		require.True(t, strings.HasPrefix(id, "gemini-"), "unexpected non-gemini model on gemini group: %s", id)
	}
	// geminicli 静态表独有条目不应出现（说明没有回落到默认列表）。
	require.NotContains(t, ids, "gemini-2.0-flash")
}

// Codex 通过 /models?client_version= 走 CodexModels，同样应看到混合调度账号的 gemini-* 映射。
func TestGatewayModels_CodexGeminiGroupListsAntigravityGeminiMappings(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(24)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{
						ID:       1,
						Platform: service.PlatformAntigravity,
						Extra:    map[string]any{"mixed_scheduling": true},
						Credentials: map[string]any{
							"model_mapping": map[string]any{
								"gemini-3.8-flash-high": "gemini-3.8-flash-high",
								"gemini-synced-custom":  "gemini-3.8-flash-high",
								"claude-sonnet-4-6":     "claude-sonnet-4-6",
							},
						},
					},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/models?client_version=0.147.0", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformGemini},
	})

	h.CodexModels(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got codexModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	slugs := codexModelSlugsForTest(got.Models)
	require.Contains(t, slugs, "gemini-3.8-flash-high")
	require.Contains(t, slugs, "gemini-synced-custom")
	require.NotContains(t, slugs, "claude-sonnet-4-6")
	require.NotContains(t, slugs, "gemini-2.0-flash")
	for _, slug := range slugs {
		require.True(t, strings.HasPrefix(slug, "gemini-"), "unexpected non-gemini model on gemini group: %s", slug)
	}
}

func TestGatewayModels_GPT6SolLunaDiscoveryRespectsGroupAndAccountRestrictions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		selected   []string
		restricted bool
		want       []string
	}{
		{"allowlist over empty source stays empty", []string{"gpt-6.1-sol", "gpt-6-luna", "gpt-6-sol"}, false, []string{}},
		{"unmapped account offers nothing to select", []string{"gpt-5.6-sol"}, false, []string{}},
		{"account restricts new models", []string{"gpt-6.1-sol", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol"}, true, []string{"gpt-5.6-sol"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(25)
			account := service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
			if tc.restricted {
				account.Credentials = map[string]any{"model_mapping": map[string]any{"gpt-5.6-sol": "gpt-5.6-sol"}}
			}
			h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{groupID: {account}}})
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: tc.selected}}})
			h.Models(c)
			require.Equal(t, http.StatusOK, rec.Code)
			var got gatewayModelsResponseForTest
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			require.Equal(t, tc.want, modelIDsForTest(got.Data))
		})
	}
}

// Scenario: jev-latest only works through /v1/systemone, so Composite groups list
// it in /v1/models only when they can serve it, and never in the Codex manifest.
func TestGatewayModels_CompositeTypeSafeListingScope(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(66)
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {{ID: 1, Platform: service.PlatformAnthropic}, {ID: 2, Platform: service.PlatformTypeSafe, Type: service.AccountTypeAPIKey}},
		},
	})
	newContext := func(path string) (*gin.Context, *httptest.ResponseRecorder) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, path, nil)
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
			Group: &service.Group{ID: groupID, Platform: service.PlatformComposite},
		})
		return c, rec
	}

	c, rec := newContext("/v1/models")
	h.Models(c)
	require.Equal(t, http.StatusOK, rec.Code)
	var models gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &models))
	require.Contains(t, modelIDsForTest(models.Data), "jev-latest")
	require.Contains(t, modelIDsForTest(models.Data), "claude-opus-4-6")

	c, rec = newContext("/models?client_version=0.147.0")
	h.CodexModels(c)
	require.Equal(t, http.StatusOK, rec.Code)
	var manifest codexModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &manifest))
	slugs := codexModelSlugsForTest(manifest.Models)
	require.Contains(t, slugs, "claude-opus-4-6")
	require.NotContains(t, slugs, "jev-latest")
}

func TestDefaultModelIDsForPlatform_CompositeFallbackExcludesTypeSafe(t *testing.T) {
	require.NotContains(t, defaultModelIDsForPlatform(service.PlatformComposite), "jev-latest")
	require.NotContains(t, defaultCodexModelIDsForPlatform(service.PlatformComposite), "jev-latest")
}

func TestGatewayModels_CompositeExactRoutesRespectTypeSafeIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 7784
	routes := &gatewayModelsRouteRepoStub{routes: []service.CompositeModelRoute{
		{PublicModel: "systemone-any-alias", TargetPlatform: service.PlatformTypeSafe, MatchType: service.CompositeRouteMatchExact, Endpoint: service.CompositeRouteEndpointAny, Enabled: true},
		{PublicModel: "systemone-responses-alias", TargetPlatform: service.PlatformTypeSafe, MatchType: service.CompositeRouteMatchExact, Endpoint: service.CompositeRouteEndpointResponses, Enabled: true},
		{PublicModel: "systemone-legacy-alias", TargetPlatform: service.PlatformTypeSafe, MatchType: service.CompositeRouteMatchExact, Enabled: true},
		{PublicModel: "jev-latest", TargetPlatform: service.PlatformTypeSafe, MatchType: service.CompositeRouteMatchExact, Enabled: true},
		{PublicModel: "responses-alias", TargetPlatform: service.PlatformOpenAI, MatchType: service.CompositeRouteMatchExact, Endpoint: service.CompositeRouteEndpointResponses, Enabled: true},
		{PublicModel: "image-alias", TargetPlatform: service.PlatformOpenAI, MatchType: service.CompositeRouteMatchExact, Endpoint: service.CompositeRouteEndpointImages, Enabled: true},
	}}
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{}, routes)
	wantModels := make([]string, 0, len(routes.routes))
	for _, route := range routes.routes {
		wantModels = append(wantModels, route.PublicModel)
	}
	for _, allowlisted := range []bool{false, true} {
		name := "unrestricted"
		if allowlisted {
			name = "allowlisted"
		}
		t.Run(name, func(t *testing.T) {
			group := &service.Group{ID: groupID, Platform: service.PlatformComposite}
			if allowlisted {
				group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{
					"systemone-any-alias", "systemone-responses-alias", "systemone-legacy-alias", "jev-latest", "responses-alias", "image-alias",
				}}
			}
			for _, codex := range []bool{false, true} {
				rec := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(rec)
				ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
				ctx.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: group})
				if codex {
					h.CodexModels(ctx)
					var manifest codexModelsResponseForTest
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &manifest))
					require.Equal(t, []string{"responses-alias"}, codexModelSlugsForTest(manifest.Models))
				} else {
					h.Models(ctx)
					var models gatewayModelsResponseForTest
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &models))
					require.ElementsMatch(t, wantModels, modelIDsForTest(models.Data))
				}
				require.Equal(t, http.StatusOK, rec.Code)
			}
		})
	}
}

func TestGatewayModels_CompositeCodexFiltersEffectiveResponsesRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 7793
	defaultModel := defaultModelIDsForPlatform(service.PlatformOpenAI)[0]
	for _, scenario := range []struct {
		name     string
		model    string
		accounts []service.Account
		routes   []service.CompositeModelRoute
	}{
		{
			name:  "colliding exact routes",
			model: "shared-alias",
			routes: []service.CompositeModelRoute{
				{PublicModel: "shared-alias", TargetPlatform: service.PlatformOpenAI, MatchType: service.CompositeRouteMatchExact, Endpoint: service.CompositeRouteEndpointAny, Priority: 1, Enabled: true},
				{PublicModel: "shared-alias", TargetPlatform: service.PlatformTypeSafe, MatchType: service.CompositeRouteMatchExact, Endpoint: service.CompositeRouteEndpointResponses, Priority: 100, Enabled: true},
			},
		},
		{
			name:  "account alias exact override",
			model: "account-alias",
			accounts: []service.Account{{ID: 1, Platform: service.PlatformOpenAI, Credentials: map[string]any{
				"model_mapping": map[string]any{"account-alias": "gpt-5"},
			}}},
			routes: []service.CompositeModelRoute{
				{PublicModel: "account-alias", TargetPlatform: service.PlatformTypeSafe, MatchType: service.CompositeRouteMatchExact, Endpoint: service.CompositeRouteEndpointResponses, Enabled: true},
			},
		},
		{
			name:  "account alias prefix override",
			model: "account-alias",
			accounts: []service.Account{{ID: 1, Platform: service.PlatformOpenAI, Credentials: map[string]any{
				"model_mapping": map[string]any{"account-alias": "gpt-5"},
			}}},
			routes: []service.CompositeModelRoute{
				{PublicModel: "account-", TargetPlatform: service.PlatformTypeSafe, MatchType: service.CompositeRouteMatchPrefix, Endpoint: service.CompositeRouteEndpointResponses, Enabled: true},
			},
		},
		{
			name:     "account default override",
			model:    defaultModel,
			accounts: []service.Account{{ID: 1, Platform: service.PlatformOpenAI}},
			routes: []service.CompositeModelRoute{
				{PublicModel: defaultModel, TargetPlatform: service.PlatformTypeSafe, MatchType: service.CompositeRouteMatchExact, Endpoint: service.CompositeRouteEndpointResponses, Enabled: true},
			},
		},
		{
			name:  "fallback default override",
			model: defaultModel,
			routes: []service.CompositeModelRoute{
				{PublicModel: defaultModel, TargetPlatform: service.PlatformTypeSafe, MatchType: service.CompositeRouteMatchPrefix, Endpoint: service.CompositeRouteEndpointResponses, Enabled: true},
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			for _, allowlisted := range []bool{false, true} {
				name := "unrestricted"
				if allowlisted {
					name = "allowlisted"
				}
				t.Run(name, func(t *testing.T) {
					routes := append([]service.CompositeModelRoute(nil), scenario.routes...)
					if scenario.name != "fallback default override" {
						routes = append(routes, service.CompositeModelRoute{
							PublicModel: "responses-control", TargetPlatform: service.PlatformOpenAI, MatchType: service.CompositeRouteMatchExact, Endpoint: service.CompositeRouteEndpointResponses, Enabled: true,
						})
					}
					h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
						byGroup: map[int64][]service.Account{groupID: scenario.accounts},
					}, &gatewayModelsRouteRepoStub{routes: routes})
					group := &service.Group{ID: groupID, Platform: service.PlatformComposite}
					if allowlisted {
						group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{scenario.model, "responses-control"}}
					}
					for _, codex := range []bool{false, true} {
						recorder := httptest.NewRecorder()
						ctx, _ := gin.CreateTestContext(recorder)
						ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
						ctx.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: group})
						if codex {
							h.CodexModels(ctx)
							var manifest codexModelsResponseForTest
							require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &manifest))
							slugs := codexModelSlugsForTest(manifest.Models)
							require.NotContains(t, slugs, scenario.model)
							if scenario.name != "fallback default override" {
								require.Contains(t, slugs, "responses-control")
							} else if allowlisted {
								require.Empty(t, slugs)
							}
						} else {
							h.Models(ctx)
							var models gatewayModelsResponseForTest
							require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &models))
							if scenario.name == "fallback default override" && allowlisted {
								require.Empty(t, modelIDsForTest(models.Data))
							} else {
								require.Contains(t, modelIDsForTest(models.Data), scenario.model)
							}
						}
						require.Equal(t, http.StatusOK, recorder.Code)
					}
				})
			}
		})
	}
}

func TestGatewayModels_CompositeCodexRouteLookupErrorKeepsFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 7793
	model := defaultCodexModelIDsForPlatform(service.PlatformComposite)[0]
	for _, allowlisted := range []bool{false, true} {
		name := "unrestricted"
		if allowlisted {
			name = "allowlisted"
		}
		t.Run(name, func(t *testing.T) {
			h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{}, &gatewayModelsRouteRepoStub{err: errors.New("unavailable")})
			group := &service.Group{ID: groupID, Platform: service.PlatformComposite}
			if allowlisted {
				group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{model}}
			}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			ctx.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: group})
			h.CodexModels(ctx)
			require.Equal(t, http.StatusOK, recorder.Code)
			var manifest codexModelsResponseForTest
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &manifest))
			if allowlisted {
				require.Empty(t, codexModelSlugsForTest(manifest.Models))
			} else {
				require.Contains(t, codexModelSlugsForTest(manifest.Models), model)
			}
		})
	}
}

// ============================================================
// composite 下挂载 WB 账号的模型探测泄露治理（compositeAvailableModels WB 收集腿）：
// 纯 WB 组只能看到 21 白名单键（domain.WbModelWhitelist），严禁经 :1181/:1289 回退
// 带出 precedence 平台的 Claude/OpenAI 全量默认；混合组为既有注入行为 ∪ 21 纯增量；
// allowlist 开启时空候选 = 严格 []（KDR-4 终态，无地板回退）。
// ============================================================

// newCompositeModelsRequestForTest 构造一次 GET 模型列表请求（任意 path），context 挂
// composite 分组的 APIKey；处理器调用与断言由调用方执行。
func newCompositeModelsRequestForTest(group *service.Group, path string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, path, nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: group})
	return c, rec
}

// requireNoClaudeForTest 探测泄露守卫：任何 claude-* 前缀（:1181 地板形状）都算泄露。
func requireNoClaudeForTest(t *testing.T, ids []string) {
	t.Helper()
	for _, id := range ids {
		require.False(t, strings.HasPrefix(id, "claude-"), "unexpected claude model leaked: %s", id)
	}
}

// AC-27-01 纯 WB composite：/v1/models = 21 白名单，无任何 claude-* 与默认泄漏。
func TestGatewayModels_CompositeWBOnlyListsWhitelistNoClaude(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 6001
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {{ID: 1, Platform: service.PlatformWB}}},
	})

	c, rec := newCompositeModelsRequestForTest(&service.Group{ID: groupID, Platform: service.PlatformComposite}, "/v1/models")
	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	ids := modelIDsForTest(got.Data)
	// 集合相等（顺序不构成契约）：恰好 21 白名单键，不多不少。
	require.ElementsMatch(t, domain.WbModelWhitelist, ids)
	requireNoClaudeForTest(t, ids)
}

// AC-27-01 混合组无回归：precedence 行为不变（openai 无映射 → 逐平台默认注入，
// allEmpty 成立）+ wb 纯增量（21 白名单并集，去重）。
func TestGatewayModels_CompositeWBMixedWithPrecedencePlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 6002
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {
				{ID: 1, Platform: service.PlatformOpenAI},
				{ID: 2, Platform: service.PlatformWB},
			},
		},
	})

	c, rec := newCompositeModelsRequestForTest(&service.Group{ID: groupID, Platform: service.PlatformComposite}, "/v1/models")
	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	ids := modelIDsForTest(got.Data)
	// 精确并集断言：openai 默认 ∪ 21 白名单，无额外模型。
	want := unionModelIDsForTest(defaultModelIDsForPlatform(service.PlatformOpenAI), domain.WbModelWhitelist)
	require.ElementsMatch(t, want, ids)
	require.NotContains(t, ids, "claude-sonnet-4-5")
}

// AC-27-01 白名单规范化：命中键保留；allowlist 中的非 WB 默认键（claude-*）不泄漏。
func TestGatewayModels_CompositeWBAllowlistFiltersWithinWhitelist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 6003
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {{ID: 1, Platform: service.PlatformWB}}},
	})

	c, rec := newCompositeModelsRequestForTest(&service.Group{
		ID:       groupID,
		Platform: service.PlatformComposite,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"gpt-5.5", "claude-sonnet-4-5"},
		},
	}, "/v1/models")
	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	// 候选源 = 21 白名单键：gpt-5.5 命中保留；claude-sonnet-4-5 不是白名单键 → 恰好一个键。
	require.ElementsMatch(t, []string{"gpt-5.5"}, modelIDsForTest(got.Data))
}

// AC-27-01 白名单不命中：空列表 []，不回退任何默认模型（#24 空交集契约）。
func TestGatewayModels_CompositeWBAllowlistEmptyIntersection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 6004
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {{ID: 1, Platform: service.PlatformWB}}},
	})

	c, rec := newCompositeModelsRequestForTest(&service.Group{
		ID:       groupID,
		Platform: service.PlatformComposite,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"not-a-real-model"},
		},
	}, "/v1/models")
	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Empty(t, modelIDsForTest(got.Data))
}

// AC-27-01 显式 mapping 求交：白名单合法键保留、外键剔除（KDR-8 出口求交）。
func TestGatewayModels_CompositeWBExplicitMappingKeepsLegalKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 6005
	account := service.Account{
		ID:       1,
		Platform: service.PlatformWB,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"gpt-5.5":           "gpt-5.5",
				"claude-sonnet-4-5": "claude-sonnet-4-5",
			},
		},
	}
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {account}},
	})

	c, rec := newCompositeModelsRequestForTest(&service.Group{ID: groupID, Platform: service.PlatformComposite}, "/v1/models")
	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	ids := modelIDsForTest(got.Data)
	// 混合键 mapping：gpt-5.5 ∈ 白名单保留；claude-sonnet-4-5 ∉ 白名单剔除。
	require.ElementsMatch(t, []string{"gpt-5.5"}, ids)
	require.NotContains(t, ids, "claude-sonnet-4-5")
}

// AC-27-01 显式 mapping 全外键 + allowlist 关闭：保守注入 21 白名单（KDR-4），
// composite 默认 100+（如 claude-opus-4-6）不得出现（防 :1181 地板）。
func TestGatewayModels_CompositeWBExplicitMappingAllIllegalFallsBackToWhitelist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 6006
	account := service.Account{
		ID:       1,
		Platform: service.PlatformWB,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"claude-sonnet-4-5": "claude-sonnet-4-5",
			},
		},
	}
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {account}},
	})

	c, rec := newCompositeModelsRequestForTest(&service.Group{ID: groupID, Platform: service.PlatformComposite}, "/v1/models")
	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	ids := modelIDsForTest(got.Data)
	// wbRaw 非空（外键）但求交为空 → 键控保守注入：恰好 21 白名单，无默认泄漏。
	require.ElementsMatch(t, domain.WbModelWhitelist, ids)
	require.NotContains(t, ids, "claude-sonnet-4-5")
	requireNoClaudeForTest(t, ids)
}

// AC-27-01 显式 mapping 全外键 + allowlist 开启：/v1/models 严格 []（KDR-4 终态，
// 无 :1181/:1174 地板回退）——空候选是终态而非“空 → 默认”。
func TestGatewayModels_CompositeWBAllExternalMappingAllowlistReturnsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 6007
	account := service.Account{
		ID:       1,
		Platform: service.PlatformWB,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"claude-sonnet-4-5": "claude-sonnet-4-5",
			},
		},
	}
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {account}},
	})

	c, rec := newCompositeModelsRequestForTest(&service.Group{
		ID:       groupID,
		Platform: service.PlatformComposite,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"gpt-5.5"},
		},
	}, "/v1/models")
	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	// allowlist 开启时求交空 = 严格 []：保守注入条件（!allowlistEnabled）不成立，
	// FilterForListing(∅) → []，:1181 复合分支地板不触发。
	require.Empty(t, modelIDsForTest(got.Data))
}

// AC-27-01 显式 mapping 全外键 + allowlist 开启：Codex manifest 严格 []（KDR-4 终态，
// 无 :1289 默认回退）——Codex 消费端与 /v1/models 同收敛。
func TestCodexModels_CompositeWBAllExternalMappingAllowlistReturnsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 6008
	account := service.Account{
		ID:       1,
		Platform: service.PlatformWB,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"claude-sonnet-4-5": "claude-sonnet-4-5",
			},
		},
	}
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {account}},
	})

	c, rec := newCompositeModelsRequestForTest(&service.Group{
		ID:       groupID,
		Platform: service.PlatformComposite,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"gpt-5.5"},
		},
	}, "/models?client_version=0.147.0")
	h.CodexModels(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got codexModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	// codexModelIDsForGroup composite 分支：allowlist 开启时空候选不落 defaultCodex 兜底
	// （len==0 && !allowlistEnabled 不成立）→ manifest 空。
	require.Empty(t, codexModelSlugsForTest(got.Models))
}

// AC-27-02 Codex 消费端同收敛：纯 WB composite manifest = 21 白名单，无 Claude 默认。
func TestCodexModels_CompositeWBNoDefaultLeak(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 6009
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {{ID: 1, Platform: service.PlatformWB}}},
	})

	c, rec := newCompositeModelsRequestForTest(&service.Group{ID: groupID, Platform: service.PlatformComposite}, "/models?client_version=0.147.0")
	h.CodexModels(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got codexModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	slugs := codexModelSlugsForTest(got.Models)
	// 候选 = 21 白名单（非空）→ 不落 defaultCodex 兜底：manifest 恰好 21 白名单键。
	require.ElementsMatch(t, domain.WbModelWhitelist, slugs)
	require.NotContains(t, slugs, "claude-sonnet-4-5")
	require.NotContains(t, slugs, "claude-opus-4-6")
}

// AC-27-01 出口重写防线：allowlist 条目 gpt-5.6-sol-thinking 经 -thinking 归一化命中
// 白名单键 gpt-5.6-sol（group_model_allowlist.go 归一匹配后输出条目原文），条目原文
// ∉ 21 白名单 → 不得带出（与专用 WB 分支出口再求交 :1193-1198 同语义，KDR-8）。
func TestGatewayModels_CompositeWBAllowlistThinkingVariantReturnsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 6010
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {{ID: 1, Platform: service.PlatformWB}}},
	})

	c, rec := newCompositeModelsRequestForTest(&service.Group{
		ID:       groupID,
		Platform: service.PlatformComposite,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"gpt-5.6-sol-thinking"},
		},
	}, "/v1/models")
	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	// 变体条目只能经归一化命中：白名单键 gpt-5.6-sol 在 WB 腿被预演剔除 → 严格 []。
	require.Empty(t, modelIDsForTest(got.Data))
	require.NotContains(t, modelIDsForTest(got.Data), "gpt-5.6-sol-thinking")
}

// AC-27-01 出口重写防线：allowlist 条目 GPT-5.6-SOL 经 EqualFold 大小写不敏感命中
// 白名单键 gpt-5.6-sol，条目原文 ∉ 21 白名单 → 不得带出（KDR-8 精确匹配）。
func TestGatewayModels_CompositeWBAllowlistCaseVariantReturnsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 6011
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {{ID: 1, Platform: service.PlatformWB}}},
	})

	c, rec := newCompositeModelsRequestForTest(&service.Group{
		ID:       groupID,
		Platform: service.PlatformComposite,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"GPT-5.6-SOL"},
		},
	}, "/v1/models")
	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	// 大写变体条目经 EqualFold 命中白名单键 → 预演剔除 → 严格 []（非 1 项大写键）。
	require.Empty(t, modelIDsForTest(got.Data))
	require.NotContains(t, modelIDsForTest(got.Data), "GPT-5.6-SOL")
}

// AC-27-02 Codex 消费端同收敛：-thinking 变体条目不得带出白名单外原文键。
func TestCodexModels_CompositeWBAllowlistThinkingVariantReturnsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 6012
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {{ID: 1, Platform: service.PlatformWB}}},
	})

	c, rec := newCompositeModelsRequestForTest(&service.Group{
		ID:       groupID,
		Platform: service.PlatformComposite,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"gpt-5.6-sol-thinking"},
		},
	}, "/models?client_version=0.147.0")
	h.CodexModels(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got codexModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Empty(t, codexModelSlugsForTest(got.Models))
}

// AC-27-02 Codex 消费端同收敛：大小写变体条目不得带出白名单外原文键。
func TestCodexModels_CompositeWBAllowlistCaseVariantReturnsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 6013
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {{ID: 1, Platform: service.PlatformWB}}},
	})

	c, rec := newCompositeModelsRequestForTest(&service.Group{
		ID:       groupID,
		Platform: service.PlatformComposite,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"GPT-5.6-SOL"},
		},
	}, "/models?client_version=0.147.0")
	h.CodexModels(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got codexModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Empty(t, codexModelSlugsForTest(got.Models))
}

// AC-27-01 合法原键与变体条目并存：allowlist 同时含 gpt-5.6-sol 与
// gpt-5.6-sol-thinking 时，合法键自身必须保留（条目原文命中候选键），仅
// 变体条目原文被白名单交集剔除——不得因「预演输出 >1 项」整体淘汰合法键。
func TestGatewayModels_CompositeWBAllowlistLegalKeyWithVariantKeepsLegalKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 6014
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {{ID: 1, Platform: service.PlatformWB}}},
	})

	c, rec := newCompositeModelsRequestForTest(&service.Group{
		ID:       groupID,
		Platform: service.PlatformComposite,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"gpt-5.6-sol", "gpt-5.6-sol-thinking"},
		},
	}, "/v1/models")
	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	// 合法键保留、变体原文键剔除：恰好 gpt-5.6-sol 一项。
	require.ElementsMatch(t, []string{"gpt-5.6-sol"}, modelIDsForTest(got.Data))
	require.NotContains(t, modelIDsForTest(got.Data), "gpt-5.6-sol-thinking")
}

// AC-27-02 Codex 消费端同收敛：合法原键保留、变体条目仅以白名单键形式出现。
func TestCodexModels_CompositeWBAllowlistLegalKeyWithVariantKeepsLegalKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 6015
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {{ID: 1, Platform: service.PlatformWB}}},
	})

	c, rec := newCompositeModelsRequestForTest(&service.Group{
		ID:       groupID,
		Platform: service.PlatformComposite,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"gpt-5.6-sol", "gpt-5.6-sol-thinking"},
		},
	}, "/models?client_version=0.147.0")
	h.CodexModels(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got codexModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.ElementsMatch(t, []string{"gpt-5.6-sol"}, codexModelSlugsForTest(got.Models))
	require.NotContains(t, codexModelSlugsForTest(got.Models), "gpt-5.6-sol-thinking")
}

// unionModelIDsForTest 按出现顺序做并集去重（测试期望构造用，集合语义）。
func unionModelIDsForTest(lists ...[]string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, list := range lists {
		for _, id := range list {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// TestQoderGatewayModelsAndCompositeLists14Whitelist 覆盖设计 §6 用例 15 ②：qoder 与 composite 分组 /v1/models 精确返回 14 个可读模型。
func TestQoderGatewayModelsAndCompositeLists14Whitelist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const groupID int64 = 7901
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {{ID: 1, Platform: service.PlatformQoder}}},
	})
	for _, plat := range []string{service.PlatformQoder, service.PlatformComposite} {
		c, rec := newCompositeModelsRequestForTest(&service.Group{ID: groupID, Platform: plat}, "/v1/models")
		h.Models(c)
		require.Equal(t, http.StatusOK, rec.Code)
		var got gatewayModelsResponseForTest
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.ElementsMatch(t, domain.QoderModelWhitelist, modelIDsForTest(got.Data))
	}
	c, rec := newCompositeModelsRequestForTest(&service.Group{ID: groupID, Platform: service.PlatformComposite, ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"sonus-fable5.1", "sonus-fable5.1-thinking"}}}, "/models?client_version=0.147.0")
	h.CodexModels(c)
	require.Equal(t, http.StatusOK, rec.Code)
	var codexGot codexModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &codexGot))
	require.ElementsMatch(t, []string{"sonus-fable5.1"}, codexModelSlugsForTest(codexGot.Models))
}

// TestQoderHandlerChatCompletionsAutoAllowedAndUnknownModel400 覆盖设计 §6 用例 15 ②：auto 放行与未知模型返回 HTTP 400。
func TestQoderHandlerChatCompletionsAutoAllowedAndUnknownModel400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(7902)
	apiKey := &service.APIKey{ID: 7903, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformQoder}, User: &service.User{ID: 1, Status: service.StatusActive}}
	for _, m := range []string{"auto", "fast", "performance", "efficient", "smodel", "SONUS-FABLE5.1"} {
		c1, _ := gin.CreateTestContext(httptest.NewRecorder())
		c1.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		require.True(t, openAICompatibleTextTargetAllowed(c1, apiKey, m))
		require.False(t, c1.Writer.Written(), "model=%s", m)
	}
	for _, tc := range []struct {
		model string
		code  int
		msg   string
	}{
		{"auto", http.StatusBadGateway, "Upstream request failed"},
		{"gpt-99-unknown", http.StatusBadRequest, "unsupported model: gpt-99-unknown"},
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+tc.model+`","messages":[{"role":"user","content":"hi"}]}`))
		c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 1, Concurrency: 1})
		newServiceTierHandlerTest(t).ChatCompletions(c)
		require.Equal(t, tc.code, rec.Code, "model=%s", tc.model)
		require.Contains(t, rec.Body.String(), tc.msg, "model=%s", tc.model)
	}
}
