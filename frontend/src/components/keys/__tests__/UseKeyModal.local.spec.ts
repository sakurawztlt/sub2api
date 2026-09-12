import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'

const { copyToClipboardMock, saveAsMock } = vi.hoisted(() => ({
  copyToClipboardMock: vi.fn().mockResolvedValue(true),
  saveAsMock: vi.fn()
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key
  })
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({
    copyToClipboard: copyToClipboardMock
  })
}))

vi.mock('file-saver', () => ({
  saveAs: saveAsMock
}))

import UseKeyModal from '../UseKeyModal.vue'

function readBlobAsText(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.addEventListener('load', () => resolve(String(reader.result || '')))
    reader.addEventListener('error', () => reject(reader.error))
    reader.readAsText(blob)
  })
}

describe('UseKeyModal', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    saveAsMock.mockClear()
  })

  it('omits the attribution override from every standard Claude Code setup form', async () => {
    const wrapper = mount(UseKeyModal, {
      props: {
        show: true,
        apiKey: 'sk-anthropic-test',
        baseUrl: 'https://example.com/v1',
        platform: 'anthropic'
      },
      global: {
        stubs: {
          BaseDialog: {
            template: '<div><slot /><slot name="footer" /></div>'
          },
          Icon: {
            template: '<span />'
          }
        }
      }
    })

    for (const [shell, trafficSetting] of [
      ['macOS / Linux', 'export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1'],
      ['Windows CMD', 'set CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1'],
      ['PowerShell', '$env:CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1']
    ]) {
      if (shell !== 'macOS / Linux') {
        const shellTab = wrapper.findAll('button').find(
          (button) => button.text().trim() === shell
        )
        expect(shellTab).toBeDefined()
        await shellTab!.trigger('click')
        await nextTick()
      }

      const codeBlocks = wrapper.findAll('pre code').map((code) => code.text())
      const allCode = codeBlocks.join('\n')
      const settings = JSON.parse(codeBlocks.find((content) => content.includes('"$schema"'))!)

      expect(allCode).not.toContain('CLAUDE_CODE_ATTRIBUTION_HEADER')
      expect(allCode).toContain(trafficSetting)
      expect(settings.env.CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC).toBe('1')
      expect(settings.env).not.toHaveProperty('CLAUDE_CODE_ATTRIBUTION_HEADER')
    }
  })

  it('renders Grok Build and OpenCode setup for Grok groups', async () => {
    const wrapper = mount(UseKeyModal, {
      props: {
        show: true,
        apiKey: 'sk-grok-test',
        baseUrl: 'https://example.com/v1',
        platform: 'grok'
      },
      global: {
        stubs: {
          BaseDialog: {
            template: '<div><slot /><slot name="footer" /></div>'
          },
          Icon: {
            template: '<span />'
          }
        }
      }
    })

    const grokTab = wrapper.findAll('button').find((button) =>
      button.text().includes('keys.useKeyModal.cliTabs.grokCli')
    )
    expect(grokTab).toBeDefined()
    await grokTab!.trigger('click')
    await nextTick()

    const grokConfig = wrapper.findAll('pre code')
      .map((code) => code.text())
      .find((content) => content.includes('[endpoints]'))
    expect(grokConfig).toContain('models_base_url = "https://example.com/v1"')
    expect(grokConfig).toContain('models_list_url = "https://example.com/v1/models"')
    expect(grokConfig).toContain('preferred_method = "api_key"')
    expect(grokConfig).toContain('[model."grok-4.5"]')
    expect(grokConfig).toContain('model = "grok-4.5"')
    expect(grokConfig).toContain('env_key = "XAI_API_KEY"')
    expect(grokConfig).toContain('api_backend = "responses"')
    expect(grokConfig).toContain('context_window = 500000')
    expect(grokConfig).toContain('[model."grok-build-0.1"]')
    expect(grokConfig).toContain('grok-4.20-multi-agent-0309')
    expect(grokConfig).toContain('image_gen = true')
    expect(grokConfig).toContain('video_gen = true')
    expect(grokConfig).not.toMatch(/^api_key = "sk-grok-test"$/m)

    const windowsTab = wrapper.findAll('button').find(
      (button) => button.text().trim() === 'Windows'
    )
    expect(windowsTab).toBeDefined()
    await windowsTab!.trigger('click')
    await nextTick()
    expect(wrapper.text().toLowerCase()).toContain('%userprofile%\\.grok\\config.toml')

    const opencodeTab = wrapper.findAll('button').find((button) =>
      button.text().includes('keys.useKeyModal.cliTabs.opencode')
    )
    expect(opencodeTab).toBeDefined()
    await opencodeTab!.trigger('click')
    await nextTick()

    const parsed = JSON.parse(wrapper.find('pre code').text())
    expect(parsed.provider.grok.npm).toBe('@ai-sdk/openai-compatible')
    expect(parsed.provider.grok.name).toBe('Grok via Sub2API')
    expect(parsed.provider.grok.options).toEqual({
      baseURL: 'https://example.com/v1',
      apiKey: 'sk-grok-test'
    })
    expect(parsed.provider.grok.models['grok-4.5']).toBeDefined()
    expect(parsed.provider.grok.models['grok-4.5'].limit.context).toBe(500000)
    expect(parsed.provider.grok.models['grok-build-0.1']).toBeDefined()
    expect(parsed.provider.grok.models['grok-4.20-multi-agent-0309']).toBeDefined()
    expect(parsed.provider.grok.models['grok-composer-2.5-fast']).toBeDefined()
  })

  it('renders copyable Claude Code setup through the Grok Messages gateway', async () => {
    copyToClipboardMock.mockClear()
    const wrapper = mount(UseKeyModal, {
      props: {
        show: true,
        apiKey: 'sk-grok-claude-test',
        baseUrl: 'https://example.com/v1',
        platform: 'grok'
      },
      global: {
        stubs: {
          BaseDialog: {
            template: '<div><slot /><slot name="footer" /></div>'
          },
          Icon: {
            template: '<span />'
          }
        }
      }
    })

    const claudeTab = wrapper.findAll('button').find((button) =>
      button.text().includes('keys.useKeyModal.cliTabs.claudeCode')
    )
    expect(claudeTab).toBeDefined()
    await claudeTab!.trigger('click')
    await nextTick()

    const codeBlocks = wrapper.findAll('pre code').map((code) => code.text())
    expect(codeBlocks.join('\n')).toContain('ANTHROPIC_BASE_URL="https://example.com"')
    expect(codeBlocks.join('\n')).toContain('ANTHROPIC_AUTH_TOKEN="sk-grok-claude-test"')
    const unixConfig = codeBlocks.find((content) => content.startsWith('export ANTHROPIC_BASE_URL'))
    for (const name of [
      'ANTHROPIC_MODEL',
      'ANTHROPIC_DEFAULT_OPUS_MODEL',
      'ANTHROPIC_DEFAULT_SONNET_MODEL',
      'ANTHROPIC_DEFAULT_HAIKU_MODEL',
      'ANTHROPIC_DEFAULT_FABLE_MODEL',
      'CLAUDE_CODE_SUBAGENT_MODEL'
    ]) {
      expect(unixConfig).toContain(`export ${name}="grok-4.5"`)
    }
    const settingsConfig = codeBlocks.find((content) => content.includes('"$schema"'))
    expect(settingsConfig).toBeDefined()
    expect(JSON.parse(settingsConfig!).env.ANTHROPIC_MODEL).toBe('grok-4.5')
    expect(wrapper.find('nav[aria-label="Client"]').classes()).toContain('min-w-max')
    expect(
      wrapper.find('nav[aria-label="Client"]').element.parentElement?.classList.contains('overflow-x-auto')
    ).toBe(true)

    const copyButton = wrapper.findAll('button').find((button) =>
      button.text().includes('keys.useKeyModal.copy')
    )
    expect(copyButton).toBeDefined()
    await copyButton!.trigger('click')
    expect(copyToClipboardMock).toHaveBeenCalledWith(
      expect.stringContaining('ANTHROPIC_AUTH_TOKEN="sk-grok-claude-test"'),
      'keys.copied'
    )
  })

  it('renders Grok Codex Responses HTTP setup without OpenAI auth files', async () => {
    const wrapper = mount(UseKeyModal, {
      props: {
        show: true,
        apiKey: 'sk-grok-codex-test',
        baseUrl: 'https://example.com/v1',
        platform: 'grok'
      },
      global: {
        stubs: {
          BaseDialog: {
            template: '<div><slot /><slot name="footer" /></div>'
          },
          Icon: {
            template: '<span />'
          }
        }
      }
    })

    const codexTab = wrapper.findAll('button').find((button) =>
      button.text().includes('keys.useKeyModal.cliTabs.codexCli')
    )
    expect(codexTab).toBeDefined()
    await codexTab!.trigger('click')
    await nextTick()

    const codeBlocks = wrapper.findAll('pre code').map((code) => code.text())
    const configToml = codeBlocks.find((content) => content.includes('[model_providers.sub2api]'))
    expect(configToml).toContain('model_provider = "sub2api"')
    expect(configToml).toContain('base_url = "https://example.com/v1"')
    expect(configToml).toContain('env_key = "SUB2API_API_KEY"')
    expect(configToml).toContain('wire_api = "responses"')
    expect(configToml).toContain('requires_openai_auth = false')
    expect(configToml).toContain('supports_websockets = false')
    expect(configToml).toContain('grok-imagine-image')
    expect(configToml).not.toContain('responses_websockets_v2')
    expect(codeBlocks.join('\n')).toContain('SUB2API_API_KEY')
    expect(wrapper.text()).not.toContain('auth.json')
  })

  it('renders GPT-5.4 mini entry in OpenCode config', async () => {
    const wrapper = mount(UseKeyModal, {
      props: {
        show: true,
        apiKey: 'sk-test',
        baseUrl: 'https://example.com/v1',
        platform: 'openai'
      },
      global: {
        stubs: {
          BaseDialog: {
            template: '<div><slot /><slot name="footer" /></div>'
          },
          Icon: {
            template: '<span />'
          }
        }
      }
    })

    const opencodeTab = wrapper.findAll('button').find((button) =>
      button.text().includes('keys.useKeyModal.cliTabs.opencode')
    )

    expect(opencodeTab).toBeDefined()
    await opencodeTab!.trigger('click')
    await nextTick()

    const codeBlock = wrapper.find('pre code')
    expect(codeBlock.exists()).toBe(true)
    expect(codeBlock.text()).toContain('"name": "GPT-5.4 Mini"')
    expect(codeBlock.text()).not.toContain('"name": "GPT-5.4 Nano"')
  })

  it('renders GPT-5.6 alias and max variants in OpenCode config', async () => {
    const wrapper = mount(UseKeyModal, {
      props: {
        show: true,
        apiKey: 'sk-test',
        baseUrl: 'https://example.com/v1',
        platform: 'openai'
      },
      global: {
        stubs: {
          BaseDialog: {
            template: '<div><slot /><slot name="footer" /></div>'
          },
          Icon: {
            template: '<span />'
          }
        }
      }
    })

    const opencodeTab = wrapper.findAll('button').find((button) =>
      button.text().includes('keys.useKeyModal.cliTabs.opencode')
    )
    expect(opencodeTab).toBeDefined()
    await opencodeTab!.trigger('click')
    await nextTick()

    const parsed = JSON.parse(wrapper.find('pre code').text())
    const models = parsed.provider.openai.models
    for (const model of ['gpt-5.6', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna']) {
      expect(models[model]).toBeDefined()
      expect(models[model].variants).toHaveProperty('max')
      expect(models[model].variants).toHaveProperty('xhigh')
    }
    expect(models['gpt-5.6'].name).toBe('GPT-5.6 (Sol)')
  })

  it('renders Claude Fable 5 OpenCode config with adaptive thinking', async () => {
    const wrapper = mount(UseKeyModal, {
      props: {
        show: true,
        apiKey: 'sk-test',
        baseUrl: 'https://example.com/v1',
        platform: 'antigravity'
      },
      global: {
        stubs: {
          BaseDialog: {
            template: '<div><slot /><slot name="footer" /></div>'
          },
          Icon: {
            template: '<span />'
          }
        }
      }
    })

    const opencodeTab = wrapper.findAll('button').find((button) =>
      button.text().includes('keys.useKeyModal.cliTabs.opencode')
    )

    expect(opencodeTab).toBeDefined()
    await opencodeTab!.trigger('click')
    await nextTick()

    const claudeConfig = wrapper.findAll('pre code')
      .map((code) => code.text())
      .find((content) => content.includes('"antigravity-claude"'))

    expect(claudeConfig).toBeDefined()
    const parsed = JSON.parse(claudeConfig!)
    const fable51 = parsed.provider['antigravity-claude'].models['claude-fable-5-1']
    const fable = parsed.provider['antigravity-claude'].models['claude-fable-5']

    expect(fable51.name).toBe('Claude Fable 5.1')
    expect(fable51.limit).toEqual({ context: 1048576, output: 128000 })
    expect(fable51.options.thinking).toEqual({ type: 'adaptive' })
    expect(fable51.options.thinking).not.toHaveProperty('budgetTokens')
    expect(fable.name).toBe('Claude Fable 5')
    expect(fable.limit).toEqual({ context: 1048576, output: 128000 })
    expect(fable.options.thinking).toEqual({ type: 'adaptive' })
    expect(fable.options.thinking).not.toHaveProperty('budgetTokens')
  })

  it('renders Claude Sonnet 5 OpenCode config with adaptive thinking', async () => {
    const wrapper = mount(UseKeyModal, {
      props: {
        show: true,
        apiKey: 'sk-test',
        baseUrl: 'https://example.com/v1',
        platform: 'antigravity'
      },
      global: {
        stubs: {
          BaseDialog: {
            template: '<div><slot /><slot name="footer" /></div>'
          },
          Icon: {
            template: '<span />'
          }
        }
      }
    })

    const opencodeTab = wrapper.findAll('button').find((button) =>
      button.text().includes('keys.useKeyModal.cliTabs.opencode')
    )

    expect(opencodeTab).toBeDefined()
    await opencodeTab!.trigger('click')
    await nextTick()

    const claudeConfig = wrapper.findAll('pre code')
      .map((code) => code.text())
      .find((content) => content.includes('"antigravity-claude"'))

    expect(claudeConfig).toBeDefined()
    const parsed = JSON.parse(claudeConfig!)
    const sonnet = parsed.provider['antigravity-claude'].models['claude-sonnet-5']

    expect(sonnet.name).toBe('Claude Sonnet 5')
    expect(sonnet.limit).toEqual({ context: 1048576, output: 128000 })
    expect(sonnet.options.thinking).toEqual({ type: 'adaptive' })
    expect(sonnet.options.thinking).not.toHaveProperty('budgetTokens')
  })
})
