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
})
