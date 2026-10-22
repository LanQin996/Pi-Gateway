<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { ChartNoAxesCombined, RefreshCw } from 'lucide-vue-next'
import { api } from '../api/client'
import type { AccountStats, KeyStats, ModelStats, StatsBucket, StatsRange, StatsSummary } from '../api/types'
import { metric, money, percent, rate } from '../api/stats-format'
import { formatDateTime, useI18n } from '../i18n'
import PageHeader from '../components/PageHeader.vue'
import BaseChart from '../components/BaseChart.vue'
import TimeRangePicker from '../components/TimeRangePicker.vue'
import StatsTable from '../components/StatsTable.vue'
import UsageSummaryCards from '../components/usage/UsageSummaryCards.vue'
import { useStatsChart, type StatsSeries } from '../components/stats/chart-options'
import { duration } from '../components/usage/format'

const { t, locale } = useI18n()
const { palette, option } = useStatsChart()
const range = ref<StatsRange>({ start: Date.now() - 86400000, end: Date.now() })
const rangePicker = ref<InstanceType<typeof TimeRangePicker>>()
const summary = ref<StatsSummary | null>(null)
const buckets = ref<StatsBucket[]>([])
const models = ref<ModelStats[]>([])
const keys = ref<KeyStats[]>([])
const accounts = ref<AccountStats[]>([])
const accountNames = ref<Record<string, string>>({})
const loading = ref(false)
const error = ref('')
const trendMetric = ref<'requests' | 'tokens'>('requests')
const performanceMetric = ref<'latency' | 'ttft' | 'throughput'>('latency')
const costMetric = ref<'cost' | 'cache'>('cost')
const dimension = ref<'model' | 'key' | 'account'>('model')
let generation = 0
const labels = computed(() => locale.value === 'zh-CN' ? {
	trend: '使用趋势', trendDescription: '按时间桶观察请求与 Token 用量', requests: '请求', tokens: 'Token',
	diagnostic: '热点诊断', diagnosticDescription: '按维度聚合请求、失败与性能', dimension: '诊断维度',
	response: '响应速度', responseDescription: '时间桶均值与全范围分位数', total: '总耗时', first: '首字', throughput: '吞吐',
	costTitle: '成本效率', costDescription: '实际费用与缓存用量', cost: '费用', cache: '缓存',
	average: '平均耗时', success: '成功率', failures: '失败请求', actualCost: '实际费用',
	details: '分组明细', detailsDescription: '查看各维度完整的 Token、费用、失败率与性能指标',
	quantileHint: '上方为所选时间范围分位数，曲线为每个时间桶的均值。',
	input: '输入', output: '输出', cached: '缓存', totalTokens: '总 Token',
} : {
	trend: 'Usage trend', trendDescription: 'Requests and tokens by time bucket', requests: 'Requests', tokens: 'Tokens',
	diagnostic: 'Hotspot diagnostics', diagnosticDescription: 'Requests, failures and performance by dimension', dimension: 'Diagnostic dimension',
	response: 'Response speed', responseDescription: 'Bucket averages and period quantiles', total: 'Latency', first: 'TTFT', throughput: 'Throughput',
	costTitle: 'Cost efficiency', costDescription: 'Actual cost and cached usage', cost: 'Cost', cache: 'Cache',
	average: 'Average latency', success: 'Success rate', failures: 'Failed requests', actualCost: 'Actual cost',
	details: 'Breakdown details', detailsDescription: 'Token, cost, failure-rate and performance metrics by dimension',
	quantileHint: 'Summary quantiles cover the selected period; curves show bucket averages.',
	input: 'Input', output: 'Output', cached: 'Cached', totalTokens: 'Total tokens',
})
const dimensions = computed(() => [
	{ value: 'model' as const, label: t('stats.model') },
	{ value: 'account' as const, label: t('stats.account') },
	{ value: 'key' as const, label: t('stats.key') },
])
const performanceOptions = computed(() => [
	{ value: 'latency' as const, label: labels.value.total },
	{ value: 'ttft' as const, label: labels.value.first },
	{ value: 'throughput' as const, label: labels.value.throughput },
])
const modelRows = computed(() => models.value.map(item => ({ ...item, name: item.key })))
const keyRows = computed(() => keys.value.map(item => ({ ...item, name: String(item.key) })))
const accountRows = computed(() => accounts.value.map(item => ({ ...item, name: accountNames.value[String(item.id)] || `#${item.id}` })))
const activeRows = computed(() => (() => {
    if (dimension.value === 'model') {
        return modelRows.value
    }
    if (dimension.value === 'account') {
        return accountRows.value
    }
    return keyRows.value
})())
const dimensionLabel = computed(() => dimensions.value.find(item => item.value === dimension.value)!.label)
function throughput(value: number | null | undefined) { return value == null ? '—' : `${metric(value, 2)} /s` }
const trendSummary = computed(() => (() => {
    if (trendMetric.value === 'tokens') {
        return [
	{ label: labels.value.totalTokens, value: metric(summary.value?.total_tokens), tone: 'blue' },
	{ label: labels.value.output, value: metric(summary.value?.output_tokens), tone: 'green' },
	{ label: labels.value.cached, value: metric(summary.value?.cached_tokens), tone: 'muted' },
]
    }
    return [
	{ label: t('stats.requests'), value: metric(summary.value?.request_count), tone: 'blue' },
	{ label: labels.value.success, value: percent(summary.value ? rate(summary.value.success_count, summary.value.request_count) : null), tone: 'green' },
	{ label: labels.value.failures, value: metric(summary.value?.failure_count), tone: 'orange' },
]
})())
const performanceSummary = computed(() => {
	const s = summary.value
	if (performanceMetric.value === 'throughput') { return [
		{ label: 'P50', value: throughput(s?.output_tps_p50) },
		{ label: 'P90', value: throughput(s?.output_tps_p90) },
		{ label: t('stats.outputTokens'), value: metric(s?.output_tokens) },
	] }
	if (performanceMetric.value === 'ttft') { return [
		{ label: 'P50', value: duration(s?.ttft_p50_ms) },
		{ label: 'P90', value: duration(s?.ttft_p90_ms) },
		{ label: 'P95', value: duration(s?.ttft_p95_ms) },
	] }
	return [
		{ label: labels.value.average, value: duration(s?.avg_latency_ms) },
		{ label: 'TTFT · P50', value: duration(s?.ttft_p50_ms) },
		{ label: t('stats.tpsP50'), value: throughput(s?.output_tps_p50) },
	]
})
const costSummary = computed(() => [
	{ label: labels.value.actualCost, value: money(summary.value?.total_cost_usd) },
	{ label: t('stats.cacheRate'), value: percent(summary.value?.cached_token_rate) },
	{ label: t('stats.cacheHitRate'), value: percent(summary.value?.cache_hit_request_rate) },
])
const trendSeries = computed<StatsSeries[]>(() => trendMetric.value === 'requests' ? [
	{ name: t('stats.requests'), values: buckets.value.map(bucket => bucket.request_count), color: palette.value.blue, bar: true },
] : [
	{ name: labels.value.totalTokens, values: buckets.value.map(bucket => bucket.total_tokens), color: palette.value.blue, area: true },
	{ name: labels.value.output, values: buckets.value.map(bucket => bucket.output_tokens), color: palette.value.green },
	{ name: labels.value.cached, values: buckets.value.map(bucket => bucket.cached_tokens), color: palette.value.muted },
])
const performanceSeries = computed<StatsSeries[]>(() => {
	if (performanceMetric.value === 'throughput') { return [{ name: t('stats.tps'), values: buckets.value.map(bucket => bucket.output_tps), unit: 'throughput', color: palette.value.green, area: true }] }
	if (performanceMetric.value === 'ttft') { return [{ name: labels.value.first, values: buckets.value.map(bucket => bucket.ttft_ms), unit: 'duration', color: palette.value.cyan, area: true }] }
	return [{ name: labels.value.average, values: buckets.value.map(bucket => bucket.avg_latency_ms), unit: 'duration', color: palette.value.orange, area: true }]
})
const costSeries = computed<StatsSeries[]>(() => costMetric.value === 'cost' ? [
	{ name: labels.value.actualCost, values: buckets.value.map(bucket => bucket.cost_usd), unit: 'money', color: palette.value.green, area: true },
] : [
	{ name: labels.value.input, values: buckets.value.map(bucket => bucket.input_tokens), color: palette.value.blue },
	{ name: labels.value.cached, values: buckets.value.map(bucket => bucket.cached_tokens), color: palette.value.green, area: true },
])
const trendOption = computed(() => option(buckets.value, trendSeries.value))
const performanceOption = computed(() => option(buckets.value, performanceSeries.value))
const costOption = computed(() => option(buckets.value, costSeries.value))
function hasSamples(series: StatsSeries[]) { return !loading.value && series.some(item => item.values.some(value => value != null && Number.isFinite(value))) }
function refreshRange() { rangePicker.value?.refresh() }
async function load(next?: StatsRange) {
	if (next) { range.value = next }
	const run = ++generation
	loading.value = true
	error.value = ''
	summary.value = null; buckets.value = []; models.value = []; keys.value = []; accounts.value = []
	const { start, end } = range.value
	try {
		const [s, trend, m, k, a] = await Promise.all([
			api.statsSummary(start, end), api.statsTrend(start, end, end - start > 172800000 ? 'day' : 'hour'),
			api.statsModels(start, end), api.statsKeys(start, end), api.statsAccounts(start, end),
		])
		if (run !== generation) { return }
		summary.value = s; buckets.value = trend.buckets ?? []; models.value = m.items ?? []; keys.value = k.items ?? []; accounts.value = a.items ?? []
	} catch (err) {
		if (run === generation) { error.value = err instanceof Error ? err.message : t('stats.loadFailed') }
	} finally { if (run === generation) { loading.value = false } }
}
onMounted(() => {
	void load()
	void api.listAccounts().then(result => { accountNames.value = Object.fromEntries((result.accounts ?? []).map(account => [account.id, account.name])) }).catch(() => { /* Keep IDs as valid fallback labels. */ })
})
onBeforeUnmount(() => { generation++ })
</script>

