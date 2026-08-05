<template>
  <div v-if="visible" class="space-y-1">
    <div class="flex flex-wrap items-center gap-1.5">
      <button
        type="button"
        class="inline-flex min-h-6 items-center gap-0.5 rounded px-1.5 py-0.5 text-[10px] font-medium text-cyan-700 transition-colors hover:bg-cyan-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-500 disabled:cursor-not-allowed disabled:opacity-50 dark:text-cyan-300 dark:hover:bg-cyan-900/30"
        :disabled="loading"
        :title="t('admin.accounts.usageWindow.grokProbeTooltip')"
        @click="handleProbe"
      >
        <svg
          class="h-2.5 w-2.5"
          :class="{ 'animate-spin': loading }"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
          aria-hidden="true"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
          />
        </svg>
        {{ t('admin.accounts.usageWindow.grokProbe') }}
      </button>

      <span
        class="inline-flex min-h-6 cursor-not-allowed items-center rounded px-1.5 py-0.5 text-[10px] font-medium text-gray-400 opacity-70 dark:text-gray-500"
        :title="t('admin.accounts.usageWindow.grokResetUnsupportedTooltip')"
      >
        {{ t('admin.accounts.usageWindow.grokResetUnsupported') }}
      </span>
    </div>

    <div v-if="summary" class="text-[10px] text-gray-600 dark:text-gray-300">
      {{ summary }}
    </div>
    <div v-if="error" class="max-w-[220px] truncate text-[10px] text-red-600 dark:text-red-400" :title="error">
      {{ truncatedError }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { GrokQuotaProbeResult, GrokQuotaWindow } from '@/api/admin/grok'
import type { Account } from '@/types'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{
  account: Account
}>()

const emit = defineEmits<{
  probed: [result: GrokQuotaProbeResult]
}>()

const { t } = useI18n()

const visible = computed(() => props.account.platform === 'grok' && props.account.type === 'oauth')
const loading = ref(false)
const error = ref<string | null>(null)
const data = ref<GrokQuotaProbeResult | null>(null)

const formatWindow = (label: string, window?: GrokQuotaWindow | null): string | null => {
  if (!window || window.limit == null || window.remaining == null) return null
  return `${label} ${window.remaining}/${window.limit}`
}

const retryAfterLabel = computed(() => {
  const seconds = data.value?.snapshot?.retry_after_seconds
  if (seconds == null || seconds <= 0) return null
  if (seconds < 60) return `${seconds}s`
  return `${Math.ceil(seconds / 60)}m`
})

const summary = computed(() => {
  const snapshot = data.value?.snapshot
  if (!data.value) return ''
  const billing = data.value.billing
  const parts: Array<string | null> = []
  if (billing?.period_type?.toLowerCase() === 'weekly' && billing.usage_percent != null) {
    parts.push(t('admin.accounts.usageWindow.grokWeeklyUsage', {
      percent: Math.round(Math.min(100, Math.max(0, billing.usage_percent)))
    }))
  }
  if (snapshot) {
    parts.push(
      formatWindow(t('admin.accounts.usageWindow.grokRequests'), snapshot.requests),
      formatWindow(t('admin.accounts.usageWindow.grokTokens'), snapshot.tokens)
    )
  }
  if (retryAfterLabel.value) {
    parts.push(t('admin.accounts.usageWindow.grokRetryAfter', { time: retryAfterLabel.value }))
  }
  if (snapshot?.entitlement_status) {
    parts.push(snapshot.entitlement_status)
  }
  const visibleParts = parts.filter((part): part is string => Boolean(part))
  return visibleParts.length > 0
    ? visibleParts.join(' | ')
    : t('admin.accounts.usageWindow.grokNoHeaders')
})

const truncatedError = computed(() => {
  if (!error.value) return ''
  return error.value.length > 80 ? `${error.value.slice(0, 80)}...` : error.value
})

const handleProbe = async () => {
  if (loading.value) return
  loading.value = true
  error.value = null
  try {
    data.value = await adminAPI.grok.queryQuota(props.account.id)
    error.value = data.value.probe_error || null
    emit('probed', data.value)
  } catch (err: unknown) {
    error.value = extractApiErrorMessage(err, t('common.error'))
  } finally {
    loading.value = false
  }
}

watch(
  () => props.account.id,
  () => {
    data.value = null
    error.value = null
    loading.value = false
  }
)
</script>
