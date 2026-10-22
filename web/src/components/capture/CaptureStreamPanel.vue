<script setup lang="ts">
import { computed, ref } from 'vue'
import type { Capture } from '../../api/types'
import { groupCaptureRows, frameSnapshot, payloadObject, payloadPreview as preview, requestSnapshots, responseHeaderSnapshots, streamSnapshot } from '../../utils/captureInspection'
import JsonViewer from '../JsonViewer.vue'
import Badge from '../Badge.vue'
import { useCaptureLocale } from './captureLocale'

const props = defineProps<{ capture: Capture; side: 'upstream' | 'client' }>()
const { c } = useCaptureLocale()
const snapshot = computed(() => streamSnapshot(props.capture, props.side))
const direction = computed(() => c(props.side === 'upstream' ? 'upstreamIn' : 'clientOut'))
const status = computed(() => props.side === 'upstream' ? props.capture.status : responseHeaderSnapshots(props.capture).status)
const explanation = computed(() => {
	if (snapshot.value.text) { return '' }
	if (props.side === 'upstream' && props.capture.status >= 400 && snapshot.value.http.length) { return c('noSseHttpError', { status: props.capture.status }) }
	if (props.side === 'client' && !snapshot.value.frames.some(frame => ['http_response', 'ws_frame', 'ws_binary', 'sse_event'].includes(frame.kind))) { return c('noClientResponse') }
	if (snapshot.value.protocol === 'ws') { return c('noSseWs') }
	if (props.side === 'client' && payloadObject(requestSnapshots(props.capture).beforeBody.value)?.stream === false) { return c('noSseAggregate') }
	if (props.side === 'client' && snapshot.value.http.length && (status.value ?? 0) >= 400) { return c('noSseClientError') }
	return c('noSseRecorded')
})
const socketGroups = computed(() => groupCaptureRows(snapshot.value.socket.map(frame => ({key:String(frame.seq), frame}))))
const openGroups = ref(new Set<string>())
const limits = ref<Record<string, number>>({})
</script>

<template>
	<section class="stream-panel card" :data-side="side">
		<header><h3>{{ direction }}</h3><Badge tone="info">{{ c(snapshot.protocol) }}</Badge><Badge v-if="status">{{ c('status', { status }) }}</Badge><small v-else>{{ c('unrecorded') }}</small></header>
		<output v-if="explanation" class="stream-explanation" >{{ explanation }}</output>
		<template v-if="snapshot.text">
			<p class="stream-hint">{{ c(side === 'upstream' || snapshot.reconstructed ? 'reconstructed' : 'clientSseNote') }}</p>
			<JsonViewer :value="snapshot.text" raw :label="c(side === 'upstream' ? 'upstreamSse' : 'clientSse')" max-height="38rem" />
			<p v-if="snapshot.limited" class="stream-hint">{{ c('limited') }}</p>
		</template>
		<div v-if="snapshot.http.length" class="stream-messages">
			<h4>{{ direction }} · {{ c(side === 'upstream' ? 'capturedErrors' : 'capturedHttp') }}</h4>
			<div v-for="frame in snapshot.http" :key="frame.seq"><JsonViewer :value="preview(frameSnapshot(frame).value).text" :label="`${direction} · ${frame.type || c('httpResponse')}`" max-height="28rem" /><p v-if="preview(frameSnapshot(frame).value).limited" class="stream-hint">{{ c('limited') }}</p></div>
		</div>
		<div v-if="snapshot.socket.length" class="stream-messages">
			<h4>{{ direction }} · {{ c('capturedWs') }}</h4>
            <details v-for="group in socketGroups" :key="group.key" :open="group.rows.length === 1" @toggle="($event.target as HTMLDetailsElement).open ? openGroups.add(group.key) : openGroups.delete(group.key)">
                <summary>{{ c('repeatedEvents', {type:group.type || c('wsFrame'), count:group.rows.length}) }}</summary>
                <div v-if="group.rows.length === 1 || openGroups.has(group.key)">
                    <JsonViewer v-for="row in group.rows.slice(0, limits[group.key] ?? 20)" :key="row.key" :value="preview(frameSnapshot(row.frame).value).text" :label="`${direction} · #${row.frame?.seq}`" max-height="24rem" />
                    <button v-if="group.rows.length > (limits[group.key] ?? 20)" class="btn" @click="limits[group.key] = (limits[group.key] ?? 20) + 20">{{ c('showMore', {count:group.rows.length - (limits[group.key] ?? 20)}) }}</button>
                </div>
            </details>
		</div>
	</section>
</template>

<style scoped>
.stream-messages > details { min-width: 0; border: 1px solid var(--color-line); border-radius: 6px; overflow: hidden; }
.stream-messages > details > summary { display: flex; align-items: center; gap: 8px; padding: 10px; cursor: pointer; font-size: 12px; overflow-wrap: anywhere; list-style: none; }
.stream-messages > details > summary::-webkit-details-marker { display: none; }
.stream-messages > details > summary::before { content: '›'; color: var(--color-ink-muted); font-size: 16px; line-height: 1; flex-shrink: 0; transition: transform .15s; }
.stream-messages > details[open] > summary::before { transform: rotate(90deg); }
.stream-messages > details > div { display: grid; gap: 10px; padding: 10px; border-top: 1px solid var(--color-line); min-width: 0; }
.stream-panel { display: grid; grid-template-columns: minmax(0, 1fr); align-content: start; gap: 12px; padding: 14px; min-width: 0; }
.stream-panel > header { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }.stream-panel h3 { font-size: 13px; font-weight: 650; margin: 0; }.stream-panel small { color: var(--color-ink-faint); }
.stream-explanation { margin: 0; font-size: 12px; line-height: 1.65; padding: 10px; border-radius: 6px; border: 1px solid var(--color-line); background: var(--color-surface-2); overflow-wrap: anywhere; }
.stream-hint { margin: 0; color: var(--color-ink-muted); font-size: 11px; line-height: 1.6; }
.stream-messages { display: grid; grid-template-columns: minmax(0, 1fr); gap: 10px; min-width: 0; }.stream-messages > div { min-width: 0; }.stream-messages h4 { font-size: 12px; margin: 0; font-weight: 600; overflow-wrap: anywhere; }
.stream-panel :deep(.code-header > span) { min-width: 0; overflow-wrap: anywhere; }
@media (max-width: 390px) { .stream-panel { padding: 10px; } }
</style>
