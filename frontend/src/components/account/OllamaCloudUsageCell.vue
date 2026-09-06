<template>
  <div class="space-y-1 text-[10px] text-gray-500 dark:text-gray-400">
    <div v-if="loading" class="h-3 w-24 animate-pulse rounded bg-gray-200 dark:bg-gray-700" />
    <span v-else-if="error" class="text-red-500">{{ error }}</span>
    <template v-else-if="usage?.five_hour?.window_stats">
      <span class="rounded bg-gray-100 px-1.5 py-0.5 dark:bg-gray-800">{{ usage.five_hour.window_stats.requests }} req</span>
      <span class="ml-1 rounded bg-gray-100 px-1.5 py-0.5 dark:bg-gray-800">{{ usage.five_hour.window_stats.tokens.toLocaleString() }} tok</span>
      <span class="ml-1 rounded bg-gray-100 px-1.5 py-0.5 dark:bg-gray-800">local / 5h</span>
    </template>
    <span v-else>—</span>
  </div>
</template>

<script setup lang="ts">
import type { Account, AccountUsageInfo } from '@/types'

defineProps<{ account: Account; usage?: AccountUsageInfo | null; loading?: boolean; error?: string | null }>()
</script>
