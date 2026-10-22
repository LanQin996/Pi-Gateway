<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { Activity, FileText, RefreshCw, Timer, Users } from 'lucide-vue-next'
import { api } from '../api/client'
import type { Account, Overview, StatsBucket, StatsSummary, UsageRecord } from '../api/types'
import { metric, percent } from '../api/stats-format'
import { useI18n } from '../i18n'
import { useToastStore, formatDuration, formatRelative } from '../stores/ui'
import PageHeader from '../components/PageHeader.vue'
import Badge from '../components/Badge.vue'
import MetricCard from '../components/dashboard/MetricCard.vue'
import RequestTrendCard from '../components/dashboard/RequestTrendCard.vue'
import WireProfileCard from '../components/dashboard/WireProfileCard.vue'
import AccountOverviewCard from '../components/dashboard/AccountOverviewCard.vue'
import UsageRecordCard from '../components/dashboard/UsageRecordCard.vue'

const { t } = useI18n()
const toast = useToastStore()
const overview = ref<Overview | null>(null)
const accounts = ref<Account[]>([])
const summary = ref<StatsSummary | null>(null)
const buckets = ref<StatsBucket[]>([])
const records = ref<UsageRecord[]>([])
const loading = ref(true)
const refreshing = ref(false)
const loadError = ref('')
const statsError = ref('')
const refreshedAt = ref(0)
const accountsLoaded = ref(false)
const usageError = ref('')
let generation = 0

function todayRange() {
	const end = Date.now()
	const startDate = new Date(end)
	startDate.setHours(0, 0, 0, 0)
	return { start: startDate.getTime(), end }
}

async function load(isRefresh = false) {
	const run = ++generation
	if (isRefresh) { refreshing.value = true }
	else { loading.value = true }
	loadError.value = ''; statsError.value = ''; usageError.value = ''
	const { start, end } = todayRange()
	const results = await Promise.allSettled([
		api.overview(), api.listAccounts(), api.statsSummary(start, end),
		api.statsTrend(start, end, 'hour'),
		api.usageRecords({ start, end, limit: 10, offset: 0, outcome: 'succeeded' }),
	])
	if (run !== generation) { return }
	const [nextOverview, accountResult, nextSummary, trend, usage] = results
	const errorText = (reason: unknown) => reason instanceof Error ? reason.message : t('stats.loadFailed')
	if (nextOverview.status === 'fulfilled') { overview.value = nextOverview.value }
	if (accountResult.status === 'fulfilled') { accounts.value = accountResult.value.accounts ?? []; accountsLoaded.value = true }
	if (nextSummary.status === 'fulfilled') { summary.value = nextSummary.value }
	else { summary.value = null }
	if (trend.status === 'fulfilled') { buckets.value = [...(trend.value.buckets ?? [])].sort((a, b) => a.bucket_start - b.bucket_start) }
	else { buckets.value = []; statsError.value = errorText(trend.reason) }
	if (usage.status === 'fulfilled') { records.value = usage.value.records ?? [] }
	else { records.value = []; usageError.value = errorText(usage.reason) }
	const errors = results.filter((result): result is PromiseRejectedResult => result.status === 'rejected').map(result => errorText(result.reason))
	loadError.value = [...new Set(errors)].join(' · ')
	if (loadError.value) { toast.error(loadError.value) }
	else { refreshedAt.value = Date.now() }
	loading.value = false
	refreshing.value = false
}

onMounted(() => void load())
onBeforeUnmount(() => { generation++ })

