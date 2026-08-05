import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import AccountUsageCell from '../AccountUsageCell.vue'
import type { Account } from '@/types'

const { getUsage } = vi.hoisted(() => ({ getUsage: vi.fn() }))

vi.mock('@/api/admin', () => ({
  adminAPI: { accounts: { getUsage } }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const account = {
  id: 99,
  name: 'Grok OAuth',
  platform: 'grok',
  type: 'oauth',
  proxy_id: null,
  concurrency: 1,
  priority: 1,
  status: 'active',
  error_message: null,
  last_used_at: null,
  expires_at: null,
  auto_pause_on_expired: true,
  created_at: '2026-08-05T00:00:00Z',
  updated_at: '2026-08-05T00:00:00Z',
  schedulable: true
} as Account

describe('AccountUsageCell Grok OAuth', () => {
  beforeEach(() => {
    getUsage.mockReset()
    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      value: vi.fn().mockImplementation(() => ({
        matches: true,
        media: '(min-width: 768px)',
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn()
      }))
    })
  })

  it('renders remaining request/token quota and compatible passive metadata', async () => {
    getUsage.mockResolvedValue({
      grok_request_quota: {
        limit: 100,
        remaining: 73,
        reset_at: '2026-08-05T01:00:00Z'
      },
      grok_token_quota: {
        limit: 10_000,
        remaining: 8_000,
        reset_at: '2026-08-05T01:00:00Z'
      },
      grok_retry_after_seconds: 120,
      grok_quota_snapshot_state: 'observed',
      grok_entitlement_status: 'active',
      grok_last_status_code: 200,
      grok_last_quota_probe_at: '2026-08-05T00:00:00Z',
      grok_last_headers_seen_at: '2026-08-05T00:00:01Z',
      grok_local_usage: { requests: 3, tokens: 4200, cost: 0.5, user_cost: 0.75 }
    })

    const wrapper = mount(AccountUsageCell, {
      props: { account },
      global: {
        stubs: {
          UsageProgressBar: {
            props: ['label', 'utilization', 'resetsAt', 'remainingCapacity'],
            template:
              '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ resetsAt }}|{{ remainingCapacity }}</div>'
          },
          AccountQuotaInfo: true,
          GrokQuotaProbeCell: { template: '<button class="grok-probe">probe</button>' }
        }
      }
    })

    await flushPromises()

    expect(getUsage).toHaveBeenCalledWith(99)
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.grokRequests|73')
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.grokTokens|80')
    expect(wrapper.text()).toContain('|true')
    expect(wrapper.text()).toContain('active')
    expect(wrapper.text()).toContain('3 req')
    expect(wrapper.text()).toContain('4.2K')
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.grokRetryAfter')
    expect(wrapper.find('.grok-probe').exists()).toBe(true)
  })

  it('explains the no-header state instead of inventing quota', async () => {
    getUsage.mockResolvedValue({
      grok_quota_snapshot_state: 'no_headers',
      grok_last_status_code: 200
    })

    const wrapper = mount(AccountUsageCell, {
      props: { account },
      global: {
        stubs: {
          UsageProgressBar: true,
          AccountQuotaInfo: true,
          GrokQuotaProbeCell: true
        }
      }
    })
    await flushPromises()

    expect(wrapper.text()).toContain('admin.accounts.usageWindow.grokNoHeaders')
    expect(wrapper.find('.usage-bar').exists()).toBe(false)
  })
})
