import { describe, it, expect } from 'vitest'
import {
  ANTIGRAVITY_PROJECT_ID_CREDENTIAL_KEY,
  HEADER_OVERRIDE_ENABLED_CREDENTIAL_KEY,
  HEADER_OVERRIDES_CREDENTIAL_KEY,
  applyAntigravityProjectID,
  applyHeaderOverride,
  applyInterceptWarmup,
  buildHeaderOverridesObject,
  getHeaderOverrideTemplate,
  isCustomGrokBaseUrl,
  isHeaderOverrideCapable,
  isHeaderOverridePlatform,
  parseHeaderOverridesJson,
  serializeHeaderOverrideRows,
  splitHeaderOverridesObject,
  validateHeaderOverrideRows
} from '../credentialsBuilder'

describe('applyInterceptWarmup', () => {
  it('create + enabled=true: should set intercept_warmup_requests to true', () => {
    const creds: Record<string, unknown> = { access_token: 'tok' }
    applyInterceptWarmup(creds, true, 'create')
    expect(creds.intercept_warmup_requests).toBe(true)
  })

  it('create + enabled=false: should not add the field', () => {
    const creds: Record<string, unknown> = { access_token: 'tok' }
    applyInterceptWarmup(creds, false, 'create')
    expect('intercept_warmup_requests' in creds).toBe(false)
  })

  it('edit + enabled=true: should set intercept_warmup_requests to true', () => {
    const creds: Record<string, unknown> = { api_key: 'sk' }
    applyInterceptWarmup(creds, true, 'edit')
    expect(creds.intercept_warmup_requests).toBe(true)
  })

  it('edit + enabled=false + field exists: should delete the field', () => {
    const creds: Record<string, unknown> = { api_key: 'sk', intercept_warmup_requests: true }
    applyInterceptWarmup(creds, false, 'edit')
    expect('intercept_warmup_requests' in creds).toBe(false)
  })

  it('edit + enabled=false + field absent: should not throw', () => {
    const creds: Record<string, unknown> = { api_key: 'sk' }
    applyInterceptWarmup(creds, false, 'edit')
    expect('intercept_warmup_requests' in creds).toBe(false)
  })

  it('should not affect other fields', () => {
    const creds: Record<string, unknown> = {
      api_key: 'sk',
      base_url: 'url',
      intercept_warmup_requests: true
    }
    applyInterceptWarmup(creds, false, 'edit')
    expect(creds.api_key).toBe('sk')
    expect(creds.base_url).toBe('url')
    expect('intercept_warmup_requests' in creds).toBe(false)
  })
})

describe('applyAntigravityProjectID', () => {
  it('create + project id: trims and stores configured project fallback', () => {
    const creds: Record<string, unknown> = { access_token: 'tok' }
    applyAntigravityProjectID(creds, '  configured-project  ', 'create')
    expect(creds[ANTIGRAVITY_PROJECT_ID_CREDENTIAL_KEY]).toBe('configured-project')
  })

  it('create + empty project id: should not add the field', () => {
    const creds: Record<string, unknown> = { access_token: 'tok' }
    applyAntigravityProjectID(creds, '   ', 'create')
    expect(ANTIGRAVITY_PROJECT_ID_CREDENTIAL_KEY in creds).toBe(false)
  })

  it('edit + empty project id: deletes existing fallback', () => {
    const creds: Record<string, unknown> = {
      access_token: 'tok',
      [ANTIGRAVITY_PROJECT_ID_CREDENTIAL_KEY]: 'old-project'
    }
    applyAntigravityProjectID(creds, '', 'edit')
    expect(ANTIGRAVITY_PROJECT_ID_CREDENTIAL_KEY in creds).toBe(false)
  })

  it('does not affect onboard project_id or other credentials', () => {
    const creds: Record<string, unknown> = {
      project_id: 'onboard-project',
      model_mapping: { 'gemini-*': 'gemini-2.5-flash' }
    }
    applyAntigravityProjectID(creds, 'configured-project', 'edit')
    expect(creds.project_id).toBe('onboard-project')
    expect(creds.model_mapping).toEqual({ 'gemini-*': 'gemini-2.5-flash' })
    expect(creds[ANTIGRAVITY_PROJECT_ID_CREDENTIAL_KEY]).toBe('configured-project')
  })
})

