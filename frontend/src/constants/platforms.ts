import type { AccountPlatform, GroupPlatform } from '@/types'

export interface PlatformOption<T extends string = string> {
  value: T
  label: string
}

/**
 * Concrete upstream platforms supported by accounts and request routing.
 * Keep platform selectors derived from this catalog so newly added providers
 * do not silently disappear from list filters.
 */
export const CONCRETE_PLATFORM_OPTIONS = [
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'openai', label: 'OpenAI' },
  { value: 'gemini', label: 'Gemini' },
  { value: 'antigravity', label: 'Antigravity' },
  { value: 'grok', label: 'Grok' },
  { value: 'kimi', label: 'Kimi' },
  { value: 'zhipu', label: 'Zhipu GLM' },
  { value: 'deepseek', label: 'DeepSeek' },
  { value: 'minimax', label: 'MiniMax' },
  { value: 'opencode_go', label: 'OpenCode' },
  { value: 'typesafe', label: 'TypeSafe / Jev' }
] as const satisfies readonly PlatformOption<AccountPlatform>[]

/**
 * wb 企业账号平台项（仅供账号面消费：创建弹窗 tab / 账号筛选 / 错误透传规则）。
 * 不进 CONCRETE_PLATFORM_OPTIONS / GROUP_PLATFORM_OPTIONS —— wb 不参与分组 /
 * 配额 / 渠道面枚举（设计 §7 边界）；需要它的账号面各自显式引用本项。
 */
export const WB_PLATFORM_OPTION: PlatformOption<AccountPlatform> = {
  value: 'wb',
  label: 'WB Enterprise'
}

/** Platforms that can own a group. */
export const GROUP_PLATFORM_OPTIONS = [
  ...CONCRETE_PLATFORM_OPTIONS,
  { value: 'composite', label: 'Composite' }
] as const satisfies readonly PlatformOption<GroupPlatform>[]
