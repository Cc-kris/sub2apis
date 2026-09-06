import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AnnouncementsView from '../AnnouncementsView.vue'

const { listAnnouncements, getAllGroups, listTags, showError } = vi.hoisted(() => ({
  listAnnouncements: vi.fn(),
  getAllGroups: vi.fn(),
  listTags: vi.fn(),
  showError: vi.fn(),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    announcements: {
      list: listAnnouncements,
      create: vi.fn(),
      update: vi.fn(),
      delete: vi.fn(),
    },
    groups: { getAll: getAllGroups },
    users: { listTags },
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess: vi.fn() }),
}))

vi.mock('@/composables/usePersistedPageSize', () => ({
  getPersistedPageSize: () => 20,
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

const DataTableStub = {
  props: ['data', 'loading'],
  template: '<div><slot v-if="!loading && data.length === 0" name="empty" /></div>',
}

const EmptyStateStub = {
  props: ['title', 'description', 'actionText'],
  emits: ['action'],
  template: '<button data-test="empty-action" :data-title="title" :data-action-text="actionText" @click="$emit(\'action\')">{{ actionText }}</button>',
}

const BaseDialogStub = {
  props: ['show', 'title'],
  template: '<div v-if="show" data-test="edit-dialog" :data-title="title"><slot /><slot name="footer" /></div>',
}

const stubs = {
  AppLayout: { template: '<div><slot /></div>' },
  TablePageLayout: {
    template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>',
  },
  DataTable: DataTableStub,
  EmptyState: EmptyStateStub,
  BaseDialog: BaseDialogStub,
  Pagination: true,
  ConfirmDialog: true,
  Select: true,
  Icon: true,
  AnnouncementTargetingEditor: true,
  AnnouncementReadStatusDialog: true,
  AnnouncementRichTextEditor: true,
}

const emptyPage = {
  items: [],
  total: 0,
  page: 1,
  page_size: 20,
  pages: 0,
}

describe('AnnouncementsView list states', () => {
  beforeEach(() => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
    listAnnouncements.mockReset()
    getAllGroups.mockReset().mockResolvedValue([])
    listTags.mockReset().mockResolvedValue([])
    showError.mockReset()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('shows the create action for a successfully loaded empty list', async () => {
    listAnnouncements.mockResolvedValue(emptyPage)
    const wrapper = mount(AnnouncementsView, { global: { stubs } })
    await flushPromises()

    const action = wrapper.get('[data-test="empty-action"]')
    expect(action.attributes('data-title')).toBe('empty.noData')
    expect(action.attributes('data-action-text')).toBe('admin.announcements.createAnnouncement')

    await action.trigger('click')
    expect(wrapper.get('[data-test="edit-dialog"]').attributes('data-title')).toBe(
      'admin.announcements.createAnnouncement',
    )
    expect(listAnnouncements).toHaveBeenCalledTimes(1)
  })

  it('shows a retry action after failure and returns to the empty state after retry', async () => {
    listAnnouncements.mockRejectedValueOnce(new Error('network down')).mockResolvedValueOnce(emptyPage)
    const wrapper = mount(AnnouncementsView, { global: { stubs } })
    await flushPromises()

    const retry = wrapper.get('[data-test="empty-action"]')
    expect(retry.attributes('data-title')).toBe('admin.announcements.failedToLoad')
    expect(retry.attributes('data-action-text')).toBe('common.refresh')
    expect(showError).toHaveBeenCalledWith('admin.announcements.failedToLoad')

    await retry.trigger('click')
    await flushPromises()

    expect(listAnnouncements).toHaveBeenCalledTimes(2)
    expect(wrapper.get('[data-test="empty-action"]').attributes('data-title')).toBe('empty.noData')
  })
})
