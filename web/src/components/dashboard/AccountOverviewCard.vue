<script setup lang="ts">
import { computed } from 'vue'
import { CircleCheck, Gauge, ShieldAlert, Users } from 'lucide-vue-next'
import type { Account, Overview } from '../../api/types'
import { metric } from '../../api/stats-format'
import { formatRelative } from '../../stores/ui'
import { useI18n } from '../../i18n'
import { accountStatusLabel, strategyDescription, strategyLabel } from '../../utils/uiOptions'
import Badge from '../Badge.vue'
import { quotaLimited } from '../accounts/accountPresentation'

const props = defineProps<{ accounts: Account[]; overview: Overview | null }>()
const { t } = useI18n()
const normal = computed(() => props.accounts.filter(account => account.enabled && account.status === 'ready' && !quotaLimited(account)).length)
const limited = computed(() => props.accounts.filter(account => account.enabled && quotaLimited(account)).length)
const disabled = computed(() => props.accounts.filter(account => !account.enabled).length)
const error = computed(() => props.accounts.filter(account => account.enabled && account.status !== 'ready' && !quotaLimited(account)).length)
const normalRate = computed(() => props.accounts.length ? `${Math.round(normal.value / props.accounts.length * 100)}%` : '—')
const statusBars = computed(() => {
	const total = props.accounts.length
	if (!total) { return [] }
	return [{ value: normal.value, tone: 'success' }, { value: limited.value, tone: 'warn' }, { value: disabled.value + error.value, tone: 'danger' }].filter(item => item.value > 0).map(item => ({ ...item, width: `${item.value / total * 100}%` }))
})
const activeAccounts = computed(() => [...props.accounts].filter(account => account.last_used_at > 0).sort((a, b) => b.last_used_at - a.last_used_at).slice(0, 4))
const strategy = computed(() => props.overview?.settings.default_strategy ?? '')
const maxConcurrency = computed(() => props.overview?.settings.max_concurrent_per_account)
// Configured capacity is deliberately not advertised as scheduler availability:
// the public account payload does not expose cooldown/model eligibility.
const totalSlots = computed(() => props.accounts.reduce((total, account) => total + (account.concurrency > 0 ? account.concurrency : maxConcurrency.value ?? 0), 0))
const usedSlots = computed(() => props.accounts.reduce((total, account) => total + account.inflight, 0))
const slotsRatio = computed(() => totalSlots.value ? Math.min(100, usedSlots.value / totalSlots.value * 100) : 0)
function accountValue(account: Account) { return metric(account.request_count) }
function statusTone(account: Account) { return (() => {
    if (quotaLimited(account)) {
        return 'warn'
    }
    if (account.enabled && account.status === 'ready') {
        return 'success'
    }
    return 'danger'
})() }
</script>

<template>
	<article class="card account-overview-card">
		<section class="schedule-column">
			<h2>{{ t('dashboard.scheduling') }}</h2>
			<div class="schedule-block"><span>{{ t('dashboard.slotUsage') }}</span><strong>{{ usedSlots }} / {{ totalSlots }}</strong><div class="capacity-track"><i :style="{ width: `${slotsRatio}%` }" /></div></div>
			<div class="schedule-grid"><div><span>{{ t('dashboard.defaultConcurrency') }}</span><strong>{{ metric(maxConcurrency) }}</strong></div><div><span>{{ t('dashboard.totalSlots') }}</span><strong>{{ metric(totalSlots) }}</strong></div><div><span>{{ t('dashboard.accounts') }}</span><strong>{{ accounts.length }}</strong></div></div>
			<div class="schedule-block strategy"><span>{{ t('dashboard.strategy') }}</span><strong>{{ strategyLabel(strategy) }}</strong><p>{{ strategyDescription(strategy) }}</p></div>
		</section>
		<section class="active-column"><h2>{{ t('dashboard.activeAccounts') }}</h2><p>{{ t('dashboard.recent') }}</p><div class="active-list"><div v-if="!activeAccounts.length" class="active-empty">{{ t('dashboard.noAccountUsage') }}</div><div v-for="account in activeAccounts" :key="account.id" class="active-row"><div class="active-identity"><span class="avatar">{{ (account.name || account.email || 'A').slice(0, 1).toUpperCase() }}</span><span><strong>{{ account.name || account.email || `#${account.id}` }}</strong><small>{{ account.email || account.account_id || '—' }}</small></span></div><span class="active-metric"><small>{{ t('accounts.col.requests') }}</small><strong>{{ accountValue(account) }}</strong></span><span class="active-last"><small>{{ t('dashboard.recent') }}</small><span>{{ formatRelative(account.last_used_at) }}</span></span><Badge :tone="statusTone(account)">{{ !account.enabled ? t('common.disabled') : accountStatusLabel(account.status) }}</Badge></div></div></section>
		<section class="status-column"><header><div><h2>{{ t('dashboard.accountStatus') }}</h2><p>{{ t('accounts.overview.pool') }}</p></div><div class="normal-rate"><strong>{{ normalRate }}</strong><span>{{ t('dashboard.normalRate') }}</span></div></header><div class="status-track"><i v-for="item in statusBars" :key="item.tone" :class="`bar-${item.tone}`" :style="{ width: item.width }" /></div><div class="status-legend"><div><span><CircleCheck :size="14" />{{ t('dashboard.normal') }}</span><strong>{{ normal }}</strong></div><div><span><Gauge :size="14" />{{ t('dashboard.quotaLimited') }}</span><strong>{{ limited }}</strong></div><div><span><ShieldAlert :size="14" />{{ t('dashboard.pending') }}</span><strong>{{ disabled + error }}</strong></div></div><div class="status-note"><Users :size="15" /><span>{{ t('dashboard.keys') }}: <b>{{ metric(overview?.keys_enabled) }} / {{ metric(overview?.keys) }}</b></span></div></section>
	</article>
