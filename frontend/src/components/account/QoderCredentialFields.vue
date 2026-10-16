<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

/**
 * Qoder 平台凭证表单（创建 / 编辑共用）。
 * 必填四项 = access_token / refresh_token / uid / org_id；
 * 可折叠高级设置五项（选填） = machine_id / umid_token / machine_type / token_expires_at / refresh_token_expires_at。
 *
 * 契约：
 * - props.modelValue：当前凭证（创建场景为空对象；编辑场景由父组件预填后端返回的值）。
 * - props.credentialsStatus：敏感凭证存在性状态（has_access_token / has_refresh_token / has_umid_token）。
 * - props.isEdit：是否为编辑模式。
 * - emit('update:modelValue')：任何输入变化即同步完整九键对象。
 * - 密钥类字段一律密码框；编辑场景输入框留空表示保持原值不变。
 * - expose.collect()：返回提交用凭证对象（密钥键未修改/留空时输出空串，触发父组件 delete 对应键以保留原值）。
 * - expose.isValid()：必填四键是否合法（编辑态下已有敏感凭据允许留空）。
 */
const MASK = '******'
const SECRET_KEYS = ['access_token', 'refresh_token', 'umid_token'] as const
const REQUIRED_KEYS = ['access_token', 'refresh_token', 'uid', 'org_id'] as const
const ADVANCED_KEYS = [
  'machine_id',
  'umid_token',
  'machine_type',
  'token_expires_at',
  'refresh_token_expires_at'
] as const
const NUMERIC_KEYS = ['token_expires_at', 'refresh_token_expires_at'] as const
const ALL_KEYS = [...REQUIRED_KEYS, ...ADVANCED_KEYS] as const

const LABELS: Record<string, string> = {
  access_token: 'Access Token (dt-...)',
  refresh_token: 'Refresh Token (drt-...)',
  uid: 'User ID (uid)',
  org_id: 'Organization ID (org_id)',
  machine_id: 'Machine ID (machine_id)',
  umid_token: 'UMID Token (umid_token)',
  machine_type: 'Machine Type (machine_type)',
  token_expires_at: 'Token Expires At (ms)',
  refresh_token_expires_at: 'Refresh Token Expires At (ms)'
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
  access_token: '',
  refresh_token: '',
  uid: '',
  org_id: '',
  machine_id: '',
  umid_token: '',
  machine_type: '',
  token_expires_at: '',
  refresh_token_expires_at: ''
})
const originals = reactive<Record<string, string>>({})
const errors = reactive<Record<string, string>>({})
const showAdvanced = ref(false)
let initialized = false

/** 判断敏感密钥是否在后端已有持久化凭证 */
function hasExistingSecret(key: string): boolean {
  if (!props.isEdit) return false
  if (props.credentialsStatus && typeof props.credentialsStatus[`has_${key}`] === 'boolean') {
    return props.credentialsStatus[`has_${key}`]
  }
  // 兼容未下发 credentials_status 的旧响应：编辑态下默认已有敏感凭证
  return true
}

// 编辑场景：只初始化一次，非敏感非时间戳键预填原值，敏感键与自动维护的过期时间戳保持空串由 placeholder/hint 提示保持原值
watch(
  () => props.modelValue,
  (value) => {
    if (initialized || !value || ALL_KEYS.every((k) => !value[k])) return
    for (const key of ALL_KEYS) {
      const v = value[key] != null ? String(value[key]) : ''
      if (!v || v === MASK) continue
      originals[key] = v
      const shouldLeaveBlankInEdit =
        props.isEdit &&
        ((SECRET_KEYS as readonly string[]).includes(key) ||
          (NUMERIC_KEYS as readonly string[]).includes(key))
      fields[key] = shouldLeaveBlankInEdit ? '' : v
    }
    initialized = true
  },
  { immediate: true }
)

/** 提交值：用户输入新值则提交；非敏感键取当前输入值；敏感键与编辑态未修改的过期时间戳输出空串以便后端保持原库值 */
function collect(): Record<string, string> {
  const out: Record<string, string> = {}
  for (const key of ALL_KEYS) {
    const val = fields[key] != null ? String(fields[key]).trim() : ''
    if ((SECRET_KEYS as readonly string[]).includes(key)) {
      out[key] = val !== MASK ? val : ''
    } else {
      out[key] = val
    }
  }
  return out
}

