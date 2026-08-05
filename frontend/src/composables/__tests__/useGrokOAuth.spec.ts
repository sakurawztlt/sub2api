import { beforeEach, describe, expect, it, vi } from 'vitest'

const { generateAuthUrl, exchangeCode, refreshGrokToken, showError } = vi.hoisted(() => ({
  generateAuthUrl: vi.fn(),
  exchangeCode: vi.fn(),
  refreshGrokToken: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError })
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key.replace('admin.accounts.oauth.grok.errors.GROK_OAUTH_INVALID_STATE', 'OAuth state is invalid')
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    grok: { generateAuthUrl, exchangeCode, refreshGrokToken }
  }
}))

import { useGrokOAuth } from '@/composables/useGrokOAuth'

describe('useGrokOAuth', () => {
  beforeEach(() => {
    generateAuthUrl.mockReset()
    exchangeCode.mockReset()
    refreshGrokToken.mockReset()
    showError.mockReset()
  })

  it('tracks the OAuth session and exchanges a code with its state and proxy', async () => {
    generateAuthUrl.mockResolvedValue({
      auth_url: 'https://accounts.x.ai/oauth',
      session_id: 'session-1',
      state: 'state-1'
    })
    exchangeCode.mockResolvedValue({ access_token: 'access-1' })

    const oauth = useGrokOAuth()
    await expect(oauth.generateAuthUrl(7)).resolves.toBe(true)
    expect(oauth.authUrl.value).toBe('https://accounts.x.ai/oauth')
    expect(oauth.sessionId.value).toBe('session-1')
    expect(oauth.state.value).toBe('state-1')

    await expect(
      oauth.exchangeAuthCode({
        code: ' code-1 ',
        sessionId: oauth.sessionId.value,
        state: oauth.state.value,
        proxyId: 7
      })
    ).resolves.toMatchObject({ access_token: 'access-1' })
    expect(exchangeCode).toHaveBeenCalledWith({
      code: 'code-1',
      session_id: 'session-1',
      state: 'state-1',
      proxy_id: 7
    })
  })

  it('builds relay-ready credentials without dropping subscription identity fields', () => {
    const oauth = useGrokOAuth()

    expect(
      oauth.buildCredentials({
        access_token: 'access-token',
        refresh_token: 'refresh-token',
        token_type: 'Bearer',
        expires_at: 1_900_000_000,
        client_id: 'client-id',
        scope: 'openid grok-cli:access',
        email: 'grok@example.com',
        sub: 'user-1',
        team_id: 'team-1',
        subscription_tier: 'SuperGrok'
      })
    ).toMatchObject({
      access_token: 'access-token',
      refresh_token: 'refresh-token',
      sub: 'user-1',
      team_id: 'team-1',
      subscription_tier: 'SuperGrok',
      base_url: 'https://cli-chat-proxy.grok.com/v1'
    })
  })

  it('validates manual refresh tokens through the backend route', async () => {
    refreshGrokToken.mockResolvedValue({ access_token: 'access-2', refresh_token: 'refresh-2' })
    const oauth = useGrokOAuth()

    await expect(oauth.validateRefreshToken(' refresh-1 ', 9)).resolves.toMatchObject({
      access_token: 'access-2'
    })
    expect(refreshGrokToken).toHaveBeenCalledWith('refresh-1', 9)
  })

  it('maps backend OAuth reason codes to actionable localized errors', async () => {
    exchangeCode.mockRejectedValue({ reason: 'GROK_OAUTH_INVALID_STATE' })
    const oauth = useGrokOAuth()

    await expect(
      oauth.exchangeAuthCode({ code: 'code-1', sessionId: 'session-1', state: 'state-1' })
    ).resolves.toBeNull()
    expect(oauth.error.value).toBe('OAuth state is invalid')
    expect(showError).toHaveBeenCalledWith('OAuth state is invalid')
  })
})