<template>
	<div class="page-view stats-view">
		<PageHeader :title="t('stats.title')" :subtitle="t('stats.subtitle')"><TimeRangePicker ref="rangePicker" :disabled="loading" @change="load" /><button type="button" class="btn btn-ghost btn-icon" :title="t('common.refresh')" :aria-label="t('common.refresh')" :disabled="loading" @click="refreshRange"><RefreshCw :size="18" :class="loading ? 'animate-spin' : ''" /></button></PageHeader>
		<div class="page-content stats-content" :aria-busy="loading">
			<UsageSummaryCards :summary="summary" :loading="loading" />
			<p v-if="error" role="alert" class="notice notice-error">{{ error }}</p>
			<section class="insights-grid" :aria-label="t('stats.title')">
				<article class="card insight-card">
					<header class="insight-header"><div><h2>{{ labels.trend }}</h2><p>{{ labels.trendDescription }}</p></div><fieldset class="segmented"  :aria-label="t('stats.metric')"><button type="button" class="segmented-button" :class="{ 'is-active': trendMetric === 'requests' }" :aria-pressed="trendMetric === 'requests'" @click="trendMetric = 'requests'">{{ labels.requests }}</button><button type="button" class="segmented-button" :class="{ 'is-active': trendMetric === 'tokens' }" :aria-pressed="trendMetric === 'tokens'" @click="trendMetric = 'tokens'">{{ labels.tokens }}</button></fieldset></header>
					<div class="insight-body"><div class="metric-strip"><div v-for="item in trendSummary" :key="item.label" class="marked-metric"><i class="summary-dot" :class="`dot-${item.tone}`" /><span><small>{{ item.label }}</small><strong :title="item.value">{{ item.value }}</strong></span></div></div><BaseChart v-if="hasSamples(trendSeries)" :option="trendOption" :label="labels.trend" :height="210" /><output v-else class="insight-empty" ><ChartNoAxesCombined :size="28" /><span>{{ loading ? t('stats.loading') : t('stats.empty') }}</span></output></div>
				</article>
				<article class="card insight-card diagnostic-card">
					<header class="insight-header"><div><h2>{{ labels.diagnostic }}</h2><p>{{ labels.diagnosticDescription }}</p></div><fieldset class="segmented"  :aria-label="labels.dimension"><button v-for="item in dimensions" :key="item.value" type="button" class="segmented-button" :class="{ 'is-active': dimension === item.value }" :aria-pressed="dimension === item.value" @click="dimension = item.value">{{ item.label }}</button></fieldset></header>
					<div class="diagnostic-body"><StatsTable :rows="activeRows" :name-label="dimensionLabel" :show-share="dimension === 'model'" :loading="loading" compact /></div>
				</article>
				<article class="card insight-card">
					<header class="insight-header"><div><h2>{{ labels.response }}</h2><p :title="labels.quantileHint">{{ labels.responseDescription }}</p></div><fieldset class="segmented"  :aria-label="t('stats.metric')"><button v-for="item in performanceOptions" :key="item.value" type="button" class="segmented-button" :class="{ 'is-active': performanceMetric === item.value }" :aria-pressed="performanceMetric === item.value" @click="performanceMetric = item.value">{{ item.label }}</button></fieldset></header>
					<div class="insight-body"><div class="metric-strip"><div v-for="item in performanceSummary" :key="item.label"><small>{{ item.label }}</small><strong :title="item.value">{{ item.value }}</strong></div></div><BaseChart v-if="hasSamples(performanceSeries)" :option="performanceOption" :label="`${labels.response}: ${labels.quantileHint}`" :height="210" /><output v-else class="insight-empty" ><ChartNoAxesCombined :size="28" /><span>{{ loading ? t('stats.loading') : t('stats.empty') }}</span></output></div>
				</article>
				<article class="card insight-card">
					<header class="insight-header"><div><h2>{{ labels.costTitle }}</h2><p>{{ labels.costDescription }}</p></div><fieldset class="segmented"  :aria-label="t('stats.metric')"><button type="button" class="segmented-button" :class="{ 'is-active': costMetric === 'cost' }" :aria-pressed="costMetric === 'cost'" @click="costMetric = 'cost'">{{ labels.cost }}</button><button type="button" class="segmented-button" :class="{ 'is-active': costMetric === 'cache' }" :aria-pressed="costMetric === 'cache'" @click="costMetric = 'cache'">{{ labels.cache }}</button></fieldset></header>
					<div class="insight-body"><div class="metric-strip"><div v-for="item in costSummary" :key="item.label"><small>{{ item.label }}</small><strong :title="item.value">{{ item.value }}</strong></div></div><BaseChart v-if="hasSamples(costSeries)" :option="costOption" :label="labels.costTitle" :height="210" /><output v-else class="insight-empty" ><ChartNoAxesCombined :size="28" /><span>{{ loading ? t('stats.loading') : t('stats.empty') }}</span></output></div>
				</article>
			</section>
			<section class="card breakdown-card">
				<header class="insight-header"><div><h2>{{ labels.details }}</h2><p>{{ labels.detailsDescription }}</p></div><fieldset class="segmented"  :aria-label="labels.dimension"><button v-for="item in dimensions" :key="item.value" type="button" class="segmented-button" :class="{ 'is-active': dimension === item.value }" :aria-pressed="dimension === item.value" @click="dimension = item.value">{{ item.label }}</button></fieldset></header>
				<StatsTable :rows="activeRows" :name-label="dimensionLabel" :show-share="dimension === 'model'" :loading="loading" />
			</section>
			<div class="stats-scope"><span>{{ formatDateTime(range.start) }} — {{ formatDateTime(range.end) }}</span><span>{{ t('stats.unknownHint') }}</span></div>
		</div>
	</div>
