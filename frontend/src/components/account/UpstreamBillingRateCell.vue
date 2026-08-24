<template>
  <div v-if="eligible" class="flex h-6 min-w-[7rem] items-center gap-1">
    <HelpTooltip class="-ml-1" width-class="w-max max-w-[calc(100vw-2rem)]" data-testid="upstream-billing-details">
      <template #trigger>
        <span
          class="cursor-help border-b border-dotted border-gray-300 text-sm font-medium dark:border-dark-600"
          :class="hasEffectiveRate ? 'font-mono text-gray-800 dark:text-gray-200' : statusClass || 'text-gray-400 dark:text-gray-500'"
          data-testid="upstream-billing-rate"
        >
          {{ primaryValue }}
        </span>
      </template>
      <div class="space-y-1">
        <template v-if="data">
          <template v-if="hasEffectiveRate">
            <p>{{ t('admin.accounts.upstreamBilling.groupRate', { value: data.group_rate_multiplier }) }}</p>
            <p v-if="data.user_rate_multiplier != null">
              {{ t('admin.accounts.upstreamBilling.userRate', { value: data.user_rate_multiplier }) }}
            </p>
            <p>
              {{
                data.peak_rate_enabled
                  ? t('admin.accounts.upstreamBilling.peakRate', {
                      start: data.peak_start,
                      end: data.peak_end,
                      value: data.peak_rate_multiplier,
                      timezone: data.timezone
                    })
                  : t('admin.accounts.upstreamBilling.noPeakRate')
              }}
            </p>
            <p>{{ t('admin.accounts.upstreamBilling.effectiveRate', { value: currentEffectiveRate ?? '-' }) }}</p>
          </template>
          <template v-else-if="stale && lastDetectedRate != null">
            <p data-testid="upstream-billing-last-rate">
              {{ t('admin.accounts.upstreamBilling.lastDetectedRate', { value: lastDetectedRate }) }}
            </p>
            <p data-testid="upstream-billing-last-time">
              {{ t('admin.accounts.upstreamBilling.lastDetectedAt', { value: formatDate(snapshot?.received_at) }) }}
            </p>
            <p data-testid="upstream-billing-elapsed">
              {{ t('admin.accounts.upstreamBilling.elapsedSince', { value: elapsedSinceLastSuccess }) }}
            </p>
          </template>
          <p>{{ t('admin.accounts.upstreamBilling.updatedAt', { value: formatDate(snapshot?.received_at) }) }}</p>
          <p v-if="providerLabel" data-testid="upstream-billing-provider">
            {{ t('admin.accounts.upstreamBilling.provider', { value: providerLabel }) }}
          </p>
          <p v-if="sourceLabel" data-testid="upstream-billing-source">
            {{ t('admin.accounts.upstreamBilling.rateSource', { value: sourceLabel }) }}
          </p>
          <p data-testid="upstream-billing-balance" :class="{ 'text-red-500 dark:text-red-400': balanceLow }">
            {{ t('admin.accounts.upstreamBilling.balance', { value: balanceText }) }}
            <span v-if="balanceStatusLabel">（{{ balanceStatusLabel }}）</span>
          </p>
        </template>
        <template v-else-if="stale && lastDetectedRate != null">
          <p data-testid="upstream-billing-last-rate">
            {{ t('admin.accounts.upstreamBilling.lastDetectedRate', { value: lastDetectedRate }) }}
          </p>
          <p data-testid="upstream-billing-last-time">
            {{ t('admin.accounts.upstreamBilling.lastDetectedAt', { value: formatDate(snapshot?.received_at) }) }}
          </p>
          <p data-testid="upstream-billing-elapsed">
            {{ t('admin.accounts.upstreamBilling.elapsedSince', { value: elapsedSinceLastSuccess }) }}
          </p>
        </template>
        <p v-else>{{ statusLabel || '-' }}</p>
        <p
          v-if="probeEnabled && globalProbeEnabled !== false && nextProbeAt"
          data-testid="upstream-billing-next-probe"
        >
          {{ t('admin.accounts.upstreamBilling.nextProbeAt', { value: formatDate(nextProbeAt) }) }}
        </p>
        <p class="mt-2 border-t border-white/15 pt-2" data-testid="upstream-billing-probe-state">
          {{ t('admin.accounts.upstreamBilling.accountProbeState') }}
          <span :class="probeEnabled ? 'text-emerald-400' : 'text-red-400'">
            {{ probeEnabled ? t('admin.accounts.upstreamBilling.enabled') : t('admin.accounts.upstreamBilling.disabled') }}
          </span>
        </p>
        <p
          v-if="globalProbeEnabled === false"
          class="mt-1"
          data-testid="upstream-billing-global-probe-state"
        >
          {{ t('admin.accounts.upstreamBilling.globalProbeState') }}
          <span class="text-red-400">{{ t('admin.accounts.upstreamBilling.disabled') }}</span>
        </p>
      </div>
    </HelpTooltip>
    <span v-if="hasEffectiveRate && statusLabel" :class="statusClass" class="whitespace-nowrap text-[10px] font-medium">
      {{ statusLabel }}
    </span>
    <button
      type="button"
      class="inline-flex h-6 w-6 flex-shrink-0 items-center justify-center rounded text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
      :disabled="probing"
      :aria-label="t('admin.accounts.upstreamBilling.manualProbe')"
      :title="t('admin.accounts.upstreamBilling.manualProbe')"
      data-testid="upstream-billing-probe"
      @click="$emit('probe')"
    >
      <Icon name="refresh" size="xs" :class="{ 'animate-spin': probing }" />
    </button>
  </div>
  <span v-else class="text-sm text-gray-400 dark:text-dark-500">-</span>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import Icon from '@/components/icons/Icon.vue'
