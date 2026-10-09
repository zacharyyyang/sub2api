<script setup lang="ts">
import { reactive, watch } from 'vue'

/**
 * WB Enterprise 平台密钥凭证表单（创建 / 编辑共用）。
 * 四件套必填 = client_id / client_secret / pt_key / enterprise_id；cli_path 可选。
 *
 * 契约：
 * - props.modelValue：当前凭证（创建场景为空对象；编辑场景由父组件预填后端返回的原值）。
 * - emit('update:modelValue')：任何输入变化即同步完整五键对象。
 * - 密钥类字段一律密码框；编辑场景以掩码 `******` 回显，用户不改则不覆盖原值。
 * - expose.collect()：返回提交用凭证对象（掩码/空值回落到原值，未填项为空串）。
 * - expose.isValid()：必填四键（回落后）是否全部非空。
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

const props = defineProps<{ modelValue: Record<string, string> }>()
const emit = defineEmits<{ (e: 'update:modelValue', value: Record<string, string>): void }>()

const fields = reactive<Record<string, string>>({ client_id: '', client_secret: '', pt_key: '', enterprise_id: '', cli_path: '' })
const originals = reactive<Record<string, string>>({})
const errors = reactive<Record<string, string>>({})
let initialized = false

// 编辑场景：只初始化一次（实例随弹窗打开而创建），预填值对密钥键以掩码呈现。
watch(
  () => props.modelValue,
  (value) => {
    if (initialized || !value || ALL_KEYS.every((k) => !value[k])) return
    for (const key of ALL_KEYS) {
      const v = value[key] || ''
      if (!v || v === MASK) continue
      originals[key] = v
      fields[key] = (SECRET_KEYS as readonly string[]).includes(key) ? MASK : v
    }
    initialized = true
  },
  { immediate: true }
)

/** 提交值：掩码 / 空回落为原值（未改不覆盖）。 */
function collect(): Record<string, string> {
  const out: Record<string, string> = {} as Record<string, string>
  for (const key of ALL_KEYS) {
    out[key] = fields[key] && fields[key] !== MASK ? fields[key] : originals[key] || ''
  }
  return out
}

function isValid(): boolean {
  const collected = collect()
  return REQUIRED_KEYS.every((k) => !!collected[k]?.trim())
}

// 输入即时校验 + 同步外抛。
watch(
  fields,
  () => {
    const collected = collect()
    for (const key of REQUIRED_KEYS) {
      const empty = !collected[key]?.trim()
      if (empty) {
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
        :placeholder="key === 'cli_path' ? '留空则自动探测本机 wb CLI' : ''"
      />
      <p v-if="errors[key]" class="mt-1 text-xs text-red-500">{{ errors[key] }}</p>
      <p v-else-if="key === 'cli_path'" class="mt-1 text-xs text-gray-500 dark:text-gray-400">
        可选。留空时自动探测本机安装的 wb CLI 路径。
      </p>
    </div>
  </div>
</template>