describe('isHeaderOverridePlatform', () => {
  it('includes every platform that supports at least one eligible account type', () => {
    expect(isHeaderOverridePlatform('anthropic')).toBe(true)
    expect(isHeaderOverridePlatform('openai')).toBe(true)
    expect(isHeaderOverridePlatform('grok')).toBe(true)
    expect(isHeaderOverridePlatform('gemini')).toBe(false)
    expect(isHeaderOverridePlatform('antigravity')).toBe(false)
    expect(isHeaderOverridePlatform('')).toBe(false)
  })
})

describe('isHeaderOverrideCapable', () => {
  it('supports API-key Anthropic/OpenAI and API-key/OAuth Grok only', () => {
    expect(isHeaderOverrideCapable('anthropic', 'apikey')).toBe(true)
    expect(isHeaderOverrideCapable('openai', 'apikey')).toBe(true)
    expect(isHeaderOverrideCapable('anthropic', 'oauth')).toBe(false)
    expect(isHeaderOverrideCapable('openai', 'oauth')).toBe(false)
    expect(isHeaderOverrideCapable('grok', 'apikey')).toBe(true)
    expect(isHeaderOverrideCapable('grok', 'oauth')).toBe(true)
    expect(isHeaderOverrideCapable('grok', 'bedrock')).toBe(false)
    expect(isHeaderOverrideCapable('gemini', 'apikey')).toBe(false)
  })

  it('kimi/zhipu/deepseek only support API-key accounts', () => {
    for (const platform of ['kimi', 'zhipu', 'deepseek']) {
      expect(isHeaderOverrideCapable(platform, 'apikey')).toBe(true)
      expect(isHeaderOverrideCapable(platform, 'oauth')).toBe(false)
    }
  })
})

describe('JSON header override helpers', () => {
  it('parses flat values, trims them and rejects nested values', () => {
    expect(parseHeaderOverridesJson('{"x-num": 3, "accept-language": " zh-CN "}')).toEqual([
      { name: 'accept-language', value: 'zh-CN' },
      { name: 'x-num', value: '3' }
    ])
    expect(parseHeaderOverridesJson('{"nested":{"value":1}}')).toBeNull()
    expect(parseHeaderOverridesJson('[1,2]')).toBeNull()
    expect(parseHeaderOverridesJson('not-json')).toBeNull()
  })

  it('serializes named rows and skips placeholders', () => {
    const text = serializeHeaderOverrideRows([
      { name: ' accept-language ', value: ' zh-CN ' },
      { name: '', value: 'ignored' }
    ])
    expect(JSON.parse(text)).toEqual({ 'accept-language': 'zh-CN' })
  })
})

describe('isCustomGrokBaseUrl', () => {
  it('treats every endpoint except the CLI gateway as an explicit selection', () => {
    expect(isCustomGrokBaseUrl('https://api.x.ai/v1')).toBe(true)
    expect(isCustomGrokBaseUrl('https://cli-chat-proxy.grok.com/v1')).toBe(false)
    expect(isCustomGrokBaseUrl('HTTPS://API.X.AI:443/')).toBe(true)
    expect(isCustomGrokBaseUrl('https://us-west-2.api.x.ai/v1')).toBe(true)
    expect(isCustomGrokBaseUrl('https://relay.example.com/xai/v1')).toBe(true)
    expect(isCustomGrokBaseUrl('not a url')).toBe(false)
    expect(isCustomGrokBaseUrl(undefined)).toBe(false)
  })
})

