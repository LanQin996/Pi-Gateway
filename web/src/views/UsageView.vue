<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { ChevronLeft, ChevronRight, RefreshCw, Search, SlidersHorizontal } from 'lucide-vue-next'
import { api } from '../api/client'
import type { Account, ApiKey, StatsRange, StatsSummary, UsageOutcome, UsageRecord } from '../api/types'
import { formatDateTime, useI18n } from '../i18n'
import PageHeader from '../components/PageHeader.vue'
import TimeRangePicker from '../components/TimeRangePicker.vue'
import UsageSummaryCards from '../components/usage/UsageSummaryCards.vue'
import UsageRecordsTable from '../components/usage/UsageRecordsTable.vue'
import UsageRecordDetailModal from '../components/usage/UsageRecordDetailModal.vue'
import OfficialUsagePanel from '../components/usage/OfficialUsagePanel.vue'

const { t, locale } = useI18n()
const labels = computed(() => locale.value === 'zh-CN'
	? { records: '请求明细', description: '成功请求与失败请求明细', filters: '筛选条件', periodScope: '概览按所选时间范围统计；下方筛选仅作用于请求明细。' }
	: { records: 'Request records', description: 'Successful and failed request details', filters: 'Filters', periodScope: 'Overview covers the selected period. Filters below apply only to request records.' })
const range = ref<StatsRange>({ start: Date.now() - 86400000, end: Date.now() })
const rangePicker = ref<InstanceType<typeof TimeRangePicker>>()
const filters = ref({ api_key_id: '', account_id: '', model: '', outcome: '' as UsageOutcome | '', search: '' })
const activeFilters = ref({ ...filters.value })
const keys = ref<ApiKey[]>([])
const accounts = ref<Account[]>([])
const records = ref<UsageRecord[]>([])
const summary = ref<StatsSummary | null>(null)
const total = ref(0)
const limit = ref(25)
const offset = ref(0)
const loading = ref(false)
const summaryLoading = ref(false)
const error = ref('')
const summaryError = ref('')
const lookupError = ref('')
const showFilters = ref(false)
const selectedRecord = ref<UsageRecord | null>(null)
const detailOpen = ref(false)
const outcomes: UsageOutcome[] = ['running', 'succeeded', 'failed', 'cancelled', 'incomplete']
let generation = 0
let summaryGeneration = 0
const page = computed(() => Math.floor(offset.value / limit.value) + 1)
const pages = computed(() => Math.max(1, Math.ceil(total.value / limit.value)))
const activeFilterCount = computed(() => Object.values(activeFilters.value).filter(Boolean).length)
const rangeLabel = computed(() => `${formatDateTime(range.value.start)} — ${formatDateTime(range.value.end)}`)
async function load() {
	const run = ++generation
	loading.value = true
	error.value = ''
	records.value = []
	try {
		const f = activeFilters.value
		const result = await api.usageRecords({ ...range.value, ...f,
			api_key_id: f.api_key_id ? Number(f.api_key_id) : undefined,
			account_id: f.account_id ? Number(f.account_id) : undefined,
			model: f.model.trim(), search: f.search.trim(), limit: limit.value, offset: offset.value,
		})
		if (run !== generation) { return }
		total.value = result.total
		if (offset.value > 0 && offset.value >= result.total) {
			offset.value = Math.max(0, (Math.ceil(result.total / limit.value) - 1) * limit.value)
			void load()
			return
		}
		records.value = result.records ?? []
	} catch (err) {
		if (run === generation) { error.value = err instanceof Error ? err.message : t('stats.loadFailed'); total.value = 0 }
	} finally { if (run === generation) { loading.value = false } }
}
async function loadSummary() {
	const run = ++summaryGeneration
	summaryLoading.value = true
	summaryError.value = ''
	summary.value = null
	try {
		const result = await api.statsSummary(range.value.start, range.value.end)
		if (run === summaryGeneration) { summary.value = result }
	} catch (err) {
		if (run === summaryGeneration) { summaryError.value = err instanceof Error ? err.message : t('stats.loadFailed') }
	} finally { if (run === summaryGeneration) { summaryLoading.value = false } }
}
function apply() { activeFilters.value = { ...filters.value }; offset.value = 0; void load() }
function changeRange(next: StatsRange) { range.value = next; offset.value = 0; void load(); void loadSummary() }
function refresh() { rangePicker.value?.refresh(); void loadLookups() }
function paginate(direction: number) { offset.value = Math.max(0, offset.value + direction * limit.value); void load() }
function reset() { filters.value = { api_key_id: '', account_id: '', model: '', outcome: '', search: '' }; apply() }
function resizePage() { offset.value = 0; void load() }
function showDetail(record: UsageRecord) { selectedRecord.value = record; detailOpen.value = true }
async function loadLookups() {
	lookupError.value = ''
	const results = await Promise.allSettled([api.listKeys(), api.listAccounts()])
	if (results[0].status === 'fulfilled') { keys.value = results[0].value.keys ?? [] }
	if (results[1].status === 'fulfilled') { accounts.value = results[1].value.accounts ?? [] }
	if (results.some(result => result.status === 'rejected')) { lookupError.value = t('usage.lookupFailed') }
}
onMounted(() => { void load(); void loadSummary(); void loadLookups() })
onBeforeUnmount(() => { generation++; summaryGeneration++ })
</script>

