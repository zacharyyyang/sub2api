package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// GroupModelAllowlist 是分组级模型白名单准入中间件。
//
// 挂载位置：每条网关链的 apiKeyAuth 之后、compositeTarget 之前——
// 保证校验发生在合成路由改写与调度之前，且只看客户端书写的公开模型名。
//
// 行为：
//   - 快速路径：未绑定分组，或白名单未开启且分组目录为空时直接放行，不读请求体。
//   - 目录反查（两条来源共用同一查找键与命中判定：剥尾部 `[1m]` 后精确匹配壳 id）：
//     请求体分支命中即改写成真实模型并回填（带 1M 时拼回后缀，交给既有 Context1MRequested
//     机制）；路径参数分支〔`/v1/models/:model` 与根别名〕命中即按裸真实模型判准入，
//     **不改写路由参数**——详情 handler 要按列表发布的 id 匹配。
//   - Responses WebSocket 入口跳过（首帧与后续 turn 由 ResponsesWebSocket 逐帧
//     校验）；Grok Realtime 的升级请求模型固定在查询参数里，仍走中间件校验，
//     其他路由伪造 Upgrade 头不得绕过校验。
//   - GET/DELETE：从路由参数（model / modelAction）或查询参数（/realtime 的
//     model）提取模型；提取不到放行，由 handler 决定是否报「model is required」。
//   - 带请求体的方法：读体（PrereadBody 回填，下游零拷贝）、提取 JSON
//     `model`/`session.model` 或 multipart `model`/`session` 后回填请求体。
//   - 拒绝：按准入拦截返回 404，按入口协议格式输出，并标记运维业务限流原因
//     local_model_configuration 与 ingress 拒绝原因 model_not_allowed；
//     multipart 带目录壳 id 返回 400（请求体不改写又不拒绝等于把壳 id 放行到上游）。
func GroupModelAllowlist() gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey, ok := GetAPIKeyFromContext(c)
		if !ok || apiKey == nil || apiKey.Group == nil {
			c.Next()
			return
		}
		allowlist := apiKey.Group.ModelAllowlist
		// 快速路径：白名单未启用且目录为空时直接放行，不读请求体。
		// 目录是「客户端看到什么」，白名单是「允许调什么」——白名单关闭时目录同样要反查，
		// 所以判据从「白名单未启用」改成「白名单未启用且目录为空」（设计 §2.4）。
		if !allowlist.Enabled && len(allowlist.Catalog) == 0 {
			c.Next()
			return
		}
		if c.Request == nil {
			c.Next()
			return
		}

		if isResponsesWebSocketRoute(c) {
			// Responses WS 长连接由 ResponsesWebSocket 校验首帧与每个 response.create。
			c.Next()
			return
		}
		// models 收集该请求全部可被下游解析器绑定到的模型值（路径参数/查询参数
		// 单值；请求体候选集含重复键与大小写变体），逐一校验，任一未命中即拒绝。
		var models []string
		if model := groupModelAllowlistModelFromParams(c); model != "" {
			models = []string{model}
			// 路径参数分支：列表按壳 id 发布，详情查询带的是同一个 id。命中目录即按命中的
			// 裸真实模型判准入（白名单只认真实模型），但**不改写 c.Params**——详情入口要的是
			// 列表里存在的那个 id（CC 形状列表发布的是壳 id，详情 handler 按它匹配并原样返回），
			// 换成真实模型只会让详情 404（设计 §2.4 路径参数分支）。
			if isCatalogPathParamRoute(c) {
				if realModel, hit := allowlist.CatalogPathParamRealModel(model); hit {
					models = []string{realModel}
				}
			}
		} else {
			switch c.Request.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch:
				candidates, body, done := groupModelAllowlistModelsFromBody(c)
				if !done {
					// 请求体读取失败（如超限 413）已写出响应。
					return
				}
				// multipart 带目录壳 id：本设计不改写 multipart 请求体（需重编码整个表单体），
				// 命中即准入层 400 显式拒绝，与白名单开关无关——不改写又不拒绝等于把壳 id
				// 原样放行到上游（设计 §2.4 步骤 6）。
				if shellID, hit := service.CatalogMultipartShellIDHit(allowlist, c.FullPath(), c.GetHeader("Content-Type"), body); hit {
					groupModelAllowlistErrorWriter(c)(c, http.StatusBadRequest, fmt.Sprintf("模型 %q 是分组模型目录的壳 id；该入口暂不支持目录壳 id，请改用 JSON 请求体", shellID))
					c.Abort()
					return
				}
				// 目录反查在归一化与合成路由之前：命中即把请求体改写成真实模型并回填，
				// 再按改写后的体重算候选，准入只判真实模型（设计 §2.4 步骤 2-6）。
				if rewritten, hit := service.RewriteCatalogModelInBody(allowlist, c.FullPath(), c.GetHeader("Content-Type"), body); hit {
					requestmodel.ResetRequestBody(c.Request, rewritten)
					candidates = requestmodel.FromBodyCandidates(c.FullPath(), c.GetHeader("Content-Type"), rewritten)
				}
				models = candidates
			}
			if len(models) == 0 {
				// Grok Realtime 升级请求把模型固定在查询参数里。
				if model := strings.TrimSpace(c.Query("model")); model != "" {
					models = []string{model}
				}
			}
		}

		blocked := ""
		for _, candidate := range models {
			if !allowlist.Allows(candidate) {
				blocked = candidate
				break
			}
		}
		if blocked == "" {
			c.Next()
			return
		}

		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalModelConfiguration)
		MarkIngressRejected(c, IngressRejectModelNotAllowed)
		groupModelAllowlistErrorWriter(c)(c, http.StatusNotFound, fmt.Sprintf("Model %q is not available for this group", blocked))
		c.Abort()
	}
}

