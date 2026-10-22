<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { RefreshCw, Send, Square } from 'lucide-vue-next'
import { ApiError, api } from '../../api/client'
import type { Account, AccountModelTestResult, SavedProxy, Settings } from '../../api/types'
import { formatTime } from '../../stores/ui'
import { transportLabel } from '../../utils/uiOptions'
import Modal from '../Modal.vue'
import { useAccountControls } from './accountControlsLocale'
import { isCodexOnlyModel, modelOriginNames, useAccountModelCatalog } from './useAccountModelCatalog'
import ModelCatalogSourceStatus from './ModelCatalogSourceStatus.vue'

const props = defineProps<{ account: Account; settings: Settings | null; proxies: SavedProxy[] }>()
const emit = defineEmits<{ close: [] }>()
const { c } = useAccountControls()
const { models: catalogModels, selectedCatalog, issues, loading: reading, refreshing, refresh, abort: abortCatalog } = useAccountModelCatalog(computed(() => [props.account]))
const model = ref('')
const message = ref('Hi')
const result = ref<AccountModelTestResult | null>(null)
const sending = ref(false)
const error = ref('')
const cancelled = ref(false)
let testController: AbortController | undefined

const inheritedByModel = computed(() => new Map((props.account.inherited_model_restrictions ?? []).map(item => [item.model, item.group_names])))
const disabledModels = computed(() => new Set([...(props.account.disabled_models ?? []), ...inheritedByModel.value.keys()]))
const models = computed(() => {
	const values = catalogModels.value.map(item => ({ ...item, missing: false }))
	const ids = new Set(values.map(item => item.id))
	for (const id of disabledModels.value) if (!ids.has(id)) { values.push({ id, name: id, missing: true, sources: [] }) }
	return values
})
const selectedModel = computed(() => catalogModels.value.find(item => item.id === model.value))
const accountUnavailable = computed(() => !props.account.enabled || props.account.status !== 'ready')
const canSend = computed(() => Boolean(selectedModel.value && message.value.trim() && !reading.value && !refreshing.value && !sending.value && !accountUnavailable.value && !disabledModels.value.has(model.value)))
const proxyName = computed(() => props.account.proxy_id ? props.proxies.find(proxy => proxy.id === props.account.proxy_id)?.name || c('missingProxy') : props.account.proxy_display || c('direct'))
const output = computed(() => result.value?.output?.slice(0, 20000) ?? '')
const formattedUpstreamBody = computed(() => {
	const body = result.value?.upstream_body ?? ''
	try { return body ? JSON.stringify(JSON.parse(body), null, 2) : '' } catch { return body }
})
const upstreamBody = computed(() => formattedUpstreamBody.value.slice(0, 20000))
const errorBodyTruncated = computed(() => Boolean(result.value?.upstream_body_truncated || formattedUpstreamBody.value.length > 20000))
const protocol = computed(() => (() => {
    if (props.account.upstream_protocol) {
        return transportLabel(props.account.upstream_protocol)
    }
    return c('protocolDefault', { value: props.settings?.upstream_transport ? transportLabel(props.settings.upstream_transport) : c('settingsFailed') })
})())
function reason(id: string) {
	const reasons: string[] = []
	if (props.account.disabled_models?.includes(id)) { reasons.push(c('blockedLocal')) }
	const groups = inheritedByModel.value.get(id)
	if (groups?.length) { reasons.push(c('blockedGroups', { groups: groups.join('、') })) }
	return reasons.join('；')
}
function modelLabel(id: string) {
	const name = catalogModels.value.find(item => item.id === id)?.name
	return name && name !== id ? `${name} · ${id}` : id || c('unknown')
}
function duration(value: number | null | undefined) { return value === null || value === undefined ? c('unknown') : `${value} ms` }
function isTestResult(value: unknown): value is AccountModelTestResult {
	if (!value || typeof value !== 'object') { return false }
	const candidate = value as Partial<AccountModelTestResult>
	return typeof candidate.ok === 'boolean' && typeof candidate.model === 'string' && typeof candidate.transport === 'string'
		&& typeof candidate.output === 'string' && typeof candidate.latency_ms === 'number'
		&& (candidate.first_token_ms === null || typeof candidate.first_token_ms === 'number')
}
watch(catalogModels, values => {
	if (!values.some(item => item.id === model.value)) { model.value = '' }
})
async function send() {
	if (!canSend.value) { return }
	const accountId = props.account.id
	const controller = new AbortController()
	testController = controller
	sending.value = true
	result.value = null
	error.value = ''
	cancelled.value = false
	try {
		const next = await api.testAccountModel(accountId, { model: model.value, message: message.value }, controller.signal)
		if (controller.signal.aborted || testController !== controller) { return }
		result.value = next
		if (!next.ok) { error.value = next.error || c('testFailed') }
	} catch (err) {
		if (!controller.signal.aborted && testController === controller) {
			if (err instanceof ApiError && isTestResult(err.payload)) { result.value = { ...err.payload, ok: false } }
			error.value = err instanceof Error ? err.message : c('requestFailed')
		}
	} finally {
		if (testController === controller) { sending.value = false; testController = undefined }
	}
}
function cancelTest() {
	testController?.abort()
	testController = undefined
	sending.value = false
	cancelled.value = true
}
function abort() { testController?.abort(); testController = undefined; abortCatalog() }
function close() { abort(); emit('close') }
onBeforeUnmount(abort)
</script>

