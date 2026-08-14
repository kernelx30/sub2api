import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AccountRankingView from '../AccountRankingView.vue'

const { getAll } = vi.hoisted(() => ({
  getAll: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    groups: { getAll }
  }
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

vi.mock('@/utils/format', () => ({
  formatRelativeTime: (value?: string) => value ?? '-'
}))

function mountView() {
  return mount(AccountRankingView, {
    global: {
      stubs: {
        AppLayout: { template: '<main><slot /></main>' },
        Icon: true,
        RouterLink: { template: '<a><slot /></a>' },
        PoolAutoPriorityLeaderboard: {
          props: ['groups'],
          template: '<div data-testid="standalone-ranking" :data-group-count="groups.length" />'
        }
      }
    }
  })
}

describe('AccountRankingView', () => {
  beforeEach(() => {
    getAll.mockReset()
  })

  it('loads OpenAI groups and renders the ranking as a standalone page', async () => {
    getAll.mockResolvedValue([
      { id: 1, name: 'Cheap', platform: 'openai', status: 'active', sort_order: 1 },
      { id: 2, name: 'Plus', platform: 'openai', status: 'active', sort_order: 2 }
    ])

    const wrapper = mountView()
    await flushPromises()

    expect(getAll).toHaveBeenCalledWith('openai')
    expect(wrapper.get('[data-testid="standalone-ranking"]').attributes('data-group-count')).toBe('2')
  })

  it('keeps the ranking page separate when group loading fails', async () => {
    getAll.mockRejectedValue(new Error('network'))

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.text()).toContain('admin.accounts.poolRanking.groupsLoadFailed')
    expect(wrapper.find('[data-testid="standalone-ranking"]').exists()).toBe(false)
  })
})
