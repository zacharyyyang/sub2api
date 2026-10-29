package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 计价汇率：把美元标度的价格换算成人民币。
//
// 背景：内置价表（LiteLLM / fallbackPrices）是美元标度，国产渠道录入的是人民币价，
// 而余额的单位是人民币（充值倍率 1 元 = 1 单位）。所以美元价在使用前必须乘汇率，
// 否则等于把美元数字当人民币收。
//
// 生效值 = 开启自动更新且已抓到值时的实时值，否则用手动值。两个来源都不允许为 0：
// 汇率缺失会让所有美元价归零，等于免费卖。
const (
	SettingKeyUSDToCNYRate     = "USD_TO_CNY_RATE"
	SettingKeyUSDToCNYRateAuto = "USD_TO_CNY_RATE_AUTO"

	// NeutralUSDToCNYRate 表示「不换算」。设置尚未配置时用它：存量行为保持不变，
	// 等管理员显式设定汇率后才打开换算。凭空默认一个 7.2 会让升级瞬间改价。
	NeutralUSDToCNYRate = 1.0
	// SuggestedUSDToCNYRate 是管理端表单的预填值，仅作输入提示，不参与运行时逻辑。
	SuggestedUSDToCNYRate = 7.2

	usdToCNYRateEndpoint = "https://open.er-api.com/v6/latest/USD"
	usdToCNYRefreshEvery = 24 * time.Hour
)

// 币种取值。定价条目与渠道默认币种都用这两个值。
const (
	PriceCurrencyUSD = "USD"
	PriceCurrencyCNY = "CNY"
)

// NormalizePriceCurrency 把任意输入归一为 USD / CNY，空值或未知值按 USD。
func NormalizePriceCurrency(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case PriceCurrencyCNY:
		return PriceCurrencyCNY
	default:
		return PriceCurrencyUSD
	}
}

type fxRateState struct {
	mu         sync.RWMutex
	manual     float64   // 设置里的手动值
	configured bool      // 设置里是否已显式配置汇率
	auto       bool      // 是否允许自动更新
	live       float64   // 最近一次抓取成功的值
	liveAt     time.Time // 最近一次抓取成功的时间
	refresh    sync.Once
	lastErr    string
	lastErrAt  time.Time
}

var fxRate = &fxRateState{manual: NeutralUSDToCNYRate, auto: true}

// fxRateSettingsLoader 由 SettingService 注册：让汇率能在首次使用时自己从设置表加载，
// 而不是依赖某个页面先调过一次 parseSettings。
var (
	fxRateLoaderMu       sync.Mutex
	fxRateSettingsLoader func(ctx context.Context) (rate float64, configured, auto, ok bool)
	fxRateLoaded         bool
	fxRateLoadEverOK     bool          // 是否至少成功加载过一次（成功且「键不存在」也算）
	fxRateLoadInFlight   chan struct{} // 非 nil 表示正在加载，关闭即表示本轮加载结束
	lastLoadFailureAt    time.Time
)

// loadRetryCooldown 是加载失败后的重试间隔：失败不能永久放弃，也不能每次请求都打数据库。
const loadRetryCooldown = 30 * time.Second

// RegisterFXRateSettingsLoader 注册设置加载器（由 NewSettingService 调用，进程内只一份）。
func RegisterFXRateSettingsLoader(loader func(ctx context.Context) (float64, bool, bool, bool)) {
	fxRateLoaderMu.Lock()
	defer fxRateLoaderMu.Unlock()
	fxRateSettingsLoader = loader
}

