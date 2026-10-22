<script setup lang="ts">
import OfficialUsagePanel from '../components/usage/OfficialUsagePanel.vue'
import { copyText as copyToClipboard } from "../utils/clipboard"
import {useId, computed, onMounted, onUnmounted, ref} from 'vue'
import { ApiError, api } from '../api/client'
import type { Account, AccountGroup, OAuthFlow, SavedProxy, Settings } from '../api/types'
import { useToastStore, formatRelative, formatTime } from '../stores/ui'
import { useI18n } from '../i18n'
import PageHeader from '../components/PageHeader.vue'
import Badge from '../components/Badge.vue'
import Modal from '../components/Modal.vue'
import QuotaPanel from '../components/QuotaPanel.vue'
import AccountOverviewCards from '../components/accounts/AccountOverviewCards.vue'
import AccountsTable from '../components/accounts/AccountsTable.vue'
import AccountManagementTools from '../components/accounts/AccountManagementTools.vue'
import AccountProxyField from '../components/accounts/AccountProxyField.vue'
import AccountModelTestModal from '../components/accounts/AccountModelTestModal.vue'
import GroupManagerModal from '../components/accounts/GroupManagerModal.vue'
import ModelRestrictionSelector from '../components/accounts/ModelRestrictionSelector.vue'
import { useAccountControls } from '../components/accounts/accountControlsLocale'
import { transportLabel } from '../utils/uiOptions'
import { Check, Copy, ExternalLink, Users } from 'lucide-vue-next'

const toast = useToastStore()
const formId = useId()
const { t } = useI18n()
const { c } = useAccountControls()
const settings = ref<Settings | null>(null)
const groupManagerOpen = ref(false)
const modelTestTarget = ref<Account | null>(null)
function accountProtocolDefaultLabel(globalValue?: string) {
	return globalValue ? c('protocolDefault', { value: transportLabel(globalValue) }) : c('settingsFailed')
}

const accounts = ref<Account[]>([])
const groups = ref<AccountGroup[]>([])
const selectedIds = ref<number[]>([])
const batchBusy = ref(false)
const proxies = ref<SavedProxy[]>([])
const groupsLoaded = ref(false)
const proxiesLoaded = ref(false)
const loaded = ref(false)
const loading = ref(false)
const loadError = ref('')
const groupError = ref('')
const proxyError = ref('')

// ---- OAuth wizard state ----
// kind "chatgpt" adds a model-access account; kind "codex" links the optional
// Codex credential for quota reads and catalog sync; generation still uses ChatGPT.
const wizardOpen = ref(false)
const wizardKind = ref<'chatgpt' | 'codex'>('chatgpt')
const wizardAccountId = ref('')
const wizardAccountName = ref('')
const wizardName = ref('')
const wizardProxy = ref('')
const wizardProxyMode = ref('default')
const starting = ref(false)
const flow = ref<OAuthFlow | null>(null)
const pasteInput = ref('')
const submitting = ref(false)
const flowError = ref('')
const flowStopped = ref(false)
let pollTimer: number | undefined
let pollingVersion = 0

// ---- edit modal state ----
const editing = ref<Account | null>(null)
const editProxy = ref('')
const editProxyMode = ref('keep')
const editCooldown429 = ref(-1)
const editConcurrency = ref(3)
const editWeight = ref(1)
const editName = ref('')
const editProtocol = ref('')
const editGroupIds = ref<number[]>([])
const editDisabledModels = ref<string[]>([])
const editSupplementalText = ref('')
const editSupplementalModels = computed(() => [...new Set(editSupplementalText.value.split(/\r?\n/).map(value => value.trim()).filter(Boolean))])
const editSaving = ref(false)
const editInherited = computed<Account['inherited_model_restrictions']>(() => {
	if (!groupsLoaded.value) { return editing.value?.inherited_model_restrictions ?? [] }
	const byModel = new Map<string, string[]>()
	for (const group of groups.value) {
		if (!group.enabled || !editGroupIds.value.includes(group.id)) { continue }
		for (const model of group.disabled_models ?? []) byModel.set(model, [...(byModel.get(model) ?? []), group.name])
	}
	return [...byModel].map(([model, group_names]) => ({ model, group_names }))
})
const missingEditGroups = computed(() => editGroupIds.value.filter(id => !groups.value.some(group => group.id === id)))

