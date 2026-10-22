<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ChevronDown, ChevronLeft, ChevronRight, Link2, Plus, Users } from 'lucide-vue-next'
import type { Account, AccountGroup, Settings } from '../../api/types'
import { metric } from '../../api/stats-format'
import { formatRelative, formatTime, formatUntil } from '../../stores/ui'
import { useI18n } from '../../i18n'
import Badge from '../Badge.vue'
import Toggle from '../Toggle.vue'
import AccountFilters from './AccountFilters.vue'
import AccountIdentityCell from './AccountIdentityCell.vue'
import AccountQuotaSummaryCell from './AccountQuotaSummaryCell.vue'
import AccountTableActions from './AccountTableActions.vue'
import { accountCategory } from './accountPresentation'
import { useAccountControls } from './accountControlsLocale'
import { useAccountManagementLocale } from './accountManagementLocale'
import { accountStatusLabel, transportLabel } from '../../utils/uiOptions'

const props = defineProps<{ accounts: Account[]; groups: AccountGroup[]; loading: boolean; groupsLoaded: boolean; quotaBusy: boolean; settings: Settings | null; selectedIds?: number[]; batchBusy?: boolean }>()
const emit = defineEmits<{ refresh: []; create: []; edit: [account: Account]; delete: [account: Account]; recover: [account: Account]; refreshToken: [account: Account]; quota: [account: Account]; refreshQuota: [account: Account]; link: [account: Account]; linkChatgpt: [account: Account]; unlink: [account: Account]; toggle: [account: Account]; test: [account: Account]; protocol: [account: Account, value: string]; 'update:selectedIds': [ids: number[]] }>()
const { t } = useI18n()
const { c } = useAccountControls()
const { m } = useAccountManagementLocale()
const protocolDefault = computed(() => c('protocolDefault', { value: props.settings?.upstream_transport ? transportLabel(props.settings.upstream_transport) : c('settingsFailed') }))
const search = ref('')
const status = ref('')
const group = ref('')
const page = ref(1)
const pageSize = ref(20)
const expanded = ref(new Set<number>())
const statuses = computed(() => [...new Set(props.accounts.map(account => account.status))].sort())
const groupMap = computed(() => {
	const map = new Map<number, Array<Pick<AccountGroup, 'id' | 'name' | 'enabled'>>>()
	for (const account of props.accounts) {
		const ids = account.group_ids ?? props.groups.filter(group => group.account_ids?.includes(account.id)).map(group => group.id)
		map.set(account.id, ids.map(id => props.groups.find(group => group.id === id) ?? { id, name: c('missingGroup'), enabled: false }))
	}
	return map
})
const filtered = computed(() => {
	const query = search.value.trim().toLowerCase()
	return props.accounts.filter(account => {
		if (status.value && !(status.value === 'disabled' ? !account.enabled : account.status === status.value)) { return false }
		const memberships = groupMap.value.get(account.id) ?? []
		if (group.value && !(group.value === 'ungrouped' ? !memberships.length : memberships.some(item => String(item.id) === group.value))) { return false }
		return !query || `${account.name} ${account.email} ${account.account_id}`.toLowerCase().includes(query)
	})
})
const pages = computed(() => Math.max(1, Math.ceil(filtered.value.length / pageSize.value)))
const visible = computed(() => filtered.value.slice((page.value - 1) * pageSize.value, page.value * pageSize.value))
const selected = computed(() => new Set(props.selectedIds ?? []))
const pageSelected = computed(() => visible.value.length > 0 && visible.value.every(a => selected.value.has(a.id)))
const pagePartial = computed(() => !pageSelected.value && visible.value.some(a => selected.value.has(a.id)))
function selectAccount(id: number, checked: boolean) {
	const ids = new Set(selected.value); checked ? ids.add(id) : ids.delete(id)
	emit('update:selectedIds', [...ids])
}
function selectPage(checked: boolean) {
	const ids = new Set(selected.value)
	for (const a of visible.value) checked ? ids.add(a.id) : ids.delete(a.id)
	emit('update:selectedIds', [...ids])
}
watch(() => props.accounts, accounts => {
	const existing = new Set(accounts.map(a => a.id))
	const ids = (props.selectedIds ?? []).filter(id => existing.has(id))
	if (ids.length !== (props.selectedIds ?? []).length) { emit('update:selectedIds', ids) }
})
watch([search, status, group, pageSize], () => { page.value = 1 })
watch(pages, value => { page.value = Math.min(page.value, value) })
function toggleDetails(id: number) { const next = new Set(expanded.value); next.has(id) ? next.delete(id) : next.add(id); expanded.value = next }
function tone(account: Account) { const category = accountCategory(account); return (() => {
    if (category === 'normal') {
        return 'success'
    }
    if (category === 'limited') {
        return 'warn'
    }
    if (category === 'disabled') {
        return 'neutral'
    }
    return 'danger'
})() }
</script>

