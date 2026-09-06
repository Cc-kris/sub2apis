<template>
  <div class="rounded-lg border border-gray-200 bg-gray-50 p-3 dark:border-dark-600 dark:bg-dark-800">
    <!-- Collapsed summary header (clickable) -->
    <div
      class="flex cursor-pointer select-none items-center gap-2"
      @click="collapsed = !collapsed"
    >
      <Icon
        :name="collapsed ? 'chevronRight' : 'chevronDown'"
        size="sm"
        :stroke-width="2"
        class="flex-shrink-0 text-gray-400 transition-transform duration-200"
      />

      <!-- Summary: model tags + billing badge -->
      <div v-if="collapsed" class="flex min-w-0 flex-1 items-center gap-2 overflow-hidden">
        <!-- Compact model tags (show first 3) -->
        <div class="flex min-w-0 flex-1 flex-wrap items-center gap-1">
          <span
            v-for="(m, i) in entry.models.slice(0, 3)"
            :key="i"
            class="inline-flex shrink-0 rounded px-1.5 py-0.5 text-xs"
            :class="getPlatformTagClass(props.platform || '')"
          >
            {{ m }}
          </span>
          <span
            v-if="entry.models.length > 3"
            class="whitespace-nowrap text-xs text-gray-400"
          >
            +{{ entry.models.length - 3 }}
          </span>
          <span
            v-if="entry.models.length === 0"
            class="text-xs italic text-gray-400"
          >
            {{ t('admin.channels.form.noModels', '未添加模型') }}
          </span>
        </div>

        <!-- Billing mode badge -->
        <span
          class="flex-shrink-0 rounded-full bg-primary-100 px-2 py-0.5 text-xs font-medium text-primary-700 dark:bg-primary-900/30 dark:text-primary-300"
        >
          {{ billingModeLabel }}
        </span>
      </div>

      <!-- Expanded: show the label "Pricing Entry" or similar -->
      <div v-else class="flex-1 text-xs font-medium text-gray-500 dark:text-gray-400">
        {{ t('admin.channels.form.pricingEntry', '定价配置') }}
      </div>

      <!-- Remove button (always visible, stop propagation) -->
      <button
        type="button"
        @click.stop="emit('remove')"
        class="flex-shrink-0 rounded p-1 text-gray-400 hover:text-red-500"
      >
        <Icon name="trash" size="sm" />
      </button>
    </div>

    <!-- Expandable content with transition -->
    <div
      class="collapsible-content"
      :class="{ 'collapsible-content--collapsed': collapsed }"
    >
      <div class="collapsible-inner">
        <!-- Header: Models + Billing Mode -->
        <div class="mt-3 flex items-start gap-2">
          <div class="flex-1">
            <label class="text-xs font-medium text-gray-500 dark:text-gray-400">
              {{ t('admin.channels.form.models', '模型列表') }} <span class="text-red-500">*</span>
            </label>
            <ModelTagInput
              :models="entry.models"
              :platform="props.platform"
              @update:models="onModelsUpdate($event)"
              :placeholder="t('admin.channels.form.modelsPlaceholder', '输入模型名后按回车添加，支持通配符 *')"
              class="mt-1"
            />
          </div>
          <div class="w-40">
            <label class="text-xs font-medium text-gray-500 dark:text-gray-400">
              {{ t('admin.channels.form.billingMode', '计费模式') }}
            </label>
            <Select
              :modelValue="entry.billing_mode"
              @update:modelValue="emit('update', { ...entry, billing_mode: $event as BillingMode, intervals: [] })"
              :options="billingModeOptions"
              class="mt-1"
            />
          </div>
        </div>

        <!-- Token mode -->
        <div v-if="entry.billing_mode === 'token'">
          <!-- Default prices (fallback when no interval matches) -->
          <label class="mt-3 block text-xs font-medium text-gray-500 dark:text-gray-400">
            {{ t('admin.channels.form.defaultPrices', '默认价格（未命中区间时使用）') }}
            <span class="ml-1 font-normal text-gray-400">$/MTok</span>
          </label>
          <div class="mt-1 grid grid-cols-2 gap-2 sm:grid-cols-5">
            <div>
              <label class="text-xs text-gray-400">{{ t('admin.channels.form.inputPrice', '输入') }}</label>
              <input :value="entry.input_price" @input="emitField('input_price', ($event.target as HTMLInputElement).value)"
                type="number" step="any" min="0" class="input mt-0.5 text-sm" :placeholder="t('admin.channels.form.pricePlaceholder', '默认')" />
            </div>
            <div>
              <label class="text-xs text-gray-400">{{ t('admin.channels.form.outputPrice', '输出') }}</label>
              <input :value="entry.output_price" @input="emitField('output_price', ($event.target as HTMLInputElement).value)"
                type="number" step="any" min="0" class="input mt-0.5 text-sm" :placeholder="t('admin.channels.form.pricePlaceholder', '默认')" />
            </div>
            <div>
              <label class="text-xs text-gray-400">{{ t('admin.channels.form.cacheWritePrice', '缓存写入') }}</label>
              <input :value="entry.cache_write_price" @input="emitField('cache_write_price', ($event.target as HTMLInputElement).value)"
                type="number" step="any" min="0" class="input mt-0.5 text-sm" :placeholder="t('admin.channels.form.pricePlaceholder', '默认')" />
            </div>
            <div>
              <label class="text-xs text-gray-400">{{ t('admin.channels.form.cacheReadPrice', '缓存读取') }}</label>
              <input :value="entry.cache_read_price" @input="emitField('cache_read_price', ($event.target as HTMLInputElement).value)"
                type="number" step="any" min="0" class="input mt-0.5 text-sm" :placeholder="t('admin.channels.form.pricePlaceholder', '默认')" />
            </div>
            <div>
              <label class="text-xs text-gray-400">{{ t('admin.channels.form.imageTokenPrice', '图片输出') }}</label>
              <input :value="entry.image_output_price" @input="emitField('image_output_price', ($event.target as HTMLInputElement).value)"
                type="number" step="any" min="0" class="input mt-0.5 text-sm" :placeholder="t('admin.channels.form.pricePlaceholder', '默认')" />
            </div>
          </div>

          <div class="mt-3 rounded border border-gray-200 p-2 dark:border-dark-600">
            <div class="text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('admin.channels.form.timePricing', '分时与服务档位倍率') }}</div>
            <div class="mt-2 grid grid-cols-1 gap-2 sm:grid-cols-3">
              <div>
                <label class="text-xs text-gray-400">{{ t('admin.channels.form.timezone', '时区') }}</label>
                <input :value="entry.time_pricing?.timezone || ''" @input="updateTimezone(($event.target as HTMLInputElement).value)" class="input mt-0.5 text-sm" placeholder="Asia/Shanghai" />
              </div>
              <div>
                <label class="text-xs text-gray-400">{{ t('admin.channels.form.fastMultiplier', 'Fast 倍率') }}</label>
                <input :value="entry.fast_multiplier" @input="emitField('fast_multiplier', ($event.target as HTMLInputElement).value)" type="number" min="0.0000001" step="any" class="input mt-0.5 text-sm" placeholder="1" />
              </div>
              <div>
                <label class="text-xs text-gray-400">{{ t('admin.channels.form.flexMultiplier', 'Flex 倍率') }}</label>
                <input :value="entry.flex_multiplier" @input="emitField('flex_multiplier', ($event.target as HTMLInputElement).value)" type="number" min="0.0000001" step="any" class="input mt-0.5 text-sm" placeholder="1" />
              </div>
            </div>
            <p class="mt-1 text-xs text-gray-400">{{ t('admin.channels.form.timePricingHint', '时段按左闭右开计算；未命中时倍率为 1。') }}</p>
            <div v-if="entry.time_pricing" class="mt-2 space-y-1.5">
              <div v-for="(period, idx) in entry.time_pricing.periods" :key="idx" class="grid grid-cols-[1fr_1fr_1fr_auto] items-end gap-2">
                <label class="text-xs text-gray-400">开始分钟<input :value="period.start_minute" type="number" min="0" max="1439" class="input mt-0.5 text-sm" @input="updatePeriod(idx, 'start_minute', ($event.target as HTMLInputElement).value)" /></label>
                <label class="text-xs text-gray-400">结束分钟<input :value="period.end_minute" type="number" min="1" max="1440" class="input mt-0.5 text-sm" @input="updatePeriod(idx, 'end_minute', ($event.target as HTMLInputElement).value)" /></label>
                <label class="text-xs text-gray-400">时段倍率<input :value="period.multiplier" type="number" min="0.0000001" step="any" class="input mt-0.5 text-sm" @input="updatePeriod(idx, 'multiplier', ($event.target as HTMLInputElement).value)" /></label>
                <button type="button" class="mb-1 rounded p-1 text-gray-400 hover:text-red-500" @click="removePeriod(idx)"><Icon name="trash" size="sm" /></button>
              </div>
              <button type="button" class="text-xs text-primary-600 hover:text-primary-700" @click="addPeriod">+ 添加时段</button>
            </div>
          </div>

          <!-- Token intervals -->
          <div class="mt-3">
            <div class="flex items-center justify-between">
              <label class="text-xs font-medium text-gray-500 dark:text-gray-400">
                {{ t('admin.channels.form.intervals', '上下文区间定价（可选）') }}
                <span class="ml-1 font-normal text-gray-400">(min, max]</span>
              </label>
              <button type="button" @click="addInterval" class="text-xs text-primary-600 hover:text-primary-700">
                + {{ t('admin.channels.form.addInterval', '添加区间') }}
              </button>
            </div>
            <div v-if="entry.intervals && entry.intervals.length > 0" class="mt-2 space-y-2">
              <IntervalRow
                v-for="(iv, idx) in entry.intervals"
                :key="idx"
                :interval="iv"
                :mode="entry.billing_mode"
                @update="updateInterval(idx, $event)"
                @remove="removeInterval(idx)"
              />
            </div>
          </div>
        </div>

        <!-- Per-request mode -->
        <div v-else-if="entry.billing_mode === 'per_request'">
          <!-- Default per-request price -->
          <label class="mt-3 block text-xs font-medium text-gray-500 dark:text-gray-400">
            {{ t('admin.channels.form.defaultPerRequestPrice', '默认单次价格（未命中层级时使用）') }}
            <span class="ml-1 font-normal text-gray-400">$</span>
          </label>
          <div class="mt-1 w-48">
            <input :value="entry.per_request_price" @input="emitField('per_request_price', ($event.target as HTMLInputElement).value)"
              type="number" step="any" min="0" class="input text-sm" :placeholder="t('admin.channels.form.pricePlaceholder', '默认')" />
          </div>

          <!-- Tiers -->
          <div class="mt-3 flex items-center justify-between">
            <label class="text-xs font-medium text-gray-500 dark:text-gray-400">
              {{ t('admin.channels.form.requestTiers', '按次计费层级') }}
            </label>
            <button type="button" @click="addInterval" class="text-xs text-primary-600 hover:text-primary-700">
              + {{ t('admin.channels.form.addTier', '添加层级') }}
            </button>
          </div>
          <div v-if="entry.intervals && entry.intervals.length > 0" class="mt-2 space-y-2">
            <IntervalRow
              v-for="(iv, idx) in entry.intervals"
              :key="idx"
              :interval="iv"
              :mode="entry.billing_mode"
              @update="updateInterval(idx, $event)"
              @remove="removeInterval(idx)"
            />
          </div>
          <div v-else class="mt-2 rounded border border-dashed border-gray-300 p-3 text-center text-xs text-gray-400 dark:border-dark-500">
            {{ t('admin.channels.form.noTiersYet', '暂无层级，点击添加配置按次计费价格') }}
          </div>
        </div>

        <!-- Image / per-second mode -->
        <div v-else-if="entry.billing_mode === 'image' || entry.billing_mode === 'per_second'">
          <!-- Default image price (per-request, same as per_request mode) -->
          <label class="mt-3 block text-xs font-medium text-gray-500 dark:text-gray-400">
            {{ entry.billing_mode === 'per_second'
              ? t('admin.channels.form.defaultPerSecondPrice', '默认每秒价格')
              : t('admin.channels.form.defaultImagePrice', '默认图片价格（未命中层级时使用）') }}
            <span class="ml-1 font-normal text-gray-400">$</span>
          </label>
          <div class="mt-1 w-48">
            <input :value="entry.per_request_price" @input="emitField('per_request_price', ($event.target as HTMLInputElement).value)"
              type="number" step="any" min="0" class="input text-sm" :placeholder="t('admin.channels.form.pricePlaceholder', '默认')" />
          </div>

          <!-- Image tiers -->
          <div v-if="entry.billing_mode === 'image'" class="mt-3 flex items-center justify-between">
            <label class="text-xs font-medium text-gray-500 dark:text-gray-400">
              {{ t('admin.channels.form.imageTiers', '图片计费层级（按次）') }}
            </label>
            <button type="button" @click="addImageTier" class="text-xs text-primary-600 hover:text-primary-700">
              + {{ t('admin.channels.form.addTier', '添加层级') }}
            </button>
          </div>
          <div v-if="entry.billing_mode === 'image' && entry.intervals && entry.intervals.length > 0" class="mt-2 space-y-2">
            <IntervalRow
              v-for="(iv, idx) in entry.intervals"
              :key="idx"
              :interval="iv"
              :mode="entry.billing_mode"
              @update="updateInterval(idx, $event)"
              @remove="removeInterval(idx)"
            />
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Select from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import IntervalRow from './IntervalRow.vue'
import ModelTagInput from './ModelTagInput.vue'
import type { PricingFormEntry, IntervalFormEntry } from './types'
import { perTokenToMTok, getPlatformTagClass } from './types'
import type { BillingMode } from '@/api/admin/channels'
import channelsAPI from '@/api/admin/channels'

