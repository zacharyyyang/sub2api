package service

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// catalogTestEntries 是多数用例共用的目录：一行未授予 1M、一行授予 1M。
func catalogTestEntries() []GroupModelCatalogEntry {
	return []GroupModelCatalogEntry{
		{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4"},
		{ShellID: "claude-mythos-8", DisplayName: "GPT 5.6", RealModel: "gpt-5.6", Supports1M: true},
	}
}

// AC-10（服务半）：目录归一化的全部拒绝分支与保留规则。错误一律 400 INVALID_MODEL_CATALOG，
// 且点名行下标与被拒的值——静默丢行等于替管理员删配置。
func TestNormalizeGroupModelCatalog(t *testing.T) {
	t.Run("trims fields and lowercases shell id, keeps order and 1m flag", func(t *testing.T) {
		got, err := normalizeGroupModelCatalog(GroupModelAllowlist{}, []GroupModelCatalogEntry{
			{ShellID: "  CLAUDE-Mythos-7  ", DisplayName: " DeepSeek V4 ", RealModel: " deepseek-v4 ", Supports1M: true},
			{ShellID: "claude-mythos-2", DisplayName: "GPT 5.6", RealModel: "gpt-5.6"},
		})
		require.NoError(t, err)
		require.Equal(t, []GroupModelCatalogEntry{
			{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4", Supports1M: true},
			{ShellID: "claude-mythos-2", DisplayName: "GPT 5.6", RealModel: "gpt-5.6"},
		}, got)
	})

	t.Run("empty catalog stays nil so legacy groups keep their json shape", func(t *testing.T) {
		got, err := normalizeGroupModelCatalog(GroupModelAllowlist{}, nil)
		require.NoError(t, err)
		require.Nil(t, got)
	})

	for _, tc := range []struct {
		name    string
		cfg     GroupModelAllowlist
		entry   GroupModelCatalogEntry
		wantMsg []string
	}{
		{
			name:    "missing shell id names the field",
			entry:   GroupModelCatalogEntry{DisplayName: "X", RealModel: "deepseek-v4"},
			wantMsg: []string{"row 0", "shell_id is required"},
		},
		{
			name:    "missing display name names the field",
			entry:   GroupModelCatalogEntry{ShellID: "claude-mythos-3", RealModel: "deepseek-v4"},
			wantMsg: []string{"row 0", "display_name is required"},
		},
		{
			name:    "missing real model names the field",
			entry:   GroupModelCatalogEntry{ShellID: "claude-mythos-3", DisplayName: "X"},
			wantMsg: []string{"row 0", "real_model is required"},
		},
		{
			name:    "family outside the six Claude families is rejected",
			entry:   GroupModelCatalogEntry{ShellID: "foo-1", DisplayName: "X", RealModel: "deepseek-v4"},
			wantMsg: []string{"row 0", "foo-1"},
		},
		{
			name:    "trailing separator without a name is rejected",
			entry:   GroupModelCatalogEntry{ShellID: "claude-mythos-", DisplayName: "X", RealModel: "deepseek-v4"},
			wantMsg: []string{"row 0", "claude-mythos-"},
		},
		{
			name:    "native Claude id is rejected",
			entry:   GroupModelCatalogEntry{ShellID: "claude-sonnet-5-5", DisplayName: "X", RealModel: "deepseek-v4"},
			wantMsg: []string{"row 0", "already used by a model id"},
		},
		{
			name:    "shell id colliding with an allowlist entry is rejected",
			cfg:     GroupModelAllowlist{Models: []string{"claude-mythos-ops"}},
			entry:   GroupModelCatalogEntry{ShellID: "claude-mythos-ops", DisplayName: "X", RealModel: "deepseek-v4"},
			wantMsg: []string{"row 0", "claude-mythos-ops", "already used"},
		},
		{
			name:    "shell id colliding with a legacy display name key is rejected",
			cfg:     GroupModelAllowlist{DisplayNames: map[string]string{"claude-mythos-2": "旧名"}},
			entry:   GroupModelCatalogEntry{ShellID: "claude-mythos-2", DisplayName: "X", RealModel: "deepseek-v4"},
			wantMsg: []string{"row 0", "claude-mythos-2", "already used"},
		},
		{
			name:    "wildcard real model is rejected",
			cfg:     GroupModelAllowlist{Enabled: true, Models: []string{"deepseek-*"}},
			entry:   GroupModelCatalogEntry{ShellID: "claude-mythos-3", DisplayName: "X", RealModel: "deepseek-*"},
			wantMsg: []string{"row 0", "deepseek-*", "concrete model id"},
		},
		{
			name:    "enabled allowlist requires the real model to be allowed",
			cfg:     GroupModelAllowlist{Enabled: true, Models: []string{"deepseek-*"}},
			entry:   GroupModelCatalogEntry{ShellID: "claude-mythos-3", DisplayName: "X", RealModel: "gpt-5.4"},
			wantMsg: []string{"row 0", "gpt-5.4", "not allowed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeGroupModelCatalog(tc.cfg, []GroupModelCatalogEntry{tc.entry})
			require.Error(t, err)
			appErr := infraerrors.FromError(err)
			require.Equal(t, int32(http.StatusBadRequest), appErr.Code)
			require.Equal(t, "INVALID_MODEL_CATALOG", appErr.Reason)
			for _, want := range tc.wantMsg {
				require.Contains(t, appErr.Message, want)
			}
		})
	}

	t.Run("duplicate shell ids inside the catalog are rejected case-insensitively", func(t *testing.T) {
		_, err := normalizeGroupModelCatalog(GroupModelAllowlist{}, []GroupModelCatalogEntry{
			{ShellID: "claude-mythos-3", DisplayName: "甲", RealModel: "deepseek-v4"},
			{ShellID: "CLAUDE-MYTHOS-3", DisplayName: "乙", RealModel: "gpt-5.6"},
		})
		require.Error(t, err)
		appErr := infraerrors.FromError(err)
		require.Equal(t, "INVALID_MODEL_CATALOG", appErr.Reason)
		require.Contains(t, appErr.Message, "row 1")
		require.Contains(t, appErr.Message, "duplicated")
	})

	t.Run("disabled allowlist does not require real model membership", func(t *testing.T) {
		got, err := normalizeGroupModelCatalog(GroupModelAllowlist{}, []GroupModelCatalogEntry{
			{ShellID: "claude-mythos-3", DisplayName: "X", RealModel: "anything-goes"},
		})
		require.NoError(t, err)
		require.Len(t, got, 1)
	})

	t.Run("wildcard allowlist entry allows the real model", func(t *testing.T) {
		got, err := normalizeGroupModelCatalog(GroupModelAllowlist{Enabled: true, Models: []string{"deepseek-*"}}, []GroupModelCatalogEntry{
			{ShellID: "claude-mythos-3", DisplayName: "X", RealModel: "deepseek-v4"},
		})
		require.NoError(t, err)
		require.Len(t, got, 1)
	})
}

// 目录归一化经 normalizeGroupModelAllowlist 的薄调用生效：占位集合用的是归一化后的
// Models / DisplayNames（原样提交的大写条目同样挡住壳 id）。
func TestNormalizeGroupModelAllowlistWiresCatalog(t *testing.T) {
	got, err := normalizeGroupModelAllowlist(GroupModelAllowlist{
		Enabled:      true,
		Models:       []string{" deepseek-* "},
		DisplayNames: map[string]string{"claude-mythos-4": "旧名"},
		Catalog: []GroupModelCatalogEntry{
			{ShellID: "CLAUDE-MYTHOS-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4", Supports1M: true},
		},
	})
	require.NoError(t, err)
	require.Equal(t, []GroupModelCatalogEntry{
		{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4", Supports1M: true},
	}, got.Catalog)
	require.Equal(t, []string{"deepseek-*"}, got.Models)

	_, err = normalizeGroupModelAllowlist(GroupModelAllowlist{
		Enabled:      true,
		Models:       []string{" deepseek-* "},
		DisplayNames: map[string]string{"claude-mythos-4": "旧名"},
		Catalog:      []GroupModelCatalogEntry{{ShellID: "claude-mythos-4", DisplayName: "X", RealModel: "deepseek-v4"}},
	})
	require.Error(t, err, "旧 display_names 键归一化后仍须挡住宿用它的壳 id")
	require.Equal(t, "INVALID_MODEL_CATALOG", infraerrors.FromError(err).Reason)
}

// AC-7（Go 半）：自动壳 id 取最小可用编号，跳过目录已占用、白名单条目与旧显示名键。
func TestNextCatalogShellID(t *testing.T) {
	require.Equal(t, "claude-mythos-1", NextCatalogShellID(GroupModelAllowlist{}))

	require.Equal(t, "claude-mythos-2", NextCatalogShellID(GroupModelAllowlist{Catalog: []GroupModelCatalogEntry{
		{ShellID: "claude-mythos-1"},
		{ShellID: "claude-mythos-3"},
	}}), "已有 1 与 3 时取 2")

	require.Equal(t, "claude-mythos-4", NextCatalogShellID(GroupModelAllowlist{
		Models: []string{"claude-mythos-2"},
		Catalog: []GroupModelCatalogEntry{
			{ShellID: "claude-mythos-1"},
			{ShellID: "claude-mythos-3"},
		},
	}), "占位集合占用 2 后继续向后取")

	require.Equal(t, "claude-mythos-2", NextCatalogShellID(GroupModelAllowlist{
		DisplayNames: map[string]string{"claude-mythos-1": "旧名"},
	}), "旧显示名键同样占位")

	require.Equal(t, "claude-mythos-2", NextCatalogShellID(GroupModelAllowlist{
		Catalog: []GroupModelCatalogEntry{{ShellID: "CLAUDE-MYTHOS-1"}},
	}), "占用比较不区分大小写")
}

// AC-9 / AC-15 / AC-16（服务半）：入站反查先剥 [1m] 再精确匹配；返回的是裸真实模型
// 与「请求带了 1M」这一事实，改写值由两条来源各自规定（请求体拼后缀、路径参数不拼）。
func TestCatalogRealModelFor(t *testing.T) {
	allowlist := GroupModelAllowlist{Catalog: catalogTestEntries()}

	for _, tc := range []struct {
		in            string
		wantRealModel string
		want1M        bool
		wantOK        bool
	}{
		{in: "claude-mythos-7", wantRealModel: "deepseek-v4", wantOK: true},
		{in: "  CLAUDE-MYTHOS-7  ", wantRealModel: "deepseek-v4", wantOK: true},
		{in: "claude-mythos-8[1m]", wantRealModel: "gpt-5.6", want1M: true, wantOK: true},
		{in: "claude-mythos-8[1M]", wantRealModel: "gpt-5.6", want1M: true, wantOK: true},
		{in: "claude-mythos-8 [1m] [1m] ", wantRealModel: "gpt-5.6", want1M: true, wantOK: true},
		{in: "claude-mythos-7[1m]", wantOK: false},
		{in: "claude-mythos-9", wantOK: false},
		{in: "", wantOK: false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			realModel, requested1M, ok := allowlist.CatalogRealModelFor(tc.in)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.wantRealModel, realModel)
			require.Equal(t, tc.want1M, requested1M)
		})
	}

	var empty GroupModelAllowlist
	_, _, ok := empty.CatalogRealModelFor("claude-mythos-7")
	require.False(t, ok)

	// 请求体分支的改写值（拼回后缀）进入既有 1M 机制：剥后缀后仍是真实模型，Context1MRequested 为 true。
	realModel, requested1M, ok := allowlist.CatalogRealModelFor("claude-mythos-8[1m]")
	require.True(t, ok)
	require.True(t, requested1M)
	canonical, requested := NormalizeInboundModelID(realModel + ContextWindow1MSuffix)
	require.Equal(t, "gpt-5.6", canonical)
	require.True(t, requested, "拼回的后缀让 1M 意图继续成立")
}

// AC-17（服务半）：路径参数与请求体共用同一查找键与命中判定；路径参数分支只取准入判定用的
// 裸真实模型（详情 handler 的参数原样不动），请求体分支才拼回 `[1m]`。
func TestCatalogPathParamRealModel(t *testing.T) {
	allowlist := GroupModelAllowlist{Catalog: catalogTestEntries()}

	for _, tc := range []struct {
		in     string
		want   string
		wantOK bool
	}{
		{in: "claude-mythos-7", want: "deepseek-v4", wantOK: true},
		{in: "claude-mythos-8", want: "gpt-5.6", wantOK: true},
		{in: "claude-mythos-8[1m]", want: "gpt-5.6", wantOK: true},
		{in: "  CLAUDE-MYTHOS-8[1M]  ", want: "gpt-5.6", wantOK: true},
		{in: "claude-mythos-7[1m]", wantOK: false},
		{in: "claude-opus-4-6", wantOK: false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			realModel, ok := allowlist.CatalogPathParamRealModel(tc.in)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.want, realModel)
		})
	}

	var empty GroupModelAllowlist
	_, ok := empty.CatalogPathParamRealModel("claude-mythos-7")
	require.False(t, ok)
}

