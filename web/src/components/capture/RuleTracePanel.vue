<script setup lang="ts">
import { computed, ref } from 'vue'
import type { Capture, CaptureRuleTrace } from '../../api/types'
import { useI18n } from '../../i18n'
import { isRuleChange, lastRuleWriters, ruleChangeSnapshot, groupRuleTraces } from '../../utils/captureInspection'
import { useCaptureLocale } from './captureLocale'
import CaptureDiff from './CaptureDiff.vue'
import Badge from '../Badge.vue'
import { ruleLabel } from '../../utils/ruleLabels'
import { traceRuleName } from '../../utils/ruleTracePresentation'

const props = defineProps<{ capture: Capture }>()
const { t, locale } = useI18n()
const { c } = useCaptureLocale()
const limit = ref(50)
const expanded = ref(new Set<string>())
const openRules = ref(new Set<string>())
const traces = computed(() => groupRuleTraces(props.capture))
const visible = computed(() => traces.value.slice(0, limit.value))
const finalWriters = computed(() => lastRuleWriters(props.capture))
const eventFrames = computed(() => {
    const result = new Map<string, {before: number[]; after: number[]}>()
    for (const frame of props.capture.response_frames ?? []) {
        if (!frame.rule_event_id) { continue }
        const item = result.get(frame.rule_event_id) ?? {before: [], after: []}
        if (frame.dir === 'in') { item.before.push(frame.seq) }
        if (frame.dir === 'client_out') { item.after.push(frame.seq) }
        result.set(frame.rule_event_id, item)
    }
    return result
})
const statusKeys: Record<string, string> = {
    not_matched: 'statusNotMatched', no_change: 'statusNoChange', changed: 'statusChanged',
    skipped: 'statusSkipped', error: 'statusError', blocked: 'statusBlocked', dropped: 'statusDropped', rolled_back: 'statusRolledBack',
}
function actionLabel(action?: string) { return action ? ruleLabel('action', action, locale.value) : '' }
function phaseLabel(phase?: string) {
    const key = (() => {
    if (phase === 'request') {
        return 'phaseRequest'
    }
    if (phase === 'response_event') {
        return 'phaseResponseEvent'
    }
    if (phase === 'response_body') {
        return 'phaseResponseBody'
    }
    return ''
})()
    return key ? t(`capture.rules.${key}`) : phase || c('unrecorded')
}
function statusLabel(trace: CaptureRuleTrace) {
    if (trace.rolled_back) { return t('capture.rules.statusRolledBack') }
    const key = Object.hasOwn(statusKeys, trace.status ?? '') ? statusKeys[trace.status!] : ''
    return key ? t(`capture.rules.${key}`) : trace.status || c('unrecorded')
}
function tone(trace: CaptureRuleTrace) {
    return (() => {
    if (trace.error || trace.status === 'error' || trace.status === 'blocked') {
        return 'danger'
    }
    if (trace.rolled_back || trace.status === 'dropped') {
        return 'warn'
    }
    if (trace.status === 'changed') {
        return 'info'
    }
    return 'neutral'
})()
}
function toggle(index: string, event: Event) {
    if ((event.target as HTMLDetailsElement).open) { expanded.value.add(index) }
    else { expanded.value.delete(index) }
}
// Show one representative per array collection/path instead of hundreds of leaf edits.
function summarizedChanges(trace: CaptureRuleTrace) {
    const groups: {change: NonNullable<CaptureRuleTrace['changes']>[number]; index: number; count: number; path: string}[] = []
    const seen = new Map<string, typeof groups[number]>()
    for (const [index, change] of (trace.changes ?? []).entries()) {
        const path = change.path.replace(/\/(?:0|[1-9]\d*)(?=\/|$).*/, '/*')
        const key = `${change.operation}:${path}`
        const previous = seen.get(key)
        if (previous) { previous.count++ }
        else { const group = {change, index, count: 1, path}; seen.set(key, group); groups.push(group) }
    }
    return groups
}
</script>

