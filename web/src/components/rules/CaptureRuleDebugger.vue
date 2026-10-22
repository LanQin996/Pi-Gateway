<script setup lang="ts">
import {useId, computed, onBeforeUnmount, ref, shallowRef, watch} from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, ApiError } from '../../api/client'
import type { Account, Capture, CaptureFrame, CaptureRuleTrace } from '../../api/types'
import type { Rule, RuleSchema } from '../../api/rules'
import type { CaptureSimulationResponse, RuleCanvasDiagnostics, RuleDebugFocus, RuleConditionTrace } from '../../api/ruleSimulation'
import { formatDateTime, useI18n } from '../../i18n'
import JsonViewer from '../JsonViewer.vue'
import Badge from '../Badge.vue'
import CaptureDiff from '../capture/CaptureDiff.vue'
import { frameSnapshot } from '../../utils/captureInspection'
import { validateRule } from '../../utils/ruleEditor'
import { ruleLabel } from '../../utils/ruleLabels'
import { traceRuleName } from '../../utils/ruleTracePresentation'
import { simulationChanges, simulationSnapshots, samplePreview, recordedRuleEvents, preferredRuleEvent, positiveCaptureID, frameSequence } from '../../utils/ruleSimulation'
import { useRuleDebugLocale, type RuleDebugText } from './ruleDebugLocale'

const formId = useId()
const props = defineProps<{ rule: Rule; schema: RuleSchema; disabled?: boolean }>()
const emit = defineEmits<{
    diagnostics: [value: RuleCanvasDiagnostics | null]
    focus: [value: RuleDebugFocus]
    errors: [value: Array<{ path: string; message: string; code?: string }>]
    sample: [value: unknown]
}>()
const route = useRoute()
const router = useRouter()
const { d } = useRuleDebugLocale()
const { locale } = useI18n()
const root = ref<HTMLElement>()
const open = ref(false)
const loading = ref(false)
const loadingCapture = ref(false)
const simulating = ref(false)
const captures = shallowRef<Capture[]>([])
const accounts = ref<Account[]>([])
const total = ref(0)
const page = ref(0)
const search = ref('')
const protocol = ref('')
const outcome = ref('')
const accountID = ref('')
const selectedCapture = shallowRef<Capture | null>(null)
const selectedEventSeq = ref<number | undefined>()
const scope = ref<'ruleset' | 'rule'>('ruleset')
const enableDraft = ref(false)
const batch = ref(false)
const result = shallowRef<CaptureSimulationResponse | null>(null)
const activeResult = ref(0)
const tab = ref<'compare' | 'fields' | 'steps'>('compare')
const stale = ref(false)
const errorKey = ref<RuleDebugText | ''>('')
const errorDetails = ref('')
let listTimer: ReturnType<typeof setTimeout> | undefined
let listSequence = 0
let captureSequence = 0
let requestSequence = 0
let controller: AbortController | undefined
let disposed = false
let accountsLoaded = false

const pageSize = 20
const phase = computed(() => props.rule.phase)
const events = computed(() => selectedCapture.value ? recordedRuleEvents(selectedCapture.value) : [])
const results = computed(() => result.value?.results ?? [])
const sample = computed(() => results.value[activeResult.value])
const snapshots = computed(() => sample.value && !sample.value.preview_truncated && !sample.value.warnings?.some(code => code === 'sample_unavailable' || code === 'context_unavailable') ? simulationSnapshots(sample.value) : null)
const changes = computed(() => snapshots.value?.before.available && snapshots.value.after.available && !sample.value?.preview_truncated
    ? simulationChanges(snapshots.value.before.value, snapshots.value.after.value) : null)
const currentRuleId = computed(() => result.value?.draft_rule_id || props.rule.id || '')
const traces = computed(() => sample.value?.result.traces ?? [])
const conditions = computed(() => sample.value?.result.condition_traces ?? [])
const diagnosticsLimited = computed(() => !!(sample.value?.preview_truncated || sample.value?.result.trace_omitted || sample.value?.result.condition_trace_omitted))
const warnings = computed(() => [...new Set([...(result.value?.warnings ?? []), ...(sample.value?.warnings ?? [])])])
const summary = computed(() => ({count: results.value.length, changed: results.value.filter(item => item.result.changed).length, dropped: results.value.filter(item => item.result.dropped).length}))
const canRun = computed(() => !!selectedCapture.value && selectedCapture.value.outcome !== 'pending' && !props.disabled && !loadingCapture.value && !simulating.value
    && (phase.value !== 'response_event' || (batch.value ? events.value.length > 0 : selectedEventSeq.value !== undefined)))