<template>
	<Modal :open="true" :title="`${c('modelTest')} · ${account.name || account.email || c('unnamedAccount')}`" width="max-w-2xl" @close="close">
		<div class="test-modal">
			<div class="test-meta">
				<div><span>{{ c('protocol') }}</span><strong>{{ protocol }}</strong><small>{{ c('protocolChain') }}</small></div>
				<div><span>{{ c('proxy') }}</span><strong>{{ proxyName }}</strong><small v-if="account.email">{{ account.email }}</small></div>
			</div>
			<div class="field-grid">
				<label class="field" for="account-test-model">
					<span class="label">{{ c('model') }}</span>
					<select id="account-test-model" v-model="model" class="input" :disabled="sending || reading || refreshing || !models.length">
						<option value="" disabled>{{ c('chooseModel') }}</option>
						<option v-for="item in models" :key="item.id" :value="item.id" :disabled="item.missing || disabledModels.has(item.id)">{{ item.name }}{{ item.name !== item.id ? ` · ${item.id}` : '' }}{{ (() => { if (!item.missing) { return ` · ${item.source === 'manual' ? c('manualModel') : modelOriginNames(item) || c('upstreamModel')}`; } return ''; })() }}{{ reason(item.id) ? ` · ${reason(item.id)}` : '' }}</option>
					</select>
					<small v-if="selectedModel?.description">{{ selectedModel.description }}</small>
					<small v-if="selectedModel">{{ selectedModel.source === 'manual' ? c('manualModel') : modelOriginNames(selectedModel) || c('upstreamModel') }}</small>
				</label>
				<button type="button" class="btn" :disabled="sending || reading || refreshing" @click="refresh"><RefreshCw :size="14" :class="{ 'animate-spin': refreshing }" />{{ refreshing ? c('fetchingModels') : c('fetchModels') }}</button>
			</div>
			<output v-if="reading" class="control-hint" >{{ c('loadingModels') }}</output>
			<p v-else-if="!catalogModels.length" class="control-hint">{{ c('emptyCatalog') }}</p>
			<p v-if="selectedCatalog" class="control-hint">{{ selectedCatalog.fetched_at ? c('cacheAt', { time: formatTime(selectedCatalog.fetched_at) }) : c('neverFetched') }}</p>
			<ModelCatalogSourceStatus :account="account" :catalog="selectedCatalog" />
			<p v-for="issue in issues" :key="issue" class="notice notice-warning" role="alert">{{ c('catalogWarnings', { message: issue }) }}</p>
			<p class="control-hint">{{ c('catalogHint') }}</p>
			<p class="control-hint">{{ c('catalogPermissionHint') }}</p>
			<p v-if="selectedModel && isCodexOnlyModel(selectedModel)" class="notice notice-warning">{{ c('codexOnlyModel') }}</p>
			<p v-if="accountUnavailable" class="notice notice-warning">{{ c('accountUnavailable') }}</p>
			<p v-if="model && reason(model)" class="notice notice-warning">{{ reason(model) }}</p>
			<label class="field" for="account-test-message"><span class="label">{{ c('message') }}</span><textarea id="account-test-message" v-model="message" class="input" rows="3" :disabled="sending" maxlength="4000" /></label>
			<p v-if="!message.trim()" class="control-hint">{{ c('requiredMessage') }}</p>
			<p class="quota-warning">{{ c('quotaWarning') }}</p>
			<p v-if="error" class="notice notice-error" role="alert">{{ error }}</p>
			<output v-if="cancelled" class="notice notice-warning" >{{ c('testCancelled') }}</output>
			<section v-if="result" class="result-card" :class="result.ok ? 'result-ok' : 'result-failed'" aria-live="polite">
				<strong>{{ result.ok ? c('testSucceeded') : c('testFailed') }}</strong>
				<div class="result-facts"><span>{{ c('actualModel') }}: {{ modelLabel(result.model) }}</span><span>{{ c('actualProtocol') }}: {{ result.transport ? transportLabel(result.transport) : c('unknown') }}</span><span>{{ c('duration') }}: {{ duration(result.latency_ms) }}</span><span>{{ c('firstToken') }}: {{ duration(result.first_token_ms) }}</span></div>
				<span class="control-hint">{{ c('output') }}</span><pre v-if="output" class="result-output">{{ output }}</pre><p v-else class="control-hint">{{ c('noOutput') }}</p><small v-if="result.output?.length > 20000" class="control-hint">{{ c('outputTruncated') }}</small>
				<section v-if="!result.ok" class="upstream-error-details" :aria-label="c('errorDetails')">
					<strong>{{ c('errorDetails') }}</strong>
					<div class="result-facts"><span v-if="result.upstream_status">{{ c('upstreamStatus') }}: {{ result.upstream_status }}</span><span v-if="result.upstream_event">{{ c('upstreamEvent') }}: {{ result.upstream_event }}</span></div>
					<span class="control-hint">{{ c('upstreamBody') }}</span>
					<pre v-if="upstreamBody" class="result-output upstream-error-body">{{ upstreamBody }}</pre><p v-else class="control-hint">{{ c('noErrorBody') }}</p>
					<small v-if="errorBodyTruncated" class="control-hint">{{ c('errorBodyTruncated') }}</small>
				</section>
			</section>
		</div>
		<template #footer><div class="test-footer"><button type="button" class="btn" @click="close">{{ c('close') }}</button><button v-if="sending" type="button" class="btn" @click="cancelTest"><Square :size="14" />{{ c('cancelTest') }}</button><button type="button" class="btn btn-primary" :disabled="!canSend" @click="send"><Send :size="14" />{{ sending ? c('sending') : c('sendTest') }}</button></div></template>
	</Modal>