// ---- quota state ----
const quotaTarget = ref<Account | null>(null)
const quotaBusy = ref(false)

async function load() {
	loading.value = true
	loadError.value = ''
	try {
		const result = await api.listAccounts()
		accounts.value = result.accounts ?? []
		loaded.value = true
	} catch (err) {
		loadError.value = err instanceof Error ? err.message : t('stats.loadFailed')
		toast.error(loadError.value)
	} finally {
		loading.value = false
	}
}

async function loadGroups() {
	try { groups.value = (await api.listAccountGroups()).groups ?? []; groupsLoaded.value = true; groupError.value = '' }
	catch (err) { groupsLoaded.value = false; groupError.value = err instanceof Error ? err.message : t('stats.loadFailed') }
}

async function loadProxies() {
	try {
		const items: SavedProxy[] = []
		let page = 1
		let total = 0
		do {
			const result = await api.listProxies({ page, page_size: 100 })
			items.push(...(result.items ?? []))
			total = result.total
			if (!result.items?.length) { break }
			page++
		} while (items.length < total)
		proxies.value = items
		proxiesLoaded.value = true
		proxyError.value = ''
	} catch (err) { proxiesLoaded.value = false; proxyError.value = err instanceof Error ? err.message : t('stats.loadFailed') }
}

async function loadSettings() {
	try { settings.value = (await api.getSettings()).current }
	catch { settings.value = null }
}
async function reload() { await Promise.all([load(), loadGroups(), loadProxies(), loadSettings()]) }
async function groupsChanged() { await Promise.all([load(), loadGroups()]) }
onMounted(reload)
onUnmounted(stopPolling)

// ---- quota ----

function openQuota(account: Account) {
	quotaTarget.value = account
}

async function refreshQuota(account: Account) {
	// Quota is only readable when the optional Codex credential is attached.
	if (!account.codex_linked) {
		toast.error(t('accounts.quotaNeedsCodex'))
		return
	}
	quotaBusy.value = true
	try {
		const result = await api.refreshQuota(account.id)
		applyQuota(account.id, result.quota)
		if (result.error) { toast.error(result.error) }
		else { toast.success(`${t('quota.title')} ✓`) }
	} catch (err) {
		toast.error(err instanceof Error ? err.message : t('quota.fetchFailed', { error: 'unknown' }))
	} finally {
		quotaBusy.value = false
	}
}

async function consumeReset(account: Account, creditId: string) {
	quotaBusy.value = true
	try {
		const result = await api.consumeResetCredit(account.id, creditId)
		applyQuota(account.id, result.quota)
		if (result.result) {
			toast.success(t('quota.useResetDone', { code: result.result.code }))
		} else if (result.error) {
			toast.error(result.error)
		}
	} catch (err) {
		toast.error(err instanceof Error ? err.message : 'reset failed')
	} finally {
		quotaBusy.value = false
	}
}

// applyQuota keeps both the list row and the open panel in sync.
function applyQuota(accountId: number, quota: Account['quota']) {
	accounts.value = accounts.value.map((a) => (a.id === accountId ? { ...a, quota } : a))
	if (quotaTarget.value?.id === accountId) {
		quotaTarget.value = { ...quotaTarget.value, quota }
	}
}

// ---- OAuth ----

const wizardTitle = computed(() =>
	wizardKind.value === 'codex' ? t('accounts.wizard.codexTitle') : t('accounts.wizard.browserTitle'),
)

const pasteHintKey = computed(() => {
	if (wizardKind.value === 'chatgpt') {
		return flow.value?.callback_available ? 'accounts.wizard.chatgptPasteHintLocal' : 'accounts.wizard.chatgptPasteHintRemote'
	}
	return flow.value?.callback_available ? 'accounts.wizard.codexPasteHintLocal' : 'accounts.wizard.codexPasteHintRemote'
})
const pastePlaceholderKey = computed(() =>
	wizardKind.value === 'chatgpt' ? 'accounts.wizard.chatgptPastePlaceholder' : 'accounts.wizard.codexPastePlaceholder',
)

