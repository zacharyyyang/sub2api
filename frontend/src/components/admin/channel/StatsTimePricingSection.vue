<template>
  <section class="mt-3 border-t border-gray-200 pt-3 dark:border-dark-600">
    <div class="flex flex-col gap-2 sm:flex-row sm:items-end sm:justify-between">
      <div class="min-w-0 flex-1 sm:max-w-2xl">
        <label class="block text-xs font-medium text-gray-500 dark:text-gray-400">
          {{ t('admin.channels.form.statsTimePricing') }}
        </label>
        <p class="mt-1 text-xs text-gray-400">
          {{ t('admin.channels.form.statsTimePricingHint') }}
        </p>
        <div class="mt-2 min-w-0">
          <label class="block text-xs text-gray-400">
            {{ t('admin.channels.form.timezone') }}
          </label>
          <Select
            :model-value="modelValue.timezone"
            :options="timezoneOptions"
            :aria-label="t('admin.channels.form.timezone')"
            data-testid="stats-time-pricing-timezone"
            searchable
            creatable
            class="mt-1 w-full"
            @update:model-value="updateTimezone"
          />
        </div>
      </div>
      <button
        type="button"
        class="self-start text-xs text-primary-600 hover:text-primary-700 sm:self-end sm:pb-2"
        data-testid="add-stats-time-period"
        @click="addPeriod"
      >
        + {{ t('admin.channels.form.addTimePeriod') }}
      </button>
    </div>

    <div v-if="modelValue.periods.length > 0" class="mt-3 space-y-3">
      <div
        v-for="(period, index) in modelValue.periods"
        :key="index"
        class="border-t border-gray-200 pt-3 dark:border-dark-600"
        :data-testid="`stats-time-period-${index}`"
      >
        <div class="grid grid-cols-1 gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_2rem] sm:items-end">
          <div class="min-w-0">
            <label :for="`${inputIdPrefix}-start-${index}`" class="block text-xs text-gray-400">
              {{ t('admin.channels.form.startTime') }}
            </label>
            <input
              :id="`${inputIdPrefix}-start-${index}`"
              :value="period.start_time"
              type="text"
              inputmode="numeric"
              maxlength="8"
              placeholder="HH:mm:ss"
              pattern="[0-9]{2}:[0-9]{2}:[0-9]{2}"
              autocomplete="off"
              class="input mt-1 w-full text-sm"
              @input="updatePeriod(index, 'start_time', normalizeClockTime(($event.target as HTMLInputElement).value))"
            />
          </div>
          <div class="min-w-0">
            <label :for="`${inputIdPrefix}-end-${index}`" class="block text-xs text-gray-400">
              {{ t('admin.channels.form.endTime') }}
            </label>
            <input
              :id="`${inputIdPrefix}-end-${index}`"
              :value="period.end_time"
              type="text"
              inputmode="numeric"
              maxlength="8"
              placeholder="HH:mm:ss"
              pattern="[0-9]{2}:[0-9]{2}:[0-9]{2}"
              autocomplete="off"
              class="input mt-1 w-full text-sm"
              @input="updatePeriod(index, 'end_time', normalizeClockTime(($event.target as HTMLInputElement).value))"
            />
          </div>
          <button
            type="button"
            class="flex h-8 w-8 items-center justify-center rounded text-gray-400 hover:text-red-500"
            :title="t('admin.channels.form.removeTimePeriod')"
            :aria-label="t('admin.channels.form.removeTimePeriod')"
            :data-testid="`remove-stats-time-period-${index}`"
            @click="removePeriod(index)"
          >
            <Icon name="trash" size="sm" />
          </button>
        </div>

        <div class="stats-time-price-grid mt-2 grid gap-2">
          <div v-for="field in priceFields" :key="field.key">
            <label :for="`${inputIdPrefix}-${field.key}-${index}`" class="block text-xs text-gray-400">
              {{ t(field.labelKey) }}
              <span v-if="field.required && isBlank(period[field.key])" class="text-red-500">*</span>
              <span class="text-gray-300">{{ field.unit }}</span>
            </label>
            <input
              :id="`${inputIdPrefix}-${field.key}-${index}`"
              :value="period[field.key] ?? ''"
              type="number"
              step="any"
              min="0"
              class="input mt-1 w-full text-xs"
              :placeholder="t('admin.channels.form.pricePlaceholder')"
              @input="updatePeriod(index, field.key, ($event.target as HTMLInputElement).value)"
            />
          </div>
        </div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, getCurrentInstance } from 'vue'
