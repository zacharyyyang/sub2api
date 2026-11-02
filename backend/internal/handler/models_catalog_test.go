package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const catalogTestGroupID int64 = 9201

// newCatalogModelsRequest 构造 /v1/models 请求：ua 为空 = 非 Claude Code 客户端。
func newCatalogModelsRequest(group *service.Group, ua string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if ua != "" {
		c.Request.Header.Set("User-Agent", ua)
	}
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{GroupID: &group.ID, Group: group})
	return c, rec
}

// catalogItemsForTest 把响应条目按 id 建索引，避免依赖列表顺序。
func catalogItemsForTest(t *testing.T, rec *httptest.ResponseRecorder) map[string]gatewayModelItemForTest {
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

// catalogWrappedIDForTest 返回某真实模型在改动前的自动包装形态（壳 id + [1m]，按既有规则算）。
func catalogWrappedIDForTest(realModel string, isCC bool) string {
	id, _, _ := service.FormatModelForDiscoveryWithClient(realModel, realModel, "claude", isCC)
	return id
}

// newCatalogHandlerWithDeepSeek 构造「一个模型的 Anthropic 分组」：账号映射提供
// deepseek-v4，出口走 writeModelsList。
func newCatalogHandlerWithDeepSeek() *GatewayHandler {
	return newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			catalogTestGroupID: {{
				ID:       1,
				Platform: service.PlatformAnthropic,
				Credentials: map[string]any{"model_mapping": map[string]any{
					"deepseek-v4": "deepseek-v4",
				}},
			}},
		},
	})
}

func catalogDeepSeekEntry() service.GroupModelCatalogEntry {
	return service.GroupModelCatalogEntry{
		ShellID:     "claude-mythos-7",
		DisplayName: "DeepSeek V4",
		RealModel:   "deepseek-v4",
		Supports1M:  true,
	}
}

// AC-8：目录行按目录值发布——壳 id（授予 1M 时带 [1m]）、显示名 + " 1M"、context_window，
// 且同一真实模型不再出现自动包装条目。
func TestGatewayModels_CatalogRowPublishedForClaudeCodeClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newCatalogHandlerWithDeepSeek()

	baselineRequest, baselineRec := newCatalogModelsRequest(&service.Group{ID: catalogTestGroupID, Platform: service.PlatformAnthropic}, "claude-cli/2.1.112")
	h.Models(baselineRequest)
	baseline := catalogItemsForTest(t, baselineRec)
	wrappedID := catalogWrappedIDForTest("deepseek-v4", true)
	require.Contains(t, baseline, wrappedID, "未配目录时沿用既有自动包装")

	group := &service.Group{
		ID:             catalogTestGroupID,
		Platform:       service.PlatformAnthropic,
		ModelAllowlist: service.GroupModelAllowlist{Catalog: []service.GroupModelCatalogEntry{catalogDeepSeekEntry()}},
	}
	request, rec := newCatalogModelsRequest(group, "claude-cli/2.1.112")
	h.Models(request)
	got := catalogItemsForTest(t, rec)

	require.Len(t, got, 1, "目录行替换自动包装条目，不额外多出一条")
	item, ok := got["claude-mythos-7[1m]"]
	require.True(t, ok, "目录行 id 应为壳 id + [1m]：%v", got)
	require.Equal(t, "DeepSeek V4 1M", item.DisplayName)
	require.Equal(t, 1000000, item.ContextWindow)
	require.NotContains(t, got, wrappedID, "目录命中不再自动包装")
}

// 未授予 1M 的行：id 不带后缀、显示名不追加 1M、context_window 省略。
func TestGatewayModels_CatalogRowWithout1M(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newCatalogHandlerWithDeepSeek()

	entry := catalogDeepSeekEntry()
	entry.Supports1M = false
	group := &service.Group{
		ID:             catalogTestGroupID,
		Platform:       service.PlatformAnthropic,
		ModelAllowlist: service.GroupModelAllowlist{Catalog: []service.GroupModelCatalogEntry{entry}},
	}
	request, rec := newCatalogModelsRequest(group, "claude-cli/2.1.112")
	h.Models(request)

	item, ok := catalogItemsForTest(t, rec)["claude-mythos-7"]
	require.True(t, ok)
	require.Equal(t, "DeepSeek V4", item.DisplayName)
	require.Zero(t, item.ContextWindow)
}

