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

// displayNameGroupRepoStub 是仅承载分组读写的内存 store：不落 jsonb，只模拟
// 「保存 → 再读取」在服务层的往返；归一化由真实 adminService 执行，不由假 store 顶替。
type displayNameGroupRepoStub struct {
	service.AdminGroupRepository

	groups map[int64]service.Group
}

func (r *displayNameGroupRepoStub) GetByID(_ context.Context, id int64) (*service.Group, error) {
	group, ok := r.groups[id]
	if !ok {
		return nil, service.ErrGroupNotFound
	}
	return &group, nil
}

func (r *displayNameGroupRepoStub) Update(_ context.Context, group *service.Group) error {
	r.groups[group.ID] = *group
	return nil
}

func (r *displayNameGroupRepoStub) ListWithFilters(
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

func newDisplayNameAdminGroupRouter(repo *displayNameGroupRepoStub) *gin.Engine {
	svc := service.NewAdminService(
		&config.Config{RunMode: config.RunModeStandard},
		nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := NewGroupHandlerWithConfig(svc, nil, nil, &config.Config{RunMode: config.RunModeStandard})
	r := gin.New()
	r.GET("/groups", h.List)
	r.PUT("/groups/:id", h.Update)
	return r
}

type displayNameAdminGroupItem struct {
	ID             int64 `json:"id"`
	ModelAllowlist *struct {
		Enabled      bool              `json:"enabled"`
		Models       []string          `json:"models"`
		DisplayNames map[string]string `json:"display_names"`
	} `json:"model_allowlist"`
}

// displayNamesFromAdminListForTest 回读 GET /groups 的显示名映射；键不存在时返回 nil。
func displayNamesFromAdminListForTest(t *testing.T, r *gin.Engine) map[string]string {
	t.Helper()
	res := httptest.NewRecorder()
	r.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/groups", nil))
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	var payload struct {
		Data struct {
			Items []displayNameAdminGroupItem `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 1)
	require.NotNil(t, payload.Data.Items[0].ModelAllowlist)
	return payload.Data.Items[0].ModelAllowlist.DisplayNames
}

func putDisplayNameModelAllowlistForTest(t *testing.T, r *gin.Engine, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/groups/1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
}

// AC-3：读取分组 → 保存显示名 → 再读取，配置值原样返回；未配置的模型不出现键。
// 入口是 UpdateGroupRequest.ModelAllowlist，回读出口是 dto.AdminGroup.ModelAllowlist。
func TestGroupHandlerDisplayNameRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)

	repo := &displayNameGroupRepoStub{groups: map[int64]service.Group{
		1: {
			ID:       1,
			Name:     "display-name-group",
			Platform: service.PlatformAnthropic,
			Status:   service.StatusActive,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: true,
				Models:  []string{"claude-a", "claude-b"},
			},
		},
	}}
	r := newDisplayNameAdminGroupRouter(repo)

	require.Nil(t, displayNamesFromAdminListForTest(t, r), "未配置时不应出现 display_names")

	putDisplayNameModelAllowlistForTest(t, r, `{"model_allowlist":{"enabled":true,"models":["claude-a","claude-b"],"display_names":{"claude-a":"DeepSeek · V4.1 Flash"}}}`)

	readBack := displayNamesFromAdminListForTest(t, r)
	require.Equal(t, map[string]string{"claude-a": "DeepSeek · V4.1 Flash"}, readBack)
	_, hasUnconfigured := readBack["claude-b"]
	require.False(t, hasUnconfigured, "未配置的模型不出现键")
}

// 空键 / 纯空白值：接口返回 200，归一化后两个键都不存在，回读无此键。
func TestGroupHandlerDisplayNameDropsEmptyKeyAndValue(t *testing.T) {
	gin.SetMode(gin.TestMode)

	repo := &displayNameGroupRepoStub{groups: map[int64]service.Group{
		1: {
			ID:       1,
			Name:     "display-name-group",
			Platform: service.PlatformAnthropic,
			Status:   service.StatusActive,
		},
	}}
	r := newDisplayNameAdminGroupRouter(repo)

	putDisplayNameModelAllowlistForTest(t, r, `{"model_allowlist":{"enabled":true,"models":["claude-a"],"display_names":{"":"x","claude-a":"   "}}}`)

	require.Empty(t, displayNamesFromAdminListForTest(t, r))
}

// 显示名独立于白名单开关：关闭白名单保存后，已配置的显示名仍在（KDR-2）。
func TestGroupHandlerDisplayNameSurvivesDisabledAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)

	repo := &displayNameGroupRepoStub{groups: map[int64]service.Group{
		1: {
			ID:       1,
			Name:     "display-name-group",
			Platform: service.PlatformAnthropic,
			Status:   service.StatusActive,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled:      true,
				Models:       []string{"claude-a"},
				DisplayNames: map[string]string{"claude-a": "甲"},
			},
		},
	}}
	r := newDisplayNameAdminGroupRouter(repo)

	putDisplayNameModelAllowlistForTest(t, r, `{"model_allowlist":{"enabled":false,"models":["claude-a"],"display_names":{"claude-a":"甲"}}}`)

	require.Equal(t, map[string]string{"claude-a": "甲"}, displayNamesFromAdminListForTest(t, r))
}
