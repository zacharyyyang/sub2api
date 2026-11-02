<template>
  <div data-testid="group-model-catalog" class="border-b border-gray-200 dark:border-dark-600">
    <div
      class="flex items-center justify-between gap-2 border-b border-gray-200 bg-gray-50 px-3 py-2 text-xs dark:border-dark-600 dark:bg-dark-800"
    >
      <span class="font-medium text-gray-600 dark:text-gray-300">
        {{ t(`${i18nPrefix}.title`) }}
      </span>
      <span class="text-gray-400 dark:text-gray-500">
        {{ t(`${i18nPrefix}.hint`) }}
      </span>
    </div>
    <table class="w-full table-fixed border-collapse text-xs">
      <thead data-testid="group-model-catalog-headers">
        <tr class="text-left text-gray-500 dark:text-gray-400">
          <th class="w-[30%] px-2 py-1.5 font-medium">
            {{ t(`${i18nPrefix}.columns.shellID`) }}
          </th>
          <th class="w-[30%] px-2 py-1.5 font-medium">
            {{ t(`${i18nPrefix}.columns.displayName`) }}
          </th>
          <th class="w-[26%] px-2 py-1.5 font-medium">
            {{ t(`${i18nPrefix}.columns.realModel`) }}
          </th>
          <th class="w-[10%] px-2 py-1.5 font-medium">
            {{ t(`${i18nPrefix}.columns.supports1M`) }}
          </th>
          <th class="w-[4%] px-2 py-1.5">
            <span class="sr-only">{{ t(`${i18nPrefix}.columns.remove`) }}</span>
          </th>
        </tr>
      </thead>
      <tbody>
        <tr v-if="rows.length === 0" data-testid="group-model-catalog-empty">
          <td colspan="5" class="px-2 py-2 text-gray-500 dark:text-gray-400">
            {{ t(`${i18nPrefix}.empty`) }}
          </td>
        </tr>
        <tr
          v-for="row in rows"
          :key="row.key"
          :data-testid="`group-model-catalog-row-${row.key}`"
          class="border-t border-gray-100 dark:border-dark-700"
          :class="rowErrors[row.key]?.length ? 'bg-red-50 dark:bg-red-900/20' : ''"
        >
          <td class="px-2 py-1.5">
            <input
              :value="row.shellID"
              type="text"
              :data-testid="`group-model-catalog-shell-${row.key}`"
              :aria-label="t(`${i18nPrefix}.columns.shellID`)"
              :placeholder="t(`${i18nPrefix}.shellIDPlaceholder`)"
              class="w-full rounded border border-gray-300 bg-white px-2 py-1 text-sm text-gray-700 focus:border-primary-500 focus:outline-none dark:border-dark-500 dark:bg-dark-700 dark:text-gray-200"
              @input="onShellIDInput(row.key, $event)"
            />
          </td>
          <td class="px-2 py-1.5">
            <input
              :ref="el => registerDisplayNameInput(row.key, el)"
              :value="row.displayName"
              type="text"
              :data-testid="`group-model-catalog-display-name-${row.key}`"
              :aria-label="t(`${i18nPrefix}.columns.displayName`)"
              :placeholder="t(`${i18nPrefix}.displayNamePlaceholder`)"
              class="w-full rounded border border-gray-300 bg-white px-2 py-1 text-sm text-gray-700 focus:border-primary-500 focus:outline-none dark:border-dark-500 dark:bg-dark-700 dark:text-gray-200"
              @input="onDisplayNameInput(row.key, $event)"
            />
          </td>
          <td class="px-2 py-1.5">
            <input
              :value="row.realModel"
              type="text"
              :list="realModelListID"
              :data-testid="`group-model-catalog-real-model-${row.key}`"
              :aria-label="t(`${i18nPrefix}.columns.realModel`)"
              :placeholder="t(`${i18nPrefix}.realModelPlaceholder`)"
              class="w-full rounded border border-gray-300 bg-white px-2 py-1 text-sm text-gray-700 focus:border-primary-500 focus:outline-none dark:border-dark-500 dark:bg-dark-700 dark:text-gray-200"
              @input="onRealModelInput(row.key, $event)"
            />
          </td>
          <td class="px-2 py-1.5">
            <input
              :checked="row.supports1M"
              type="checkbox"
              :data-testid="`group-model-catalog-1m-${row.key}`"
              :aria-label="t(`${i18nPrefix}.columns.supports1M`)"
              class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
              @change="onSupports1MToggle(row.key, $event)"
            />
          </td>
          <td class="px-2 py-1.5">
            <button
              type="button"
              :data-testid="`group-model-catalog-remove-${row.key}`"
              :aria-label="t(`${i18nPrefix}.columns.remove`)"
              class="rounded p-1 text-gray-400 hover:bg-gray-100 hover:text-gray-700 dark:hover:bg-dark-600 dark:hover:text-gray-200"
              @click="remove(row.key)"
            >
              <Icon name="trash" size="sm" />
            </button>
          </td>
        </tr>
      </tbody>
    </table>
    <datalist :id="realModelListID">
      <option v-for="model in realModelOptions" :key="model" :value="model" />
    </datalist>
    <div
      class="flex items-center justify-between gap-2 border-t border-gray-200 px-3 py-2 dark:border-dark-600"
    >
      <button
        type="button"
        data-testid="group-model-catalog-add"
        class="rounded bg-primary-600 px-3 py-1.5 text-sm font-medium text-white transition-colors hover:bg-primary-700"
        @click="append"
      >
        {{ t(`${i18nPrefix}.add`) }}
      </button>
    </div>
    <ul
      v-if="errorLines.length > 0"
      data-testid="group-model-catalog-errors"
      class="space-y-1 border-t border-gray-200 px-3 py-2 dark:border-dark-600"
    >
      <li
        v-for="line in errorLines"
        :key="line.key"
        class="text-xs text-red-500"
      >
        {{ line.text }}
      </li>
    </ul>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import {
  appendCatalogRow,
  catalogRealModelOptions,
  catalogRowErrorMessageKey,
  catalogRowErrors,
  nextGroupModelCatalogListID,
  removeCatalogRow,
  updateCatalogRow,
  type CatalogOverviewSource,
  type GroupModelCatalogRow,
} from '@/views/admin/groupModelCatalog'

