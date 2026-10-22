<script setup lang="ts">
import { copyText } from "../utils/clipboard"
import { computed, onMounted, ref } from 'vue'
import { api } from '../api/client'
import type { AccountGroup, ApiKey, SettingsResponse } from '../api/types'
import { strategyDescription, strategyLabel, strategyValues, transportDescription, transportLabel, transportValues } from '../utils/uiOptions'
import { useToastStore, formatRelative } from '../stores/ui'
import { useI18n } from '../i18n'
import PageHeader from '../components/PageHeader.vue'
import Badge from '../components/Badge.vue'
import Modal from '../components/Modal.vue'
import Toggle from '../components/Toggle.vue'
import { Check, ChevronDown, Copy, KeyRound, Plus, RefreshCw, Search, Trash2 } from 'lucide-vue-next'

const toast = useToastStore()
const { t } = useI18n()

const keys = ref<ApiKey[]>([])
const groups = ref<AccountGroup[]>([])
const loading = ref(false)
const groupsFailed = ref(false)

const modalOpen = ref(false)
const editTarget = ref<ApiKey | null>(null)
const emptyForm = () => ({ name: '', label: '', strategy: '', transport: '', max_concurrency: 0, requests_per_minute: 0, daily_limit_usd: 0, weekly_limit_usd: 0, group_ids: [] as number[] })
const form = ref(emptyForm())
const saving = ref(false)

const copied = ref<number | null>(null)
const gatewayOrigin = window.location.origin
const websocketOrigin = gatewayOrigin.replace(/^http/, 'ws')
const defaultModel = ref('YOUR_MODEL')
const requestExample = computed(() => JSON.stringify({
	model: defaultModel.value,
	input: [{ role: 'user', content: [{ type: 'input_text', text: 'Hi' }] }],
	stream: true,
	store: false,
}, null, 2))
const shellRequestExample = computed(() => requestExample.value.replaceAll("'", String.raw`'\''`))
const search = ref('')
const statusFilter = ref('')
const visibleKeys = computed(() => {
	const query = search.value.trim().toLowerCase()
	return keys.value.filter(key => (!query || `${key.name} ${key.label ?? ''}`.toLowerCase().includes(query))
		&& (!statusFilter.value || key.enabled === (statusFilter.value === 'enabled')))
})

const settings = ref<SettingsResponse | null>(null)
const globalTransport = computed(() => settings.value?.current.upstream_transport ?? '')
const globalStrategy = computed(() => settings.value?.current.default_strategy ?? '')
const inheritedTransport = computed(() => t('ui.options.inheritGlobal', { value: transportLabel(globalTransport.value) }))
const inheritedStrategy = computed(() => t('ui.options.inheritGlobal', { value: strategyLabel(globalStrategy.value) }))
const transportOptions = computed(() => [
	{ value: '', label: inheritedTransport.value },
	...(settings.value?.transports ?? transportValues).map(value => ({ value, label: transportLabel(value) })),
])
const strategyOptions = computed(() => [
	{ value: '', label: inheritedStrategy.value },
	...(settings.value?.strategies ?? strategyValues).map(value => ({ value, label: strategyLabel(value) })),
])
const selectedTransportDescription = computed(() => transportDescription(form.value.transport || globalTransport.value))
const selectedStrategyDescription = computed(() => strategyDescription(form.value.strategy || globalStrategy.value))
const missingGroupIds = computed(() => form.value.group_ids.filter(id => !groups.value.some(group => group.id === id)))

async function load() {
	loading.value = true
	try {
		groupsFailed.value = false
		const [keyResult, groupResult, settingsResult] = await Promise.all([
			api.listKeys(), api.listAccountGroups().catch(() => { groupsFailed.value = true; return null }),
			api.getSettings().catch(() => null),
		])
		keys.value = keyResult.keys ?? []
		groups.value = groupResult?.groups ?? []
		settings.value = settingsResult
		defaultModel.value = settingsResult?.current.default_model || 'YOUR_MODEL'
	} catch (err) {
		toast.error(err instanceof Error ? err.message : 'failed to load API keys')
	} finally {
		loading.value = false
	}
}

onMounted(load)

function openCreate() {
	editTarget.value = null
	form.value = emptyForm()
	modalOpen.value = true
}

function openEdit(key: ApiKey) {
	editTarget.value = key
	form.value = {
		name: key.name,
		label: key.label ?? '',
		max_concurrency: key.max_concurrency ?? 0,
		requests_per_minute: key.requests_per_minute ?? 0,
		daily_limit_usd: key.daily_limit_usd ?? 0,
		weekly_limit_usd: key.weekly_limit_usd ?? 0,
		group_ids: [...(key.group_ids ?? [])],
		strategy: key.strategy,
		transport: key.transport,
	}
	modalOpen.value = true
}

