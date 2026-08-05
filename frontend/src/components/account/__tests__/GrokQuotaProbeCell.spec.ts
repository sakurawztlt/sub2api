import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import GrokQuotaProbeCell from '../GrokQuotaProbeCell.vue'
import type { Account } from '@/types'

const { queryQuota } = vi.hoisted(() => ({ queryQuota: vi.fn() }))

vi.mock('@/api/admin', () => ({
  adminAPI: { grok: { queryQuota } }
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) =>
      params?.time == null ? key : `${key}:${params.time}`
  })
}))

const account = { id: 99, platform: 'grok', type: 'oauth' } as Account

describe('GrokQuotaProbeCell', () => {
  beforeEach(() => queryQuota.mockReset())

  it('renders the initial active-probe contract and emits the observation', async () => {
    queryQuota.mockResolvedValue({
      source: 'active_probe',
      snapshot: {
        requests: { limit: 100, remaining: 73 },
        tokens: { limit: 10_000, remaining: 8_000 },
        retry_after_seconds: 120,
        entitlement_status: 'active',
        headers_observed: true,
        updated_at: '2026-08-05T00:00:00Z'
      },
      status_code: 200,
      headers_observed: true,
      reset_supported: false,
      fetched_at: 1
    })
    const wrapper = mount(GrokQuotaProbeCell, { props: { account } })

    await wrapper.get('button').trigger('click')
    await flushPromises()

    expect(queryQuota).toHaveBeenCalledWith(99)
    expect(wrapper.text()).toContain('73/100')
    expect(wrapper.text()).toContain('8000/10000')
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.grokRetryAfter:2m')
    expect(wrapper.text()).toContain('active')
    expect(wrapper.emitted('probed')?.[0]?.[0]).toMatchObject({ source: 'active_probe' })
  })

  it('stays hidden for Grok API-key accounts', () => {
    const wrapper = mount(GrokQuotaProbeCell, {
      props: { account: { ...account, type: 'apikey' } as Account }
    })
    expect(wrapper.html()).toBe('<!--v-if-->')
  })
})
