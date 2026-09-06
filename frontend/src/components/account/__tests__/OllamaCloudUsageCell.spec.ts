import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import OllamaCloudUsageCell from '../OllamaCloudUsageCell.vue'
import type { Account, AccountUsageInfo } from '@/types'

const account = { id: 7, platform: 'ollama', type: 'apikey' } as Account

describe('OllamaCloudUsageCell', () => {
  it('shows the local five-hour request and token snapshot', () => {
    const usage: AccountUsageInfo = {
      updated_at: '2026-08-29T00:00:00Z',
      five_hour: {
        window_stats: { requests: 12, tokens: 12345, cost: 0.2, standard_cost: 0.2, user_cost: 0.3 }
      },
      seven_day: null,
      seven_day_sonnet: null
    }
    const wrapper = mount(OllamaCloudUsageCell, { props: { account, usage } })

    expect(wrapper.text()).toContain('12 req')
    expect(wrapper.text()).toContain('12,345 tok')
    expect(wrapper.text()).toContain('local / 5h')
  })

  it('keeps loading, error and empty states explicit', () => {
    expect(mount(OllamaCloudUsageCell, { props: { account, loading: true } }).find('.animate-pulse').exists()).toBe(true)
    expect(mount(OllamaCloudUsageCell, { props: { account, error: 'failed' } }).text()).toContain('failed')
    expect(mount(OllamaCloudUsageCell, { props: { account, usage: null } }).text()).toContain('—')
  })
})
