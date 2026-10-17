<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

const MASK = '******'
const SECRET_KEYS = ['pass_token', 'service_token', 'mimo_ph'] as const
const CORE_KEYS = ['user_id', 'pass_token', 'service_token'] as const
const ADVANCED_KEYS = ['c_user_id', 'mimo_ph', 'base_url'] as const
const ALL_KEYS = [...CORE_KEYS, ...ADVANCED_KEYS] as const

const LABELS: Record<string, string> = {
  user_id: 'Xiaomi User ID (user_id)',
  pass_token: 'Pass Token (pass_token)',
  service_token: 'Service Token (service_token)',
  c_user_id: 'Encrypted User ID (c_user_id)',
  mimo_ph: 'MiMo PH Cookie (mimo_ph)',
  base_url: 'Base URL (base_url)'
}

const PLACEHOLDERS: Record<string, string> = {
  user_id: '如 100000001',
  pass_token: 'V1:...==（推荐填写，支持自动换取并续期 serviceToken）',
  service_token: '留空则由 pass_token 自动换取；若无 pass_token 则必填',
  c_user_id: '留空由 SSO 自动提取',
  mimo_ph: '留空由 SSO 自动提取',
  base_url: '默认 https://mimo-server-cn.xiaomimimo.com'
}

const { t } = useI18n()

interface Props {
  modelValue?: Record<string, string>
  credentialsStatus?: Record<string, boolean>
  isEdit?: boolean
}

const props = withDefaults(defineProps<Props>(), {
  modelValue: () => ({}),
  credentialsStatus: undefined,
  isEdit: false
})
const emit = defineEmits<{ (e: 'update:modelValue', value: Record<string, string>): void }>()

const fields = reactive<Record<string, string>>({
  user_id: '',
  pass_token: '',
  service_token: '',
  c_user_id: '',
  mimo_ph: '',
  base_url: ''
})
const errors = reactive<Record<string, string>>({})
const showAdvanced = ref(false)
let initialized = false

function isSecret(key: string): boolean {
  return (SECRET_KEYS as readonly string[]).includes(key)
}

function hasExistingSecret(key: string): boolean {
  if (!props.isEdit) return false
  if (props.credentialsStatus && typeof props.credentialsStatus[`has_${key}`] === 'boolean') {
    return props.credentialsStatus[`has_${key}`]
  }
  return true
}

watch(
  () => props.modelValue,
  (value) => {
    if (initialized || !value || ALL_KEYS.every((k) => !value[k])) return
    for (const key of ALL_KEYS) {
      const v = value[key] != null ? String(value[key]) : ''
      if (!v || v === MASK) continue
      fields[key] = props.isEdit && isSecret(key) ? '' : v
    }
    initialized = true
  },
  { immediate: true }
)

function collect(): Record<string, string> {
  const out: Record<string, string> = {}
  for (const key of ALL_KEYS) {
    const val = fields[key] != null ? String(fields[key]).trim() : ''
    out[key] = isSecret(key) && val === MASK ? '' : val
  }
  return out
}

function isValid(): boolean {
  let allValid = true
  const uid = fields.user_id?.trim()
  if (!uid || uid === MASK) {
    errors.user_id = `请填写 ${LABELS.user_id}`
    allValid = false
  } else {
    delete errors.user_id
  }
  const pt = fields.pass_token?.trim()
  const st = fields.service_token?.trim()
  const hasToken =
    Boolean((pt && pt !== MASK) || (st && st !== MASK)) ||
    hasExistingSecret('pass_token') ||
    hasExistingSecret('service_token')
  if (!hasToken) {
    const msg = '请填写 Pass Token (pass_token) 或 Service Token (service_token)（至少一项）'
    errors.pass_token = msg
    errors.service_token = msg
    allValid = false
  } else {
    delete errors.pass_token
    delete errors.service_token
  }
  return allValid
}

function getPlaceholder(key: (typeof ALL_KEYS)[number]): string {
  if (isSecret(key) && hasExistingSecret(key)) {
    return t('admin.accounts.leaveEmptyToKeep')
  }
  return PLACEHOLDERS[key] || ''
}

function getInputType(key: (typeof ALL_KEYS)[number]): string {
  return isSecret(key) ? 'password' : 'text'
}

watch(
  fields,
  () => {
    const collected = collect()
    isValid()
    emit('update:modelValue', collected)
  },
  { deep: true }
)

defineExpose({ collect, isValid })
</script>

<template>
  <div class="space-y-4">
    <div v-for="key in CORE_KEYS" :key="key">
      <label class="input-label">
        {{ LABELS[key] }}
        <span v-if="key === 'user_id'" class="text-red-500">*</span>
      </label>
      <input
        v-model="fields[key]"
        :type="getInputType(key)"
        class="input font-mono"
        :class="{ 'border-red-500': errors[key] }"
        :autocomplete="getInputType(key) === 'password' ? 'new-password' : 'off'"
        :placeholder="getPlaceholder(key)"
      />
      <p v-if="errors[key]" class="mt-1 text-xs text-red-500">{{ errors[key] }}</p>
      <p v-else-if="isSecret(key) && hasExistingSecret(key)" class="mt-1 text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.accounts.leaveEmptyToKeep') }}
      </p>
    </div>

    <div class="rounded-lg border border-gray-200 dark:border-dark-600">
      <button
        type="button"
        class="flex w-full items-center justify-between px-4 py-2.5 text-left text-xs font-medium text-gray-600 hover:text-gray-900 dark:text-gray-400 dark:hover:text-gray-200"
        data-testid="mimo-advanced-toggle"
        @click="showAdvanced = !showAdvanced"
      >
        <span>高级设置（选填）</span>
        <svg class="h-4 w-4 transition-transform" :class="{ 'rotate-180': showAdvanced }" fill="none" viewBox="0 0 24 24" stroke="currentColor">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7" />
        </svg>
      </button>
      <div v-if="showAdvanced" class="space-y-4 border-t border-gray-200 p-4 dark:border-dark-600">
        <div v-for="key in ADVANCED_KEYS" :key="key">
          <label class="input-label">{{ LABELS[key] }}</label>
          <input
            v-model="fields[key]"
            :type="getInputType(key)"
            class="input font-mono"
            :class="{ 'border-red-500': errors[key] }"
            :autocomplete="getInputType(key) === 'password' ? 'new-password' : 'off'"
            :placeholder="getPlaceholder(key)"
          />
          <p v-if="errors[key]" class="mt-1 text-xs text-red-500">{{ errors[key] }}</p>
          <p v-else-if="isSecret(key) && hasExistingSecret(key)" class="mt-1 text-xs text-gray-500 dark:text-gray-400">
            {{ t('admin.accounts.leaveEmptyToKeep') }}
          </p>
        </div>
      </div>
    </div>
  </div>
</template>
