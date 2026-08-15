import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'

import PoolAutoPriorityLeaderboard from '../PoolAutoPriorityLeaderboard.vue'
import type {
  AdminGroup,
  PoolAutoPriorityRankingEntry,
  PoolAutoPriorityRankingResponse
} from '@/types'

const { getPoolAutoPriorityRanking } = vi.hoisted(() => ({
  getPoolAutoPriorityRanking: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getPoolAutoPriorityRanking
    }
  }
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) => {
      if (!params) return key
      return `${key}:${Object.values(params).join(',')}`
    }
  })
}))

vi.mock('@/utils/format', () => ({
  formatRelativeTime: (value?: string) => value ?? '-'
}))

const groups = [
  { id: 1, name: 'Cheap', platform: 'openai', status: 'active', sort_order: 1 },
  { id: 2, name: 'Plus', platform: 'openai', status: 'active', sort_order: 2 },
  { id: 3, name: 'Claude', platform: 'anthropic', status: 'active', sort_order: 3 }
] as AdminGroup[]

function rankingEntry(overrides: Partial<PoolAutoPriorityRankingEntry>): PoolAutoPriorityRankingEntry {
  return {
    rank: 1,
    cohort_rank: 1,
    account_id: 1,
    account_name: 'fast-upstream',
    group_id: 1,
    manual_priority: 10,
    schedulable: true,
    probe_status: 'ok',
    sample_count: 5,
    success_rate: 1,
    p50_latency_ms: 1200,
    p95_latency_ms: 1800,
    latest_latency_ms: 1300,
    consecutive_failures: 0,
    last_probe_at: '2026-08-14T08:00:00Z',
    balance_unlimited: false,
    runtime_ttft_ms: 1450,
    runtime_sample_count: 4,
    runtime_mature: true,
    ranking_source: 'real_traffic',
    ...overrides
  }
}

function rankingResponse(
  groupId: number,
  items: PoolAutoPriorityRankingEntry[]
): PoolAutoPriorityRankingResponse {
  return {
    enabled: true,
    interval_minutes: 5,
    group_id: groupId,
    generated_at: '2026-08-14T08:01:00Z',
    total: items.length,
    items
  }
}

function mountLeaderboard(selectedGroup = '1'): VueWrapper {
  return mount(PoolAutoPriorityLeaderboard, {
    props: { groups, selectedGroup },
    global: {
      stubs: {
        Icon: true
      }
    }
  })
}

describe('PoolAutoPriorityLeaderboard', () => {
  let wrapper: VueWrapper | undefined

  beforeEach(() => {
    vi.useFakeTimers()
    getPoolAutoPriorityRanking.mockReset()
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = undefined
    vi.useRealTimers()
  })

  it('renders the cached ranking in API order and reports it to the account table', async () => {
    const entries = [
      rankingEntry({
        rank: 1,
        account_id: 7,
        account_name: 'fast-upstream',
        available_balance: 75,
        balance_source: 'upstream_api_key_quota'
      }),
      rankingEntry({
        rank: 2,
        account_id: 9,
        account_name: 'backup-upstream',
        available_balance: -0.11810209,
        balance_source: 'upstream_wallet'
      }),
      rankingEntry({
        rank: null,
        cohort_rank: null,
        account_id: 11,
        account_name: 'unknown-balance',
        schedulable: false,
        probe_status: 'unmeasured',
        sample_count: 0,
        success_rate: 0,
        p50_latency_ms: 0,
        p95_latency_ms: 0,
        latest_latency_ms: 0
      })
    ]
    getPoolAutoPriorityRanking.mockResolvedValue(rankingResponse(1, entries))

    wrapper = mountLeaderboard()
    await flushPromises()

    expect(getPoolAutoPriorityRanking).toHaveBeenCalledWith(1, 50)
    const rows = wrapper.findAll('[data-testid^="pool-ranking-row-"]')
    expect(rows.map(row => row.attributes('data-testid'))).toEqual([
      'pool-ranking-row-7',
      'pool-ranking-row-9',
      'pool-ranking-row-11'
    ])
    expect(rows[0].text()).toContain('$75.0')
    expect(rows[1].text()).toContain('$-0.12')
    expect(rows[1].find('[data-testid="pool-ranking-balance"]').attributes('title')).toBe(
      'admin.accounts.poolRanking.upstreamWalletBalance'
    )
    expect(rows[0].find('[data-testid="pool-ranking-runtime-ttft"]').text()).toContain('1450 ms')
    expect(rows[0].find('[data-testid="pool-ranking-runtime-ttft"]').text()).toContain(
      'admin.accounts.poolRanking.realTrafficSource'
    )
    expect(rows[2].find('[data-testid="pool-ranking-balance"]').text()).toBe('—')
    expect(wrapper.emitted('updated')?.at(-1)?.[0]).toEqual(entries)
  })

  it('hides legacy local balance estimates', async () => {
    const entries = [
      rankingEntry({
        account_id: 7,
        available_balance: 999,
        balance_source: 'local_account_quota'
      })
    ]
    getPoolAutoPriorityRanking.mockResolvedValue(rankingResponse(1, entries))

    wrapper = mountLeaderboard()
    await flushPromises()

    const balance = wrapper.get('[data-testid="pool-ranking-balance"]')
    expect(balance.text()).not.toContain('$')
    expect(balance.attributes('title')).toBe('admin.accounts.poolRanking.unknownBalance')
  })

  it('renders a stable empty state when the selected group has no participants', async () => {
    getPoolAutoPriorityRanking.mockResolvedValue(rankingResponse(1, []))

    wrapper = mountLeaderboard()
    await flushPromises()

    expect(wrapper.text()).toContain('admin.accounts.poolRanking.empty')
  })

  it('switches groups and refreshes the selected group cache every 60 seconds', async () => {
    getPoolAutoPriorityRanking.mockImplementation(async (groupId: number) =>
      rankingResponse(groupId, [rankingEntry({ account_id: groupId, group_id: groupId })])
    )

    wrapper = mountLeaderboard()
    await flushPromises()

    await wrapper.get('[data-testid="pool-ranking-group-select"]').setValue('2')
    await flushPromises()

    expect(getPoolAutoPriorityRanking).toHaveBeenNthCalledWith(1, 1, 50)
    expect(getPoolAutoPriorityRanking).toHaveBeenNthCalledWith(2, 2, 50)

    await vi.advanceTimersByTimeAsync(60_000)
    await flushPromises()

    expect(getPoolAutoPriorityRanking).toHaveBeenNthCalledWith(3, 2, 50)
  })
})