const { t } = useI18n()

const props = defineProps<{
  entry: PricingFormEntry
  platform?: string
}>()

const emit = defineEmits<{
  update: [entry: PricingFormEntry]
  remove: []
}>()

// Collapse state: entries with existing models default to collapsed
const collapsed = ref(props.entry.models.length > 0)

const billingModeOptions = computed(() => [
  { value: 'token', label: 'Token' },
  { value: 'per_request', label: t('admin.channels.billingMode.perRequest', '按次') },
  { value: 'image', label: t('admin.channels.billingMode.image', '图片（按次）') },
  { value: 'per_second', label: t('admin.channels.billingMode.perSecond', '视频（按秒）') }
])

const billingModeLabel = computed(() => {
  const opt = billingModeOptions.value.find(o => o.value === props.entry.billing_mode)
  return opt ? opt.label : props.entry.billing_mode
})

function emitField(field: keyof PricingFormEntry, value: string) {
  emit('update', { ...props.entry, [field]: value === '' ? null : value })
}

function updateTimezone(timezone: string) {
  const current = props.entry.time_pricing
  emit('update', {
    ...props.entry,
    time_pricing: timezone.trim() === '' ? null : {
      timezone,
      periods: current?.periods || [{ start_minute: 0, end_minute: 1440, multiplier: 1 }]
    }
  })
}

