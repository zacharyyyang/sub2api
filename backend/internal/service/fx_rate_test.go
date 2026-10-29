package service

import (
	"math"
	"testing"
	"time"
)

func withRate(t *testing.T, rate float64, auto bool) {
	t.Helper()
	ConfigureUSDToCNYRate(rate, true, auto)
	t.Cleanup(func() { ConfigureUSDToCNYRate(1, false, false) }) // 回到测试基线：未配置、不换算
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-12
}

// AC-1：内置价表（LiteLLM / fallback）是美元标度，换算后应等于「美元价 × 汇率」。
func TestApplyPricingCurrencyConversion_FallbackMultipliesByRate(t *testing.T) {
	withRate(t, 7.2, false)

	resolved := &ResolvedPricing{
		Source: PricingSourceFallback,
		BasePricing: &ModelPricing{
			InputPricePerToken:     2.5e-6,
			OutputPricePerToken:    15e-6,
			CacheReadPricePerToken: 0.25e-6,
			FastMultiplier:         fxPtr(2),
		},
	}
	applyPricingCurrencyConversion(resolved)

	// $2.5/M → ¥18/M；$15/M → ¥108/M；$0.25/M → ¥1.8/M
	if !almostEqual(resolved.BasePricing.InputPricePerToken, 2.5e-6*7.2) {
		t.Fatalf("input: got %v, want %v", resolved.BasePricing.InputPricePerToken, 2.5e-6*7.2)
	}
	if !almostEqual(resolved.BasePricing.OutputPricePerToken, 15e-6*7.2) {
		t.Fatalf("output: got %v, want %v", resolved.BasePricing.OutputPricePerToken, 15e-6*7.2)
	}
	if !almostEqual(resolved.BasePricing.CacheReadPricePerToken, 0.25e-6*7.2) {
		t.Fatalf("cache read not converted: %v", resolved.BasePricing.CacheReadPricePerToken)
	}
	// AC-6：倍率不参与换算
	if resolved.BasePricing.FastMultiplier == nil || *resolved.BasePricing.FastMultiplier != 2 {
		t.Fatalf("fast multiplier must stay 2, got %v", resolved.BasePricing.FastMultiplier)
	}
}

// AC-2：条目声明 CNY 时不换算——0.4 元/百万 token 就是 0.4。
func TestApplyPricingCurrencyConversion_CNYEntryIsNotConverted(t *testing.T) {
	withRate(t, 7.2, false)

	resolved := &ResolvedPricing{
		Source:         PricingSourceChannel,
		BasePricing:    &ModelPricing{InputPricePerToken: 0.4e-6},
		channelPricing: &ChannelModelPricing{PriceCurrency: PriceCurrencyCNY},
	}
	applyPricingCurrencyConversion(resolved)

	if !almostEqual(resolved.BasePricing.InputPricePerToken, 0.4e-6) {
		t.Fatalf("CNY entry must keep 0.4/M, got %v", resolved.BasePricing.InputPricePerToken)
	}
}

// AC-3：条目默认 USD（未声明）时照样换算。
func TestApplyPricingCurrencyConversion_USDEntryMultipliesByRate(t *testing.T) {
	withRate(t, 7.2, false)

	resolved := &ResolvedPricing{
		Source:         PricingSourceChannel,
		BasePricing:    &ModelPricing{InputPricePerToken: 0.4e-6},
		channelPricing: &ChannelModelPricing{},
	}
	applyPricingCurrencyConversion(resolved)

	if !almostEqual(resolved.BasePricing.InputPricePerToken, 0.4e-6*7.2) {
		t.Fatalf("USD entry must be converted, got %v", resolved.BasePricing.InputPricePerToken)
	}
}

// 换算不得污染共享指针：fallbackPrices 在进程内被复用，就地改会永久改成人民币价。
func TestApplyPricingCurrencyConversion_DoesNotMutateSharedPricing(t *testing.T) {
	withRate(t, 7.2, false)

	shared := &ModelPricing{InputPricePerToken: 2.5e-6}
	resolved := &ResolvedPricing{Source: PricingSourceFallback, BasePricing: shared}
	applyPricingCurrencyConversion(resolved)

	if !almostEqual(shared.InputPricePerToken, 2.5e-6) {
		t.Fatalf("shared pricing mutated: %v", shared.InputPricePerToken)
	}
	if resolved.BasePricing == shared {
		t.Fatal("resolved pricing must be a clone")
	}
}

