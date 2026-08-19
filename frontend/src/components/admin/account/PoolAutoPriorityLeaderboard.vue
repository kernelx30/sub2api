<template>
  <section
    v-if="groupOptions.length > 0"
    class="overflow-hidden rounded-lg border border-gray-200 bg-gray-50/70 shadow-sm dark:border-dark-700 dark:bg-dark-900/30"
    data-testid="pool-auto-priority-leaderboard"
  >
    <div class="flex flex-wrap items-center justify-between gap-3 px-3 py-2.5">
      <div class="flex min-w-0 items-center gap-2.5">
        <span class="flex h-8 w-8 flex-none items-center justify-center rounded-md bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300">
          <Icon name="trophy" size="sm" />
        </span>
        <div class="min-w-0">
          <div class="flex flex-wrap items-center gap-2">
            <h2 class="text-sm font-semibold text-gray-900 dark:text-white">
              {{ t('admin.accounts.poolRanking.title') }}
            </h2>
            <span
              :class="[
                'rounded px-1.5 py-0.5 text-[10px] font-medium',
                ranking?.enabled
                  ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
                  : 'bg-gray-200 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
              ]"
            >
              {{ ranking?.enabled ? t('admin.accounts.poolRanking.enabled') : t('admin.accounts.poolRanking.disabled') }}
            </span>
          </div>
          <p class="mt-0.5 text-xs text-gray-500 dark:text-dark-400">
            {{ t('admin.accounts.poolRanking.cadence', { minutes: ranking?.interval_minutes ?? 5 }) }}
          </p>
        </div>
      </div>

      <div class="flex items-center gap-2">
        <select
          v-model.number="currentGroupId"
          class="h-8 min-w-[9rem] rounded-md border border-gray-300 bg-white px-2 text-xs text-gray-700 focus:border-primary-500 focus:outline-none focus:ring-1 focus:ring-primary-500 dark:border-dark-600 dark:bg-dark-800 dark:text-gray-200"
          :aria-label="t('admin.accounts.poolRanking.group')"
          data-testid="pool-ranking-group-select"
        >
          <option v-for="group in groupOptions" :key="group.id" :value="group.id">
            {{ group.name }}
          </option>
        </select>
        <select
          v-model="currentModel"
          class="h-8 min-w-[10rem] rounded-md border border-gray-300 bg-white px-2 text-xs text-gray-700 focus:border-primary-500 focus:outline-none focus:ring-1 focus:ring-primary-500 dark:border-dark-600 dark:bg-dark-800 dark:text-gray-200"
          aria-label="排序模型"
          data-testid="pool-ranking-model-select"
        >
          <option value="">通用模型</option>
          <option v-for="model in modelOptions" :key="model" :value="model">
            {{ model }}
          </option>
        </select>
        <button
          type="button"
          class="btn btn-secondary h-8 w-8 p-0"
          :disabled="loading"
          :title="t('common.refresh')"
          :aria-label="t('common.refresh')"
          data-testid="pool-ranking-refresh"
          @click="loadRanking"
        >
          <Icon name="refresh" size="sm" :class="loading ? 'animate-spin' : ''" />
        </button>
      </div>
    </div>

    <div v-if="error" class="border-t border-red-200 px-3 py-2 text-xs text-red-700 dark:border-red-900/50 dark:text-red-300">
      {{ t('admin.accounts.poolRanking.loadFailed') }}
    </div>

    <div v-else class="overflow-x-auto border-t border-gray-200 dark:border-dark-700">
      <table class="w-full min-w-[1040px] table-fixed text-left text-xs">
        <thead class="bg-white text-gray-500 dark:bg-dark-800 dark:text-dark-400">
          <tr>
            <th class="w-14 px-3 py-2 font-medium">{{ t('admin.accounts.poolRanking.rank') }}</th>
            <th class="w-52 px-3 py-2 font-medium">{{ t('admin.accounts.poolRanking.account') }}</th>
            <th class="w-24 px-3 py-2 font-medium">{{ t('admin.accounts.poolRanking.status') }}</th>
            <th class="w-24 px-3 py-2 font-medium">P50</th>
            <th class="w-24 px-3 py-2 font-medium">P95</th>
            <th class="w-24 px-3 py-2 font-medium">{{ t('admin.accounts.poolRanking.latest') }}</th>
            <th class="w-32 px-3 py-2 font-medium">{{ t('admin.accounts.poolRanking.runtimeTTFT') }}</th>
            <th class="w-28 px-3 py-2 font-medium">{{ t('admin.accounts.poolRanking.successRate') }}</th>
            <th class="w-32 px-3 py-2 font-medium">{{ t('admin.accounts.poolRanking.balance') }}</th>
            <th class="w-28 px-3 py-2 font-medium">{{ t('admin.accounts.poolRanking.lastProbe') }}</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-100 bg-white dark:divide-dark-700 dark:bg-dark-800/60">
          <tr v-if="loading && entries.length === 0">
            <td colspan="10" class="px-3 py-8 text-center text-gray-400 dark:text-dark-500">
              {{ t('common.loading') }}
            </td>
          </tr>
          <tr v-else-if="entries.length === 0">
            <td colspan="10" class="px-3 py-8 text-center text-gray-400 dark:text-dark-500">
              {{ t('admin.accounts.poolRanking.empty') }}
            </td>
          </tr>
          <template v-else>
            <tr
              v-for="entry in entries"
              :key="entry.account_id"
              class="text-gray-700 dark:text-gray-300"
              :data-testid="`pool-ranking-row-${entry.account_id}`"
            >
              <td class="px-3 py-2.5">
                <span
                  v-if="entry.rank != null"
                  :class="[
                    'inline-flex h-6 min-w-6 items-center justify-center rounded px-1.5 font-mono font-semibold',
                    entry.rank === 1
                      ? 'bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-200'
                      : 'bg-gray-100 text-gray-700 dark:bg-dark-700 dark:text-gray-200'
                  ]"
                >
                  {{ entry.rank }}
                </span>
                <span v-else class="text-gray-400 dark:text-dark-500">—</span>
              </td>
              <td class="px-3 py-2.5">
                <div class="truncate font-medium text-gray-900 dark:text-white" :title="entry.account_name">
                  {{ entry.account_name }}
                </div>
                <div class="mt-0.5 font-mono text-[10px] text-gray-400 dark:text-dark-500">
                  #{{ entry.account_id }} · P{{ entry.manual_priority }}
                </div>
              </td>
              <td class="px-3 py-2.5">
                <span :class="['rounded px-1.5 py-0.5 text-[10px] font-medium', statusClass(entry)]">
                  {{ statusLabel(entry) }}
                </span>
              </td>
              <td class="px-3 py-2.5 font-mono">{{ formatLatency(entry.p50_latency_ms) }}</td>
              <td class="px-3 py-2.5 font-mono">{{ formatLatency(entry.p95_latency_ms) }}</td>
              <td class="px-3 py-2.5 font-mono">{{ formatLatency(entry.latest_latency_ms) }}</td>
              <td class="px-3 py-2.5" data-testid="pool-ranking-runtime-ttft">
                <div class="font-mono">{{ formatLatency(entry.runtime_ttft_ms) }}</div>
                <div class="mt-0.5 text-[10px] text-gray-400 dark:text-dark-500">
                  {{ rankingSourceLabel(entry) }}<span v-if="entry.runtime_updated_at"> · {{ formatRelativeTime(entry.runtime_updated_at) }}</span><span v-if="entry.runtime_sample_count > 0"> / {{ entry.runtime_sample_count }}</span>
                </div>
              </td>
              <td class="px-3 py-2.5 font-mono">{{ formatSuccess(entry) }}</td>
              <td class="px-3 py-2.5 font-mono" :title="balanceTitle(entry)" data-testid="pool-ranking-balance">
                {{ formatBalance(entry) }}
              </td>
              <td class="px-3 py-2.5 text-gray-500 dark:text-dark-400">
                {{ formatRelativeTime(entry.last_probe_at) }}
              </td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>

    <div
      v-if="ranking?.generated_at"
      class="flex flex-wrap items-center justify-between gap-2 border-t border-gray-200 px-3 py-2 text-[10px] text-gray-400 dark:border-dark-700 dark:text-dark-500"
    >
      <span>{{ t('admin.accounts.poolRanking.displayed', { count: entries.length, total: ranking.total }) }}</span>
      <span>{{ t('admin.accounts.poolRanking.cachedAt', { time: formatRelativeTime(ranking.generated_at) }) }}</span>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import Icon from '@/components/icons/Icon.vue'
