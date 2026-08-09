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

  it('renders current paid billing, prepaid balance, and retry metadata', async () => {
    getUsage.mockResolvedValue({
      grok_billing: {
        plan: 'SuperGrok',
        period_type: 'weekly',
        usage_percent: 25,
        used_percent: 40,
        prepaid_balance: 12.5,
        monthly_used: 8,
        monthly_limit: 20,
        monthly_limit_cents: 2000,
        period_end: '2026-08-12T00:00:00Z',
        billing_period_end: '2026-09-05T00:00:00Z'
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
    expect(wrapper.text()).toContain('7d|25')
    expect(wrapper.text()).toContain('30d|40')
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.grokPrepaid $12.5')
    expect(wrapper.text()).toContain('8.00/20.0')
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
