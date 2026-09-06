<template>
  <section class="card overflow-hidden">
    <div class="flex items-center justify-between border-b border-gray-200 px-4 py-3 dark:border-dark-700">
      <div>
        <h2 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('usage.tokenRanking.title') }}</h2>
        <p class="mt-0.5 text-xs text-gray-500 dark:text-gray-400">{{ t('usage.tokenRanking.range', { start: startDate, end: endDate }) }}</p>
      </div>
      <button class="btn btn-secondary px-3 py-1.5 text-xs" type="button" :disabled="loading" @click="load">
        {{ loading ? t('common.loading') : t('common.refresh') }}
      </button>
    </div>

    <div v-if="loading" class="p-8 text-center text-sm text-gray-500 dark:text-gray-400">{{ t('common.loading') }}</div>
    <div v-else-if="error" class="p-8 text-center text-sm text-red-600 dark:text-red-400">{{ t('usage.tokenRanking.failed') }}</div>
    <div v-else-if="items.length === 0" class="p-8 text-center text-sm text-gray-500 dark:text-gray-400">{{ t('usage.tokenRanking.empty') }}</div>
    <div v-else class="overflow-x-auto">
      <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700">
        <thead class="bg-gray-50 text-left text-xs uppercase tracking-wide text-gray-500 dark:bg-dark-800/60 dark:text-gray-400">
          <tr>
            <th class="px-4 py-3">#</th>
            <th class="px-4 py-3">{{ t('usage.tokenRanking.user') }}</th>
            <th class="px-4 py-3 text-right">{{ t('usage.totalTokens') }}</th>
            <th class="px-4 py-3 text-right">{{ t('usage.totalRequests') }}</th>
            <th class="px-4 py-3 text-right">{{ t('usage.actualCost') }}</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-100 dark:divide-dark-800">
          <tr v-for="(item, index) in items" :key="item.user_id" class="hover:bg-gray-50 dark:hover:bg-dark-800/60">
            <td class="px-4 py-3 font-medium text-gray-500 dark:text-gray-400">{{ index + 1 }}</td>
            <td class="px-4 py-3">
              <button
                type="button"
                class="font-medium text-primary-700 underline decoration-dashed underline-offset-2 transition-colors hover:text-primary-800 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 dark:text-primary-300 dark:hover:text-primary-200"
                @click="$emit('userClick', item.user_id)"
              >
                {{ item.email || `#${item.user_id}` }}
              </button>
            </td>
            <td class="px-4 py-3 text-right text-gray-900 dark:text-white">{{ formatNumber(item.tokens) }}</td>
            <td class="px-4 py-3 text-right text-gray-600 dark:text-gray-300">{{ formatNumber(item.requests) }}</td>
            <td class="px-4 py-3 text-right text-gray-600 dark:text-gray-300">{{ item.actual_cost.toFixed(6) }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { getUserSpendingRanking } from '@/api/admin/dashboard'
import type { UserSpendingRankingItem } from '@/types'

const props = defineProps<{ startDate: string; endDate: string; userId?: number; active?: boolean }>()
defineEmits<{ (event: 'userClick', userId: number): void }>()
const { t } = useI18n()
const items = ref<UserSpendingRankingItem[]>([])
const loading = ref(false)
const error = ref(false)
const loaded = ref(false)

const formatNumber = (value: number) => new Intl.NumberFormat().format(value || 0)

const load = async () => {
  if (loading.value) return
  loading.value = true
  error.value = false
  try {
    const response = await getUserSpendingRanking({ start_date: props.startDate, end_date: props.endDate, user_id: props.userId, limit: 50 })
    items.value = [...(response.ranking || [])].sort((a, b) => b.tokens - a.tokens || a.user_id - b.user_id)
    loaded.value = true
  } catch {
    error.value = true
  } finally {
    loading.value = false
  }
}

watch(() => [props.active, props.startDate, props.endDate, props.userId] as const, ([active], previous) => {
  if (!active) return
  const filterChanged = previous && (previous[1] !== props.startDate || previous[2] !== props.endDate || previous[3] !== props.userId)
  if (!loaded.value || filterChanged) void load()
}, { immediate: true })
</script>
