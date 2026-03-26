import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import CodexBulkImportModal from '../CodexBulkImportModal.vue'
import { adminAPI } from '@/api/admin'

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    proxies: {
      getAllWithCount: vi.fn()
    },
    accounts: {
      createCodexBulkImport: vi.fn()
    }
  }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

function mountModal() {
  return mount(CodexBulkImportModal, {
    props: {
      show: true,
      groups: []
    },
    global: {
      stubs: {
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' },
        GroupSelector: { template: '<div />' }
      }
    }
  })
}

describe('CodexBulkImportModal', () => {
  beforeEach(() => {
    vi.mocked(adminAPI.proxies.getAllWithCount).mockReset()
    vi.mocked(adminAPI.accounts.createCodexBulkImport).mockReset()

    vi.mocked(adminAPI.proxies.getAllWithCount).mockResolvedValue([
      {
        id: 1,
        name: 'proxy-a',
        protocol: 'http',
        host: '10.0.0.1',
        port: 8080,
        username: null,
        password: null,
        status: 'active',
        account_count: 0,
        latency_status: 'success',
        quality_status: 'healthy',
        quality_score: 90,
        quality_grade: 'A',
        created_at: '',
        updated_at: ''
      },
      {
        id: 2,
        name: 'tokyo-node',
        protocol: 'socks5',
        host: '20.0.0.2',
        port: 1080,
        username: null,
        password: null,
        status: 'active',
        account_count: 1,
        latency_status: 'success',
        quality_status: 'healthy',
        quality_score: 88,
        quality_grade: 'B',
        created_at: '',
        updated_at: ''
      },
      {
        id: 3,
        name: 'blocked-node',
        protocol: 'http',
        host: '30.0.0.3',
        port: 8081,
        username: null,
        password: null,
        status: 'active',
        account_count: 4,
        latency_status: 'failed',
        quality_status: 'failed',
        quality_score: 10,
        quality_grade: 'D',
        created_at: '',
        updated_at: ''
      }
    ] as any)

    vi.mocked(adminAPI.accounts.createCodexBulkImport).mockResolvedValue({
      batch_id: 'batch-1',
      summary: {
        requested_count: 2,
        parsed_count: 2,
        created_count: 2,
        failed_count: 0,
        selected_proxy_count: 1,
        eligible_proxy_count: 1,
        accounts_per_proxy: 4,
        total_capacity: 4,
        remaining_capacity: 2
      },
      items: [
        { line_no: 1, token_hint: 'rt-1', name: 'codex-batch-1-001', status: 'created', proxy_id: 1, proxy_name: 'proxy-a' },
        { line_no: 2, token_hint: 'rt-2', name: 'codex-batch-1-002', status: 'created', proxy_id: 1, proxy_name: 'proxy-a' }
      ],
      proxy_allocations: [
        {
          proxy_id: 1,
          proxy_name: 'proxy-a',
          account_count: 0,
          allocatable_capacity: 4,
          assigned_count: 2,
          total_after_import: 2
        }
      ]
    } as any)
  })

  it('parses refresh tokens and submits import payload with selected proxy pool', async () => {
    const wrapper = mountModal()
    await flushPromises()

    await wrapper.get('#codex-bulk-refresh-tokens').setValue('rt-1\n\nrt-2')
    await wrapper.get('#codex-bulk-import-form').trigger('submit')
    await flushPromises()

    expect(adminAPI.accounts.createCodexBulkImport).toHaveBeenCalledTimes(1)
    expect(adminAPI.accounts.createCodexBulkImport).toHaveBeenCalledWith(
      expect.objectContaining({
        refresh_tokens: ['rt-1', 'rt-2'],
        proxy_pool_ids: [1, 2],
        accounts_per_proxy: 4
      })
    )

    expect(wrapper.text()).toContain('admin.accounts.codexBulk.resultTitle')
  })

  it('filters proxies by search text', async () => {
    const wrapper = mountModal()
    await flushPromises()

    const searchInput = wrapper.get('input[placeholder="admin.proxies.searchProxies"]')
    await searchInput.setValue('tokyo')

    expect(wrapper.text()).toContain('tokyo-node')
    expect(wrapper.text()).not.toContain('proxy-a')
    expect(wrapper.get('[data-testid="codex-bulk-allocatable-ips"]').text()).toContain('20.0.0.2:1080')
    expect(wrapper.get('[data-testid="codex-bulk-allocatable-ips"]').text()).not.toContain('10.0.0.1:8080')
    expect(wrapper.get('[data-testid="codex-bulk-allocatable-ips"]').text()).not.toContain('30.0.0.3:8081')
  })
})