// openWizard starts a ChatGPT login (kind="chatgpt") or, when an account is
// given, the Codex quota-credential link for that account (kind="codex").
function linkChatGPT(account: Account) { openWizard('chatgpt', account) }
function openWizard(kind: 'chatgpt' | 'codex', account?: Account) {
	resetFlow()
	wizardKind.value = kind
	wizardAccountId.value = account ? String(account.id) : ''
	wizardAccountName.value = account?.name ?? ''
	wizardOpen.value = true
	wizardName.value = ''
	wizardProxy.value = ''
	// Linking the optional Codex credential must not alter the account's existing egress.
	wizardProxyMode.value = account ? 'keep' : 'default'
	void loadProxies()
}

function resetFlow() {
	stopPolling()
	flow.value = null
	flowError.value = ''
	flowStopped.value = false
	pasteInput.value = ''
	starting.value = false
	submitting.value = false
}

function proxyPatch(mode: string, url: string): Partial<Pick<Account, 'proxy_id' | 'proxy_url'>> {
	if (mode === 'keep') { return {} }
	if (mode === 'default') { return { proxy_id: null, proxy_url: '' } }
	if (mode === 'manual') {
		if (!url.trim()) { throw new Error(t('accounts.proxy.urlRequired')) }
		return { proxy_id: null, proxy_url: url.trim() }
	}
	const id = Number(mode.replace('proxy:', ''))
	if (!Number.isSafeInteger(id) || !proxies.value.some(proxy => proxy.id === id)) { throw new Error(t('accounts.proxy.selectRequired')) }
	return { proxy_id: id }
}

async function startFlow() {
	if (starting.value) { return }
	stopPolling()
	const version = pollingVersion
	starting.value = true
	flowError.value = ''
	flowStopped.value = false
	try {
		const result = await api.startOAuth({
			kind: wizardKind.value,
			name: wizardName.value || undefined,
			...proxyPatch(wizardProxyMode.value, wizardProxy.value),
			account_id: wizardAccountId.value || undefined,
		})
		if (version !== pollingVersion || !wizardOpen.value) { return }
		flow.value = result.flow
		starting.value = false
		startPolling()
	} catch (err) {
		if (version !== pollingVersion) { return }
		flowError.value = err instanceof Error ? err.message : t('accounts.wizard.failed')
	} finally {
		if (version === pollingVersion) { starting.value = false }
	}
}

// finishFlow closes the wizard and refreshes the list after a completed flow.
async function finishFlow(account?: Account) {
	stopPolling()
	if (wizardKind.value === 'codex') {
		toast.success(t('accounts.codexLinkedDone', { name: wizardAccountName.value }))
	} else {
		toast.success(t('accounts.wizard.added', { name: account?.name ?? '' }))
	}
	wizardOpen.value = false
	await load()
	// Quota is only readable once a Codex credential is attached.
	if (wizardKind.value === 'chatgpt' && account && account.codex_linked) { void refreshQuota(account) }
}

function flowHasExpired(current: OAuthFlow): boolean {
	const expiresAt = Date.parse(current.expires_at)
	return Number.isFinite(expiresAt) && Date.now() >= expiresAt
}

function stopFlow(message: string) {
	stopPolling()
	flowStopped.value = true
	flowError.value = message
}

function markFlowExpired() {
	stopFlow(t('accounts.wizard.expired'))
}

function flowRequestExpired(err: unknown) {
    return (flow.value && flowHasExpired(flow.value)) || (err instanceof ApiError && (err.status === 404 || err.status === 410))
}

async function handleFlowResult(result: Awaited<ReturnType<typeof api.oauthStatus>>) {
    if (result.flow.status === 'completed') { await finishFlow(result.account); return }
    if (result.flow.status === 'failed') { stopFlow(result.flow.error ?? t('accounts.wizard.failed')); return }
    if (flowHasExpired(result.flow)) { markFlowExpired() }
}