// ensureLoaded 在汇率状态未知时同步加载一次设置。
// 并发语义：加载进行中的其他调用**等待同一个结果**再继续，不允许拿到未配置的中性汇率去计价。
// 失败语义：读取失败不记为「已加载」，过 loadRetryCooldown 后重试；每次都记 WARN，不静默。
func ensureLoaded() {
	for {
		fxRateLoaderMu.Lock()
		if fxRateLoaded {
			fxRateLoaderMu.Unlock()
			return
		}
		if fxRateLoadInFlight != nil {
			wait := fxRateLoadInFlight
			fxRateLoaderMu.Unlock()
			<-wait // 等同一次加载结束，再回环判断是否成功
			continue
		}
		if loader := fxRateSettingsLoader; loader == nil {
			fxRateLoaderMu.Unlock()
			return
		}
		if !lastLoadFailureAt.IsZero() && time.Since(lastLoadFailureAt) < loadRetryCooldown {
			fxRateLoaderMu.Unlock()
			return
		}
		done := make(chan struct{})
		fxRateLoadInFlight = done
		loader := fxRateSettingsLoader
		fxRateLoaderMu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		rate, configured, auto, ok := loader(ctx)
		cancel()
		if ok {
			ConfigureUSDToCNYRate(rate, configured, auto)
		}

		fxRateLoaderMu.Lock()
		if ok {
			fxRateLoaded = true
			fxRateLoadEverOK = true
			lastLoadFailureAt = time.Time{}
		} else {
			lastLoadFailureAt = time.Now()
		}
		fxRateLoadInFlight = nil
		fxRateLoaderMu.Unlock()
		close(done)

		if !ok {
			slog.Warn("rate.settings_load_failed",
				"note", "settings read failed; pricing keeps the last known rate and retries after cooldown")
		}
		return
	}
}

// ConfigureUSDToCNYRate 由设置解析路径调用，把手动值、是否已配置、自动开关灌进状态。
// 非法值（<=0 / NaN）不覆写现值：保留上一个有效汇率，绝不允许把汇率清成 0。
func ConfigureUSDToCNYRate(manual float64, configured, auto bool) {
	fxRate.mu.Lock()
	defer fxRate.mu.Unlock()
	if manual > 0 && !math.IsNaN(manual) && !math.IsInf(manual, 0) {
		fxRate.manual = manual
	}
	fxRate.configured = configured
	fxRate.auto = auto
}

// CurrentUSDToCNYRate 返回生效汇率（用于计费换算）。
// 未配置汇率时返回中性值 1（不换算）——自动抓取不得在管理员没设之前把换算打开。
// 首次调用会启动后台刷新协程：抓取是外部网络调用，不能阻塞计费热路径。
func CurrentUSDToCNYRate() float64 {
	ensureLoaded()
	fxRate.ensureRefresher()
	fxRate.mu.RLock()
	defer fxRate.mu.RUnlock()
	if !fxRate.configured {
		return NeutralUSDToCNYRate
	}
	if fxRate.auto && fxRate.live > 0 {
		return fxRate.live
	}
	return fxRate.manual
}

// ErrPricingRateUnavailable 表示「汇率状态未知」：设置从未成功读取过，
// 此时既不能当未配置按 1 计价（少收 6~7 倍），也不能凭空猜一个汇率。
var ErrPricingRateUnavailable = errors.New("pricing exchange rate unavailable")

// FXRateForBilling 是计费路径专用的取价入口：状态未知时返回错误，让调用方显式失败。
// 与 CurrentUSDToCNYRate 的区别：后者服务展示/测试，前者不允许在未知状态下静默按 1 计价。
func FXRateForBilling() (float64, error) {
	ensureLoaded()
	fxRate.mu.RLock()
	defer fxRate.mu.RUnlock()
	if fxRate.configured {
		if fxRate.auto && fxRate.live > 0 {
			return fxRate.live, nil
		}
		return fxRate.manual, nil
	}
	fxRateLoaderMu.Lock()
	loadedOnce := fxRateLoadEverOK
	hasLoader := fxRateSettingsLoader != nil
	fxRateLoaderMu.Unlock()
	if hasLoader && !loadedOnce {
		return 0, ErrPricingRateUnavailable
	}
	// 已成功确认过「没有配置」→ 按规则不换算。
	return NeutralUSDToCNYRate, nil
}

// LoadFXRateAtStartup 在进程启动时同步加载一次汇率设置。
// 读不到就返回错误，由 main 拒绝启动：这样「汇率状态未知」不会进入运行时，
// 任何计费入口（含图像/视频/语音/搜索这些没有错误通道的路径）都不会在未知状态下按中性 1 计价。
func LoadFXRateAtStartup(ctx context.Context) error {
	ensureLoaded()
	fxRateLoaderMu.Lock()
	loadedOnce := fxRateLoadEverOK
	hasLoader := fxRateSettingsLoader != nil
	fxRateLoaderMu.Unlock()
	if hasLoader && !loadedOnce {
		return ErrPricingRateUnavailable
	}
	return nil
}

