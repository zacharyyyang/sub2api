package service

import (
	"strings"
	"time"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

// qoderAndWBAliasToCanonicalModel maps Qoder 14-tier aliases and WB short aliases
// to their canonical base model names for pricing resolution.
var qoderAndWBAliasToCanonicalModel = map[string]string{
	"sonus-fable5.1":     "claude-fable-5-1",
	"sonus":              "claude-fable-5-1",
	"smodel":             "claude-fable-5-1",
	"cantus-opus5.5":     "claude-opus-5-5",
	"cantus":             "claude-opus-5-5",
	"cmodel":             "claude-opus-5-5",
	"lupus-sonnet5.5":    "claude-sonnet-5-5",
	"lupus":              "claude-sonnet-5-5",
	"lmodel":             "claude-sonnet-5-5",
	"volans-haiku5.5":    "claude-haiku-5-5",
	"volans":             "claude-haiku-5-5",
	"vmodel":             "claude-haiku-5-5",
	"ultimate-gpt6.1sol": "gpt-6.1-sol",
	"ultimate":           "gpt-6.1-sol",
	"proxima-gpt6sol":    "gpt-6-sol",
	"proxima":            "gpt-6-sol",
	"auri-gpt6astra":     "gpt-6-astra",
	"auri":               "gpt-6-astra",
	"amodel":             "gpt-6-astra",
	"orion-gemini3.1":    "gemini-3.1-pro",
	"orion":              "gemini-3.1-pro",
	"vela-gemini3.8":     "gemini-3.8-flash",
	"vela":               "gemini-3.8-flash",
	"pavo-glm5.3":        "glm-5.3",
	"pavo":               "glm-5.3",
	"gmodel":             "glm-5.3",
	"phoenix-kimi3":      "kimi-k3",
	"phoenix":            "kimi-k3",
	"kmodel":             "kimi-k3",
	"qmodel":             "qwen3.8-max",
	"mmodel":             "minimax-m3",
	"lite":               "deepseek-v4-flash",
}

func (s *BillingService) initCNAndExtendedFallbackPrices() {
	// 1. Alibaba DashScope (Bailian) Qwen models
	s.fallbackPrices["qwen3.8-max"] = &ModelPricing{
		InputPricePerToken:         1.768842e-6,
		OutputPricePerToken:        5.306526e-6,
		CacheCreationPricePerToken: 2.211052e-6,
		CacheCreation5mPrice:       2.211052e-6,
		CacheReadPricePerToken:     0.221105e-6,
	}
	s.fallbackPrices["qwen3.8-flash"] = &ModelPricing{
		InputPricePerToken:         0.117923e-6,
		OutputPricePerToken:        0.397989e-6,
		CacheCreationPricePerToken: 0.147403e-6,
		CacheCreation5mPrice:       0.147403e-6,
		CacheReadPricePerToken:     0.014740e-6,
	}
	s.fallbackPrices["qwen3.7-max"] = &ModelPricing{
		InputPricePerToken:         1.768842e-6,
		OutputPricePerToken:        5.306526e-6,
		CacheCreationPricePerToken: 2.211052e-6,
		CacheCreation5mPrice:       2.211052e-6,
		CacheReadPricePerToken:     0.353768e-6,
	}
	s.fallbackPrices["qwen3.7-plus"] = &ModelPricing{
		InputPricePerToken:          0.294807e-6,
		OutputPricePerToken:         1.179228e-6,
		CacheReadPricePerToken:      0.058961e-6,
		LongContextInputThreshold:   256000,
		LongContextInputMultiplier:  3.0,
		LongContextOutputMultiplier: 3.0,
	}
	qwen36Plus := &ModelPricing{
		InputPricePerToken:          0.294807e-6,
		OutputPricePerToken:         1.768842e-6,
		CacheReadPricePerToken:      0.058961e-6,
		LongContextInputThreshold:   256000,
		LongContextInputMultiplier:  4.0,
		LongContextOutputMultiplier: 2.0,
	}
	s.fallbackPrices["qwen3.6-plus"] = qwen36Plus
	s.fallbackPrices["qwen3.6-plus-2026-04-02"] = qwen36Plus
	qwen35Plus := &ModelPricing{
		InputPricePerToken:     0.117923e-6,
		OutputPricePerToken:    0.707537e-6,
		CacheReadPricePerToken: 0.011792e-6,
	}
	s.fallbackPrices["qwen3.5-plus"] = qwen35Plus
	s.fallbackPrices["qwen3.5-plus-2026-02-15"] = qwen35Plus
	qwen35Flash := &ModelPricing{
		InputPricePerToken:     0.029481e-6,
		OutputPricePerToken:    0.294807e-6,
		CacheReadPricePerToken: 0.002948e-6,
	}
	s.fallbackPrices["qwen3.5-flash"] = qwen35Flash
	s.fallbackPrices["qwen3.5-flash-2026-02-23"] = qwen35Flash
	qwenMax := &ModelPricing{
		InputPricePerToken:     0.368509e-6,
		OutputPricePerToken:    1.474035e-6,
		CacheReadPricePerToken: 0.073702e-6,
	}
	for _, k := range []string{"qwen-max", "qwen3-max", "qwen3-max-2026-01-23", "qwen3-max-preview"} {
		s.fallbackPrices[k] = qwenMax
	}
	s.fallbackPrices["qwen-plus"] = &ModelPricing{
		InputPricePerToken:  0.117923e-6,
		OutputPricePerToken: 0.294807e-6,
	}
	qwenFlash := &ModelPricing{
		InputPricePerToken:  0.044221e-6,
		OutputPricePerToken: 0.088442e-6,
	}
	s.fallbackPrices["qwen-flash"] = qwenFlash
	s.fallbackPrices["qwen-turbo"] = qwenFlash
	s.fallbackPrices["qwen-long"] = &ModelPricing{
		InputPricePerToken:  0.073702e-6,
		OutputPricePerToken: 0.294807e-6,
	}
	qwenCoderPlus := &ModelPricing{
		InputPricePerToken:     0.589614e-6,
		OutputPricePerToken:    2.358456e-6,
		CacheReadPricePerToken: 0.058961e-6,
	}
	for _, k := range []string{"qwen3-coder-plus", "qwen3-coder-plus-2025-09-23", "qwen3-coder-plus-2025-07-22"} {
		s.fallbackPrices[k] = qwenCoderPlus
	}
	qwenCoderFlash := &ModelPricing{
		InputPricePerToken:     0.147403e-6,
		OutputPricePerToken:    0.589614e-6,
		CacheReadPricePerToken: 0.014740e-6,
	}
	for _, k := range []string{"qwen3-coder-next", "qwen3-coder-flash", "qwen3-coder-flash-2025-07-28"} {
		s.fallbackPrices[k] = qwenCoderFlash
	}
	qwenVLPlus := &ModelPricing{
		InputPricePerToken:  0.147403e-6,
		OutputPricePerToken: 1.474035e-6,
	}
	for _, k := range []string{"qwen3-vl-plus", "qwen3-vl-plus-2025-12-19", "qwen3-vl-plus-2025-09-23"} {
		s.fallbackPrices[k] = qwenVLPlus
	}
	qwenVLFlash := &ModelPricing{
		InputPricePerToken:  0.044221e-6,
		OutputPricePerToken: 0.442210e-6,
	}
	for _, k := range []string{"qwen3-vl-flash", "qwen3-vl-flash-2026-01-22", "qwen3-vl-flash-2025-10-15"} {
		s.fallbackPrices[k] = qwenVLFlash
	}

	// 2. Zhipu GLM
	s.fallbackPrices["glm-5.3-flashx"] = &ModelPricing{
		InputPricePerToken:     0.295e-6,
		OutputPricePerToken:    1.032e-6,
		CacheReadPricePerToken: 0.084e-6,
	}

	// 3. Tencent HunYuan
	s.fallbackPrices["hy4-preview"] = &ModelPricing{
		InputPricePerToken:     0.834e-6,
		OutputPricePerToken:    2.501e-6,
		CacheReadPricePerToken: 0.042e-6,
	}
	s.fallbackPrices["hy3"] = &ModelPricing{
		InputPricePerToken:     0.132e-6,
		OutputPricePerToken:    0.528e-6,
		CacheReadPricePerToken: 0.033e-6,
	}

	// 4. Moonshot Kimi
	s.fallbackPrices["kimi-k2.8-preview"] = &ModelPricing{
		InputPricePerToken:     0.95e-6,
		OutputPricePerToken:    4.00e-6,
		CacheReadPricePerToken: 0.25e-6,
	}
	s.fallbackPrices["kimi-k2.7-code"] = &ModelPricing{
		InputPricePerToken:     0.95e-6,
		OutputPricePerToken:    4.00e-6,
		CacheReadPricePerToken: 0.19e-6,
	}
	kimiHighspeed := &ModelPricing{
		InputPricePerToken:     1.90e-6,
		OutputPricePerToken:    8.00e-6,
		CacheReadPricePerToken: 0.38e-6,
	}
	s.fallbackPrices["kimi-k2.7-code-highspeed"] = kimiHighspeed
	s.fallbackPrices["kimi-for-coding-highspeed"] = kimiHighspeed
	if k3, ok := s.fallbackPrices["kimi-k3"]; ok && k3 != nil {
		k3.CacheCreationPricePerToken = 3e-6
		k3.CacheCreationPriceExplicit = true
		k3.CacheCreation5mPrice = 3e-6
		k3.CacheCreation1hPrice = 6e-6
	}

	// 5. Baidu Qianfan & DeepSeek special models
	s.fallbackPrices["deepseek-ocr"] = &ModelPricing{
		InputPricePerToken:  0.044221e-6,
		OutputPricePerToken: 0.176884e-6,
	}
	s.fallbackPrices["deepseek-v4-flash-0731"] = &ModelPricing{
		InputPricePerToken:     0.221105e-6,
		OutputPricePerToken:    0.663316e-6,
		CacheReadPricePerToken: 0.022111e-6,
	}
	s.fallbackPrices["ernie-5.1"] = &ModelPricing{
		InputPricePerToken:          0.589614e-6,
		OutputPricePerToken:         2.653263e-6,
		LongContextInputThreshold:   32000,
		LongContextInputMultiplier:  1.5,
		LongContextOutputMultiplier: 1.333333,
	}
	ernie50 := &ModelPricing{
		InputPricePerToken:          0.884421e-6,
		OutputPricePerToken:         3.537684e-6,
		LongContextInputThreshold:   32000,
		LongContextInputMultiplier:  1.666667,
		LongContextOutputMultiplier: 1.666667,
	}
	s.fallbackPrices["ernie-5.0"] = ernie50
	s.fallbackPrices["ernie-5.0-thinking-preview"] = ernie50
	ernie45Turbo := &ModelPricing{
		InputPricePerToken:     0.117923e-6,
		OutputPricePerToken:    0.471691e-6,
		CacheReadPricePerToken: 0.029481e-6,
	}
	for _, k := range []string{"ernie-4.5-turbo", "ernie-4.5-turbo-128k", "ernie-4.5-turbo-32k", "ernie-4.5-turbo-latest", "ernie-4.5-turbo-vl"} {
		s.fallbackPrices[k] = ernie45Turbo
	}

	// 6. Xiaomi MiMo, MiniMax & Gemini 3.5-flash
	mimoPro := &ModelPricing{
		InputPricePerToken:          0.435e-6,
		OutputPricePerToken:         0.87e-6,
		CacheReadPricePerToken:      0.0036e-6,
		LongContextInputThreshold:   256000,
		LongContextInputMultiplier:  2.0,
		LongContextOutputMultiplier: 2.0,
	}
	s.fallbackPrices["mimo-v2.6-pro"] = mimoPro
	s.fallbackPrices["mimo-v2.5-pro"] = mimoPro
	mimoFlash := &ModelPricing{
		InputPricePerToken:     0.14e-6,
		OutputPricePerToken:    0.28e-6,
		CacheReadPricePerToken: 0.0028e-6,
	}
	s.fallbackPrices["mimo-v2.6-flash"] = mimoFlash
	s.fallbackPrices["mimo-v2.5"] = mimoFlash
	mimoUltraspeed := &ModelPricing{
		InputPricePerToken:     4.35e-6,
		OutputPricePerToken:    8.70e-6,
		CacheReadPricePerToken: 0.036e-6,
	}
	for _, k := range []string{"mimo-v2.6-pro-ultraspeed", "mimo-v2.6-ultraspeed", "ultraspeed"} {
		s.fallbackPrices[k] = mimoUltraspeed
	}
	if m3, ok := s.fallbackPrices["minimax-m3"]; ok && m3 != nil {
		m3.LongContextInputThreshold = 512000
		m3.LongContextInputMultiplier = 2.0
		m3.LongContextOutputMultiplier = 2.0
	}
	s.fallbackPrices["gemini-3.5-flash"] = &ModelPricing{
		InputPricePerToken:     0.10e-6,
		OutputPricePerToken:    0.40e-6,
		CacheReadPricePerToken: 0.025e-6,
	}
}

func hasFamilyPrefix(model, prefix string) bool {
	if model == prefix {
		return true
	}
	if !strings.HasPrefix(model, prefix) {
		return false
	}
	sep := model[len(prefix)]
	return sep == '-' || sep == '_' || sep == '.' || sep == ':'
}

func (s *BillingService) getCNAndExtendedFallbackPricing(model string) *ModelPricing {
	modelLower := strings.ToLower(strings.TrimSpace(model))
	if modelLower == "" {
		return nil
	}
	strippedPath := modelLower
	if idx := strings.LastIndex(strippedPath, "/"); idx >= 0 {
		strippedPath = strings.TrimSpace(strippedPath[idx+1:])
	}

	candidates := []string{modelLower, strippedPath}
	if mapped, ok := qoderAndWBAliasToCanonicalModel[strippedPath]; ok {
		candidates = append(candidates, mapped)
	}
	if strings.HasSuffix(strippedPath, "-thinking") {
		withoutThinking := strings.TrimSuffix(strippedPath, "-thinking")
		if withoutThinking != "" {
			candidates = append(candidates, withoutThinking)
			if mapped, ok := qoderAndWBAliasToCanonicalModel[withoutThinking]; ok {
				candidates = append(candidates, mapped)
			}
		}
	}

	for _, c := range candidates {
		if price, ok := s.fallbackPrices[c]; ok {
			return price
		}
		if normalized := normalizeGeminiThinkingTierAlias(c); normalized != c {
			if price, ok := s.fallbackPrices[normalized]; ok {
				return price
			}
		}
		if normalized := normalizeAntigravityClaudeTierAlias(c); normalized != c {
			if price, ok := s.fallbackPrices[normalized]; ok {
				return price
			}
		}
	}

	// Prefix/family matching with longest-prefix priority on the stripped model segment
	check := candidates[len(candidates)-1]
	switch {
	case hasFamilyPrefix(check, "deepseek-ocr"):
		return s.fallbackPrices["deepseek-ocr"]
	case hasFamilyPrefix(check, "deepseek-v4-flash-0731"):
		return s.fallbackPrices["deepseek-v4-flash-0731"]
	case hasFamilyPrefix(check, "glm-5.3-flashx"):
		return s.fallbackPrices["glm-5.3-flashx"]
	case hasFamilyPrefix(check, "kimi-k2.7-code-highspeed"), hasFamilyPrefix(check, "kimi-for-coding-highspeed"):
		return s.fallbackPrices["kimi-k2.7-code-highspeed"]
	case hasFamilyPrefix(check, "kimi-k2.7-code"):
		return s.fallbackPrices["kimi-k2.7-code"]
	case hasFamilyPrefix(check, "kimi-k2.8-preview"):
		return s.fallbackPrices["kimi-k2.8-preview"]
	case hasFamilyPrefix(check, "hy4-preview"):
		return s.fallbackPrices["hy4-preview"]
	case hasFamilyPrefix(check, "hy3"):
		return s.fallbackPrices["hy3"]
	case hasFamilyPrefix(check, "qwen3.8-max"):
		return s.fallbackPrices["qwen3.8-max"]
	case hasFamilyPrefix(check, "qwen3.8-flash"):
		return s.fallbackPrices["qwen3.8-flash"]
	case hasFamilyPrefix(check, "qwen3.7-max"):
		return s.fallbackPrices["qwen3.7-max"]
	case hasFamilyPrefix(check, "qwen3.7-plus"):
		return s.fallbackPrices["qwen3.7-plus"]
	case hasFamilyPrefix(check, "qwen3.6-plus"):
		return s.fallbackPrices["qwen3.6-plus"]
	case hasFamilyPrefix(check, "qwen3.5-plus"):
		return s.fallbackPrices["qwen3.5-plus"]
	case hasFamilyPrefix(check, "qwen3.5-flash"):
		return s.fallbackPrices["qwen3.5-flash"]
	case hasFamilyPrefix(check, "qwen3-coder-plus"):
		return s.fallbackPrices["qwen3-coder-plus"]
	case hasFamilyPrefix(check, "qwen3-coder-next"), hasFamilyPrefix(check, "qwen3-coder-flash"):
		return s.fallbackPrices["qwen3-coder-flash"]
	case hasFamilyPrefix(check, "qwen3-vl-plus"):
		return s.fallbackPrices["qwen3-vl-plus"]
	case hasFamilyPrefix(check, "qwen3-vl-flash"):
		return s.fallbackPrices["qwen3-vl-flash"]
	case hasFamilyPrefix(check, "qwen3-max"), hasFamilyPrefix(check, "qwen-max"):
		return s.fallbackPrices["qwen-max"]
	case hasFamilyPrefix(check, "qwen-plus"):
		return s.fallbackPrices["qwen-plus"]
	case hasFamilyPrefix(check, "qwen-flash"), hasFamilyPrefix(check, "qwen-turbo"):
		return s.fallbackPrices["qwen-flash"]
	case hasFamilyPrefix(check, "qwen-long"):
		return s.fallbackPrices["qwen-long"]
	case hasFamilyPrefix(check, "ernie-5.0-thinking-preview"), hasFamilyPrefix(check, "ernie-5.0"):
		return s.fallbackPrices["ernie-5.0"]
	case hasFamilyPrefix(check, "ernie-5.1"):
		return s.fallbackPrices["ernie-5.1"]
	case hasFamilyPrefix(check, "ernie-4.5-turbo"):
		return s.fallbackPrices["ernie-4.5-turbo"]
	case hasFamilyPrefix(check, "mimo-v2.6-pro-ultraspeed"), hasFamilyPrefix(check, "mimo-v2.6-ultraspeed"), check == "ultraspeed":
		return s.fallbackPrices["mimo-v2.6-pro-ultraspeed"]
	case hasFamilyPrefix(check, "mimo-v2.6-pro"), hasFamilyPrefix(check, "mimo-v2.5-pro"):
		return s.fallbackPrices["mimo-v2.6-pro"]
	case hasFamilyPrefix(check, "mimo-v2.6-flash"), hasFamilyPrefix(check, "mimo-v2.5"):
		return s.fallbackPrices["mimo-v2.6-flash"]
	case (hasFamilyPrefix(check, "gemini-3.5-flash") || hasFamilyPrefix(check, "gemini-3-5-flash")) &&
		!strings.Contains(check, "image") && !strings.Contains(check, "lite"):
		return s.fallbackPrices["gemini-3.5-flash"]
	}
	return nil
}

// isDashScopeOrQianfanGroup reports whether the group represents an Alibaba DashScope (Bailian)
// or Baidu Qianfan channel where DeepSeek 08:00-22:00 daily 2x peak-hour pricing applies.
func isDashScopeOrQianfanGroup(group *Group) bool {
	if group == nil {
		return false
	}
	stripSpaces := func(s string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, strings.ToLower(s))
	}
	platform := stripSpaces(group.Platform)
	name := stripSpaces(group.Name)
	for _, kw := range []string{"dashscope", "qianfan", "阿里云", "百度千帆", "千帆"} {
		if (platform != "" && strings.Contains(platform, kw)) ||
			(name != "" && strings.Contains(name, kw)) {
			return true
		}
	}
	return false
}

// deepseekPeakMultiplierForGroupAt returns the DeepSeek peak/off-peak multiplier at t:
//   - DashScope/Qianfan group: 2.0 during Beijing time (UTC+8) 08:00 <= t < 22:00 daily (incl. weekends), else 1.0
//   - Official DeepSeek group (or nil group): delegates to deepseekPeakMultiplierAt(t)
func deepseekPeakMultiplierForGroupAt(group *Group, t time.Time) float64 {
	if t.IsZero() {
		t = timezone.Now()
	}
	if isDashScopeOrQianfanGroup(group) {
		h := t.UTC().Hour()
		if h >= 0 && h < 14 {
			return 2.0
		}
		return 1.0
	}
	return deepseekPeakMultiplierAt(t)
}