// catalogMultipartFixture 组装一个 multipart/form-data 请求体，字段名与值由调用方给。
func catalogMultipartFixture(fieldName, fieldValue string) (contentType string, body []byte) {
	const boundary = "catalogtestboundary"
	body = []byte("--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"" + fieldName + "\"\r\n\r\n" +
		fieldValue + "\r\n" +
		"--" + boundary + "--\r\n")
	return "multipart/form-data; boundary=" + boundary, body
}

// AC-9（服务半）：multipart 的模型候选按同一查找键与命中判定反查，命中即报出该候选原文，
// 供准入层显式 400（请求体不改写）。
func TestCatalogMultipartShellIDHit(t *testing.T) {
	allowlist := GroupModelAllowlist{Catalog: catalogTestEntries()}

	t.Run("form model field carrying a shell id is a hit", func(t *testing.T) {
		contentType, body := catalogMultipartFixture("model", "claude-mythos-7")
		shellID, hit := CatalogMultipartShellIDHit(allowlist, "/v1/messages", contentType, body)
		require.True(t, hit)
		require.Equal(t, "claude-mythos-7", shellID)
	})

	t.Run("granted 1m shell id keeps the suffix in the reported value", func(t *testing.T) {
		contentType, body := catalogMultipartFixture("model", "claude-mythos-8[1m]")
		shellID, hit := CatalogMultipartShellIDHit(allowlist, "/v1/messages", contentType, body)
		require.True(t, hit)
		require.Equal(t, "claude-mythos-8[1m]", shellID)
	})

	t.Run("legible model id is not a hit", func(t *testing.T) {
		contentType, body := catalogMultipartFixture("model", "claude-opus-4-6")
		_, hit := CatalogMultipartShellIDHit(allowlist, "/v1/messages", contentType, body)
		require.False(t, hit)
	})

	t.Run("1m suffix on a row without the grant is not a hit", func(t *testing.T) {
		contentType, body := catalogMultipartFixture("model", "claude-mythos-7[1m]")
		_, hit := CatalogMultipartShellIDHit(allowlist, "/v1/messages", contentType, body)
		require.False(t, hit)
	})

	t.Run("session field candidates use the same lookup", func(t *testing.T) {
		contentType, body := catalogMultipartFixture("session", `{"model":"claude-mythos-7"}`)
		shellID, hit := CatalogMultipartShellIDHit(allowlist, "/v1/messages", contentType, body)
		require.True(t, hit)
		require.Equal(t, "claude-mythos-7", shellID)
	})

	for _, tc := range []struct {
		name        string
		cfg         GroupModelAllowlist
		routePath   string
		contentType string
		body        []byte
	}{
		{
			name:        "json bodies are not this branch",
			cfg:         allowlist,
			routePath:   "/v1/messages",
			contentType: "application/json",
			body:        []byte(`{"model":"claude-mythos-7"}`),
		},
		{
			name:        "non messages routes do not participate",
			cfg:         allowlist,
			routePath:   "/v1/chat/completions",
			contentType: "multipart/form-data; boundary=catalogtestboundary",
			body:        []byte("--catalogtestboundary\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nclaude-mythos-7\r\n--catalogtestboundary--\r\n"),
		},
		{
			name:        "empty catalog never hits",
			cfg:         GroupModelAllowlist{},
			routePath:   "/v1/messages",
			contentType: "multipart/form-data; boundary=catalogtestboundary",
			body:        []byte("--catalogtestboundary\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nclaude-mythos-7\r\n--catalogtestboundary--\r\n"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, hit := CatalogMultipartShellIDHit(tc.cfg, tc.routePath, tc.contentType, tc.body)
			require.False(t, hit)
		})
	}
}