function updatePeriod(idx: number, field: 'start_minute' | 'end_minute' | 'multiplier', value: string) {
  if (!props.entry.time_pricing) return
  const periods = props.entry.time_pricing.periods.map((period, i) => i === idx ? { ...period, [field]: Number(value) } : period)
  emit('update', { ...props.entry, time_pricing: { ...props.entry.time_pricing, periods } })
}

function addPeriod() {
  if (!props.entry.time_pricing) return
  emit('update', { ...props.entry, time_pricing: { ...props.entry.time_pricing, periods: [...props.entry.time_pricing.periods, { start_minute: 0, end_minute: 1440, multiplier: 1 }] } })
}

function removePeriod(idx: number) {
  if (!props.entry.time_pricing) return
  emit('update', { ...props.entry, time_pricing: { ...props.entry.time_pricing, periods: props.entry.time_pricing.periods.filter((_, i) => i !== idx) } })
}

function addInterval() {
  const intervals = [...(props.entry.intervals || [])]
  intervals.push({
    min_tokens: 0, max_tokens: null, tier_label: '',
    input_price: null, output_price: null, cache_write_price: null,
    cache_read_price: null, per_request_price: null,
    sort_order: intervals.length
  })
  emit('update', { ...props.entry, intervals })
}

function addImageTier() {
  const intervals = [...(props.entry.intervals || [])]
  const labels = ['1K', '2K', '4K', 'HD']
  intervals.push({
    min_tokens: 0, max_tokens: null, tier_label: labels[intervals.length] || '',
    input_price: null, output_price: null, cache_write_price: null,
    cache_read_price: null, per_request_price: null,
    sort_order: intervals.length
  })
  emit('update', { ...props.entry, intervals })
}