// F7 出口：非 Claude Code 客户端只吃旧机制，目录不发布。
func TestGatewayModels_CatalogHiddenFromNonClaudeClients(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newCatalogHandlerWithDeepSeek()

	group := &service.Group{
		ID:             catalogTestGroupID,
		Platform:       service.PlatformAnthropic,
		ModelAllowlist: service.GroupModelAllowlist{Catalog: []service.GroupModelCatalogEntry{catalogDeepSeekEntry()}},
	}
	request, rec := newCatalogModelsRequest(group, "")
	h.Models(request)

	got := catalogItemsForTest(t, rec)
	require.Equal(t, []string{"deepseek-v4"}, catalogIDsOf(got), "非 CC 客户端看到的是裸真实模型 id")
}

// AC-11：旧显示名与目录并存——CC 显示目录名、非 CC 显示旧名；删掉目录行后 CC 回到
// 自动包装 + 旧名。两条机制互不吞并。
func TestGatewayModels_CatalogAndLegacyDisplayNameCoexist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newCatalogHandlerWithDeepSeek()

	group := &service.Group{
		ID:       catalogTestGroupID,
		Platform: service.PlatformAnthropic,
		ModelAllowlist: service.GroupModelAllowlist{
			DisplayNames: map[string]string{"deepseek-v4": "旧名"},
			Catalog:      []service.GroupModelCatalogEntry{catalogDeepSeekEntry()},
		},
	}

	ccRequest, ccRec := newCatalogModelsRequest(group, "claude-cli/2.1.112")
	h.Models(ccRequest)
	ccItem, ok := catalogItemsForTest(t, ccRec)["claude-mythos-7[1m]"]
	require.True(t, ok, "CC 客户端显示目录行")
	require.Equal(t, "DeepSeek V4 1M", ccItem.DisplayName)

	rawRequest, rawRec := newCatalogModelsRequest(group, "")
	h.Models(rawRequest)
	rawItem, ok := catalogItemsForTest(t, rawRec)["deepseek-v4"]
	require.True(t, ok, "非 CC 客户端仍是裸真实模型 id")
	require.Equal(t, "旧名", rawItem.DisplayName)

	// 删掉目录行：CC 回到自动包装 + 旧名。
	legacy := &service.Group{
		ID:             catalogTestGroupID,
		Platform:       service.PlatformAnthropic,
		ModelAllowlist: service.GroupModelAllowlist{DisplayNames: map[string]string{"deepseek-v4": "旧名"}},
	}
	legacyRequest, legacyRec := newCatalogModelsRequest(legacy, "claude-cli/2.1.112")
	h.Models(legacyRequest)
	legacyItems := catalogItemsForTest(t, legacyRec)
	legacyItem, ok := legacyItems[catalogWrappedIDForTest("deepseek-v4", true)]
	require.True(t, ok, "目录行删除后 CC 回到自动包装：%v", legacyItems)
	require.Equal(t, "旧名", legacyItem.DisplayName)
}

// AC-14（通用回归）：目录为空、旧配置也为空时，Claude 默认列表出口在两种客户端下
// 与改动前逐字相同。
func TestGatewayModels_EmptyCatalogOutputUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{}})
	group := &service.Group{ID: catalogTestGroupID, Platform: service.PlatformAnthropic}

	for _, tc := range []struct {
		name string
		ua   string
		isCC bool
	}{
		{name: "non-cc client", ua: "", isCC: false},
		{name: "claude code client", ua: "claude-cli/2.1.112 (external, cli)", isCC: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, rec := newCatalogModelsRequest(group, tc.ua)
			h.Models(request)
			require.Equal(t, http.StatusOK, rec.Code)
			require.Nil(t, group.ModelAllowlist.Catalog, "存量分组读出来必须是 nil")
			require.Equal(t, catalogClaudeDefaultListBodyBeforeChange(t, tc.isCC), rec.Body.String())
		})
	}
}

