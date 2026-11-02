package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// catalogGroupRepoStub 是仅承载分组读写的内存 store：不落 jsonb，只模拟
// 「保存 → 再读取」的往返；归一化由真实 adminService 执行，不由假 store 顶替。
type catalogGroupRepoStub struct {
	service.AdminGroupRepository

	groups map[int64]service.Group
}

func (r *catalogGroupRepoStub) GetByID(_ context.Context, id int64) (*service.Group, error) {
	group, ok := r.groups[id]
	if !ok {
		return nil, service.ErrGroupNotFound
	}
	return &group, nil
}

func (r *catalogGroupRepoStub) Update(_ context.Context, group *service.Group) error {
	r.groups[group.ID] = *group
	return nil
}

func (r *catalogGroupRepoStub) ListWithFilters(
	_ context.Context,
	_ pagination.PaginationParams,
	_, _, _ string,
	_ *bool,
) ([]service.Group, *pagination.PaginationResult, error) {
	groups := make([]service.Group, 0, len(r.groups))
	for _, group := range r.groups {
		groups = append(groups, group)
	}
	return groups, &pagination.PaginationResult{Total: int64(len(groups)), Page: 1, PageSize: len(groups), Pages: 1}, nil
}

func newCatalogAdminGroupRouter(repo *catalogGroupRepoStub) *gin.Engine {
	cfg := &config.Config{RunMode: config.RunModeStandard}
	svc := service.NewAdminService(
		cfg, nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := NewGroupHandlerWithConfig(svc, nil, nil, cfg)
	r := gin.New()
	r.GET("/groups", h.List)
	r.PUT("/groups/:id", h.Update)
	return r
}

func newCatalogAdminRepo(allowlist service.GroupModelAllowlist) *catalogGroupRepoStub {
	return &catalogGroupRepoStub{groups: map[int64]service.Group{
		1: {
			ID:             1,
			Name:           "catalog-group",
			Platform:       service.PlatformAnthropic,
			Status:         service.StatusActive,
			ModelAllowlist: allowlist,
		},
	}}
}

type catalogAdminGroupItem struct {
	ModelAllowlist *struct {
		Enabled bool                             `json:"enabled"`
		Models  []string                         `json:"models"`
		Catalog []service.GroupModelCatalogEntry `json:"catalog"`
	} `json:"model_allowlist"`
}

// catalogAdminReadAllowlist 回读 GET /groups 的 model_allowlist 原文字段，
// 同时返回解析结构，用来分辨「键不存在」与「键为空数组」。
func catalogAdminReadAllowlist(t *testing.T, r *gin.Engine) (map[string]any, catalogAdminGroupItem) {
	t.Helper()
	res := httptest.NewRecorder()
	r.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/groups", nil))
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	var raw struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &raw))
	require.Len(t, raw.Data.Items, 1)

	var parsed struct {
		Data struct {
			Items []catalogAdminGroupItem `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &parsed))
	require.Len(t, parsed.Data.Items, 1)
	require.NotNil(t, parsed.Data.Items[0].ModelAllowlist)

	allowlist, ok := raw.Data.Items[0]["model_allowlist"].(map[string]any)
	require.True(t, ok, "model_allowlist 必须是对象：%v", raw.Data.Items[0])
	return allowlist, parsed.Data.Items[0]
}

func catalogAdminPut(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/groups/1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)
	return res
}

// AC-6：PUT 含 catalog 的 model_allowlist → GET 回读逐字段相等；无目录的分组回读无 catalog 键。
func TestGroupHandlerCatalogRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := newCatalogAdminGroupRouter(newCatalogAdminRepo(service.GroupModelAllowlist{}))

	allowlist, _ := catalogAdminReadAllowlist(t, r)
	require.NotContains(t, allowlist, "catalog", "未配置目录时不得出现 catalog 键")

	res := catalogAdminPut(t, r, `{"model_allowlist":{"enabled":true,"models":["deepseek-*","gpt-*"],"catalog":[`+
		`{"shell_id":" CLAUDE-MYTHOS-7 ","display_name":" DeepSeek V4 ","real_model":" deepseek-v4 ","supports_1m":true},`+
		`{"shell_id":"claude-mythos-8","display_name":"GPT 5.6","real_model":"gpt-5.6"}]}}`)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	allowlist, item := catalogAdminReadAllowlist(t, r)
	require.Contains(t, allowlist, "catalog")
	require.Equal(t, []service.GroupModelCatalogEntry{
		{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4", Supports1M: true},
		{ShellID: "claude-mythos-8", DisplayName: "GPT 5.6", RealModel: "gpt-5.6"},
	}, item.ModelAllowlist.Catalog, "回读逐字段相等（壳 id 已小写、字段已 trim）")
}

// AC-12（后端半）：一次保存同时携带目录与旧 display_names ⇒ 两个键都落库、回读都在。
// 「转为目录行」不删旧键，非 CC 客户端因此不丢名字。
func TestGroupHandlerCatalogKeepsLegacyDisplayNames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := newCatalogAdminGroupRouter(newCatalogAdminRepo(service.GroupModelAllowlist{}))

	res := catalogAdminPut(t, r, `{"model_allowlist":{"enabled":false,"display_names":{"deepseek-v4":"旧名"},"catalog":[`+
		`{"shell_id":"claude-mythos-7","display_name":"DeepSeek V4","real_model":"deepseek-v4"}]}}`)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	allowlist, item := catalogAdminReadAllowlist(t, r)
	require.Contains(t, allowlist, "display_names", "转换保留旧键：非 CC 客户端只吃旧机制")
	require.Equal(t, map[string]any{"deepseek-v4": "旧名"}, allowlist["display_names"])
	require.Len(t, item.ModelAllowlist.Catalog, 1)
	require.Equal(t, "claude-mythos-7", item.ModelAllowlist.Catalog[0].ShellID)
}

// AC-10（接口半）：目录校验失败一律 400 INVALID_MODEL_CATALOG，且不落库。
func TestGroupHandlerCatalogValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name string
		repo *catalogGroupRepoStub
		body string
	}{
		{
			name: "duplicate shell id",
			repo: newCatalogAdminRepo(service.GroupModelAllowlist{Enabled: true, Models: []string{"deepseek-*"}}),
			body: `{"model_allowlist":{"enabled":true,"models":["deepseek-*"],"catalog":[` +
				`{"shell_id":"claude-mythos-3","display_name":"甲","real_model":"deepseek-v4"},` +
				`{"shell_id":"claude-mythos-3","display_name":"乙","real_model":"deepseek-v4"}]}}`,
		},
		{
			name: "invalid shell id format",
			repo: newCatalogAdminRepo(service.GroupModelAllowlist{Enabled: true, Models: []string{"deepseek-*"}}),
			body: `{"model_allowlist":{"enabled":true,"models":["deepseek-*"],"catalog":[` +
				`{"shell_id":"foo-1","display_name":"甲","real_model":"deepseek-v4"}]}}`,
		},
		{
			name: "collides with a native Claude id",
			repo: newCatalogAdminRepo(service.GroupModelAllowlist{Enabled: true, Models: []string{"deepseek-*"}}),
			body: `{"model_allowlist":{"enabled":true,"models":["deepseek-*"],"catalog":[` +
				`{"shell_id":"claude-sonnet-5-5","display_name":"甲","real_model":"deepseek-v4"}]}}`,
		},
		{
			name: "collides with an allowlist entry",
			repo: newCatalogAdminRepo(service.GroupModelAllowlist{Enabled: true, Models: []string{"claude-mythos-ops", "deepseek-*"}}),
			body: `{"model_allowlist":{"enabled":true,"models":["claude-mythos-ops","deepseek-*"],"catalog":[` +
				`{"shell_id":"claude-mythos-ops","display_name":"甲","real_model":"deepseek-v4"}]}}`,
		},
		{
			name: "collides with a legacy display name key",
			repo: newCatalogAdminRepo(service.GroupModelAllowlist{
				Enabled:      true,
				Models:       []string{"deepseek-*"},
				DisplayNames: map[string]string{"claude-mythos-2": "旧名"},
			}),
			body: `{"model_allowlist":{"enabled":true,"models":["deepseek-*"],"display_names":{"claude-mythos-2":"旧名"},"catalog":[` +
				`{"shell_id":"claude-mythos-2","display_name":"甲","real_model":"deepseek-v4"}]}}`,
		},
		{
			name: "missing display name",
			repo: newCatalogAdminRepo(service.GroupModelAllowlist{Enabled: true, Models: []string{"deepseek-*"}}),
			body: `{"model_allowlist":{"enabled":true,"models":["deepseek-*"],"catalog":[` +
				`{"shell_id":"claude-mythos-3","real_model":"deepseek-v4"}]}}`,
		},
		{
			name: "wildcard real model is rejected",
			repo: newCatalogAdminRepo(service.GroupModelAllowlist{Enabled: true, Models: []string{"deepseek-*"}}),
			body: `{"model_allowlist":{"enabled":true,"models":["deepseek-*"],"catalog":[` +
				`{"shell_id":"claude-mythos-3","display_name":"甲","real_model":"deepseek-*"}]}}`,
		},
		{
			name: "enabled allowlist rejects an unlisted real model",
			repo: newCatalogAdminRepo(service.GroupModelAllowlist{Enabled: true, Models: []string{"deepseek-*"}}),
			body: `{"model_allowlist":{"enabled":true,"models":["deepseek-*"],"catalog":[` +
				`{"shell_id":"claude-mythos-3","display_name":"甲","real_model":"gpt-5.4"}]}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCatalogAdminGroupRouter(tc.repo)
			res := catalogAdminPut(t, r, tc.body)
			require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
			require.Contains(t, res.Body.String(), "INVALID_MODEL_CATALOG", "错误码必须可指认")
			require.Empty(t, tc.repo.groups[1].ModelAllowlist.Catalog, "拒绝时不得落库")
		})
	}

	t.Run("valid catalog is accepted", func(t *testing.T) {
		repo := newCatalogAdminRepo(service.GroupModelAllowlist{})
		r := newCatalogAdminGroupRouter(repo)
		res := catalogAdminPut(t, r, `{"model_allowlist":{"enabled":false,"catalog":[`+
			`{"shell_id":"claude-mythos-custom","display_name":"甲","real_model":"deepseek-v4"}]}}`)
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		require.Len(t, repo.groups[1].ModelAllowlist.Catalog, 1, "白名单关闭时不做成员校验，目录照常落库")
	})
}