// FXRateStatus 是给管理端展示的汇率状态。
type FXRateStatus struct {
	Rate           float64   `json:"rate"`
	Source         string    `json:"source"` // live | manual
	Auto           bool      `json:"auto"`
	Configured     bool      `json:"configured"`
	LiveRate       float64   `json:"live_rate"`
	LiveFetchedAt  time.Time `json:"live_fetched_at"`
	ManualRate     float64   `json:"manual_rate"`
	LastError      string    `json:"last_error,omitempty"`
	LastErrorAt    time.Time `json:"last_error_at,omitempty"`
	RefreshEveryMs int64     `json:"refresh_every_ms"`
}

// FXRateCurrentStatus 只读地取一次当前状态。
func FXRateCurrentStatus() FXRateStatus {
	fxRate.mu.RLock()
	defer fxRate.mu.RUnlock()
	st := FXRateStatus{
		Auto:           fxRate.auto,
		Configured:     fxRate.configured,
		LiveRate:       fxRate.live,
		LiveFetchedAt:  fxRate.liveAt,
		ManualRate:     fxRate.manual,
		LastError:      fxRate.lastErr,
		LastErrorAt:    fxRate.lastErrAt,
		RefreshEveryMs: usdToCNYRefreshEvery.Milliseconds(),
	}
	switch {
	case !fxRate.configured:
		// 与 CurrentUSDToCNYRate 同一规则：未配置就是中性值，不报告手动/实时值。
		st.Rate, st.Source = NeutralUSDToCNYRate, "neutral"
	case fxRate.auto && fxRate.live > 0:
		st.Rate, st.Source = fxRate.live, "live"
	default:
		st.Rate, st.Source = fxRate.manual, "manual"
	}
	return st
}

// RefreshUSDToCNYRateNow 立即抓一次（管理端"立即更新"与启动预热用）。
// 抓取失败不改动现值，只记录错误——汇率缺失时不允许把价格算成 0。
func RefreshUSDToCNYRateNow(ctx context.Context) error {
	return fxRate.refreshNow(ctx)
}

func (s *fxRateState) ensureRefresher() {
	s.refresh.Do(func() {
		go func() {
			// 启动时先抓一次，之后按「每日」周期刷新（usdToCNYRefreshEvery）。
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = s.refreshNow(ctx)
			cancel()
			ticker := time.NewTicker(usdToCNYRefreshEvery)
			defer ticker.Stop()
			for range ticker.C {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_ = s.refreshNow(ctx)
				cancel()
			}
		}()
	})
}

func (s *fxRateState) refreshNow(ctx context.Context) error {
	s.mu.RLock()
	enabled := s.auto && s.configured
	s.mu.RUnlock()
	if !enabled {
		return nil
	}

	rate, err := fetchUSDToCNYRate(ctx)
	if err != nil {
		s.mu.Lock()
		s.lastErr = err.Error()
		s.lastErrAt = time.Now()
		current := s.live
		s.mu.Unlock()
		slog.Warn("rate.fetch_failed",
			"source", usdToCNYRateEndpoint,
			"error", err,
			"kept_live_rate", current,
			"note", "keeping previous rate; prices must never fall back to 0")
		return err
	}

	s.mu.Lock()
	s.live = rate
	s.liveAt = time.Now()
	s.lastErr = ""
	s.mu.Unlock()
	slog.Info("rate.refreshed", "pair", "USD/CNY", "rate", rate)
	return nil
}

