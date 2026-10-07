import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountUsageCell from '../AccountUsageCell.vue'
import type { Account } from '@/types'

const { getUsage } = vi.hoisted(() => ({
  getUsage: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getUsage
    }
  }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const barStub = {
  props: ['label', 'utilization', 'resetsAt', 'color'],
  template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ resetsAt }}</div>'
}

function makeAccount(overrides: Partial<Account>): Account {
  return {
    id: 1,
    name: 'account',
    platform: 'antigravity',
    type: 'oauth',
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: true,
    created_at: '2026-03-15T00:00:00Z',
    updated_at: '2026-03-15T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides,
  }
}

function mountCell(usage: unknown, accountId = 1001) {
  getUsage.mockResolvedValue(usage)
  const wrapper = mount(AccountUsageCell, {
    props: {
      account: makeAccount({
        id: accountId,
        platform: 'antigravity',
        type: 'oauth',
        extra: {}
      })
    },
    global: {
      stubs: {
        UsageProgressBar: barStub,
        AccountQuotaInfo: true
      }
    }
  })
  return flushPromises().then(() => wrapper)
}

// 三组六窗：gemini 账号级 1 组 + claude_gpt 每域 1 组（三期起仅 daily 组渲染）
const groups4 = [
  {
    kind: 'gemini',
    label: 'Gemini',
    windows: [
      { bucket_id: 'g1', kind: 'five_hour', utilization: 45 },
      { bucket_id: 'g2', kind: 'seven_day', utilization: 80 }
    ]
  },
  {
    kind: 'claude_gpt',
    label: 'Claude/GPT',
    domain: 'prod',
    windows: [
      { bucket_id: 'g3', kind: 'five_hour', utilization: 12 },
      { bucket_id: 'g4', kind: 'seven_day', utilization: 30 }
    ]
  },
  {
    kind: 'claude_gpt',
    label: 'Claude/GPT',
    domain: 'daily',
    windows: [
      { bucket_id: 'g5', kind: 'five_hour', utilization: 55 },
      { bucket_id: 'g6', kind: 'seven_day', utilization: 22 }
    ]
  }
]

// 仅 prod 组（带窗口）：数据非空但全被显示策略过滤（S22 第四态输入）
const prodOnlyGroups = [
  {
    kind: 'claude_gpt',
    label: 'Claude/GPT',
    domain: 'prod',
    windows: [{ bucket_id: 'p1', kind: 'five_hour', utilization: 12 }]
  }
]

describe('AccountUsageCell · Google 配额组', () => {
  it('M1: 合并单块渲 daily 组条 4 条（gemini 2 + daily 2），无逐模型条、无 prod', async () => {
    const wrapper = await mountCell({
      antigravity_quota: {
        'gemini-3-pro-low': { utilization: 80, reset_time: '2026-03-17T01:00:00Z' },
        'gemini-3-flash': { utilization: 60, reset_time: '2026-03-17T02:00:00Z' },
        'gemini-2.5-flash-image': { utilization: 70, reset_time: '2026-03-17T03:00:00Z' },
        'claude-sonnet-4-5': { utilization: 50, reset_time: '2026-03-17T04:00:00Z' }
      },
      google_quota_groups: groups4
    })

    expect(wrapper.findAll('.usage-bar')).toHaveLength(4) // 仅 daily 组条（gemini 2 + daily 2），prod 组被过滤
    const text = wrapper.text()
    expect(text).toContain('admin.accounts.googleQuota.groupGemini')
    expect(text).toContain('admin.accounts.googleQuota.groupClaudeGPT · admin.accounts.googleQuota.domain.daily')
    // 无逐模型条
    expect(text).not.toContain('admin.accounts.usageWindow.gemini3')
    expect(text).not.toContain('admin.accounts.usageWindow.claude')
    // 无 prod 行
    expect(text).not.toContain('admin.accounts.googleQuota.domain.prod')
    // daily 窗口数值照常渲染，prod 对应窗口不得挂入
    expect(text).toContain('admin.accounts.googleQuota.window5h|55|')
    expect(text).not.toContain('admin.accounts.googleQuota.window5h|12|')
  })

  it('M2: 无逐模型额度、无 AI Credits 但有组额度时，合并单块渲 4 条且 `-` 不出现', async () => {
    const wrapper = await mountCell({
      antigravity_quota: null,
      google_quota_groups: groups4
    })

    expect(wrapper.findAll('.usage-bar')).toHaveLength(4)
    expect(wrapper.text()).not.toContain('-')
    expect(wrapper.text()).toContain('admin.accounts.googleQuota.window5h|45|')
  })

  it('M3: 组额度为空时只渲染 AI Credits 行、0 组节点', async () => {
    const wrapper = await mountCell({
      antigravity_quota: null,
      ai_credits: [{ credit_type: 'GOOGLE_ONE_AI', amount: 25, minimum_balance: 5 }],
      google_quota_groups: null
    })

    expect(wrapper.findAll('.usage-bar')).toHaveLength(0)
    expect(wrapper.text()).toContain('admin.accounts.aiCreditsBalance')
    expect(wrapper.text()).toContain('25')
  })

  it('M4: 组数据缺字段（undefined）时挂载不抛错，占位 `-` 且不渲逐模型条', async () => {
    const wrapper = await mountCell({
      antigravity_quota: { 'gemini-3-flash': { utilization: 10, reset_time: '2026-03-17T01:00:00Z' } },
      google_quota_groups: undefined
    })

    expect(wrapper.findAll('.usage-bar')).toHaveLength(0)
    expect(wrapper.text()).toContain('-')
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.gemini3Flash')
  })

  it('M5: 有逐模型额度但组与 credits 均空时占位 `-`，无空块、不塌', async () => {
    const wrapper = await mountCell({
      antigravity_quota: {
        'gemini-3-flash': { utilization: 60, reset_time: '2026-03-17T02:00:00Z' },
        'claude-sonnet-4-5': { utilization: 50, reset_time: '2026-03-17T04:00:00Z' }
      },
      google_quota_groups: null
    })

    expect(wrapper.findAll('.usage-bar')).toHaveLength(0)
    expect(wrapper.text()).toContain('-')
  })

  it('M6: 仅 prod 组且无 credits 时格空白（非 `-` 占位、0 元素节点、不回退渲 prod）', async () => {
    const wrapper = await mountCell({ antigravity_quota: null, google_quota_groups: prodOnlyGroups })

    // 合并单块仍挂载（googleQuotaGroups.length > 0 真），但子组件过滤 prod 后渲 0 元素节点 =>
    expect(wrapper.findAll('.usage-bar')).toHaveLength(0)
    expect(wrapper.text()).toBe('')
    expect(wrapper.text()).not.toContain('-')
    expect(wrapper.text()).not.toContain('admin.accounts.googleQuota.domain.prod')
  })
})