async function save() {
	const values = [form.value.max_concurrency, form.value.requests_per_minute, form.value.daily_limit_usd, form.value.weekly_limit_usd]
	if (values.some(value => typeof value !== 'number' || !Number.isFinite(value) || value < 0)
		|| !Number.isInteger(form.value.max_concurrency) || !Number.isInteger(form.value.requests_per_minute)) {
		toast.error(t('keys.field.invalidLimits'))
		return
	}
	if (groupsFailed.value || loading.value) { toast.error(t('keys.field.groupsFailed')); return }
	saving.value = true
	try {
		const payload = {
			name: form.value.name,
			label: form.value.label,
			max_concurrency: form.value.max_concurrency,
			requests_per_minute: form.value.requests_per_minute,
			daily_limit_usd: form.value.daily_limit_usd,
			weekly_limit_usd: form.value.weekly_limit_usd,
			group_ids: form.value.group_ids,
			strategy: form.value.strategy,
			transport: form.value.transport,
		}
		if (editTarget.value) {
			await api.updateKey(editTarget.value.id, payload)
			toast.success(t('keys.saved'))
		} else {
			const result = await api.createKey(payload)
			toast.success(t('keys.created'))
			if (result.key?.key) { void copyKey(result.key.key, result.key.id) }
		}
		modalOpen.value = false
		await load()
	} catch (err) {
		toast.error(err instanceof Error ? err.message : 'save failed')
	} finally {
		saving.value = false
	}
}

async function toggleEnabled(key: ApiKey) {
	try {
		await api.updateKey(key.id, { enabled: !key.enabled })
		await load()
	} catch (err) {
		toast.error(err instanceof Error ? err.message : 'update failed')
	}
}

async function remove(key: ApiKey) {
	if (!confirm(t('keys.deleteConfirm', { name: key.name }))) { return }
	try {
		await api.deleteKey(key.id)
		toast.success(t('keys.deleted'))
		await load()
	} catch (err) {
		toast.error(err instanceof Error ? err.message : 'delete failed')
	}
}

async function copyKey(value: string, id: number) {
	try {
		await copyText(value)
		copied.value = id
		setTimeout(() => (copied.value = null), 1500)
	} catch {
		toast.error(t('common.clipboardBlocked'))
	}
}

function groupLabel(id: number) {
	return groups.value.find((group) => group.id === id)?.name ?? (groupsFailed.value || loading.value ? t('ui.options.unavailable') : t('keys.deletedGroup'))
}
</script>