// 二级出口之一：Gemini 平台默认列表同样按目录发布（该循环不经 writeModelsList）。
func TestGatewayModels_CatalogRowInGeminiDefaultList(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{}})
	group := &service.Group{
		ID:       catalogTestGroupID,
		Platform: service.PlatformGemini,
		ModelAllowlist: service.GroupModelAllowlist{Catalog: []service.GroupModelCatalogEntry{{
			ShellID: "claude-mythos-7", DisplayName: "Gemini 3.8", RealModel: "gemini-3.8-flash",
		}}},
	}

	request, rec := newCatalogModelsRequest(group, "claude-cli/2.1.112")
	h.Models(request)
	got := catalogItemsForTest(t, rec)

	require.Contains(t, got, "claude-mythos-7", "目录行必须出现在 Gemini 默认列表出口")
	require.NotContains(t, got, catalogWrappedIDForTest("gemini-3.8-flash", true), "被目录覆盖的默认条目不再自动包装")
}

// 二级出口之二：Claude 默认列表同样按目录发布。
func TestGatewayModels_CatalogRowInClaudeDefaultList(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{}})
	group := &service.Group{
		ID:       catalogTestGroupID,
		Platform: service.PlatformAnthropic,
		ModelAllowlist: service.GroupModelAllowlist{Catalog: []service.GroupModelCatalogEntry{{
			ShellID: "claude-mythos-7", DisplayName: "Opus 代理", RealModel: "claude-opus-4-6", Supports1M: true,
		}}},
	}

	request, rec := newCatalogModelsRequest(group, "claude-cli/2.1.112")
	h.Models(request)
	got := catalogItemsForTest(t, rec)

	item, ok := got["claude-mythos-7[1m]"]
	require.True(t, ok, "目录行必须出现在 Claude 默认列表出口")
	require.Equal(t, "Opus 代理 1M", item.DisplayName)
	require.Contains(t, got, "claude-opus-4-7[1m]", "默认列表的其它条目照旧")
	require.NotContains(t, got, "claude-opus-4-6[1m]", "被目录覆盖的默认条目被替换")
}

// 非 CC 客户端 + 非空目录：Claude 默认列表输出与「完全未配置目录」逐字相同。
func TestGatewayModels_NonClaudeClientIgnoresCatalogInDefaultList(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{}})
	group := &service.Group{
		ID:       catalogTestGroupID,
		Platform: service.PlatformAnthropic,
		ModelAllowlist: service.GroupModelAllowlist{Catalog: []service.GroupModelCatalogEntry{{
			ShellID: "claude-mythos-7", DisplayName: "Opus 代理", RealModel: "claude-opus-4-6", Supports1M: true,
		}}},
	}

	request, rec := newCatalogModelsRequest(group, "")
	h.Models(request)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, catalogClaudeDefaultListBodyBeforeChange(t, false), rec.Body.String())
}

// 目录行的真实模型不在可见集合时（白名单不放行）：不发布。
func TestGatewayModels_CatalogRowBlockedByAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newCatalogHandlerWithDeepSeek()
	group := &service.Group{
		ID:       catalogTestGroupID,
		Platform: service.PlatformAnthropic,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"other-*"},
			Catalog: []service.GroupModelCatalogEntry{catalogDeepSeekEntry()},
		},
	}

	request, rec := newCatalogModelsRequest(group, "claude-cli/2.1.112")
	h.Models(request)

	got := catalogItemsForTest(t, rec)
	require.NotContains(t, got, "claude-mythos-7[1m]", "白名单不放行的真实模型不发布目录行")
}

