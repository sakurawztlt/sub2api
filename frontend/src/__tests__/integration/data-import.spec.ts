import { describe, it, expect, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ImportDataModal from '@/components/admin/account/ImportDataModal.vue'
import { mergeAccountImportPayloads, normalizeAccountImportPayload } from '@/utils/adminDataImport'

const { importData } = vi.hoisted(() => ({
  importData: vi.fn()
}))

const showError = vi.fn()
const showSuccess = vi.fn()
const showWarning = vi.fn()

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess,
    showWarning
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      importData
    }
  }
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key
  })
}))

const mountModal = () =>
  mount(ImportDataModal, {
    props: { show: true },
    global: {
      stubs: {
        BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' }
      }
    }
  })

const makeJsonFile = (name: string, content: string, type = 'application/json') => {
  const file = new File([content], name, { type })
  Object.defineProperty(file, 'text', {
    value: () => Promise.resolve(content)
  })
  return file
}

const setInputFiles = (element: Element, files: File[]) => {
  Object.defineProperty(element, 'files', {
    value: files,
    configurable: true
  })
}

describe('ImportDataModal', () => {
  beforeEach(async () => {
    showError.mockReset()
    showSuccess.mockReset()
    showWarning.mockReset()
    importData.mockReset()
  })

  it('未选择文件时提示错误', async () => {
    const wrapper = mountModal()

    await wrapper.find('form').trigger('submit')
    expect(showError).toHaveBeenCalledWith('admin.accounts.dataImportSelectFile')
  })

  it('无效 JSON 时按文件名提示解析失败', async () => {
    const { adminAPI } = await import('@/api/admin')
    const wrapper = mountModal()

    const input = wrapper.find('input[type="file"]')
    setInputFiles(input.element, [makeJsonFile('data.json', 'invalid json')])

    await input.trigger('change')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('admin.accounts.dataImportParseFailedFile')
    expect(adminAPI.accounts.importData).not.toHaveBeenCalled()
  })

  it('不是导出数据的 JSON 按文件名拒绝', async () => {
    const { adminAPI } = await import('@/api/admin')
    const wrapper = mountModal()

    const input = wrapper.find('input[type="file"]')
    setInputFiles(input.element, [makeJsonFile('random.json', JSON.stringify({ name: 'test' }))])

    await input.trigger('change')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('admin.accounts.dataImportInvalidFile')
    expect(adminAPI.accounts.importData).not.toHaveBeenCalled()
  })

  it('无有效 JSON 的选择不清空已有选择', async () => {
    const { adminAPI } = await import('@/api/admin')
    vi.mocked(adminAPI.accounts.importData).mockResolvedValue({
      proxy_created: 0,
      proxy_reused: 0,
      proxy_failed: 0,
      account_created: 1,
      account_failed: 0
    })

    const wrapper = mountModal()
    const input = wrapper.find('input[type="file"]')

    const valid = makeJsonFile(
      'valid.json',
      JSON.stringify({ exported_at: '2026-07-05T00:00:00Z', proxies: [], accounts: [{ name: 'a' }] })
    )
    setInputFiles(input.element, [valid])
    await input.trigger('change')

    setInputFiles(input.element, [new File(['hello'], 'notes.txt', { type: 'text/plain' })])
    await input.trigger('change')
    expect(showError).toHaveBeenCalledWith('admin.accounts.dataImportSelectFile')

    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(adminAPI.accounts.importData).toHaveBeenCalledWith({
      data: expect.objectContaining({
        accounts: [expect.objectContaining({ name: 'a' })]
      }),
      skip_default_group_bind: true
    })
  })

  it('支持纯账号数组批量导入', async () => {
    importData.mockResolvedValue({
      proxy_created: 0,
      proxy_reused: 0,
      proxy_failed: 0,
      account_created: 2,
      account_failed: 0,
      errors: []
    })

    const wrapper = mountModal()

    const input = wrapper.find('input[type="file"]')
    const file = makeJsonFile(
      'data.json',
      JSON.stringify([
        {
          name: 'acc-1',
          platform: 'openai',
          type: 'oauth',
          credentials: { access_token: 'token-1' },
          concurrency: 2,
          priority: 10
        },
        {
          name: 'acc-2',
          platform: 'gemini',
          type: 'apikey',
          credentials: { api_key: 'token-2' },
          concurrency: 3,
          priority: 20
        }
      ])
    )
    setInputFiles(input.element, [file])

    await input.trigger('change')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(importData).toHaveBeenCalledTimes(1)
    expect(importData).toHaveBeenCalledWith({
      data: {
        type: 'sub2api-data',
        version: 1,
        exported_at: expect.any(String),
        proxies: [],
        accounts: [
          {
            name: 'acc-1',
            notes: null,
            platform: 'openai',
            type: 'oauth',
            credentials: { access_token: 'token-1' },
            extra: undefined,
            proxy_key: null,
            concurrency: 2,
            priority: 10,
            rate_multiplier: null,
            expires_at: null,
            auto_pause_on_expired: undefined
          },
          {
            name: 'acc-2',
            notes: null,
            platform: 'gemini',
            type: 'apikey',
            credentials: { api_key: 'token-2' },
            extra: undefined,
            proxy_key: null,
            concurrency: 3,
            priority: 20,
            rate_multiplier: null,
            expires_at: null,
            auto_pause_on_expired: undefined
          }
        ]
      },
      skip_default_group_bind: true
    })
    expect(showSuccess).toHaveBeenCalledWith('admin.accounts.dataImportSuccess')
  })

  it('支持一次选择多个单独 json 文件', async () => {
    importData.mockResolvedValue({
      proxy_created: 0,
      proxy_reused: 0,
      proxy_failed: 0,
      account_created: 2,
      account_failed: 0,
      errors: []
    })

    const wrapper = mountModal()

    const payloadA = {
      name: 'acc-a',
      platform: 'openai',
      type: 'oauth',
      credentials: { access_token: 'token-a' },
      concurrency: 1,
      priority: 10
    }
    const payloadB = {
      name: 'acc-b',
      platform: 'gemini',
      type: 'apikey',
      credentials: { api_key: 'token-b' },
      concurrency: 2,
      priority: 20
    }

    const fileA = makeJsonFile('a.json', JSON.stringify(payloadA))
    const fileB = makeJsonFile('b.json', JSON.stringify(payloadB))

    const input = wrapper.find('input[type="file"]')
    setInputFiles(input.element, [fileA, fileB])

    await input.trigger('change')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    const expectedPayload = mergeAccountImportPayloads([
      normalizeAccountImportPayload(payloadA),
      normalizeAccountImportPayload(payloadB)
    ])

    expect(importData).toHaveBeenCalledWith({
      data: {
        ...expectedPayload,
        exported_at: expect.any(String)
      },
      skip_default_group_bind: true
    })
  })

  it('部分成功时关闭弹窗仍通知父组件刷新', async () => {
    const { adminAPI } = await import('@/api/admin')
    vi.mocked(adminAPI.accounts.importData).mockResolvedValue({
      proxy_created: 0,
      proxy_reused: 0,
      proxy_failed: 0,
      account_created: 1,
      account_failed: 1
    })

    const wrapper = mountModal()
    const input = wrapper.find('input[type="file"]')
    setInputFiles(input.element, [
      makeJsonFile(
        'mixed.json',
        JSON.stringify({
          exported_at: '2026-07-05T00:00:00Z',
          proxies: [],
          accounts: [{ name: 'a' }, { name: 'b' }]
        })
      )
    ])

    await input.trigger('change')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('admin.accounts.dataImportCompletedWithErrors')
    expect(wrapper.emitted('imported')).toBeUndefined()

    // 第二个 btn-secondary 是 footer 的取消按钮(第一个是选择文件)
    await wrapper.findAll('button.btn-secondary')[1]!.trigger('click')

    expect(wrapper.emitted('imported')).toHaveLength(1)
    expect(wrapper.emitted('close')).toHaveLength(1)
  })
})