function clearError() { errorKey.value = ''; errorDetails.value = '' }
function reportError(cause: unknown, fallback: RuleDebugText) {
    errorKey.value = (() => {
    if (cause instanceof ApiError && cause.status === 404) {
        return 'recordMissing'
    }
    if (cause instanceof ApiError && cause.status === 409) {
        return 'conflict'
    }
    if (cause instanceof ApiError && cause.status === 422) {
        return 'unavailable'
    }
    return fallback
})()
    errorDetails.value = cause instanceof Error ? cause.message : String(cause)
    if (cause instanceof ApiError && cause.payload && typeof cause.payload === 'object') {
        const details = (cause.payload as {errors?: Array<{path: string; message: string; code?: string}>}).errors
        if (Array.isArray(details)) {
            errorDetails.value = details.map(item => `${item.path || '/'}: ${item.message}`).join('\n')
            const ruleErrors = details.filter(item => /^\/(rule|when|actions|name|priority|phase)(\/|$)/.test(item.path))
            if (ruleErrors.length) { emit('errors', ruleErrors) }
        }
    }
}
function invalidate() {
    requestSequence++
    controller?.abort()
    simulating.value = false
    if (result.value) { stale.value = true }
    emit('diagnostics', null)
    emit('sample', undefined)
}
function statusLabel(value = '') {
    const keys: Record<string, RuleDebugText> = {ok: 'ok', error: 'error', aborted: 'aborted', pending: 'pending', matched: 'matched', not_matched: 'not_matched', changed: 'changed', no_change: 'no_change', skipped: 'skipped', blocked: 'blocked', dropped: 'dropped', rolled_back: 'rolled_back'}
    return d(keys[value] ?? 'notExecuted')
}
function tone(value?: string) {
    return (() => {
    if (value === 'error' || value === 'blocked') {
        return 'danger'
    }
    if (value === 'ok') {
        return 'success'
    }
    if (value === 'changed' || value === 'matched') {
        return 'info'
    }
    if (value === 'dropped' || value === 'rolled_back' || value === 'aborted') {
        return 'warn'
    }
    return 'neutral'
})()
}
function frameLabel(type?: string) {
    const keys: Record<string, RuleDebugText> = {'response.output_text.delta': 'textDelta', 'response.created': 'started', 'response.in_progress': 'generating', 'response.completed': 'completed', 'response.failed': 'failed', 'response.incomplete': 'incomplete', 'response.output_item.added': 'itemAdded', 'response.output_item.done': 'itemDone'}
    return d(keys[type ?? ''] ?? 'otherEvent')
}
function framePreview(frame: CaptureFrame) { return samplePreview(frameSnapshot(frame).value) }
function protocolLabel(value: string) { return (() => {
    if (value === 'ws') {
        return 'WebSocket'
    }
    if (value === 'sse') {
        return 'SSE'
    }
    if (value === 'http') {
        return 'HTTP'
    }
    return '—'
})() }
function sourceLabel() {
    const source = sample.value?.source
    return d(source === 'checkpoint' || source === 'recorded_event' || source === 'reconstructed' ? source : 'unknownWarning')
}
function warningLabel(value: string) {
    const keys: Record<string, RuleDebugText> = {
        capture_truncated: 'truncatedCapture', capture_pending: 'pendingCapture', input_reconstructed_current_config: 'requestRebuilt',
        aggregate_reconstructed: 'bodyRebuilt', current_event_rules_replayed: 'eventRulesReplayed', context_partial: 'contextPartial',
        checkpoint_unavailable: 'checkpointUnavailable', context_unavailable: 'contextUnavailable', sample_unavailable: 'sampleUnavailable',
        preview_truncated: 'previewUnavailable', diagnostics_truncated: 'omitted',
    }
    return d(keys[value] ?? 'unknownWarning')
}
async function loadCaptures(offset = page.value * pageSize) {
    clearTimeout(listTimer)
    const sequence = ++listSequence
    loading.value = true
    clearError()
    try {
        const response = await api.listCaptures({search: search.value || undefined, transport: protocol.value || undefined,
            outcome: outcome.value || undefined, account_id: accountID.value && accountID.value !== 'unassigned' ? accountID.value : undefined,
            unassigned: accountID.value === 'unassigned' ? 'true' : undefined, limit: pageSize, offset})
        if (disposed || sequence !== listSequence) { return }
        captures.value = response.captures ?? []
        total.value = response.total ?? 0
        page.value = Math.floor(offset / pageSize)
    } catch (cause) { if (!disposed && sequence === listSequence) { reportError(cause, 'loadFailed') } }
    finally { if (!disposed && sequence === listSequence) { loading.value = false } }
}
function openCaptureList() {
    open.value = !open.value
    if (!open.value) { return }
    void loadCaptures(0)
    if (!accountsLoaded) {
        accountsLoaded = true
        void api.listAccounts().then(response => { if (!disposed) { accounts.value = response.accounts ?? [] } }).catch(() => { accountsLoaded = false })
    }
}
async function selectCapture(id: number) {
    const sequence = ++captureSequence
    invalidate()
    selectedCapture.value = null
    result.value = null
    stale.value = false
    loadingCapture.value = true
    clearError()
    try {
        // List rows omit bodies. Always fetch the complete capture after selection.
        const response = await api.getCapture(id)
        if (disposed || sequence !== captureSequence) { return }
        selectedCapture.value = response.capture
        selectedEventSeq.value = phase.value === 'response_event' ? preferredRuleEvent(recordedRuleEvents(response.capture), frameSequence(route.query.frame)) : undefined
        open.value = false
    } catch (cause) { if (!disposed && sequence === captureSequence) { reportError(cause, 'loadFailed'); open.value = true } }
    finally { if (!disposed && sequence === captureSequence) { loadingCapture.value = false } }
}
function openCapture() { if (selectedCapture.value) { void router.push({name: 'capture-detail', params: {id: selectedCapture.value.id}}) } }
function acceptSimulation(response: Awaited<ReturnType<typeof api.simulateCapture>>) {
    result.value = response
    activeResult.value = 0
    stale.value = false
    if (!response.errors?.length) { return }
    emit('errors', response.errors)
    errorKey.value = 'simulationFailed'
    errorDetails.value = response.errors.map(item => item.message).join('\n')
}
function canContinueBatch() {
    return !stale.value && !!result.value?.has_more && !!result.value.batch_token
}
function simulationFrame(isBatch: boolean) {
    return phase.value === 'response_event' && !isBatch ? selectedEventSeq.value : undefined
}
async function simulate(nextBatch = false) {
    if (!canRun.value) { return }
    if (root.value?.closest('form')?.reportValidity() === false) { return }
    const validation = validateRule(props.rule, props.schema)
    if (validation.length) { emit('errors', validation); errorKey.value = 'ruleInvalid'; return }
    if (nextBatch && !canContinueBatch()) { return }
    const previous = nextBatch ? result.value : null
    invalidate()
    const sequence = ++requestSequence
    controller = new AbortController()
    simulating.value = true
    clearError()
    const isBatch = phase.value === 'response_event' && batch.value
    try {
        const response = await api.simulateCapture({capture_id: selectedCapture.value!.id, rule: props.rule, phase: phase.value,
            scope: scope.value, enable_draft: enableDraft.value, frame_seq: simulationFrame(isBatch),
            batch: isBatch, limit: isBatch ? 50 : undefined,
            after_seq: previous?.next_after_seq, expected_version: previous?.rules_version, batch_token: previous?.batch_token,
        }, controller.signal)
        if (disposed || sequence !== requestSequence) { return }
        acceptSimulation(response)
    } catch (cause) {
        if (disposed || sequence !== requestSequence || controller.signal.aborted) { return }
        reportError(cause, 'simulationFailed')
    } finally { if (!disposed && sequence === requestSequence) { simulating.value = false } }
}
function selectResult(value: string) {
    const index = Number(value)
    if (Number.isInteger(index) && index >= 0 && index < results.value.length) { activeResult.value = index }
}
function focus(ruleId: string | undefined, actionId?: string, path?: string) {
    if (stale.value || props.disabled || !ruleId || ruleId !== currentRuleId.value) { return }
    emit('focus', {ruleId, actionId, path})
}
function conditionTitle(trace: RuleConditionTrace) {
    if (trace.rule_id !== currentRuleId.value) { return d('condition') }
    let value: unknown = props.rule
    for (const part of trace.path.split('/').slice(1)) {
        const key = part.replaceAll('~1', '/').replaceAll('~0', '~')
        if (!value || typeof value !== 'object' || !Object.hasOwn(value, key)) { value = undefined; break }
        value = (value as Record<string, unknown>)[key]
    }
    return value && typeof value === 'object' && 'op' in value && typeof value.op === 'string' ? ruleLabel('condition', value.op, locale.value) : d('condition')
}
function traceTitle(trace: CaptureRuleTrace) {
    return trace.action_type ? ruleLabel('action', trace.action_type, locale.value) : traceRuleName(trace, locale.value)
}
function onFilterChange() {
    ++listSequence
    clearTimeout(listTimer)
    loading.value = true
    listTimer = setTimeout(() => void loadCaptures(0), 200)
}
watch(() => props.rule, invalidate, {deep: true, flush: 'sync'})
watch(() => props.disabled, invalidate, {flush: 'sync'})
watch([scope, enableDraft, batch, selectedEventSeq], invalidate, {flush: 'sync'})
watch(phase, () => { selectedEventSeq.value = phase.value === 'response_event' ? preferredRuleEvent(events.value) : undefined; batch.value = false })
watch(() => route.query.capture, value => { const id = positiveCaptureID(value); if (id) { void selectCapture(id) } }, {immediate: true})
watch(() => route.query.frame, value => { const seq = frameSequence(value); if (events.value.some(frame => frame.seq === seq)) { selectedEventSeq.value = seq } })
watch([sample, stale, currentRuleId], () => {
    if (!sample.value || stale.value || props.disabled) { emit('diagnostics', null); return }
    emit('diagnostics', {ruleId: currentRuleId.value, traces: traces.value, conditions: conditions.value})
    emit('sample', snapshots.value?.before.available ? snapshots.value.before.value : undefined)
})
onBeforeUnmount(() => { disposed = true; ++captureSequence; ++listSequence; clearTimeout(listTimer); invalidate() })
</script>