<template>
	<div class="page-view">
		<PageHeader :title="t('keys.title')" :subtitle="t('keys.subtitle')">
			<button class="btn" :disabled="loading" @click="load">
				<RefreshCw class="h-3.5 w-3.5" />
				{{ t('common.refresh') }}
			</button>
			<button class="btn btn-primary" @click="openCreate">
				<Plus class="h-3.5 w-3.5" />
				{{ t('keys.new') }}
			</button>
		</PageHeader>

		<div class="page-content" :aria-busy="loading">
			<div class="card table-panel overflow-hidden">
				<div class="filter-toolbar">
					<div class="flex min-w-0 flex-wrap items-center gap-3">
						<label class="search-field"><Search aria-hidden="true" /><input v-model="search" class="input" :placeholder="t('keys.search')" :aria-label="t('keys.search')" /></label>
						<select v-model="statusFilter" class="input !w-auto" :aria-label="t('common.allStatuses')"><option value="">{{ t('common.allStatuses') }}</option><option value="enabled">{{ t('common.enabled') }}</option><option value="disabled">{{ t('common.disabled') }}</option></select>
					</div>
					<span class="text-xs text-[color:var(--color-ink-faint)]">{{ t('keys.count', { count: keys.length }) }}</span>
				</div>
				<div class="table-scroll">
					<table class="w-full">
						<thead>
							<tr>
								<th class="th">{{ t('keys.col.name') }}</th>
								<th class="th">{{ t('keys.col.key') }}</th>
								<th class="th">{{ t('keys.col.routes') }}</th>
								<th class="th">{{ t('keys.col.transport') }}</th>
								<th class="th">{{ t('keys.col.strategy') }}</th>
								<th class="th text-right">{{ t('keys.col.requests') }}</th>
								<th class="th">{{ t('keys.col.lastUsed') }}</th>
								<th class="th text-right">{{ t('keys.col.actions') }}</th>
							</tr>
						</thead>
						<tbody>
							<tr v-for="key in visibleKeys" :key="key.id" class="row-hover">
								<td class="td">
									<div class="flex items-center gap-2">
										<Toggle :model-value="key.enabled" @update:modelValue="toggleEnabled(key)" />
										<div class="min-w-0"><div class="font-medium">{{ key.name }}</div><div v-if="key.label" class="mt-0.5 text-xs text-[color:var(--color-ink-faint)]">{{ key.label }}</div></div>
									</div>
								</td>
								<td class="td">
									<div class="flex items-center gap-2">
										<code class="font-mono text-[11px] text-[color:var(--color-ink-muted)]">
											{{ key.key.slice(0, 14) }}…{{ key.key.slice(-4) }}
										</code>
										<button class="btn btn-ghost !px-1.5 !py-0.5" :title="t('common.copy')" @click="copyKey(key.key, key.id)">
											<component :is="copied === key.id ? Check : Copy" class="h-3.5 w-3.5" />
										</button>
									</div>
								</td>
								<td class="td">
									<Badge v-if="!key.group_ids?.length" tone="info">{{ t('keys.allAccounts') }}</Badge>
									<div v-else class="flex flex-wrap gap-1">
										<Badge v-for="id in key.group_ids" :key="id" tone="neutral">{{ groupLabel(id) }}</Badge>
									</div>
								</td>
								<td class="td"><div class="key-option"><Badge tone="neutral">{{ key.transport ? transportLabel(key.transport) : inheritedTransport }}</Badge><small>{{ transportDescription(key.transport || globalTransport) }}</small></div></td>
								<td class="td"><div class="key-option"><span>{{ key.strategy ? strategyLabel(key.strategy) : inheritedStrategy }}</span><small>{{ strategyDescription(key.strategy || globalStrategy) }}</small></div></td>
								<td class="td text-right font-mono text-[12px]">{{ key.request_count }}</td>
								<td class="td text-[color:var(--color-ink-muted)]">{{ formatRelative(key.last_used_at) }}</td>
								<td class="td">
									<div class="flex justify-end gap-1">
										<button class="btn btn-ghost" @click="openEdit(key)">{{ t('keys.edit') }}</button>
										<button class="btn btn-ghost !px-1.5" :title="t('common.delete')" @click="remove(key)">
											<Trash2 class="h-3.5 w-3.5 text-[color:var(--color-danger)]" />
										</button>
									</div>
								</td>
							</tr>
							<tr v-if="!visibleKeys.length">
								<td colspan="8"><output class="empty-state" ><KeyRound aria-hidden="true" /><span style="display: block">{{ (() => { if (loading) { return t('stats.loading'); } if (keys.length) { return t('common.noMatches'); } return t('keys.empty'); })() }}</span><button v-if="!loading && !keys.length" class="btn btn-primary mt-2" @click="openCreate"><Plus class="h-4 w-4" />{{ t('keys.new') }}</button></output></td>
							</tr>
						</tbody>
					</table>
				</div>
			</div>

			<!-- Client setup hint -->
			<details class="card mt-5 panel-content group">
				<summary class="flex items-center justify-between gap-3 text-sm font-semibold"><span>{{ t('keys.setupTitle') }}</span><ChevronDown class="h-4 w-4 text-[color:var(--color-ink-faint)] transition-transform group-open:rotate-180" /></summary>
				<p class="my-4 text-xs leading-relaxed text-[color:var(--color-ink-muted)]">{{ t('keys.setupHint') }}</p>
				<div class="grid gap-4 xl:grid-cols-2">
					<div class="code-panel"><div class="code-header">{{ t('keys.httpExample') }}</div><pre class="overflow-x-auto px-4 pb-4 font-mono text-xs leading-relaxed">curl -N --request POST "{{ gatewayOrigin }}/v1/responses" \
  -H "Authorization: Bearer YOUR_KEY" \
  -H "Content-Type: application/json" \
  --data-raw '{{ shellRequestExample }}'</pre></div>
					<div class="code-panel"><div class="code-header">{{ t('keys.compatExample') }}</div><pre class="overflow-x-auto px-4 pb-4 font-mono text-xs leading-relaxed">base_url: {{ gatewayOrigin }}/v1
api_key:  YOUR_KEY