<template>
	<section class="card accounts-table-card">
		<AccountFilters v-model:search="search" v-model:status="status" v-model:group="group" :groups="groups" :groups-loaded="groupsLoaded" :statuses="statuses" :loading="loading" @refresh="emit('refresh')" @create="emit('create')" />
		<div v-if="selected.size" class="filtered-selection"><span>{{ m('selected', {count: selected.size}) }}</span><button class="btn btn-ghost btn-sm" :disabled="batchBusy || filtered.length > 1000" @click="emit('update:selectedIds', filtered.map(a => a.id))">{{ m('selectFiltered', {count: filtered.length}) }}</button></div>
		<div class="accounts-table-scroll">
			<table class="accounts-table">
				<thead><tr><th class="th selection-cell"><input type="checkbox" :checked="pageSelected" :indeterminate="pagePartial" :disabled="batchBusy || !visible.length" :aria-label="m('selectPage')" @change="selectPage(($event.target as HTMLInputElement).checked)" /></th><th class="th expander-cell" /><th class="th identity-column">{{ t('accounts.col.account') }}</th><th class="th">{{ t('accounts.credentials') }}</th><th class="th">{{ t('accounts.col.status') }}</th><th class="th">{{ t('accounts.col.plan') }}</th><th class="th quota-column">{{ t('accounts.col.quota') }}</th><th class="th">{{ t('accounts.groups') }}</th><th class="th">{{ t('dashboard.recent') }}</th><th class="th actions-column">{{ t('accounts.col.actions') }}</th></tr></thead>
				<tbody>
					<template v-for="(account, index) in visible" :key="account.id">
						<tr class="account-row" :class="{ 'is-striped': index % 2 === 1 }">
							<td class="td selection-cell"><input type="checkbox" :checked="selected.has(account.id)" :disabled="batchBusy" :aria-label="m('select', {name: account.name})" @change="selectAccount(account.id, ($event.target as HTMLInputElement).checked)" /></td>
							<td class="td expander-cell"><button type="button" class="detail-toggle" :aria-expanded="expanded.has(account.id)" :aria-label="t(expanded.has(account.id) ? 'accounts.collapse' : 'accounts.details')" @click="toggleDetails(account.id)"><ChevronDown :size="14" :class="{ collapsed: !expanded.has(account.id) }" /></button></td>
							<td class="td identity-column"><AccountIdentityCell :account="account" /></td>
							<td class="td"><div class="credential-icons"><button v-if="account.has_chatgpt_credential === false" type="button" class="btn btn-sm" :title="m('awaitingChatGPT')" @click="emit('linkChatgpt', account)">{{ m('linkChatGPT') }}</button><span v-else class="credential-chip" title="ChatGPT OAuth">GPT</span><button type="button" class="credential-link" :class="{ linked: account.codex_linked }" :title="account.codex_linked ? t('accounts.codexLinked') : t('accounts.linkCodex')" :aria-label="account.codex_linked ? t('accounts.codexLinked') : t('accounts.linkCodex')" @click="account.codex_linked ? emit('quota', account) : emit('link', account)"><Link2 :size="14" /></button></div></td>
							<td class="td"><Badge :tone="tone(account)" :title="account.last_error || undefined"><span class="status-dot" />{{ account.enabled ? accountStatusLabel(account.status) : c('disabled') }}</Badge></td>
							<td class="td"><Badge tone="info">{{ account.plan_type || '—' }}</Badge></td>
							<td class="td quota-column"><AccountQuotaSummaryCell :account="account" :on-open="account => emit('quota', account)" :on-link="account => emit('link', account)" /></td>
							<td class="td"><div class="group-chips"><Badge v-for="item in groupMap.get(account.id) ?? []" :key="item.id" :tone="item.enabled ? 'neutral' : 'warn'">{{ item.name }}</Badge><span v-if="!groupMap.get(account.id)?.length" class="muted">{{ c('publicAccount') }}</span></div></td>
							<td class="td last-used"><span :title="formatTime(account.last_used_at)">{{ formatRelative(account.last_used_at) }}</span><small>{{ metric(account.request_count) }} {{ t('accounts.col.requests') }}</small></td>
							<td class="td actions-column"><AccountTableActions :account="account" :quota-busy="quotaBusy" @recover="emit('recover', $event)" @edit="emit('edit', $event)" @delete="emit('delete', $event)" @refresh="emit('refreshToken', $event)" @quota="emit('quota', $event)" @refresh-quota="emit('refreshQuota', $event)" @link="emit('link', $event)" @unlink="emit('unlink', $event)" @toggle="emit('toggle', $event)" @test="emit('test', $event)" /></td>
						</tr>
						<tr v-if="expanded.has(account.id)" class="detail-row"><td colspan="10"><div class="account-detail-grid">
							<div><span class="detail-label">{{ t('common.enabled') }}</span><Toggle :model-value="account.enabled" :aria-label="c(account.enabled ? 'disableAccount' : 'enableAccount')" @update:modelValue="emit('toggle', account)" /></div>
							<div><span class="detail-label">{{ t('accounts.col.token') }}</span><span :class="{ 'text-danger': account.token_expires_in_ms <= 0 }">{{ account.token_expires_in_ms > 0 ? formatUntil(account.token_expires_in_ms) : t('accounts.expired') }}</span><small v-if="!account.has_refresh_token">{{ t('accounts.noRefreshToken') }}</small></div>
							<div><span class="detail-label">{{ t('accounts.col.codex') }}</span><span>{{ t(account.codex_linked ? 'accounts.codexLinked' : 'accounts.codexNotLinked') }}</span><button class="detail-link" @click="account.codex_linked ? emit('unlink', account) : emit('link', account)">{{ t(account.codex_linked ? 'accounts.unlinkCodex' : 'accounts.linkCodex') }}</button></div>
							<div><span class="detail-label">{{ t('accounts.col.proxy') }}</span><span class="break-anywhere">{{ account.proxy_display || '—' }}</span><button class="detail-link" @click="emit('edit', account)">{{ t('accounts.edit') }}</button></div>
							<div><label class="detail-label" :for="`account-protocol-${account.id}`">{{ t('accounts.col.protocol') }}</label><select :id="`account-protocol-${account.id}`" class="input protocol-select" :title="c('protocolChain')" :value="account.upstream_protocol ?? ''" @change="emit('protocol', account, ($event.target as HTMLSelectElement).value)"><option value="">{{ protocolDefault }}</option><option value="sse">{{ transportLabel('sse') }}</option><option value="ws">{{ transportLabel('ws') }}</option></select></div>
							<div><span class="detail-label">{{ t('accounts.capacity') }}</span><span>{{ account.inflight }} / {{ account.concurrency }}</span><small>{{ t('accounts.edit.weight') }} {{ account.weight }}</small></div>
							<div><span class="detail-label">{{ t('accounts.col.requests') }}</span><span>{{ metric(account.request_count) }}</span><small class="text-danger">{{ t('dashboard.failed') }} {{ metric(account.error_count) }}</small></div>
							<div><span class="detail-label">{{ t('accounts.edit.lastRefresh') }}</span><span>{{ formatRelative(account.last_refresh_at) }}</span></div>
						</div><p v-if="account.last_error" class="account-error">{{ t('accounts.lastError') }}: {{ account.last_error }}</p></td></tr>
					</template>
				</tbody>
			</table>
			<output v-if="!visible.length" class="empty-state" ><Users :size="30" aria-hidden="true" /><span style="display: block">{{ (() => { if (loading) { return t('stats.loading'); } if (accounts.length) { return t('common.noMatches'); } return t('accounts.empty'); })() }}</span><button v-if="!loading && !accounts.length" class="btn btn-primary" @click="emit('create')"><Plus :size="16" />{{ t('accounts.add') }}</button></output>
		</div>
		<footer class="accounts-pagination"><span>{{ t('accounts.pagination', { total: filtered.length, page, pages }) }}</span><div><select v-model.number="pageSize" class="input page-size" :aria-label="t('accounts.pageSize')"><option v-for="size in [10, 20, 50, 100]" :key="size" :value="size">{{ size }} / {{ t('usage.pageSize') }}</option></select><button class="btn btn-ghost page-button" :disabled="page <= 1" :aria-label="t('usage.previous')" @click="page--"><ChevronLeft :size="15" /></button><span class="page-number">{{ page }}</span><button class="btn btn-ghost page-button" :disabled="page >= pages" :aria-label="t('usage.next')" @click="page++"><ChevronRight :size="15" /></button></div></footer>
	</section>