// catalogGeminiDefaultListBodyBeforeChange 按改动前的 Gemini 默认列表循环重建响应体，
// 与 Claude 默认列表同理：没有目录时输出必须逐字相同（AC-14）。
func catalogGeminiDefaultListBodyBeforeChange(t *testing.T, isCC bool) string {
	t.Helper()
	defaults := slices.Clone(geminicli.DefaultModels)
	models := make([]claude.Model, 0, len(defaults))
	for i := range defaults {
		id, displayName, contextWindow := service.FormatModelForDiscoveryWithClient(defaults[i].ID, defaults[i].DisplayName, "claude", isCC)
		models = append(models, claude.Model{ID: id, Type: defaults[i].Type, DisplayName: displayName, CreatedAt: defaults[i].CreatedAt, ContextWindow: contextWindow})
	}
	body, err := json.Marshal(gin.H{"object": "list", "data": models})
	require.NoError(t, err)
	return string(body)
}

// AC-14（Gemini 回退出口）：目录为空时该出口在两种客户端下同样与改动前逐字相同。
func TestGatewayModels_EmptyCatalogGeminiDefaultListUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{}})
	group := &service.Group{ID: catalogTestGroupID, Platform: service.PlatformGemini}

	for _, tc := range []struct {
		name string
		ua   string
		isCC bool
	}{
		{name: "non-cc client", ua: "", isCC: false},
		{name: "claude code client", ua: "claude-cli/2.1.112 (external, cli)", isCC: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, rec := newCatalogModelsRequest(group, tc.ua)
			h.Models(request)
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, catalogGeminiDefaultListBodyBeforeChange(t, tc.isCC), rec.Body.String())
		})
	}
}

// catalogUsageLogStub 只捕获写入的用量行：端到端断言「用量归属按真实模型」。
type catalogUsageLogStub struct {
	service.UsageLogRepository

	logs []*service.UsageLog
}

func (s *catalogUsageLogStub) Create(_ context.Context, log *service.UsageLog) (bool, error) {
	s.logs = append(s.logs, log)
	return true, nil
}

// catalogEndToEnd 是「真实准入中间件 + 真实 Messages handler + 假上游」的夹具：
// 列表与对话走同一个 router，所以对话用的 id 就是列表实际返回的 id。
type catalogEndToEnd struct {
	router   *gin.Engine
	upstream *gateway1MTestUpstream
	usage    *catalogUsageLogStub
	group    *service.Group
}

func newCatalogEndToEnd(t *testing.T, entry service.GroupModelCatalogEntry) *catalogEndToEnd {
	t.Helper()
	gin.SetMode(gin.TestMode)

	groupID := int64(9301)
	group := &service.Group{
		ID:       groupID,
		Hydrated: true,
		Platform: service.PlatformAnthropic,
		Status:   service.StatusActive,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{entry.RealModel},
			Catalog: []service.GroupModelCatalogEntry{entry},
		},
	}
	account := service.Account{
		ID:       1,
		Name:     "catalog-acc",
		Platform: service.PlatformAnthropic,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":       "sk-ant-test",
			"model_mapping": map[string]any{entry.RealModel: entry.RealModel},
		},
		Concurrency:   5,
		Status:        service.StatusActive,
		Schedulable:   true,
		AccountGroups: []service.AccountGroup{{AccountID: 1, GroupID: groupID}},
	}

	upstream := &gateway1MTestUpstream{}
	usageLogs := &catalogUsageLogStub{}
	cfg := &config.Config{
		RunMode: config.RunModeSimple,
		Gateway: config.GatewayConfig{MaxBodySize: 1 << 20, MaxLineSize: 1 << 20},
	}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	settingSvc := service.NewSettingService(gateway1MSettingRepoStub{}, cfg)
	gwSvc := service.NewGatewayService(
		&gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{groupID: {account}}},
		nil, usageLogs, nil, nil, nil, nil, nil, cfg,
		nil, nil, nil, &service.RateLimitService{}, billingCacheSvc, nil,
		upstream, nil, nil, nil, nil, nil, settingSvc, nil, nil, nil, nil, nil, nil,
	)
	h := &GatewayHandler{
		gatewayService:      gwSvc,
		billingCacheService: billingCacheSvc,
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(&helperConcurrencyCacheStub{userSeq: []bool{true}, accountSeq: []bool{true}}), SSEPingFormatClaude, 0),
		maxAccountSwitches:  1,
		cfg:                 cfg,
	}

	apiKey := &service.APIKey{
		ID: 901, UserID: 902, GroupID: &groupID, Group: group, Status: service.StatusActive,
		User: &service.User{ID: 902, Concurrency: 10, Balance: 100, Status: service.StatusActive},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 902, Concurrency: 10})
		c.Next()
	})
	router.Use(middleware2.GroupModelAllowlist())
	router.GET("/v1/models", h.Models)
	router.GET("/v1/models/:model", h.Models)
	router.POST("/v1/messages", h.Messages)
	return &catalogEndToEnd{router: router, upstream: upstream, usage: usageLogs, group: group}
}

