<template>
  <template v-if="effectiveGroups.length > 0">
    <template v-for="group in effectiveGroups" :key="`${group.kind}:${group.label}:${group.domain ?? ''}`">
      <div class="group-name mt-1 text-[10px] text-gray-400">
        {{ groupLabel(group) }}
      </div>
      <UsageProgressBar
        v-for="window in group.windows"
        :key="window.bucket_id"
        :label="windowLabel(window)"
        :utilization="window.utilization"
        :resets-at="window.reset_time"
        :color="groupColor(group.kind)"
      />
    </template>
  </template>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { GoogleQuotaGroup, GoogleQuotaWindow } from '@/types'
import UsageProgressBar from './UsageProgressBar.vue'

const props = withDefaults(
  defineProps<{
    groups?: GoogleQuotaGroup[] | null
  }>(),
  { groups: () => [] }
)

const { t } = useI18n()

// 前端二次防御：非有限或负数的 utilization 窗口跳过（后端已清洗，此处只保证不显示 NaN / 负值）
function isValidWindow(window: GoogleQuotaWindow): boolean {
  return Number.isFinite(window.utilization) && window.utilization >= 0
}

// 组按后端返回顺序展示，不做前端二次排序；无有效窗口的组整体跳过
const effectiveGroups = computed(() => {
  if (!props.groups || props.groups.length === 0) return []
  const result: GoogleQuotaGroup[] = []
  for (const group of props.groups) {
    const windows = (group.windows ?? []).filter(isValidWindow)
    if (windows.length === 0) continue
    result.push({ ...group, windows })
  }
  return result
})

const groupLabel = (group: GoogleQuotaGroup): string => {
  if (group.kind === 'gemini') return t('admin.accounts.googleQuota.groupGemini')
  if (group.kind === 'claude_gpt') {
    // claude_gpt 组名行拼域标签（组词条 + · + 域词条），同组多域各有独立行
    const base = t('admin.accounts.googleQuota.groupClaudeGPT')
    if (!group.domain) return base // domain 缺省（v1 旧数据）不拼域，向后兼容
    return `${base} · ${t(`admin.accounts.googleQuota.domain.${group.domain}`)}`
  }
  // kind = other：优先上游 label，缺失时兜底本地词条
  return group.label || t('admin.accounts.googleQuota.groupOther')
}

const windowLabel = (window: GoogleQuotaWindow): string => {
  if (window.kind === 'five_hour') return t('admin.accounts.googleQuota.window5h')
  if (window.kind === 'seven_day') return t('admin.accounts.googleQuota.windowWeekly')
  // kind = other：与组名同规则——上游 label 缺失时兜底本地词条
  return window.label || t('admin.accounts.googleQuota.groupOther')
}

type BarColor = 'indigo' | 'amber' | 'purple'

const groupColor = (kind: GoogleQuotaGroup['kind']): BarColor => {
  if (kind === 'gemini') return 'indigo'
  if (kind === 'claude_gpt') return 'amber'
  return 'purple'
}
</script>