<template>
    <section class="rule-trace-panel" aria-labelledby="rule-trace-title">
        <div class="trace-heading"><h2 id="rule-trace-title">{{ t('capture.rules.title') }}</h2><span>{{ t('capture.rules.version', { version: capture.rules_version ?? 0 }) }}</span></div>
        <output v-if="capture.rules_trace_truncated || capture.rules_trace_omitted" class="notice notice-warning" >{{ t('capture.rules.truncated', { count: capture.rules_trace_omitted ?? 0 }) }}</output>
        <p v-if="!traces.length" class="trace-hint">{{ !capture.rules_version && !capture.rules_trace_truncated ? t('capture.rules.legacy') : t('capture.rules.noChanges') }}</p>
        <p v-else class="trace-hint">{{ t('capture.rules.imprecise') }}</p>
        <details v-for="group in visible" :key="group.key" class="card trace-step" data-testid="trace-rule-group" @toggle="($event.target as HTMLDetailsElement).open ? openRules.add(group.key) : openRules.delete(group.key)">
            <summary><strong>{{ traceRuleName(group.trace, locale) }}</strong><Badge :tone="tone(group.status)">{{ statusLabel(group.status) }}</Badge><span>{{ phaseLabel(group.trace.phase) }}</span><span>{{ c('traceSummary', {steps: group.steps.length, count: group.count, changes: group.changes}) }}</span></summary>
            <div v-if="openRules.has(group.key)" class="trace-body">
            <details v-for="({trace, index, count, key}, stepIndex) in group.steps.slice(0, limit)" :key="key" class="trace-step" :data-rule-status="trace.status" @toggle="toggle(key, $event)">
                <summary><span class="trace-order">{{ stepIndex + 1 }}.</span><code>{{ actionLabel(trace.action_type) || t('rules.when') }}</code><Badge :tone="tone(trace)">{{ statusLabel(trace) }}</Badge><span v-if="count > 1">× {{ count }}</span><span>{{ trace.event_type }}</span></summary>
            <div v-if="expanded.has(key)" class="trace-body">
                <p v-if="count > 1" class="trace-hint">{{ c('traceSample', {count}) }}</p>
                <div class="trace-meta">
                    <code v-if="trace.rule_id">{{ trace.rule_id }}</code>
                    <span v-if="trace.revision !== undefined">{{ t('capture.rules.revision', { value: trace.revision }) }}</span>
                    <span v-if="trace.priority !== undefined">{{ t('capture.rules.priority', { value: trace.priority }) }}</span>
                    <span>{{ t('capture.rules.version', { version: trace.rules_version ?? capture.rules_version ?? 0 }) }}</span>
                    <span v-if="trace.action_id">{{ t('capture.rules.action', { index: (trace.action_index ?? 0) + 1 }) }} · <code>{{ trace.action_id }}</code></span>
                    <span v-if="typeof trace.duration_ns === 'number'">{{ (trace.duration_ns / 1_000_000).toFixed(3) }} ms</span>
                </div>
                <p v-if="trace.event_id" class="trace-hint">{{ t('capture.rules.event') }}: <code>{{ trace.event_id }}</code> · {{ trace.event_type || '' }}<span v-if="trace.sequence !== undefined"> #{{ trace.sequence }}</span></p>
                <p v-if="trace.event_id && eventFrames.has(trace.event_id)" class="trace-hint">{{ c('upstreamIn') }}: {{ eventFrames.get(trace.event_id)?.before.map(seq => `#${seq}`).join(', ') || c('unrecorded') }} → {{ c('clientOut') }}: {{ eventFrames.get(trace.event_id)?.after.map(seq => `#${seq}`).join(', ') || c('unrecorded') }}</p>
                <p v-if="trace.status === 'blocked'" class="notice notice-error">{{ t('capture.rules.blocked') }}</p>
                <p v-if="trace.error" class="notice notice-error">{{ trace.error }}</p>
                <p v-if="trace.trace_truncated || trace.omitted_changes" class="notice notice-warning">{{ t('capture.rules.omittedChanges', { count: trace.omitted_changes ?? 0 }) }}</p>
                <div v-for="({change, index: changeIndex, count: changeCount, path}) in isRuleChange(trace) ? summarizedChanges(trace).slice(0, limit) : []" :key="changeIndex" class="trace-change">
                    <div class="trace-meta"><code>{{ change.operation }} {{ c('changeGroup', {path: path || '/', count: changeCount}) }}</code><span v-if="count === 1 && changeCount === 1 && finalWriters.has(`${index}:${changeIndex}`)">{{ t('capture.rules.finalWriter') }}</span></div>
                    <p v-if="changeCount > 1" class="trace-hint">{{ c('traceSample', {count: changeCount}) }}</p>
                    <CaptureDiff :title="change.path || '/'" :before="ruleChangeSnapshot(change, 'before')" :after="ruleChangeSnapshot(change, 'after')" :before-label="change.before_exists === false ? t('capture.rules.missing') : c('before')" :after-label="change.after_exists === false ? t('capture.rules.missing') : c('after')" :partial="Boolean(change.truncated || trace.trace_truncated)" />
                </div>
                <button v-if="isRuleChange(trace) && summarizedChanges(trace).length > limit" type="button" class="btn" @click="limit += 50">{{ c('showMore', {count: summarizedChanges(trace).length - limit}) }}</button>
            </div>
            </details>
            <button v-if="group.steps.length > limit" type="button" class="btn" @click="limit += 50">{{ c('showMore', {count: group.steps.length - limit}) }}</button>
            </div>
        </details>
        <button v-if="traces.length > limit" type="button" class="btn" @click="limit += 50">{{ c('showMore', { count: traces.length - limit }) }}</button>
    </section>
</template>

<style scoped>
.trace-step > summary { list-style: none; padding: 12px; cursor: pointer; display: flex; align-items: center; flex-wrap: wrap; gap: 7px; font-size: 12px; overflow-wrap: anywhere; }
.trace-step > summary::-webkit-details-marker { display: none; }
.trace-step > summary::before { content: '›'; color: var(--color-ink-muted); font-size: 16px; line-height: 1; flex-shrink: 0; transition: transform .15s; }
.trace-step[open] > summary::before { transform: rotate(90deg); }
.rule-trace-panel { display: grid; gap: 10px; min-width: 0; }.trace-heading, .trace-meta { display: flex; flex-wrap: wrap; align-items: center; gap: 6px 12px; }.trace-heading h2 { font-size: 14px; margin: 0; }.trace-heading > span, .trace-meta, .trace-hint { font-size: 11px; color: var(--color-ink-muted); line-height: 1.6; overflow-wrap: anywhere; }.trace-hint { margin: 0; }.trace-step { min-width: 0; overflow: hidden; }.trace-order { color: var(--color-ink-faint); }.trace-body { border-top: 1px solid var(--color-line); padding: 12px; display: grid; gap: 10px; min-width: 0; }.trace-change { display: grid; gap: 6px; min-width: 0; }.trace-body code { overflow-wrap: anywhere; }.notice { margin: 0; overflow-wrap: anywhere; }
</style>