// 区间价与按次价也要换算；区间倍率不换算。
func TestApplyPricingCurrencyConversion_IntervalsAndRequestTiers(t *testing.T) {
	withRate(t, 7.2, false)

	multiplier := 0.5
	resolved := &ResolvedPricing{
		Source: PricingSourceFallback,
		Intervals: []PricingInterval{
			{InputPrice: fxPtr(1e-6), OutputPrice: fxPtr(2e-6), InputMultiplier: &multiplier},
		},
		RequestTiers:           []PricingInterval{{PerRequestPrice: fxPtr(0.01)}},
		DefaultPerRequestPrice: 0.02,
	}
	applyPricingCurrencyConversion(resolved)

	if !almostEqual(*resolved.Intervals[0].InputPrice, 1e-6*7.2) {
		t.Fatalf("interval input not converted: %v", *resolved.Intervals[0].InputPrice)
	}
	if !almostEqual(*resolved.Intervals[0].InputMultiplier, 0.5) {
		t.Fatalf("interval multiplier must stay 0.5, got %v", *resolved.Intervals[0].InputMultiplier)
	}
	if !almostEqual(*resolved.RequestTiers[0].PerRequestPrice, 0.01*7.2) {
		t.Fatalf("request tier price not converted: %v", *resolved.RequestTiers[0].PerRequestPrice)
	}
	if !almostEqual(resolved.DefaultPerRequestPrice, 0.02*7.2) {
		t.Fatalf("default per-request price not converted: %v", resolved.DefaultPerRequestPrice)
	}
}

// AC-5：汇率为 0 / NaN / 负数时不得覆写为非法值，必须保留上一个有效汇率。
func TestConfigureUSDToCNYRate_RejectsNonPositive(t *testing.T) {
	withRate(t, 7.2, false)

	ConfigureUSDToCNYRate(0, true, false)
	if got := CurrentUSDToCNYRate(); !almostEqual(got, 7.2) {
		t.Fatalf("zero must keep previous 7.2, got %v", got)
	}

	ConfigureUSDToCNYRate(math.NaN(), true, false)
	if got := CurrentUSDToCNYRate(); !almostEqual(got, 7.2) {
		t.Fatalf("NaN must keep previous 7.2, got %v", got)
	}

	ConfigureUSDToCNYRate(-1, true, false)
	if got := CurrentUSDToCNYRate(); got <= 0 {
		t.Fatalf("rate must stay positive, got %v", got)
	}
}

// 设置未配置时（空串）使用「不换算」的中性值，不应凭空打开换算；
// 未配置也不会被自动抓取打开。
func TestParseFXRateSetting(t *testing.T) {
	if rate, configured := parseFXRateSetting(""); configured || !almostEqual(rate, NeutralUSDToCNYRate) {
		t.Fatalf("empty setting must mean neutral+unconfigured, got rate=%v configured=%v", rate, configured)
	}
	if rate, configured := parseFXRateSetting("7.15"); !configured || !almostEqual(rate, 7.15) {
		t.Fatalf("valid setting not parsed, got rate=%v configured=%v", rate, configured)
	}
	if rate, configured := parseFXRateSetting("abc"); !configured || rate != 0 {
		t.Fatalf("invalid setting must keep configured and signal 0, got rate=%v configured=%v", rate, configured)
	}
	if rate, configured := parseFXRateSetting("0"); !configured || rate != 0 {
		t.Fatalf("zero setting must keep configured and signal 0, got rate=%v configured=%v", rate, configured)
	}
}

// 未配置汇率时，即便自动抓取开关打开，生效汇率仍是中性值（不换算）。
func TestCurrentUSDToCNYRate_UnconfiguredStaysNeutral(t *testing.T) {
	t.Cleanup(func() { ConfigureUSDToCNYRate(1, false, false) })
	ConfigureUSDToCNYRate(0, false, true)
	if got := CurrentUSDToCNYRate(); !almostEqual(got, NeutralUSDToCNYRate) {
		t.Fatalf("unconfigured rate must stay neutral, got %v", got)
	}
}

// 成本侧条目副本：CNY 原样、USD 换算，且原配置不被修改（时段价也覆盖）。
func TestChannelPricingInCNY(t *testing.T) {
	withRate(t, 7.2, false)

	cny := &ChannelModelPricing{
		PriceCurrency: PriceCurrencyCNY,
		InputPrice:    fxPtr(0.4e-6),
		StatsTimePricing: &AccountStatsTimePricing{
			Timezone: "Asia/Shanghai",
			Periods:  []AccountStatsTimePricingPeriod{{StartTime: "08:00:00", EndTime: "22:00:00", InputPrice: fxPtr(0.8e-6)}},
		},
	}
	gotCNY := channelPricingInCNY(cny, 7.2)
	if !almostEqual(*gotCNY.InputPrice, 0.4e-6) {
		t.Fatalf("CNY base price must stay, got %v", *gotCNY.InputPrice)
	}
	if !almostEqual(*gotCNY.StatsTimePricing.Periods[0].InputPrice, 0.8e-6) {
		t.Fatalf("CNY period price must stay, got %v", *gotCNY.StatsTimePricing.Periods[0].InputPrice)
	}

	usd := &ChannelModelPricing{
		InputPrice: fxPtr(0.4e-6),
		StatsTimePricing: &AccountStatsTimePricing{
			Timezone: "Asia/Shanghai",
			Periods:  []AccountStatsTimePricingPeriod{{StartTime: "08:00:00", EndTime: "22:00:00", InputPrice: fxPtr(0.8e-6)}},
		},
	}
	gotUSD := channelPricingInCNY(usd, 7.2)
	if !almostEqual(*gotUSD.InputPrice, 0.4e-6*7.2) {
		t.Fatalf("USD base price must be converted, got %v", *gotUSD.InputPrice)
	}
	if !almostEqual(*gotUSD.StatsTimePricing.Periods[0].InputPrice, 0.8e-6*7.2) {
		t.Fatalf("USD period price must be converted, got %v", *gotUSD.StatsTimePricing.Periods[0].InputPrice)
	}
	// 原配置不得被就地改写
	if !almostEqual(*usd.InputPrice, 0.4e-6) || !almostEqual(*usd.StatsTimePricing.Periods[0].InputPrice, 0.8e-6) {
		t.Fatal("original channel pricing must not be mutated")
	}
}

