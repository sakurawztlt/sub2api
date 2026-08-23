import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get, post } = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: { get, post }
}))

import {
  authorizePassword,
  createFromSSO,
  exchangeCode,
  generateAuthUrl,
  getGrokSSOImportTimeout,
  queryQuota,
  refreshGrokToken,
  resetQuota
} from '@/api/admin/grok'

describe('admin Grok API', () => {
  beforeEach(() => {
    get.mockReset()
    post.mockReset()
  })

  it('uses the backend OAuth routes and unwraps their payloads', async () => {
    post
      .mockResolvedValueOnce({
        data: { auth_url: 'https://accounts.x.ai/oauth', session_id: 'session-1', state: 'state-1' }
      })
      .mockResolvedValueOnce({ data: { access_token: 'access-1', refresh_token: 'refresh-1' } })
      .mockResolvedValueOnce({ data: { access_token: 'access-2' } })

    await expect(generateAuthUrl({ proxy_id: 7 })).resolves.toMatchObject({
      session_id: 'session-1',
      state: 'state-1'
    })
    await expect(
      exchangeCode({ session_id: 'session-1', state: 'state-1', code: 'code-1', proxy_id: 7 })
    ).resolves.toMatchObject({ refresh_token: 'refresh-1' })
    await expect(refreshGrokToken('refresh-1', 7)).resolves.toMatchObject({
      access_token: 'access-2'
    })

    expect(post).toHaveBeenNthCalledWith(1, '/admin/grok/oauth/auth-url', { proxy_id: 7 })
    expect(post).toHaveBeenNthCalledWith(2, '/admin/grok/oauth/exchange-code', {
      session_id: 'session-1',
      state: 'state-1',
      code: 'code-1',
      proxy_id: 7
    })
    expect(post).toHaveBeenNthCalledWith(3, '/admin/grok/oauth/refresh-token', {
      refresh_token: 'refresh-1',
      proxy_id: 7
    })
  })

  it('uses the initial active-probe and unsupported-reset routes', async () => {
    get.mockResolvedValueOnce({
      data: {
        source: 'active_probe',
        snapshot: {
          requests: { limit: 100, remaining: 75 },
          headers_observed: true,
          updated_at: '2026-08-05T00:00:00Z'
        },
        headers_observed: true,
        reset_supported: false,
        fetched_at: 1
      }
    })
    post.mockResolvedValueOnce({
      data: { supported: false, code: 'GROK_QUOTA_RESET_UNSUPPORTED', message: 'unsupported' }
    })

    await expect(queryQuota(42)).resolves.toMatchObject({
      source: 'active_probe',
      headers_observed: true
    })
    await expect(resetQuota(42)).resolves.toMatchObject({ supported: false })

    expect(get).toHaveBeenCalledWith('/admin/grok/accounts/42/quota')
    expect(post).toHaveBeenCalledWith('/admin/grok/accounts/42/reset-quota')
  })

  it('preserves password whitespace and applies the authorization timeout', async () => {
    post.mockResolvedValueOnce({ data: { access_token: 'access-token' } })

    await authorizePassword(' user@example.com ----  password with spaces  ', 7)

    expect(post).toHaveBeenCalledWith(
      '/admin/grok/oauth/password',
      {
        email: 'user@example.com',
        password: '  password with spaces  ',
        proxy_id: 7
      },
      { timeout: 120_000 }
    )
  })

  it.each([
    [1, 180_000],
    [3, 180_000],
    [4, 270_000],
    [7, 360_000]
  ])('uses a bounded-batch timeout sized for %i SSO keys', async (keyCount, expectedTimeout) => {
    post.mockResolvedValueOnce({ data: { created: [], failed: [] } })
    expect(getGrokSSOImportTimeout(keyCount)).toBe(expectedTimeout)

    const payload = {
      sso_tokens: Array.from({ length: keyCount }, (_, index) => `sso-${index + 1}`),
      credentials: { base_url: 'https://relay.example.com/v1' }
    }
    await createFromSSO(payload)

    expect(post).toHaveBeenCalledWith('/admin/grok/sso-to-oauth', payload, {
      timeout: expectedTimeout
    })
  })
})
