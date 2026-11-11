import type { BillingMode, ChannelTimePricing, AccountStatsTimePricing, PricingInterval, PriceCurrency } from '@/api/admin/channels'
import { REASONING_EFFORT_LEVELS } from '@/constants/channel'

type TranslateFn = (key: string, params?: Record<string, unknown>) => string

export interface IntervalFormEntry {
  min_tokens: number
  max_tokens: number | null
  tier_label: string
  input_price: number | string | null
  output_price: number | string | null
  cache_write_price: number | string | null
  cache_write_1h_price?: number | string | null
  cache_write_12h_price?: number | string | null
  cache_read_price: number | string | null
  input_multiplier: number | string | null
  output_multiplier: number | string | null
  cache_write_multiplier: number | string | null
  cache_read_multiplier: number | string | null
  per_request_price: number | string | null
  sort_order: number
}

export interface PricingFormEntry {
  models: string[]
  billing_mode: BillingMode
  input_price: number | string | null
  output_price: number | string | null
  cache_write_price: number | string | null
  cache_write_1h_price?: number | string | null
  cache_write_12h_price?: number | string | null
  cache_read_price: number | string | null
  fast_multiplier?: number | string | null
  flex_multiplier?: number | string | null
  reasoning_effort_multipliers?: Record<string, number | string> | null
  image_input_price: number | string | null
  image_output_price: number | string | null
  per_request_price: number | string | null
  /** 条目计价币种：USD（默认，用前乘汇率）/ CNY（原样）。 */
  price_currency?: PriceCurrency
  intervals: IntervalFormEntry[]
  time_pricing: TimePricingFormEntry
  /** 仅账号统计（成本）条目使用：分时独立价。售价侧条目不带此字段。 */
  stats_time_pricing?: StatsTimePricingFormEntry
}

export interface TimePricingPeriodFormEntry {
  start_time: string
  end_time: string
  multiplier: number | string
}

export interface TimePricingFormEntry {
  timezone: string
  weekdays_only: boolean
  periods: TimePricingPeriodFormEntry[]
}

export const DEFAULT_TIME_PRICING_TIMEZONE = 'Asia/Shanghai'

