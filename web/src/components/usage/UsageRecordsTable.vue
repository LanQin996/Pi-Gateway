<script setup lang="ts">
import { computed } from 'vue'
import { ArrowRight, Eye, FileText, Info } from 'lucide-vue-next'
import type { UsageRecord } from '../../api/types'
import { formatDateTime, useI18n } from '../../i18n'
import UsageTokenCell from './UsageTokenCell.vue'
import UsageLatencyCell from './UsageLatencyCell.vue'
import UsageCostCell from './UsageCostCell.vue'
import UsageRequestBadges from './UsageRequestBadges.vue'

defineProps<{ rows: UsageRecord[]; loading?: boolean; error?: string }>()
const emit = defineEmits<{ detail: [record: UsageRecord] }>()
const { t, locale } = useI18n()
const labels = computed(() => locale.value === 'zh-CN'
	? { tokens: 'TOKEN', cost: '费用', latency: '耗时 / 流速', time: '时间', status: '状态', actions: '操作', compaction: '压缩请求' }
	: { tokens: 'TOKEN', cost: 'Cost', latency: 'Latency / TPS', time: 'Time', status: 'Status', actions: 'Actions', compaction: 'Compaction request' })
function statusTone(code: number) {
	if (!code) { return 'neutral' }
	if (code >= 200 && code < 300) { return 'success' }
	if (code >= 300 && code < 400) { return 'warn' }
	return 'danger'
}
function datePart(timestamp: number, part: 'date' | 'time') {
	if (!timestamp) { return '—' }
	return new Intl.DateTimeFormat(locale.value, part === 'date' ? { month: '2-digit', day: '2-digit' } : { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false }).format(timestamp)
}
function transport(value: string) { return value ? ({ websocket: 'WS', ws: 'WS', sse: 'SSE', http_sse: 'SSE', http: 'HTTP' }[value] ?? value) : '—' }
</script>

<template>
	<div class="usage-table-scroll" :aria-busy="loading">
		<table class="usage-records-table">
			<thead><tr>
				<th class="th">{{ t('usage.col.key') }}</th><th class="th">{{ t('usage.col.account') }}</th>
				<th class="th">{{ t('usage.col.model') }}</th><th class="th">{{ t('usage.col.transport') }}</th>
				<th class="th">{{ labels.status }}</th><th class="th numeric">{{ labels.tokens }}</th>
				<th class="th numeric">{{ labels.cost }}</th><th class="th numeric">{{ labels.latency }}</th>
				<th class="th">{{ labels.time }}</th><th class="th">{{ labels.actions }}</th>
			</tr></thead>
			<tbody>
				<tr v-for="row in rows" :key="row.id" class="row-hover">
					<td class="td"><code class="identity-text" :title="row.api_key_name">{{ row.api_key_name || '—' }}</code></td>
					<td class="td"><code class="identity-text account-text" :title="row.account_name">{{ row.account_name || '—' }}</code></td>
					<td class="td"><div class="model-cell"><code class="identity-text model-text" :title="row.model">{{ row.model || '—' }}</code><UsageRequestBadges :record="row" /><span v-if="row.request_kind === 'compaction'" class="compaction-badge">{{ labels.compaction }}</span><button type="button" class="request-id" :title="`${t('usage.col.requestId')}: ${row.request_id}`" @click="emit('detail', row)">{{ row.request_id || '—' }}</button></div></td>
					<td class="td"><div class="transport-cell"><span class="transport-badge">{{ transport(row.client_transport) }}</span><ArrowRight :size="12" aria-hidden="true" /><span class="transport-badge">{{ transport(row.upstream_transport) }}</span></div></td>
					<td class="td"><div class="status-cell"><span class="status-code" :class="`status-${statusTone(row.status_code)}`">{{ row.status_code || '—' }}</span><span class="outcome-label" :class="`outcome-${row.outcome}`">{{ t(`usage.outcome.${row.outcome}`) }}</span><span v-if="row.error_code" class="error-code" :title="row.error_code">{{ row.error_code }}</span></div></td>
					<td class="td numeric"><div class="metrics-with-detail"><UsageTokenCell :record="row" /><button type="button" class="cell-info" :aria-label="`${t('common.viewDetails')}: ${t('stats.totalTokens')}`" @click="emit('detail', row)"><Info :size="13" /></button></div></td>
					<td class="td numeric"><UsageCostCell :record="row" /></td>
					<td class="td numeric"><UsageLatencyCell :first-token="row.first_token_ms" :latency="row.latency_ms" :throughput="row.output_tps ?? null" /></td>
					<td class="td"><time class="record-time" :datetime="new Date(row.started_at).toISOString()" :title="formatDateTime(row.started_at)"><span>{{ datePart(row.started_at, 'time') }}</span><small>{{ datePart(row.started_at, 'date') }}</small></time></td>
					<td class="td"><button type="button" class="btn btn-ghost btn-icon detail-button" :aria-label="t('common.viewDetails')" :title="t('common.viewDetails')" @click="emit('detail', row)"><Eye :size="14" /></button></td>
				</tr>
				<tr v-if="!rows.length"><td colspan="10"><output class="usage-empty" ><FileText :size="28" aria-hidden="true" /><span>{{ (() => { if (loading) { return t('stats.loading'); } if (error) { return t('stats.loadFailed'); } return t('stats.empty'); })() }}</span></output></td></tr>
			</tbody>
		</table>
	</div>