<template>
	<div class="page-view usage-view">
		<PageHeader :title="t('usage.title')" :subtitle="t('usage.subtitle')"><TimeRangePicker ref="rangePicker" :disabled="loading || summaryLoading" @change="changeRange" /></PageHeader>
		<div class="page-content usage-content">
			<UsageSummaryCards :summary="summary" :loading="summaryLoading" />
			<p v-if="summaryError" role="alert" class="notice notice-error">{{ summaryError }}</p>
			<OfficialUsagePanel :accounts="accounts" />
			<section class="card records-card" :aria-busy="loading">
				<header class="records-header">
					<div><h2>{{ labels.records }}</h2><p>{{ labels.description }}</p></div>
					<span class="records-count">{{ t('usage.pagination', { page, pages, total }) }}</span>
				</header>
				<div class="records-body">
					<form class="records-filters" @submit.prevent="apply">
						<div class="usage-filter-toolbar">
							<label class="search-field records-search"><Search :size="18" aria-hidden="true" /><input v-model="filters.search" class="input" :aria-label="t('usage.search')" :placeholder="t('usage.searchPlaceholder')" :disabled="loading" /></label>
							<div class="filter-actions"><button type="button" class="btn btn-ghost" :aria-expanded="showFilters" aria-controls="usage-filters" @click="showFilters = !showFilters"><SlidersHorizontal :size="16" />{{ labels.filters }}<span v-if="activeFilterCount" class="filter-count">{{ activeFilterCount }}</span></button><button type="submit" class="btn" :disabled="loading">{{ t('stats.apply') }}</button><button type="button" class="btn btn-ghost btn-icon" :title="t('common.refresh')" :aria-label="t('common.refresh')" :disabled="loading || summaryLoading" @click="refresh"><RefreshCw :size="18" :class="loading ? 'animate-spin' : ''" /></button></div>
						</div>
						<div v-if="showFilters" id="usage-filters" class="filter-fields">
							<label><span class="label">{{ t('stats.key') }}</span><select v-model="filters.api_key_id" class="input" :disabled="loading"><option value="">{{ t('usage.all') }}</option><option v-for="key in keys" :key="key.id" :value="String(key.id)">{{ key.name }}</option></select></label>
							<label><span class="label">{{ t('stats.account') }}</span><select v-model="filters.account_id" class="input" :disabled="loading"><option value="">{{ t('usage.all') }}</option><option v-for="account in accounts" :key="account.id" :value="String(account.id)">{{ account.name }}</option></select></label>
							<label><span class="label">{{ t('stats.model') }}</span><input v-model="filters.model" class="input" :placeholder="t('usage.modelPlaceholder')" :disabled="loading" /></label>
							<label><span class="label">{{ t('usage.outcome') }}</span><select v-model="filters.outcome" class="input" :disabled="loading"><option value="">{{ t('usage.all') }}</option><option v-for="outcome in outcomes" :key="outcome" :value="outcome">{{ t(`usage.outcome.${outcome}`) }}</option></select></label>
							<button type="button" class="btn btn-ghost" :disabled="loading" @click="reset">{{ t('usage.reset') }}</button>
						</div>
					</form>
					<output v-if="lookupError"  class="lookup-warning">{{ lookupError }}</output>
					<p v-if="error" role="alert" class="notice notice-error">{{ error }}</p>
					<UsageRecordsTable :rows="records" :loading="loading" :error="error" @detail="showDetail" />
					<footer class="records-pagination"><span>{{ t('usage.pagination', { page, pages, total }) }}</span><div class="pagination-controls"><select v-model.number="limit" class="input page-size" :aria-label="t('usage.pageSize')" :disabled="loading" @change="resizePage"><option v-for="size in [25, 50, 100]" :key="size" :value="size">{{ size }} / {{ t('usage.pageSize') }}</option></select><button type="button" class="btn btn-ghost btn-icon" :title="t('usage.previous')" :aria-label="t('usage.previous')" :disabled="loading || offset === 0" @click="paginate(-1)"><ChevronLeft :size="16" /></button><span class="current-page">{{ page }}</span><button type="button" class="btn btn-ghost btn-icon" :title="t('usage.next')" :aria-label="t('usage.next')" :disabled="loading || offset + limit >= total" @click="paginate(1)"><ChevronRight :size="16" /></button></div></footer>
				</div>
			</section>
			<div class="usage-scope"><span>{{ rangeLabel }}</span><span>{{ labels.periodScope }} {{ t('stats.unknownHint') }}</span></div>
		</div>
		<UsageRecordDetailModal v-model="detailOpen" :record="selectedRecord" />
	</div>
