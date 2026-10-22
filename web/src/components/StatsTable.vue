<script setup lang="ts">
import { computed } from 'vue'
import type { StatsBreakdown } from '../api/types'
import { metric, money, percent, rate } from '../api/stats-format'
import { useI18n } from '../i18n'
import UsageTokenCell from './usage/UsageTokenCell.vue'
import UsageLatencyCell from './usage/UsageLatencyCell.vue'

defineProps<{ rows: Array<StatsBreakdown & { name: string }>; nameLabel: string; showShare?: boolean; compact?: boolean; loading?: boolean }>()
const { t, locale } = useI18n()
const labels = computed(() => locale.value === 'zh-CN'
	? { requests: '请求', tokens: 'TOKEN', cost: '费用', errors: '失败', performance: '性能', share: '占比' }
	: { requests: 'Requests', tokens: 'TOKEN', cost: 'Cost', errors: 'Failed', performance: 'Performance', share: 'Share' })
</script>

<template>
	<div class="stats-table-scroll" :class="{ compact }" :aria-busy="loading">
		<table class="stats-table">
			<thead><tr><th class="th">{{ nameLabel }}</th><th class="th numeric">{{ labels.requests }}</th><th class="th numeric">{{ labels.errors }}</th><th v-if="!compact" class="th numeric">{{ labels.tokens }}</th><th class="th numeric">{{ labels.performance }}</th><th class="th numeric">{{ labels.cost }}</th><th v-if="showShare && !compact" class="th numeric">{{ labels.share }}</th></tr></thead>
			<tbody>
				<tr v-for="row in rows" :key="`${row.id}:${row.name}`" class="row-hover">
					<td class="td"><code class="breakdown-name" :title="row.name">{{ row.name || '—' }}</code></td>
					<td class="td numeric"><strong class="main-number" :title="`${t('usage.outcome.succeeded')}: ${metric(row.success_count)}`">{{ metric(row.request_count) }}</strong><small v-if="compact && showShare" class="sub-number">{{ percent(row.share) }}</small><small v-else-if="!compact" class="sub-number">{{ metric(row.success_count) }} {{ t('usage.outcome.succeeded') }}</small></td>
					<td class="td numeric"><strong :class="row.failure_count > 0 ? 'danger-number' : 'muted-number'">{{ metric(row.failure_count) }}</strong><small class="sub-number" :title="t('stats.failureRate')">{{ percent(rate(row.failure_count, row.request_count)) }}</small></td>
					<td v-if="!compact" class="td numeric"><UsageTokenCell :record="row" show-total /></td>
					<td class="td numeric"><UsageLatencyCell :first-token="row.ttft_ms" :latency="row.avg_latency_ms" :throughput="compact ? undefined : row.output_tps" /></td>
					<td class="td numeric"><span class="cost-number">{{ money(row.cost_usd) }}</span></td>
					<td v-if="showShare && !compact" class="td numeric">{{ percent(row.share) }}</td>
				</tr>
				<tr v-if="!rows.length"><td :colspan="(() => { if (compact) { return 5; } if (showShare) { return 7; } return 6; })()" class="stats-empty"><output>{{ loading ? t('stats.loading') : t('stats.empty') }}</output></td></tr>
			</tbody>
		</table>
	</div>
</template>

<style scoped>
.stats-table-scroll { min-width: 0; overflow: auto; border-radius: var(--radius-control); }
.stats-table { width: 100%; min-width: 820px; border-collapse: separate; border-spacing: 0; white-space: nowrap; }
.stats-table .th { height: 40px; padding: 10px 12px; font-size: 12px; font-weight: 700; }
.stats-table .td { height: 62px; padding: 10px 12px; color: var(--color-ink-muted); font-family: var(--font-mono); font-size: 12px; font-weight: 650; font-variant-numeric: tabular-nums; }
.numeric { text-align: right; }
.breakdown-name { display: block; max-width: 260px; overflow: hidden; color: var(--color-ink); font-family: var(--font-mono); font-size: 12px; font-weight: 750; text-overflow: ellipsis; }
.main-number { display: block; color: var(--color-ink); font-weight: 700; line-height: 1.1; }
.sub-number { display: block; margin-top: 5px; color: var(--color-ink-faint); font-size: 10px; font-weight: 600; line-height: 1; }
.cost-number { color: var(--color-success); font-weight: 700; }
.danger-number { color: var(--color-danger); font-weight: 700; }
.muted-number { color: var(--color-ink-faint); }
.stats-empty { height: 180px; padding: 24px 16px; color: var(--color-ink-faint); font-family: var(--font-sans); font-size: 12px; text-align: center; }
.compact .stats-table { min-width: 520px; }
.compact .th { padding: 8px; font-size: 11px; }
.compact .td { height: 53px; padding: 8px; }
.compact .breakdown-name { max-width: 175px; }
.compact :deep(.latency-cell) { gap: 5px 6px; font-size: 11px; }
.compact :deep(.latency-label) { font-size: 10px; }
</style>