describe('validateHeaderOverrideRows', () => {
  it('accepts valid rows and empty placeholder rows', () => {
    expect(
      validateHeaderOverrideRows([
        { name: 'x-provider-routing', value: 'region-a' },
        { name: 'accept-language', value: '' },
        { name: '', value: '' }
      ])
    ).toBeNull()
  })

  it('rejects empty name with non-empty value', () => {
    expect(validateHeaderOverrideRows([{ name: '', value: 'v' }])).toBe('invalidName')
  })

  it('rejects invalid header names', () => {
    expect(validateHeaderOverrideRows([{ name: 'bad name', value: '' }])).toBe('invalidName')
    expect(validateHeaderOverrideRows([{ name: 'bad:name', value: '' }])).toBe('invalidName')
    expect(validateHeaderOverrideRows([{ name: '名称', value: '' }])).toBe('invalidName')
  })

  it('rejects blocked header names case-insensitively', () => {
    expect(validateHeaderOverrideRows([{ name: 'Authorization', value: '' }])).toBe('blockedName')
    expect(validateHeaderOverrideRows([{ name: 'X-Api-Key', value: '' }])).toBe('blockedName')
    expect(validateHeaderOverrideRows([{ name: 'host', value: '' }])).toBe('blockedName')
    expect(validateHeaderOverrideRows([{ name: 'Content-Length', value: '' }])).toBe('blockedName')
    expect(validateHeaderOverrideRows([{ name: 'Content-Type', value: '' }])).toBe('blockedName')
    expect(validateHeaderOverrideRows([{ name: 'Cookie', value: '' }])).toBe('blockedName')
    expect(validateHeaderOverrideRows([{ name: 'x-goog-api-key', value: '' }])).toBe('blockedName')
    expect(validateHeaderOverrideRows([{ name: 'User-Agent', value: '' }])).toBe('blockedName')
    expect(validateHeaderOverrideRows([{ name: 'Anthropic-Beta', value: '' }])).toBe('blockedName')
    expect(validateHeaderOverrideRows([{ name: 'X-Stainless-Custom', value: '' }])).toBe(
      'blockedName'
    )
    expect(validateHeaderOverrideRows([{ name: 'Originator', value: '' }])).toBe('blockedName')
    expect(validateHeaderOverrideRows([{ name: 'X-Grok-Conv-Id', value: '' }])).toBe(
      'blockedName'
    )
  })

  it('rejects duplicate names case-insensitively', () => {
    expect(
      validateHeaderOverrideRows([
        { name: 'X-Custom', value: 'a' },
        { name: 'x-custom', value: 'b' }
      ])
    ).toBe('duplicateName')
  })
})

describe('buildHeaderOverridesObject / splitHeaderOverridesObject', () => {
  it('lowercases names, trims values and drops empty-name rows', () => {
    expect(
      buildHeaderOverridesObject([
        { name: ' X-Custom ', value: ' custom-value ' },
        { name: 'Accept-Language', value: '' },
        { name: '', value: 'ignored' }
      ])
    ).toEqual({ 'x-custom': 'custom-value', 'accept-language': '' })
  })

  it('splits an object into sorted rows and ignores non-string values', () => {
    expect(
      splitHeaderOverridesObject({ 'x-custom': 'value', 'accept-language': 'zh-CN', bogus: 42 })
    ).toEqual([
      { name: 'accept-language', value: 'zh-CN' },
      { name: 'x-custom', value: 'value' }
    ])
    expect(splitHeaderOverridesObject(null)).toEqual([])
    expect(splitHeaderOverridesObject(['a'])).toEqual([])
    expect(splitHeaderOverridesObject('str')).toEqual([])
  })

  it('roundtrips through build and split', () => {
    const rows = [
      { name: 'accept-language', value: 'zh-CN' },
      { name: 'x-custom', value: 'value' }
    ]
    expect(splitHeaderOverridesObject(buildHeaderOverridesObject(rows))).toEqual(rows)
  })
})

describe('getHeaderOverrideTemplate', () => {
  it('returns safe generic headers with empty values for anthropic', () => {
    const rows = getHeaderOverrideTemplate('anthropic')
    expect(rows.every((r) => r.value === '')).toBe(true)
    const names = rows.map((r) => r.name)
    expect(names).toEqual(['accept', 'accept-language'])
    expect(validateHeaderOverrideRows(rows)).toBeNull()
  })

  it('returns safe generic headers with empty values for openai', () => {
    const rows = getHeaderOverrideTemplate('openai')
    expect(rows.every((r) => r.value === '')).toBe(true)
    const names = rows.map((r) => r.name)
    expect(names).toEqual(['accept', 'accept-language'])
    expect(validateHeaderOverrideRows(rows)).toBeNull()
  })

  it('returns the same safe generic headers for grok', () => {
    expect(getHeaderOverrideTemplate('grok')).toEqual([
      { name: 'accept', value: '' },
      { name: 'accept-language', value: '' }
    ])
  })

  it('returns no template for unsupported platforms', () => {
    expect(getHeaderOverrideTemplate('gemini')).toEqual([])
  })
})

