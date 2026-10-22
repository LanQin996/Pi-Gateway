<script setup lang="ts">
import { computed, ref } from 'vue'
import { ChevronDown } from 'lucide-vue-next'
import { payloadPreview as preview, type CaptureSnapshot } from '../../utils/captureInspection'
import { diffPayloads, equalPayloads } from '../../utils/captureDiff'
import JsonViewer from '../JsonViewer.vue'
import Badge from '../Badge.vue'
import { useCaptureLocale } from './captureLocale'
import { useI18n } from '../../i18n'

const props = withDefaults(defineProps<{
	title: string
	before: CaptureSnapshot
	after: CaptureSnapshot
	beforeLabel: string
	afterLabel: string
	initialOpen?: boolean
	partial?: boolean
	sources?: string[]
	sourceNote?: string
}>(), { initialOpen: false, partial: false, sources: () => [], sourceNote: '' })
const { c } = useCaptureLocale()
const { t } = useI18n()
const open = ref(props.initialOpen)
const complete = computed(() => props.before.available && props.after.available)
const same = computed(() => complete.value && equalPayloads(props.before.value, props.after.value))
const diff = computed(() => open.value && complete.value ? diffPayloads(props.before.value, props.after.value) : null)
const state = computed(() => (() => {
    if (!complete.value) {
        return 'unavailable'
    }
    if (props.partial) {
        return 'partial'
    }
    if (same.value) {
        return 'unchanged'
    }
    if (!diff.value) {
        return 'inspectDiff'
    }
    if (diff.value.limited) {
        return 'previewLimited'
    }
    return 'changed'
})())
const single = computed(() => props.after.available ? props.after : props.before)
const singleLabel = computed(() => props.after.available ? props.afterLabel : props.beforeLabel)
</script>

<template>
	<details class="capture-diff card" :data-state="state" :open="open" @toggle="open = ($event.target as HTMLDetailsElement).open">
		<summary class="diff-summary">
			<strong>{{ title }}</strong>
			<Badge :tone="(() => { if (state === 'changed') { return 'warn'; } if (state === 'unchanged') { return 'success'; } return 'neutral'; })()">{{ c(state === 'partial' ? 'partial' : state) }}</Badge>
			<ChevronDown :size="15" class="diff-chevron" :class="{ rotated: open }" />
		</summary>
		<div class="diff-directions"><span>{{ beforeLabel }}</span><span aria-hidden="true">→</span><span>{{ afterLabel }}</span></div>
		<div v-if="sources?.length || sourceNote" class="diff-sources" role="note">
			<strong>{{ t('capture.rules.sources') }}</strong>
			<span v-for="(source, index) in sources" :key="index" class="diff-source">{{ source }}</span>
			<span v-if="sourceNote" class="capture-note">{{ sourceNote }}</span>
		</div>
		<div v-if="open" class="diff-body">
			<template v-if="!complete">
				<p class="capture-note">{{ c('unavailable') }}</p>
				<JsonViewer v-if="single.available" :value="preview(single.value).text" :label="singleLabel" max-height="28rem" />
				<p v-else class="capture-note">{{ c('noPayload') }}</p>
				<p v-if="single.available && preview(single.value).limited" class="capture-note">{{ c('limited') }}</p>
			</template>
			<template v-else-if="diff">
				<div v-if="!same" class="diff-legend"><span class="removed-label">{{ c('removed') }} · {{ beforeLabel }}</span><span class="added-label">{{ c('added') }} · {{ afterLabel }}</span><code>{{ c('changes', { added: diff.added, removed: diff.removed }) }}</code></div>
				<output v-if="diff.limited" class="capture-note" >{{ c('limited') }}</output>
				<p v-if="partial" class="capture-note">{{ c('partial') }}</p>
				<!-- Overflow must remain keyboard-scrollable, including when no links/buttons occur in the payload. -->
				<div class="diff-lines" :aria-label="title" tabindex="0">
					<div v-for="(line, index) in diff.lines" :key="index" class="diff-line" :class="`diff-${line.kind}`">
						<span class="line-number" aria-hidden="true">{{ line.beforeLine ?? '' }}</span><span class="line-number" aria-hidden="true">{{ line.afterLine ?? '' }}</span><span class="line-sign">{{ (() => { if (line.kind === 'added') { return '+'; } if (line.kind === 'removed') { return '−'; } return ' '; })() }}</span><code>{{ line.text || ' ' }}</code>
					</div>
				</div>
				<details v-if="!same" class="recorded-values"><summary>{{ c('rawValues') }}</summary><div class="recorded-grid"><JsonViewer :value="preview(before.value).text" :label="beforeLabel" max-height="24rem" /><JsonViewer :value="preview(after.value).text" :label="afterLabel" max-height="24rem" /></div><p v-if="preview(before.value).limited || preview(after.value).limited" class="capture-note">{{ c('limited') }}</p></details>
			</template>
		</div>
	</details>