<template>
    <section ref="root" class="rule-debugger card" data-testid="rule-debugger">
        <header class="debugger-header">
            <div><h3>{{ d('title') }}</h3><p>{{ d('subtitle') }}</p></div>
            <div class="debugger-actions"><button type="button" class="btn" data-testid="choose-capture" @click="openCaptureList">{{ (() => { if (open) { return d('close'); } if (selectedCapture) { return d('change'); } return d('choose'); })() }}</button><button v-if="selectedCapture" type="button" class="btn" @click="openCapture">{{ d('openRecord') }}</button></div>
        </header>
        <p class="debugger-note">{{ d('offline') }}</p>
        <div v-if="errorKey" class="notice notice-error" role="alert"><strong>{{ d(errorKey) }}</strong><pre v-if="errorDetails" class="debugger-error-detail">{{ errorDetails }}</pre></div>
        <div v-if="open" class="debugger-picker">
            <div class="debugger-filter-row">
                <label class="sr-only" :for="`${formId}-search`">{{ d('search') }}</label><input :id="`${formId}-search`" v-model="search" data-testid="capture-search" class="input" :placeholder="d('search')" @input="onFilterChange" />
                <label class="sr-only" :for="`${formId}-accountID`">{{ d('allAccounts') }}</label><select :id="`${formId}-accountID`" v-model="accountID" data-testid="capture-account" class="input" @change="onFilterChange"><option value="">{{ d('allAccounts') }}</option><option value="unassigned">{{ d('unassigned') }}</option><option v-for="account in accounts" :key="account.id" :value="account.id">{{ account.name }}</option></select>
                <label class="sr-only" :for="`${formId}-protocol`">{{ d('allProtocols') }}</label><select :id="`${formId}-protocol`" v-model="protocol" data-testid="capture-protocol" class="input" @change="onFilterChange"><option value="">{{ d('allProtocols') }}</option><option value="http">HTTP</option><option value="sse">SSE</option><option value="ws">WebSocket</option></select>
                <label class="sr-only" :for="`${formId}-outcome`">{{ d('allOutcomes') }}</label><select :id="`${formId}-outcome`" v-model="outcome" data-testid="capture-outcome" class="input" @change="onFilterChange"><option value="">{{ d('allOutcomes') }}</option><option value="ok">{{ d('ok') }}</option><option value="error">{{ d('error') }}</option><option value="aborted">{{ d('aborted') }}</option><option value="pending">{{ d('pending') }}</option></select>
            </div>
            <p v-if="loading" class="debugger-muted">{{ d('loading') }}</p>
            <p v-else-if="!captures.length" class="debugger-muted">{{ d('empty') }}</p>
            <div v-else class="capture-picker-list">
                <button v-for="capture in captures" :key="capture.id" :data-testid="`capture-choice-${capture.id}`" type="button" class="capture-choice" :class="{selected: selectedCapture?.id === capture.id}" @click="selectCapture(capture.id)">
                    <span class="capture-choice-main"><strong>{{ capture.model || '—' }}</strong><span>{{ d('record', {id: capture.id}) }}</span></span>
                    <span class="capture-choice-meta"><Badge :tone="tone(capture.outcome)">{{ statusLabel(capture.outcome) }}</Badge><span>{{ protocolLabel(capture.client_transport) }} → {{ protocolLabel(capture.upstream_transport) }}</span><span>{{ formatDateTime(capture.created_at) }}</span></span>
                    <span class="capture-choice-preview">{{ capture.account_id === 0 ? d('unassigned') : capture.account_name || capture.url || '—' }}</span>
                </button>
            </div>
            <div class="debugger-pagination"><button type="button" class="btn" :disabled="page <= 0 || loading" @click="loadCaptures((page - 1) * pageSize)">{{ d('previous') }}</button><span>{{ d('page', {page: page + 1, total}) }}</span><button type="button" class="btn" :disabled="(page + 1) * pageSize >= total || loading" @click="loadCaptures((page + 1) * pageSize)">{{ d('next') }}</button></div>
        </div>
        <output v-if="loadingCapture" class="debugger-muted" >{{ d('loading') }}</output>
        <p v-else-if="!selectedCapture && !open" class="debugger-muted">{{ d('chooseFirst') }}</p>
        <div v-if="selectedCapture" class="debugger-selected">
            <div class="selected-summary"><strong>{{ d('record', {id: selectedCapture.id}) }}</strong><span>{{ selectedCapture.model || '—' }}</span><Badge :tone="tone(selectedCapture.outcome)">{{ statusLabel(selectedCapture.outcome) }}</Badge><span v-if="selectedCapture.truncated" class="debugger-muted">{{ d('truncatedCapture') }}</span><span v-if="selectedCapture.outcome === 'pending'" class="debugger-muted">{{ d('pendingCapture') }}</span></div>
            <div v-if="phase === 'response_event'" class="event-picker">
                <label>{{ d('event') }}<select v-model="selectedEventSeq" data-testid="capture-event" class="input"><option v-if="!events.length" :value="undefined">{{ d('noEvents') }}</option><option v-for="frame in events" :key="frame.seq" :value="frame.seq">{{ d('frame', {seq: frame.seq}) }} · {{ frameLabel(frame.type) }} · {{ framePreview(frame) }}</option></select></label>
            </div>
            <div class="debugger-controls">
                <label>{{ d('scope') }}<select v-model="scope" data-testid="simulation-scope" class="input"><option value="ruleset">{{ d('ruleset') }}</option><option value="rule">{{ d('singleRule') }}</option></select></label>
                <label v-if="!props.rule.enabled" class="checkbox"><input v-model="enableDraft" data-testid="enable-draft" type="checkbox" /> {{ d('enableDraft') }}</label>
                <label v-if="phase === 'response_event'" class="checkbox"><input v-model="batch" data-testid="batch-events" type="checkbox" /> {{ d('batch') }}</label>
                <button type="button" data-testid="simulate-capture" class="btn btn-primary" :disabled="!canRun" @click="simulate()">{{ simulating ? d('running') : d('run') }}</button>
                <button v-if="result?.has_more" type="button" data-testid="simulate-next-batch" class="btn" :disabled="simulating || stale" @click="simulate(true)">{{ d('runNext') }}</button>
            </div>
            <p v-if="!props.rule.enabled" class="debugger-muted">{{ d('disabledRule') }}</p>
            <p v-if="phase === 'response_event' && batch" class="debugger-hint">{{ d('batchHint') }}</p>
        </div>
        <div v-if="result" class="debugger-results" :class="{stale}" data-testid="simulation-result">
            <output v-if="stale" class="notice notice-warning" data-testid="simulation-stale" >{{ d('stale') }}</output>
            <div class="result-summary">
                <strong>{{ d('version', {version: result.rules_version}) }}</strong>
                <Badge :tone="summary.changed ? 'info' : 'neutral'">{{ d('batchSummary', summary) }}</Badge>
            </div>
            <label v-if="results.length > 1" class="result-selector">{{ d('resultEvent') }}
                <select data-testid="simulation-result-event" class="input" :value="activeResult" @change="selectResult(($event.target as HTMLSelectElement).value)">
                    <option v-for="(item, index) in results" :key="index" :value="index">{{ d('frame', {seq: item.frame_seq ?? index}) }} · {{ frameLabel(item.event_type) }} · {{ (() => { if (item.error) { return d('error'); } if (item.result.dropped) { return d('dropped'); } if (item.result.changed) { return d('changed'); } return d('noChanges'); })() }}</option>
                </select>
            </label>
            <p v-if="!results.length" class="debugger-muted">{{ d('noResults') }}</p>
            <article v-if="sample" class="result-sample card">
                <header class="sample-header">
                    <strong>{{ sample.frame_seq === undefined ? ruleLabel('phase', phase, locale) : d('frame', {seq: sample.frame_seq}) }}</strong>
                    <span v-if="sample.event_type">{{ frameLabel(sample.event_type) }}</span>
                    <Badge :tone="(() => { if (sample.error) { return 'danger'; } if (sample.result.blocked) { return 'danger'; } if (sample.result.dropped) { return 'warn'; } if (sample.result.changed) { return 'info'; } return 'neutral'; })()">{{ (() => { if (sample.error) { return d('error'); } if (sample.result.blocked) { return d('blocked'); } if (sample.result.dropped) { return d('dropped'); } if (sample.result.changed) { return d('changed'); } return d('noChanges'); })() }}</Badge>
                    <span v-if="changes">{{ d(changes.limited ? 'limitedChanges' : 'changes', {count: changes.changes.length}) }}</span>
                </header>
                <p class="sample-source"><strong>{{ d('source') }}:</strong> {{ sourceLabel() }}</p>
                <p v-if="sample.source === 'reconstructed'" class="notice notice-warning">{{ d('reconstructedHint') }}</p>
                <p v-if="diagnosticsLimited" class="notice notice-warning">{{ d('omitted') }}</p>
                <details v-if="warnings.length" class="technical-details"><summary>{{ d('unknownWarning') }}</summary><p v-for="warning in warnings" :key="warning">{{ warningLabel(warning) }}</p></details>
                <details v-if="sample.error" class="notice notice-error"><summary>{{ d('simulationFailed') }}</summary><pre>{{ sample.error }}</pre></details>
                <div class="segmented simulation-tabs" :aria-label="d('title')">
                    <button v-for="item in (['compare', 'fields', 'steps'] as const)" :key="item" type="button" :data-testid="`simulation-tab-${item}`" class="segmented-button" :class="{'is-active': tab === item}" :aria-pressed="tab === item" @click="tab = item">{{ d(item) }}</button>
                </div>
                <template v-if="tab === 'compare'">
                    <p v-if="!snapshots" class="debugger-muted">{{ d('previewUnavailable') }}</p>
                    <div v-if="snapshots" class="comparison-grid" data-testid="simulation-before-after">
                        <JsonViewer v-if="snapshots.before.available" :value="snapshots.before.value" :label="d('before')" max-height="20rem" />
                        <p v-else class="debugger-muted">{{ d('previewUnavailable') }}</p>
                        <JsonViewer v-if="snapshots.after.available" :value="snapshots.after.value" :label="d('after')" max-height="20rem" />
                        <p v-else class="debugger-muted">{{ d('previewUnavailable') }}</p>
                    </div>
                    <CaptureDiff v-if="snapshots" :key="`${result.capture_id}:${activeResult}:${result.rules_version}`" data-testid="simulation-diff" :title="d('diff')" :before="snapshots.before" :after="snapshots.after" :before-label="d('before')" :after-label="d('after')" initial-open :partial="Boolean(sample.preview_truncated)" />
                </template>
                <template v-else-if="tab === 'fields'">
                    <div v-if="changes?.changes.length" class="change-list" data-testid="simulation-fields">
                        <div v-for="change in changes.changes" :key="change.path" class="change-row">
                            <code>{{ change.path || '/' }}</code><Badge>{{ d(change.operation) }}</Badge>
                            <span>{{ change.beforeExists ? samplePreview(change.before) : d('missing') }} → {{ change.afterExists ? samplePreview(change.after) : d('missing') }}</span>
                        </div>
                        <p v-if="changes.limited" class="debugger-muted">{{ d('omitted') }}</p>
                    </div>
                    <p v-else class="debugger-muted">{{ changes ? d('noChanges') : d('previewUnavailable') }}</p>
                </template>
                <template v-else>
                    <div v-if="traces.length || conditions.length" class="trace-list" data-testid="simulation-steps">
                        <button v-for="(condition, index) in conditions" :key="`condition:${index}`" data-testid="simulation-condition" :data-path="condition.path" :data-rule-id="condition.rule_id" type="button" class="trace-row" :disabled="stale || condition.rule_id !== currentRuleId" :title="d('selectNode')" @click="focus(condition.rule_id, undefined, condition.path)">
                            <span>{{ conditionTitle(condition) }}<small v-if="condition.item_index !== undefined"> · {{ d('item', {index: condition.item_index}) }}</small></span>
                            <Badge :tone="tone(condition.status)">{{ statusLabel(condition.status) }}</Badge>
                        </button>
                        <button v-for="(trace, traceIndex) in traces" :key="`action:${traceIndex}`" data-testid="simulation-step" :data-action-id="trace.action_id" :data-rule-id="trace.rule_id" type="button" class="trace-row" :disabled="stale || trace.rule_id !== currentRuleId" :title="d('selectNode')" @click="focus(trace.rule_id, trace.action_id)">
                            <span>{{ traceTitle(trace) }}<small v-if="trace.rule_id !== currentRuleId"> · {{ trace.rule_name || d('unknownRule') }}</small></span>
                            <Badge :tone="tone(trace.rolled_back ? 'rolled_back' : trace.status)">{{ statusLabel(trace.rolled_back ? 'rolled_back' : trace.status) }}</Badge>
                        </button>
                        <p v-if="!traces.some(trace => trace.rule_id === currentRuleId)" class="debugger-muted">{{ d('noCurrentTrace') }}</p>
                    </div>
                    <p v-else class="debugger-muted">{{ d('noSteps') }}</p>
                </template>
                <details class="technical-details"><summary>{{ d('rawResult') }}</summary><JsonViewer :value="sample" max-height="24rem" /></details>
            </article>
            <p v-if="result.has_more" class="debugger-muted">{{ d('more') }}</p><p v-else-if="batch && results.length" class="debugger-muted">{{ d('finished') }}</p>
        </div>
    </section>
