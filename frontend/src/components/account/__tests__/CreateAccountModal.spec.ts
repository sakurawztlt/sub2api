import { defineComponent, ref } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import CreateAccountModal from '../CreateAccountModal.vue'

const {
  getSettingsMock,
  getWebSearchEmulationConfigMock,
  listTlsProfilesMock,
  createAccountMock,
  probeUpstreamBillingMock,
  createOpenAICodexPATMock
} = vi.hoisted(() => ({
  getSettingsMock: vi.fn(),
  getWebSearchEmulationConfigMock: vi.fn(),
  listTlsProfilesMock: vi.fn(),
  createAccountMock: vi.fn(),
  probeUpstreamBillingMock: vi.fn(),
  createOpenAICodexPATMock: vi.fn()
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn(),
    showWarning: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    isSimpleMode: false
  })
}))

vi.mock('@/composables/useModelWhitelist', () => ({
  claudeModels: ['claude-3-7-sonnet'],
  getPresetMappingsByPlatform: vi.fn(() => []),
  getModelsByPlatform: vi.fn(() => ['claude-3-7-sonnet']),
  commonErrorCodes: [],
  buildModelMappingObject: vi.fn(() => undefined),
  fetchAntigravityDefaultMappings: vi.fn().mockResolvedValue([]),
  isValidWildcardPattern: vi.fn(() => true)
}))

vi.mock('@/composables/useQuotaNotifyState', () => ({
  useQuotaNotifyState: () => ({
    globalEnabled: ref(false),
    state: {
      daily: { enabled: null, threshold: null, thresholdType: null },
      weekly: { enabled: null, threshold: null, thresholdType: null },
      total: { enabled: null, threshold: null, thresholdType: null }
    },
    loadGlobalState: vi.fn(),
    writeToExtra: vi.fn()
  })
}))

function createOAuthStub() {
  return {
    authUrl: ref(''),
    sessionId: ref(''),
    loading: ref(false),
    error: ref(''),
    resetState: vi.fn(),
    parseSessionKeys: vi.fn(() => []),
    getCapabilities: vi.fn().mockResolvedValue({ ai_studio_oauth_enabled: false })
  }
}

vi.mock('@/composables/useAccountOAuth', () => ({
  useAccountOAuth: () => createOAuthStub()
}))

vi.mock('@/composables/useOpenAIOAuth', () => ({
  useOpenAIOAuth: () => createOAuthStub()
}))

vi.mock('@/composables/useGeminiOAuth', () => ({
  useGeminiOAuth: () => createOAuthStub()
}))