import { formatRelativeTime } from '@/utils/format'
import type {
  AdminGroup,
  PoolAutoPriorityRankingEntry,
  PoolAutoPriorityRankingResponse
} from '@/types'

const props = defineProps<{
  groups: AdminGroup[]
  selectedGroup?: string
}>()

const emit = defineEmits<{
  updated: [entries: PoolAutoPriorityRankingEntry[]]
}>()

const { t } = useI18n()
const currentGroupId = ref<number | null>(null)
const currentModel = ref('')
const ranking = ref<PoolAutoPriorityRankingResponse | null>(null)
const entries = ref<PoolAutoPriorityRankingEntry[]>([])
const loading = ref(false)
const error = ref(false)
let requestSequence = 0
let pollTimer: ReturnType<typeof setInterval> | null = null

const groupOptions = computed(() =>
  props.groups
    .filter(group => group.platform === 'openai' && group.status === 'active')
    .slice()
    .sort((left, right) => left.sort_order - right.sort_order || left.id - right.id)
)

const modelOptions = computed(() => {
  const group = groupOptions.value.find(item => item.id === currentGroupId.value)
  const models = new Set<string>()
  for (const pricing of group?.model_pricing ?? []) {
    for (const model of pricing.models ?? []) {
      if (model.trim()) models.add(model.trim())
    }
  }
  if (group?.default_mapped_model?.trim()) models.add(group.default_mapped_model.trim())
  return [...models].sort()
})