function startPolling() {
	const version = pollingVersion
	let inFlight = false
	const poll = async () => {
		if (version !== pollingVersion || !flow.value) { return }
		// Check the deadline even if an earlier request is still hanging.
		if (flowHasExpired(flow.value)) {
			markFlowExpired()
			return
		}
		if (inFlight) { return }
		inFlight = true
		try {
			const result = await api.oauthStatus(flow.value.id)
			if (version !== pollingVersion) { return }
			flow.value = result.flow
			await handleFlowResult(result)
		} catch (err) {
			if (version !== pollingVersion) { return }
			if (flowRequestExpired(err)) {
				markFlowExpired()
			} else if (!(err instanceof TypeError)) {
				// Fetch network failures are TypeErrors. HTTP and parsing errors are terminal.
				stopFlow(err instanceof Error ? err.message : t('accounts.wizard.failed'))
			}
		} finally {
			inFlight = false
		}
	}
	pollTimer = window.setInterval(() => void poll(), 1500)
	void poll()
}

function stopPolling() {
	if (pollTimer !== undefined) { window.clearInterval(pollTimer) }
	pollTimer = undefined
	// Ignore late responses from a closed, expired or superseded flow.
	pollingVersion++
}

async function submitPastedCode() {
	if (!flow.value || flowStopped.value || submitting.value || !pasteInput.value.trim()) { return }
	if (flowHasExpired(flow.value)) {
		markFlowExpired()
		return
	}
	const version = pollingVersion
	submitting.value = true
	flowError.value = ''
	try {
		const result = await api.completeOAuth(flow.value.id, pasteInput.value.trim())
		if (version !== pollingVersion) { return }
		flow.value = result.flow
		await handleFlowResult(result)
	} catch (err) {
		if (version !== pollingVersion) { return }
		if (flowRequestExpired(err)) {
			markFlowExpired()
		} else {
			flowError.value = err instanceof Error ? err.message : t('accounts.wizard.failed')
		}
	} finally {
		if (version === pollingVersion) { submitting.value = false }
	}
}

function closeWizard() {
	stopPolling()
	wizardOpen.value = false
	flow.value = null
}

// ---- row actions ----

async function toggleEnabled(account: Account) {
	try {
		await api.updateAccount(account.id, { enabled: !account.enabled })
		await load()
	} catch (err) {
		toast.error(err instanceof Error ? err.message : 'update failed')
	}
}

async function refresh(account: Account) {
	try {
		await api.refreshAccount(account.id)
		toast.success(t('accounts.refreshed', { name: account.name }))
		await load()
	} catch (err) {
		toast.error(err instanceof Error ? err.message : 'refresh failed')
	}
}

async function recover(account: Account) {
	try {
		await api.recoverAccount(account.id)
		toast.success(c('recovered', { name: account.name }))
		await load()
	} catch (err) {
		toast.error(err instanceof Error ? err.message : 'recovery failed')
	}
}

async function remove(account: Account) {
	if (!confirm(t('accounts.deleteConfirm', { name: account.name }))) { return }
	try {
		await api.deleteAccount(account.id)
		toast.success(t('common.delete'))
		await groupsChanged()
	} catch (err) {
		toast.error(err instanceof Error ? err.message : 'delete failed')
	}
}

// linkCodex opens the OAuth wizard in codex mode, bound to this account.
function linkCodex(account: Account) {
	openWizard('codex', account)
}

async function unlinkCodex(account: Account) {
	if (!confirm(t('accounts.unlinkCodexConfirm', { name: account.name }))) { return }
	try {
		await api.unlinkCodex(account.id)
		toast.success(t('accounts.unlinkCodexDone'))
		await load()
	} catch (err) {
		toast.error(err instanceof Error ? err.message : 'unlink failed')
	}
}

async function setProtocol(account: Account, value: string) {
	try {
		await api.updateAccount(account.id, { upstream_protocol: value })
		await load()
	} catch (err) {
		toast.error(err instanceof Error ? err.message : 'update failed')
	}
}

