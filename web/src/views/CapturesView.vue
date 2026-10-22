<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { api } from '../api/client'
import type { Account, Capture } from '../api/types'
import { useToastStore } from '../stores/ui'
import { useI18n } from '../i18n'
import PageHeader from '../components/PageHeader.vue'
import CaptureRow from '../components/CaptureRow.vue'
import { Activity, Download, RefreshCw, Trash2 } from 'lucide-vue-next'

const toast = useToastStore()
const router = useRouter()
const { t } = useI18n()

const captures = ref<Capture[]>([])
const accounts = ref<Account[]>([])
const total = ref(0)
const loading = ref(false)

const filters = ref({
	account_id: '' as string | number,
	outcome: '',
	transport: '',
	search: '',
	limit: 50,
	offset: 0,
})

const unassigned = computed(() => filters.value.account_id === 'unassigned')
const accountFilter = computed(() => ({
	account_id: unassigned.value ? undefined : filters.value.account_id || undefined,
	unassigned: unassigned.value ? 'true' : undefined,
}))
const displayCaptures = computed(() => captures.value.map(capture => capture.account_id === 0
	? { ...capture, account_name: t('capture.unassigned') } : capture))

let searchTimer: number | undefined

async function load() {
	loading.value = true
	try {
		const result = await api.listCaptures({
			...accountFilter.value,
			outcome: filters.value.outcome || undefined,
			transport: filters.value.transport || undefined,
			search: filters.value.search || undefined,
			limit: filters.value.limit,
			offset: filters.value.offset,
		})
		captures.value = result.captures ?? []
		total.value = result.total ?? 0
	} catch (err) {
		toast.error(err instanceof Error ? err.message : 'failed to load transfers')
	} finally {
		loading.value = false
	}
}

onMounted(async () => {
	const accountResult = await api.listAccounts().catch(() => ({ accounts: [] as Account[] }))
	accounts.value = accountResult.accounts ?? []
	await load()
})

watch(
	() => [filters.value.account_id, filters.value.outcome, filters.value.transport],
	() => {
		filters.value.offset = 0
		void load()
	},
)

watch(
	() => filters.value.search,
	() => {
		if (searchTimer) { window.clearTimeout(searchTimer) }
		searchTimer = window.setTimeout(() => {
			filters.value.offset = 0
			void load()
		}, 300)
	},
)

const page = computed(() => Math.floor(filters.value.offset / filters.value.limit) + 1)
const pageCount = computed(() => Math.max(1, Math.ceil(total.value / filters.value.limit)))

function goToPage(next: number) {
	const clamped = Math.min(Math.max(1, next), pageCount.value)
	filters.value.offset = (clamped - 1) * filters.value.limit
	void load()
}

function openCapture(capture: Capture) {
	void router.push(`/captures/${capture.id}`)
}

async function clearAll() {
	const scope = (() => {
    if (unassigned.value) {
        return t('captures.clearScopeUnassigned')
    }
    if (filters.value.account_id) {
        return t('captures.clearScopeAccount')
    }
    return t('captures.clearScopeAll')
})()
	if (!confirm(t('captures.clearConfirm', { scope }))) { return }
	try {
		const result = await api.clearCaptures((() => {
    if (unassigned.value) {
        return undefined
    }
    if (filters.value.account_id) {
        return Number(filters.value.account_id)
    }
    return undefined
})(), unassigned.value)
		toast.success(`${result.deleted}`)
		filters.value.offset = 0
		await load()
	} catch (err) {
		toast.error(err instanceof Error ? err.message : 'delete failed')
	}
}
</script>

