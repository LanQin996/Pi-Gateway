<script setup lang="ts">
import { computed } from 'vue'
import type { UsageRecord } from '../../api/types'
import { metric } from '../../api/stats-format'
import { formatDateTime, useI18n } from '../../i18n'
import Modal from '../Modal.vue'
import UsageLatencyCell from './UsageLatencyCell.vue'
import UsageBillingDetails from './UsageBillingDetails.vue'
import UsageRequestBadges from './UsageRequestBadges.vue'

const props = defineProps<{ record: UsageRecord | null }>()
const open = defineModel<boolean>({ default: false })
const { t, locale } = useI18n()
const labels = computed(() => locale.value === 'zh-CN'
	? { subtitle: '单次请求的链路、Token、耗时与费用信息', overview: '请求概览', requestId: '请求 ID', startedAt: '开始时间', apiKey: 'API Key', account: '账号', model: '模型', transport: '传输', outcome: '执行结果', status: '状态码 / 错误', tokens: 'Token 明细', latency: '延迟明细', billing: '费用', reasoning: '推理 Token' }
	: { subtitle: 'Request route, tokens, latency and billing details', overview: 'Request overview', requestId: 'Request ID', startedAt: 'Started at', apiKey: 'API key', account: 'Account', model: 'Model', transport: 'Transport', outcome: 'Outcome', status: 'Status / error', tokens: 'Token details', latency: 'Latency details', billing: 'Billing', reasoning: 'Reasoning tokens' })
const detailItems = computed(() => {
	const row = props.record
	if (!row) { return [] }
	return [
		{ label: locale.value === 'zh-CN' ? '请求类型' : 'Request type', value: (() => {
    if (row.request_kind === 'compaction') {
        if (locale.value === 'zh-CN') {
            return '压缩请求'
        }
        return 'Compaction request'
    }
    if (row.request_kind === 'generation') {
        if (locale.value === 'zh-CN') {
            return '生成请求'
        }
        return 'Generation request'
    }
    return '—'
})(), mono: false },
		{ label: labels.value.requestId, value: row.request_id || '—', mono: true },
		{ label: labels.value.startedAt, value: formatDateTime(row.started_at), mono: true },
		{ label: labels.value.apiKey, value: row.api_key_name || '—', mono: true },
		{ label: labels.value.account, value: row.account_name || '—', mono: true },
		{ label: labels.value.model, value: row.model || '—', mono: true },
		{ label: labels.value.transport, value: [row.client_transport || '—', row.upstream_transport || '—'].join(' → '), mono: true },
		{ label: labels.value.outcome, value: t(`usage.outcome.${row.outcome}`), mono: false },
		{ label: labels.value.status, value: row.error_code ? `${row.status_code || '—'} · ${row.error_code}` : String(row.status_code || '—'), mono: true },
	]
})
</script>

<template>
	<Modal :open="open" :title="t('common.viewDetails')" width="max-w-3xl" @close="open = false">
		<div v-if="record" class="detail-modal-content">
			<p class="detail-subtitle">{{ labels.subtitle }}</p>
			<UsageRequestBadges :record="record" />
			<section class="detail-section">
				<h3 class="detail-heading">{{ labels.overview }}</h3>
				<dl class="detail-grid"><div v-for="item in detailItems" :key="item.label" class="detail-field"><dt>{{ item.label }}</dt><dd :class="item.mono ? 'mono' : ''">{{ item.value }}</dd></div></dl>
			</section>
			<section class="detail-section detail-two-columns">
				<div><h3 class="detail-heading">{{ labels.tokens }}</h3><dl class="detail-panel token-detail-panel"><div v-for="item in [{ label: t('stats.inputTokens'), value: record.input_tokens }, { label: t('stats.outputTokens'), value: record.output_tokens }, { label: t('stats.cachedTokens'), value: record.cached_tokens }, { label: labels.reasoning, value: record.reasoning_tokens }, { label: t('stats.totalTokens'), value: record.total_tokens }]" :key="item.label" class="detail-row"><dt>{{ item.label }}</dt><dd>{{ metric(item.value) }}</dd></div></dl></div>
				<div><h3 class="detail-heading">{{ labels.latency }}</h3><div class="detail-panel"><UsageLatencyCell :first-token="record.first_token_ms" :latency="record.latency_ms" :throughput="record.output_tps ?? null" /></div></div>
			</section>
			<section class="detail-section"><UsageBillingDetails :record="record" /></section>
		</div>
	</Modal>
</template>

<style scoped>
.detail-modal-content { display: grid; gap: 16px; }
.detail-subtitle { margin: -4px 0 0; color: var(--color-ink-muted); font-size: 13px; }
.detail-section { display: grid; gap: 10px; padding: 14px; border-radius: var(--radius-control); background: var(--color-surface-2); }
.detail-heading { margin: 0; color: var(--color-ink-muted); font-size: 12px; font-weight: 750; }
.detail-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px 18px; margin: 0; }
.detail-field { min-width: 0; }
.detail-field dt { color: var(--color-ink-faint); font-size: 11px; line-height: 1.2; }
.detail-field dd { margin: 5px 0 0; overflow-wrap: anywhere; color: var(--color-ink); font-size: 13px; font-weight: 650; }
.detail-field dd.mono, .detail-panel, .detail-panel strong { font-family: var(--font-mono); font-variant-numeric: tabular-nums; }
.detail-two-columns { grid-template-columns: repeat(2, minmax(0, 1fr)); }
.detail-panel { display: flex; min-height: 54px; align-items: center; justify-content: space-between; gap: 12px; padding: 10px 12px; border-radius: 9px; background: var(--color-surface); color: var(--color-ink-muted); font-size: 12px; }
.detail-panel :deep(.latency-cell) { font-size: 12px; }
.token-detail-panel { display: grid; gap: 8px; margin: 0; }
.detail-two-columns > div { display: grid; align-content: start; gap: 10px; }
.detail-row { display: flex; align-items: center; justify-content: space-between; gap: 12px; color: var(--color-ink-muted); }
.detail-row:last-child { padding-top: 8px; border-top: 1px solid var(--color-line); }
.detail-row dd { margin: 0; color: var(--color-ink); font-weight: 750; }
.billing-panel strong { color: var(--color-success); }
@media (max-width: 640px) { .detail-grid, .detail-two-columns { grid-template-columns: 1fr; } }
</style>