describe('applyHeaderOverride', () => {
  it('create + enabled: writes enabled flag and overrides object', () => {
    const creds: Record<string, unknown> = { api_key: 'sk' }
    applyHeaderOverride(creds, true, [{ name: 'X-Custom', value: 'value' }], 'create')
    expect(creds[HEADER_OVERRIDE_ENABLED_CREDENTIAL_KEY]).toBe(true)
    expect(creds[HEADER_OVERRIDES_CREDENTIAL_KEY]).toEqual({ 'x-custom': 'value' })
  })

  it('create + disabled: does not add fields', () => {
    const creds: Record<string, unknown> = { api_key: 'sk' }
    applyHeaderOverride(creds, false, [{ name: 'x-custom', value: 'value' }], 'create')
    expect(HEADER_OVERRIDE_ENABLED_CREDENTIAL_KEY in creds).toBe(false)
    expect(HEADER_OVERRIDES_CREDENTIAL_KEY in creds).toBe(false)
  })

  it('edit + disabled: deletes existing fields', () => {
    const creds: Record<string, unknown> = {
      api_key: 'sk',
      [HEADER_OVERRIDE_ENABLED_CREDENTIAL_KEY]: true,
      [HEADER_OVERRIDES_CREDENTIAL_KEY]: { 'x-custom': 'value' }
    }
    applyHeaderOverride(creds, false, [], 'edit')
    expect(HEADER_OVERRIDE_ENABLED_CREDENTIAL_KEY in creds).toBe(false)
    expect(HEADER_OVERRIDES_CREDENTIAL_KEY in creds).toBe(false)
    expect(creds.api_key).toBe('sk')
  })

  it('edit + enabled: replaces overrides object wholesale', () => {
    const creds: Record<string, unknown> = {
      [HEADER_OVERRIDE_ENABLED_CREDENTIAL_KEY]: true,
      [HEADER_OVERRIDES_CREDENTIAL_KEY]: { 'x-old': 'old' }
    }
    applyHeaderOverride(creds, true, [{ name: 'x-new', value: 'new' }], 'edit')
    expect(creds[HEADER_OVERRIDES_CREDENTIAL_KEY]).toEqual({ 'x-new': 'new' })
  })
})

describe('validateHeaderOverrideRows value/entry limits', () => {
  it('rejects websocket handshake headers', () => {
    expect(validateHeaderOverrideRows([{ name: 'Sec-WebSocket-Key', value: '' }])).toBe(
      'blockedName'
    )
  })

  it('rejects control characters in values', () => {
    expect(validateHeaderOverrideRows([{ name: 'x-custom', value: 'a\x0bb' }])).toBe(
      'invalidValue'
    )
  })

  it('rejects oversized values', () => {
    expect(validateHeaderOverrideRows([{ name: 'x-custom', value: 'a'.repeat(8193) }])).toBe(
      'invalidValue'
    )
  })

  it('measures value length in UTF-8 bytes to match backend', () => {
    // 3000 个 CJK 字符 = 3000 UTF-16 code units，但 9000 UTF-8 字节 > 8192
    expect(validateHeaderOverrideRows([{ name: 'x-custom', value: '测'.repeat(3000) }])).toBe(
      'invalidValue'
    )
    expect(validateHeaderOverrideRows([{ name: 'x-custom', value: '测'.repeat(2000) }])).toBeNull()
  })

  it('rejects too many entries', () => {
    const rows = Array.from({ length: 65 }, (_, i) => ({ name: `x-h-${i}`, value: 'v' }))
    expect(validateHeaderOverrideRows(rows)).toBe('tooManyEntries')
  })
})

describe('validateHeaderOverrideRows session isolation headers', () => {
  it('rejects per-request session headers', () => {
    expect(validateHeaderOverrideRows([{ name: 'session_id', value: '' }])).toBe('blockedName')
    expect(validateHeaderOverrideRows([{ name: 'Conversation_ID', value: '' }])).toBe('blockedName')
    expect(validateHeaderOverrideRows([{ name: 'x-codex-turn-state', value: '' }])).toBe(
      'blockedName'
    )
    expect(validateHeaderOverrideRows([{ name: 'X-Claude-Code-Session-Id', value: '' }])).toBe(
      'blockedName'
    )
    expect(validateHeaderOverrideRows([{ name: 'x-client-request-id', value: '' }])).toBe(
      'blockedName'
    )
  })

  it('allows tab inside value', () => {
    expect(validateHeaderOverrideRows([{ name: 'x-custom', value: 'a\tb' }])).toBeNull()
  })

  it('rejects oversized names', () => {
    expect(validateHeaderOverrideRows([{ name: 'x'.repeat(201), value: 'v' }])).toBe('invalidName')
  })
})