// publishedModels 返回 Claude Code 客户端看到的列表条目（顺序同响应）。
func (e *catalogEndToEnd) publishedModels(t *testing.T) []gatewayModelItemForTest {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("User-Agent", "claude-cli/2.1.112 (external, cli)")
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, e.group))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return got.Data
}

// getModelDetail 用列表发布的 id 查详情（URL 编码同客户端）。
func (e *catalogEndToEnd) getModelDetail(t *testing.T, modelID string) *httptest.ResponseRecorder {
	t.Helper()
	return e.getModelDetailAs(t, "claude-cli/2.1.112 (external, cli)", modelID)
}

// getModelDetailAs 指定 UA 查详情：UA 为空 = 非 Claude Code 客户端。
func (e *catalogEndToEnd) getModelDetailAs(t *testing.T, userAgent, modelID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models/"+url.PathEscape(modelID), nil)
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, e.group))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// publishedModelIDs 返回 Claude Code 客户端看到的列表 id（AC-15 要求用列表返回的 id 发请求）。
func (e *catalogEndToEnd) publishedModelIDs(t *testing.T) []string {
	t.Helper()
	items := e.publishedModels(t)
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func (e *catalogEndToEnd) postMessages(t *testing.T, model string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, e.group))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// AC-9 / AC-15 端到端：取列表实际返回的 id 发对话请求 ⇒ 准入、选号、上游模型与
// 用量归属全部落在真实模型上；授予 1M 的行还须把 1M 意图送到上游。
func TestGatewayMessages_CatalogShellIDEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name      string
		entry     service.GroupModelCatalogEntry
		wantModel string
		want1M    bool
	}{
		{
			name:      "plain shell routes to its real model",
			entry:     service.GroupModelCatalogEntry{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4"},
			wantModel: "deepseek-v4",
		},
		{
			name:      "1m shell takes the published id back verbatim",
			entry:     service.GroupModelCatalogEntry{ShellID: "claude-mythos-7", DisplayName: "Opus 代理", RealModel: "claude-opus-4-6", Supports1M: true},
			wantModel: "claude-opus-4-6",
			want1M:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e2e := newCatalogEndToEnd(t, tc.entry)

			// 看到的：列表里只有目录行。
			ids := e2e.publishedModelIDs(t)
			require.Len(t, ids, 1, "目录行替换自动包装条目：%v", ids)
			publishedID := ids[0]
			require.True(t, strings.HasPrefix(publishedID, tc.entry.ShellID), "列表 id 应为壳 id：%s", publishedID)
			require.Equal(t, tc.want1M, strings.HasSuffix(publishedID, "[1m]"), "发布 id 的 1M 后缀：%s", publishedID)

			// 能调的：原样把列表返回的 id 打回来。
			rec := e2e.postMessages(t, publishedID)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.NotNil(t, e2e.upstream.lastBody, "请求必须真的到达上游")
			require.Equal(t, tc.wantModel, gjson.GetBytes(e2e.upstream.lastBody, "model").String(), "上游拿到真实模型")
			require.NotContains(t, string(e2e.upstream.lastBody), tc.entry.ShellID, "壳 id 不得进入上游")

			beta := getRawHeaderForTest(e2e.upstream.lastReq.Header, "anthropic-beta")
			if tc.want1M {
				require.Equal(t, 1, strings.Count(beta, claude.BetaContext1M), "1M 意图经既有 beta 头送到上游：%q", beta)
			} else {
				require.NotContains(t, beta, claude.BetaContext1M, "未授予 1M 的行不得向客户端凭空开窗口：%q", beta)
			}

			require.NotEmpty(t, e2e.usage.logs, "用量必须落库")
			require.Equal(t, tc.wantModel, e2e.usage.logs[0].Model, "用量归属按真实模型")
			require.Equal(t, int64(1), e2e.usage.logs[0].AccountID, "选号与用量落在同一账号")
		})
	}
}