</template>

<style scoped>
.usage-table-scroll { min-width: 0; overflow: auto; border-radius: var(--radius-control); }
.usage-records-table { width: 100%; min-width: 1150px; border-collapse: separate; border-spacing: 0; white-space: nowrap; }
.usage-records-table .th { height: 40px; padding: 10px 12px; font-size: 12px; font-weight: 700; }
.usage-records-table .td { height: 60px; padding: 12px; vertical-align: middle; font-size: 12px; }
.numeric { text-align: right; }
.identity-text { display: block; max-width: 140px; overflow: hidden; color: var(--color-ink); font-family: var(--font-mono); font-size: 12px; font-weight: 700; line-height: 1.2; text-overflow: ellipsis; }
.account-text { max-width: 190px; }
.model-cell { display: grid; gap: 5px; }
.model-text { font-weight: 750; }
.compaction-badge { justify-self: start; padding: 3px 6px; border-radius: 5px; background: var(--color-accent-soft); color: var(--color-accent); font-size: 10px; font-weight: 650; }
.request-id { display: block; max-width: 155px; overflow: hidden; padding: 0; background: transparent; color: var(--color-ink-faint); text-align: left; font-family: var(--font-mono); font-size: 10px; font-weight: 600; line-height: 1.2; text-overflow: ellipsis; }
.request-id:hover { color: var(--color-info); }
.transport-cell { display: inline-flex; align-items: center; gap: 5px; color: var(--color-ink-faint); }
.transport-badge { padding: 3px 5px; border-radius: 5px; background: var(--color-surface-2); color: var(--color-ink-muted); font-family: var(--font-mono); font-size: 10px; font-weight: 700; line-height: 1.2; }
.status-cell { display: grid; justify-items: start; gap: 5px; }
.status-code { display: inline-flex; height: 24px; min-width: 48px; align-items: center; justify-content: center; padding: 0 8px; border-radius: 999px; font-family: var(--font-mono); font-size: 12px; font-weight: 700; line-height: 1; }
.status-neutral { background: var(--color-surface-2); color: var(--color-ink-muted); }
.status-success { background: var(--color-success-soft); color: var(--color-success-on, var(--color-success)); }
.status-warn { background: var(--color-warn-soft); color: var(--color-warn-on, var(--color-warn)); }
.status-danger { background: var(--color-danger-soft); color: var(--color-danger-on, var(--color-danger)); }
.outcome-label { padding-left: 3px; color: var(--color-ink-faint); font-size: 10px; font-weight: 600; line-height: 1; }
.outcome-succeeded { color: var(--color-success); }
.outcome-failed, .error-code { color: var(--color-danger); }
.error-code { max-width: 115px; overflow: hidden; font-family: var(--font-mono); font-size: 10px; text-overflow: ellipsis; }
.metrics-with-detail { display: flex; align-items: center; justify-content: flex-end; gap: 6px; }
.cell-info { display: inline-flex; align-items: center; justify-content: center; width: 22px; height: 26px; padding: 0; border-radius: 5px; color: var(--color-ink-faint); }
.cell-info:hover { background: var(--color-surface-2); color: var(--color-info); }
.cost-value { color: var(--color-success); font-family: var(--font-mono); font-size: 12px; font-weight: 700; font-variant-numeric: tabular-nums; }
.record-time { display: grid; gap: 5px; color: var(--color-ink-muted); font-family: var(--font-mono); font-size: 11px; font-weight: 600; line-height: 1; }
.record-time small { color: var(--color-ink-faint); font-size: 10px; }
.detail-button { width: 28px; min-height: 28px; padding: 0; }
.usage-empty { display: grid; min-height: 220px; place-content: center; justify-items: center; gap: 12px; padding: 24px; color: var(--color-ink-faint); font-size: 13px; font-weight: 600; text-align: center; white-space: normal; }
.usage-empty svg { opacity: .65; }
</style>
