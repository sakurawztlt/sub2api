import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import GrokFreeIcon from '../GrokFreeIcon.vue'
import PlatformTypeBadge from '../PlatformTypeBadge.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

describe('PlatformTypeBadge Grok plans', () => {
  it('normalizes FREE and BASIC to the dedicated Grok Free badge', async () => {
    const wrapper = mount(PlatformTypeBadge, {
      props: {
        platform: 'grok',
        type: 'oauth',
        planType: 'BASIC',
        subscriptionExpiresAt: '2027-01-01T00:00:00Z'
      }
    })

    expect(wrapper.text()).toContain('Grok Free')
    expect(wrapper.findComponent(GrokFreeIcon).exists()).toBe(true)
    expect(wrapper.find('[data-testid="grok-plan-icon"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('2027-01-01')

    await wrapper.setProps({ planType: 'FREE' })
    expect(wrapper.text()).toContain('Grok Free')
  })

  it('normalizes paid SuperGrok labels and displays their plan mark', async () => {
    const wrapper = mount(PlatformTypeBadge, {
      props: { platform: 'grok', type: 'oauth', planType: 'SuperGrok Heavy' }
    })

    expect(wrapper.text()).toContain('SuperGrok Heavy')
    expect(wrapper.find('[data-testid="grok-plan-icon"]').exists()).toBe(true)
    expect(wrapper.html()).toContain('bg-purple-100')

    await wrapper.setProps({ planType: 'super_grok' })
    expect(wrapper.text()).toContain('SuperGrok')
  })

  it('colors free gray, SuperGrok cyan, and Heavy purple', () => {
    const free = mount(PlatformTypeBadge, {
      props: { platform: 'grok', type: 'oauth', planType: 'free' }
    })
    expect(free.html()).toContain('bg-gray-100')

    const superGrok = mount(PlatformTypeBadge, {
      props: { platform: 'grok', type: 'oauth', planType: 'supergrok' }
    })
    expect(superGrok.html()).toContain('bg-cyan-100')

    const heavy = mount(PlatformTypeBadge, {
      props: { platform: 'grok', type: 'oauth', planType: 'Heavy' }
    })
    expect(heavy.text()).toContain('Heavy')
    expect(heavy.html()).toContain('bg-purple-100')
    expect(heavy.find('[data-testid="grok-plan-icon"]').exists()).toBe(true)

    const lite = mount(PlatformTypeBadge, {
      props: { platform: 'grok', type: 'oauth', planType: 'supergrok_lite' },
    })
    expect(lite.text()).toContain('SuperGrok Lite')
    expect(lite.html()).toContain('bg-cyan-100')
  })
})
