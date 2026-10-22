<script setup lang="ts">
import { computed, ref } from 'vue'
import type { EChartsCoreOption } from 'echarts/core'
import type { StatsBucket, StatsSummary } from '../../api/types'
import { metric } from '../../api/stats-format'
import { useI18n } from '../../i18n'
import { useUiStore } from '../../stores/ui'
import BaseChart from '../BaseChart.vue'

const props = defineProps<{ buckets: StatsBucket[]; summary: StatsSummary | null; loading: boolean; error: string }>()
const { t, locale } = useI18n()
const ui = useUiStore()
const kind = ref<'tokens' | 'latency' | 'requests'>('tokens')
const activeSeries = ref('')
const themeSnapshot = computed(() => ({theme: ui.theme, css: getComputedStyle(document.documentElement)}))
const tabs = computed(() => [{ value: 'tokens', label: t('stats.totalTokens') }, { value: 'latency', label: t('dashboard.latency') }, { value: 'requests', label: t('stats.requests') }] as const)
const summaries = computed(() => {
	const s = props.summary
	if (kind.value === 'tokens') { return [
		{ key: 'input_tokens', label: t('dashboard.input'), value: metric(s?.input_tokens), tone: 'accent' },
		{ key: 'output_tokens', label: t('dashboard.output'), value: metric(s?.output_tokens), tone: 'success' },
		{ key: 'cached_tokens', label: t('dashboard.cached'), value: metric(s?.cached_tokens), tone: 'ink-faint' },
	] }
	if (kind.value === 'latency') { return [
		{ key: 'avg_latency_ms', label: t('stats.latency'), value: metric(s?.avg_latency_ms, 1), tone: 'accent' },
		{ key: 'ttft_ms', label: 'TTFT · p50 (ms)', value: metric(s?.ttft_p50_ms, 1), tone: 'success' },
		{ key: 'output_tps', label: t('stats.tpsP50'), value: metric(s?.output_tps_p50, 1), tone: 'warn' },
	] }
	return [
		{ key: 'request_count', label: t('stats.requests'), value: metric(s?.request_count), tone: 'accent' },
		{ key: '', label: t('dashboard.success'), value: metric(s?.success_count), tone: 'success' },
		{ key: '', label: t('dashboard.failed'), value: metric(s?.failure_count), tone: 'danger' },
	]
})
const fields = computed(() => (() => {
    if (kind.value === 'tokens') {
        return ['input_tokens', 'output_tokens', 'cached_tokens'] as const
    }
    if (kind.value === 'latency') {
        return ['avg_latency_ms', 'ttft_ms', 'output_tps'] as const
    }
    return ['request_count'] as const
})())
const hasSamples = computed(() => props.buckets.some(bucket => bucket.request_count > 0 && fields.value.some(field => bucket[field] != null)))
const chartTone = (tone: string) => (() => {
    if (tone === 'success') {
        return 'chart-green'
    }
    if (tone === 'warn') {
        return 'chart-orange'
    }
    if (tone === 'info' || tone === 'accent') {
        return 'chart-blue'
    }
    return 'ink-faint'
})()
const option = computed<EChartsCoreOption>(() => {
	const css = themeSnapshot.value.css
	const token = (name: string) => css.getPropertyValue(`--color-${name}`).trim()
	const dualAxis = kind.value !== 'requests'
	const axis = (side: 'left' | 'right') => ({ type: 'value', position: side, axisLabel: { color: token('ink-faint'), fontSize: 10, fontFamily: 'monospace', formatter: (value: number) => new Intl.NumberFormat(locale.value, { notation: 'compact', maximumFractionDigits: 1 }).format(value) }, splitLine: { show: side === 'left', lineStyle: { color: token('line'), type: 'dashed' } } })
	return {
		animation: false, aria: { enabled: true }, textStyle: { fontFamily: 'monospace', color: token('ink-faint') },
		tooltip: { trigger: 'axis', renderMode: 'richText', backgroundColor: token('surface'), borderColor: token('line'), textStyle: { color: token('ink') }, valueFormatter: (value: unknown) => (() => {
    if (typeof value === 'number') {
        return metric(value, kind.value === 'latency' ? 1 : 0)
    }
    return '—'
})() },
		grid: { left: 6, right: 8, top: 12, bottom: 6, containLabel: true },
		xAxis: { type: 'category', boundaryGap: false, data: props.buckets.map(bucket => new Intl.DateTimeFormat(locale.value, { hour: '2-digit', minute: '2-digit', hour12: false }).format(new Date(bucket.bucket_start))), axisLine: { show: false }, axisTick: { show: false }, axisLabel: { color: token('ink-faint'), fontSize: 10, hideOverlap: true, margin: 12 } },
		yAxis: dualAxis ? [axis('left'), axis('right')] : [axis('left')],
		series: fields.value.map((field, index) => ({ name: (() => {
    if (kind.value === 'latency' && field === 'ttft_ms') {
        return t('stats.ttft')
    }
    if (kind.value === 'latency' && field === 'output_tps') {
        return t('stats.tps')
    }
    return summaries.value[index]?.label
})(), type: 'line', data: props.buckets.map(bucket => bucket[field]), yAxisIndex: dualAxis && ((kind.value === 'tokens' && field === 'output_tokens') || field === 'output_tps') ? 1 : 0, smooth: false, connectNulls: false, showSymbol: props.buckets.length < 3, symbolSize: 5, lineStyle: { width: 1.8, color: token(chartTone(summaries.value[index]?.tone ?? 'accent')), opacity: !activeSeries.value || activeSeries.value === field ? 1 : .18 }, itemStyle: { color: token(chartTone(summaries.value[index]?.tone ?? 'accent')) }, areaStyle: { color: token(chartTone(summaries.value[index]?.tone ?? 'accent')), opacity: !activeSeries.value || activeSeries.value === field ? .04 : 0 } })),
	}
})
</script>

