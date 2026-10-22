<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ArrowDownLeft, ArrowLeft, ArrowUpRight, Download, Info } from 'lucide-vue-next'
import { api } from '../api/client'
import type { Capture, CaptureFrame, CaptureRuleTrace } from '../api/types'
import { useToastStore, formatBytes, formatDuration, formatTime } from '../stores/ui'
import { useI18n } from '../i18n'
import { captureRows, groupCaptureRows, frameSnapshot, hopProtocol, payloadPreview, requestSnapshots, responseHeaderSnapshots, ruleSources } from '../utils/captureInspection'
import Badge from '../components/Badge.vue'
import JsonViewer from '../components/JsonViewer.vue'
import CaptureDiff from '../components/capture/CaptureDiff.vue'
import CaptureStreamPanel from '../components/capture/CaptureStreamPanel.vue'
import RuleTracePanel from '../components/capture/RuleTracePanel.vue'
import { useCaptureLocale, type CaptureTextKey } from '../components/capture/captureLocale'
import { useRuleDebugLocale } from '../components/rules/ruleDebugLocale'
import { ruleLabel } from '../utils/ruleLabels'
import { traceRuleName } from '../utils/ruleTracePresentation'

const route = useRoute()
const router = useRouter()
const toast = useToastStore()
const { t, locale } = useI18n()
const { c } = useCaptureLocale()
const { d } = useRuleDebugLocale()
const capture = ref<Capture | null>(null)
const loading = ref(true)
const tab = ref<'timeline' | 'request' | 'response' | 'raw'>('timeline')
const compact = ref(true)
const rowLimit = ref(50)
const expandedGroups = ref(new Set<string>())
const groupLimits = ref<Record<string, number>>({})
let version = 0
async function load() {
	const current = ++version
	loading.value = true
	capture.value = null
	rowLimit.value = 50
	expandedGroups.value.clear()
	groupLimits.value = {}
	try {
		const id = Number(route.params.id)
		const result = await api.getCapture(id)
		if (current === version) { capture.value = result.capture }
	} catch (err) {
		if (current === version) { toast.error(err instanceof Error ? err.message : t('capture.notFound')) }
	} finally { if (current === version) { loading.value = false } }
}
watch(() => route.params.id, load, { immediate: true })
onBeforeUnmount(() => { version++ })
const request = computed(() => capture.value ? requestSnapshots(capture.value) : null)
const responseHeaders = computed(() => capture.value ? responseHeaderSnapshots(capture.value) : null)
const rows = computed(() => capture.value ? captureRows(capture.value, compact.value) : [])
const rowGroups = computed(() => groupCaptureRows(rows.value))
const visibleGroups = computed(() => rowGroups.value.slice(0, rowLimit.value))
const tabs = computed(() => [
	{ key: 'timeline', label: c('timeline') }, { key: 'request', label: c('request') },
	{ key: 'response', label: c('headers') }, { key: 'raw', label: c('streams') },
])
function direction(frame: CaptureFrame) {
	if (frame.kind === 'note' || frame.dir === 'internal') { return c('internal') }
	const names: Record<string, CaptureTextKey> = { client_in: 'clientIn', out: 'upstreamOut', in: 'upstreamIn', client_out: 'clientOut' }
	return c(Object.hasOwn(names, frame.dir) ? names[frame.dir]! : 'unknownDirection')
}
function frameLabel(frame: CaptureFrame) {
	if (frame.kind === 'rule_input') { return d('internalInput') }
	if (frame.kind === 'rule_context') { return ruleLabel('source', 'context', locale.value) }
	const names: Record<string, CaptureTextKey> = { note: 'note', sse_event: 'sseEvent', ws_frame: 'wsFrame', ws_binary: 'wsFrame', ws_close: 'wsClose', ws_error: 'wsError', http_error_body: 'httpError', http_response: 'httpResponse', request_body: 'requestBody', handshake_request: 'requestHead', handshake_response: 'responseHead' }
	return Object.hasOwn(names, frame.kind) ? c(names[frame.kind]!) : frame.kind || c('frame')
}
function frameProtocol(frame: CaptureFrame) {
	return c((() => {
    if (frame.kind.startsWith('ws_')) {
        return 'ws'
    }
    if (frame.kind === 'sse_event') {
        return 'sse'
    }
    return 'http'
})())
}
function outcomeTone(outcome: string) {
	return (() => {
    if (outcome === 'ok') {
        return 'success'
    }
    if (outcome === 'error') {
        return 'danger'
    }
    if (outcome === 'aborted') {
        return 'warn'
    }
    return 'neutral'
})()
}
function outcomeLabel(outcome: string) {
	return (() => {
    if (outcome === 'ok') {
        return t('usage.outcome.succeeded')
    }
    if (outcome === 'error') {
        return t('usage.outcome.failed')
    }
    if (outcome === 'aborted') {
        return t('usage.outcome.cancelled')
    }
    return t('usage.outcome.running')
})()
}
function frameTone(frame: CaptureFrame) {
	if (frame.kind === 'note') { return 'warn' }
	if (frame.type === 'response.completed') { return 'success' }
	if (['error', 'response.failed'].includes(frame.type ?? '') || frame.kind === 'http_error_body') { return 'danger' }
	return frame.dir === 'out' || frame.dir === 'client_out' ? 'info' : 'neutral'
}
function preview(frame: CaptureFrame) {
	return payloadPreview(frameSnapshot(frame).value)
}
function sourceLabel(trace: CaptureRuleTrace) {
	const action = trace.action_type ? ` · ${ruleLabel('action', trace.action_type, locale.value)} #${(trace.action_index ?? 0) + 1}` : ''
	const priority = trace.priority === undefined ? '' : ` · ${t('capture.rules.priority', { value: trace.priority })}`
	return `${traceRuleName(trace, locale.value)}${action}${priority} · ${t('capture.rules.version', { version: trace.rules_version ?? capture.value?.rules_version ?? 0 })}`
}
function sourceLabels(phase?: string, eventID?: string) {
	if (!capture.value) { return [] }
    return [...new Set(ruleSources(capture.value, phase, eventID).map(sourceLabel))]
}
function sourceNote() {
	if (capture.value?.rules_trace_truncated || capture.value?.rules_trace_omitted) { return t('capture.rules.truncated', { count: capture.value.rules_trace_omitted ?? 0 }) }
	if (!capture.value?.rule_traces?.length) { return t(capture.value?.rules_version ? 'capture.rules.noChanges' : 'capture.rules.legacy') }
	return t('capture.rules.imprecise')
}
function rowSources(before?: CaptureFrame, after?: CaptureFrame) {
	const eventID = after?.rule_event_id || before?.rule_event_id
	return eventID ? sourceLabels(undefined, eventID) : []
}
function downloadJson() {
	if (!capture.value) { return }
	const blob = new Blob([JSON.stringify(capture.value, null, 2)], { type: 'application/json' })
	const url = URL.createObjectURL(blob)
	const anchor = document.createElement('a')
	anchor.href = url
	anchor.download = `capture-${capture.value.id}.json`
	anchor.click()
	URL.revokeObjectURL(url)
}
</script>