</template>

<style scoped>
.test-modal { display: grid; gap: 12px; min-width: 0; }
.test-meta { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 10px; }
.test-meta > div { display: grid; min-width: 0; gap: 4px; padding: 12px; border: 1px solid var(--color-line); border-radius: 9px; background: var(--color-canvas); }
.test-meta span, .test-meta small, .field small { color: var(--color-ink-faint); font-size: 10px; overflow-wrap: anywhere; }
.test-meta strong { min-width: 0; overflow-wrap: anywhere; font-size: 12px; }
.field-grid { display: grid; grid-template-columns: minmax(0, 1fr) auto; align-items: end; gap: 10px; }
.field { display: grid; gap: 7px; min-width: 0; }.label { margin: 0; color: var(--color-ink-muted); font-size: 11px; font-weight: 650; }
.quota-warning { margin: 0; padding: 10px; border-radius: 8px; background: var(--color-warn-soft); color: var(--color-warn); font-size: 11px; line-height: 1.6; }
.notice { margin: 0; padding: 9px 10px; border-radius: 8px; font-size: 11px; line-height: 1.6; overflow-wrap: anywhere; }.notice-warning { color: var(--color-warn); background: var(--color-warn-soft); }.notice-error { color: var(--color-danger); background: var(--color-danger-soft); }
.result-card { display: grid; min-width: 0; gap: 10px; padding: 12px; border: 1px solid var(--color-line); border-radius: 9px; background: var(--color-canvas); }.result-ok { border-color: var(--color-success); }.result-failed { border-color: var(--color-danger); }
.result-card > strong { font-size: 12px; }.result-facts { display: flex; flex-wrap: wrap; gap: 8px 14px; color: var(--color-ink-muted); font-size: 11px; overflow-wrap: anywhere; }.result-output { max-height: 280px; overflow: auto; margin: 0; padding: 10px; white-space: pre-wrap; overflow-wrap: anywhere; border-radius: 7px; background: var(--color-surface-2); font-family: var(--font-mono); font-size: 11px; line-height: 1.6; }
.upstream-error-details { display: grid; min-width: 0; gap: 8px; padding-top: 12px; border-top: 1px solid var(--color-line); }.upstream-error-details > strong { font-size: 12px; }.upstream-error-body { max-width: 100%; }
.control-hint { margin: 0; color: var(--color-ink-faint); font-size: 11px; line-height: 1.6; overflow-wrap: anywhere; }.test-footer { display: flex; width: 100%; min-width: 0; justify-content: flex-end; flex-wrap: wrap; gap: 8px; }
@media (max-width: 480px) { .test-meta, .field-grid { grid-template-columns: 1fr; }.test-footer .btn { flex: 1 1 auto; min-width: 0; padding-inline: 9px; } }
</style>
