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
    windows: [
      { bucket_id: 'g3', kind: 'five_hour', utilization: 12 },
      { bucket_id: 'g4', kind: 'seven_day', utilization: 30 }
    ]
  }
]

describe('AccountUsageCell · Google 配额组', () => {
  it('M1: 块一内 4 条逐模型行与 4 条组额度条并存', async () => {
    const wrapper = await mountCell({
      antigravity_quota: {
        'gemini-3-pro-low': { utilization: 80, reset_time: '2026-03-17T01:00:00Z' },
        'gemini-3-flash': { utilization: 60, reset_time: '2026-03-17T02:00:00Z' },
        'gemini-2.5-flash-image': { utilization: 70, reset_time: '2026-03-17T03:00:00Z' },
        'claude-sonnet-4-5': { utilization: 50, reset_time: '2026-03-17T04:00:00Z' }
      },
      google_quota_groups: groups4
    })

    expect(wrapper.findAll('.usage-bar')).toHaveLength(8) // 4 逐模型 + 4 组额度
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.gemini3Pro|80|')
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.gemini3Flash|60|')
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.gemini3Image|70|')
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.claude|50|')
    expect(wrapper.text()).toContain('admin.accounts.googleQuota.window5h|45|')
    expect(wrapper.text()).toContain('admin.accounts.googleQuota.windowWeekly|80|')
    expect(wrapper.text()).toContain('admin.accounts.googleQuota.groupGemini')
    expect(wrapper.text()).toContain('admin.accounts.googleQuota.groupClaudeGPT')
  })

  it('M2: 无逐模型额度、无 AI Credits 但有组额度时，块二渲染 4 条且 `-` 不出现', async () => {
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

  it('M4: 组数据缺字段时挂载不抛错，有效窗口仍渲染', async () => {
    const wrapper = await mountCell({
      antigravity_quota: { 'gemini-3-flash': { utilization: 10, reset_time: '2026-03-17T01:00:00Z' } },
      google_quota_groups: [{ kind: 'gemini', label: 'Gemini', windows: [{ utilization: 12 }] }]
    })

    expect(wrapper.findAll('.usage-bar')).toHaveLength(2) // 1 逐模型 + 1 组额度
    expect(wrapper.text()).toContain('admin.accounts.googleQuota.groupGemini')
  })
})