</template>

<style scoped>
.account-overview-card { display: grid; grid-template-columns: minmax(0, .9fr) minmax(0, 1.28fr) minmax(280px, .9fr); gap: 28px; padding: 24px; }
.account-overview-card h2 { margin: 0; font-size: 20px; line-height: 1.15; font-weight: 750; }
.account-overview-card p { margin: 6px 0 0; color: var(--color-ink-muted); font-size: 12px; line-height: 1.15; }
.schedule-column, .active-column, .status-column { min-width: 0; }
.schedule-column { display: grid; align-content: start; gap: 16px; }
.schedule-block, .schedule-grid, .active-row, .status-note { border-radius: 10px; background: var(--color-surface-2); }
.schedule-block { display: grid; gap: 14px; padding: 16px; }
.schedule-block > span, .schedule-grid span, .active-row small { color: var(--color-ink-faint); font-size: 10px; }
.schedule-block > strong { font-family: var(--font-mono); font-size: 28px; line-height: 1; font-weight: 750; font-variant-numeric: tabular-nums; }
.capacity-track, .status-track { height: 9px; overflow: hidden; border-radius: 999px; background: var(--color-surface-3); }
.capacity-track i { display: block; height: 100%; border-radius: inherit; background: var(--color-success); }
.schedule-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 12px; padding: 16px; }
.schedule-grid div { display: grid; align-content: space-between; gap: 12px; }
.schedule-grid strong { font-family: var(--font-mono); font-size: 16px; line-height: 1; font-weight: 750; }
.strategy { min-height: 70px; }
.strategy strong { font-family: inherit; font-size: 15px; line-height: 1.4; overflow-wrap: anywhere; }
.strategy p { margin: 0; font-size: 11px; line-height: 1.5; overflow-wrap: anywhere; }
.active-column { display: grid; grid-template-rows: auto auto 1fr; }
.active-list { display: grid; align-content: start; gap: 10px; margin-top: 20px; }
.active-row { min-height: 72px; display: grid; grid-template-columns: minmax(0, 1.3fr) minmax(60px, .5fr) minmax(72px, .7fr) auto; align-items: center; gap: 10px; padding: 0 14px; }
.active-identity { min-width: 0; display: flex; align-items: center; gap: 9px; }
.avatar { width: 28px; height: 28px; flex-shrink: 0; display: inline-flex; align-items: center; justify-content: center; border-radius: 7px; color: var(--color-info); background: var(--color-info-soft); font-size: 12px; font-weight: 800; }
.active-identity span:last-child, .active-metric, .active-last { min-width: 0; display: grid; gap: 5px; }
.active-identity strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 12px; }
.active-identity small, .active-last span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--color-ink-muted); font-size: 10px; }
.active-metric strong { font-family: var(--font-mono); font-size: 12px; font-weight: 750; }
.active-last span { font-weight: 650; }
.active-empty { display: grid; min-height: 180px; place-content: center; border-radius: 10px; background: var(--color-surface-2); color: var(--color-ink-faint); font-size: 12px; }
.status-column { display: grid; align-content: start; gap: 20px; }
.status-column header { display: flex; align-items: start; justify-content: space-between; gap: 12px; }
.normal-rate { display: grid; justify-items: end; gap: 3px; }
.normal-rate strong { color: var(--color-success); font-family: var(--font-mono); font-size: 24px; line-height: 1; }
.normal-rate span { color: var(--color-ink-muted); font-size: 10px; font-weight: 650; }
.status-track { display: flex; }
.status-track i { display: block; height: 100%; }
.bar-success { background: var(--color-success); }.bar-warn { background: var(--color-warn); }.bar-danger { background: var(--color-danger); }
.status-legend { display: grid; gap: 13px; }
.status-legend div { display: flex; justify-content: space-between; gap: 10px; }
.status-legend span { display: flex; align-items: center; gap: 7px; color: var(--color-ink-muted); font-size: 12px; }
.status-legend strong { font-family: var(--font-mono); font-size: 13px; font-weight: 750; }
.status-note { display: flex; align-items: center; gap: 8px; padding: 12px; color: var(--color-ink-muted); font-size: 11px; }
.status-note svg { color: var(--color-info); }
.status-note b { color: var(--color-ink); font-family: var(--font-mono); }
@media (max-width: 1199px) { .account-overview-card { grid-template-columns: 1fr 1.2fr; } .status-column { grid-column: 1 / -1; } }
@media (max-width: 767px) { .account-overview-card { grid-template-columns: 1fr; padding: 20px 16px; } .status-column { grid-column: auto; } .active-row { grid-template-columns: minmax(0, 1fr) auto; min-height: auto; gap: 12px; padding: 12px; } .active-metric, .active-last { display: none; } }
</style>