const CLOCK_TIME = /^(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d$/
const LEGACY_CLOCK_TIME = /^(?:[01]\d|2[0-3]):[0-5]\d$/
const TWO_DECIMAL_MULTIPLIER = /^\d+(?:\.\d{1,2})?$/

export function isValidTimePricingMultiplier(value: number | string): boolean {
  const multiplier = String(value)
  const numericValue = Number(multiplier)
  return TWO_DECIMAL_MULTIPLIER.test(multiplier) &&
    Number.isFinite(numericValue) && numericValue > 0
}

export const COMMON_TIMEZONES = [
  'UTC', 'Asia/Shanghai', 'Asia/Tokyo', 'Asia/Seoul', 'Asia/Singapore', 'Asia/Kolkata',
  'Australia/Sydney', 'Europe/London', 'Europe/Paris', 'Europe/Berlin',
  'America/New_York', 'America/Chicago', 'America/Denver', 'America/Los_Angeles',
  'America/Toronto', 'America/Sao_Paulo', 'Pacific/Auckland', 'Pacific/Honolulu',
]

export function createDefaultTimePricingForm(): TimePricingFormEntry {
  return { timezone: DEFAULT_TIME_PRICING_TIMEZONE, weekdays_only: false, periods: [] }
}

export function apiTimePricingToForm(value: ChannelTimePricing | null | undefined): TimePricingFormEntry {
  if (!value) return createDefaultTimePricingForm()
  return {
    timezone: value.timezone || DEFAULT_TIME_PRICING_TIMEZONE,
    weekdays_only: value.weekdays_only === true,
    periods: (value.periods || []).map(period => ({
      start_time: LEGACY_CLOCK_TIME.test(period.start_time) ? `${period.start_time}:00` : period.start_time,
      end_time: LEGACY_CLOCK_TIME.test(period.end_time) ? `${period.end_time}:00` : period.end_time,
      multiplier: Number(period.multiplier).toFixed(2),
    })),
  }
}

export function formTimePricingToAPI(value: TimePricingFormEntry | null | undefined): ChannelTimePricing | null {
  if (!value?.periods?.length) return null
  const timezone = typeof value.timezone === 'string' ? value.timezone.trim() : ''
  return {
    timezone,
    weekdays_only: value.weekdays_only === true,
    periods: value.periods.map(period => ({
      start_time: period.start_time,
      end_time: period.end_time,
      multiplier: Number(period.multiplier),
    })),
  }
}

function timeToSeconds(time: string, isEnd: boolean): number {
  if (isEnd && time === '00:00:00') return 24 * 60 * 60
  const [hours, minutes, seconds] = time.split(':').map(Number)
  return hours * 60 * 60 + minutes * 60 + seconds
}

function timePricingValidationMessage(t: TranslateFn, key: string): string {
  return t(`admin.channels.timePricingValidation.${key}`)
}

export function validateTimePricing(value: TimePricingFormEntry, t: TranslateFn): string | null {
  if (!value?.periods?.length) return null

  if (typeof value.timezone !== 'string' || value.timezone.trim() === '') {
    return timePricingValidationMessage(t, 'timezone')
  }
  const timezone = value.timezone.trim()

  try {
    new Intl.DateTimeFormat('en-US', { timeZone: timezone })
  } catch {
    return timePricingValidationMessage(t, 'timezone')
  }

  const periods = [] as { start: number, end: number }[]
  for (const period of value.periods) {
    if (!CLOCK_TIME.test(period.start_time) || !CLOCK_TIME.test(period.end_time)) {
      return timePricingValidationMessage(t, 'format')
    }
    if (period.start_time === period.end_time) {
      return timePricingValidationMessage(t, 'range')
    }

    const start = timeToSeconds(period.start_time, false)
    const end = timeToSeconds(period.end_time, true)
    if (start >= end) return timePricingValidationMessage(t, 'range')

    if (!isValidTimePricingMultiplier(period.multiplier)) {
      return timePricingValidationMessage(t, 'multiplier')
    }
    periods.push({ start, end })
  }

  const sorted = [...periods].sort((a, b) => a.start - b.start)
  for (let i = 1; i < sorted.length; i++) {
    if (sorted[i].start < sorted[i - 1].end) {
      return timePricingValidationMessage(t, 'overlap')
    }
  }
  return null
}

// ── 账号统计（成本）分时独立价 ──────────────────────────────
// 与售价侧分时倍率是两套机制：成本侧每段自带完整单价（无倍率），
// 结构键名 `stats_time_pricing`，售价侧 `time_pricing` 一字不动。

export interface StatsTimePricingPeriodFormEntry {
  start_time: string
  end_time: string
  input_price: number | string | null
  output_price: number | string | null
  cache_write_price: number | string | null
  /**
   * 界面上不出现在段表列内（§8 只列 7 列），按接口原值（per-token）透写：
   * 不走 $/MTok 换算，否则超过 10 位有效数字的值在保存时会被改写。
   */
  cache_write_1h_price?: number | null
  cache_read_price: number | string | null
  per_request_price: number | string | null
}

export interface StatsTimePricingFormEntry {
  timezone: string
  periods: StatsTimePricingPeriodFormEntry[]
}

/** 时区默认值与售价侧时段表同一默认，避免两张时段表默认值不同 */
export function createDefaultStatsTimePricingForm(): StatsTimePricingFormEntry {
  return { timezone: DEFAULT_TIME_PRICING_TIMEZONE, periods: [] }
}

export function apiStatsTimePricingToForm(value: AccountStatsTimePricing | null | undefined): StatsTimePricingFormEntry {
  if (!value || !value.periods?.length) return createDefaultStatsTimePricingForm()
  return {
    // 已配置时段时原样带上库里的时区：缺失或非法不在读侧就地补上默认值，
    // 否则一次无关的保存会把坏配置静默写成默认时区。空值留给提交校验阻断。
    timezone: typeof value.timezone === 'string' ? value.timezone : '',
    periods: value.periods.map(period => ({
      start_time: LEGACY_CLOCK_TIME.test(period.start_time) ? `${period.start_time}:00` : period.start_time,
      end_time: LEGACY_CLOCK_TIME.test(period.end_time) ? `${period.end_time}:00` : period.end_time,
      input_price: perTokenToMTok(period.input_price),
      output_price: perTokenToMTok(period.output_price),
      cache_write_price: perTokenToMTok(period.cache_write_price),
      cache_write_1h_price: period.cache_write_1h_price ?? null,
      cache_read_price: perTokenToMTok(period.cache_read_price),
      per_request_price: period.per_request_price ?? null,
    })),
  }
}

/** 段表删空 ⇒ null（回到未配置形态，行为与改动前一致） */
export function formStatsTimePricingToAPI(
  value: StatsTimePricingFormEntry | null | undefined,
): AccountStatsTimePricing | null {
  if (!value?.periods?.length) return null
  const timezone = typeof value.timezone === 'string' ? value.timezone.trim() : ''
  return {
    timezone,
    periods: value.periods.map(period => ({
      start_time: period.start_time,
      end_time: period.end_time,
      input_price: mTokToPerToken(period.input_price),
      output_price: mTokToPerToken(period.output_price),
      cache_write_price: mTokToPerToken(period.cache_write_price),
      cache_write_1h_price: toNullableNumber(period.cache_write_1h_price),
      cache_read_price: mTokToPerToken(period.cache_read_price),
      per_request_price: toNullableNumber(period.per_request_price),
    })),
  }
}

function statsTimePricingRequiredPrice(
  period: StatsTimePricingPeriodFormEntry,
  billingMode: BillingMode,
): boolean {
  const blank = (val: number | string | null | undefined) => val === null || val === undefined || val === ''
  // 必填项按条目计费模式分（与后端写入校验同一判据；空模式按 token）
  if (!billingMode || billingMode === 'token') {
    return blank(period.input_price) || blank(period.output_price) || blank(period.cache_read_price)
  }
  return blank(period.per_request_price)
}

/**
 * 校验账号统计分时价；返回错误消息，通过返回 null。
 * 时段语义与售价侧一致（左闭右开、00:00:00 作当日 24:00、不跨零点、不重叠），
 * 必填项按条目 billing_mode 区分：token 需三价齐全，按次/图片/视频需按次价。
 */
export function validateStatsTimePricing(entry: PricingFormEntry, t: TranslateFn): string | null {
  const value = entry.stats_time_pricing
  if (!value?.periods?.length) return null

  if (typeof value.timezone !== 'string' || value.timezone.trim() === '') {
    return timePricingValidationMessage(t, 'timezone')
  }
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: value.timezone.trim() })
  } catch {
    return timePricingValidationMessage(t, 'timezone')
  }

  const periods: { start: number, end: number, index: number }[] = []
  for (const [index, period] of value.periods.entries()) {
    const number = index + 1
    if (!CLOCK_TIME.test(period.start_time) || !CLOCK_TIME.test(period.end_time)) {
      return t('admin.channels.statsTimePricingValidation.format', { index: number })
    }
    if (period.start_time === period.end_time) {
      return t('admin.channels.statsTimePricingValidation.range', { index: number })
    }

    const start = timeToSeconds(period.start_time, false)
    const end = timeToSeconds(period.end_time, true)
    if (start >= end) return t('admin.channels.statsTimePricingValidation.range', { index: number })
    periods.push({ start, end, index: number })

    if (statsTimePricingRequiredPrice(period, entry.billing_mode)) {
      const mode = !entry.billing_mode || entry.billing_mode === 'token' ? 'token' : 'perRequest'
      return t(`admin.channels.statsTimePricingValidation.required.${mode}`, { index: number })
    }
    const prices: [string, number | string | null | undefined][] = [
      ['inputPrice', period.input_price],
      ['outputPrice', period.output_price],
      ['cacheWritePrice', period.cache_write_price],
      ['cacheWrite1hPrice', period.cache_write_1h_price],
      ['cacheReadPrice', period.cache_read_price],
      ['perRequestPrice', period.per_request_price],
    ]
    for (const [key, val] of prices) {
      if (val == null || val === '') continue
      if (!Number.isFinite(Number(val))) {
        return t('admin.channels.statsTimePricingValidation.invalidPrice', {
          index: number,
          field: intervalPriceLabel(t, key),
        })
      }
      if (Number(val) < 0) {
        return t('admin.channels.statsTimePricingValidation.negativePrice', {
          index: number,
          field: intervalPriceLabel(t, key),
        })
      }
    }
  }

  const sorted = [...periods].sort((a, b) => a.start - b.start)
  for (let i = 1; i < sorted.length; i++) {
    if (sorted[i].start < sorted[i - 1].end) {
      return t('admin.channels.statsTimePricingValidation.overlap', { index: sorted[i].index })
    }
  }
  return null
}