func fetchUSDToCNYRate(ctx context.Context) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usdToCNYRateEndpoint, nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("rate endpoint status %d", resp.StatusCode)
	}

	var payload struct {
		Result string             `json:"result"`
		Rates  map[string]float64 `json:"rates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, err
	}
	rate := payload.Rates["CNY"]
	if rate <= 0 {
		return 0, fmt.Errorf("rate endpoint returned no positive CNY rate (result=%q)", payload.Result)
	}
	return rate, nil
}

// convertModelPricingToCNY 原地把美元标度的价格乘汇率。
// 只动货币字段：倍率、阈值、标记位与"是否显式设置"这类开关不参与换算。
func convertModelPricingToCNY(p *ModelPricing, rate float64) {
	if p == nil || rate <= 0 || rate == 1 {
		return
	}
	p.InputPricePerToken *= rate
	p.InputPricePerTokenPriority *= rate
	p.ImageInputPricePerToken *= rate
	p.ImageCacheReadPricePerToken *= rate
	p.OutputPricePerToken *= rate
	p.OutputPricePerTokenPriority *= rate
	p.CacheCreationPricePerToken *= rate
	p.CacheCreationPricePerTokenPriority *= rate
	p.CacheReadPricePerToken *= rate
	p.CacheReadPricePerTokenPriority *= rate
	p.CacheCreation5mPrice *= rate
	p.CacheCreation1hPrice *= rate
	p.ImageOutputPricePerToken *= rate
}

// convertIntervalsToCNY 换算区分层价，返回新切片，不改原配置（配置被缓存复用）。
func convertIntervalsToCNY(in []PricingInterval, rate float64) []PricingInterval {
	if len(in) == 0 || rate <= 0 || rate == 1 {
		return in
	}
	out := make([]PricingInterval, len(in))
	copy(out, in)
	scale := func(v *float64) *float64 {
		if v == nil {
			return nil
		}
		scaled := *v * rate
		return &scaled
	}
	for i := range out {
		out[i].InputPrice = scale(out[i].InputPrice)
		out[i].OutputPrice = scale(out[i].OutputPrice)
		out[i].CacheWritePrice = scale(out[i].CacheWritePrice)
		out[i].CacheWrite1hPrice = scale(out[i].CacheWrite1hPrice)
		out[i].CacheReadPrice = scale(out[i].CacheReadPrice)
		out[i].PerRequestPrice = scale(out[i].PerRequestPrice)
	}
	return out
}

// channelPricingInCNY 返回一个按币种换算后的人民币价副本（不修改原配置）。
// 成本侧用：条目是 USD 时价格 ×汇率，是 CNY 时原样；
// 时段价、区间价、按次/图片价一并换算，倍率与开关字段不动。
func channelPricingInCNY(p *ChannelModelPricing, rate float64) *ChannelModelPricing {
	if p == nil {
		return nil
	}
	cloned := *p
	if NormalizePriceCurrency(p.PriceCurrency) == PriceCurrencyCNY || rate <= 0 {
		return &cloned
	}
	scale := func(v *float64) *float64 {
		if v == nil {
			return nil
		}
		scaled := *v * rate
		return &scaled
	}
	cloned.InputPrice = scale(p.InputPrice)
	cloned.OutputPrice = scale(p.OutputPrice)
	cloned.CacheWritePrice = scale(p.CacheWritePrice)
	cloned.CacheWrite1hPrice = scale(p.CacheWrite1hPrice)
	cloned.CacheReadPrice = scale(p.CacheReadPrice)
	cloned.ImageInputPrice = scale(p.ImageInputPrice)
	cloned.ImageOutputPrice = scale(p.ImageOutputPrice)
	cloned.PerRequestPrice = scale(p.PerRequestPrice)
	cloned.Intervals = convertIntervalsToCNY(p.Intervals, rate)
	// 金额已换成人民币，币种标识必须跟着走，否则调用方会把人民币数字当美元解读。
	cloned.PriceCurrency = PriceCurrencyCNY

	if p.StatsTimePricing != nil {
		periods := make([]AccountStatsTimePricingPeriod, len(p.StatsTimePricing.Periods))
		copy(periods, p.StatsTimePricing.Periods)
		for i := range periods {
			periods[i].InputPrice = scale(periods[i].InputPrice)
			periods[i].OutputPrice = scale(periods[i].OutputPrice)
			periods[i].CacheWritePrice = scale(periods[i].CacheWritePrice)
			periods[i].CacheWrite1hPrice = scale(periods[i].CacheWrite1hPrice)
			periods[i].CacheReadPrice = scale(periods[i].CacheReadPrice)
			periods[i].PerRequestPrice = scale(periods[i].PerRequestPrice)
		}
		cloned.StatsTimePricing = &AccountStatsTimePricing{
			Timezone: p.StatsTimePricing.Timezone,
			Periods:  periods,
		}
	}
	return &cloned
}

// PriceEntryInCNYForDisplay 返回按币种换算后的定价条目副本，供只读定价展示接口使用。
// 与计费使用同一个汇率与同一套币种规则，避免「界面价」与「实际扣费价」两张皮。
func PriceEntryInCNYForDisplay(p *ChannelModelPricing) *ChannelModelPricing {
	return channelPricingInCNY(p, CurrentUSDToCNYRate())
}

// USDPriceInCNYForDisplay 把美元标度的单价换算成人民币（官方参考价这类恒为美元的数据）。
func USDPriceInCNYForDisplay(v *float64) *float64 {
	if v == nil {
		return nil
	}
	rate := CurrentUSDToCNYRate()
	if rate <= 0 || rate == 1 {
		return v
	}
	scaled := *v * rate
	return &scaled
}

// ToCNYAmount 把美元标度的单价换算成人民币（用于没有币种字段的内置默认价）。
// 币种未配置时中性值为 1，等价于不换算。
func ToCNYAmount(usd float64) float64 {
	rate := CurrentUSDToCNYRate()
	if rate <= 0 {
		return usd
	}
	return usd * rate
}

// parseFXRateSetting 解析设置值。返回值：
//   - 空串：未配置 → 中性值 1.0 + configured=false（不换算）
//   - 合法正数：值 + configured=true
//   - 非法（0/负数/非数字）：0 + configured=true——不覆写现有汇率，也不把换算关掉
func parseFXRateSetting(raw string) (rate float64, configured bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return NeutralUSDToCNYRate, false
	}
	v, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, true
	}
	return v, true
}

// parseFXRateAutoSetting 容错解析布尔设置，缺省视为开启自动更新。
func parseFXRateAutoSetting(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// applyPricingCurrencyConversion 把解析结果统一换算成人民币。
//
// 币种判定：内置价表（LiteLLM / fallback）恒为美元；渠道与分组条目看自身声明，
// 未声明时按美元处理（与迁移默认值一致）。CNY 条目跳过换算——它们本来就是人民币。
func applyPricingCurrencyConversion(resolved *ResolvedPricing) {
	if resolved == nil || resolved.currencyNormalized {
		return
	}

	currency := PriceCurrencyUSD
	if resolved.channelPricing != nil {
		currency = NormalizePriceCurrency(resolved.channelPricing.PriceCurrency)
	}
	if resolved.Source == PricingSourceLiteLLM || resolved.Source == PricingSourceFallback {
		currency = PriceCurrencyUSD
	}
	if currency == PriceCurrencyCNY {
		// 理论上 CNY 条目已在合并阶段换算并置位；这里再兜一道，
		// 保证任何路径都不会把人民币价再乘一次汇率。
		return
	}

	rate := CurrentUSDToCNYRate()
	if rate <= 0 || rate == 1 {
		return
	}

	// BasePricing 可能是 fallbackPrices 里的共享指针，先克隆再乘，
	// 否则一次换算会把进程内的价表永久改成人民币价。
	if resolved.BasePricing != nil {
		cloned := *resolved.BasePricing
		convertModelPricingToCNY(&cloned, rate)
		resolved.BasePricing = &cloned
	}
	resolved.Intervals = convertIntervalsToCNY(resolved.Intervals, rate)
	resolved.RequestTiers = convertIntervalsToCNY(resolved.RequestTiers, rate)
	if resolved.DefaultPerRequestPrice > 0 {
		resolved.DefaultPerRequestPrice *= rate
	}

	// 区间计价时下游会用原始条目重新拿图片价（applyChannelImagePriceOverrides），
	// 这里同步换成人民币副本，否则图片价会把已换算的结果又改回美元数字。
	if resolved.channelPricing != nil {
		resolved.channelPricing = channelPricingInCNY(resolved.channelPricing, rate)
	}
}
