<template>
  <div class="flex min-w-[150px] items-stretch gap-3" :title="title">
    <div class="flex w-2 shrink-0 flex-col overflow-hidden rounded-full bg-gray-200 dark:bg-gray-700" aria-hidden="true">
      <div class="min-h-1 flex-1" :class="healthBarClass(firstTokenHealth.state)" />
      <div class="min-h-1 flex-1" :class="healthBarClass(durationHealth.state)" />
    </div>
    <div class="space-y-1 text-xs leading-5">
      <div class="flex items-center gap-3">
        <span class="text-gray-500 dark:text-gray-400">{{ firstTokenLabel }}</span>
        <span :class="healthTextClass(firstTokenHealth.state)">{{ formatDuration(firstTokenMs) }}</span>
      </div>
      <div class="flex items-center gap-3">
        <span class="text-gray-500 dark:text-gray-400">{{ durationLabel }}</span>
        <span :class="healthTextClass(durationHealth.state)">{{ formatDuration(durationMs) }}</span>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { formatDuration, latencyHealth } from '@/utils/duration'

const props = withDefaults(defineProps<{
  firstTokenMs: number | null | undefined
  durationMs: number | null | undefined
  isError?: boolean
  firstTokenLabel?: string
  durationLabel?: string
}>(), {
  firstTokenLabel: '首字',
  durationLabel: '总耗时',
})
const firstTokenLabel = computed(() => props.firstTokenLabel)
const durationLabel = computed(() => props.durationLabel)
const firstTokenHealth = computed(() => latencyHealth(props.firstTokenMs, props.isError, 'first_token'))
const durationHealth = computed(() => latencyHealth(props.durationMs, props.isError))

function healthTextClass(state: ReturnType<typeof latencyHealth>['state']) {
  return {
    'text-gray-400 dark:text-gray-500': state === 'missing',
    'text-emerald-600 dark:text-emerald-400': state === 'good',
    'text-amber-600 dark:text-amber-400': state === 'warn',
    'text-orange-600 dark:text-orange-400': state === 'slow',
    'text-red-600 dark:text-red-400': state === 'critical' || state === 'error',
  }
}

function healthBarClass(state: ReturnType<typeof latencyHealth>['state']) {
  return {
    'bg-gray-300 dark:bg-gray-600': state === 'missing',
    'bg-emerald-500': state === 'good',
    'bg-amber-400': state === 'warn',
    'bg-orange-500': state === 'slow',
    'bg-red-500': state === 'critical' || state === 'error',
  }
}
const title = computed(() => `TTFT ${formatDuration(props.firstTokenMs)} · Total ${formatDuration(props.durationMs)}`)
</script>