// AC-8（服务半）：发布期 1M 装饰——后缀、显示名 ` 1M` 与 context_window，不重复追加。
func TestFormatCatalogEntryForDiscovery(t *testing.T) {
	id, displayName, contextWindow := FormatCatalogEntryForDiscovery(GroupModelCatalogEntry{
		ShellID: "CLAUDE-Mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4", Supports1M: true,
	})
	require.Equal(t, "claude-mythos-7[1m]", id)
	require.Equal(t, "DeepSeek V4 1M", displayName)
	require.Equal(t, ContextWindow1MTokens, contextWindow)

	_, displayName, _ = FormatCatalogEntryForDiscovery(GroupModelCatalogEntry{
		ShellID: "claude-mythos-8", DisplayName: "X 1M", Supports1M: true,
	})
	require.Equal(t, "X 1M", displayName, "已有 1M 尾巴不重复追加")

	id, displayName, contextWindow = FormatCatalogEntryForDiscovery(GroupModelCatalogEntry{
		ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4",
	})
	require.Equal(t, "claude-mythos-7", id)
	require.Equal(t, "DeepSeek V4", displayName)
	require.Zero(t, contextWindow, "未授予 1M 不报窗口")
}

// F7 出口（服务半）：目录行只在真实模型属于可见集合、且白名单放行时发布；
// 非 Claude Code 客户端不发布；同一真实模型的多行各发一条。
func TestCatalogDiscoveryEntries(t *testing.T) {
	cfg := GroupModelAllowlist{Catalog: catalogTestEntries()}

	t.Run("publishes visible catalog rows for claude code clients", func(t *testing.T) {
		entries, covered := CatalogDiscoveryEntries(cfg, []string{"gpt-5.2", "deepseek-v4"}, true)
		require.Len(t, entries, 1)
		require.Equal(t, "claude-mythos-7", entries[0].ID)
		require.Equal(t, "DeepSeek V4", entries[0].DisplayName)
		require.Zero(t, entries[0].ContextWindow)
		require.Contains(t, covered, "deepseek-v4")
		require.NotContains(t, covered, "gpt-5.2")

		_, coveredBoth := CatalogDiscoveryEntries(cfg, []string{"deepseek-v4", "gpt-5.6"}, true)
		require.Contains(t, coveredBoth, "gpt-5.6")
	})

	t.Run("non claude code clients never see the catalog", func(t *testing.T) {
		entries, covered := CatalogDiscoveryEntries(cfg, []string{"deepseek-v4", "gpt-5.6"}, false)
		require.Nil(t, entries)
		require.Nil(t, covered)
	})

	t.Run("real model outside the visible set is not published", func(t *testing.T) {
		entries, covered := CatalogDiscoveryEntries(cfg, []string{"claude-opus-4-6"}, true)
		require.Nil(t, entries)
		require.Nil(t, covered)
	})

	t.Run("enabled allowlist must allow the real model", func(t *testing.T) {
		blocked := cfg
		blocked.Enabled = true
		blocked.Models = []string{"other-*"}
		entries, _ := CatalogDiscoveryEntries(blocked, []string{"deepseek-v4"}, true)
		require.Nil(t, entries, "看到什么 = 能调什么：白名单不放行即不发布")

		allowed := cfg
		allowed.Enabled = true
		allowed.Models = []string{"deepseek-*"}
		entries, _ = CatalogDiscoveryEntries(allowed, []string{"deepseek-v4"}, true)
		require.Len(t, entries, 1)
	})

	t.Run("two rows on the same real model both publish", func(t *testing.T) {
		two := GroupModelAllowlist{Catalog: []GroupModelCatalogEntry{
			{ShellID: "claude-mythos-5", DisplayName: "甲", RealModel: "deepseek-v4"},
			{ShellID: "claude-mythos-6", DisplayName: "乙", RealModel: "deepseek-v4", Supports1M: true},
		}}
		entries, covered := CatalogDiscoveryEntries(two, []string{"deepseek-v4"}, true)
		require.Len(t, entries, 2)
		require.Equal(t, "claude-mythos-5", entries[0].ID)
		require.Equal(t, "claude-mythos-6[1m]", entries[1].ID)
		require.Contains(t, covered, "deepseek-v4")
	})
}