export function formatTimezoneOffset(timezone: string, at = new Date()): string {
  try {
    const part = new Intl.DateTimeFormat('en-US', {
      timeZone: timezone,
      timeZoneName: 'shortOffset',
    }).formatToParts(at).find(item => item.type === 'timeZoneName')?.value
    if (!part || part === 'GMT') return 'UTC+00:00'
    const match = /^GMT([+-])(\d{1,2})(?::(\d{2}))?$/.exec(part)
    if (!match) return ''
    return `UTC${match[1]}${match[2].padStart(2, '0')}:${match[3] || '00'}`
  } catch {
    return ''
  }
}

// 价格转换：后端存 per-token，前端显示 per-MTok ($/1M tokens)
const MTOK = 1_000_000

export function toNullableNumber(val: number | string | null | undefined): number | null {
  if (val === null || val === undefined || val === '') return null
  const num = Number(val)
  return isNaN(num) ? null : num
}

export function isValidPositiveMultiplier(val: number | string | null | undefined): boolean {
  if (val === null || val === undefined || val === '') return true
  const multiplier = Number(val)
  return Number.isFinite(multiplier) && multiplier > 0
}

export function formReasoningEffortMultipliersToAPI(
  value: PricingFormEntry['reasoning_effort_multipliers'],
): Record<string, number> | null {
  const entries = Object.entries(value || {})
    .filter(([, multiplier]) => multiplier !== '')
    .map(([effort, multiplier]) => [effort, Number(multiplier)])
  return entries.length ? Object.fromEntries(entries) : null
}

