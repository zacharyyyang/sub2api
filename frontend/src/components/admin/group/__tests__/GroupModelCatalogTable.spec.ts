import { enableAutoUnmount, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, nextTick, reactive } from 'vue'

import GroupModelCatalogTable from '../GroupModelCatalogTable.vue'
import type {
  CatalogOverviewSource,
  GroupModelCatalogRow
} from '@/views/admin/groupModelCatalog'
import zhOverview from '@/i18n/locales/zh/admin/overview'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}(${Object.values(params).join(',')})` : key
    })
  }
})

// 用例把表格挂到 document.body 上以断言焦点，逐例卸载避免互相残留
enableAutoUnmount(afterEach)

const row = (overrides: Partial<GroupModelCatalogRow> = {}): GroupModelCatalogRow => ({
  key: 1,
  shellID: 'claude-mythos-7',
  displayName: 'DeepSeek V4',
  realModel: 'deepseek-v4',
  supports1M: false,
  ...overrides
})

// 挂一个 v-model:rows 的宿主，贴近抽屉里的真实用法（组件只回写、不持有行集）
const mountTable = (
  initialRows: GroupModelCatalogRow[] = [],
  source: CatalogOverviewSource = {}
) => {
  const state = reactive({ rows: [...initialRows] })
  const host = defineComponent({
    setup() {
      return () =>
        h(GroupModelCatalogTable, {
          rows: state.rows,
          source,
          'onUpdate:rows': (rows: GroupModelCatalogRow[]) => {
            state.rows = rows
          }
        })
    }
  })
  const wrapper = mount(host, { attachTo: document.body })
  return { wrapper, state }
}

const headerTexts = (wrapper: ReturnType<typeof mountTable>['wrapper']) =>
  wrapper
    .findAll('[data-testid="group-model-catalog-headers"] th')
    .map(th => th.text().trim())

describe('GroupModelCatalogTable · 列头与空态（AC-13）', () => {
  it('shows the five fixed column headers', () => {
    const { wrapper } = mountTable([])
    expect(headerTexts(wrapper)).toEqual([
      'admin.groups.modelCatalog.columns.shellID',
      'admin.groups.modelCatalog.columns.displayName',
      'admin.groups.modelCatalog.columns.realModel',
      'admin.groups.modelCatalog.columns.supports1M',
      'admin.groups.modelCatalog.columns.remove'
    ])
  })

  it('renders the empty hint and keeps the add button in the footer', () => {
    const { wrapper } = mountTable([])
    expect(wrapper.find('[data-testid="group-model-catalog-empty"]').text()).toBe(
      'admin.groups.modelCatalog.empty'
    )
    expect(wrapper.find('[data-testid="group-model-catalog-add"]').text()).toBe(
      'admin.groups.modelCatalog.add'
    )
  })

  it('keeps the Chinese copy named by the UI decisions', () => {
    const columns = zhOverview.groups.modelCatalog.columns
    expect([
      columns.shellID,
      columns.displayName,
      columns.realModel,
      columns.supports1M,
      columns.remove
    ]).toEqual(['壳 ID', '显示名', '真实模型', '1M', '删除'])
    expect(zhOverview.groups.modelCatalog.add).toBe('添加')
    expect(zhOverview.groups.modelCatalog.title).toBe('模型目录')
    expect(zhOverview.groups.modelCatalog.empty).toBe('暂无')
    expect(zhOverview.groups.modelAllowlist.displayNameTableTitle).toBe('旧显示名（按真实模型 ID）')
    expect(zhOverview.groups.modelAllowlist.displayNameSectionHint).toBe(
      '非 Claude Code 客户端使用'
    )
    expect(zhOverview.groups.modelAllowlist.convertToCatalog).toBe('转为目录行')
  })
})

describe('GroupModelCatalogTable · 添加行', () => {
  it('inserts a row with the next free shell id and focuses its display name input', async () => {
    const { wrapper, state } = mountTable([])

    await wrapper.find('[data-testid="group-model-catalog-add"]').trigger('click')
    await nextTick()
    await nextTick()

    expect(state.rows).toHaveLength(1)
    expect(state.rows[0]).toMatchObject({
      shellID: 'claude-mythos-1',
      displayName: '',
      realModel: '',
      supports1M: false
    })
    expect(document.activeElement?.getAttribute('data-testid')).toBe(
      'group-model-catalog-display-name-1'
    )
  })

  it('prefills after the shell ids already used in the catalog and remains editable', async () => {
    const { wrapper, state } = mountTable([
      row({ key: 1, shellID: 'claude-mythos-1' }),
      row({ key: 2, shellID: 'claude-mythos-3', realModel: 'kimi-k2.6' })
    ])

    await wrapper.find('[data-testid="group-model-catalog-add"]').trigger('click')
    await nextTick()

    expect(state.rows[2].shellID).toBe('claude-mythos-2')

    const shellInput = wrapper.find('[data-testid="group-model-catalog-shell-3"]')
    await shellInput.setValue('claude-haiku-9')
    expect(state.rows[2].shellID).toBe('claude-haiku-9')
  })
})

describe('GroupModelCatalogTable · 行渲染与校验提示', () => {
  it('renders the row values and the 1M checkbox state', () => {
    const { wrapper } = mountTable([row({ supports1M: true })])

    expect(
      (wrapper.find('[data-testid="group-model-catalog-shell-1"]').element as HTMLInputElement)
        .value
    ).toBe('claude-mythos-7')
    expect(
      (
        wrapper.find('[data-testid="group-model-catalog-display-name-1"]')
          .element as HTMLInputElement
      ).value
    ).toBe('DeepSeek V4')
    expect(
      (
        wrapper.find('[data-testid="group-model-catalog-real-model-1"]')
          .element as HTMLInputElement
      ).value
    ).toBe('deepseek-v4')
    expect(
      (wrapper.find('[data-testid="group-model-catalog-1m-1"]').element as HTMLInputElement)
        .checked
    ).toBe(true)
    expect(wrapper.find('[data-testid="group-model-catalog-errors"]').exists()).toBe(false)
  })

  it('marks the offending row red and lists the reason under the table', () => {
    const { wrapper } = mountTable([row({ shellID: 'foo-1', realModel: '' })])

    const tableRow = wrapper.find('[data-testid="group-model-catalog-row-1"]')
    expect(tableRow.classes()).toContain('bg-red-50')

    const errors = wrapper.find('[data-testid="group-model-catalog-errors"]')
    expect(errors.exists()).toBe(true)
    expect(errors.text()).toContain(
      'admin.groups.modelCatalog.errors.shellFormat'
    )
    expect(errors.text()).toContain(
      'admin.groups.modelCatalog.errors.realModelRequired'
    )
  })

  it('toggles the 1M switch back into the row set', async () => {
    const { wrapper, state } = mountTable([row()])

    await wrapper.find('[data-testid="group-model-catalog-1m-1"]').setValue(true)

    expect(state.rows[0].supports1M).toBe(true)
  })

  it('removes the row it owns and leaves the others untouched', async () => {
    const { wrapper, state } = mountTable([
      row({ key: 1 }),
      row({ key: 2, shellID: 'claude-mythos-8', displayName: 'Kimi', realModel: 'kimi-k2.6' })
    ])

    await wrapper.find('[data-testid="group-model-catalog-remove-1"]').trigger('click')

    expect(state.rows.map(item => item.key)).toEqual([2])
  })
})