<template>
	<div class="page-view capture-inspector">
		<header class="page-header">
			<div>
				<button class="capture-back" @click="router.push('/captures')"><ArrowLeft :size="14" />{{ t('capture.back') }}</button>
				<h1 class="page-title">{{ t('capture.exchange') }} #{{ capture?.id ?? '—' }} <Badge v-if="capture" :tone="outcomeTone(capture.outcome)">{{ outcomeLabel(capture.outcome) }}</Badge></h1>
				<p class="page-description">{{ capture?.account_id === 0 ? t('capture.unassigned') : capture?.account_name || '—' }} · {{ capture?.model || '—' }} · {{ capture ? formatTime(capture.created_at) : '' }}</p>
			</div>
			<div class="capture-actions"><button class="btn" data-testid="debug-capture-rules" :disabled="!capture" @click="capture && router.push({name: 'rules', query: {capture: capture.id}})">{{ c('debugRules') }}</button><button class="btn" :disabled="!capture" @click="downloadJson"><Download :size="14" />{{ t('capture.download') }}</button><button class="btn" :disabled="loading" @click="load">{{ t('common.refresh') }}</button></div>
		</header>

		<div v-if="capture && request && responseHeaders" :key="capture.id" class="page-content capture-content" :aria-busy="loading">
			<div class="capture-summary">
				<div class="card summary-item"><span>{{ c('clientProtocol') }}</span><strong>{{ c(hopProtocol(capture, 'client')) }}</strong><small>{{ c('clientStatus') }}：{{ responseHeaders.status || c('unrecorded') }}</small></div>
				<div class="card summary-item"><span>{{ c('upstreamProtocol') }}</span><strong>{{ c(hopProtocol(capture, 'upstream')) }}</strong><small>{{ c('upstreamStatus') }}：{{ capture.status || c('unrecorded') }}</small></div>
				<div class="card summary-item"><span>{{ t('capture.duration') }}</span><strong>{{ formatDuration(capture.duration_ms) }}</strong><small>{{ t('capture.firstByte', { value: formatDuration(capture.ttfb_ms) }) }}</small></div>
				<div class="card summary-item"><span>{{ t('capture.tokens') }}</span><strong>{{ capture.total_tokens }}</strong><small>{{ t('capture.tokensDetail', { in: capture.prompt_tokens, out: capture.completion_tokens }) }}</small></div>
				<div class="card summary-item"><span>{{ t('capture.requestSize') }}</span><strong>{{ formatBytes(Math.abs(capture.request_bytes)) }}</strong><small>{{ capture.response_id || '—' }}</small></div>
			</div>
			<div v-if="capture.error" class="notice notice-error capture-error" role="alert"><Info :size="16" /><p>{{ capture.error }}</p></div>
			<output v-if="capture.truncated" class="notice notice-warning" >{{ c('truncated') }}</output>
			<div class="flow-legend" aria-label="direction"><Badge tone="neutral">{{ c('clientIn') }}</Badge><Badge tone="info">{{ c('upstreamOut') }}</Badge><Badge tone="neutral">{{ c('upstreamIn') }}</Badge><Badge tone="info">{{ c('clientOut') }}</Badge></div>
			<p class="capture-hint">{{ c('participantNote') }} {{ c('semanticNote') }} {{ c('headersNote') }}</p>
			<RuleTracePanel v-if="capture" :capture="capture" />
			<div class="segmented capture-tabs" :aria-label="t('common.viewDetails')"><button v-for="item in tabs" :key="item.key" class="segmented-button" :class="{ 'is-active': tab === item.key }" :aria-pressed="tab === item.key" @click="tab = item.key as typeof tab">{{ item.label }}</button></div>

			<section v-if="tab === 'timeline'" class="flow-section">
				<div class="flow-toolbar"><label><input v-model="compact" type="checkbox" />{{ c('compact') }}</label><span>{{ c('compactHint') }}</span></div>
				<section v-if="compact" class="request-stage">
					<div class="flow-stage-heading"><h2>{{ c('requestStage') }}</h2><span>{{ c('requestRoute') }}</span></div>
					<code v-if="capture.url" class="target-url">{{ capture.url }}</code>
					<CaptureDiff :title="c('requestBody')" :before="request.beforeBody" :after="request.afterBody" :before-label="c('clientIn')" :after-label="c('upstreamOut')" :partial="capture.truncated" :sources="sourceLabels('request')" :source-note="sourceNote()" initial-open />
					<CaptureDiff :title="c('requestHeaders')" :before="request.beforeHeaders" :after="request.afterHeaders" :before-label="c('clientIn')" :after-label="c('upstreamOut')" :partial="capture.truncated" />
					<p class="capture-hint">{{ c('localOnly') }}</p>
				</section>
				<div v-if="compact" class="flow-stage-heading response-stage"><h2>{{ c('responseStage') }}</h2><span>{{ c('responseRoute') }}</span></div>
                <details v-for="group in visibleGroups" :key="`${compact}:${group.key}`" class="response-event-group" :class="{'card': group.rows.length > 1}" :open="group.rows.length === 1" @toggle="($event.target as HTMLDetailsElement).open ? expandedGroups.add(group.key) : expandedGroups.delete(group.key)">
                    <summary v-if="group.rows.length > 1" class="event-group-summary" data-testid="response-event-group"><code>{{ c('repeatedEvents', {type: group.type, count: group.rows.length}) }}</code><span>+{{ (group.rows[0]?.before ?? group.rows[0]?.frame)?.at_ms }} → +{{ (group.rows.at(-1)?.after ?? group.rows.at(-1)?.frame)?.at_ms }} ms</span></summary>
                    <div v-if="group.rows.length === 1 || expandedGroups.has(group.key)" class="event-group-body">
				<div v-for="(row, index) in group.rows.slice(0, groupLimits[group.key] ?? 20)" :key="`${compact}:${row.key}`" class="flow-row" :data-paired="Boolean(row.before && row.after)">
					<template v-if="row.before && row.after">
						<div class="flow-event-meta"><span>{{ c('protocolChange', { before: frameProtocol(row.before), after: frameProtocol(row.after) }) }}</span><span>+{{ row.before.at_ms }} → +{{ row.after.at_ms }} ms</span><button v-if="row.before.dir === 'in' && ['sse_event', 'ws_frame'].includes(row.before.kind)" type="button" class="btn" @click="router.push({name:'rules', query:{capture:capture.id, frame:row.before.seq}})">{{ c('debugRules') }}</button></div>
						<CaptureDiff :title="row.before.type === row.after.type ? row.before.type || c('responsePayload') : `${row.before.type || frameLabel(row.before)} → ${row.after.type || frameLabel(row.after)}`" :before="frameSnapshot(row.before)" :after="frameSnapshot(row.after)" :before-label="c('upstreamIn')" :after-label="c('clientOut')" :partial="capture.truncated" :sources="rowSources(row.before, row.after)" :source-note="sourceNote()" :initial-open="group.rows.length === 1 && index < 2" />
					</template>
					<details v-else-if="row.frame" class="card individual-frame" :open="group.rows.length === 1 && index < 2" :data-direction="row.frame.dir" :data-kind="row.frame.kind">
						<summary><component :is="(() => { if (row.frame.kind === 'note') { return Info; } if (row.frame.dir === 'out' || row.frame.dir === 'client_out') { return ArrowUpRight; } return ArrowDownLeft; })()" :size="15" /><strong class="frame-direction">{{ direction(row.frame) }}</strong><Badge :tone="frameTone(row.frame)">{{ frameLabel(row.frame) }}</Badge><code>{{ row.frame.type || '' }}</code><span class="frame-meta">+{{ row.frame.at_ms }} ms · {{ formatBytes(row.frame.bytes) }}</span></summary>
						<div class="frame-content"><button v-if="row.frame.dir === 'in' && ['sse_event', 'ws_frame'].includes(row.frame.kind)" type="button" class="btn" @click="router.push({name:'rules', query:{capture:capture.id, frame:row.frame.seq}})">{{ c('debugRules') }}</button><JsonViewer v-if="frameSnapshot(row.frame).available" :value="preview(row.frame).text" :label="direction(row.frame)" max-height="28rem" /><p v-else class="capture-hint">{{ c('noPayload') }}</p><p v-if="preview(row.frame).limited" class="capture-hint">{{ c('limited') }}</p></div>
					</details>
				</div>
                    <button v-if="group.rows.length > (groupLimits[group.key] ?? 20)" type="button" class="btn" @click="groupLimits[group.key] = (groupLimits[group.key] ?? 20) + 20">{{ c('showMore', {count: group.rows.length - (groupLimits[group.key] ?? 20)}) }}</button>
                    </div>
                </details>
				<button v-if="rowGroups.length > rowLimit" type="button" class="btn" @click="rowLimit += 50">{{ c('showMore', { count: rowGroups.length - rowLimit }) }}</button>
				<p v-if="!rows.length" class="card capture-empty">{{ c('noMessages') }}</p>
			</section>

			<section v-else-if="tab === 'request'" class="flow-section request-comparison">
				<div class="flow-stage-heading"><h2>{{ c('requestStage') }}</h2><span>{{ c('requestRoute') }}</span></div>
				<p class="capture-hint">{{ c('localOnly') }}</p>
				<p v-if="!request.beforeBody.available" class="notice notice-warning">{{ c('requestNotRecorded') }}</p>
				<CaptureDiff :title="c('requestHeaders')" :before="request.beforeHeaders" :after="request.afterHeaders" :before-label="c('clientIn')" :after-label="c('upstreamOut')" :partial="capture.truncated" initial-open />
				<CaptureDiff :title="c('requestBody')" :before="request.beforeBody" :after="request.afterBody" :before-label="c('clientIn')" :after-label="c('upstreamOut')" :partial="capture.truncated" :sources="sourceLabels('request')" :source-note="sourceNote()" initial-open />
			</section>
			<section v-else-if="tab === 'response'" class="flow-section">
				<div class="flow-stage-heading"><h2>{{ c('responseHeaders') }}</h2><span>{{ c('responseRoute') }}</span></div>
				<p class="capture-hint">{{ c('upstreamStatus') }}：{{ capture.status || c('unrecorded') }} · {{ c('clientStatus') }}：{{ responseHeaders.status || c('unrecorded') }}</p>
				<p v-if="!responseHeaders.after.available" class="notice notice-warning">{{ c('responseNotRecorded') }}</p>
				<CaptureDiff :title="c('responseHeaders')" :before="responseHeaders.before" :after="responseHeaders.after" :before-label="c('upstreamIn')" :after-label="c('clientOut')" :partial="capture.truncated" initial-open />
			</section>
			<section v-else class="stream-columns"><CaptureStreamPanel :capture="capture" side="upstream" /><CaptureStreamPanel :capture="capture" side="client" /></section>
		</div>
		<div v-else-if="!loading" class="capture-empty">{{ t('capture.notFound') }}</div>
	</div>