export function validateReasoningEffortMultipliers(
  value: PricingFormEntry['reasoning_effort_multipliers'],
  t: TranslateFn,
): string | null {
  for (const [effort, multiplier] of Object.entries(value || {})) {
    if (!REASONING_EFFORT_LEVELS.some(level => level === effort)) {
      return t('admin.channels.form.reasoningEffortLevelInvalid', { effort })
    }
    if (multiplier !== '' && !isValidPositiveMultiplier(multiplier)) {
      return t('admin.channels.form.reasoningEffortMultiplierPositive', { effort })
    }
  }
  return null
}

/** 前端显示值($/MTok) → 后端存储值(per-token) */
export function mTokToPerToken(val: number | string | null | undefined): number | null {
  const num = toNullableNumber(val)
  return num === null ? null : parseFloat((num / MTOK).toPrecision(10))
}

/** 后端存储值(per-token) → 前端显示值($/MTok) */
export function perTokenToMTok(val: number | null | undefined): number | null {
  if (val === null || val === undefined) return null
  // toPrecision(10) 消除 IEEE 754 浮点乘法精度误差，如 5e-8 * 1e6 = 0.04999...96 → 0.05
  return parseFloat((val * MTOK).toPrecision(10))
}

export function apiIntervalsToForm(intervals: PricingInterval[]): IntervalFormEntry[] {
  return (intervals || []).map(iv => ({
    min_tokens: iv.min_tokens,
    max_tokens: iv.max_tokens,
    tier_label: iv.tier_label || '',
    input_price: perTokenToMTok(iv.input_price),
    output_price: perTokenToMTok(iv.output_price),
    cache_write_price: perTokenToMTok(iv.cache_write_price),
    cache_write_1h_price: perTokenToMTok(iv.cache_write_1h_price),
    cache_write_12h_price: perTokenToMTok(iv.cache_write_12h_price),
    cache_read_price: perTokenToMTok(iv.cache_read_price),
    input_multiplier: iv.input_multiplier,
    output_multiplier: iv.output_multiplier,
    cache_write_multiplier: iv.cache_write_multiplier,
    cache_read_multiplier: iv.cache_read_multiplier,
    per_request_price: iv.per_request_price,
    sort_order: iv.sort_order
  }))
}