const trendRequests = computed(() => buckets.value.map(bucket => bucket.request_count))
const trendTokens = computed(() => buckets.value.map(bucket => bucket.total_tokens))
const trendCache = computed(() => buckets.value.map(bucket => bucket.input_tokens != null && bucket.input_tokens > 0 && bucket.cached_tokens != null ? bucket.cached_tokens / bucket.input_tokens * 100 : null))
const cards = computed(() => {
	const s = summary.value
	return [
		{ label: t('dashboard.accounts'), value: overview.value ? metric(overview.value.accounts.total) : '—', firstLabel: t('common.enabled'), firstValue: overview.value ? metric(overview.value.accounts.enabled) : '—', secondLabel: t('common.disabled'), secondValue: overview.value ? metric(Math.max(0, overview.value.accounts.total - overview.value.accounts.enabled)) : '—', icon: Users, tone: 'info' as const, sparkline: [] },
		{ label: t('stats.requests'), value: metric(s?.request_count), firstLabel: t('dashboard.success'), firstValue: metric(s?.success_count), secondLabel: t('dashboard.failed'), secondValue: metric(s?.failure_count), icon: Activity, tone: 'info' as const, sparkline: trendRequests.value },
		{ label: t('stats.totalTokens'), value: metric(s?.total_tokens), firstLabel: t('dashboard.input'), firstValue: metric(s?.input_tokens), secondLabel: t('dashboard.output'), secondValue: metric(s?.output_tokens), icon: FileText, tone: 'success' as const, sparkline: trendTokens.value },
		{ label: t('stats.cacheRate'), value: percent(s?.cached_token_rate), firstLabel: t('dashboard.cached'), firstValue: metric(s?.cached_tokens), secondLabel: t('stats.cacheHitRate'), secondValue: percent(s?.cache_hit_request_rate), icon: Timer, tone: 'warn' as const, sparkline: trendCache.value },
	]
})
const lastRefreshed = computed(() => refreshedAt.value ? formatRelative(refreshedAt.value) : '—')
const successRate = computed(() => (() => {
    if (summary.value) {
        return percent(summary.value.request_count ? summary.value.success_count / summary.value.request_count : null)
    }
    return '—'
})())
</script>

<template>
	<div class="page-view dashboard-view">
		<PageHeader :title="t('dashboard.title')" :subtitle="`${t('dashboard.today')}${refreshedAt ? ` · ${t('dashboard.updated', { time: lastRefreshed })}` : ''}`">
			<template #actions><button class="btn btn-ghost refresh-button" :disabled="loading || refreshing" :aria-label="t('dashboard.refresh')" :title="t('dashboard.refresh')" @click="load(true)"><RefreshCw :size="19" :class="{ 'animate-spin': loading || refreshing }" /></button></template>
		</PageHeader>
		<main class="page-content dashboard-content" :aria-busy="loading || refreshing">
			<div v-if="loadError" class="notice notice-error" role="alert">{{ loadError }}</div>
			<section class="metric-grid" :aria-label="t('dashboard.title')"><MetricCard v-for="card in cards" :key="card.label" v-bind="card" /></section>
			<section class="dashboard-two-column"><RequestTrendCard :buckets="buckets" :summary="summary" :loading="loading" :error="statsError" /><WireProfileCard :overview="overview" :error="loadError" /></section>
			<AccountOverviewCard v-if="accountsLoaded" :accounts="accounts" :overview="overview" />
			<output v-else class="card empty-state" >{{ loading ? t('stats.loading') : loadError }}</output>
			<section class="result-card card"><header><div><h2>{{ t('dashboard.requestsToday') }}</h2><p>{{ t('stats.successRate') }} <strong>{{ successRate }}</strong></p></div><Badge tone="info">{{ metric(summary?.request_count) }}</Badge></header><div class="result-grid"><div><span>{{ t('dashboard.success') }}</span><strong class="tone-success">{{ metric(summary?.success_count) }}</strong></div><div><span>{{ t('dashboard.failed') }}</span><strong class="tone-danger">{{ metric(summary?.failure_count) }}</strong></div><div><span>{{ t('dashboard.cancelled') }}</span><strong>{{ metric(summary?.cancelled_count) }}</strong></div><div><span>{{ t('dashboard.incomplete') }}</span><strong>{{ metric(summary?.incomplete_count) }}</strong></div><div><span>{{ t('stats.latency') }}</span><strong>{{ summary?.avg_latency_ms == null ? '—' : formatDuration(summary.avg_latency_ms) }}</strong></div></div></section>
			<UsageRecordCard :rows="records" :loading="loading" :error="usageError" />
			<section v-if="overview?.recent_captures?.length" class="card recent-card"><header class="card-heading"><div><h2>{{ t('dashboard.recentExchanges') }}</h2></div><RouterLink to="/captures" class="btn btn-ghost">{{ t('dashboard.viewAll') }}</RouterLink></header><div class="table-scroll"><table class="w-full"><thead><tr><th class="th">ID</th><th class="th">{{ t('accounts.col.account') }}</th><th class="th">{{ t('settings.defaultModel') }}</th><th class="th">{{ t('captures.filter.transport') }}</th><th class="th">{{ t('captures.filter.outcome') }}</th><th class="th text-right">{{ t('capture.duration') }}</th></tr></thead><tbody><tr v-for="capture in overview.recent_captures.slice(0, 6)" :key="capture.id" class="row-hover"><td class="td font-mono"><RouterLink :to="`/captures/${capture.id}`" class="capture-link">#{{ capture.id }}</RouterLink></td><td class="td">{{ capture.account_name || '—' }}</td><td class="td font-mono text-[12px]">{{ capture.model || '—' }}</td><td class="td"><Badge tone="info">{{ capture.client_transport }}→{{ capture.upstream_transport }}</Badge></td><td class="td"><Badge :tone="capture.outcome === 'ok' ? 'success' : 'danger'">{{ capture.outcome }}</Badge></td><td class="td text-right font-mono text-[12px]">{{ formatDuration(capture.duration_ms) }}</td></tr></tbody></table></div></section>
			<details v-if="overview?.audit?.length" class="card audit-card"><summary>{{ t('dashboard.adminActivity') }}</summary><ul><li v-for="event in overview.audit" :key="event.id"><span>{{ event.action }}<small v-if="event.detail">{{ event.detail }}</small></span><time>{{ formatRelative(event.created_at) }}</time></li></ul></details>
		</main>
	</div>