// isResponsesWebSocketRoute 判断当前请求是否命中 OpenAI Responses WebSocket
// 入口。只有这三条路由的模型在升级后的帧里，交给 handler 逐帧校验；其他路由
// 即使携带 WebSocket Upgrade 头也必须经过本中间件校验。
func isResponsesWebSocketRoute(c *gin.Context) bool {
	if c.Request == nil || c.Request.Method != http.MethodGet {
		return false
	}
	switch c.FullPath() {
	case "/v1/responses", "/responses", "/backend-api/codex/responses":
		return true
	}
	return false
}

// isCatalogPathParamRoute 判断当前路由是否为 Claude 形状的单模型查询入口：
// `GET /v1/models/:model` 与其根路径别名 `/models/:model`。这两条是列表发布值的
// 第二个回程入口，目录参与准入判定；`/v1beta/models/:model`、`/v1beta/models/*modelAction`
// 属 Gemini 原生入口，目录不参与（设计 §2.4 路径参数分支 / §7）。
func isCatalogPathParamRoute(c *gin.Context) bool {
	switch c.FullPath() {
	case "/v1/models/:model", "/models/:model":
		return true
	}
	return false
}

// groupModelAllowlistModelsFromBody 读取请求体并提取客户端模型名，随后把请求体
// 回填（PrereadBody），保证后续 handler 零拷贝重读。读取失败按现有合成中间件
// 的方式返回 400/413（返回 false 表示已写出响应并 Abort）。
//
// 同时返回已读出的 body：目录命中时调用方要改写它并回填。
//
// 下游同时存在 gjson（首个、大小写敏感）、encoding/json 绑定（末值、大小写
// 不敏感）与 multipart 表单（首/末字段）三类解析器，这里返回「任一解析器可能
// 绑定到的全部模型值」，调用方必须逐一校验，任一未命中即拒绝。
func groupModelAllowlistModelsFromBody(c *gin.Context) ([]string, []byte, bool) {
	body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		status := http.StatusBadRequest
		message := "Failed to read request body"
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
			message = "Request body is too large"
		}
		c.JSON(status, gin.H{"error": gin.H{"type": "invalid_request_error", "message": message}})
		c.Abort()
		return nil, nil, false
	}
	requestmodel.ResetRequestBody(c.Request, body)
	return requestmodel.FromBodyCandidates(c.FullPath(), c.GetHeader("Content-Type"), body), body, true
}

// groupModelAllowlistModelFromParams 从路由参数提取模型名：Gemini 原生 URL 的
// `model` / `modelAction`（去掉 `:action` 后缀）。提取不到返回空串，
// 由调用方决定是否继续读请求体或查询参数。
func groupModelAllowlistModelFromParams(c *gin.Context) string {
	if model := strings.TrimSpace(c.Param("model")); model != "" {
		return model
	}
	if modelAction := strings.TrimSpace(c.Param("modelAction")); modelAction != "" {
		modelAction = strings.TrimPrefix(modelAction, "/")
		if idx := strings.LastIndex(modelAction, ":"); idx >= 0 {
			return strings.TrimSpace(modelAction[:idx])
		}
		return modelAction
	}
	return ""
}

// groupModelAllowlistErrorWriter 按入口协议选择错误格式：
// Gemini 原生（/v1beta、/antigravity/v1beta）用 Google 格式；
// Messages 入口（含根路径别名与 /antigravity/v1）用 Anthropic 格式；
// 其余（OpenAI 兼容入口）用 OpenAI 格式。
func groupModelAllowlistErrorWriter(c *gin.Context) GatewayErrorWriter {
	path := ""
	if c.Request != nil && c.Request.URL != nil {
		path = c.Request.URL.Path
	}
	switch {
	case strings.HasPrefix(path, "/v1beta") || strings.HasPrefix(path, "/antigravity/v1beta"):
		return GoogleErrorWriter
	case strings.Contains(path, "/messages"):
		return AnthropicErrorWriter
	default:
		return OpenAIErrorWriter
	}
}