function isNumericKeyValid(key: (typeof NUMERIC_KEYS)[number]): boolean {
  const val = fields[key] != null ? String(fields[key]).trim() : ''
  if (!val) return true
  return /^[1-9]\d{12}$/.test(val)
}

function isKeyValid(key: (typeof REQUIRED_KEYS)[number]): boolean {
  const val = fields[key]?.trim()
  if (val && val !== MASK) return true
  if ((SECRET_KEYS as readonly string[]).includes(key) && hasExistingSecret(key)) {
    return true
  }
  return false
}

function isValid(): boolean {
  let allValid = true
  for (const key of REQUIRED_KEYS) {
    if (!isKeyValid(key)) {
      errors[key] = `请填写 ${LABELS[key]}`
      allValid = false
    } else {
      delete errors[key]
    }
  }
  for (const key of NUMERIC_KEYS) {
    if (!isNumericKeyValid(key)) {
      errors[key] = `${LABELS[key]} 需为 13 位毫秒级正整数时间戳`
      allValid = false
    } else {
      delete errors[key]
    }
  }
  return allValid
}

function getPlaceholder(key: (typeof ALL_KEYS)[number]): string {
  if ((SECRET_KEYS as readonly string[]).includes(key) && hasExistingSecret(key)) {
    return t('admin.accounts.leaveEmptyToKeep')
  }
  if ((NUMERIC_KEYS as readonly string[]).includes(key) && props.isEdit) {
    return t('admin.accounts.leaveEmptyToKeep')
  }
  switch (key) {
    case 'access_token':
      return 'dt-...'
    case 'refresh_token':
      return 'drt-...'
    case 'machine_id':
      return '留空自动根据 uid/org_id 派生稳定 64 位 hex'
    case 'umid_token':
      return '留空自动生成稳定设备标识'
    case 'machine_type':
      return '默认 win32'
    case 'token_expires_at':
    case 'refresh_token_expires_at':
      return '留空首次使用或刷新时自动回填'
    default:
      return ''
  }
}

function getInputType(key: (typeof ALL_KEYS)[number]): string {
  if ((SECRET_KEYS as readonly string[]).includes(key)) return 'password'
  if ((NUMERIC_KEYS as readonly string[]).includes(key)) return 'number'
  return 'text'
}

watch(
  fields,
  () => {
    const collected = collect()
    for (const key of REQUIRED_KEYS) {
      if (!isKeyValid(key)) {
        errors[key] = `请填写 ${LABELS[key]}`
      } else {
        delete errors[key]
      }
    }
    for (const key of NUMERIC_KEYS) {
      if (!isNumericKeyValid(key)) {
        errors[key] = `${LABELS[key]} 需为 13 位毫秒级正整数时间戳`
      } else {
        delete errors[key]
      }
    }
    emit('update:modelValue', collected)
  },
  { deep: true }
)

defineExpose({ collect, isValid })
</script>

<template>
  <div class="space-y-4">
    <div v-for="key in REQUIRED_KEYS" :key="key">
      <label class="input-label">
        {{ LABELS[key] }}
        <span class="text-red-500">*</span>
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
      <p
        v-else-if="(SECRET_KEYS as readonly string[]).includes(key) && hasExistingSecret(key)"
        class="mt-1 text-xs text-gray-500 dark:text-gray-400"
      >
        {{ t('admin.accounts.leaveEmptyToKeep') }}
      </p>
    </div>

    <div class="rounded-lg border border-gray-200 dark:border-dark-600">
      <button
        type="button"
        class="flex w-full items-center justify-between px-4 py-2.5 text-left text-xs font-medium text-gray-600 hover:text-gray-900 dark:text-gray-400 dark:hover:text-gray-200"
        data-testid="qoder-advanced-toggle"
        @click="showAdvanced = !showAdvanced"
      >
        <span>高级设置（选填）</span>
        <svg
          class="h-4 w-4 transition-transform"
          :class="{ 'rotate-180': showAdvanced }"
          fill="none"
          viewBox="0 0 24 24"
          stroke="currentColor"
        >
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
          <p
            v-else-if="
              ((SECRET_KEYS as readonly string[]).includes(key) && hasExistingSecret(key)) ||
              ((NUMERIC_KEYS as readonly string[]).includes(key) && props.isEdit)
            "
            class="mt-1 text-xs text-gray-500 dark:text-gray-400"
          >
            {{ t('admin.accounts.leaveEmptyToKeep') }}
          </p>
        </div>
      </div>
    </div>
  </div>
</template>