<template>
	<div class="page-view">
		<PageHeader :title="t('captures.title')" :subtitle="t('captures.subtitle')">
			<a
				class="btn"
				:href="api.exportUrl({ ...accountFilter, outcome: filters.outcome, transport: filters.transport, search: filters.search })"
			>
				<Download class="h-3.5 w-3.5" />
				{{ t('captures.export') }}
			</a>
			<button class="btn" :disabled="loading" @click="load">
				<RefreshCw class="h-3.5 w-3.5" />
				{{ t('common.refresh') }}
			</button>
			<button class="btn btn-danger" @click="clearAll">
				<Trash2 class="h-3.5 w-3.5" />
				{{ t('captures.clear') }}
			</button>
		</PageHeader>

		<div class="page-content space-y-5" :aria-busy="loading">
			<!-- Filters -->
			<div class="card panel-content grid gap-3 sm:grid-cols-2 xl:grid-cols-5">
				<div>
					<label class="label" for="f-account">{{ t('captures.filter.account') }}</label>
					<select id="f-account" v-model="filters.account_id" class="input">
						<option value="">{{ t('captures.filter.allAccounts') }}</option>
						<option value="unassigned">{{ t('capture.unassigned') }}</option>
						<option v-for="account in accounts" :key="account.id" :value="account.id">{{ account.name }}</option>
					</select>
				</div>
				<div>
					<label class="label" for="f-outcome">{{ t('captures.filter.outcome') }}</label>
					<select id="f-outcome" v-model="filters.outcome" class="input">
						<option value="">{{ t('captures.filter.any') }}</option>
						<option value="ok">ok</option>
						<option value="error">error</option>
						<option value="aborted">aborted</option>
						<option value="pending">pending</option>
					</select>
				</div>
				<div>
					<label class="label" for="f-transport">{{ t('captures.filter.transport') }}</label>
					<select id="f-transport" v-model="filters.transport" class="input">
						<option value="">{{ t('captures.filter.any') }}</option>
						<option value="sse">sse</option>
						<option value="ws">ws</option>
					</select>
				</div>
				<div class="lg:col-span-2">
					<label class="label" for="f-search">{{ t('captures.filter.search') }}</label>
					<input id="f-search" v-model="filters.search" class="input" :placeholder="t('captures.filter.searchPlaceholder')" />
				</div>
			</div>

			<!-- Table -->
			<div class="card overflow-hidden">
				<div class="card-heading">
					<h2 class="text-[13px] text-[color:var(--color-ink-muted)]">
						{{ t('captures.count', { count: total }) }}
						<span class="text-[color:var(--color-ink-faint)]">· {{ t('captures.newestFirst') }}</span>
					</h2>
					<div class="flex items-center gap-2 text-[12px] text-[color:var(--color-ink-faint)]">
						<button class="btn btn-ghost" :disabled="page <= 1" @click="goToPage(page - 1)">{{ t('captures.prev') }}</button>
						<span>{{ t('captures.page', { page, total: pageCount }) }}</span>
						<button class="btn btn-ghost" :disabled="page >= pageCount" @click="goToPage(page + 1)">{{ t('captures.next') }}</button>
					</div>
				</div>

				<div v-if="captures.length" class="overflow-x-auto">
					<table class="w-full">
						<thead>
							<tr>
								<th class="th">ID</th>
								<th class="th">{{ t('keys.col.lastUsed') }}</th>
								<th class="th">{{ t('accounts.col.account') }}</th>
								<th class="th">{{ t('settings.defaultModel') }}</th>
								<th class="th">{{ t('captures.filter.transport') }}</th>
								<th class="th">{{ t('capture.status') }}</th>
								<th class="th">{{ t('captures.filter.outcome') }}</th>
								<th class="th text-right">{{ t('capture.duration') }}</th>
								<th class="th text-right">{{ t('capture.tokens') }}</th>
								<th class="th text-right">{{ t('capture.requestSize') }}</th>
							</tr>
						</thead>
						<tbody>
							<CaptureRow v-for="capture in displayCaptures" :key="capture.id" :capture="capture" @click="openCapture(capture)" />
						</tbody>
					</table>
				</div>

				<output v-else class="empty-state" >
					<Activity aria-hidden="true" />
					<span style="display: block">{{ loading ? t('stats.loading') : t('captures.empty') }}</span>
					<span style="display: block" v-if="!loading">{{ t('captures.emptyHint') }}</span>
				</output>
			</div>

			<div class="flex flex-wrap items-center justify-between gap-3 text-[12px] text-[color:var(--color-ink-faint)]">
				<span>{{ t('captures.hint') }}</span>
				<RouterLink to="/settings" class="hover:text-[color:var(--color-ink)]">{{ t('captures.adjustRetention') }}</RouterLink>
			</div>
		</div>
	</div>
</template>