</template>

<style scoped>
.stats-content { display: grid; gap: 20px; }
.insights-grid { display: grid; grid-template-columns: minmax(0, 1fr); gap: 12px; }
.insight-card { display: flex; min-width: 0; min-height: 360px; flex-direction: column; gap: 18px; padding: 22px; }
.insight-header { display: flex; align-items: flex-start; justify-content: space-between; flex-wrap: wrap; gap: 12px; }
.insight-header > div:first-child { min-width: 0; flex: 1 1 120px; }
.insight-header h2 { margin: 0; color: var(--color-ink); font-size: 20px; font-weight: 760; line-height: 1.15; }
.insight-header p { margin: 7px 0 0; color: var(--color-ink-muted); font-size: 13px; font-weight: 600; line-height: 1.25; }
.insight-header .segmented { flex-shrink: 0; flex-wrap: nowrap; }
.insight-header .segmented-button { min-height: 30px; padding: 5px 12px; font-size: 12px; font-weight: 700; }
.insight-body { display: grid; min-width: 0; align-content: start; gap: 12px; }
.metric-strip { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 8px; padding: 8px; border-radius: 12px; background: color-mix(in srgb, var(--color-surface-2) 45%, transparent); }
.metric-strip > div { display: grid; min-width: 0; gap: 5px; padding: 0 8px; }
.metric-strip small { display: block; overflow: hidden; color: var(--color-ink-faint); font-size: 10px; font-weight: 700; line-height: 1.4; text-overflow: ellipsis; white-space: nowrap; }
.metric-strip strong { display: block; overflow: hidden; color: var(--color-ink); font-family: var(--font-mono); font-size: 13px; font-weight: 750; line-height: 1.2; text-overflow: ellipsis; white-space: nowrap; font-variant-numeric: tabular-nums; }
.metric-strip .marked-metric { grid-template-columns: 8px minmax(0, 1fr); align-items: center; gap: 8px; }
.marked-metric > span { display: grid; min-width: 0; gap: 6px; }
.summary-dot { width: 8px; height: 8px; border-radius: 50%; }
.dot-blue { background: var(--color-chart-blue, var(--color-primary-seed)); }
.dot-green { background: var(--color-chart-green, var(--color-success)); }
.dot-orange { background: var(--color-chart-orange, var(--color-warn)); }
.dot-muted { background: var(--color-ink-faint); }
.diagnostic-card { min-height: 360px; }
.diagnostic-body { min-width: 0; max-height: 270px; overflow: auto; }
.diagnostic-body :deep(.stats-table-scroll) { min-height: 100%; }
.insight-empty { display: grid; min-height: 210px; align-content: center; justify-items: center; gap: 12px; color: var(--color-ink-faint); font-size: 12px; font-weight: 600; text-align: center; }
.insight-empty svg { opacity: .65; }
.breakdown-card { display: grid; min-width: 0; gap: 18px; padding: 22px; }
.stats-scope { display: flex; flex-wrap: wrap; gap: 4px 16px; color: var(--color-ink-faint); font-size: 11px; line-height: 1.5; }
@media (min-width: 1280px) { .insights-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); grid-auto-rows: 388px; } .diagnostic-body { flex: 1; min-height: 0; max-height: none; } }
@media (max-width: 640px) { .insight-card, .breakdown-card { padding: 16px; } .metric-strip { gap: 4px; padding: 8px 4px; } .metric-strip > div { padding: 0 4px; } .metric-strip .marked-metric { gap: 5px; } .insight-header .segmented-button { padding-inline: 10px; } }
</style>