// catalogClaudeDefaultListBodyBeforeChange 按改动前的 Claude 默认列表循环重建响应体，
// 用于证明「没有目录时输出逐字相同」（AC-14）。
func catalogClaudeDefaultListBodyBeforeChange(t *testing.T, isCC bool) string {
	t.Helper()
	defaults := slices.Clone(claude.DefaultModels)
	for i := range defaults {
		id, displayName, contextWindow := service.FormatModelForDiscoveryWithClient(defaults[i].ID, defaults[i].DisplayName, "claude", isCC)
		defaults[i].ID = id
		defaults[i].DisplayName = displayName
		defaults[i].ContextWindow = contextWindow
	}
	body, err := json.Marshal(gin.H{"object": "list", "data": defaults})
	require.NoError(t, err)
	return string(body)
}

// AC-17：CC 客户端拿列表发布的壳 id（含带 [1m] 的那行）查详情 ⇒ 200，
// id / display_name / context_window 与列表一致。
// 路径参数分支只判准入、不改写 c.Params，详情 handler 按列表 id 匹配并原样返回。
func TestGatewayModels_CatalogDetailLookupByPublishedID(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry service.GroupModelCatalogEntry
	}{
		{
			name:  "plain shell id",
			entry: service.GroupModelCatalogEntry{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4"},
		},
		{
			name:  "1m shell id",
			entry: service.GroupModelCatalogEntry{ShellID: "claude-mythos-7", DisplayName: "Opus 代理", RealModel: "claude-opus-4-6", Supports1M: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e2e := newCatalogEndToEnd(t, tc.entry)

			listed := e2e.publishedModels(t)
			require.Len(t, listed, 1, "目录行替换自动包装条目：%v", listed)
			published := listed[0]
			require.True(t, strings.HasPrefix(published.ID, tc.entry.ShellID), "列表 id 应为壳 id：%s", published.ID)

			rec := e2e.getModelDetail(t, published.ID)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, published.ID, gjson.GetBytes(rec.Body.Bytes(), "id").String(), "详情 id 与列表发布值一致")
			require.Equal(t, published.DisplayName, gjson.GetBytes(rec.Body.Bytes(), "display_name").String(), "显示名与列表一致")
			require.Equal(t, published.ContextWindow, int(gjson.GetBytes(rec.Body.Bytes(), "context_window").Int()), "窗口与列表一致")
		})
	}
}

// AC-17 后半：不带壳 id 的原有详情查询行为不变——非 CC 客户端用裸真实模型查详情 ⇒ 200，
// id 就是真实模型；目录只影响带壳 id 的那条路径。
func TestGatewayModels_DetailUnchangedForBareRealModel(t *testing.T) {
	e2e := newCatalogEndToEnd(t, service.GroupModelCatalogEntry{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4"})

	rec := e2e.getModelDetailAs(t, "", "deepseek-v4")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "deepseek-v4", gjson.GetBytes(rec.Body.Bytes(), "id").String())
}

// catalogIDsOf 返回条目 id 的升序集合，用于断言「只剩裸真实模型 id」。
func catalogIDsOf(items map[string]gatewayModelItemForTest) []string {
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