</template>

<style scoped>
.selection-cell { width: 36px; text-align: center; }
.selection-cell input { width: 15px; height: 15px; vertical-align: middle; accent-color: var(--color-brand); cursor: pointer; }
.filtered-selection { display: flex; flex-wrap: wrap; gap: 10px; align-items: center; padding: 0 24px 12px; font-size: 12px; }
.accounts-table-card { display: flex; min-width: 0; min-height: 520px; flex-direction: column; padding-bottom: 4px; }
.accounts-table-scroll { min-width: 0; min-height: 0; overflow: auto; flex: 1; margin: 0 24px; }
.accounts-table { width: 100%; border-collapse: separate; border-spacing: 0; font-size: 12px; }
.accounts-table .th { height: 40px; padding: 10px 12px; position: sticky; top: 0; z-index: 2; }
.accounts-table .th:first-child { border-radius: 8px 0 0 8px; }
.accounts-table .th:last-child { border-radius: 0 8px 8px 0; }
.account-row .td { height: 72px; border: 0; padding: 10px 12px; }
.account-row.is-striped { background: var(--color-row-stripe); }
.account-row:hover { background: var(--color-row-hover); }
.accounts-table .expander-cell { width: 32px; padding-left: 4px; padding-right: 4px; }
.identity-column { min-width: 250px; max-width: 340px; }
.quota-column { min-width: 270px; }
.actions-column { position: sticky; right: 0; z-index: 1; min-width: 112px; background: var(--color-surface); }
.account-row.is-striped .actions-column { background: var(--color-row-stripe); }
.account-row:hover .actions-column { background: var(--color-row-hover); }
.accounts-table .th.actions-column { z-index: 3; background: var(--color-surface-2); }
.detail-toggle { display: inline-flex; align-items: center; justify-content: center; width: 24px; height: 24px; border: 0; border-radius: 6px; background: transparent; color: var(--color-ink-muted); }
.detail-toggle:hover { background: var(--color-surface-2); }
.detail-toggle svg { transition: transform .15s; }.detail-toggle .collapsed { transform: rotate(-90deg); }
.credential-icons { display: flex; gap: 8px; align-items: center; }
.credential-chip, .credential-link { display: inline-flex; align-items: center; justify-content: center; width: 28px; height: 28px; border-radius: 7px; background: var(--color-surface-2); color: var(--color-ink-muted); font-size: 9px; font-weight: 750; border: 0; }
.credential-link.linked { color: var(--color-success); background: var(--color-success-soft); }
.status-dot { width: 5px; height: 5px; border-radius: 50%; background: currentColor; }
.group-chips { display: flex; min-width: 72px; max-width: 150px; flex-wrap: wrap; gap: 4px; }
.last-used { white-space: nowrap; color: var(--color-ink-muted); font-size: 11px; }
.last-used small { display: block; margin-top: 4px; color: var(--color-ink-faint); font-family: var(--font-mono); font-size: 10px; }
.muted { color: var(--color-ink-faint); }
.detail-row > td { padding: 20px; background: var(--color-surface-2); border-radius: 10px; }
.account-detail-grid { display: grid; grid-template-columns: repeat(4, minmax(120px, 1fr)); gap: 20px; }
.account-detail-grid > div { display: flex; flex-direction: column; align-items: flex-start; gap: 7px; }
.account-detail-grid .detail-label { font-size: 10px; color: var(--color-ink-faint); }
.account-detail-grid span { font-size: 12px; }
.account-detail-grid small { font-size: 10px; color: var(--color-ink-faint); }
.break-anywhere { overflow-wrap: anywhere; }
.protocol-select { min-height: 30px; padding: 4px 8px; font-size: 11px; }
.detail-link { padding: 0; background: transparent; border: 0; color: var(--color-accent); font-size: 11px; }
.text-danger, .account-error { color: var(--color-danger); }
.account-error { margin-top: 14px; font-size: 11px; overflow-wrap: anywhere; }
.accounts-pagination { display: flex; min-width: 0; justify-content: space-between; align-items: center; flex-wrap: wrap; gap: 12px; padding: 14px 24px; font-size: 11px; }
.accounts-pagination > span { min-width: 0; overflow-wrap: anywhere; }
.accounts-pagination > div { display: flex; min-width: 0; max-width: 100%; flex-wrap: wrap; gap: 8px; align-items: center; }
.page-size { width: 110px; min-height: 32px; padding: 5px 10px; font-size: 11px; }
.page-button { width: 32px; min-height: 32px; padding: 0; background: var(--color-surface-2); }
.page-number { display: inline-flex; width: 32px; height: 32px; justify-content: center; align-items: center; border-radius: 8px; color: var(--color-on-solid, var(--color-surface)); background: var(--color-accent-dim); font-family: var(--font-mono); }
@media (min-width: 1200px) { .accounts-table-card { height: calc(100dvh - 270px); min-height: 480px; } }
@media (max-width: 767px) { .accounts-table-scroll { margin-inline: 16px; } .accounts-pagination { padding-inline: 16px; } .account-detail-grid { grid-template-columns: repeat(2, minmax(120px, 1fr)); } }
</style>
