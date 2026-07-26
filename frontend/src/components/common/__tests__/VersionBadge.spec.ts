import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import VersionBadge from '@/components/common/VersionBadge.vue'

const { fetchVersion } = vi.hoisted(() => ({ fetchVersion: vi.fn() }))

vi.mock('@/stores', () => ({
  useAuthStore: () => ({ isAdmin: true }),
  useAppStore: () => ({
    currentVersion: '2.24.0-relay',
    fetchVersion,
  }),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

describe('VersionBadge', () => {
  beforeEach(() => {
    fetchVersion.mockReset()
  })

  it('只展示自有静态版本，不提供更新交互', () => {
    const wrapper = mount(VersionBadge)

    expect(wrapper.text()).toBe('v2.24.0-relay')
    expect(wrapper.find('button').exists()).toBe(false)
    expect(wrapper.find('a').exists()).toBe(false)
    expect(fetchVersion).toHaveBeenCalledTimes(1)
  })
})