<template>
	<article class="card trend-card">
		<header class="trend-heading"><h2>{{ t('dashboard.trend') }}</h2><div class="segmented trend-tabs" :aria-label="t('stats.metric')"><button v-for="tab in tabs" :key="tab.value" type="button" class="segmented-button" :class="{ 'is-active': kind === tab.value }" :aria-pressed="kind === tab.value" @click="kind = tab.value; activeSeries = ''">{{ tab.label }}</button></div></header>
		<div class="trend-summary"><button v-for="item in summaries" :key="item.label" type="button" :disabled="!item.key" :aria-pressed="activeSeries === item.key && Boolean(item.key)" @click="activeSeries = activeSeries === item.key ? '' : item.key"><i :style="{ background: `var(--color-${item.tone})` }" /><span><small>{{ item.label }}</small><strong>{{ item.value }}</strong></span></button></div>
		<div class="trend-chart"><BaseChart v-if="hasSamples" :option="option" :label="t('dashboard.trend')" /><output v-else class="trend-empty" >{{ loading ? t('stats.loading') : error || t('dashboard.noTrend') }}</output></div>
	</article>
</template>

<style scoped>
.trend-card { min-height: 380px; padding: 24px; }
.trend-heading { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 14px; margin-bottom: 20px; }
.trend-heading h2 { margin: 0; font-size: 20px; line-height: 1.15; font-weight: 750; }
.trend-tabs { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); width: 246px; padding: 3px; border-radius: 9px; gap: 0; }
.trend-tabs .segmented-button { min-height: 32px; padding: 4px 8px; font-size: 11px; font-weight: 650; }
.trend-summary { height: 57px; display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 6px; padding: 6px; background: var(--color-surface-2); border-radius: 12px; }
.trend-summary button { min-width: 0; display: grid; grid-template-columns: 8px minmax(0, 1fr); align-items: center; gap: 8px; padding: 5px 10px; border: 0; background: transparent; text-align: left; color: var(--color-ink); border-radius: 8px; }
.trend-summary button:disabled { cursor: default; }
.trend-summary button[aria-pressed='true'] { background: var(--color-surface); }
.trend-summary i { width: 8px; height: 8px; border-radius: 50%; }
.trend-summary span { display: grid; gap: 6px; min-width: 0; }
.trend-summary small { font-size: 10px; line-height: 1; color: var(--color-ink-muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.trend-summary strong { font-family: var(--font-mono); font-size: 16px; font-weight: 750; line-height: 1; font-variant-numeric: tabular-nums; overflow: hidden; text-overflow: ellipsis; }
.trend-chart { height: 220px; margin-top: 14px; overflow: hidden; }
.trend-chart :deep(> div) { height: 220px; }
.trend-empty { display: grid; place-content: center; color: var(--color-ink-faint); font-size: 12px; text-align: center; }
@media (max-width: 599px) { .trend-card { padding: 20px 16px; } .trend-tabs { width: 100%; } .trend-summary button { padding-inline: 4px; } .trend-summary strong { font-size: 13px; } }
</style>
