import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import DataTable from '../DataTable.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key
  })
}))

const stubDesktopMatchMedia = (desktop = true) => {
  Object.defineProperty(window, 'matchMedia', {
    writable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: desktop,
      media: query,
      onchange: null,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn()
    }))
  })
}

describe('DataTable', () => {
  beforeEach(() => {
    stubDesktopMatchMedia()
    localStorage.clear()
  })

  it('renders paired sort arrows and highlights the active direction', async () => {
    const wrapper = mount(DataTable, {
      props: {
        columns: [
          { key: 'name', label: 'Name', sortable: true },
          { key: 'created_at', label: 'Created', sortable: true }
        ],
        data: [
          { id: 1, name: 'Beta', created_at: '2026-01-02T00:00:00Z' },
          { id: 2, name: 'Alpha', created_at: '2026-01-01T00:00:00Z' }
        ],
        defaultSortKey: 'name',
        defaultSortOrder: 'asc'
      },
      slots: {
        'header-name': '<span data-test="custom-name-header">Name</span>'
      }
    })

    await wrapper.vm.$nextTick()

    const nameHeader = wrapper.findAll('th')[0]
    expect(nameHeader.find('[data-test="custom-name-header"]').exists()).toBe(true)
    expect(nameHeader.attributes('aria-sort')).toBe('ascending')
    expect(nameHeader.findAll('svg')).toHaveLength(2)
    expect(nameHeader.findAll('svg')[0].classes()).toContain('text-primary-600')
    expect(nameHeader.findAll('svg')[1].classes()).toContain('text-gray-300')

    await nameHeader.trigger('click')
    await wrapper.vm.$nextTick()

    expect(nameHeader.attributes('aria-sort')).toBe('descending')
    expect(nameHeader.findAll('svg')[0].classes()).toContain('text-gray-300')
    expect(nameHeader.findAll('svg')[1].classes()).toContain('text-primary-600')
  })
  it('emits controlled current-page selection while preserving off-page keys', async () => {
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data: [
          { id: 1, name: 'One' },
          { id: 2, name: 'Two' }
        ],
        rowKey: 'id',
        selectable: true,
        selectedKeys: [99]
      }
    })

    await wrapper.get('[data-test="select-all"]').setValue(true)

    const selectedAll = wrapper.emitted('update:selectedKeys')?.at(-1)?.[0]
    expect(selectedAll).toEqual([99, 1, 2])

    await wrapper.setProps({ selectedKeys: selectedAll as number[] })
    const rowCheckboxes = wrapper.findAll<HTMLInputElement>('[data-test="select-row"]')
    expect(rowCheckboxes).toHaveLength(2)
    expect(rowCheckboxes.every((checkbox) => checkbox.element.checked)).toBe(true)

    await rowCheckboxes[0].setValue(false)

    expect(wrapper.emitted('update:selectedKeys')?.at(-1)?.[0]).toEqual([99, 2])
    expect(wrapper.emitted('selectionChange')?.at(-1)?.[0]).toEqual([99, 2])
    wrapper.unmount()
  })

  it('offers current-page select all in the mobile card layout', async () => {
    stubDesktopMatchMedia(false)
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data: [
          { id: 1, name: 'One' },
          { id: 2, name: 'Two' }
        ],
        rowKey: 'id',
        selectable: true,
        selectedKeys: [99]
      }
    })

    await wrapper.get('[data-test="select-all-mobile"]').setValue(true)

    expect(wrapper.emitted('update:selectedKeys')?.at(-1)?.[0]).toEqual([99, 1, 2])
    expect(wrapper.findAll('[data-test="select-row"]')).toHaveLength(2)
    wrapper.unmount()
  })
})
