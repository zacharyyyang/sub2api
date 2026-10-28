package service

import (
	"fmt"
	"sort"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// parsedAccountStatsTimePeriod 是时段校验与选段共用的已解析时段。
// second 语义与售价侧分时解析一致：每日重复、秒粒度、左闭右开。
type parsedAccountStatsTimePeriod struct {
	start  int
	end    int
	period AccountStatsTimePricingPeriod
}

// accountStatsTimePricingError 表示账号统计时段价解析失败（时区名或时段文本损坏）。
// 解析失败即失败关闭：本次成本不计算，由调用点记日志并让 AccountStatsCost 保持 nil，
// 不回落一个看起来正确的基础价。
type accountStatsTimePricingError struct {
	PricingID int64
	Platform  string
	Models    []string
	Reason    string
}

func (e *accountStatsTimePricingError) Error() string {
	return fmt.Sprintf("pricing_id=%d platform=%q models=%v: %s", e.PricingID, e.Platform, e.Models, e.Reason)
}

// validateAccountStatsTimePricing 校验账号统计成本的时段独立价。
// 必填项按所属条目的计费模式判定，因此入口收整个条目而不是只收时段配置：
// token（空模式按 token）要求输入/输出/缓存读取价；按次类模式要求按次价。
// 未给出的价字段继承条目基础值，不为负。空 periods 视作未启用，字段置 nil。
func validateAccountStatsTimePricing(pricing *ChannelModelPricing) error {
	if pricing == nil || pricing.StatsTimePricing == nil {
		return nil
	}
	config := pricing.StatsTimePricing
	if len(config.Periods) == 0 {
		pricing.StatsTimePricing = nil
		return nil
	}
	if _, err := loadChannelTimePricingLocation(config.Timezone); err != nil {
		return invalidAccountStatsTimePricing(pricing, "timezone: %v", err)
	}
	if _, err := parseAccountStatsTimePeriods(config.Periods); err != nil {
		return invalidAccountStatsTimePricing(pricing, "%v", err)
	}
	perRequestMode := pricing.BillingMode == BillingModePerRequest ||
		pricing.BillingMode == BillingModeImage ||
		pricing.BillingMode == BillingModeVideo
	for i := range config.Periods {
		period := &config.Periods[i]
		label := fmt.Sprintf("period %d (%s-%s)", i+1, period.StartTime, period.EndTime)
		if perRequestMode {
			if period.PerRequestPrice == nil {
				return invalidAccountStatsTimePricing(pricing,
					"%s: per_request_price is required for %q billing mode", label, pricing.BillingMode)
			}
		} else if period.InputPrice == nil || period.OutputPrice == nil || period.CacheReadPrice == nil {
			return invalidAccountStatsTimePricing(pricing,
				"%s: input_price, output_price and cache_read_price are required", label)
		}
		for _, price := range []struct {
			field string
			value *float64
		}{
			{"input_price", period.InputPrice},
			{"output_price", period.OutputPrice},
			{"cache_write_price", period.CacheWritePrice},
			{"cache_write_1h_price", period.CacheWrite1hPrice},
			{"cache_read_price", period.CacheReadPrice},
			{"per_request_price", period.PerRequestPrice},
		} {
			if price.value != nil && *price.value < 0 {
				return invalidAccountStatsTimePricing(pricing, "%s: %s must be >= 0", label, price.field)
			}
		}
	}
	return nil
}

func invalidAccountStatsTimePricing(pricing *ChannelModelPricing, format string, args ...any) error {
	return infraerrors.BadRequest("INVALID_ACCOUNT_STATS_TIME_PRICING", fmt.Sprintf(
		"invalid account stats time pricing for platform '%s' models %v: %v",
		pricing.Platform, pricing.Models, fmt.Sprintf(format, args...)))
}

// parseAccountStatsTimePeriods 解析并校验时段：起止格式、start < end、同条目内不重叠。
// 时刻文本逐字复用售价侧 parseChannelTime，保证全项目只有一种切分语义。
func parseAccountStatsTimePeriods(periods []AccountStatsTimePricingPeriod) ([]parsedAccountStatsTimePeriod, error) {
	parsed := make([]parsedAccountStatsTimePeriod, 0, len(periods))
	for i := range periods {
		period := periods[i]
		start, err := parseChannelTime(period.StartTime, false)
		if err != nil {
			return nil, fmt.Errorf("period %d: %w", i+1, err)
		}
		end, err := parseChannelTime(period.EndTime, true)
		if err != nil {
			return nil, fmt.Errorf("period %d: %w", i+1, err)
		}
		if period.StartTime == period.EndTime || start >= end {
			return nil, fmt.Errorf("period %d: start time must be before end time", i+1)
		}
		parsed = append(parsed, parsedAccountStatsTimePeriod{start: start, end: end, period: period})
	}
	sort.Slice(parsed, func(i, j int) bool {
		return parsed[i].start < parsed[j].start
	})
	for i := 1; i < len(parsed); i++ {
		if parsed[i].start < parsed[i-1].end {
			return nil, fmt.Errorf("account stats time pricing periods overlap")
		}
	}
	return parsed, nil
}

// PriceAt 返回 at 在配置时区下命中的时段价；未配置或落在空档返回 nil。
// 时区或时段文本解析失败返回错误——失败关闭，调用方不得把它当作「未命中」处理。
func (config *AccountStatsTimePricing) PriceAt(at time.Time) (*AccountStatsTimePricingPeriod, error) {
	if config == nil || len(config.Periods) == 0 {
		return nil, nil
	}
	location, err := loadChannelTimePricingLocation(config.Timezone)
	if err != nil {
		return nil, fmt.Errorf("timezone: %w", err)
	}
	periods, err := parseAccountStatsTimePeriods(config.Periods)
	if err != nil {
		return nil, err
	}
	local := at.In(location)
	second := local.Hour()*60*60 + local.Minute()*60 + local.Second()
	for i := range periods {
		if second >= periods[i].start && second < periods[i].end {
			return &periods[i].period, nil
		}
	}
	return nil, nil
}
