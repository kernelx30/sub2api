<template>
  <section class="py-3 md:py-4">
    <div class="flex items-center justify-end gap-3 flex-wrap">
      <span class="inline-flex items-center gap-1.5 text-xs font-medium text-gray-500 dark:text-gray-400">
        <Icon name="clock" size="sm" />
        {{ t('channelStatus.windowLabel') }}
      </span>

      <span
        class="inline-flex items-center px-2.5 py-1 rounded-full text-xs font-semibold tracking-wider uppercase"
        :class="overallChipClass"
      >
        <span
          class="w-1.5 h-1.5 rounded-full mr-1.5"
          :class="overallDotClass"
        ></span>
        {{ overallLabel }}
      </span>

      <button
        type="button"
        class="h-8 w-8 rounded-lg flex items-center justify-center text-gray-500 hover:text-gray-700 hover:bg-gray-100 dark:text-gray-400 dark:hover:text-gray-200 dark:hover:bg-dark-700 transition-colors disabled:opacity-50"
        :disabled="loading"
        :title="t('common.refresh')"
        @click="emit('refresh')"
      >
        <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
      </button>

      <span class="inline-flex items-center gap-1.5 text-xs font-medium tabular-nums text-gray-500 dark:text-gray-400">
        <Icon name="refresh" size="sm" />
        {{ t('common.autoRefresh.countdown', { seconds: countdownSeconds }) }}
      </span>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'

export type OverallStatus = 'operational' | 'degraded'

const props = defineProps<{
  overallStatus: OverallStatus
  loading: boolean
  countdownSeconds: number
}>()

const emit = defineEmits<{
  (e: 'refresh'): void
}>()

const { t } = useI18n()

const overallLabel = computed(() => t(`channelStatus.overall.${props.overallStatus}`))

const overallChipClass = computed(() => {
  switch (props.overallStatus) {
    case 'operational':
      return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300'
    case 'degraded':
    default:
      return 'bg-amber-100 text-amber-700 dark:bg-amber-500/15 dark:text-amber-300'
  }
})

const overallDotClass = computed(() => {
  switch (props.overallStatus) {
    case 'operational':
      return 'bg-emerald-500 animate-pulse'
    case 'degraded':
    default:
      return 'bg-amber-500 animate-pulse'
  }
})
</script>
