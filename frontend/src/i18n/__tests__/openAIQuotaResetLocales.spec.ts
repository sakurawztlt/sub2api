import { describe, expect, it } from 'vitest'

import en from '../locales/en'
import zh from '../locales/zh'

describe('OpenAI quota reset locale keys', () => {
  it('exposes the complete operator-state copy in English and Chinese', () => {
    for (const locale of [en, zh]) {
      expect(locale.admin.accounts.openaiQuotaReset).toMatchObject({
        count: expect.any(String),
        reset: expect.any(String),
        resetTooltipShadow: expect.any(String),
        resetTooltipNeedQuery: expect.any(String),
        resetCacheRefreshFailed: expect.any(String),
        resetAccountRecoveryFailed: expect.any(String),
        refreshCachePersistFailed: expect.any(String),
        confirmTitle: expect.any(String),
        confirmMessage: expect.any(String)
      })
    }
  })
})