</template>

<style scoped>
.event-group-summary { display:flex; flex-wrap:wrap; justify-content:space-between; gap:8px; padding:12px; font-size:12px; cursor:pointer; }
.event-group-summary span { color:var(--color-ink-muted); font-size:11px; }
.event-group-body { display:grid; gap:10px; }
.response-event-group.card > .event-group-body { padding:12px; border-top:1px solid var(--color-line); }

.capture-content { display: grid; align-content: start; gap: 16px; min-width: 0; }.capture-back { display: flex; align-items: center; gap: 6px; margin-bottom: 8px; color: var(--color-ink-muted); font-size: 12px; }.capture-back:hover { color: var(--color-ink); }.capture-actions { display: flex; flex-wrap: wrap; gap: 8px; }
.capture-summary { display: grid; grid-template-columns: repeat(5, minmax(0, 1fr)); gap: 12px; }.summary-item { display: grid; align-content: start; gap: 6px; min-width: 0; padding: 12px; }.summary-item > span { color: var(--color-ink-faint); font-size: 11px; }.summary-item > strong { font-size: 15px; font-weight: 600; }.summary-item > small { font-size: 10px; color: var(--color-ink-muted); overflow-wrap: anywhere; }
.capture-error { display: flex; align-items: flex-start; gap: 8px; }.capture-error svg { flex-shrink: 0; margin-top: 2px; }.capture-error p { margin: 0; overflow-wrap: anywhere; min-width: 0; }
.flow-legend { display: flex; gap: 6px; flex-wrap: wrap; }.capture-hint { margin: 0; color: var(--color-ink-muted); font-size: 11px; line-height: 1.6; overflow-wrap: anywhere; }
.capture-tabs { display: flex; flex-wrap: wrap; width: fit-content; max-width: 100%; }.capture-tabs .segmented-button { white-space: normal; }
.flow-section, .request-stage { display: grid; gap: 10px; min-width: 0; }.flow-toolbar { display: flex; flex-wrap: wrap; gap: 6px 14px; align-items: center; margin-bottom: 2px; }.flow-toolbar label { display: flex; align-items: center; gap: 7px; font-size: 12px; font-weight: 600; }.flow-toolbar input { accent-color: var(--color-accent); }.flow-toolbar > span { color: var(--color-ink-muted); font-size: 11px; line-height: 1.6; }
.flow-stage-heading { display: flex; flex-wrap: wrap; gap: 5px 12px; align-items: center; }.flow-stage-heading h2 { margin: 0; font-size: 14px; font-weight: 650; }.flow-stage-heading > span { font-size: 11px; color: var(--color-ink-muted); }.response-stage { margin-top: 12px; }.target-url { font-size: 11px; color: var(--color-ink-muted); overflow-wrap: anywhere; }
.flow-row { min-width: 0; }.flow-event-meta { display: flex; justify-content: space-between; gap: 8px; flex-wrap: wrap; margin: 3px 0 6px; font-size: 10px; color: var(--color-ink-faint); }
.individual-frame { min-width: 0; overflow: hidden; }.individual-frame > summary { display: flex; align-items: center; flex-wrap: wrap; gap: 7px; padding: 12px; cursor: pointer; list-style: none; }.individual-frame > summary::-webkit-details-marker { display: none; }.individual-frame > summary svg { flex-shrink: 0; color: var(--color-info); }.frame-direction { font-size: 12px; font-weight: 650; }.individual-frame > summary code { font-size: 11px; color: var(--color-ink-muted); overflow-wrap: anywhere; }.frame-meta { margin-left: auto; font-size: 10px; color: var(--color-ink-faint); }.frame-content { padding: 12px; border-top: 1px solid var(--color-line); min-width: 0; }.capture-empty { padding: 24px; color: var(--color-ink-muted); font-size: 12px; }
.stream-columns { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; align-items: start; min-width: 0; }
@media (max-width: 1100px) { .capture-summary { grid-template-columns: repeat(3, minmax(0, 1fr)); }.stream-columns { grid-template-columns: minmax(0, 1fr); } }
@media (max-width: 600px) { .capture-summary { grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 8px; }.summary-item { padding: 10px; }.capture-content { gap: 12px; }.capture-tabs { width: 100%; }.capture-tabs .segmented-button { flex: 1 1 42%; }.frame-meta { margin-left: 0; flex-basis: 100%; }.frame-content { padding: 8px; } }
</style>
