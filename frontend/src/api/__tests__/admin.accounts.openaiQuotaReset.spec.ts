import { beforeEach, describe, expect, it, vi } from 'vitest'

const { post } = vi.hoisted(() => ({
  post: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: { post }
}))

import { refreshOpenAIQuota, resetOpenAIQuota } from '@/api/admin/accounts'

describe('admin OpenAI quota reset API', () => {
  beforeEach(() => {
    post.mockReset()
  })

  it('refreshes quota through the audited snapshot endpoint', async () => {
    const result = {
      fetched_at: 1770000000,
      cache_persisted: true,
      rate_limit_reset_credits: { available_count: 2 }
    }
    post.mockResolvedValueOnce({ data: result })

    await expect(refreshOpenAIQuota(7)).resolves.toEqual(result)
    expect(post).toHaveBeenCalledWith('/admin/openai/accounts/7/quota/refresh')
  })

  it('uses an extended timeout for the non-refundable reset operation', async () => {
    const result = {
      code: 'success',
      windows_reset: 1,
      cache_refreshed: true,
      account_state_recovered: true
    }
    post.mockResolvedValueOnce({ data: result })

    await expect(resetOpenAIQuota(7)).resolves.toEqual(result)
    expect(post).toHaveBeenCalledWith(
      '/admin/openai/accounts/7/reset-quota',
      undefined,
      { timeout: 90_000 }
    )
  })
})
