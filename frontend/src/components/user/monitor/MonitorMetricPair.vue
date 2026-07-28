<template>
  <div class="mt-4 grid w-full min-w-0 self-stretch grid-cols-2 gap-2 sm:mt-5">
    <div
      class="min-w-0 rounded-xl border border-gray-100 bg-gray-50/80 p-2.5 dark:border-dark-700/50 dark:bg-dark-900/40 sm:p-3"
    >
      <div
        class="flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-wider text-gray-400"
      >
        <Icon :name="primaryIcon" size="xs" />
        <span class="truncate">{{ primaryLabel }}</span>
      </div>
      <div class="mt-1.5 truncate text-lg font-bold font-mono tabular-nums text-gray-900 dark:text-gray-100">
        {{ primaryValue }}<span class="text-xs font-normal text-gray-400 ml-0.5">{{ primaryUnit }}</span>
      </div>
    </div>
    <div
      class="min-w-0 rounded-xl border border-gray-100 bg-gray-50/80 p-2.5 dark:border-dark-700/50 dark:bg-dark-900/40 sm:p-3"
    >
      <div
        class="flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-wider text-gray-400"
      >
        <Icon :name="secondaryIcon" size="xs" />
        <span class="truncate">{{ secondaryLabel }}</span>
      </div>
      <div class="mt-1.5 truncate text-lg font-bold font-mono tabular-nums text-gray-900 dark:text-gray-100">
        {{ secondaryValue }}<span class="text-xs font-normal text-gray-400 ml-0.5">{{ secondaryUnit }}</span>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import Icon from '@/components/icons/Icon.vue'

defineProps<{
  primaryLabel: string
  primaryValue: string
  primaryUnit: string
  primaryIcon: 'bolt' | 'globe' | 'clock' | 'link'
  secondaryLabel: string
  secondaryValue: string
  secondaryUnit: string
  secondaryIcon: 'bolt' | 'globe' | 'clock' | 'link'
}>()
</script>
