import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { mount } from '@vue/test-utils'

const { updateAccountMock, checkMixedChannelRiskMock } = vi.hoisted(() => ({
  updateAccountMock: vi.fn(),
  checkMixedChannelRiskMock: vi.fn()
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ isSimpleMode: true })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      update: updateAccountMock,
      checkMixedChannelRisk: checkMixedChannelRiskMock
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: { list: vi.fn().mockResolvedValue([]) }
  }
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn()
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import EditAccountModal from '../EditAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

function buildGrokOAuthAccount(credentials: Record<string, unknown> = {}) {
  return {
    id: 5,
    name: 'Grok OAuth',
    notes: '',
    platform: 'grok',
    type: 'oauth',
    credentials: {
      refresh_token: 'grok-rt',
      expires_at: '2027-01-01T00:00:00Z',
      token_type: 'Bearer',
      ...credentials
    },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false
  } as any
}

function mountModal(account = buildGrokOAuthAccount()) {
  return mount(EditAccountModal, {
    props: { show: true, account, proxies: [], groups: [] },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: true,
        Icon: true,
        ProxySelector: true,
        GroupSelector: true,
        ModelWhitelistSelector: true
      }
    }
  })
}

describe('EditAccountModal Grok upstream config', () => {
  beforeEach(() => {
    updateAccountMock.mockReset()
    checkMixedChannelRiskMock.mockReset()
    checkMixedChannelRiskMock.mockResolvedValue({ has_risk: false })
  })

  it('persists an official xAI endpoint selected from the preset chips', async () => {
    const account = buildGrokOAuthAccount({ base_url: 'https://cli-chat-proxy.grok.com/v1' })
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    await wrapper.get('[data-testid="grok-custom-base-url-toggle"]').trigger('click')
    const presets = wrapper.findAll('[data-testid="grok-base-url-preset"]')
    expect(presets).toHaveLength(5)
    await presets[1].trigger('click')

    expect(
      (wrapper.get('[data-testid="grok-custom-base-url-input"]').element as HTMLInputElement).value
    ).toBe('https://api.x.ai/v1')

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalledTimes(1))
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.base_url).toBe(
      'https://api.x.ai/v1'
    )
  })

  it('echoes a stored regional endpoint with the custom toggle on', () => {
    const wrapper = mountModal(
      buildGrokOAuthAccount({ base_url: 'https://us-west-2.api.x.ai/v1' })
    )

    expect(wrapper.get('[data-testid="grok-custom-base-url-toggle"]').attributes('aria-checked')).toBe(
      'true'
    )
    expect(
      (wrapper.get('[data-testid="grok-custom-base-url-input"]').element as HTMLInputElement).value
    ).toBe('https://us-west-2.api.x.ai/v1')
  })

  it('keeps existing safe header overrides on an untouched save', async () => {
    const account = buildGrokOAuthAccount({
      header_override_enabled: true,
      header_overrides: {
        accept: 'application/json',
        'accept-language': 'zh-CN'
      }
    })
    updateAccountMock.mockResolvedValue(account)
    const wrapper = mountModal(account)

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await vi.waitFor(() => expect(updateAccountMock).toHaveBeenCalledTimes(1))

    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials).toMatchObject({
      header_override_enabled: true,
      header_overrides: {
        accept: 'application/json',
        'accept-language': 'zh-CN'
      }
    })
  })
})