</template>

<style scoped>
.capture-diff { min-width: 0; overflow: hidden; }
.diff-summary { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; padding: 12px 14px 6px; cursor: pointer; list-style: none; }
.diff-summary::-webkit-details-marker { display: none; }
.diff-summary strong { font-size: 13px; font-weight: 650; min-width: 0; overflow-wrap: anywhere; }
.diff-summary :deep(.badge) { min-width: 0; white-space: normal; overflow-wrap: anywhere; }
.diff-chevron { flex-shrink: 0; margin-left: auto; transition: transform .15s ease; }.diff-chevron.rotated { transform: rotate(180deg); }
.diff-directions { display: flex; flex-wrap: wrap; gap: 7px; padding: 0 14px 12px; color: var(--color-ink-muted); font-size: 11px; }
.diff-sources { display: flex; flex-wrap: wrap; gap: 5px 10px; padding: 0 14px 12px; font-size: 11px; overflow-wrap: anywhere; }.diff-sources > .capture-note { flex-basis: 100%; }.diff-source { color: var(--color-ink-muted); }
.diff-body { display: grid; gap: 10px; min-width: 0; padding: 12px; border-top: 1px solid var(--color-line); }
.diff-legend { display: flex; flex-wrap: wrap; gap: 6px 14px; align-items: center; font-size: 11px; }
.removed-label { color: var(--color-danger); }.added-label { color: var(--color-success); }
.diff-legend > code { margin-left: auto; color: var(--color-ink-muted); }
.diff-lines { max-height: 32rem; min-width: 0; overflow: auto; border: 1px solid var(--color-line); border-radius: 6px; background: var(--color-canvas); }
.diff-line { display: grid; grid-template-columns: 34px 34px 18px minmax(0, 1fr); padding: 2px 8px 2px 2px; min-width: 0; font: 11px/1.6 var(--font-mono, monospace); }
.diff-line code { white-space: pre-wrap; overflow-wrap: anywhere; min-width: 0; font: inherit; }
.line-number { padding-right: 7px; text-align: right; color: var(--color-ink-faint); user-select: none; }.line-sign { white-space: pre; user-select: none; }
.diff-added { background: var(--color-success-soft); color: var(--color-success); }.diff-removed { background: var(--color-danger-soft); color: var(--color-danger); }
.capture-note { margin: 0; color: var(--color-ink-muted); font-size: 11px; line-height: 1.6; overflow-wrap: anywhere; }
.recorded-values > summary { cursor: pointer; color: var(--color-ink-muted); font-size: 11px; }.recorded-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 10px; margin-block: 10px; }
@media (max-width: 640px) { .recorded-grid { grid-template-columns: minmax(0, 1fr); }.diff-line { grid-template-columns: 26px 26px 14px minmax(0, 1fr); font-size: 10px; }.diff-summary { padding-inline: 10px; }.diff-body { padding: 8px; } }
</style>
