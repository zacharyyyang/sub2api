package service

import (
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// Gemini 原生请求（/v1beta/models/{model}:generateContent 等）经 Antigravity 账号转发时，
// 客户端（如 Antigravity CLI / go-genai）习惯发"裸"模型名（gemini-3.8-flash）并用
// generationConfig.thinkingConfig 表达思考深度；而 Antigravity 上游的模型目录只有
// gemini-3.8-flash-low / -medium / -high / -tiered 这类带后缀的变体，裸名直接转发会被上游
// 以 404 "Requested entity was not found." 拒绝。
//
// 这里在账号 model_mapping 没有为裸名配置显式条目、但配置了它的后缀变体时，
// 按 thinkingConfig 自动挑一个变体：
//   - thinkingLevel: "low" / "medium" / "high" → 同名后缀
//   - thinkingBudget: -1（动态）→ high；0 → low；1..1024 → low；1025..8192 → medium；>8192 → high
//   - 未携带 thinkingConfig → high（与 Gemini 3 系列默认开启动态思考一致）
// 选中的后缀在映射表里不存在时按 high → medium → low → tiered 的顺序降级到存在的变体。
// 显式映射的优先边界：裸名映射到非同族目标（如 -tiered 或跨模型重定向）时尊重用户配置、
// 不做档位覆盖；映射到同族默认目标（k==v、同名 -high/-medium/-low、gemini-3.1-pro 家族的
// gemini-pro-agent）时仍按档位动态解析——这被视为“声明家族归属”而非显式档位选择。
// （见 isSameFamilyThinkingVariantTarget 与 resolveGeminiThinkingVariantForLevel 的判定。）

var geminiThinkingVariantSuffixes = []string{"-low", "-medium", "-high", "-tiered"}

const (
	geminiThinkingBudgetLowMax    = 1024
	geminiThinkingBudgetMediumMax = 8192
)

type geminiThinkingConfigProbe struct {
	GenerationConfig struct {
		ThinkingConfig *struct {
			ThinkingBudget *json.Number `json:"thinkingBudget"`
			ThinkingLevel  string       `json:"thinkingLevel"`
		} `json:"thinkingConfig"`
	} `json:"generationConfig"`
}

// hasGeminiThinkingVariantSuffix 判断模型名是否已经带了思考深度后缀。
func hasGeminiThinkingVariantSuffix(model string) bool {
	for _, suffix := range geminiThinkingVariantSuffixes {
		if strings.HasSuffix(model, suffix) {
			return true
		}
	}
	return false
}

// geminiThinkingLevelFromBody 从 Gemini 原生请求体推导期望的思考档位（low/medium/high）。
// 解析失败或未携带 thinkingConfig 时返回 "high"。
func geminiThinkingLevelFromBody(body []byte) string {
	if len(body) == 0 {
		return "high"
	}
	var probe geminiThinkingConfigProbe
	if err := json.Unmarshal(body, &probe); err != nil || probe.GenerationConfig.ThinkingConfig == nil {
		return "high"
	}
	tc := probe.GenerationConfig.ThinkingConfig
	switch strings.ToLower(strings.TrimSpace(tc.ThinkingLevel)) {
	case "low", "none", "minimal":
		// none/minimal 与 Claude 兼容层（output_config.effort）的语义对齐：都是“尽量浅”，落 low。
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	}
	if tc.ThinkingBudget == nil {
		return "high"
	}
	budget, err := tc.ThinkingBudget.Float64()
	if err != nil {
		return "high"
	}
	switch {
	case budget < 0:
		return "high" // -1 = 动态思考
	case budget <= geminiThinkingBudgetLowMax:
		return "low" // 含 0（关闭思考）：上游没有"无思考"变体，取最浅档，budget 本身仍原样透传
	case budget <= geminiThinkingBudgetMediumMax:
		return "medium"
	default:
		return "high"
	}
}

// geminiThinkingLevelFromClaudeRequest 综合 Claude Messages 协议的 output_config.effort 与
// thinking.budget_tokens 推导思考档位（low/medium/high）。
// 优先采用 output_config.effort（low/none -> low, medium -> medium, high/xhigh/max -> high），
// 未指定时回退到 geminiThinkingLevelFromClaudeThinking。
func geminiThinkingLevelFromClaudeRequest(thinking *antigravity.ThinkingConfig, outputConfig *antigravity.OutputConfig) string {
	if thinking != nil && strings.EqualFold(strings.TrimSpace(thinking.Type), "disabled") {
		return "low"
	}
	if outputConfig != nil {
		switch strings.ToLower(strings.TrimSpace(outputConfig.Effort)) {
		case "low", "none", "minimal":
			return "low"
		case "medium":
			return "medium"
		case "high", "xhigh", "max":
			return "high"
		}
	}
	return geminiThinkingLevelFromClaudeThinking(thinking)
}

// geminiThinkingLevelFromClaudeThinking 用 Claude Messages 协议的 thinking 配置推导档位。
// 正预算的阈值与 geminiThinkingLevelFromBody 保持一致；0/负数的语义两路不同：
// 原生路径把 0 视为“关闭思考”（落 low），这里 0/负数表示“未指定/动态思考”（落 high），
// 因为 Claude 协议里 budget_tokens 缺省与显式 0 无法在指针 nil 之外区分。
func geminiThinkingLevelFromClaudeThinking(thinking *antigravity.ThinkingConfig) string {
	if thinking == nil {
		return "high"
	}
	if strings.EqualFold(strings.TrimSpace(thinking.Type), "disabled") {
		return "low"
	}
	budget := thinking.BudgetTokens
	switch {
	case budget <= 0:
		return "high" // 动态思考 / 未指定预算
	case budget <= geminiThinkingBudgetLowMax:
		return "low"
	case budget <= geminiThinkingBudgetMediumMax:
		return "medium"
	default:
		return "high"
	}
}

// accountRawModelMappingHasKey 判断 key 是否由用户写在账号 credentials.model_mapping 里
// （而不是 resolveModelMapping 运行时补进来的默认透传条目）。
func accountRawModelMappingHasKey(account *Account, key string) bool {
	if account == nil || account.Credentials == nil {
		return false
	}
	raw, _ := account.Credentials["model_mapping"].(map[string]any)
	_, ok := raw[key]
	return ok
}

// isSameFamilyThinkingVariantTarget 判断裸模型名 model 在 model_mapping 中的映射目标 mapped
// 是否属于同族默认思考档位（例如 gemini-4-argon -> gemini-4-argon-high、gemini-3.1-pro -> gemini-pro-agent
// 或 claude-opus-5-5 -> claude-opus-5-5-high）。
// 当用户在 credentials.model_mapping 中仅保留裸名声明（k==v 或指向默认 -high / gemini-pro-agent）
// 以便对外 /v1/models 收敛后缀时，仍允许按请求的 thinkingLevel 动态路由到对应档位。
func isSameFamilyThinkingVariantTarget(model, mapped string) bool {
	if mapped == "" || mapped == model {
		return true
	}
	for _, level := range []string{"high", "medium", "low"} {
		if mapped == model+"-"+level {
			return true
		}
	}
	if (model == "gemini-3.1-pro" || model == "gemini-3.1-pro-preview") && mapped == domain.AntigravityGemini31ProAgentModel {
		return true
	}
	return false
}

// resolveBuiltInGeminiThinkingVariant 在账号收敛了冗余 -high/-medium/-low 映射键、仅保留裸名键时，
// 为已知支持多档思考的 Gemini / Claude 5.5 裸模型直接解析出上游真实变体名。
func resolveBuiltInGeminiThinkingVariant(model, level string) (string, bool) {
	switch model {
	case "gemini-3.6-flash", "gemini-3.7-flash", "gemini-3.8-flash":
		switch level {
		case "low", "medium", "high", "tiered":
			return model + "-" + level, true
		}
	case "gemini-4-argon", "claude-opus-5-5", "claude-sonnet-5-5":
		switch level {
		case "low", "medium", "high":
			return model + "-" + level, true
		}
	case "gemini-3.1-pro", "gemini-3.1-pro-preview":
		switch level {
		case "low":
			return "gemini-3.1-pro-low", true
		case "medium", "high":
			return domain.AntigravityGemini31ProAgentModel, true
		}
	case "gemini-3-pro", "gemini-3-pro-preview":
		switch level {
		case "low":
			return "gemini-3-pro-low", true
		case "medium", "high":
			return "gemini-3-pro-high", true
		}
	case "gemini-3.5-flash":
		switch level {
		case "low", "medium", "high":
			return "gemini-3.5-flash-low", true
		}
	}
	return "", false
}

// splitAntigravityThinkingVariantRequest 将带思考档位后缀（-low/-medium/-high/-max/-tiered）的
// Antigravity 多档思考请求模型名拆解为 (裸模型名, 归一化档位, 是否命中)。
// 其中 -max 归一化为 "high"。
func splitAntigravityThinkingVariantRequest(requestedModel string) (baseModel string, level string, ok bool) {
	model := strings.TrimSpace(strings.TrimPrefix(requestedModel, "models/"))
	if model == "" {
		return "", "", false
	}
	for _, item := range []struct {
		suffix string
		level  string
	}{
		{suffix: "-low", level: "low"},
		{suffix: "-medium", level: "medium"},
		{suffix: "-high", level: "high"},
		{suffix: "-max", level: "high"},
		{suffix: "-tiered", level: "tiered"},
	} {
		if !strings.HasSuffix(model, item.suffix) {
			continue
		}
		base := strings.TrimSuffix(model, item.suffix)
		if _, builtInOK := resolveBuiltInGeminiThinkingVariant(base, item.level); builtInOK {
			return base, item.level, true
		}
		if (base == "claude-opus-4-6" || base == "claude-sonnet-4-6") && item.level != "tiered" {
			return base, item.level, true
		}
	}
	return "", "", false
}

// resolveAntigravitySuffixedVariantFromBaseMapping 在账号仅保留裸名映射键（如 claude-opus-5-5 或 gemini-4-argon）时，
// 允许客户端直接请求带 -low/-medium/-high/-max/-tiered 后缀的变体名并解析到对应的上游模型。
func resolveAntigravitySuffixedVariantFromBaseMapping(mapping map[string]string, requestedModel string) (string, bool) {
	if len(mapping) == 0 {
		return "", false
	}
	baseModel, level, ok := splitAntigravityThinkingVariantRequest(requestedModel)
	if !ok {
		return "", false
	}
	bareMapped, bareMatched := resolveRequestedModelInMapping(mapping, baseModel)
	if !bareMatched || strings.TrimSpace(bareMapped) == "" {
		return "", false
	}
	bareMapped = strings.TrimSpace(bareMapped)
	if (baseModel == "claude-opus-4-6" || baseModel == "claude-sonnet-4-6") && level != "tiered" {
		// 4-6 家族上游没有档位变体：后缀名仅用于准入（组白名单/调度认这个名字），
		// 落点固定为裸名映射目标，请求里的档位意图不改变转发结果（tiered 除外，走特例映射）。
		return bareMapped, true
	}
	if !isSameFamilyThinkingVariantTarget(baseModel, bareMapped) {
		return "", false
	}
	if (baseModel == "gemini-3.1-pro" || baseModel == "gemini-3.1-pro-preview") && level == "high" && bareMapped == "gemini-3.1-pro-high" {
		return "gemini-3.1-pro-high", true
	}
	return resolveBuiltInGeminiThinkingVariant(baseModel, level)
}

// antigravityRawMappingClaimsModel 判断 Antigravity 账号的原始 credentials.model_mapping
// 是否认领 requestedModel（支持显式键、裸名派生后缀变体、以及后缀变体派生裸名）。
func antigravityRawMappingClaimsModel(rawMapping map[string]string, requestedModel string) bool {
	model := strings.TrimSpace(strings.TrimPrefix(requestedModel, "models/"))
	if len(rawMapping) == 0 || model == "" {
		return false
	}
	if mapped, ok := rawMapping[model]; ok && strings.TrimSpace(mapped) != "" {
		return true
	}
	if _, ok := resolveAntigravitySuffixedVariantFromBaseMapping(rawMapping, model); ok {
		return true
	}
	if isAntigravityThinkingVariantFamily(model) && !hasGeminiThinkingVariantSuffix(model) {
		for _, level := range []string{"high", "medium", "low", "tiered"} {
			if mapped, ok := rawMapping[model+"-"+level]; ok && strings.TrimSpace(mapped) != "" {
				return true
			}
		}
	}
	return false
}

// resolveGeminiThinkingVariant 为裸 Gemini 模型名挑选账号映射表里存在的思考深度变体。
// 返回 (映射后的上游模型名, 是否命中)。未命中时调用方应回退到常规 getMappedModel 流程。
func resolveGeminiThinkingVariant(account *Account, requestedModel string, body []byte) (string, bool) {
	return resolveGeminiThinkingVariantForLevel(account, requestedModel, geminiThinkingLevelFromBody(body))
}

func isAntigravityThinkingVariantFamily(model string) bool {
	if strings.HasPrefix(model, "gemini-") {
		return true
	}
	return model == "claude-opus-5-5" || model == "claude-sonnet-5-5"
}

// resolveGeminiThinkingVariantForLevel 是 resolveGeminiThinkingVariant 的协议无关内核：
// 调用方负责按自身协议推导 preferred 档位（Gemini 原生读 generationConfig.thinkingConfig，
// Claude/OpenAI 兼容层读 output_config.effort / thinking.budget_tokens），此处只做映射查找与降级。
func resolveGeminiThinkingVariantForLevel(account *Account, requestedModel string, preferred string) (string, bool) {
	if account == nil {
		return "", false
	}
	model := strings.TrimSpace(strings.TrimPrefix(requestedModel, "models/"))
	if !isAntigravityThinkingVariantFamily(model) || hasGeminiThinkingVariantSuffix(model) {
		return "", false
	}
	mapping := account.GetModelMapping()
	if len(mapping) == 0 {
		return "", false
	}
	// 若裸名映射到了非同族自定义目标（如 gemini-3.8-flash -> gemini-3.8-flash-tiered 或跨模型重定向），
	// 尊重用户显式配置，不做思考档位覆盖。
	// 若裸名仅自映射（k==v）或指向同族默认单档（如 gemini-4-argon-high / gemini-pro-agent），
	// 则继续按 preferred 档位动态解析。
	bareMapped, bareMatched := resolveRequestedModelInMapping(mapping, model)
	if bareMatched {
		bareMapped = strings.TrimSpace(bareMapped)
		if !isSameFamilyThinkingVariantTarget(model, bareMapped) {
			return "", false
		}
	}

	if preferred == "" {
		preferred = "high"
	}
	order := []string{preferred}
	for _, level := range []string{"high", "medium", "low", "tiered"} {
		if level != preferred {
			order = append(order, level)
		}
	}
	for _, level := range order {
		candidate := model + "-" + level
		if mapped, matched := resolveRequestedModelInMapping(mapping, candidate); matched && strings.TrimSpace(mapped) != "" {
			mapped = strings.TrimSpace(mapped)
			// ensureAntigravityDefaultPassthroughs 会自动注入 "gemini-3.1-pro-high" -> "gemini-3.1-pro-high" 的自映射；
			// 当账号未配置 gemini-pro-agent 键但把 gemini-3.1-pro 映射到 gemini-pro-agent 时，
			// high/medium 档应优先走 gemini-pro-agent。
			if candidate == "gemini-3.1-pro-high" && mapped == "gemini-3.1-pro-high" && !accountRawModelMappingHasKey(account, candidate) {
				if bareMatched && bareMapped == domain.AntigravityGemini31ProAgentModel {
					return domain.AntigravityGemini31ProAgentModel, true
				}
			}
			return mapped, true
		}
	}
	if bareMatched {
		for _, level := range order {
			if builtIn, ok := resolveBuiltInGeminiThinkingVariant(model, level); ok {
				return builtIn, true
			}
		}
	}
	return "", false
}