function openEdit(account: Account) {
	editing.value = account
	editName.value = account.name
	editProxy.value = ''
	editProxyMode.value = 'keep'
	void loadProxies()
	editCooldown429.value = account.cooldown_429_seconds ?? -1
	editConcurrency.value = account.concurrency
	editWeight.value = account.weight
	editProtocol.value = account.upstream_protocol ?? ''
	editGroupIds.value = [...(account.group_ids ?? groups.value.filter(group => group.account_ids?.includes(account.id)).map(group => group.id))]
	editDisabledModels.value = [...(account.disabled_models ?? [])]
	editSupplementalText.value = (account.supplemental_models ?? []).join('\n')
}

function setEditGroup(id: number, selected: boolean) {
	editGroupIds.value = selected ? [...new Set([...editGroupIds.value, id])] : editGroupIds.value.filter(value => value !== id)
}

async function saveEdit() {
	if (!editing.value || editSaving.value) { return }
	editSaving.value = true
	try {
		await api.updateAccount(editing.value.id, {
			name: editName.value.trim(),
			...proxyPatch(editProxyMode.value, editProxy.value),
			cooldown_429_seconds: editCooldown429.value,
			concurrency: editConcurrency.value,
			weight: editWeight.value,
			upstream_protocol: editProtocol.value,
			disabled_models: [...editDisabledModels.value],
			supplemental_models: [...editSupplementalModels.value],
			...(groupsLoaded.value ? { group_ids: [...editGroupIds.value] } : {}),
		})
		toast.success(t('accounts.edit.saved'))
		editing.value = null
		await groupsChanged()
	} catch (err) {
		toast.error(err instanceof Error ? err.message : c('requestFailed'))
	} finally { editSaving.value = false }
}

const copied = ref('')
async function copyText(value: string, tag: string) {
	try {
		await copyToClipboard(value)
		copied.value = tag
		setTimeout(() => (copied.value = ''), 1500)
	} catch {
		toast.error(t('common.clipboardBlocked'))
	}
}
</script>

