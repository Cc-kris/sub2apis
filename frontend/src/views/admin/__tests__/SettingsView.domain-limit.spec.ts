import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent, h } from 'vue'

import SettingsView from '../SettingsView.vue'

const getSettings = vi.fn()
const updateSettings = vi.fn()

vi.mock('@/api', () => ({
  adminAPI: {
    settings: new Proxy({}, { get: (_target, key) => key === 'getSettings' ? getSettings : key === 'updateSettings' ? updateSettings : vi.fn().mockResolvedValue({}) }),
    groups: { getAll: vi.fn().mockResolvedValue([]) }, proxies: { list: vi.fn().mockResolvedValue([]) }, users: { getById: vi.fn(), list: vi.fn().mockResolvedValue([]) }, payment: { getProviders: vi.fn().mockResolvedValue([]), updateProvider: vi.fn(), createProvider: vi.fn(), deleteProvider: vi.fn() },
  },
}))
vi.mock('@/stores', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showWarning: vi.fn(), showInfo: vi.fn(), fetchPublicSettings: vi.fn() }) }))
vi.mock('@/stores/adminSettings', () => ({ useAdminSettingsStore: () => ({ fetch: vi.fn() }) }))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copyToClipboard: vi.fn() }) }))
vi.mock('@/utils/apiError', () => ({ extractApiErrorMessage: () => 'error' }))
vi.mock('vue-i18n', async (importOriginal) => ({ ...(await importOriginal()), useI18n: () => ({ t: (key: string) => key, locale: { value: 'zh-CN' } }) }))

const Toggle = defineComponent({ props: { modelValue: Boolean }, emits: ['update:modelValue'], setup(props, { emit, attrs }) { return () => h('input', { ...attrs, type: 'checkbox', checked: props.modelValue, onChange: (e: Event) => emit('update:modelValue', (e.target as HTMLInputElement).checked) }) } })
const stubs = { AppLayout: { template: '<div><slot /></div>' }, Toggle, Select: true, Icon: true, ConfirmDialog: true, PaymentProviderList: true, PaymentProviderDialog: true, GroupBadge: true, GroupOptionItem: true, ProxySelector: true, ImageUpload: true, BackupSettings: true }

describe('SettingsView registration domain limit', () => {
  beforeEach(() => {
    getSettings.mockReset().mockResolvedValue({ registration_domain_limit_enabled: true, registration_domain_limit_per_domain: 7 })
    updateSettings.mockReset().mockImplementation(async (payload) => payload)
  })

  it('loads configured limit and toggles the per-domain input', async () => {
    const wrapper = mount(SettingsView, { global: { stubs } })
    await new Promise((resolve) => setTimeout(resolve, 0))
    const input = wrapper.find('input[type="number"]')
    expect((input.element as HTMLInputElement).value).toBe('7')
    expect((input.element as HTMLInputElement).disabled).toBe(false)
    const toggle = wrapper.find('input[type="checkbox"]')
    await toggle.setValue(false)
    expect((input.element as HTMLInputElement).disabled).toBe(true)
  })

  it('associates visible numeric settings with explicit labels', async () => {
    const wrapper = mount(SettingsView, { global: { stubs } })
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(wrapper.find('label[for="settings-table-default-page-size"]').exists()).toBe(true)
    expect(wrapper.find('label[for="settings-translation-timeout"]').exists()).toBe(true)
    expect(wrapper.find('label[for="settings-registration-domain-limit"]').exists()).toBe(true)
    expect(wrapper.find('#settings-registration-domain-limit').exists()).toBe(true)
    expect(wrapper.find('label[for="settings-x-search-price"]').exists()).toBe(true)
  })
})