import { useI18n } from 'vue-i18n'
import Select from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import {
  COMMON_TIMEZONES,
  formatTimezoneOffset,
  type StatsTimePricingFormEntry,
  type StatsTimePricingPeriodFormEntry,
} from './types'
import type { BillingMode } from '@/api/admin/channels'

const { t } = useI18n()

const props = defineProps<{
  modelValue: StatsTimePricingFormEntry
  billingMode: BillingMode
}>()
const emit = defineEmits<{ 'update:modelValue': [value: StatsTimePricingFormEntry] }>()
const inputIdPrefix = `stats-time-pricing-${getCurrentInstance()?.uid}`

const timezoneOptions = COMMON_TIMEZONES.map(value => {
  const offset = formatTimezoneOffset(value)
  return { value, label: offset ? `${value} (${offset})` : value }
})

type PriceFieldKey =
  | 'input_price'
  | 'output_price'
  | 'cache_write_price'
  | 'cache_read_price'
  | 'per_request_price'

interface PriceField {
  key: PriceFieldKey
  labelKey: string
  unit: string
  required: boolean
}

const isTokenMode = computed(() => !props.billingMode || props.billingMode === 'token')

/**
 * 段表列：开始 / 结束 / 输入价 / 输出价 / 缓存写价 / 缓存读价 / 按次价。
 * 缓存写 1h 价不在表内（界面不暴露），但随表单类型原样回写，避免保存时抹掉已配置的值。
 */
const priceFields = computed<PriceField[]>(() => [
  { key: 'input_price', labelKey: 'admin.channels.form.inputPrice', unit: '$/M', required: isTokenMode.value },
  { key: 'output_price', labelKey: 'admin.channels.form.outputPrice', unit: '$/M', required: isTokenMode.value },
  { key: 'cache_write_price', labelKey: 'admin.channels.form.cacheWritePriceShort', unit: '$/M', required: false },
  { key: 'cache_read_price', labelKey: 'admin.channels.form.cacheReadPriceShort', unit: '$/M', required: isTokenMode.value },
  { key: 'per_request_price', labelKey: 'admin.channels.form.perRequestPrice', unit: '$', required: !isTokenMode.value },
])

function isBlank(val: number | string | null | undefined): boolean {
  return val === null || val === undefined || val === ''
}

function updateTimezone(value: string | number | boolean | null) {
  emit('update:modelValue', { ...props.modelValue, timezone: String(value ?? '') })
}

function normalizeClockTime(value: string): string {
  const normalized = value.replace(/：/g, ':')
  return normalized === '24:00:00' ? '00:00:00' : normalized
}

function addPeriod() {
  emit('update:modelValue', {
    ...props.modelValue,
    periods: [
      ...props.modelValue.periods,
      {
        start_time: '',
        end_time: '',
        input_price: null,
        output_price: null,
        cache_write_price: null,
        cache_write_1h_price: null,
        cache_read_price: null,
        per_request_price: null,
      },
    ],
  })
}

function updatePeriod(index: number, field: keyof StatsTimePricingPeriodFormEntry, value: string) {
  const periods = props.modelValue.periods.map((period, current) =>
    current === index ? { ...period, [field]: value === '' ? null : value } : period)
  emit('update:modelValue', { ...props.modelValue, periods })
}

function removePeriod(index: number) {
  emit('update:modelValue', {
    ...props.modelValue,
    periods: props.modelValue.periods.filter((_period, current) => current !== index),
  })
}
</script>

<style scoped>
.stats-time-price-grid {
  grid-template-columns: repeat(auto-fit, minmax(6.5rem, 1fr));
}
</style>