</template>

<style scoped>
.usage-content { display: grid; gap: 20px; }
.records-header { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 12px; padding: 22px 22px 0; }
.records-header h2 { margin: 0; color: var(--color-ink); font-size: 20px; font-weight: 750; line-height: 1.15; }
.records-header p { margin: 7px 0 0; color: var(--color-ink-muted); font-size: 13px; font-weight: 600; line-height: 1.15; }
.records-count { color: var(--color-ink-faint); font-size: 12px; font-weight: 600; }
.records-body { display: grid; min-width: 0; gap: 12px; padding: 16px 22px 22px; }
.records-filters { display: grid; gap: 12px; }
.usage-filter-toolbar { display: flex; width: 100%; align-items: center; gap: 12px; }
.records-search { min-width: 0; flex: 1; }
.filter-actions { display: flex; flex-shrink: 0; align-items: center; justify-content: flex-end; gap: 8px; margin-left: auto; }
.filter-count { display: inline-grid; min-width: 18px; height: 18px; place-items: center; border-radius: 999px; background: var(--color-accent-soft); color: var(--color-accent); font-size: 10px; }
.filter-fields { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)) auto; align-items: end; gap: 12px; padding: 12px; border-radius: var(--radius-control); background: color-mix(in srgb, var(--color-surface-2) 45%, transparent); }
.lookup-warning { margin: 0; color: var(--color-warn); font-size: 12px; }
.records-pagination { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 12px; padding-top: 4px; color: var(--color-ink-faint); font-size: 12px; font-weight: 600; }
.pagination-controls { display: flex; align-items: center; gap: 6px; }
.page-size { width: auto; min-height: 30px; margin-right: 6px; padding: 4px 9px; font-size: 12px; }
.pagination-controls .btn { width: 30px; min-height: 30px; }
.current-page { display: inline-grid; width: 30px; height: 30px; place-items: center; border-radius: 7px; background: var(--color-accent-soft); color: var(--color-accent); font-family: var(--font-mono); }
.usage-scope { display: flex; flex-wrap: wrap; gap: 4px 16px; color: var(--color-ink-faint); font-size: 11px; line-height: 1.5; }
@media (min-width: 640px) { .records-search { width: 384px; flex: 0 1 384px; } }
@media (max-width: 900px) { .filter-fields { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 640px) { .records-header { padding: 16px 16px 0; } .records-body { padding: 16px; } .usage-filter-toolbar { flex-wrap: wrap; } .records-search { flex-basis: 100%; } .filter-actions { width: 100%; } .filter-fields { grid-template-columns: 1fr; } }
</style>
