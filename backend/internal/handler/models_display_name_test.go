package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const displayNameTestGroupID int64 = 9101

// newDisplayNameModelsRequestForTest 构造 /v1/models 请求。
// ua 为空 = 非 Claude Code 客户端；headers 用来覆盖或补充请求头。
func newDisplayNameModelsRequestForTest(
	group *service.Group,
	ua string,
	headers map[string]string,
) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if ua != "" {
		c.Request.Header.Set("User-Agent", ua)
	}
	for key, value := range headers {
		c.Request.Header.Set(key, value)
	}
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{GroupID: &group.ID, Group: group})
	return c, rec
}

// displayNameItemsByIDForTest 把响应条目按 id 建索引，避免依赖列表顺序。
func displayNameItemsByIDForTest(t *testing.T, rec *httptest.ResponseRecorder) map[string]gatewayModelItemForTest {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	items := make(map[string]gatewayModelItemForTest, len(got.Data))
	for _, item := range got.Data {
		items[item.ID] = item
	}
	return items
}

// displayNameDefaultValueForTest 返回未配置显示名时的原推导值：
// 改动前出口把 modelID 同时传给 id 与 displayName。
func displayNameDefaultValueForTest(modelID string, isCCClient bool) string {
	_, displayName, _ := service.FormatModelForDiscoveryWithClient(modelID, modelID, "claude", isCCClient)
	return displayName
}

// displayNameExpectedIDForTest 返回某模型 id 在列表里的库存 id（壳 id / 归一后的形式）。
func displayNameExpectedIDForTest(modelID string, isCCClient bool) string {
	id, _, _ := service.FormatModelForDiscoveryWithClient(modelID, modelID, "claude", isCCClient)
	return id
}

// displayNameClaudeDefaultListBodyPreChange 按改动前的 Claude 默认列表循环重建响应体，
// 用于证明「未配置显示名时输出逐字相同」（AC-5）。
func displayNameClaudeDefaultListBodyPreChange(t *testing.T, isCCClient bool) string {
	t.Helper()
	defaults := slices.Clone(claude.DefaultModels)
	for i := range defaults {
		id, displayName, contextWindow := service.FormatModelForDiscoveryWithClient(defaults[i].ID, defaults[i].DisplayName, "claude", isCCClient)
		defaults[i].ID = id
		defaults[i].DisplayName = displayName
		defaults[i].ContextWindow = contextWindow
	}
	body, err := json.Marshal(gin.H{"object": "list", "data": defaults})
	require.NoError(t, err)
	return string(body)
}

// AC-1 / AC-2 / KDR-6：配置了显示名的模型用配置值，同分组未配置的模型保持原推导值，
// 所有条目的 id 与未配置时逐条相同；显示名指向不在可用列表的模型时不产生条目（§6 边界）。
func TestGatewayModels_DisplayNameConfiguredAndUnconfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// claude-mythos-ds 是 CCD 壳 id：出口 id 会被归一成 ds，而显示名的查找键是映射原文。
	const configuredKey = "claude-mythos-ds"
	const configuredDisplayName = "DeepSeek · V4.1 Flash"
	const siblingModel = "claude-haiku-4-5-20251001"

	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			displayNameTestGroupID: {{
				ID:       1,
				Platform: service.PlatformAnthropic,
				Credentials: map[string]any{"model_mapping": map[string]any{
					configuredKey: configuredKey,
					siblingModel:  siblingModel,
				}},
			}},
		},
	})

	baselineGroup := &service.Group{ID: displayNameTestGroupID, Platform: service.PlatformAnthropic}
	baselineRequest, baselineRec := newDisplayNameModelsRequestForTest(baselineGroup, "", nil)
	h.Models(baselineRequest)
	baseline := displayNameItemsByIDForTest(t, baselineRec)
	require.Len(t, baseline, 2)

	group := &service.Group{
		ID:       displayNameTestGroupID,
		Platform: service.PlatformAnthropic,
		ModelAllowlist: service.GroupModelAllowlist{
			DisplayNames: map[string]string{
				configuredKey:  configuredDisplayName,
				"absent-model": "X",
			},
		},
	}
	c, rec := newDisplayNameModelsRequestForTest(group, "", nil)
	h.Models(c)
	got := displayNameItemsByIDForTest(t, rec)

	require.Len(t, got, len(baseline), "显示名不得增删条目")
	for id, before := range baseline {
		after, stillThere := got[id]
		require.Truef(t, stillThere, "id %q 在配置显示名后消失了", id)
		require.Equal(t, before.ID, after.ID, "id 一律不动")
		if id == displayNameExpectedIDForTest(configuredKey, false) || id == configuredKey {
			require.Equal(t, configuredDisplayName, after.DisplayName, "AC-1: 命中配置用配置值")
			continue
		}
		require.Equalf(t, before.DisplayName, after.DisplayName, "AC-2: 未配置的模型 %q 显示名逐字相同", id)
	}
	require.NotContains(t, got, "absent-model", "显示名不能把不在可用列表的模型加进来")
}