import { formatMultiplier } from '@/utils/formatters'
import type { Account, UpstreamBillingProbeSnapshot } from '@/types'

const props = withDefaults(defineProps<{
  account: Account
  now: number
  probing?: boolean
  globalProbeEnabled?: boolean
}>(), {
  globalProbeEnabled: true
})

defineEmits<{
  (event: 'probe'): void
}>()

const { t } = useI18n()
const CLOCK_SKEW_TOLERANCE_MS = 5 * 60 * 1000
// 探测资格已放宽到全部 API-key 平台（上游是 sub2api 即可应答）。
const eligible = computed(() => props.account.type === 'apikey')
const snapshot = computed<UpstreamBillingProbeSnapshot | undefined>(() => props.account.extra?.upstream_billing_probe)
const data = computed(() => snapshot.value?.data)
const probeEnabled = computed(() => props.account.extra?.upstream_billing_probe_enabled === true)
const nextProbeAt = computed(() => {
  const value = snapshot.value?.next_probe_at
  return typeof value === 'string' && Number.isFinite(Date.parse(value)) ? value : ''
})
const receivedAt = computed(() => typeof snapshot.value?.received_at === 'string' ? Date.parse(snapshot.value.received_at) : Number.NaN)
const freshUntil = computed(() => {
  if (typeof snapshot.value?.fresh_until === 'string') return Date.parse(snapshot.value.fresh_until)
  if (snapshot.value?.status !== 'ok' || typeof snapshot.value.next_probe_at !== 'string') return Number.NaN
  const nextProbeAt = Date.parse(snapshot.value.next_probe_at)
  return Number.isFinite(nextProbeAt) && nextProbeAt > receivedAt.value
    ? receivedAt.value + 2 * (nextProbeAt - receivedAt.value)
    : Number.NaN
})
const validTimestamps = computed(() => {
  if (!Number.isFinite(receivedAt.value) || receivedAt.value > props.now + CLOCK_SKEW_TOLERANCE_MS) return false
  return Number.isFinite(freshUntil.value) && freshUntil.value > receivedAt.value
})
const stale = computed(() => {
  if (!snapshot.value) return false
  if (!Number.isFinite(receivedAt.value)) return snapshot.value.status === 'ok'
  if (!validTimestamps.value) return true
  return props.now > freshUntil.value
})
const parseMinute = (value?: string) => {
  if (typeof value !== 'string') return null
  const match = /^(\d{2}):(\d{2})$/.exec(value)
  if (!match) return null
  const hour = Number(match[1])
  const minute = Number(match[2])
  return hour < 24 && minute < 60 ? hour * 60 + minute : null
}
const minuteInTimeZone = (timestamp: number, timeZone?: string) => {
  if (!timeZone) return null
  try {
    const parts = new Intl.DateTimeFormat('en-GB', {
      timeZone,
      hour: '2-digit',
      minute: '2-digit',
      hourCycle: 'h23'
    }).formatToParts(new Date(timestamp))
    const hour = Number(parts.find(part => part.type === 'hour')?.value)
    const minute = Number(parts.find(part => part.type === 'minute')?.value)
    return Number.isInteger(hour) && Number.isInteger(minute) ? hour * 60 + minute : null
  } catch {
    return null
  }
}
const currentEffectiveRate = computed(() => {
  const billing = data.value
  if (!billing) return null
  if (billing.billing_scope == null) {
    const fallback = billing.effective_rate_multiplier
    return typeof fallback === 'number' && Number.isFinite(fallback) && fallback >= 0 ? fallback : null
  }
  if (billing.billing_scope !== 'token') return null
  const base = billing.resolved_rate_multiplier
  if (typeof base !== 'number' || !Number.isFinite(base) || base < 0) return null
  if (typeof billing.peak_rate_enabled !== 'boolean') return null
  if (!billing.peak_rate_enabled) return base
  const start = parseMinute(billing.peak_start)
  const end = parseMinute(billing.peak_end)
  const minute = minuteInTimeZone(props.now, billing.timezone)
  const peak = billing.peak_rate_multiplier
  if (start == null || end == null || minute == null || start >= end || typeof peak !== 'number' || !Number.isFinite(peak) || peak < 0) return null
  const value = minute >= start && minute < end ? base * peak : base
  return Number.isFinite(value) ? value : null
})
const lastDetectedRate = computed(() => {
  const value = data.value?.effective_rate_multiplier
  return typeof value === 'number' && Number.isFinite(value) && value >= 0
    ? Number(value.toPrecision(12))
    : null
})
const sourceLabel = computed(() => {
  const billing = data.value
  if (!billing) return ''
  const source = billing.upstream_rate_source ?? billing.rate_source ?? billing.billing_provider
  if (source === 'shuai_api_usage' || source === 'shuai_api') return t('admin.accounts.upstreamBilling.shuaiApiUsage')
  if (source === 'sub2api_billing') return t('admin.accounts.upstreamBilling.sub2apiBilling')
  return ''
})
const providerLabel = computed(() => {
  const provider = snapshot.value?.provider ?? data.value?.provider
  if (!provider || provider === 'auto') return ''
  const labels: Record<string, string> = {
    sub2api: 'Sub2API',
    new_api: 'new-api',
    shuai_api: '帅 API',
    opencode: 'OpenCode',
    custom: 'Custom'
  }
  return labels[provider] || provider
})
const balanceValue = computed(() => {
  const billing = data.value
  if (!billing) return null
  const raw = billing.balance ?? billing.upstream_balance
  return typeof raw === 'number' && Number.isFinite(raw) ? raw : null
})
const balanceUnit = computed(() => data.value?.unit || data.value?.upstream_balance_unit || '')
const balanceText = computed(() => {
  if (balanceValue.value == null) return '-'
  const value = balanceValue.value.toLocaleString(undefined, { maximumFractionDigits: 4 })
  return balanceUnit.value ? `${value} ${balanceUnit.value}` : value
})
const balanceLow = computed(() => balanceValue.value != null && balanceValue.value <= 0)
const balanceError = computed(() => data.value?.balance_error || data.value?.upstream_balance_error || '')
const balanceExpired = computed(() => {
  const status = snapshot.value?.balance_status ?? data.value?.balance_status
  return status === 'failed' || status === 'unsupported' || Boolean(balanceError.value)
})
const balanceStatusLabel = computed(() => {
  if (balanceExpired.value && balanceValue.value != null) {
    return t('admin.accounts.upstreamBilling.balanceExpired')
  }
  if (balanceValue.value != null && balanceValue.value <= 0) return t('admin.accounts.upstreamBilling.balanceLow')
  const status = snapshot.value?.balance_status ?? data.value?.balance_status
  if (status === 'failed' || status === 'unsupported' || status === 'unknown' || balanceValue.value == null) {
    return t('admin.accounts.upstreamBilling.balanceUnknown')
  }
  return ''
})
const elapsedSinceLastSuccess = computed(() => {
  if (!Number.isFinite(receivedAt.value)) return '-'
  const elapsedMinutes = Math.max(0, Math.floor((props.now - receivedAt.value) / 60_000))
  if (elapsedMinutes < 1) return t('admin.accounts.upstreamBilling.justNow')
  if (elapsedMinutes < 60) return t('admin.accounts.upstreamBilling.minutesAgo', { count: elapsedMinutes })
  const elapsedHours = Math.floor(elapsedMinutes / 60)
  if (elapsedHours < 24) return t('admin.accounts.upstreamBilling.hoursAgo', { count: elapsedHours })
  return t('admin.accounts.upstreamBilling.daysAgo', { count: Math.floor(elapsedHours / 24) })
})
const effectiveRate = computed(() => {
  if (!validTimestamps.value || stale.value || !['ok', 'failed'].includes(snapshot.value?.status ?? '')) return '-'
  const value = currentEffectiveRate.value
  return value == null ? '-' : `${formatMultiplier(value)}x`
})
const statusLabel = computed(() => {
  if (!snapshot.value) return t('admin.accounts.upstreamBilling.notProbed')
  if (snapshot.value.status === 'unsupported') return t('admin.accounts.upstreamBilling.unsupported')
  if (stale.value) return t('admin.accounts.upstreamBilling.stale')
  if (snapshot.value.status === 'failed') return t('admin.accounts.upstreamBilling.failed')
  return ''
})
const statusClass = computed(() => {
  if (!snapshot.value) return 'text-gray-400 dark:text-gray-500'
  if (snapshot.value.status === 'unsupported') return 'text-gray-500 dark:text-gray-400'
  if (stale.value) return 'text-amber-600 dark:text-amber-400'
  if (snapshot.value.status === 'failed') return 'text-red-600 dark:text-red-400'
  return ''
})
const hasEffectiveRate = computed(() => effectiveRate.value !== '-')
const primaryValue = computed(() => hasEffectiveRate.value ? effectiveRate.value : statusLabel.value || '-')
const formatDate = (value?: string) => value
  ? new Date(value).toLocaleString(undefined, {
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit'
    })
  : '-'
</script>