function updateInterval(idx: number, updated: IntervalFormEntry) {
  const intervals = [...(props.entry.intervals || [])]
  intervals[idx] = updated
  emit('update', { ...props.entry, intervals })
}

function removeInterval(idx: number) {
  const intervals = [...(props.entry.intervals || [])]
  intervals.splice(idx, 1)
  emit('update', { ...props.entry, intervals })
}

async function onModelsUpdate(newModels: string[]) {
  const oldModels = props.entry.models
  emit('update', { ...props.entry, models: newModels })

  // 只在新增模型且当前无价格时自动填充
  const addedModels = newModels.filter(m => !oldModels.includes(m))
  if (addedModels.length === 0) return

  // 检查是否所有价格字段都为空
  const e = props.entry
  const hasPrice = e.input_price != null || e.output_price != null ||
                   e.cache_write_price != null || e.cache_read_price != null
  if (hasPrice) return

  // 查询第一个新增模型的默认价格
  try {
    const result = await channelsAPI.getModelDefaultPricing(addedModels[0])
    if (result.found) {
      emit('update', {
        ...props.entry,
        models: newModels,
        input_price: perTokenToMTok(result.input_price ?? null),
        output_price: perTokenToMTok(result.output_price ?? null),
        cache_write_price: perTokenToMTok(result.cache_write_price ?? null),
        cache_read_price: perTokenToMTok(result.cache_read_price ?? null),
        image_output_price: perTokenToMTok(result.image_output_price ?? null),
      })
    }
  } catch {
    // 查询失败不影响用户操作
  }
}
</script>

<style scoped>
.collapsible-content {
  display: grid;
  grid-template-rows: 1fr;
  transition: grid-template-rows 0.25s ease;
}

.collapsible-content--collapsed {
  grid-template-rows: 0fr;
}

.collapsible-inner {
  overflow: hidden;
}
</style>
