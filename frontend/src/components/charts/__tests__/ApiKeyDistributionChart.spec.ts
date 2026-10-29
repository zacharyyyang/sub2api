import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import ApiKeyDistributionChart from '../ApiKeyDistributionChart.vue'
import type { ApiKeyStat } from '@/types'

const messages: Record<string, string> = {
  'usage.apiKeyDistribution': 'API Key Distribution',
  'usage.apiKeyColumn': 'API Key',
  'usage.apiKeyDeleted': '(deleted key)',
  'admin.dashboard.requests': 'Requests',
  'admin.dashboard.tokens': 'Tokens',
  'admin.dashboard.actual': 'Actual',
  'admin.dashboard.standard': 'Standard',
  'admin.dashboard.metricTokens': 'By Tokens',
  'admin.dashboard.metricActualCost': 'By Actual Cost',
  'admin.dashboard.noDataAvailable': 'No data available',
  'common.retry': 'Retry',
}

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => messages[key] ?? key,
    }),
  }
})

vi.mock('vue-chartjs', () => ({
  Doughnut: {
    props: ['data'],
    template: '<div class="chart-data">{{ JSON.stringify(data) }}</div>',
  },
}))

describe('ApiKeyDistributionChart', () => {
  const baseStats: ApiKeyStat[] = [
    { id: 7, name: 'wb-beta', requests: 6, total_tokens: 2100, cost: 2.0, actual_cost: 1.2 },
    { id: 9, name: 'wb-alpha', requests: 4, total_tokens: 1400, cost: 1.0, actual_cost: 0.8 },
  ]

  it('renders per-key rows sorted by the selected metric and shows CNY prices', () => {
    const wrapper = mount(ApiKeyDistributionChart, {
      props: { apiKeyStats: baseStats, metric: 'tokens' },
    })

    const rows = wrapper.findAll('tbody tr')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toContain('wb-beta')
    expect(rows[0].text()).toContain('2.10K')
    expect(rows[0].text()).toContain('¥1.2000')
    expect(rows[1].text()).toContain('¥0.8000')
  })

  it('re-sorts by actual cost when the metric changes', () => {
    const sorted = [...baseStats].sort((a, b) => b.actual_cost - a.actual_cost)
    const wrapper = mount(ApiKeyDistributionChart, {
      props: { apiKeyStats: sorted, metric: 'actual_cost' },
    })

    const rows = wrapper.findAll('tbody tr')
    expect(rows[0].text()).toContain('wb-beta')
  })

  // 名称查不到（密钥已删除）时必须带上 id，否则多把这样的密钥显示成同名行无法分辨来源
  it('labels deleted keys with the localized placeholder plus the key id', () => {
    const wrapper = mount(ApiKeyDistributionChart, {
      props: {
        apiKeyStats: [
          { id: 42, name: '', requests: 3, total_tokens: 300, cost: 0.6, actual_cost: 0.6 },
          { id: 43, name: '', requests: 1, total_tokens: 100, cost: 0.2, actual_cost: 0.2 },
        ],
      },
    })

    const text = wrapper.text()
    expect(text).toContain('(deleted key) #42')
    expect(text).toContain('(deleted key) #43')
    expect(text).not.toContain('unknown')
  })

  // 读取失败不能被渲染成「没有用量」：必须报错并给出重试入口
  it('shows an error state with retry instead of the empty-data placeholder when loading failed', async () => {
    const wrapper = mount(ApiKeyDistributionChart, {
      props: { apiKeyStats: [], error: 'Failed to load statistics' },
    })

    expect(wrapper.find('[data-test="api-key-distribution-error"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('Failed to load statistics')
    expect(wrapper.text()).not.toContain('No data available')

    await wrapper.get('[data-test="api-key-distribution-error"] button').trigger('click')
    expect(wrapper.emitted('retry')).toHaveLength(1)
  })

  it('falls back to the empty-data placeholder only when there is genuinely no data', () => {
    const wrapper = mount(ApiKeyDistributionChart, {
      props: { apiKeyStats: [], error: null },
    })

    expect(wrapper.text()).toContain('No data available')
    expect(wrapper.find('[data-test="api-key-distribution-error"]').exists()).toBe(false)
  })
})