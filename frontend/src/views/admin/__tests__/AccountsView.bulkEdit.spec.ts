import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AccountsView from '../AccountsView.vue'

const {
  listAccounts,
  listWithEtag,
  getBatchTodayStats,
  getAllProxies,
  getAllGroups,
  batchTestActive
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  batchTestActive: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      listWithEtag,
      getBatchTodayStats,
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn(),
      batchTestActive
    },
    proxies: {
      getAll: getAllProxies
    },
    groups: {
      getAll: getAllGroups
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    token: 'test-token'
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

const DataTableStub = {
  props: ['columns', 'data'],
  template: `
    <div data-test="data-table">
      <div v-for="row in data" :key="row.id" :data-test="'account-' + row.id">
        {{ row.test_status || 'empty' }}
      </div>
    </div>
  `
}

const AccountBulkActionsBarStub = {
  props: ['selectedIds'],
  emits: ['edit-filtered'],
  template: '<button data-test="edit-filtered" @click="$emit(\'edit-filtered\')">edit filtered</button>'
}

const BulkEditAccountModalStub = {
  props: ['show', 'target'],
  template: '<div data-test="bulk-edit-modal" :data-show="String(show)" :data-target-mode="target?.mode ?? \'\'" :data-platforms="JSON.stringify(target?.selectedPlatforms ?? [])" :data-types="JSON.stringify(target?.selectedTypes ?? [])"></div>'
}

describe('admin AccountsView bulk edit scope', () => {
  beforeEach(() => {
    localStorage.clear()

    listAccounts.mockReset()
    listWithEtag.mockReset()
    getBatchTodayStats.mockReset()
    getAllProxies.mockReset()
    getAllGroups.mockReset()
    batchTestActive.mockReset()

    listAccounts.mockResolvedValue({
      items: [],
      total: 0,
      page: 1,
      page_size: 20,
      pages: 0
    })
    listWithEtag.mockResolvedValue({
      notModified: true,
      etag: null,
      data: null
    })
    getBatchTodayStats.mockResolvedValue({ stats: {} })
    getAllProxies.mockResolvedValue([])
    getAllGroups.mockResolvedValue([])
    batchTestActive.mockResolvedValue({ total: 0, passed: 0, failed: 0, results: [] })
  })

  it('opens bulk edit in filtered-results mode from the bulk actions dropdown', async () => {
    const wrapper = mount(AccountsView, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' },
          TablePageLayout: {
            template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>'
          },
          DataTable: DataTableStub,
          Pagination: true,
          ConfirmDialog: true,
          AccountTableActions: { template: '<div><slot name="beforeCreate" /><slot name="after" /></div>' },
          AccountTableFilters: { template: '<div></div>' },
          AccountBulkActionsBar: AccountBulkActionsBarStub,
          AccountActionMenu: true,
          ImportDataModal: true,
          ReAuthAccountModal: true,
          AccountTestModal: true,
          AccountStatsModal: true,
          ScheduledTestsPanel: true,
          SyncFromCrsModal: true,
          TempUnschedStatusModal: true,
          ErrorPassthroughRulesModal: true,
          TLSFingerprintProfilesModal: true,
          CreateAccountModal: true,
          EditAccountModal: true,
          BulkEditAccountModal: BulkEditAccountModalStub,
          PlatformTypeBadge: true,
          AccountCapacityCell: true,
          AccountStatusIndicator: true,
          AccountTodayStatsCell: true,
          AccountGroupsCell: true,
          AccountUsageCell: true,
          Icon: true
        }
      }
    })

    await flushPromises()
    await wrapper.get('[data-test="edit-filtered"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-test="bulk-edit-modal"]').attributes('data-show')).toBe('true')
    expect(wrapper.get('[data-test="bulk-edit-modal"]').attributes('data-target-mode')).toBe('filtered')
  })

  it('hides platform-specific fields when the filtered preview is incomplete', async () => {
    listAccounts.mockResolvedValue({
      items: Array.from({ length: 100 }, (_, index) => ({
        id: index + 1,
        name: `oauth-${index + 1}`,
        platform: 'openai',
        type: 'oauth',
        status: 'active',
        schedulable: true
      })),
      total: 101,
      page: 1,
      page_size: 100,
      pages: 2
    })
    const wrapper = mount(AccountsView, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' },
          TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
          DataTable: DataTableStub,
          Pagination: true,
          ConfirmDialog: true,
          AccountTableActions: { template: '<div><slot name="beforeCreate" /><slot name="after" /></div>' },
          AccountTableFilters: { template: '<div></div>' },
          AccountBulkActionsBar: AccountBulkActionsBarStub,
          AccountActionMenu: true,
          ImportDataModal: true,
          ReAuthAccountModal: true,
          AccountTestModal: true,
          AccountStatsModal: true,
          ScheduledTestsPanel: true,
          SyncFromCrsModal: true,
          TempUnschedStatusModal: true,
          ErrorPassthroughRulesModal: true,
          TLSFingerprintProfilesModal: true,
          CreateAccountModal: true,
          EditAccountModal: true,
          BulkEditAccountModal: BulkEditAccountModalStub,
          PlatformTypeBadge: true,
          AccountCapacityCell: true,
          AccountStatusIndicator: true,
          AccountTodayStatsCell: true,
          AccountGroupsCell: true,
          AccountUsageCell: true,
          Icon: true
        }
      }
    })
    await flushPromises()
    await wrapper.get('[data-test="edit-filtered"]').trigger('click')
    await flushPromises()

    const modal = wrapper.get('[data-test="bulk-edit-modal"]')
    expect(modal.attributes('data-platforms')).toBe('[]')
    expect(modal.attributes('data-types')).toBe('[]')
  })

  it('only marks active schedulable accounts as testing for one-click connection test', async () => {
    listAccounts.mockResolvedValue({
      items: [
        { id: 1, name: 'enabled', platform: 'openai', type: 'oauth', status: 'active', schedulable: true },
        { id: 2, name: 'paused', platform: 'openai', type: 'oauth', status: 'active', schedulable: false },
        { id: 3, name: 'inactive', platform: 'openai', type: 'oauth', status: 'inactive', schedulable: true },
        { id: 4, name: 'temp-unschedulable', platform: 'openai', type: 'oauth', status: 'active', schedulable: true, temp_unschedulable_until: new Date(Date.now() + 60_000).toISOString() },
        { id: 5, name: 'rate-limited', platform: 'openai', type: 'oauth', status: 'active', schedulable: true, rate_limit_reset_at: new Date(Date.now() + 60_000).toISOString() },
        { id: 6, name: 'overloaded', platform: 'openai', type: 'oauth', status: 'active', schedulable: true, overload_until: new Date(Date.now() + 60_000).toISOString() },
        { id: 7, name: 'expired', platform: 'openai', type: 'oauth', status: 'active', schedulable: true, auto_pause_on_expired: true, expires_at: Math.floor((Date.now() - 60_000) / 1000) }
      ],
      total: 7,
      page: 1,
      page_size: 20,
      pages: 1
    })

    let resolveBatch!: (value: unknown) => void
    batchTestActive.mockReturnValue(new Promise(resolve => { resolveBatch = resolve }))

    const wrapper = mount(AccountsView, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' },
          TablePageLayout: {
            template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>'
          },
          DataTable: DataTableStub,
          Pagination: true,
          ConfirmDialog: true,
          AccountTableActions: { template: '<div><slot name="beforeCreate" /><slot name="after" /></div>' },
          AccountTableFilters: { template: '<div></div>' },
          AccountBulkActionsBar: AccountBulkActionsBarStub,
          AccountActionMenu: true,
          ImportDataModal: true,
          ReAuthAccountModal: true,
          AccountTestModal: true,
          AccountStatsModal: true,
          ScheduledTestsPanel: true,
          SyncFromCrsModal: true,
          TempUnschedStatusModal: true,
          ErrorPassthroughRulesModal: true,
          TLSFingerprintProfilesModal: true,
          CreateAccountModal: true,
          EditAccountModal: true,
          BulkEditAccountModal: BulkEditAccountModalStub,
          PlatformTypeBadge: true,
          AccountCapacityCell: true,
          AccountStatusIndicator: true,
          AccountTodayStatsCell: true,
          AccountGroupsCell: true,
          AccountUsageCell: true,
          Icon: true
        }
      }
    })

    await flushPromises()

    const buttons = wrapper.findAll('button')
    await buttons.find(button => button.text().includes('admin.accounts.moreActions'))!.trigger('click')
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text().includes('admin.accounts.batchTest.action'))!.trigger('click')
    await flushPromises()

    expect(batchTestActive).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-test="account-1"]').text()).toBe('testing')
    expect(wrapper.get('[data-test="account-2"]').text()).toBe('empty')
    expect(wrapper.get('[data-test="account-3"]').text()).toBe('empty')
    expect(wrapper.get('[data-test="account-4"]').text()).toBe('empty')
    expect(wrapper.get('[data-test="account-5"]').text()).toBe('empty')
    expect(wrapper.get('[data-test="account-6"]').text()).toBe('empty')
    expect(wrapper.get('[data-test="account-7"]').text()).toBe('empty')

    resolveBatch({ total: 1, passed: 1, failed: 0, results: [{ account_id: 1, status: 'pass' }] })
    await flushPromises()
  })

})
