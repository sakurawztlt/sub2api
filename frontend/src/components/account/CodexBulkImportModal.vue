<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.codexBulk.title')"
    width="wide"
    @close="handleClose"
  >
    <form id="codex-bulk-import-form" class="space-y-5" @submit.prevent="handleImport">
      <div class="rounded-xl border border-emerald-200 bg-emerald-50 p-4 text-sm text-emerald-800 dark:border-emerald-800/40 dark:bg-emerald-900/20 dark:text-emerald-200">
        {{ t('admin.accounts.codexBulk.intro') }}
      </div>

      <div class="grid gap-4 md:grid-cols-2">
        <div>
          <label class="input-label">{{ t('admin.accounts.codexBulk.batchId') }}</label>
          <input v-model="form.batch_id" type="text" class="input" />
          <p class="input-hint">{{ t('admin.accounts.codexBulk.batchIdHint') }}</p>
        </div>
        <div>
          <label class="input-label">{{ t('admin.accounts.codexBulk.nameTemplate') }}</label>
          <input v-model="form.name_template" type="text" class="input font-mono" />
          <p class="input-hint">{{ t('admin.accounts.codexBulk.nameTemplateHint') }}</p>
        </div>
        <div>
          <label class="input-label">{{ t('admin.accounts.codexBulk.accountsPerProxy') }}</label>
          <input v-model.number="form.accounts_per_proxy" type="number" min="1" class="input" />
          <p class="input-hint">{{ t('admin.accounts.codexBulk.accountsPerProxyHint') }}</p>
        </div>
        <div>
          <label class="input-label">{{ t('admin.accounts.codexBulk.concurrency') }}</label>
          <input v-model.number="form.concurrency" type="number" min="1" class="input" />
        </div>
        <div>
          <label class="input-label">{{ t('admin.accounts.codexBulk.priority') }}</label>
          <input v-model.number="form.priority" type="number" min="0" class="input" />
        </div>
        <div>
          <label class="input-label">{{ t('admin.accounts.codexBulk.rateMultiplier') }}</label>
          <input v-model.number="form.rate_multiplier" type="number" min="0" step="0.01" class="input" />
        </div>
      </div>

      <div>
        <label class="input-label">{{ t('admin.accounts.codexBulk.notes') }}</label>
        <textarea v-model="form.notes" rows="2" class="input" :placeholder="t('admin.accounts.codexBulk.notesPlaceholder')"></textarea>
      </div>

      <div>
        <label class="input-label">{{ t('admin.accounts.codexBulk.groups') }}</label>
        <GroupSelector v-model="form.group_ids" :groups="groups" platform="openai" />
      </div>

      <div class="space-y-3">
        <div class="flex items-center justify-between gap-3">
          <label class="input-label mb-0">{{ t('admin.accounts.codexBulk.proxyPool') }}</label>
          <div class="flex flex-wrap gap-2">
            <button type="button" class="btn btn-secondary px-3 py-1.5 text-xs" @click="selectEligibleProxies">
              {{ t('admin.accounts.codexBulk.selectEligible') }}
            </button>
            <button type="button" class="btn btn-secondary px-3 py-1.5 text-xs" @click="clearProxySelection">
              {{ t('common.clear') }}
            </button>
          </div>
        </div>

        <input
          v-model="proxySearch"
          type="text"
          class="input"
          :placeholder="t('admin.proxies.searchProxies')"
        />

        <div class="grid gap-3 md:grid-cols-3">
          <div class="rounded-xl border border-gray-200 bg-gray-50 p-3 dark:border-dark-600 dark:bg-dark-800">
            <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.codexBulk.selectedProxyCount') }}</div>
            <div class="mt-1 text-lg font-semibold text-gray-900 dark:text-white">{{ selectedProxyCount }}</div>
          </div>
          <div class="rounded-xl border border-gray-200 bg-gray-50 p-3 dark:border-dark-600 dark:bg-dark-800">
            <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.codexBulk.eligibleProxyCount') }}</div>
            <div class="mt-1 text-lg font-semibold text-gray-900 dark:text-white">{{ eligibleProxyCount }}</div>
          </div>
          <div class="rounded-xl border border-gray-200 bg-gray-50 p-3 dark:border-dark-600 dark:bg-dark-800">
            <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.codexBulk.estimatedCapacity') }}</div>
            <div class="mt-1 text-lg font-semibold text-gray-900 dark:text-white">{{ estimatedCapacity }}</div>
          </div>
        </div>

        <div class="rounded-xl border border-gray-200 bg-gray-50 p-3 dark:border-dark-600 dark:bg-dark-800">
          <div class="flex items-center justify-between gap-3">
            <div>
              <div class="text-sm font-medium text-gray-900 dark:text-white">{{ t('admin.accounts.codexBulk.allocatableIpsTitle') }}</div>
              <div class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.codexBulk.allocatableIpsHint') }}</div>
            </div>
            <span class="rounded bg-white px-2 py-1 text-xs text-gray-600 shadow-sm dark:bg-dark-700 dark:text-gray-300">
              {{ filteredAllocatableProxyMeta.length }}
            </span>
          </div>

          <div
            data-testid="codex-bulk-allocatable-ips"
            class="mt-3 max-h-44 space-y-2 overflow-y-auto"
          >
            <div
              v-for="proxy in filteredAllocatableProxyMeta"
              :key="`alloc-${proxy.id}`"
              data-testid="codex-bulk-allocatable-ip-item"
              class="flex items-center justify-between gap-3 rounded-lg border border-gray-200 bg-white px-3 py-2 text-sm dark:border-dark-600 dark:bg-dark-900"
            >
              <div class="min-w-0">
                <div class="font-medium text-gray-900 dark:text-white">{{ proxy.host }}:{{ proxy.port }}</div>
                <div class="mt-1 truncate text-xs text-gray-500 dark:text-gray-400">
                  {{ proxy.name }}<span v-if="proxy.country_code"> · {{ proxy.country_code }}</span><span v-if="proxy.protocol"> · {{ proxy.protocol }}</span>
                </div>
              </div>
              <span class="shrink-0 rounded bg-emerald-100 px-2 py-1 text-xs text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300">
                {{ t('admin.accounts.codexBulk.allocatableCapacity', { count: proxy.allocatable_capacity }) }}
              </span>
            </div>
            <div v-if="!filteredAllocatableProxyMeta.length" class="py-4 text-center text-sm text-gray-500 dark:text-gray-400">
              {{ t('admin.accounts.codexBulk.noAllocatableIps') }}
            </div>
          </div>
        </div>

        <div class="max-h-72 space-y-2 overflow-y-auto rounded-xl border border-gray-200 bg-white p-3 dark:border-dark-600 dark:bg-dark-900">
          <label
            v-for="proxy in filteredProxyMeta"
            :key="proxy.id"
            class="flex cursor-pointer items-start gap-3 rounded-lg border px-3 py-2 transition-colors"
            :class="proxy.selected
              ? 'border-primary-300 bg-primary-50 dark:border-primary-600/50 dark:bg-primary-900/20'
              : 'border-gray-200 hover:border-gray-300 dark:border-dark-600 dark:hover:border-dark-500'"
          >
            <input
              type="checkbox"
              class="mt-1 h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
              :checked="proxy.selected"
              :disabled="!proxy.eligible"
              @change="toggleProxySelection(proxy.id, ($event.target as HTMLInputElement).checked)"
            />
            <div class="min-w-0 flex-1">
              <div class="flex flex-wrap items-center gap-2">
                <span class="font-medium text-gray-900 dark:text-white">{{ proxy.name }}</span>
                <span class="rounded bg-gray-100 px-2 py-0.5 text-xs text-gray-600 dark:bg-dark-700 dark:text-gray-300">
                  {{ proxy.protocol }}://{{ proxy.host }}:{{ proxy.port }}
                </span>
                <span
                  class="rounded px-2 py-0.5 text-xs"
                  :class="proxy.eligible
                    ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
                    : 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'"
                >
                  {{ proxy.eligible ? t('admin.accounts.codexBulk.eligible') : t('admin.accounts.codexBulk.unavailable') }}
                </span>
              </div>
              <div class="mt-2 flex flex-wrap gap-2 text-xs text-gray-500 dark:text-gray-400">
                <span>{{ t('admin.accounts.codexBulk.accountCount', { count: proxy.account_count || 0 }) }}</span>
                <span>{{ t('admin.accounts.codexBulk.allocatableCapacity', { count: proxy.allocatable_capacity }) }}</span>
                <span v-if="proxy.quality_grade">{{ t('admin.accounts.codexBulk.qualityGrade', { grade: proxy.quality_grade, score: proxy.quality_score ?? '-' }) }}</span>
                <span v-if="proxy.latency_status">{{ t('admin.accounts.codexBulk.latencyStatus', { status: proxy.latency_status }) }}</span>
              </div>
            </div>
          </label>
          <div v-if="!filteredProxyMeta.length" class="py-6 text-center text-sm text-gray-500 dark:text-gray-400">
            {{ t('admin.accounts.codexBulk.noProxies') }}
          </div>
        </div>
      </div>

      <div class="space-y-3">
        <div class="flex items-center justify-between gap-3">
          <label class="input-label mb-0">{{ t('admin.accounts.codexBulk.refreshTokens') }}</label>
          <div class="flex flex-wrap gap-2">
            <button type="button" class="btn btn-secondary px-3 py-1.5 text-xs" @click="openFilePicker">
              {{ t('admin.accounts.codexBulk.uploadFile') }}
            </button>
            <button type="button" class="btn btn-secondary px-3 py-1.5 text-xs" @click="clearRefreshTokens">
              {{ t('common.clear') }}
            </button>
          </div>
          <input ref="fileInput" type="file" class="hidden" accept=".txt,.csv,text/plain" @change="handleFileChange" />
        </div>
        <textarea
          id="codex-bulk-refresh-tokens"
          v-model="refreshTokensText"
          rows="10"
          class="input font-mono text-sm"
          :placeholder="t('admin.accounts.codexBulk.refreshTokensPlaceholder')"
        ></textarea>
        <div class="flex flex-wrap gap-3 text-xs text-gray-500 dark:text-gray-400">
          <span>{{ t('admin.accounts.codexBulk.parsedCount', { count: parsedRefreshTokens.length }) }}</span>
          <span>{{ t('admin.accounts.codexBulk.selectedCapacity', { count: estimatedCapacity }) }}</span>
        </div>
      </div>

      <div v-if="importResult" class="space-y-4 rounded-xl border border-gray-200 p-4 dark:border-dark-600">
        <div class="flex items-center justify-between gap-3">
          <h3 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.accounts.codexBulk.resultTitle') }}</h3>
          <span class="rounded bg-gray-100 px-2 py-1 text-xs text-gray-600 dark:bg-dark-700 dark:text-gray-300">
            {{ importResult.batch_id }}
          </span>
        </div>

        <div class="grid gap-3 md:grid-cols-4">
          <div class="rounded-xl border border-gray-200 bg-gray-50 p-3 dark:border-dark-600 dark:bg-dark-800">
            <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.codexBulk.previewRequested') }}</div>
            <div class="mt-1 text-lg font-semibold text-gray-900 dark:text-white">{{ importResult.summary.requested_count }}</div>
          </div>
          <div class="rounded-xl border border-gray-200 bg-gray-50 p-3 dark:border-dark-600 dark:bg-dark-800">
            <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.codexBulk.resultCreated') }}</div>
            <div class="mt-1 text-lg font-semibold text-emerald-600 dark:text-emerald-400">{{ importResult.summary.created_count ?? 0 }}</div>
          </div>
          <div class="rounded-xl border border-gray-200 bg-gray-50 p-3 dark:border-dark-600 dark:bg-dark-800">
            <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.codexBulk.previewFailedLabel') }}</div>
            <div class="mt-1 text-lg font-semibold text-red-600 dark:text-red-400">{{ importResult.summary.failed_count }}</div>
          </div>
          <div class="rounded-xl border border-gray-200 bg-gray-50 p-3 dark:border-dark-600 dark:bg-dark-800">
            <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.codexBulk.previewRemainingCapacity') }}</div>
            <div class="mt-1 text-lg font-semibold text-gray-900 dark:text-white">{{ importResult.summary.remaining_capacity }}</div>
          </div>
        </div>

        <div class="grid gap-3 lg:grid-cols-2">
          <div class="space-y-2">
            <div class="text-sm font-medium text-gray-900 dark:text-white">{{ t('admin.accounts.codexBulk.proxyAllocation') }}</div>
            <div class="max-h-64 space-y-2 overflow-y-auto">
              <div
                v-for="allocation in importResult.proxy_allocations"
                :key="allocation.proxy_id"
                class="rounded-lg border border-gray-200 bg-gray-50 px-3 py-2 text-sm dark:border-dark-600 dark:bg-dark-800"
              >
                <div class="font-medium text-gray-900 dark:text-white">{{ allocation.proxy_name }}</div>
                <div class="mt-1 text-xs text-gray-500 dark:text-gray-400">
                  {{ t('admin.accounts.codexBulk.proxyAllocationLine', {
                    assigned: allocation.assigned_count,
                    current: allocation.account_count,
                    total: allocation.total_after_import
                  }) }}
                </div>
              </div>
            </div>
          </div>

          <div class="space-y-2">
            <div class="text-sm font-medium text-gray-900 dark:text-white">{{ t('admin.accounts.codexBulk.itemResults') }}</div>
            <div class="max-h-64 space-y-2 overflow-y-auto">
              <div
                v-for="item in importResult.items"
                :key="`${item.line_no}-${item.token_hint}`"
                class="rounded-lg border px-3 py-2 text-sm"
                :class="item.status === 'failed'
                  ? 'border-red-200 bg-red-50 dark:border-red-800/40 dark:bg-red-900/20'
                  : item.status === 'created'
                    ? 'border-emerald-200 bg-emerald-50 dark:border-emerald-800/40 dark:bg-emerald-900/20'
                    : 'border-gray-200 bg-gray-50 dark:border-dark-600 dark:bg-dark-800'"
              >
                <div class="flex items-center justify-between gap-3">
                  <div class="min-w-0">
                    <div class="font-medium text-gray-900 dark:text-white">{{ item.name }}</div>
                    <div class="text-xs text-gray-500 dark:text-gray-400">
                      #{{ item.line_no }} · {{ item.token_hint }}<span v-if="item.proxy_name"> · {{ item.proxy_name }}</span>
                    </div>
                  </div>
                  <span
                    class="rounded px-2 py-0.5 text-xs"
                    :class="item.status === 'failed'
                      ? 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
                      : item.status === 'created'
                        ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
                        : 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'"
                  >
                    {{ item.status }}
                  </span>
                </div>
                <div v-if="item.reason" class="mt-2 text-xs text-red-600 dark:text-red-300">{{ item.reason }}</div>
                <div v-else-if="item.email || item.plan_type" class="mt-2 text-xs text-gray-500 dark:text-gray-400">
                  <span v-if="item.email">{{ item.email }}</span>
                  <span v-if="item.email && item.plan_type"> · </span>
                  <span v-if="item.plan_type">{{ item.plan_type }}</span>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </form>

    <template #footer>
      <div class="flex flex-wrap justify-end gap-3">
        <button class="btn btn-secondary" type="button" :disabled="importing" @click="handleClose">
          {{ t('common.cancel') }}
        </button>
        <button
          type="submit"
          form="codex-bulk-import-form"
          class="btn btn-primary"
          data-testid="codex-bulk-import"
          :disabled="!canImport"
        >
          {{ importing ? t('admin.accounts.codexBulk.importing') : t('admin.accounts.codexBulk.importButton') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import GroupSelector from '@/components/common/GroupSelector.vue'
import { adminAPI } from '@/api/admin'
import { useAppStore } from '@/stores/app'
import type { AdminGroup, CodexBulkImportRequest, CodexBulkImportResult, Proxy } from '@/types'

interface Props {
  show: boolean
  groups: AdminGroup[]
}

interface Emits {
  (e: 'close'): void
  (e: 'created'): void
}

const props = defineProps<Props>()
const emit = defineEmits<Emits>()

const { t } = useI18n()
const appStore = useAppStore()

const defaultBatchId = () => {
  const now = new Date()
  const pad = (value: number) => value.toString().padStart(2, '0')
  return `${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}-${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}`
}

const defaultFormState = () => ({
  batch_id: defaultBatchId(),
  name_template: 'codex-{batch}-{index}',
  accounts_per_proxy: 4,
  group_ids: [] as number[],
  concurrency: 3,
  priority: 50,
  rate_multiplier: 1,
  notes: '',
  skip_default_group_bind: false
})

const form = reactive(defaultFormState())
const refreshTokensText = ref('')
const proxies = ref<Proxy[]>([])
const proxySearch = ref('')
const selectedProxyIds = ref<number[]>([])
const importResult = ref<CodexBulkImportResult | null>(null)
const importing = ref(false)
const fileInput = ref<HTMLInputElement | null>(null)

const parsedRefreshTokens = computed(() =>
  refreshTokensText.value
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean)
)

const proxyMeta = computed(() => {
  const selectedSet = new Set(selectedProxyIds.value)
  return proxies.value.map((proxy) => {
    const allocatableCapacity = Math.max((form.accounts_per_proxy || 0) - (proxy.account_count || 0), 0)
    const eligible =
      proxy.status === 'active' &&
      allocatableCapacity > 0 &&
      proxy.latency_status !== 'failed' &&
      proxy.quality_status !== 'failed' &&
      proxy.quality_status !== 'challenge'
    return {
      ...proxy,
      allocatable_capacity: allocatableCapacity,
      eligible,
      selected: selectedSet.has(proxy.id)
    }
  })
})

const filteredProxyMeta = computed(() => {
  const query = proxySearch.value.trim().toLowerCase()
  if (!query) {
    return proxyMeta.value
  }
  return proxyMeta.value.filter((proxy) => {
    return (
      proxy.name.toLowerCase().includes(query) ||
      proxy.host.toLowerCase().includes(query) ||
      proxy.protocol.toLowerCase().includes(query)
    )
  })
})

const filteredAllocatableProxyMeta = computed(() =>
  filteredProxyMeta.value.filter((proxy) => proxy.eligible && proxy.allocatable_capacity > 0)
)

const selectedProxyCount = computed(() => selectedProxyIds.value.length)
const eligibleProxyCount = computed(() => proxyMeta.value.filter((proxy) => proxy.eligible).length)
const estimatedCapacity = computed(() =>
  proxyMeta.value
    .filter((proxy) => selectedProxyIds.value.includes(proxy.id))
    .reduce((sum, proxy) => sum + proxy.allocatable_capacity, 0)
)

const payload = computed<CodexBulkImportRequest>(() => ({
  batch_id: form.batch_id.trim(),
  name_template: form.name_template.trim(),
  refresh_tokens: parsedRefreshTokens.value,
  proxy_pool_ids: [...selectedProxyIds.value].sort((a, b) => a - b),
  accounts_per_proxy: Number.isFinite(form.accounts_per_proxy) ? form.accounts_per_proxy : 4,
  group_ids: [...form.group_ids].sort((a, b) => a - b),
  concurrency: Number.isFinite(form.concurrency) ? form.concurrency : 3,
  priority: Number.isFinite(form.priority) ? form.priority : 50,
  rate_multiplier: Number.isFinite(form.rate_multiplier) ? form.rate_multiplier : undefined,
  notes: form.notes.trim() || null,
  skip_default_group_bind: form.skip_default_group_bind
}))

const canImport = computed(() =>
  !importing.value &&
  parsedRefreshTokens.value.length > 0 &&
  selectedProxyIds.value.length > 0
)

const resetState = () => {
  Object.assign(form, defaultFormState())
  refreshTokensText.value = ''
  proxySearch.value = ''
  proxies.value = []
  selectedProxyIds.value = []
  importResult.value = null
  importing.value = false
  if (fileInput.value) {
    fileInput.value.value = ''
  }
}

const loadProxies = async () => {
  const loaded = await adminAPI.proxies.getAllWithCount()
  proxies.value = loaded
  selectedProxyIds.value = proxyMeta.value.filter((proxy) => proxy.eligible).map((proxy) => proxy.id)
}

watch(
  () => props.show,
  async (open) => {
    if (!open) return
    resetState()
    try {
      await loadProxies()
    } catch (error: any) {
      appStore.showError(error?.message || t('admin.accounts.codexBulk.failedToLoadProxies'))
    }
  },
  { immediate: true }
)

watch(payload, () => {
  if (importing.value) return
  importResult.value = null
}, { deep: true })

const toggleProxySelection = (proxyId: number, checked: boolean) => {
  if (checked) {
    if (!selectedProxyIds.value.includes(proxyId)) {
      selectedProxyIds.value = [...selectedProxyIds.value, proxyId]
    }
    return
  }
  selectedProxyIds.value = selectedProxyIds.value.filter((id) => id !== proxyId)
}

const selectEligibleProxies = () => {
  selectedProxyIds.value = proxyMeta.value.filter((proxy) => proxy.eligible).map((proxy) => proxy.id)
}

const clearProxySelection = () => {
  selectedProxyIds.value = []
}

const clearRefreshTokens = () => {
  refreshTokensText.value = ''
}

const openFilePicker = () => {
  fileInput.value?.click()
}

const readFileAsText = async (file: File): Promise<string> => {
  if (typeof file.text === 'function') {
    return file.text()
  }
  return await new Promise<string>((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result ?? ''))
    reader.onerror = () => reject(reader.error || new Error('Failed to read file'))
    reader.readAsText(file)
  })
}

