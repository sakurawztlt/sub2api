import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const source = readFileSync(
  resolve(process.cwd(), 'src/components/account/CreateAccountModal.vue'),
  'utf8'
)

describe('CreateAccountModal Grok account capabilities', () => {
  it('offers OAuth and API-key setup with the official xAI API-key default', () => {
    expect(source).toContain('data-testid="grok-account-type-oauth"')
    expect(source).toContain('data-testid="grok-account-type-api-key"')
    expect(source).toContain('@click="accountCategory = \'apikey\'"')
    expect(source).toContain("newPlatform === 'grok'")
    expect(source).toContain("? 'https://api.x.ai/v1'")
    expect(source).toContain(':placeholder="apiKeyValuePlaceholder"')
    expect(source).toContain("return 'xai-...'")
  })

  it('exposes endpoint presets and safe header overrides for OAuth creation', () => {
    expect(source).toContain('data-testid="grok-custom-base-url-toggle"')
    expect(source).toContain('data-testid="grok-custom-base-url-input"')
    expect(source).toContain('data-testid="grok-oauth-header-override-toggle"')
    expect(source).toContain('<GrokBaseUrlPresets')
    expect(source).toContain('<HeaderOverrideEditor')
    expect(source).toContain("applyHeaderOverride(credentials, headerOverrideEnabled.value")
  })

  it('applies custom endpoint, headers, and model mapping to both OAuth creation paths', () => {
    expect(source.match(/applyGrokOAuthUpstreamConfig\(credentials\)/g)?.length).toBeGreaterThanOrEqual(2)
    expect(source).toContain("await createAccountAndFinish(\n      'grok',\n      'oauth'")
    expect(source).toContain("await adminAPI.accounts.create({\n          name: accountName")
    expect(source).toContain('if (modelMapping) credentials.model_mapping = modelMapping')
  })

  it('keeps Grok password authorization hidden in the create flow', () => {
    expect(source).toContain(':show-email-password-option="false"')
  })

  it('wires Grok SSO batch import through the dedicated safe backend endpoint', () => {
    expect(source).toContain(':show-sso-option="form.platform === \'grok\'"')
    expect(source).toContain('@import-sso="handleGrokImportSSO"')
    expect(source).toContain('const handleGrokImportSSO = async (ssoInput: string) => {')
    expect(source).toContain('await adminAPI.grok.createFromSSO({')
    expect(source).toContain('sso_tokens: ssoTokens')
    expect(source).not.toContain('credentials.sso_token')
    expect(source).not.toContain('credentials.password')
  })
})