</template>

<style scoped>
.dashboard-content { display: grid; gap: 24px; }
.metric-grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 16px; }
.dashboard-two-column { display: grid; grid-template-columns: minmax(0, 948fr) minmax(0, 608fr); gap: 24px; }
.refresh-label { color: var(--color-ink-faint); font-size: 11px; }
.refresh-button { width: 36px; padding: 0; }
.result-card { padding: 24px; }
.result-card header, .recent-card header { display: flex; align-items: start; justify-content: space-between; gap: 16px; }
.result-card h2, .recent-card h2 { margin: 0; font-size: 18px; line-height: 1.15; font-weight: 750; }
.result-card p, .recent-card p { margin: 6px 0 0; color: var(--color-ink-muted); font-size: 11px; }
.result-card p strong { color: var(--color-success); font-family: var(--font-mono); }
.result-grid { display: grid; grid-template-columns: repeat(5, minmax(0, 1fr)); gap: 10px; margin-top: 20px; }
.result-grid div { display: grid; gap: 10px; padding: 14px; border-radius: 10px; background: var(--color-surface-2); }
.result-grid span { color: var(--color-ink-faint); font-size: 10px; }
.result-grid strong { font-family: var(--font-mono); font-size: 18px; line-height: 1; font-weight: 750; font-variant-numeric: tabular-nums; }
.recent-card { overflow: hidden; }
.recent-card .card-heading { padding: 22px 24px; }
.tone-success { color: var(--color-success); }.tone-danger { color: var(--color-danger); }
.capture-link { color: var(--color-info); }.capture-link:hover { text-decoration: underline; }
.audit-card { overflow: hidden; }.audit-card summary { cursor: pointer; padding: 18px 24px; color: var(--color-ink); font-size: 13px; font-weight: 700; }.audit-card ul { margin: 0; padding: 0 24px; list-style: none; }.audit-card li { display: flex; justify-content: space-between; gap: 14px; padding: 10px 0; border-top: 1px solid var(--color-line); color: var(--color-ink-muted); font-family: var(--font-mono); font-size: 11px; }.audit-card li small { margin-left: 10px; color: var(--color-ink-faint); font-family: var(--font-sans); }.audit-card time { flex-shrink: 0; color: var(--color-ink-faint); font-family: var(--font-sans); }
@media (max-width: 1535px) { .metric-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } .dashboard-two-column { grid-template-columns: 1fr; } .result-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); } }
@media (max-width: 599px) { .metric-grid { grid-template-columns: 1fr; } .result-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } .refresh-label { display: none; } .result-card, .recent-card .card-heading { padding: 20px 16px; } }
</style>
