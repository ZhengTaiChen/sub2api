<template>
  <section class="card overflow-hidden">
    <div class="flex items-center justify-between gap-4 border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <div class="flex min-w-0 items-center gap-3">
        <div class="rounded-lg bg-cyan-100 p-2 dark:bg-cyan-900/30">
          <Icon name="database" size="md" class="text-cyan-600 dark:text-cyan-400" :stroke-width="2" />
        </div>
        <div class="min-w-0">
          <h2 class="truncate text-lg font-semibold text-gray-900 dark:text-white">{{ t('dashboard.upstreamBilling.title') }}</h2>
          <p class="truncate text-xs text-gray-500 dark:text-dark-400">{{ t('dashboard.upstreamBilling.description') }}</p>
        </div>
      </div>
      <button type="button" class="inline-flex h-9 shrink-0 items-center gap-2 rounded-lg border border-gray-200 px-3 text-sm font-medium text-gray-600 transition-colors hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-60 dark:border-dark-600 dark:text-dark-200 dark:hover:bg-dark-800" :disabled="loading" :title="t('dashboard.upstreamBilling.refresh')" @click="$emit('refresh')">
        <Icon name="refresh" size="sm" :class="loading ? 'animate-spin' : ''" />
        <span class="hidden sm:inline">{{ t('dashboard.upstreamBilling.refresh') }}</span>
      </button>
    </div>

    <div v-if="loading && !accounts.length" class="flex items-center justify-center py-10"><LoadingSpinner /></div>
    <div v-else-if="!accounts.length" class="px-6 py-10 text-center text-sm text-gray-500 dark:text-dark-400">{{ t('dashboard.upstreamBilling.empty') }}</div>
    <div v-else class="overflow-x-auto">
      <table class="w-full min-w-[720px] text-left text-sm">
        <thead class="bg-gray-50 text-xs uppercase text-gray-500 dark:bg-dark-800/60 dark:text-dark-400">
          <tr>
            <th class="px-6 py-3 font-medium">{{ t('dashboard.upstreamBilling.account') }}</th>
            <th class="px-4 py-3 font-medium">{{ t('dashboard.upstreamBilling.provider') }}</th>
            <th class="px-4 py-3 font-medium">{{ t('dashboard.upstreamBilling.rate') }}</th>
            <th class="px-4 py-3 font-medium">{{ t('dashboard.upstreamBilling.balance') }}</th>
            <th class="px-4 py-3 font-medium">{{ t('dashboard.upstreamBilling.status') }}</th>
            <th class="px-6 py-3 text-right font-medium">{{ t('dashboard.upstreamBilling.observedAt') }}</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
          <tr v-for="account in accounts" :key="account.account_id" class="hover:bg-gray-50/70 dark:hover:bg-dark-800/40">
            <td class="max-w-[220px] px-6 py-3"><div class="truncate font-medium text-gray-900 dark:text-white" :title="account.name">{{ account.name }}</div><div class="mt-0.5 text-xs text-gray-400 dark:text-dark-500">{{ account.platform }}</div></td>
            <td class="px-4 py-3"><span class="inline-flex rounded-md bg-gray-100 px-2 py-1 text-xs font-medium text-gray-600 dark:bg-dark-700 dark:text-dark-200">{{ providerLabel(account.provider) }}</span></td>
            <td class="whitespace-nowrap px-4 py-3 font-mono text-gray-700 dark:text-dark-200">{{ formatRate(account.rate_multiplier) }}<span v-if="account.rate_status === 'unsupported'" class="ml-1 text-xs text-gray-400">({{ t('dashboard.upstreamBilling.unsupported') }})</span></td>
            <td class="whitespace-nowrap px-4 py-3"><span :class="balanceClass(account)">{{ formatBalance(account.balance, account.unit) }}</span></td>
            <td class="whitespace-nowrap px-4 py-3"><span class="inline-flex items-center gap-1.5 text-xs font-medium" :class="statusClass(account)"><span class="h-1.5 w-1.5 rounded-full bg-current" />{{ statusLabel(account) }}</span></td>
            <td class="whitespace-nowrap px-6 py-3 text-right text-xs text-gray-500 dark:text-dark-400">{{ formatDate(account.observed_at) }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import type { UserUpstreamBillingAccount } from '@/types'

defineProps<{ accounts: UserUpstreamBillingAccount[]; loading: boolean }>()
defineEmits<{ refresh: [] }>()
const { t } = useI18n()

function providerLabel(provider?: string): string {
  const normalized = provider?.trim().toLowerCase() || 'custom'
  const labels: Record<string, string> = { auto: 'Auto', sub2api: 'Sub2API', new_api: 'new-api', shuai_api: '帅 API', opencode: 'OpenCode', custom: t('dashboard.upstreamBilling.custom') }
  return labels[normalized] || provider || labels.custom
}
function formatRate(rate?: number | null): string { return rate == null || !Number.isFinite(rate) ? t('dashboard.upstreamBilling.unknown') : rate.toFixed(2) + 'x' }
function formatBalance(balance?: number | null, unit?: string): string {
  if (balance == null || !Number.isFinite(balance)) return t('dashboard.upstreamBilling.unknown')
  return new Intl.NumberFormat(undefined, { maximumFractionDigits: 4 }).format(balance) + (unit ? ' ' + unit : '')
}
function formatDate(value?: string): string {
  if (!value) return t('dashboard.upstreamBilling.unknown')
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString(undefined, { dateStyle: 'short', timeStyle: 'short' })
}
function hasBalance(account: UserUpstreamBillingAccount): boolean { return account.balance != null && Number.isFinite(account.balance) }
function isLowBalance(account: UserUpstreamBillingAccount): boolean { return hasBalance(account) && account.balance! <= 0 }
function statusLabel(account: UserUpstreamBillingAccount): string {
  const expired = account.balance_status === 'failed' || account.balance_status === 'unsupported' || Boolean(account.balance_error)
  if (expired) return hasBalance(account) ? t('dashboard.upstreamBilling.expired') : t('dashboard.upstreamBilling.unknown')
  if (isLowBalance(account)) return t('dashboard.upstreamBilling.lowBalance')
  if (account.balance_status === 'unknown' || !hasBalance(account)) return t('dashboard.upstreamBilling.unknown')
  return t('dashboard.upstreamBilling.ok')
}
function statusClass(account: UserUpstreamBillingAccount): string {
  if (isLowBalance(account) || account.balance_status === 'failed' || account.balance_status === 'unsupported' || Boolean(account.balance_error)) return 'text-amber-600 dark:text-amber-400'
  if (account.balance_status === 'unknown' || !hasBalance(account)) return 'text-gray-500 dark:text-dark-400'
  return 'text-emerald-600 dark:text-emerald-400'
}
function balanceClass(account: UserUpstreamBillingAccount): string {
  if (isLowBalance(account)) return 'font-semibold text-amber-600 dark:text-amber-400'
  if (!hasBalance(account)) return 'text-gray-500 dark:text-dark-400'
  return 'font-medium text-gray-900 dark:text-white'
}
</script>
