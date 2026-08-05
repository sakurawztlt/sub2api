import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import OAuthAuthorizationFlow from '../OAuthAuthorizationFlow.vue'

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

vi.mock('@/composables/useClipboard', async () => {
  const { ref } = await import('vue')
  return {
    useClipboard: () => ({
      copied: ref(false),
      copyToClipboard: vi.fn()
    })
  }
})

describe('OAuthAuthorizationFlow Grok callback', () => {
  it('uses Grok copy and extracts code and state from the callback URL', async () => {
    const wrapper = mount(OAuthAuthorizationFlow, {
      props: {
        addMethod: 'oauth',
        platform: 'grok',
        authUrl: 'https://accounts.x.ai/oauth/authorize'
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    const callbackInput = wrapper.get(
      'textarea[placeholder="admin.accounts.oauth.grok.authCodePlaceholder"]'
    )
    await callbackInput.setValue('http://localhost:8085/callback?code=grok-code&state=grok-state')

    expect((wrapper.vm as unknown as { authCode: string }).authCode).toBe('grok-code')
    expect((wrapper.vm as unknown as { oauthState: string }).oauthState).toBe('grok-state')
    expect(wrapper.text()).toContain('admin.accounts.oauth.grok.importantNotice')
  })
})