vi.mock('@/composables/useAntigravityOAuth', () => ({
  useAntigravityOAuth: () => createOAuthStub()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      create: createAccountMock,
      createOpenAICodexPAT: createOpenAICodexPATMock,
      probeUpstreamBilling: probeUpstreamBillingMock,
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false })
    },
    settings: {
      getSettings: getSettingsMock,
      getWebSearchEmulationConfig: getWebSearchEmulationConfigMock
    },
    tlsFingerprintProfiles: {
      list: listTlsProfilesMock
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

const BaseDialogStub = defineComponent({
  name: 'BaseDialogStub',
  props: {
    show: {
      type: Boolean,
      default: false
    }
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const ProxySelectorStub = defineComponent({
  name: 'ProxySelectorStub',
  props: {
    modelValue: {
      type: [Number, null],
      default: null
    }
  },
  emits: ['update:modelValue'],
  template: '<div data-testid="proxy-selector">{{ modelValue ?? "" }}</div>'
})

const SelectStub = defineComponent({
  name: 'SelectStub',
  props: {
    modelValue: {
      type: [String, Number, Boolean, null],
      default: ''
    },
    options: {
      type: Array,
      default: () => []
    }
  },
  emits: ['update:modelValue'],
  template: `
    <select :value="modelValue" @change="$emit('update:modelValue', $event.target.value)">
      <option v-for="option in options" :key="option.value" :value="option.value">
        {{ option.label }}
      </option>
    </select>
  `
})

const GroupSelectorStub = defineComponent({
  name: 'GroupSelector',
  props: {
    modelValue: {
      type: Array,
      default: () => []
    }
  },
  emits: ['update:modelValue'],
  template: '<button type="button" data-testid="select-pricing-groups" @click="$emit(\'update:modelValue\', [1, 2])">groups</button>'
})

const ModelWhitelistSelectorStub = defineComponent({
  name: 'ModelWhitelistSelector',
  props: {
    modelValue: { type: Array, default: () => [] },
    platform: String,
    syncCredentials: Object
  },
  emits: ['update:modelValue'],
  template: '<div data-testid="model-whitelist-selector" />'
})

const OAuthAuthorizationFlowStub = defineComponent({
  name: 'OAuthAuthorizationFlow',
  props: {
    showCodexSessionImportOption: Boolean,
    showCodexPatOption: Boolean
  },
  emits: ['import-codex-pat'],
  template: `
    <button
      v-if="showCodexPatOption"
      type="button"
      data-testid="import-codex-pat"
      @click="$emit('import-codex-pat', 'at-test-token')"
    >pat</button>
  `
})

const createWrapper = (groups: any[] = [], show = false) =>
  mount(CreateAccountModal, {
    props: {
      show,
      proxies: [
        {
          id: 7,
          name: 'Default Proxy',
          protocol: 'http',
          host: 'proxy.example.com',
          port: 8080,
          username: null,
          password: null,
          status: 'active',
          created_at: '2026-04-27T00:00:00Z',
          updated_at: '2026-04-27T00:00:00Z'
        }
      ] as any,
      groups
    },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        ConfirmDialog: true,
        Select: SelectStub,
        Toggle: true,
        Icon: true,
        ProxySelector: ProxySelectorStub,
        GroupSelector: GroupSelectorStub,
        ModelWhitelistSelector: ModelWhitelistSelectorStub,
        QuotaLimitCard: true,
        OAuthAuthorizationFlow: OAuthAuthorizationFlowStub
      }
    }
  })

async function selectButtonByText(wrapper: ReturnType<typeof createWrapper>, text: string) {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes(text))
  expect(button).toBeDefined()
  await button?.trigger('click')
}

const createDeferred = <T>() => {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise
  })
  return { promise, resolve }
}