func fxPtr(v float64) *float64 { return &v }

// F1 回归：只填了输入价的人民币卡片，继承来的输出价必须换成人民币，不能是美元数字。

// F1 回归：只填了输入价的人民币卡片，继承来的输出价必须换成人民币，不能是美元数字。
func TestPartialCNYCard_ConvertsInheritedCataloguePrices(t *testing.T) {
	withRate(t, 7.2, false)

	r := &ModelPricingResolver{}
	resolved := &ResolvedPricing{
		Source:      PricingSourceChannel,
		BasePricing: &ModelPricing{InputPricePerToken: 2.5e-6, OutputPricePerToken: 15e-6},
	}
	entry := &ChannelModelPricing{PriceCurrency: PriceCurrencyCNY, InputPrice: fxPtr(0.4e-6)}
	r.applyTokenOverrides(entry, resolved)
	applyPricingCurrencyConversion(resolved)

	// 条目覆盖的输入价是人民币，原样保留
	if !almostEqual(resolved.BasePricing.InputPricePerToken, 0.4e-6) {
		t.Fatalf("CNY input must stay 0.4/M, got %v", resolved.BasePricing.InputPricePerToken)
	}
	// 条目没填的输出价继承目录价（美元），必须被换成人民币
	if !almostEqual(resolved.BasePricing.OutputPricePerToken, 15e-6*7.2) {
		t.Fatalf("inherited output must be converted, got %v", resolved.BasePricing.OutputPricePerToken)
	}
}

// F2 回归：区间计价时下游会拿原始条目重新读图片价，出口必须给出人民币副本。
func TestExitConversion_ConvertsChannelPricingForImageOverrides(t *testing.T) {
	withRate(t, 7.2, false)

	resolved := &ResolvedPricing{
		Source:         PricingSourceChannel,
		BasePricing:    &ModelPricing{InputPricePerToken: 1e-6},
		channelPricing: &ChannelModelPricing{ImageOutputPrice: fxPtr(0.04)},
	}
	applyPricingCurrencyConversion(resolved)

	if !almostEqual(*resolved.channelPricing.ImageOutputPrice, 0.04*7.2) {
		t.Fatalf("channel pricing copy must be converted, got %v", *resolved.channelPricing.ImageOutputPrice)
	}
}

// F4/F5 回归：不经过 Resolver 的按次/搜索/语音/图片默认价同样要过汇率。
func TestNonTokenDefaults_ConvertToCNY(t *testing.T) {
	withRate(t, 7.2, false)
	s := &BillingService{}

	if got := s.CalculateWebSearchCost(1, nil, 1).TotalCost; !almostEqual(got, 0.01*7.2) {
		t.Fatalf("web search default not converted: %v", got)
	}
	if got := s.CalculateSearchCost(1000, nil, 1).TotalCost; !almostEqual(got, 5.0*7.2) {
		t.Fatalf("search default not converted: %v", got)
	}
	if got := s.CalculateAudioCost("realtime", 1, nil, 1).TotalCost; !almostEqual(got, 0.05*7.2) {
		t.Fatalf("audio default not converted: %v", got)
	}
	if got := s.CalculateImageCost("unknown-image-model", "1K", 1, nil, 1).TotalCost; !almostEqual(got, 0.134*7.2) {
		t.Fatalf("image default not converted: %v", got)
	}
}

// F3 回归：DeepSeek 官方价常量必须跟着调用点的单位走——目录取价是美元，计费时是人民币。
// 写错会让价格被算成汇率平方（R²）。
func TestDeepSeekPolicyRespectsCurrencyContext(t *testing.T) {
	withRate(t, 7.2, false)
	s := &BillingService{}

	usd := s.applyModelSpecificPricingPolicyEx("deepseek-v4-flash", &ModelPricing{}, true, time.Time{}, false)
	if !almostEqual(usd.InputPricePerToken, deepseekFlashOffPeakInputPrice) {
		t.Fatalf("catalogue context must keep USD constants, got %v", usd.InputPricePerToken)
	}

	cny := s.applyModelSpecificPricingPolicyEx("deepseek-v4-flash", &ModelPricing{}, true, time.Time{}, true)
	if !almostEqual(cny.InputPricePerToken, deepseekFlashOffPeakInputPrice*7.2) {
		t.Fatalf("billing context must convert once, got %v", cny.InputPricePerToken)
	}
}