</template>

<style scoped>
.rule-debugger { display: grid; gap: 12px; min-width: 0; padding: 14px; resize: vertical; overflow: auto; min-height: 180px; max-height: 85vh; }.debugger-header, .selected-summary, .debugger-controls, .debugger-filter-row, .debugger-pagination, .result-summary, .sample-header { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; }.debugger-header { justify-content: space-between; }.debugger-header h3 { margin: 0; font-size: 14px; }.debugger-header p, .debugger-note, .debugger-hint, .debugger-muted, .sample-source { margin: 0; color: var(--color-ink-muted); font-size: 11px; line-height: 1.6; overflow-wrap: anywhere; }.debugger-actions { display: flex; gap: 8px; }.debugger-note { padding: 8px 10px; border-radius: 6px; background: var(--color-info-soft); }.debugger-picker, .debugger-selected, .debugger-results { display: grid; gap: 10px; min-width: 0; }.debugger-filter-row > .input { min-width: 130px; flex: 1; }.capture-picker-list { display: grid; gap: 6px; max-height: 19rem; overflow: auto; }.capture-choice { display: grid; gap: 4px; padding: 9px 10px; text-align: left; border: 1px solid var(--color-line); border-radius: 7px; background: var(--color-canvas); color: inherit; cursor: pointer; }.capture-choice:hover, .capture-choice.selected { border-color: var(--color-accent); background: var(--color-accent-soft); }.capture-choice-main, .capture-choice-meta { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }.capture-choice-main > span, .capture-choice-meta { color: var(--color-ink-muted); font-size: 11px; }.capture-choice-preview { color: var(--color-ink-muted); font: 11px/1.5 var(--font-mono, monospace); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }.debugger-pagination { justify-content: space-between; color: var(--color-ink-muted); font-size: 11px; }.selected-summary { padding: 8px 10px; border: 1px solid var(--color-line); border-radius: 6px; }.event-picker label, .debugger-controls label { display: grid; gap: 4px; color: var(--color-ink-muted); font-size: 11px; }.event-picker .input { width: 100%; }.debugger-controls { align-items: end; }.debugger-controls > label { min-width: 180px; flex: 1; }.debugger-controls .checkbox { display: flex; align-items: center; min-width: auto; flex: 0 1 auto; }.result-summary strong { margin-right: auto; font-size: 12px; }.result-sample { display: grid; gap: 9px; min-width: 0; padding: 10px; }.sample-header strong { margin-right: auto; }.sample-header > span { color: var(--color-ink-muted); font-size: 11px; }.sample-source { padding: 6px 8px; background: var(--color-canvas); border-radius: 5px; }.change-list, .trace-list, .condition-list { display: grid; gap: 6px; }.change-list > strong, .trace-list > strong { font-size: 12px; }.change-row, .trace-row { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; min-width: 0; padding: 6px 8px; border: 1px solid var(--color-line); border-radius: 5px; background: var(--color-canvas); font-size: 11px; text-align: left; }.change-row code, .trace-row code { color: var(--color-ink-muted); overflow-wrap: anywhere; }.trace-row { width: 100%; color: inherit; cursor: pointer; }.trace-row span:first-child { margin-right: auto; overflow-wrap: anywhere; }.technical-details { font-size: 11px; }.technical-details > summary { cursor: pointer; color: var(--color-ink-muted); }.debugger-results.stale { opacity: .7; }.notice { margin: 0; }.notice-error { color: var(--color-danger); }.notice-warning { color: var(--color-warning); }

.comparison-grid { display: grid; grid-template-columns: repeat(2,minmax(0,1fr)); gap: 10px; min-width: 0; }
.result-selector { display: grid; gap: 5px; font-size: 12px; }.simulation-tabs { width: fit-content; max-width: 100%; flex-wrap: wrap; }
.debugger-error-detail, .technical-details pre, .result-sample pre { white-space: pre-wrap; overflow-wrap: anywhere; max-height: 16rem; overflow: auto; font-size: 11px; }
.trace-row:disabled { cursor: default; }.trace-row small { color: var(--color-ink-muted); }.change-row > span:last-child { overflow-wrap: anywhere; }
@media (max-width: 800px) { .comparison-grid { grid-template-columns: minmax(0,1fr); } }
@media (max-width: 680px) { .debugger-controls > label { min-width: 100%; }.debugger-filter-row > .input { min-width: 100%; }.sample-header .btn { width: 100%; }.capture-choice-meta { align-items: flex-start; flex-direction: column; gap: 3px; } }
</style>