const handleFileChange = async (event: Event) => {
  const target = event.target as HTMLInputElement
  const file = target.files?.[0]
  if (!file) return
  try {
    const text = await readFileAsText(file)
    refreshTokensText.value = [refreshTokensText.value.trim(), text.trim()].filter(Boolean).join('\n')
  } catch (error: any) {
    appStore.showError(error?.message || t('admin.accounts.codexBulk.failedToReadFile'))
  } finally {
    target.value = ''
  }
}

const validateBeforeSubmit = (): boolean => {
  if (parsedRefreshTokens.value.length === 0) {
    appStore.showError(t('admin.accounts.codexBulk.refreshTokensRequired'))
    return false
  }
  if (selectedProxyIds.value.length === 0) {
    appStore.showError(t('admin.accounts.codexBulk.proxyPoolRequired'))
    return false
  }
  return true
}

const handleImport = async () => {
  if (!validateBeforeSubmit()) return
  importing.value = true
  try {
    const result = await adminAPI.accounts.createCodexBulkImport(payload.value)
    importResult.value = result
    emit('created')
    appStore.showSuccess(t('admin.accounts.codexBulk.importDoneToast', {
      created: result.summary.created_count ?? 0,
      failed: result.summary.failed_count
    }))
  } catch (error: any) {
    appStore.showError(error?.message || t('admin.accounts.codexBulk.importFailed'))
  } finally {
    importing.value = false
  }
}

const handleClose = () => {
  if (importing.value) return
  emit('close')
}
</script>
