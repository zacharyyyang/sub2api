import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import GoogleQuotaGroups from '../GoogleQuotaGroups.vue'
import type { GoogleQuotaGroup, GoogleQuotaWindow } from '@/types'

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

const makeWindow = (overrides: Partial<GoogleQuotaWindow> = {}): GoogleQuotaWindow => ({
  bucket_id: 'b1',
  kind: 'five_hour',
  utilization: 50,
  ...overrides
})

const makeGroup = (overrides: Partial<GoogleQuotaGroup> = {}): GoogleQuotaGroup => ({
  kind: 'gemini',
  label: 'Gemini',
  windows: [makeWindow()],
  ...overrides
})

function mountGroups(groups: GoogleQuotaGroup[] | null | undefined) {
  return mount(GoogleQuotaGroups, {
    props: { groups },
    global: { stubs: { UsageProgressBar: barStub } }
  })
}

describe('GoogleQuotaGroups', () => {
  it('F1: 按后端顺序渲染每个组的组名行与其窗口条，claude_gpt 多域组带域后缀，Gemini 组在前', () => {
    const wrapper = mountGroups([
      makeGroup({
        windows: [makeWindow({ bucket_id: 'g1', utilization: 45 }), makeWindow({ bucket_id: 'g2', kind: 'seven_day', utilization: 80 })]
      }),
      makeGroup({
        kind: 'claude_gpt',
        label: 'Claude/GPT',
        domain: 'prod',
        windows: [makeWindow({ bucket_id: 'g3', utilization: 12 }), makeWindow({ bucket_id: 'g4', kind: 'seven_day', utilization: 30 })]
      }),
      makeGroup({
        kind: 'claude_gpt',
        label: 'Claude/GPT',
        domain: 'daily',
        windows: [makeWindow({ bucket_id: 'g5', utilization: 55 }), makeWindow({ bucket_id: 'g6', kind: 'seven_day', utilization: 22 })]
      })
    ])

    expect(wrapper.findAll('.group-name')).toHaveLength(3)
    expect(wrapper.findAll('.usage-bar')).toHaveLength(6)

    const text = wrapper.text()
    // gemini 组（账号级）不拼域
    expect(text).toContain('admin.accounts.googleQuota.groupGemini')
    expect(text).not.toContain('admin.accounts.googleQuota.groupGemini ·')
    // claude_gpt 组名行 = 组词条 + · + 域词条（prod / daily 各一行）
    expect(text).toContain('admin.accounts.googleQuota.groupClaudeGPT · admin.accounts.googleQuota.domain.prod')
    expect(text).toContain('admin.accounts.googleQuota.groupClaudeGPT · admin.accounts.googleQuota.domain.daily')
    // gemini 组在后端顺序中在前
    expect(text.indexOf('admin.accounts.googleQuota.groupGemini')).toBeLessThan(text.indexOf('admin.accounts.googleQuota.groupClaudeGPT'))
    // 窗口条标签与数值按窗口顺序呈现
    expect(text).toContain('admin.accounts.googleQuota.window5h|45|')
    expect(text).toContain('admin.accounts.googleQuota.windowWeekly|80|')
  })

  it.each([[null], [[]], [undefined]])('F2: groups 为 %p 时不渲染任何节点', (groups) => {
    const wrapper = mountGroups(groups as never)
    // DOM 元素节点为零（Vue 空态根只留单个 <!--v-if--> 注释节点，注释非元素节点）——
    // 设计 S6「组件根为空」的等价断言，不绑定注释文本格式
    expect(wrapper.findAll('*')).toHaveLength(0)
    expect(wrapper.findAll('.group-name')).toHaveLength(0)
    expect(wrapper.findAll('.usage-bar')).toHaveLength(0)
    expect(wrapper.text()).toBe('')
  })

  it('F3: 组 kind=other 时用上游 label 而非本地词条', () => {
    const wrapper = mountGroups([
      makeGroup({ kind: 'other', label: '自定义组名', windows: [makeWindow({ bucket_id: 'o1' })] })
    ])

    expect(wrapper.text()).toContain('自定义组名')
    expect(wrapper.text()).not.toContain('admin.accounts.googleQuota.groupOther')
    // 窗口标签规则同为 other ⇒ 上游 label 优先
    expect(wrapper.text()).toContain('admin.accounts.googleQuota.window5h|50|')
  })

  it('F3b: kind 命中本地词条时上游 label 不参与（gemini 组名固化为词条）', () => {
    const wrapper = mountGroups([makeGroup({ label: '上游传来的名字', windows: [makeWindow({ bucket_id: 'g1' })] })])

    expect(wrapper.text()).toContain('admin.accounts.googleQuota.groupGemini')
    expect(wrapper.text()).not.toContain('上游传来的名字')
  })

  it('F3c: kind=other 且上游 label 缺失时兜底本地词条', () => {
    const wrapper = mountGroups([makeGroup({ kind: 'other', label: '', windows: [makeWindow({ bucket_id: 'o1' })] })])

    // 组名兜底到本地词条
    expect(wrapper.text()).toContain('admin.accounts.googleQuota.groupOther')
    expect(wrapper.findAll('.group-name')).toHaveLength(1)
    // 窗口标签不参与组名兜底（five_hour 命中词条）
    expect(wrapper.text()).toContain('admin.accounts.googleQuota.window5h|50|')
  })

  it('F4: 非有限 / 负数 utilization 的窗口被跳过，窗口数组缺失时不抛错且 0 节点', () => {
    const wrapper = mountGroups([
      makeGroup({
        windows: [
          makeWindow({ bucket_id: 'nan', utilization: Number.NaN }),
          makeWindow({ bucket_id: 'inf', utilization: Number.POSITIVE_INFINITY }),
          makeWindow({ bucket_id: 'neg', utilization: -1 })
        ]
      }),
      makeGroup({ kind: 'claude_gpt', label: 'Claude/GPT', windows: undefined as never })
    ])

    expect(wrapper.findAll('.usage-bar')).toHaveLength(0)
    expect(wrapper.findAll('.group-name')).toHaveLength(0)
  })

  it('F4b: 组内仅部分窗口无效时有效窗口照常渲染且组名行保留', () => {
    const wrapper = mountGroups([
      makeGroup({
        windows: [
          makeWindow({ bucket_id: 'bad', utilization: Number.NaN }),
          makeWindow({ bucket_id: 'ok', utilization: 30 })
        ]
      })
    ])

    expect(wrapper.findAll('.group-name')).toHaveLength(1)
    expect(wrapper.findAll('.usage-bar')).toHaveLength(1)
    expect(wrapper.text()).toContain('admin.accounts.googleQuota.window5h|30|')
  })

  it('F5: reset_time 缺失时窗口条照常渲染、时间列缺位', () => {
    const wrapper = mountGroups([
      makeGroup({
        windows: [
          makeWindow({ bucket_id: 'no-time', utilization: 20 }),
          makeWindow({ bucket_id: 'with-time', utilization: 60, reset_time: '2026-03-17T02:30:00Z' })
        ]
      })
    ])

    const bars = wrapper.findAll('.usage-bar')
    expect(bars).toHaveLength(2)
    // 时间列缺位：无 reset_time 的条文本以分隔符结尾（无时间值）
    expect(bars[0].text()).toBe('admin.accounts.googleQuota.window5h|20|')
    // 有 reset_time 的条保留时间值
    expect(bars[1].text()).toBe('admin.accounts.googleQuota.window5h|60|2026-03-17T02:30:00Z')
  })

  it('F6: claude_gpt 组 domain 缺省（v1 旧数据形态）时不拼域后缀、不报错', () => {
    const wrapper = mountGroups([
      makeGroup({ kind: 'claude_gpt', label: 'Claude/GPT', windows: [makeWindow({ bucket_id: 'g1', utilization: 40 })] })
    ])

    expect(wrapper.findAll('.group-name')).toHaveLength(1)
    expect(wrapper.text()).toContain('admin.accounts.googleQuota.groupClaudeGPT')
    expect(wrapper.text()).not.toContain('·')
    expect(wrapper.text()).not.toContain('admin.accounts.googleQuota.domain')
  })
})