<template>
	<div class="page-view">
		<PageHeader :title="t('accounts.title')">
			<template #actions><button type="button" class="btn btn-ghost group-manager-button" @click="groupManagerOpen = true"><Users :size="15" />{{ c('manageGroups') }}</button></template>
		</PageHeader>
		<div class="page-content accounts-content" :aria-busy="loading">
			<AccountOverviewCards :accounts="accounts" :loaded="loaded" />
			<div v-if="loadError" class="notice notice-error" role="alert">{{ loadError }}</div>
			<div v-if="groupError" class="notice notice-warning" role="alert">{{ t('accounts.groups') }}: {{ groupError }}</div>
			<AccountManagementTools :accounts="accounts" :groups="groups" :groups-loaded="groupsLoaded" :selected-ids="selectedIds" @changed="groupsChanged" @busy="batchBusy = $event" @clear="selectedIds = []" />
			<AccountsTable v-model:selected-ids="selectedIds" :batch-busy="batchBusy" :accounts="accounts" :groups="groups" :groups-loaded="groupsLoaded" :loading="loading" :quota-busy="quotaBusy" :settings="settings"
				@refresh="reload" @create="openWizard('chatgpt')" @edit="openEdit" @delete="remove" @refresh-token="refresh" @recover="recover"
				@quota="openQuota" @refresh-quota="refreshQuota" @link="linkCodex" @link-chatgpt="linkChatGPT" @unlink="unlinkCodex" @toggle="toggleEnabled" @protocol="setProtocol"
				@test="account => { modelTestTarget = account }" />
		</div>

		<AccountModelTestModal v-if="modelTestTarget" :key="modelTestTarget.id" :account="modelTestTarget" :settings="settings" :proxies="proxies" @close="modelTestTarget = null" />
		<GroupManagerModal :open="groupManagerOpen" :accounts="accounts" :groups="groups" :loaded="groupsLoaded" :error="groupError" @close="groupManagerOpen = false" @changed="groupsChanged" />

		<!-- Quota detail -->
		<Modal
			:open="!!quotaTarget"
			:title="quotaTarget ? `${t('quota.title')} · ${quotaTarget.name}` : t('quota.title')"
			width="max-w-2xl"
			@close="quotaTarget = null"
		>
			<QuotaPanel
				v-if="quotaTarget"
				:quota="quotaTarget.quota"
				:account-name="quotaTarget.name"
				:busy="quotaBusy"
				:codex-linked="quotaTarget.codex_linked"
				@refresh="refreshQuota(quotaTarget)"
				@consume="(creditId: string) => consumeReset(quotaTarget!, creditId)"
				@link-codex="linkCodex(quotaTarget)"
			/>
			<OfficialUsagePanel v-if="quotaTarget?.codex_linked" :key="quotaTarget.id" :accounts="accounts" :account-id="quotaTarget.id" />
		</Modal>

		<!-- OAuth wizard -->
		<Modal :open="wizardOpen" :title="wizardTitle" width="max-w-2xl" @close="closeWizard">
			<div v-if="!flow" class="space-y-4">
				<p v-if="wizardKind === 'codex'" class="text-[12px] text-[color:var(--color-ink-muted)]">
					{{ t('accounts.wizard.codexBody', { name: wizardAccountName }) }}
				</p>
				<div class="grid gap-4 sm:grid-cols-2">
					<div v-if="wizardKind === 'chatgpt'">
						<label class="label" for="wiz-name">{{ t('accounts.wizard.name') }}</label>
						<input id="wiz-name" v-model="wizardName" class="input" :placeholder="t('accounts.wizard.namePlaceholder')" />
					</div>
					<div>
						<label class="label" for="wiz-proxy">{{ t('accounts.wizard.proxy') }}</label>
						<AccountProxyField id="wiz-proxy" v-model:mode="wizardProxyMode" v-model:url="wizardProxy" :proxies="proxies" :loaded="proxiesLoaded" :error="proxyError" :keep="Boolean(wizardAccountId)" :current-display="accounts.find(account => String(account.id) === wizardAccountId)?.proxy_display" />
					</div>
				</div>
				<p v-if="flowError" class="text-[12px] text-[color:var(--color-danger)]">{{ flowError }}</p>
				<div class="flex justify-end gap-2">
					<button class="btn" @click="closeWizard">{{ t('accounts.wizard.cancel') }}</button>
					<button class="btn btn-primary" :disabled="starting" @click="startFlow">
						{{ starting ? t('accounts.wizard.starting') : t('accounts.wizard.start') }}
					</button>
				</div>
			</div>

			<div v-else class="space-y-4">
				<div class="rounded-lg border border-[color:var(--color-line)] bg-[color:var(--color-canvas)] p-3">
					<div class="mb-1.5 flex items-center justify-between">
						<span class="text-[11px] font-medium tracking-wide text-[color:var(--color-ink-faint)] uppercase">
							{{ t('accounts.wizard.step1') }}
						</span>
						<button class="btn btn-ghost !px-1.5 !py-0.5" @click="copyText(flow.auth_url, 'url')">
							<component :is="copied === 'url' ? Check : Copy" class="h-3.5 w-3.5" />
						</button>
					</div>
					<p class="font-mono text-[11px] break-all text-[color:var(--color-ink-muted)]">{{ flow.auth_url }}</p>
					<div class="mt-2 flex items-center gap-2">
						<a :href="flow.auth_url" target="_blank" rel="noopener" class="btn">
							<ExternalLink class="h-3.5 w-3.5" />
							{{ t('accounts.wizard.openTab') }}
						</a>
						<Badge :tone="flow.callback_available ? 'success' : 'warn'">
							{{ flow.callback_available ? t('accounts.wizard.callbackListening') : t('accounts.wizard.callbackBusy') }}
						</Badge>
					</div>
				</div>

				<div class="rounded-lg border border-[color:var(--color-line)] bg-[color:var(--color-canvas)] p-3">
					<div class="mb-1.5 text-[11px] font-medium tracking-wide text-[color:var(--color-ink-faint)] uppercase">
						{{ t('accounts.wizard.step2') }}
					</div>
					<p class="mb-2 text-[12px] text-[color:var(--color-ink-muted)]">
						{{ t(pasteHintKey) }}
					</p>
					<p v-if="wizardKind === 'chatgpt'" class="mb-2 text-[11px] text-[color:var(--color-ink-faint)]">
						{{ t('accounts.wizard.redirectUri', { redirect: flow.redirect_uri }) }}
					</p>
					<label class="sr-only" :for="`${formId}-paste`">{{ t(pastePlaceholderKey) }}</label>
					<textarea
						:id="`${formId}-paste`"
						v-model="pasteInput"
						class="input font-mono"
						rows="3"
						:placeholder="t(pastePlaceholderKey)"
						:disabled="flowStopped"
					/>
					<p v-if="flowError" class="mt-2 text-[12px] text-[color:var(--color-danger)]">{{ flowError }}</p>
					<div class="mt-2 flex items-center justify-between">
						<span v-if="!flowStopped" class="text-[11px] text-[color:var(--color-ink-faint)]">
							<template v-if="flow.status === 'exchanging'">{{ t('accounts.wizard.exchanging') }}</template>
							<template v-else-if="flow.status === 'failed'">{{ t('accounts.wizard.failed') }}</template>
							<template v-else>{{ t('accounts.wizard.waiting') }}</template>
						</span>
						<button v-if="flowStopped" class="btn btn-primary" @click="resetFlow">
							{{ t('accounts.wizard.restart') }}
						</button>
						<button v-else class="btn btn-primary" :disabled="submitting || !pasteInput.trim()" @click="submitPastedCode">
							{{ submitting ? t('accounts.wizard.completing') : t('accounts.wizard.complete') }}
						</button>
					</div>
				</div>
			</div>
		</Modal>

		<!-- Edit modal -->
		<Modal :open="!!editing" :title="t('accounts.edit.title')" width="max-w-2xl" @close="!editSaving && (editing = null)">
			<div v-if="editing" class="space-y-4">
				<div>
					<label class="label" for="edit-name">{{ t('accounts.edit.name') }}</label>
					<input id="edit-name" v-model="editName" class="input" />
				</div>
				<div>
					<label class="label" for="edit-proxy">{{ t('accounts.edit.proxy') }}</label>
					<AccountProxyField id="edit-proxy" v-model:mode="editProxyMode" v-model:url="editProxy" :proxies="proxies" :loaded="proxiesLoaded" :error="proxyError" keep :current-display="editing.proxy_display" />
					<p class="account-control-hint">{{ c('proxyManagementHint') }}</p>
				</div>
				<div class="grid grid-cols-2 gap-4">
					<div>
						<label class="label" for="edit-conc">{{ t('accounts.edit.concurrency') }}</label>
						<input id="edit-conc" v-model.number="editConcurrency" type="number" min="1" class="input" />
					</div>
					<div>
						<label class="label" for="edit-weight">{{ t('accounts.edit.weight') }}</label>
						<input id="edit-weight" v-model.number="editWeight" type="number" min="1" class="input" />
					</div>
				</div>
				<div>
					<label class="label" for="edit-cooldown-429">{{ t('retry429.cooldown') }}</label>
					<input id="edit-cooldown-429" v-model.number="editCooldown429" type="number" min="-1" max="604800" step="1" class="input" />
					<p class="account-control-hint">{{ t('retry429.accountHint') }}</p>
				</div>
				<div>
					<label class="label" for="edit-protocol">{{ t('accounts.protocol.label') }}</label>
					<select id="edit-protocol" v-model="editProtocol" class="input">
						<option value="">{{ accountProtocolDefaultLabel(settings?.upstream_transport) }}</option>
						<option value="sse">{{ transportLabel('sse') }}</option>
						<option value="ws">{{ transportLabel('ws') }}</option>
					</select>
					<p class="account-control-hint">{{ c('protocolChain') }}</p>
				</div>
				<fieldset class="account-membership" :disabled="!groupsLoaded || editSaving"><legend>{{ c('membership') }}</legend><label v-for="group in groups" :key="group.id"><input type="checkbox" :checked="editGroupIds.includes(group.id)" @change="setEditGroup(group.id, ($event.target as HTMLInputElement).checked)" /><span>{{ group.name }}<small v-if="!group.enabled">{{ c('disabled') }}</small></span></label><span v-for="id in missingEditGroups" :key="id" class="account-control-hint">{{ c('missingGroup') }}</span><p v-if="!groupsLoaded" class="account-control-hint">{{ c('groupLoadFailed') }}</p><p v-else-if="!editGroupIds.length" class="account-control-hint">{{ c('noMembership') }}</p></fieldset>
				<div>
					<label class="label" for="edit-supplemental-models">{{ c('supplementalModels') }}</label>
					<textarea id="edit-supplemental-models" v-model="editSupplementalText" class="input font-mono" rows="3" placeholder="gpt-6.1-sol" :disabled="editSaving" aria-describedby="supplemental-models-hint" spellcheck="false" />
					<p id="supplemental-models-hint" class="account-control-hint">{{ c('supplementalHint') }}</p>
				</div>
				<p class="account-control-hint">{{ c('rules') }}</p>
				<ModelRestrictionSelector :key="editing.id" v-model="editDisabledModels" :sources="[editing]" :inherited="editInherited" :disabled="editSaving" :supplemental-models="editSupplementalModels" />
				<dl class="account-edit-details rounded-lg border border-[color:var(--color-line)] bg-[color:var(--color-canvas)] p-3 text-[12px]">
					<div class="flex justify-between py-0.5">
						<dt class="text-[color:var(--color-ink-faint)]">{{ t('accounts.edit.accountId') }}</dt>
						<dd class="font-mono">{{ editing.account_id || '—' }}</dd>
					</div>
					<div class="flex justify-between py-0.5">
						<dt class="text-[color:var(--color-ink-faint)]">{{ t('accounts.edit.email') }}</dt>
						<dd>{{ editing.email || '—' }}</dd>
					</div>
					<div class="flex justify-between py-0.5">
						<dt class="text-[color:var(--color-ink-faint)]">{{ t('accounts.edit.tokenExpires') }}</dt>
						<dd>{{ formatTime(editing.expires_at) }}</dd>
					</div>
					<div class="flex justify-between py-0.5">
						<dt class="text-[color:var(--color-ink-faint)]">{{ t('accounts.edit.lastRefresh') }}</dt>
						<dd>{{ formatRelative(editing.last_refresh_at) }}</dd>
					</div>
				</dl>
			</div>
			<template #footer>
				<button class="btn" :disabled="editSaving" @click="editing = null">{{ t('common.cancel') }}</button>
				<button class="btn btn-primary" :disabled="editSaving" @click="saveEdit">{{ editSaving ? c('saving') : t('common.save') }}</button>
			</template>
		</Modal>
	</div>