// AC-5：白名单未启用且 DisplayNames 为 nil 的存量分组，输出与改动前逐字相同。
func TestGatewayModels_DisplayNameUnconfiguredOutputUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{},
	})
	group := &service.Group{ID: displayNameTestGroupID, Platform: service.PlatformAnthropic}

	for _, tc := range []struct {
		name string
		ua   string
		isCC bool
	}{
		{name: "non-cc client", ua: "", isCC: false},
		{name: "claude code client", ua: "claude-cli/2.1.112 (external, cli)", isCC: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newDisplayNameModelsRequestForTest(group, tc.ua, nil)
			h.Models(c)

			require.Equal(t, http.StatusOK, rec.Code)
			require.Nil(t, group.ModelAllowlist.DisplayNames, "存量分组读出来必须是 nil")
			require.Equal(t, displayNameClaudeDefaultListBodyPreChange(t, tc.isCC), rec.Body.String())
		})
	}
}

// 二级出口：Gemini 平台默认列表（白名单未启用、本平台无可用模型）不经过 writeModelsList，
// 配置必须在循环内生效。
func TestGatewayModels_DisplayNameGeminiDefaultList(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const configuredModel = "gemini-3.8-flash"
	const configuredDisplayName = "Gemini 3.8 Flash 自研"

	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{},
	})
	group := &service.Group{
		ID:       displayNameTestGroupID,
		Platform: service.PlatformGemini,
		ModelAllowlist: service.GroupModelAllowlist{
			DisplayNames: map[string]string{configuredModel: configuredDisplayName},
		},
	}

	c, rec := newDisplayNameModelsRequestForTest(group, "", nil)
	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	var item gatewayModelItemForTest
	found := false
	for _, entry := range got.Data {
		if entry.ID == configuredModel {
			item, found = entry, true
			break
		}
	}
	require.Truef(t, found, "%s 不在 Gemini 默认列表里：%v", configuredModel, modelIDsForTest(got.Data))
	require.Equal(t, configuredModel, item.ID)
	require.Equal(t, configuredDisplayName, item.DisplayName)
}

// 一级出口 writeAllowlistedModelsList：白名单启用 + composite 分组，
// 白名单内配了显示名的条目用配置值，同组未配置的条目用原推导值。
func TestGatewayModels_DisplayNameCompositeAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const configuredModel = "gpt-5.4"
	const configuredDisplayName = "甲"
	const otherModel = "gpt-5.2"

	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			displayNameTestGroupID: {{
				ID:       1,
				Platform: service.PlatformOpenAI,
				Credentials: map[string]any{"model_mapping": map[string]any{
					configuredModel: configuredModel,
					otherModel:      otherModel,
				}},
			}},
		},
	})
	group := &service.Group{
		ID:       displayNameTestGroupID,
		Platform: service.PlatformComposite,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled:      true,
			Models:       []string{configuredModel, otherModel},
			DisplayNames: map[string]string{configuredModel: configuredDisplayName},
		},
	}

	c, rec := newDisplayNameModelsRequestForTest(group, "", nil)
	h.Models(c)
	got := displayNameItemsByIDForTest(t, rec)

	configured, ok := got[configuredModel]
	require.True(t, ok, "白名单内的条目不出现：%v", got)
	require.Equal(t, configuredDisplayName, configured.DisplayName)

	other, ok := got[otherModel]
	require.True(t, ok)
	require.Equal(t, displayNameDefaultValueForTest(otherModel, false), other.DisplayName)
}

// Claude Code 客户端 + 1M 模型：id 照旧套壳并带 [1m] 后缀，显示名在配置值后照旧追加 " 1M"（KDR-5）。
func TestGatewayModels_DisplayNameClaudeCodeShellAnd1M(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const modelID = "gpt-5.4"
	const configuredDisplayName = "DeepSeek · V4.1 Flash"

	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			displayNameTestGroupID: {{
				ID:       1,
				Platform: service.PlatformOpenAI,
				Credentials: map[string]any{"model_mapping": map[string]any{
					modelID: modelID,
				}},
			}},
		},
	})
	// composite 的 Claude 形状出口（exitPlatform = "claude"）才会套壳并标记 1M。
	baselineGroup := &service.Group{ID: displayNameTestGroupID, Platform: service.PlatformComposite}
	baselineRequest, baselineRec := newDisplayNameModelsRequestForTest(baselineGroup, "", map[string]string{"x-app": "cli"})
	h.Models(baselineRequest)
	baseline := displayNameItemsByIDForTest(t, baselineRec)
	require.Len(t, baseline, 1)

	group := &service.Group{
		ID:       displayNameTestGroupID,
		Platform: service.PlatformComposite,
		ModelAllowlist: service.GroupModelAllowlist{
			DisplayNames: map[string]string{modelID: configuredDisplayName},
		},
	}
	c, rec := newDisplayNameModelsRequestForTest(group, "", map[string]string{"x-app": "cli"})
	h.Models(c)
	got := displayNameItemsByIDForTest(t, rec)
	require.Len(t, got, 1)

	for id, before := range baseline {
		after, ok := got[id]
		require.Truef(t, ok, "id %q 在配置显示名后变了：现在 %v", id, got)
		require.Equal(t, before.ID, after.ID, "id 一律不动")
		require.NotEqual(t, modelID, after.ID, "CC 客户端的 id 应为壳 id")
		require.Contains(t, after.ID, "[1m]", "1M 模型 id 照旧带后缀")
		require.Equal(t, 1000000, after.ContextWindow)
		require.Equal(t, configuredDisplayName+" 1M", after.DisplayName, "配置值后照旧追加 1M")
	}
}
