package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// newCatalogMiddlewareRouter 挂上准入中间件，并让 handler 记录它实际读到的请求体——
// 目录改写是否回填、准入是否放行，都以 handler 看到的体为准。
func newCatalogMiddlewareRouter(apiKey *service.APIKey) (*gin.Engine, *string) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if apiKey != nil {
			c.Set(string(ContextKeyAPIKey), apiKey)
		}
		c.Next()
	})
	router.Use(GroupModelAllowlist())
	seen := new(string)
	record := func(c *gin.Context) {
		body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
		if err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		*seen = string(body)
		c.Status(http.StatusOK)
	}
	router.POST("/v1/messages", record)
	router.POST("/v1/chat/completions", record)
	return router, seen
}

func catalogMiddlewareAPIKey(enabled bool, models []string, catalog ...service.GroupModelCatalogEntry) *service.APIKey {
	return &service.APIKey{Group: &service.Group{
		Platform:       service.PlatformAnthropic,
		ModelAllowlist: service.GroupModelAllowlist{Enabled: enabled, Models: models, Catalog: catalog},
	}}
}

func catalogMiddlewarePost(t *testing.T, router *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// catalogMessagesBody 是 /v1/messages 的最小可用体（便于后续解析断言）。
func catalogMessagesBody(model string) string {
	return `{"model":"` + model + `","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
}

// AC-9（准入半）：请求带壳 id 时先反查成真实模型再准入，handler 收到的是真实模型。
func TestGroupModelCatalogMiddlewareRewritesShellIDToRealModel(t *testing.T) {
	apiKey := catalogMiddlewareAPIKey(true, []string{"deepseek-v4"},
		service.GroupModelCatalogEntry{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4"})
	router, seen := newCatalogMiddlewareRouter(apiKey)

	w := catalogMiddlewarePost(t, router, "/v1/messages", catalogMessagesBody("claude-mythos-7"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "deepseek-v4", gjson.Get(*seen, "model").String())
}

// AC-18：改写落点 = 命中的那个字段。仅 session.model 为壳 id ⇒ 改写该字段、顶层不凭空创建；
// 顶层与 session.model 同时带壳 id ⇒ 顶层改写、session 原文不动（候选顺序顶层优先）。
func TestGroupModelCatalogMiddlewareRewritesTheMatchedField(t *testing.T) {
	entries := []service.GroupModelCatalogEntry{
		{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4"},
		{ShellID: "claude-mythos-8", DisplayName: "GPT 5.6", RealModel: "gpt-5.6", Supports1M: true},
	}

	t.Run("session only shell id is rewritten in place", func(t *testing.T) {
		router, seen := newCatalogMiddlewareRouter(catalogMiddlewareAPIKey(true, []string{"deepseek-v4"}, entries...))

		w := catalogMiddlewarePost(t, router, "/v1/messages", `{"session":{"model":"claude-mythos-7"},"max_tokens":8}`)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, "deepseek-v4", gjson.Get(*seen, "session.model").String())
		require.False(t, gjson.Get(*seen, "model").Exists(), "顶层不凭空创建")
	})

	t.Run("granted 1m keeps the suffix on the session field", func(t *testing.T) {
		router, seen := newCatalogMiddlewareRouter(catalogMiddlewareAPIKey(true, []string{"gpt-5.6"}, entries...))

		w := catalogMiddlewarePost(t, router, "/v1/messages", `{"session":{"model":"claude-mythos-8[1m]"},"max_tokens":8}`)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, "gpt-5.6[1m]", gjson.Get(*seen, "session.model").String())
	})

	t.Run("top level shell id wins when both fields carry one", func(t *testing.T) {
		router, seen := newCatalogMiddlewareRouter(catalogMiddlewareAPIKey(true, []string{"deepseek-v4", "gpt-5.6"}, entries...))

		w := catalogMiddlewarePost(t, router, "/v1/messages", `{"model":"claude-mythos-7","session":{"model":"claude-mythos-8"},"max_tokens":8}`)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, "deepseek-v4", gjson.Get(*seen, "model").String())
		require.Equal(t, "claude-mythos-8", gjson.Get(*seen, "session.model").String(), "顶层优先：其余命中字段不动")
	})
}

// AC-9 后半：壳 id 的真实模型不在白名单 ⇒ 404，文案点名真实模型。
func TestGroupModelCatalogMiddlewareAdmissionNamesRealModel(t *testing.T) {
	apiKey := catalogMiddlewareAPIKey(true, []string{"other-model"},
		service.GroupModelCatalogEntry{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4"})
	router, seen := newCatalogMiddlewareRouter(apiKey)

	w := catalogMiddlewarePost(t, router, "/v1/messages", catalogMessagesBody("claude-mythos-7"))
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "deepseek-v4", "拒绝文案点名真实模型，管理员能看懂哪条准入挡住")
	require.Empty(t, *seen, "被拒绝的请求不得到达 handler")
}

// AC-15：授予 1M 的行，用列表返回的 id（带 [1m] / [1M]）打回来同样命中，
// 改写值保留后缀，交给既有 Context1MRequested 机制。
func TestGroupModelCatalogMiddleware1MClosure(t *testing.T) {
	apiKey := catalogMiddlewareAPIKey(true, []string{"gpt-5.6"},
		service.GroupModelCatalogEntry{ShellID: "claude-mythos-8", DisplayName: "GPT 5.6", RealModel: "gpt-5.6", Supports1M: true})

	for _, requestModel := range []string{"claude-mythos-8[1m]", "claude-mythos-8[1M]"} {
		t.Run(requestModel, func(t *testing.T) {
			router, seen := newCatalogMiddlewareRouter(apiKey)
			w := catalogMiddlewarePost(t, router, "/v1/messages", catalogMessagesBody(requestModel))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, "gpt-5.6[1m]", gjson.Get(*seen, "model").String())

			parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef([]byte(*seen)), domain.PlatformAnthropic)
			require.NoError(t, err)
			require.Equal(t, "gpt-5.6", parsed.Model, "[1m] 不进上游模型名")
			require.True(t, parsed.Context1MRequested, "1M 意图经既有机制继续成立")
		})
	}
}

// AC-16：未授予 1M 的行带 [1m] ⇒ 不命中目录，请求体不动，准入按原文判并点名原文。
func TestGroupModelCatalogMiddlewareNotGranted1MStaysUntouched(t *testing.T) {
	apiKey := catalogMiddlewareAPIKey(true, []string{"deepseek-v4"},
		service.GroupModelCatalogEntry{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4"})
	router, seen := newCatalogMiddlewareRouter(apiKey)

	w := catalogMiddlewarePost(t, router, "/v1/messages", catalogMessagesBody("claude-mythos-7[1m]"))
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "claude-mythos-7[1m]", "文案点名原文，不存在静默换成真实模型")
	require.Empty(t, *seen)
}

// 白名单关闭时目录照常反查：目录是客户端可见面，与白名单开关无关。
func TestGroupModelCatalogMiddlewareWorksWithDisabledAllowlist(t *testing.T) {
	apiKey := catalogMiddlewareAPIKey(false, nil,
		service.GroupModelCatalogEntry{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4"})
	router, seen := newCatalogMiddlewareRouter(apiKey)

	w := catalogMiddlewarePost(t, router, "/v1/messages", catalogMessagesBody("claude-mythos-7"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "deepseek-v4", gjson.Get(*seen, "model").String())
}

// 非 Claude 形状入口（OpenAI chat）维持现状：目录不参与反查。
func TestGroupModelCatalogMiddlewareLeavesNonClaudeRoutesAlone(t *testing.T) {
	apiKey := catalogMiddlewareAPIKey(false, nil,
		service.GroupModelCatalogEntry{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4"})
	router, seen := newCatalogMiddlewareRouter(apiKey)

	body := `{"model":"claude-mythos-7"}`
	w := catalogMiddlewarePost(t, router, "/v1/chat/completions", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, body, *seen)
}

// 白名单关闭且目录为空 ⇒ 保持既有快速路径：不读请求体。
func TestGroupModelCatalogMiddlewareKeepsEmptyCatalogFastPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyAPIKey), catalogMiddlewareAPIKey(false, nil))
		c.Next()
	})
	router.Use(GroupModelAllowlist())
	router.POST("/v1/messages", func(c *gin.Context) { c.Status(http.StatusOK) })

	body := &readTrackingBody{Reader: strings.NewReader(`{"model":"claude-mythos-7"}`)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.False(t, body.read, "目录为空时快速路径不得读体")
}

// 目录非空时即使白名单关闭也必须读体：快速路径的判据是「白名单未启用且目录为空」。
func TestGroupModelCatalogMiddlewareReadsBodyWhenCatalogPresent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyAPIKey), catalogMiddlewareAPIKey(false, nil,
			service.GroupModelCatalogEntry{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4"}))
		c.Next()
	})
	router.Use(GroupModelAllowlist())
	router.POST("/v1/messages", func(c *gin.Context) { c.Status(http.StatusOK) })

	body := &readTrackingBody{Reader: strings.NewReader(`{"model":"claude-mythos-7"}`)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.True(t, body.read, "目录非空时必须读体做反查")
}

// newCatalogPathParamRouter 挂上准入中间件，并记录 handler 实际读到的路径参数。
// 路径参数分支只判准入、不改写 c.Params，所以这里记下的值就是详情 handler 会拿去匹配的值。
func newCatalogPathParamRouter(apiKey *service.APIKey) (*gin.Engine, *string) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if apiKey != nil {
			c.Set(string(ContextKeyAPIKey), apiKey)
		}
		c.Next()
	})
	router.Use(GroupModelAllowlist())
	seen := new(string)
	record := func(c *gin.Context) {
		*seen = c.Param("model")
		c.Status(http.StatusOK)
	}
	router.GET("/v1/models/:model", record)
	router.GET("/models/:model", record)
	router.GET("/v1beta/models/:model", record)
	return router, seen
}

func catalogMiddlewareGet(t *testing.T, router *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func catalogMiddlewareDetailAPIKey(enabled bool, models []string) *service.APIKey {
	return catalogMiddlewareAPIKey(enabled, models,
		service.GroupModelCatalogEntry{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4"},
		service.GroupModelCatalogEntry{ShellID: "claude-mythos-8", DisplayName: "GPT 5.6", RealModel: "gpt-5.6", Supports1M: true})
}

// AC-17（准入半）：路径参数命中目录 ⇒ 按裸真实模型判准入并放行，c.Params 原值不变
// ——详情入口要的是列表里存在的那个 id，改成真实模型只会让详情 404。
func TestGroupModelCatalogMiddlewarePathParamAdmitsRealModelWithoutRewriting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		apiKey    *service.APIKey
		param     string
		wantParam string
	}{
		{name: "allowlist enabled", apiKey: catalogMiddlewareDetailAPIKey(true, []string{"deepseek-v4"}), param: "claude-mythos-7", wantParam: "claude-mythos-7"},
		{name: "allowlist disabled", apiKey: catalogMiddlewareDetailAPIKey(false, nil), param: "claude-mythos-7", wantParam: "claude-mythos-7"},
		{name: "granted 1m suffix", apiKey: catalogMiddlewareDetailAPIKey(true, []string{"gpt-5.6"}), param: "claude-mythos-8%5B1m%5D", wantParam: "claude-mythos-8[1m]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, seen := newCatalogPathParamRouter(tc.apiKey)

			w := catalogMiddlewareGet(t, router, "/v1/models/"+tc.param)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, tc.wantParam, *seen, "路径参数原样交给 handler，不换成真实模型")
		})
	}
}

// AC-17 后半：壳 id 的真实模型不在白名单 ⇒ 404，文案点名真实模型。
func TestGroupModelCatalogMiddlewarePathParamAdmissionNamesRealModel(t *testing.T) {
	router, seen := newCatalogPathParamRouter(catalogMiddlewareDetailAPIKey(true, []string{"other-model"}))

	w := catalogMiddlewareGet(t, router, "/v1/models/claude-mythos-7")

	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "deepseek-v4", "准入按真实模型判，拒绝文案点名真实模型")
	require.NotContains(t, w.Body.String(), "claude-mythos-7", "文案点的是真实模型，不是壳 id")
	require.Empty(t, *seen, "被拒绝的请求不得到达 handler")
}

// 未命中目录 ⇒ 按原文走既有准入（含「未授予 1M 的行带 [1m]」这一不命中态）。
func TestGroupModelCatalogMiddlewarePathParamMissKeepsExistingAdmission(t *testing.T) {
	for _, param := range []string{"claude-mythos-9", "claude-mythos-7%5B1m%5D"} {
		t.Run(param, func(t *testing.T) {
			router, seen := newCatalogPathParamRouter(catalogMiddlewareDetailAPIKey(true, []string{"deepseek-v4"}))

			w := catalogMiddlewareGet(t, router, "/v1/models/"+param)

			require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), strings.ReplaceAll(param, "%5B1m%5D", "[1m]"),
				"文案点名原文，不存在静默换成真实模型")
			require.Empty(t, *seen)
		})
	}
}

// 根路径别名 `/models/:model` 是同一条详情入口，同样参与准入判定、同样不改写参数。
func TestGroupModelCatalogMiddlewarePathParamRootAliasAdmits(t *testing.T) {
	router, seen := newCatalogPathParamRouter(catalogMiddlewareDetailAPIKey(true, []string{"deepseek-v4"}))

	w := catalogMiddlewareGet(t, router, "/models/claude-mythos-7")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "claude-mythos-7", *seen, "根别名的路径参数同样原样交给 handler")
}

// Gemini 原生入口不参与目录反查：路径参数按原文准入。
func TestGroupModelCatalogMiddlewareLeavesGeminiPathParamsAlone(t *testing.T) {
	router, seen := newCatalogPathParamRouter(catalogMiddlewareDetailAPIKey(true, []string{"deepseek-v4"}))

	w := catalogMiddlewareGet(t, router, "/v1beta/models/claude-mythos-7")

	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "claude-mythos-7", "Gemini 原生入口按原文准入")
	require.Empty(t, *seen)
}

// catalogMultipartMessagesBody 组装一个 multipart/form-data 请求体，字段 model 取给入值。
func catalogMultipartMessagesBody(model string) (contentType, body string) {
	const boundary = "catalogmultipartboundary"
	body = "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"model\"\r\n\r\n" +
		model + "\r\n" +
		"--" + boundary + "--\r\n"
	return "multipart/form-data; boundary=" + boundary, body
}

func catalogMiddlewareMultipartPost(t *testing.T, router *gin.Engine, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// AC-9（multipart 半）：带目录壳 id 的 multipart 请求 ⇒ 准入层 400 显式拒绝，
// 白名单开 / 关两态一致；文案点名壳 id 与「该入口暂不支持目录壳 id，请改用 JSON 请求体」，
// 请求不改写、不落到下游。
func TestGroupModelCatalogMiddlewareRejectsMultipartShellID(t *testing.T) {
	entry := service.GroupModelCatalogEntry{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4"}
	for _, tc := range []struct {
		name   string
		apiKey *service.APIKey
	}{
		{name: "allowlist enabled", apiKey: catalogMiddlewareAPIKey(true, []string{"deepseek-v4"}, entry)},
		{name: "allowlist disabled", apiKey: catalogMiddlewareAPIKey(false, nil, entry)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, seen := newCatalogMiddlewareRouter(tc.apiKey)
			contentType, body := catalogMultipartMessagesBody("claude-mythos-7")

			w := catalogMiddlewareMultipartPost(t, router, contentType, body)

			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			message := gjson.Get(w.Body.String(), "error.message").String()
			require.Contains(t, message, "claude-mythos-7", "文案点名壳 id")
			require.Contains(t, message, "该入口暂不支持目录壳 id，请改用 JSON 请求体")
			require.Equal(t, "api_error", gjson.Get(w.Body.String(), "error.type").String(),
				"Anthropic 形状没有 code 字段：判据 = 400 + error.type=api_error + 点名文案")
			require.Empty(t, *seen, "被拒绝的请求不得落到下游")
		})
	}
}

// multipart 未命中目录 ⇒ 按原文准入：不改写、不拒绝。
func TestGroupModelCatalogMiddlewareMultipartMissPassesThrough(t *testing.T) {
	entry := service.GroupModelCatalogEntry{ShellID: "claude-mythos-7", DisplayName: "DeepSeek V4", RealModel: "deepseek-v4"}
	router, seen := newCatalogMiddlewareRouter(catalogMiddlewareAPIKey(false, nil, entry))
	contentType, body := catalogMultipartMessagesBody("claude-opus-4-6")

	w := catalogMiddlewareMultipartPost(t, router, contentType, body)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, body, *seen, "未命中目录：请求体原样到达下游")
}