export function formIntervalsToAPI(intervals: IntervalFormEntry[]): PricingInterval[] {
  return (intervals || []).map(iv => ({
    min_tokens: iv.min_tokens,
    max_tokens: iv.max_tokens,
    tier_label: iv.tier_label,
    input_price: mTokToPerToken(iv.input_price),
    output_price: mTokToPerToken(iv.output_price),
    cache_write_price: mTokToPerToken(iv.cache_write_price),
    cache_write_1h_price: mTokToPerToken(iv.cache_write_1h_price),
    cache_write_12h_price: mTokToPerToken(iv.cache_write_12h_price),
    cache_read_price: mTokToPerToken(iv.cache_read_price),
    input_multiplier: toNullableNumber(iv.input_multiplier),
    output_multiplier: toNullableNumber(iv.output_multiplier),
    cache_write_multiplier: toNullableNumber(iv.cache_write_multiplier),
    cache_read_multiplier: toNullableNumber(iv.cache_read_multiplier),
    per_request_price: toNullableNumber(iv.per_request_price),
    sort_order: iv.sort_order
  }))
}

// ── 模型模式冲突检测 ──────────────────────────────────────

interface ModelPattern {
  pattern: string
  prefix: string  // lowercase, 通配符去掉尾部 *
  wildcard: boolean
}

function toModelPattern(model: string): ModelPattern {
  const lower = model.toLowerCase()
  const wildcard = lower.endsWith('*')
  return {
    pattern: model,
    prefix: wildcard ? lower.slice(0, -1) : lower,
    wildcard,
  }
}

function patternsConflict(a: ModelPattern, b: ModelPattern): boolean {
  if (!a.wildcard && !b.wildcard) return a.prefix === b.prefix
  if (a.wildcard && !b.wildcard) return b.prefix.startsWith(a.prefix)
  if (!a.wildcard && b.wildcard) return a.prefix.startsWith(b.prefix)
  // 双通配符：任一前缀是另一前缀的前缀即冲突
  return a.prefix.startsWith(b.prefix) || b.prefix.startsWith(a.prefix)
}

/** 检测模型模式列表中的冲突，返回冲突的两个模式名；无冲突返回 null */
export function findModelConflict(models: string[]): [string, string] | null {
  const patterns = models.map(toModelPattern)
  for (let i = 0; i < patterns.length; i++) {
    for (let j = i + 1; j < patterns.length; j++) {
      if (patternsConflict(patterns[i], patterns[j])) {
        return [patterns[i].pattern, patterns[j].pattern]
      }
    }
  }
  return null
}

// ── 区间校验 ──────────────────────────────────────────────

/** 校验区间列表的合法性，返回错误消息；通过则返回 null
 *
 * mode 决定区间语义：
 * - token：区间是上下文 token 数分段 (min, max]，不能重叠，无上限段必须放最后
 * - per_request / image：区间是按 tier_label 分层（1K/2K/4K 等），后端按 label
 *   匹配，不依赖 min/max，因此跳过重叠 / last-unlimited 校验
 */
export function validateIntervals(
  intervals: IntervalFormEntry[],
  mode: BillingMode,
  t: TranslateFn,
): string | null {
  if (!intervals || intervals.length === 0) return null

  // 按 min_tokens 排序（不修改原数组）
  const sorted = [...intervals].sort((a, b) => a.min_tokens - b.min_tokens)

  for (let i = 0; i < sorted.length; i++) {
    const err = validateSingleInterval(sorted[i], i, t)
    if (err) return err
  }

  // per_request / image 模式按 tier_label 匹配，不做 token 区间重叠校验
  if (mode !== 'token') return null
  return checkIntervalOverlap(sorted, t)
}

function intervalValidationMessage(
  t: TranslateFn,
  key: string,
  params: Record<string, unknown>,
): string {
  return t(`admin.channels.intervalValidation.${key}`, params)
}

function intervalPriceLabel(t: TranslateFn, key: string): string {
  return t(`admin.channels.intervalValidation.price.${key}`)
}

function validateSingleInterval(iv: IntervalFormEntry, idx: number, t: TranslateFn): string | null {
  const index = idx + 1
  if (iv.min_tokens < 0) {
    return intervalValidationMessage(
      t,
      'negativeMin',
      { index, value: iv.min_tokens },
    )
  }
  if (iv.max_tokens != null) {
    if (iv.max_tokens <= 0) {
      return intervalValidationMessage(
        t,
        'maxPositive',
        { index, value: iv.max_tokens },
      )
    }
    if (iv.max_tokens <= iv.min_tokens) {
      return intervalValidationMessage(
        t,
        'maxGreaterThanMin',
        { index, max: iv.max_tokens, min: iv.min_tokens },
      )
    }
  }
  return validateIntervalPrices(iv, idx, t)
}

