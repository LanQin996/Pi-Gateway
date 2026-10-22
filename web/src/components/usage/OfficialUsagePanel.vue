<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RefreshCw } from 'lucide-vue-next'
import type { Account, OfficialUsageResponse } from '../../api/types'
import { api } from '../../api/client'
import { metric, money } from '../../api/stats-format'
import { formatDateTime, useI18n } from '../../i18n'

const props = defineProps<{ accounts: Account[]; accountId?: number }>()
const { locale } = useI18n()
const zh = computed(() => locale.value === 'zh-CN')
const preset = ref('30')
const selected = ref(props.accountId ? String(props.accountId) : '')
const utcDate = (date: Date) => date.toISOString().slice(0, 10)
const end = ref(utcDate(new Date()))
const start = ref(utcDate(new Date(Date.now() - 29 * 86400000)))
const data = ref<OfficialUsageResponse | null>(null)
const loading = ref(false)
const syncing = ref(false)
const error = ref('')
let generation = 0
const eligible = computed(() => props.accounts.filter(a => a.codex_linked && (!props.accountId || a.id === props.accountId)))
const targets = computed(() => eligible.value.filter(a => !selected.value || String(a.id) === selected.value))
const valid = computed(() => /^\d{4}-\d{2}-\d{2}$/.test(start.value) && /^\d{4}-\d{2}-\d{2}$/.test(end.value) && start.value <= end.value && end.value <= utcDate(new Date()))
const updated = computed(() => data.value?.sync.length ? Math.min(...data.value.sync.map(s => s.synced_at)) : 0)
const syncErrors = computed(() => data.value?.sync.filter(s => s.error).map(s => s.error) ?? [])
const accountName = (id: number) => props.accounts.find(a => a.id === id)?.name ?? String(id)
const totalTokens = computed(() => {
    const values = data.value?.items.map(d => d.total_tokens).filter((n): n is number => n != null) ?? []
    return values.length ? values.reduce((sum, n) => sum + n, 0) : null
})
async function load() {
    const run = ++generation
    data.value = null
    error.value = ''
    if (!valid.value) { error.value = zh.value ? '请选择有效日期区间（最多 366 天）' : 'Choose a valid date range (up to 366 days)'; return }
    loading.value = true
    try {
        const result = await api.officialUsage(start.value, end.value, selected.value ? Number(selected.value) : undefined)
        if (run === generation) { data.value = result }
    } catch (err) { if (run === generation) { error.value = err instanceof Error ? err.message : String(err) } }
    finally { if (run === generation) { loading.value = false } }
}
function changePreset() {
    if (preset.value === 'custom') { return }
    const now = new Date()
    end.value = utcDate(now)
    start.value = utcDate(new Date(now.getTime() - (Number(preset.value) - 1) * 86400000))
    void load()
}
async function refresh() {
    syncing.value = true
    const failures: string[] = []
    // Keep upstream refreshes sequential to avoid creating a burst of vendor requests.
    await targets.value.reduce((previous, account) => previous.then(async () => {
        try {
            const result = await api.refreshOfficialUsage(account.id)
            if (result.error) { failures.push(`${account.name}: ${result.error}`) }
        } catch (err) { failures.push(`${account.name}: ${err instanceof Error ? err.message : String(err)}`) }
    }), Promise.resolve())
    await load()
    if (failures.length) { error.value = failures.join('; ') }
    syncing.value = false
}
watch(() => props.accountId, id => { selected.value = id ? String(id) : ''; void load() })
watch(() => props.accounts.map(a => `${a.id}:${a.codex_linked}:${a.codex_account_id}`).join(','), () => { void load() })
onMounted(() => { void load() })
onBeforeUnmount(() => { generation++ })
</script>

