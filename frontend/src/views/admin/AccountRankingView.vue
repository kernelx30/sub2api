<template>
  <AppLayout>
    <div class="mx-auto w-full max-w-[1600px] space-y-4 pb-8">
      <div class="flex justify-end">
        <RouterLink to="/admin/accounts" class="btn btn-secondary">
          <Icon name="arrowLeft" size="sm" />
          <span>{{ t('admin.accounts.poolRanking.backToAccounts') }}</span>
        </RouterLink>
      </div>

      <div
        v-if="loadingGroups"
        class="rounded-lg border border-gray-200 bg-white px-4 py-12 text-center text-sm text-gray-500 shadow-sm dark:border-dark-700 dark:bg-dark-800 dark:text-dark-400"
      >
        {{ t('common.loading') }}
      </div>

      <div
        v-else-if="groupsError"
        class="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700 dark:border-red-900/50 dark:bg-red-950/20 dark:text-red-300"
      >
        <span>{{ t('admin.accounts.poolRanking.groupsLoadFailed') }}</span>
        <button type="button" class="btn btn-secondary" @click="loadGroups">
          <Icon name="refresh" size="sm" />
          <span>{{ t('common.refresh') }}</span>
        </button>
      </div>

      <div
        v-else-if="groups.length === 0"
        class="rounded-lg border border-gray-200 bg-white px-4 py-12 text-center text-sm text-gray-500 shadow-sm dark:border-dark-700 dark:bg-dark-800 dark:text-dark-400"
      >
        {{ t('admin.accounts.poolRanking.noGroups') }}
      </div>

      <PoolAutoPriorityLeaderboard v-else :groups="groups" />
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import PoolAutoPriorityLeaderboard from '@/components/admin/account/PoolAutoPriorityLeaderboard.vue'
import Icon from '@/components/icons/Icon.vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import type { AdminGroup } from '@/types'

const { t } = useI18n()
const groups = ref<AdminGroup[]>([])
const loadingGroups = ref(true)
const groupsError = ref(false)

async function loadGroups(): Promise<void> {
  loadingGroups.value = true
  groupsError.value = false
  try {
    groups.value = await adminAPI.groups.getAll('openai')
  } catch {
    groupsError.value = true
  } finally {
    loadingGroups.value = false
  }
}

onMounted(() => {
  void loadGroups()
})
</script>