function validateIntervalPrices(iv: IntervalFormEntry, idx: number, t: TranslateFn): string | null {
  const index = idx + 1
  const prices: [string, number | string | null][] = [
    ['inputPrice', iv.input_price],
    ['outputPrice', iv.output_price],
    ['cacheWritePrice', iv.cache_write_price],
    ['cacheWrite1hPrice', iv.cache_write_1h_price ?? null],
    ['cacheWrite12hPrice', iv.cache_write_12h_price ?? null],
    ['cacheReadPrice', iv.cache_read_price],
    ['perRequestPrice', iv.per_request_price],
  ]
  for (const [key, val] of prices) {
    if (val != null && val !== '' && Number(val) < 0) {
      const field = intervalPriceLabel(t, key)
      return intervalValidationMessage(
        t,
        'negativePrice',
        { index, field },
      )
    }
  }
  const multipliers: [string, number | string | null][] = [
    ['inputMultiplier', iv.input_multiplier],
    ['outputMultiplier', iv.output_multiplier],
    ['cacheWriteMultiplier', iv.cache_write_multiplier],
    ['cacheReadMultiplier', iv.cache_read_multiplier],
  ]
  for (const [key, val] of multipliers) {
    if (!isValidPositiveMultiplier(val)) {
      return intervalValidationMessage(t, 'multiplierPositive', {
        index,
        field: intervalPriceLabel(t, key),
      })
    }
  }
  return null
}

function checkIntervalOverlap(sorted: IntervalFormEntry[], t: TranslateFn): string | null {
  for (let i = 0; i < sorted.length; i++) {
    // 无上限区间必须是最后一个
    if (sorted[i].max_tokens == null && i < sorted.length - 1) {
      return intervalValidationMessage(
        t,
        'unboundedLast',
        { index: i + 1 },
      )
    }
    if (i === 0) continue
    const prev = sorted[i - 1]
    // (min, max] 语义：前一个区间上界 > 当前区间下界则重叠
    if (prev.max_tokens == null || prev.max_tokens > sorted[i].min_tokens) {
      const prevMax = prev.max_tokens == null ? '∞' : String(prev.max_tokens)
      return intervalValidationMessage(
        t,
        'overlap',
        { previousIndex: i, currentIndex: i + 1, previousMax: prevMax, currentMin: sorted[i].min_tokens },
      )
    }
  }
  return null
}

/** 平台对应的模型 tag 样式（背景+文字） */
export function getPlatformTagClass(platform: string): string {
  switch (platform) {
    case 'anthropic': return 'bg-orange-100 text-orange-700 dark:bg-orange-900/30 dark:text-orange-400'
    case 'openai': return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400'
    case 'gemini': return 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400'
    case 'antigravity': return 'bg-purple-100 text-purple-700 dark:bg-purple-900/30 dark:text-purple-400'
    case 'grok': return 'bg-slate-100 text-slate-700 dark:bg-slate-800 dark:text-slate-300'
    case 'kimi': return 'bg-pink-100 text-pink-700 dark:bg-pink-900/30 dark:text-pink-400'
    case 'zhipu': return 'bg-indigo-100 text-indigo-700 dark:bg-indigo-900/30 dark:text-indigo-400'
    case 'deepseek': return 'bg-teal-100 text-teal-700 dark:bg-teal-900/30 dark:text-teal-400'
    default: return 'bg-gray-100 text-gray-700 dark:bg-gray-900/30 dark:text-gray-400'
  }
}

/** 平台对应的模型文字色（仅 text-*，用于 input/text 场景）— 与 getPlatformTagClass 同色系 */
export function getPlatformTextClass(platform: string): string {
  switch (platform) {
    case 'anthropic': return 'text-orange-700 dark:text-orange-400'
    case 'openai': return 'text-emerald-700 dark:text-emerald-400'
    case 'gemini': return 'text-blue-700 dark:text-blue-400'
    case 'antigravity': return 'text-purple-700 dark:text-purple-400'
    case 'grok': return 'text-slate-700 dark:text-slate-300'
    case 'kimi': return 'text-pink-700 dark:text-pink-400'
    case 'zhipu': return 'text-indigo-700 dark:text-indigo-400'
    case 'deepseek': return 'text-teal-700 dark:text-teal-400'
    default: return ''
  }
}
