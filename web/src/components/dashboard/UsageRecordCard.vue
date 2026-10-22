<script setup lang="ts">
import type { UsageRecord } from '../../api/types'
import { metric, money } from '../../api/stats-format'
import { formatRelative, formatDuration } from '../../stores/ui'
import { useI18n } from '../../i18n'
import Badge from '../Badge.vue'
import { RouterLink } from 'vue-router'

defineProps<{ rows: UsageRecord[]; loading: boolean; error?: string }>()
const { t } = useI18n()
function outcomeTone(outcome: UsageRecord['outcome']) { return (() => {
    if (outcome === 'succeeded') {
        return 'success'
    }
    if (outcome === 'failed') {
        return 'danger'
    }
    if (outcome === 'running') {
        return 'info'
    }
    return 'warn'
})() }
</script>

<template>
	<article class="card usage-card"><header class="card-heading"><div><h2>{{ t('usage.title') }}</h2><p>{{ t('dashboard.successRecords') }}</p></div><RouterLink to="/usage" class="btn btn-ghost">{{ t('dashboard.viewAll') }}</RouterLink></header><div v-if="rows.length" class="table-scroll"><table class="w-full"><thead><tr><th class="th">{{ t('usage.col.startedAt') }}</th><th class="th">{{ t('usage.col.account') }}</th><th class="th">{{ t('usage.col.model') }}</th><th class="th">{{ t('usage.col.outcome') }}</th><th class="th text-right">{{ t('usage.col.totalTokens') }}</th><th class="th text-right">{{ t('usage.col.latency') }}</th><th class="th text-right">{{ t('usage.col.cost') }}</th></tr></thead><tbody><tr v-for="row in rows" :key="row.id" class="row-hover"><td class="td whitespace-nowrap text-[color:var(--color-ink-muted)]">{{ formatRelative(row.started_at) }}</td><td class="td">{{ row.account_name || '—' }}</td><td class="td font-mono text-[12px]">{{ row.model || '—' }}</td><td class="td"><Badge :tone="outcomeTone(row.outcome)">{{ t(`usage.outcome.${row.outcome}`) }}</Badge></td><td class="td text-right font-mono text-[12px]">{{ metric(row.total_tokens) }}</td><td class="td text-right font-mono text-[12px] text-[color:var(--color-ink-muted)]">{{ row.latency_ms == null ? '—' : formatDuration(row.latency_ms) }}</td><td class="td text-right font-mono text-[12px]">{{ money(row.cost_usd) }}</td></tr></tbody></table></div><output v-else class="usage-empty" >{{ loading ? t('stats.loading') : error || t('stats.empty') }}</output></article>
</template>

<style scoped>
.usage-card { overflow: hidden; }
.card-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 22px 24px; }
.card-heading h2 { margin: 0; font-size: 18px; font-weight: 750; }
.card-heading p { margin: 5px 0 0; color: var(--color-ink-faint); font-size: 11px; }
.usage-empty { display: grid; min-height: 180px; place-content: center; color: var(--color-ink-faint); font-size: 12px; }
@media (max-width: 599px) { .card-heading { padding: 18px 16px; } }
</style>