describe('CreateAccountModal', () => {
  beforeEach(() => {
    getSettingsMock.mockReset()
    getWebSearchEmulationConfigMock.mockReset()
    listTlsProfilesMock.mockReset()
    createAccountMock.mockReset().mockResolvedValue({ id: 42, platform: 'openai', type: 'apikey' })
    createOpenAICodexPATMock.mockReset().mockResolvedValue({ id: 43, platform: 'openai', type: 'oauth' })
    probeUpstreamBillingMock.mockReset().mockResolvedValue({})

    getSettingsMock.mockResolvedValue({ default_proxy_id: 7 })
    getWebSearchEmulationConfigMock.mockResolvedValue({ enabled: false, providers: [] })
    listTlsProfilesMock.mockResolvedValue([])
  })

  it('打开时自动选中默认代理', async () => {
    const wrapper = createWrapper()

    await wrapper.setProps({ show: true })
    await flushPromises()

    expect(wrapper.get('[data-testid="proxy-selector"]').text()).toBe('7')
    expect(wrapper.text()).toContain('admin.accounts.defaultProxyAppliedHint')
  })

  it('关闭并重开时忽略上一次默认代理请求的迟到响应', async () => {
    const staleRequest = createDeferred<{ default_proxy_id: number | null }>()
    const currentRequest = createDeferred<{ default_proxy_id: number | null }>()
    getSettingsMock
      .mockReset()
      .mockReturnValueOnce(staleRequest.promise)
      .mockReturnValueOnce(currentRequest.promise)

    const wrapper = createWrapper()

    await wrapper.setProps({ show: true })
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    expect(getSettingsMock).toHaveBeenCalledTimes(2)

    currentRequest.resolve({ default_proxy_id: null })
    await flushPromises()
    expect(wrapper.get('[data-testid="proxy-selector"]').text()).toBe('')

    staleRequest.resolve({ default_proxy_id: 7 })
    await flushPromises()
    expect(wrapper.get('[data-testid="proxy-selector"]').text()).toBe('')
  })

  it('all selected long-context pricing groups hide only the redundant account toggle', async () => {
    const wrapper = createWrapper([
      { id: 1, long_context_pricing_enabled: true },
      { id: 2, long_context_pricing_enabled: true }
    ], true)

    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('[data-testid="select-pricing-groups"]').trigger('click')

    expect(wrapper.find('[data-testid="openai-long-context-billing-toggle"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="create-openai-ws-mode"]').exists()).toBe(true)
  })

  it('keeps the account long-context toggle when any selected group disables tier pricing', async () => {
    const wrapper = createWrapper([
      { id: 1, long_context_pricing_enabled: true },
      { id: 2, long_context_pricing_enabled: false }
    ], true)

    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('[data-testid="select-pricing-groups"]').trigger('click')

    expect(wrapper.find('[data-testid="openai-long-context-billing-toggle"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="create-openai-ws-mode"]').exists()).toBe(true)
  })

  it('uses the edited adaptive Kimi Chat endpoint for model preview credentials', async () => {
    const wrapper = createWrapper([], true)
    await selectButtonByText(wrapper, 'Kimi')
    await wrapper
      .get('[data-testid="cn-adaptive-base-url-chat_completions"]')
      .setValue('https://relay.example.com/v1')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-relay')

    expect(wrapper.getComponent(ModelWhitelistSelectorStub).props('syncCredentials')).toMatchObject({
      platform: 'kimi',
      type: 'apikey',
      base_url: 'https://relay.example.com/v1',
      api_key: 'sk-relay'
    })
  })

  it('submits adaptive Kimi protocol endpoints', async () => {
    const wrapper = createWrapper([], true)
    await selectButtonByText(wrapper, 'Kimi')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Kimi adaptive')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-kimi')

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      account_mode: 'payg',
      api_protocol: 'adaptive',
      base_url: 'https://api.moonshot.cn/v1',
      api_base_urls: {
        chat_completions: 'https://api.moonshot.cn/v1',
        anthropic: 'https://api.moonshot.cn/anthropic',
        responses: 'https://api.moonshot.cn/v1'
      }
    })
  })

  it('submits adaptive Kimi Coding Plan Responses endpoint', async () => {
    const wrapper = createWrapper([], true)
    await selectButtonByText(wrapper, 'Kimi')
    await selectButtonByText(wrapper, 'admin.accounts.cnProviders.accountMode.coding')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Kimi coding')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('sk-kimi-coding')

    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      account_mode: 'coding',
      api_protocol: 'adaptive',
      base_url: 'https://api.kimi.com/coding/v1',
      api_base_urls: {
        chat_completions: 'https://api.kimi.com/coding/v1',
        anthropic: 'https://api.kimi.com/coding',
        responses: 'https://api.kimi.com/coding/v1'
      }
    })
  })

  it('exposes the Codex PAT entry and submits the dedicated PAT payload', async () => {
    const wrapper = createWrapper([], true)
    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Codex PAT')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')

    const flow = wrapper.getComponent(OAuthAuthorizationFlowStub)
    expect(flow.props('showCodexPatOption')).toBe(true)
    await wrapper.get('[data-testid="import-codex-pat"]').trigger('click')
    await flushPromises()

    expect(createOpenAICodexPATMock).toHaveBeenCalledTimes(1)
    expect(createOpenAICodexPATMock.mock.calls[0]?.[0]).toMatchObject({
      access_token: 'at-test-token',
      name: 'Codex PAT'
    })
    expect(
      createOpenAICodexPATMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled
    ).toBeUndefined()
  })
})