</template>

<style scoped>
.accounts-content { display: grid; min-width: 0; gap: 16px; }
.group-manager-button { min-width: 0; font-size: 12px; }
.account-control-hint { margin: 7px 0 0; font-size: 11px; line-height: 1.6; color: var(--color-ink-faint); overflow-wrap: anywhere; }
.account-membership { display: flex; flex-wrap: wrap; gap: 10px 14px; min-width: 0; margin: 0; padding: 12px; border: 1px solid var(--color-line); border-radius: 9px; }
.account-membership legend { padding: 0 5px; font-size: 12px; font-weight: 600; }
.account-membership label { display: flex; align-items: flex-start; gap: 7px; min-width: 0; font-size: 12px; cursor: pointer; }
.account-membership input { flex-shrink: 0; margin-top: 3px; accent-color: var(--color-accent); }
.account-membership label span { overflow-wrap: anywhere; }
.account-membership small { margin-left: 5px; color: var(--color-ink-faint); }
.account-membership > p { flex-basis: 100%; }
.account-edit-details > div { gap: 10px; flex-wrap: wrap; }
.account-edit-details dd { min-width: 0; overflow-wrap: anywhere; text-align: right; }
@media (max-width: 390px) { .group-manager-button { padding-inline: 8px; } }
</style>
