import zhLocale from '@/i18n/locales/zh'
import enLocale from '@/i18n/locales/en'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, ref } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import OpsSystemLogTable from '../OpsSystemLogTable.vue'

const mockListSystemLogs = vi.fn()
const mockCleanupSystemLogs = vi.fn()

vi.mock('@vueuse/core', () => ({
  useMediaQuery: () => ref(true)
}))

vi.mock('@/api/admin/ops', () => ({
  opsAPI: {
    listSystemLogs: (...args: any[]) => mockListSystemLogs(...args),
    cleanupSystemLogs: (...args: any[]) => mockCleanupSystemLogs(...args),
    getSystemLogSinkHealth: vi.fn().mockResolvedValue({}),
    getRuntimeLogConfig: vi.fn().mockResolvedValue({
      level: 'info',
      enable_sampling: false,
      sampling_initial: 100,
      sampling_thereafter: 100,
      caller: true,
      stacktrace_level: 'error',
      retention_days: 30
    }),
    updateRuntimeLogConfig: vi.fn(),
    resetRuntimeLogConfig: vi.fn()
  }
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn()
  })
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

const SelectStub = defineComponent({
  props: { modelValue: { type: [String, Number], default: '' } },
  emits: ['update:modelValue'],
  template: '<div class="select-stub" />'
})

const PaginationStub = defineComponent({
  name: 'PaginationStub',
  template: '<div class="pagination-stub" />',
})

const runtimeConfig = {
  level: 'info',
  persist_access_logs: false,
  enable_sampling: false,
  sampling_initial: 100,
  sampling_thereafter: 100,
  caller: true,
  stacktrace_level: 'error',
  retention_days: 30,
}

const sinkHealth = {
  queue_depth: 0,
  queue_capacity: 5000,
  dropped_count: 0,
  write_failed_count: 0,
  written_count: 1,
  avg_write_delay_ms: 0,
}

describe('OpsSystemLogTable host support', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    mockListSystemLogs.mockResolvedValue({
      items: [{ id: 1, created_at: '2026-07-14T00:10:01Z', host: 'api-node-1', level: 'warn', component: 'app', message: 'request failed' }],
      total: 1,
      page: 1,
      page_size: 20
    })
    mockCleanupSystemLogs.mockResolvedValue({ deleted: 1 })
  })

  it('renders host and sends trimmed host in list and cleanup filters', async () => {
    const wrapper = mount(OpsSystemLogTable, {
      global: {
        stubs: {
          Select: SelectStub,
          Pagination: { template: '<div />' }
        }
      }
    })
    await flushPromises()

    expect(wrapper.text()).toContain('api-node-1')
    const hostLabel = wrapper.findAll('label').find((label) => label.text().includes('admin.ops.systemLogs.host'))
    expect(hostLabel).toBeDefined()
    await hostLabel!.find('input').setValue(' api-node-2 ')

    await wrapper.findAll('button').find((button) => button.text() === '查询')!.trigger('click')
    await flushPromises()
    expect(mockListSystemLogs).toHaveBeenLastCalledWith(expect.objectContaining({ host: 'api-node-2' }))

    await wrapper.findAll('button').find((button) => button.text() === '按当前筛选清理')!.trigger('click')
    await flushPromises()
    expect(mockCleanupSystemLogs).toHaveBeenCalledWith(expect.objectContaining({ host: 'api-node-2' }))
  })

  it('keeps database access-log persistence opt-in', async () => {
    const wrapper = mount(OpsSystemLogTable, {
      global: {
        stubs: {
          Select: SelectStub,
          Pagination: PaginationStub,
        },
      },
    })
    await flushPromises()

    const label = wrapper.findAll('label').find((item) => item.text().includes('admin.ops.systemLogs.persistAccessLogs'))
    expect(label).toBeDefined()
    expect((label!.find('input').element as HTMLInputElement).checked).toBe(false)
  })

  it.each([
    ['zh', zhLocale],
    ['en', enLocale],
  ])('defines the Host translation for %s', (_name, locale) => {
    expect(locale.admin.ops.systemLogs.host).toBe('Host')
  })
})