// F8 入站（服务半）：改写请求体的定位与形状边界。
func TestRewriteCatalogModelInBody(t *testing.T) {
	cfg := GroupModelAllowlist{Catalog: catalogTestEntries()}

	rewrite := func(routePath, contentType, body string) (string, bool) {
		rewritten, changed := RewriteCatalogModelInBody(cfg, routePath, contentType, []byte(body))
		return string(rewritten), changed
	}

	t.Run("messages route rewrites the shell id to the real model", func(t *testing.T) {
		body, changed := rewrite("/v1/messages", "application/json", `{"model":"claude-mythos-7","max_tokens":8}`)
		require.True(t, changed)
		require.Equal(t, "deepseek-v4", gjson.Get(body, "model").String())
	})

	t.Run("granted 1m keeps the suffix on the rewritten value", func(t *testing.T) {
		body, changed := rewrite("/v1/messages", "application/json", `{"model":"claude-mythos-8[1M]"}`)
		require.True(t, changed)
		require.Equal(t, "gpt-5.6[1m]", gjson.Get(body, "model").String())
	})

	t.Run("not granted 1m leaves the body untouched", func(t *testing.T) {
		body, changed := rewrite("/v1/messages", "application/json", `{"model":"claude-mythos-7[1m]"}`)
		require.False(t, changed)
		require.Contains(t, body, "claude-mythos-7[1m]")
	})

	// 反查绑定在「将被改写的那个字段」的值上：其它字段里的壳 id 不得盖到顶层 model 上，
	// 也不得把一个正常模型静默改道（评审 round1 #1）。
	t.Run("a shell id in another field never hijacks the top level model", func(t *testing.T) {
		body, changed := rewrite("/v1/messages", "application/json", `{"model":"claude-opus-4-6","session":{"model":"claude-mythos-7"}}`)
		require.False(t, changed)
		require.Equal(t, "claude-opus-4-6", gjson.Get(body, "model").String())
	})

	// AC-18：改写落点 = 命中的那个字段，未命中的字段原样保留。
	t.Run("session only bodies rewrite session.model and create no top level model", func(t *testing.T) {
		body, changed := rewrite("/v1/messages", "application/json", `{"session":{"model":"claude-mythos-7"},"max_tokens":8}`)
		require.True(t, changed)
		require.Equal(t, "deepseek-v4", gjson.Get(body, "session.model").String())
		require.False(t, gjson.Get(body, "model").Exists(), "顶层不凭空创建")
	})

	t.Run("granted 1m keeps the suffix on the rewritten session field", func(t *testing.T) {
		body, changed := rewrite("/v1/messages", "application/json", `{"session":{"model":"claude-mythos-8[1m]"}}`)
		require.True(t, changed)
		require.Equal(t, "gpt-5.6[1m]", gjson.Get(body, "session.model").String())
	})

	t.Run("both fields carry shell ids: top level is rewritten and session is left alone", func(t *testing.T) {
		body, changed := rewrite("/v1/messages", "application/json", `{"model":"claude-mythos-7","session":{"model":"claude-mythos-8"}}`)
		require.True(t, changed)
		require.Equal(t, "deepseek-v4", gjson.Get(body, "model").String())
		require.Equal(t, "claude-mythos-8", gjson.Get(body, "session.model").String(), "顶层优先：其余命中字段不动")
	})

	// 重复键 / 重复 session 对象：改写的是命中那个出现本身，其余出现原样保留
	// （按路径写入只会改首个出现，命中后一个时就会改错字段）。
	t.Run("the first catalog hit among duplicate keys is rewritten in place", func(t *testing.T) {
		body, changed := rewrite("/v1/messages", "application/json", `{"model":"claude-opus-4-6","model":"claude-mythos-7"}`)
		require.True(t, changed)
		require.Equal(t, `{"model":"claude-opus-4-6","model":"deepseek-v4"}`, body)
	})

	t.Run("a later session object is rewritten on its own", func(t *testing.T) {
		body, changed := rewrite("/v1/messages", "application/json", `{"session":{"model":"claude-opus-4-6"},"session":{"model":"claude-mythos-7"}}`)
		require.True(t, changed)
		require.Equal(t, `{"session":{"model":"claude-opus-4-6"},"session":{"model":"deepseek-v4"}}`, body)
	})

	// 顶层 model 非字符串时按既有候选规则回落 session.model（与 FromBodyCandidates 同序）。
	t.Run("a non string top level model falls back to session.model", func(t *testing.T) {
		body, changed := rewrite("/v1/messages", "application/json", `{"model":null,"session":{"model":"claude-mythos-7"}}`)
		require.True(t, changed)
		require.Equal(t, `{"model":null,"session":{"model":"deepseek-v4"}}`, body)
	})

	// 大小写变体：键名原样保留，只换值。
	t.Run("case variant keys are rewritten where they are", func(t *testing.T) {
		body, changed := rewrite("/v1/messages", "application/json", `{"Model":"claude-mythos-7"}`)
		require.True(t, changed)
		require.Equal(t, `{"Model":"deepseek-v4"}`, body)
	})

	// 同一键名的前一次出现不是字符串：跳过后仍按命中那一次的位置改写。
	t.Run("a non string duplicate never shifts the rewrite position", func(t *testing.T) {
		body, changed := rewrite("/v1/messages", "application/json", `{"model":123,"model":"claude-mythos-7"}`)
		require.True(t, changed)
		require.Equal(t, `{"model":123,"model":"deepseek-v4"}`, body)
	})

	// 大小写变体并存：只改命中首个候选的那一处，另一个变体原样保留。
	t.Run("a case variant sibling is left alone when the exact key hits first", func(t *testing.T) {
		body, changed := rewrite("/v1/messages", "application/json", `{"model":"claude-mythos-7","Model":"claude-opus-4-6"}`)
		require.True(t, changed)
		require.Equal(t, `{"model":"deepseek-v4","Model":"claude-opus-4-6"}`, body)
	})

	for _, tc := range []struct {
		name        string
		routePath   string
		contentType string
		body        string
	}{
		{name: "openai chat route does not participate", routePath: "/v1/chat/completions", contentType: "application/json", body: `{"model":"claude-mythos-7"}`},
		{name: "gemini native route does not participate", routePath: "/v1beta/models", contentType: "application/json", body: `{"model":"claude-mythos-7"}`},
		{name: "multipart bodies are not rewritten", routePath: "/v1/messages", contentType: "multipart/form-data; boundary=x", body: `{"model":"claude-mythos-7"}`},
		{name: "invalid json is left alone", routePath: "/v1/messages", contentType: "application/json", body: `not-json`},
		{name: "unknown model is left alone", routePath: "/v1/messages", contentType: "application/json", body: `{"model":"claude-opus-4-6"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, changed := rewrite(tc.routePath, tc.contentType, tc.body)
			require.False(t, changed)
			require.Equal(t, tc.body, body)
		})
	}

	t.Run("empty catalog never rewrites", func(t *testing.T) {
		_, changed := RewriteCatalogModelInBody(GroupModelAllowlist{}, "/v1/messages", "application/json", []byte(`{"model":"claude-mythos-7"}`))
		require.False(t, changed)
	})
}

// 目录必须逐字段穿过 domain 往返：漏一个字段就是「配置写进库、网关侧恒为空」。
func TestGroupModelCatalogDomainRoundTrip(t *testing.T) {
	cfg := GroupModelAllowlist{
		Enabled: true,
		Models:  []string{"deepseek-*"},
		Catalog: catalogTestEntries(),
	}

	domainCfg := DomainGroupModelAllowlist(cfg)
	require.Equal(t, []domain.GroupModelCatalogEntry{
		{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4"},
		{ShellID: "claude-mythos-8", DisplayName: "GPT 5.6", RealModel: "gpt-5.6", Supports1M: true},
	}, domainCfg.Catalog)

	back := GroupModelAllowlistFromDomain(domainCfg)
	require.Equal(t, cfg.Catalog, back.Catalog)

	empty := GroupModelAllowlistFromDomain(DomainGroupModelAllowlist(GroupModelAllowlist{Enabled: true, Models: []string{"m"}}))
	require.Nil(t, empty.Catalog, "存量分组读出来必须是 nil")
}

// 前端与后端同一条自动发号规则：本用例是 Go 侧的判定，前端 spec 断言同一组输入输出。
func TestNextCatalogShellIDMatchesProductRule(t *testing.T) {
	cfg := GroupModelAllowlist{Catalog: []GroupModelCatalogEntry{
		{ShellID: "claude-mythos-1", DisplayName: "甲", RealModel: "deepseek-v4"},
		{ShellID: "claude-mythos-3", DisplayName: "乙", RealModel: "gpt-5.6"},
	}}
	next := NextCatalogShellID(cfg)
	require.Equal(t, "claude-mythos-2", next)
	require.True(t, strings.HasPrefix(next, "claude-mythos-"), "自动 id 固定落在 mythos 家族")
}
