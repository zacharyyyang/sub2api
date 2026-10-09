<script setup lang="ts">
import { reactive, watch } from 'vue'
import { useI18n } from 'vue-i18n'

/**
 * WB Enterprise 平台密钥凭证表单（创建 / 编辑共用）。
 * 四件套必填 = client_id / client_secret / pt_key / enterprise_id；cli_path 可选。
 *
 * 契约：
 * - props.modelValue：当前凭证（创建场景为空对象；编辑场景由父组件预填后端返回的值）。
 * - props.credentialsStatus：敏感凭证存在性状态（如 has_client_secret / has_pt_key）。
 * - props.isEdit：是否为编辑模式。
 * - emit('update:modelValue')：任何输入变化即同步完整五键对象。
 * - 密钥类字段一律密码框；编辑场景输入框留空表示保持原值不变。
 * - expose.collect()：返回提交用凭证对象（密钥键未修改/留空时输出空串，触发父组件 delete wbCredentials[key] 以保留原值）。
 * - expose.isValid()：必填四键是否合法（编辑态下已有敏感凭据允许留空）。
 */
const MASK = '******'
const SECRET_KEYS = ['client_secret', 'pt_key'] as const
const ALL_KEYS = ['client_id', 'client_secret', 'pt_key', 'enterprise_id', 'cli_path'] as const
const REQUIRED_KEYS = ['client_id', 'client_secret', 'pt_key', 'enterprise_id'] as const

const LABELS: Record<string, string> = {
  client_id: 'Client ID',
  client_secret: 'Client Secret',
  pt_key: 'PT Key',
  enterprise_id: 'Enterprise ID',
  cli_path: 'CLI Path'
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

const fields = reactive<Record<string, string>>({ client_id: '', client_secret: '', pt_key: '', enterprise_id: '', cli_path: '' })
const originals = reactive<Record<string, string>>({})
const errors = reactive<Record<string, string>>({})
let initialized = false

/** 判断敏感密钥是否在后端已有持久化凭证 */
function hasExistingSecret(key: string): boolean {
  if (!props.isEdit) return false
  if (props.credentialsStatus && typeof props.credentialsStatus[`has_${key}`] === 'boolean') {
    return props.credentialsStatus[`has_${key}`]
  }
  if (originals[key] || (props.modelValue && props.modelValue[key])) {
    return true
  }
  // 编辑态下默认已有合法凭证（兼容未传 status 或旧后端）
  return true
}

// 编辑场景：只初始化一次（实例随弹窗打开而创建），非敏感键预填原值，敏感键保持空串由 placeholder 提示保持原值
watch(
  () => props.modelValue,
  (value) => {
    if (initialized || !value || ALL_KEYS.every((k) => !value[k])) return
    for (const key of ALL_KEYS) {
      const v = value[key] || ''
      if (!v || v === MASK) continue
      originals[key] = v
      fields[key] = (SECRET_KEYS as readonly string[]).includes(key) && props.isEdit ? '' : v
    }
    initialized = true
  },
  { immediate: true }
)

/** 提交值：用户输入新值则提交；非敏感键取当前输入值；敏感键未修改/留空时输出空串以便后端保持原库值 */
function collect(): Record<string, string> {
  const out: Record<string, string> = {} as Record<string, string>
  for (const key of ALL_KEYS) {
    if ((SECRET_KEYS as readonly string[]).includes(key)) {
      const val = fields[key]?.trim() || ''
      out[key] = val !== MASK ? val : ''
    } else {
      out[key] = fields[key]?.trim() || ''
    }
  }
  return out
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
  return allValid
}
function getPlaceholder(key: (typeof ALL_KEYS)[number]): string {
  if (key === 'cli_path') {
    return '留空则自动探测本机 wb CLI'
  }
  if ((SECRET_KEYS as readonly string[]).includes(key) && hasExistingSecret(key)) {
    return t('admin.accounts.leaveEmptyToKeep')
  }
  return ''
}

// 输入即时校验 + 同步外抛。
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
    emit('update:modelValue', collected)
  },
  { deep: true }
)

defineExpose({ collect, isValid })
</script>

<template>
  <div class="space-y-4">
    <div v-for="key in ALL_KEYS" :key="key">
      <label class="input-label">
        {{ LABELS[key] }}
        <span v-if="(REQUIRED_KEYS as readonly string[]).includes(key)" class="text-red-500">*</span>
      </label>
      <input
        v-model="fields[key]"
        :type="(SECRET_KEYS as readonly string[]).includes(key) ? 'password' : 'text'"
        class="input font-mono"
        :class="{ 'border-red-500': errors[key] }"
        :autocomplete="(SECRET_KEYS as readonly string[]).includes(key) ? 'new-password' : 'off'"
        :placeholder="getPlaceholder(key)"
      />
      <p v-if="errors[key]" class="mt-1 text-xs text-red-500">{{ errors[key] }}</p>
      <p v-else-if="key === 'cli_path'" class="mt-1 text-xs text-gray-500 dark:text-gray-400">
        可选。留空时自动探测本机安装的 wb CLI 路径。
      </p>
      <p v-else-if="(SECRET_KEYS as readonly string[]).includes(key) && hasExistingSecret(key)" class="mt-1 text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.accounts.leaveEmptyToKeep') }}
      </p>
    </div>
  </div>
</template>