function syncGroupSelection(): void {
  const selected = Number(props.selectedGroup)
  if (Number.isInteger(selected) && groupOptions.value.some(group => group.id === selected)) {
    currentGroupId.value = selected
    return
  }
  if (currentGroupId.value != null && groupOptions.value.some(group => group.id === currentGroupId.value)) {
    return
  }
  currentGroupId.value = groupOptions.value[0]?.id ?? null
  currentModel.value = ''
}

async function loadRanking(): Promise<void> {
  const groupId = currentGroupId.value
  const sequence = ++requestSequence
  if (groupId == null) {
    loading.value = false
    error.value = false
    return
  }
  loading.value = true
  error.value = false
  try {
    const result = currentModel.value
      ? await adminAPI.accounts.getPoolAutoPriorityRanking(groupId, 50, currentModel.value)
      : await adminAPI.accounts.getPoolAutoPriorityRanking(groupId, 50)
    if (sequence !== requestSequence) return
    ranking.value = result
    entries.value = result.items
    emit('updated', result.items)
  } catch {
    if (sequence !== requestSequence) return
    error.value = true
  } finally {
    if (sequence === requestSequence) loading.value = false
  }
}

function formatLatency(value: number): string {
  return value > 0 ? `${Math.round(value)} ms` : '—'
}

function formatSuccess(entry: PoolAutoPriorityRankingEntry): string {
  if (entry.sample_count <= 0) return '—'
  return `${Math.round(entry.success_rate * 100)}% · ${entry.sample_count}`
}

function rankingSourceLabel(entry: PoolAutoPriorityRankingEntry): string {
  return entry.ranking_source === 'real_traffic'
    ? t('admin.accounts.poolRanking.realTrafficSource')
    : t('admin.accounts.poolRanking.probeSource')
}

function formatBalance(entry: PoolAutoPriorityRankingEntry): string {
  if (entry.balance_unlimited) {
    if (entry.balance_source === 'upstream_api_key_quota') {
      return t('admin.accounts.poolRanking.keyUnlimited')
    }
    if (entry.balance_source === 'upstream_subscription') {
      return t('admin.accounts.poolRanking.subscriptionUnlimited')
    }
    return '—'
  }
  if (
    entry.available_balance == null ||
    !['upstream_api_key_quota', 'upstream_wallet', 'upstream_subscription'].includes(
      entry.balance_source ?? ''
    )
  ) {
    return '—'
  }
  return `$${entry.available_balance.toFixed(entry.available_balance < 10 ? 2 : 1)}`
}

function balanceTitle(entry: PoolAutoPriorityRankingEntry): string {
  if (entry.balance_source === 'upstream_wallet') {
    return t('admin.accounts.poolRanking.upstreamWalletBalance')
  }
  if (entry.balance_source === 'upstream_subscription') {
    return t('admin.accounts.poolRanking.upstreamSubscriptionBalance')
  }
  if (entry.balance_source === 'upstream_api_key_quota') {
    return t('admin.accounts.poolRanking.upstreamBalance')
  }
  return t('admin.accounts.poolRanking.unknownBalance')
}

function statusLabel(entry: PoolAutoPriorityRankingEntry): string {
  if (!entry.schedulable) return t('admin.accounts.poolRanking.notSchedulable')
  return t(`admin.accounts.poolRanking.probeStatus.${entry.probe_status}`)
}

function statusClass(entry: PoolAutoPriorityRankingEntry): string {
  if (!entry.schedulable) return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
  switch (entry.probe_status) {
    case 'ok':
      return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
    case 'failed':
      return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
    case 'stale':
    case 'unsupported':
      return 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
    default:
      return 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'
  }
}

watch(
  () => [props.selectedGroup, props.groups.map(group => `${group.id}:${group.status}:${group.platform}`).join(',')],
  syncGroupSelection,
  { immediate: true }
)

watch(currentGroupId, (next, previous) => {
  if (next !== previous) currentModel.value = ''
  entries.value = []
  ranking.value = null
  emit('updated', [])
  void loadRanking()
})

watch(currentModel, () => {
  entries.value = []
  ranking.value = null
  emit('updated', [])
  void loadRanking()
})

onMounted(() => {
  void loadRanking()
  pollTimer = setInterval(() => void loadRanking(), 60_000)
})

onUnmounted(() => {
  requestSequence++
  if (pollTimer != null) clearInterval(pollTimer)
})
</script>