{{ websocketOrigin }}/v1/responses</pre></div>
				</div>
			</details>
		</div>

		<Modal :open="modalOpen" :title="editTarget ? t('keys.editTitle') : t('keys.createTitle')" width="max-w-xl" @close="modalOpen = false">
			<div class="space-y-4">
				<div>
					<label class="label" for="key-name">{{ t('keys.field.name') }}</label>
					<input id="key-name" v-model="form.name" class="input" :placeholder="t('keys.field.namePlaceholder')" />
				</div>

				<div>
					<label class="label" for="key-label">{{ t('keys.field.label') }}</label>
					<input id="key-label" v-model="form.label" class="input" />
				</div>
				<div class="grid gap-3 sm:grid-cols-2">
					<label><span class="label">{{ t('keys.field.maxConcurrency') }}</span><input v-model.number="form.max_concurrency" class="input" type="number" min="0" step="1" required /></label>
					<label><span class="label">{{ t('keys.field.requestsPerMinute') }}</span><input v-model.number="form.requests_per_minute" class="input" type="number" min="0" step="1" required /></label>
					<label><span class="label">{{ t('keys.field.dailyLimit') }}</span><input v-model.number="form.daily_limit_usd" class="input" type="number" min="0" step="0.000001" required /></label>
					<label><span class="label">{{ t('keys.field.weeklyLimit') }}</span><input v-model.number="form.weekly_limit_usd" class="input" type="number" min="0" step="0.000001" required /></label>
				</div>
				<p class="text-xs text-[color:var(--color-ink-muted)]">{{ t('keys.field.unlimitedHint') }}</p>
				<fieldset class="rounded-lg border border-[color:var(--color-line)] p-3">
					<legend class="px-1 text-xs">{{ t('keys.field.groups') }}</legend>
					<p class="mb-2 text-xs text-[color:var(--color-ink-muted)]">{{ t('keys.field.groupsHint') }}</p>
					<p v-if="groupsFailed" role="alert" class="text-xs text-[color:var(--color-danger)]">{{ t('keys.field.groupsFailed') }}</p>
					<p v-else-if="!groups.length" class="text-xs text-[color:var(--color-ink-faint)]">{{ t('keys.field.noGroups') }}</p>
					<label v-for="group in groups" :key="group.id" class="key-group-option">
						<input v-model="form.group_ids" type="checkbox" :value="group.id" class="accent-[color:var(--color-accent)]" /><span>{{ group.name }}</span>
					</label>
					<label v-for="id in missingGroupIds" :key="id" class="key-group-option">
						<input v-model="form.group_ids" type="checkbox" :value="id" :disabled="groupsFailed || loading" class="accent-[color:var(--color-accent)]" /><span>{{ groupLabel(id) }}</span>
					</label>
				</fieldset>


				<div class="key-policy-grid grid gap-4 sm:grid-cols-2">
					<div>
						<label class="label" for="key-transport">{{ t('keys.field.transport') }}</label>
						<select id="key-transport" v-model="form.transport" class="input" aria-describedby="key-transport-hint">
							<option v-for="option in transportOptions" :key="option.value" :value="option.value">{{ option.label }}</option>
							<option v-if="!transportOptions.some(option => option.value === form.transport)" :value="form.transport">{{ transportLabel(form.transport) }}</option>
						</select>
						<p id="key-transport-hint" class="key-option-hint">{{ selectedTransportDescription }}</p>
						<p class="key-option-hint">{{ t('ui.options.transportPriority') }}</p>
					</div>
					<div>
						<label class="label" for="key-strategy">{{ t('keys.field.strategy') }}</label>
						<select id="key-strategy" v-model="form.strategy" class="input" aria-describedby="key-strategy-hint">
							<option v-for="option in strategyOptions" :key="option.value" :value="option.value">{{ option.label }}</option>
							<option v-if="!strategyOptions.some(option => option.value === form.strategy)" :value="form.strategy">{{ strategyLabel(form.strategy) }}</option>
						</select>
						<p id="key-strategy-hint" class="key-option-hint">{{ selectedStrategyDescription }}</p>
						<p class="key-option-hint">{{ t('ui.options.strategyPriority') }}</p>
					</div>
				</div>
			</div>
			<template #footer>
				<button class="btn" @click="modalOpen = false">{{ t('common.cancel') }}</button>
				<button class="btn btn-primary" :disabled="saving || loading || groupsFailed || !form.name.trim()" @click="save">
					{{ (() => { if (saving) { return t('settings.saving'); } if (editTarget) { return t('common.save'); } return t('keys.create'); })() }}
				</button>
			</template>
		</Modal>
	</div>
</template>

<style scoped>
.key-option { display: grid; gap: 4px; min-width: 130px; max-width: 250px; white-space: normal; overflow-wrap: anywhere; }
.key-option > span { justify-self: start; max-width: 100%; white-space: normal; }
.key-option small, .key-option-hint { color: var(--color-ink-faint); font-size: 11px; line-height: 1.5; }
.key-option-hint { margin: 6px 0 0; overflow-wrap: anywhere; }
.key-policy-grid > div { min-width: 0; }
.key-policy-grid select { min-width: 0; max-width: 100%; text-overflow: ellipsis; }
.key-group-option { display: flex; min-width: 0; cursor: pointer; align-items: center; gap: 8px; padding: 4px 0; font-size: 13px; }
.key-group-option input { flex-shrink: 0; }
.key-group-option span { min-width: 0; overflow-wrap: anywhere; }
.code-panel { min-width: 0; }
@media (max-width: 639px) { .search-field { width: 100%; min-width: 0; } }
</style>