<template>
    <section class="card official-usage" :aria-busy="loading || syncing">
        <header class="official-heading">
            <div><h2>{{ zh ? '官方结算消耗' : 'Official usage' }}</h2><p>{{ zh ? '官方每日数据 · UTC 日期 · 包含绕过本网关的客户端消耗' : 'Official daily data · UTC dates · Includes usage outside this gateway' }}</p></div>
            <div class="official-controls">
                <select v-if="!accountId" v-model="selected" class="input" :disabled="loading || syncing" :aria-label="zh ? '官方统计账号' : 'Official usage account'" @change="load"><option value="">{{ zh ? '全部已绑定账号' : 'All linked accounts' }}</option><option v-for="a in eligible" :key="a.id" :value="String(a.id)">{{ a.name }}</option></select>
                <select v-model="preset" class="input" :disabled="loading || syncing" :aria-label="zh ? '官方统计期间' : 'Official usage period'" @change="changePreset"><option value="1">{{ zh ? '今日' : 'Today' }}</option><option value="7">{{ zh ? '近 7 天' : 'Last 7 days' }}</option><option value="30">{{ zh ? '近 30 天' : 'Last 30 days' }}</option><option value="84">{{ zh ? '近 84 天' : 'Last 84 days' }}</option><option value="custom">{{ zh ? '自定义' : 'Custom' }}</option></select>
                <button type="button" class="btn btn-ghost" :disabled="loading || syncing || !targets.length" @click="refresh"><RefreshCw :size="14" :class="syncing ? 'animate-spin' : ''" />{{ (() => { if (zh) { if (syncing) { return '同步中'; } return '同步官方数据'; } if (syncing) { return 'Syncing'; } return 'Sync official data'; })() }}</button>
            </div>
        </header>
        <form v-if="preset === 'custom'" class="official-controls custom-dates" @submit.prevent="load"><label>{{ zh ? '开始日期' : 'Start date' }}<input v-model="start" class="input" type="date" required :max="end" :disabled="loading || syncing" /></label><label>{{ zh ? '结束日期' : 'End date' }}<input v-model="end" class="input" type="date" required :min="start" :max="utcDate(new Date())" :disabled="loading || syncing" /></label><button type="submit" class="btn" :disabled="loading || syncing">{{ zh ? '应用' : 'Apply' }}</button></form>
        <p v-if="error" role="alert" class="notice notice-error">{{ error }}</p>
        <output v-for="(message, index) in syncErrors" :key="index"  class="official-warning">{{ message }}</output>
        <div class="official-summary">
            <div><span>{{ zh ? '期间 Credits' : 'Period credits' }}</span><strong>{{ metric(data?.total_credits, 4) }}</strong></div>
            <div><span>{{ zh ? '等价美元' : 'Equivalent USD' }}</span><strong>{{ money(data?.equivalent_usd) }}</strong></div>
            <div><span>{{ zh ? '官方 Token' : 'Official tokens' }}</span><strong>{{ metric(totalTokens) }}</strong></div>
            <div><span>{{ zh ? '待结算记录' : 'Unsettled records' }}</span><strong>{{ metric(data?.pending_days) }}</strong></div>
        </div>
        <p class="official-note">{{ start }} — {{ end }} · {{ zh ? 'UTC 日汇总；今日可能延迟。美元按 25 credits/$ 折算为等价金额，实际付款依套餐而定。首次回补近 84 天，此后每小时更新近 7 天，历史快照保留。' : 'UTC daily totals; today may lag. USD is a reference conversion at 25 credits/$; actual payment depends on your plan. Initially backfills 84 days, then updates the last 7 days hourly and retains history.' }}</p>
        <p v-if="data?.missing_credit_days" class="official-warning">{{ zh ? `${data.missing_credit_days} 条记录缺少 Credits，合计为已知部分。` : `${data.missing_credit_days} records lack credits; totals are partial.` }}</p>
        <p v-if="data?.sync.some(s => !s.synced_at)" class="official-warning">{{ zh ? '部分账号尚未成功同步，期间合计可能不完整。' : 'Some accounts have not synced successfully; period totals may be incomplete.' }}</p>
        <div class="official-scroll"><table v-if="data?.items.length" class="official-table"><thead><tr><th>{{ zh ? '日期（UTC）' : 'Date (UTC)' }}</th><th>{{ zh ? '账号' : 'Account' }}</th><th>Credits</th><th>{{ zh ? '等价美元' : 'Equivalent USD' }}</th><th>{{ zh ? '输入' : 'Input' }}</th><th>{{ zh ? '缓存输入' : 'Cached input' }}</th><th>{{ zh ? '输出' : 'Output' }}</th><th>{{ zh ? '状态' : 'Status' }}</th></tr></thead><tbody><tr v-for="d in data.items" :key="`${d.account_id}:${d.day}`"><td>{{ d.day }}</td><td>{{ accountName(d.account_id) }}</td><td>{{ metric(d.credits, 4) }}</td><td>{{ money(d.credits == null ? null : d.credits / data.credits_per_usd) }}</td><td>{{ metric(d.uncached_input_tokens) }}</td><td>{{ metric(d.cached_input_tokens) }}</td><td>{{ metric(d.output_tokens) }}</td><td>{{ (() => { if (zh) { if (d.settled) { return '已结算'; } return '待结算'; } if (d.settled) { return 'Settled'; } return 'Pending'; })() }}</td></tr></tbody></table></div>
        <p v-if="!data?.items.length" class="official-note">{{ (() => { if (loading) { if (zh) { return '正在读取…'; } return 'Loading…'; } if (!eligible.length) { if (zh) { return '绑定 Codex 凭证后可同步官方消耗。'; } return 'Link a Codex credential to sync official usage.'; } if (zh) { return '该期间暂无官方记录。官方未返回的数据不记为零消耗。'; } return 'No official records for this period. Unavailable data is not treated as zero usage.'; })() }}</p>
        <p v-if="updated" class="official-note">{{ zh ? '最近完整同步' : 'Last complete sync' }} · {{ formatDateTime(updated) }}</p>
    </section>
</template>

<style scoped>
.official-usage { display: grid; gap: 14px; padding: 22px; min-width: 0; }
.official-heading { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 16px; }
.official-heading h2 { margin: 0; font-size: 19px; color: var(--color-ink); }
.official-heading p, .official-note { margin: 6px 0 0; color: var(--color-ink-muted); font-size: 12px; line-height: 1.6; }
.official-controls { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }
.official-controls .input { width: auto; max-width: 210px; font-size: 12px; }
.custom-dates label { display: grid; gap: 4px; color: var(--color-ink-muted); font-size: 12px; }.custom-dates { align-items: end; }
.official-summary { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; }
.official-summary > div { display: grid; gap: 8px; padding: 12px; background: var(--color-surface-2); border-radius: var(--radius-control); }
.official-summary span { color: var(--color-ink-muted); font-size: 12px; }.official-summary strong { font-family: var(--font-mono); color: var(--color-ink); font-size: 18px; overflow-wrap: anywhere; }
.official-warning { margin: 0; color: var(--color-warn); font-size: 12px; overflow-wrap: anywhere; }
.official-scroll { overflow: auto; }.official-table { width: 100%; border-collapse: collapse; font-size: 12px; white-space: nowrap; }
.official-table th, .official-table td { padding: 10px 12px; text-align: right; border-bottom: 1px solid var(--color-border); font-variant-numeric: tabular-nums; }.official-table th { color: var(--color-ink-muted); }.official-table th:first-child, .official-table td:first-child, .official-table th:nth-child(2), .official-table td:nth-child(2) { text-align: left; }
@media (max-width: 700px) { .official-usage { padding: 16px; }.official-summary { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
</style>