// 目录表本体：列头 / 行 / 表尾「添加」/ 行内标红 / 空态。状态全部由父级持有
// （行集经 v-model:rows 回写），组件只做渲染与输入转发，判定规则来自 groupModelCatalog.ts。
const props = withDefaults(
  defineProps<{
    /** 目录行集（父级持有，本组件通过 update:rows 回写） */
    rows: GroupModelCatalogRow[]
    /** 校验与候选的对照面：白名单提交态 + 旧显示名键 + 平台候选 */
    source?: CatalogOverviewSource
    /** i18n 文案前缀，默认 admin.groups.modelCatalog */
    i18nPrefix?: string
  }>(),
  { source: () => ({}), i18nPrefix: 'admin.groups.modelCatalog' }
)

const emit = defineEmits<{
  'update:rows': [rows: GroupModelCatalogRow[]]
}>()

const { t } = useI18n()

// datalist 的 id 模块级递增：新建与编辑两个抽屉的目录表可能同时在 DOM 里。
const realModelListID = nextGroupModelCatalogListID()

const rowErrors = computed(() => catalogRowErrors(props.rows, props.source))
const realModelOptions = computed(() => catalogRealModelOptions(props.source))

const errorLines = computed(() => {
  const lines: { key: string; text: string }[] = []
  props.rows.forEach((row, index) => {
    for (const error of rowErrors.value[row.key] ?? []) {
      lines.push({
        key: `${row.key}-${error}`,
        text: t(`${props.i18nPrefix}.rowError`, {
          index: index + 1,
          reason: t(catalogRowErrorMessageKey(props.i18nPrefix, error))
        })
      })
    }
  })
  return lines
})

const displayNameInputs = new Map<number, HTMLInputElement>()

const registerDisplayNameInput = (key: number, element: unknown) => {
  if (element instanceof HTMLInputElement) {
    displayNameInputs.set(key, element)
    return
  }
  displayNameInputs.delete(key)
}

const inputValue = (event: Event): string => (event.target as HTMLInputElement).value

const patchRow = (key: number, patch: Partial<Omit<GroupModelCatalogRow, 'key'>>) => {
  emit('update:rows', updateCatalogRow(props.rows, key, patch))
}

const onShellIDInput = (key: number, event: Event) => patchRow(key, { shellID: inputValue(event) })
const onDisplayNameInput = (key: number, event: Event) =>
  patchRow(key, { displayName: inputValue(event) })
const onRealModelInput = (key: number, event: Event) =>
  patchRow(key, { realModel: inputValue(event) })
const onSupports1MToggle = (key: number, event: Event) =>
  patchRow(key, { supports1M: (event.target as HTMLInputElement).checked })

const remove = (key: number) => emit('update:rows', removeCatalogRow(props.rows, key))

// 添加：立即插入一行，壳 ID 预填下一个可用值，光标落到该行的显示名输入框。
const append = () => {
  const before = props.rows.length
  const next = appendCatalogRow(props.rows, props.source)
  emit('update:rows', next)
  const added = next[next.length - 1]
  if (next.length > before && added) {
    void nextTick(() => displayNameInputs.get(added.key)?.focus())
  }
}
</script>