<script setup lang="ts">
import { computed, ref, useId } from 'vue'
import { RefreshCw, Search, ShieldBan } from 'lucide-vue-next'
import type { Account } from '../../api/types'
import { formatTime } from '../../stores/ui'
import { useAccountControls } from './accountControlsLocale'
import { isCodexOnlyModel, manualCatalogModel, modelCapabilities, modelMetadataJSON, modelMetadataPreview, modelOriginNames, useAccountModelCatalog } from './useAccountModelCatalog'
import ModelCatalogSourceStatus from './ModelCatalogSourceStatus.vue'

const props = withDefaults(defineProps<{
	sources: Account[]
	inherited?: Account['inherited_model_restrictions']
	groupMode?: boolean
	disabled?: boolean
	supplementalModels?: string[]
}>(), { inherited: () => [], groupMode: false, disabled: false, supplementalModels: () => [] })
const selected = defineModel<string[]>({ required: true })
const { c } = useAccountControls()
const id = useId()
const search = ref('')
const { models, sourceId, selectedCatalog, issues, loading, refreshing, readCache, refresh, accountName } = useAccountModelCatalog(computed(() => props.sources))
const selectedAccount = computed(() => props.sources.find(account => account.id === sourceId.value))
const capabilityLabels: Record<string, Parameters<typeof c>[0]> = {
	supported_reasoning_levels: 'capabilityReasoningLevels',
	default_reasoning_level: 'capabilityDefaultReasoning',
	supports_reasoning_summary_parameter: 'capabilityReasoningSummary',
	default_reasoning_summary: 'capabilityDefaultSummary',
	context_window: 'capabilityContextWindow',
	max_context_window: 'capabilityMaxContextWindow',
	max_output_tokens: 'capabilityMaxOutput',
	input_modalities: 'capabilityInputModalities',
	output_modalities: 'capabilityOutputModalities',
	supports_parallel_tool_calls: 'capabilityParallelTools',
}
function capabilityLabel(key: string) {
	const label = capabilityLabels[key]
	return label ? c(label) : key
}
const inheritedByModel = computed(() => new Map(props.inherited.map(item => [item.model, item.group_names])))
const choices = computed(() => {
	const listed = models.value.map(model => ({ ...model, missing: false }))
	const existing = new Set(listed.map(model => model.id))
	for (const model of props.supplementalModels) {
		if (!existing.has(model)) {
			listed.push({ ...manualCatalogModel(model), missing: false })
			existing.add(model)
		}
	}
	for (const model of new Set([...selected.value, ...inheritedByModel.value.keys()])) {
		if (!existing.has(model)) { listed.push({ id: model, name: model, sources: [], missing: true }) }
	}
	const query = search.value.trim().toLowerCase()
	return listed.filter(model => !query || `${model.name} ${model.id} ${model.sources.join(' ')}`.toLowerCase().includes(query))
		.map(model => ({ ...model, capabilities: modelCapabilities(model.metadata), metadataPreview: modelMetadataPreview(model.metadata) }))
})
function downloadMetadata(metadata: unknown) {
	const json = modelMetadataJSON(metadata)
	if (json === undefined) { return }
	const url = URL.createObjectURL(new Blob([json], { type: 'application/json;charset=utf-8' }))
	const link = document.createElement('a')
	link.href = url
	link.download = 'model-metadata.json'
	document.body.appendChild(link)
	link.click()
	link.remove()
	setTimeout(() => URL.revokeObjectURL(url), 1000)
}
function toggle(model: string, checked: boolean) {
	if (props.disabled || inheritedByModel.value.has(model)) { return }
	selected.value = checked ? [...new Set([...selected.value, model])] : selected.value.filter(value => value !== model)
}
</script>

<template>
	<section class="restriction-selector" :aria-label="c('disabledModels')">
		<div class="restriction-heading"><h3><ShieldBan :size="15" />{{ c('disabledModels') }}</h3><span>{{ c('disabledCount', { count: selected.length }) }}</span></div>
		<p v-if="groupMode" class="control-hint">{{ c('groupCatalogHint') }}</p>
		<div class="catalog-toolbar">
			<label v-if="groupMode" :for="`${id}-source`" class="source-label">{{ c('source') }}<select :id="`${id}-source`" v-model="sourceId" class="input" :disabled="!sources.length || loading || refreshing || disabled"><option v-for="account in sources" :key="account.id" :value="account.id">{{ accountName(account) }}{{ account.email && account.email !== account.name ? ` · ${account.email}` : '' }}</option></select></label>
			<button type="button" class="btn" :disabled="!sources.length || loading || refreshing || disabled" @click="refresh"><RefreshCw :size="14" :class="{ 'animate-spin': refreshing }" />{{ refreshing ? c('fetchingModels') : c('fetchModels') }}</button>
			<button type="button" class="btn btn-ghost" :disabled="!sources.length || loading || refreshing || disabled" @click="readCache">{{ c('readCache') }}</button>
		</div>
		<p v-if="selectedCatalog" class="control-hint">{{ selectedCatalog.fetched_at ? c('cacheAt', { time: formatTime(selectedCatalog.fetched_at) }) : c('neverFetched') }}<span v-if="selectedCatalog.attempted_at && selectedCatalog.error"> · {{ c('failedAt', { time: formatTime(selectedCatalog.attempted_at) }) }}</span></p>
		<ModelCatalogSourceStatus v-if="selectedAccount" :account="selectedAccount" :catalog="selectedCatalog" />
		<p v-for="issue in issues" :key="issue" class="control-warning" role="alert">{{ c('catalogWarnings', { message: issue }) }}</p>
		<p class="control-hint">{{ c('catalogHint') }}</p>
		<p class="control-hint">{{ c('catalogPermissionHint') }}</p>
		<label class="search-field model-search"><Search :size="16" aria-hidden="true" /><input v-model="search" class="input" :placeholder="c('searchModels')" :aria-label="c('searchModels')" /></label>
		<output v-if="loading" class="control-hint" >{{ c('loadingModels') }}</output>
		<div v-if="choices.length" class="model-choices">
			<div v-for="(model, index) in choices" :key="model.id" class="model-choice" :class="{ inherited: inheritedByModel.has(model.id) }">
				<input :id="`${id}-model-${index}`" type="checkbox" :checked="selected.includes(model.id) || inheritedByModel.has(model.id)" :disabled="disabled || inheritedByModel.has(model.id)" @change="toggle(model.id, ($event.target as HTMLInputElement).checked)" />
				<div class="model-copy">
					<label :for="`${id}-model-${index}`"><strong>{{ model.name }}</strong></label>
					<code v-if="model.name !== model.id">{{ model.id }}</code>
					<small v-if="model.description">{{ model.description }}</small>
					<small v-if="!model.missing">{{ model.source === 'manual' ? c('manualModel') : modelOriginNames(model) || c('upstreamModel') }}</small>
					<small v-if="model.sources.length">{{ c('sourceAccounts', { names: model.sources.join('、') }) }}</small>
					<small v-if="isCodexOnlyModel(model)" class="model-warning">{{ c('codexOnlyModel') }}</small>
					<small v-if="model.capabilities.length" class="model-capabilities">{{ c('modelCapabilities') }}：<span v-for="capability in model.capabilities" :key="capability.key">{{ capabilityLabel(capability.key) }}={{ capability.value }}</span></small>
					<details v-if="model.metadata !== undefined" class="model-metadata">
						<summary>{{ c('modelMetadataDetails') }}<span v-if="model.metadataPreview.truncated"> · {{ c('modelMetadataTruncated') }}</span></summary>
						<pre v-if="model.metadataPreview.failed">{{ c('modelMetadataUnavailable') }}</pre>
						<pre v-else>{{ model.metadataPreview.text }}</pre>
						<button v-if="model.metadataPreview.truncated && !model.metadataPreview.failed" type="button" class="btn btn-ghost" @click.stop="downloadMetadata(model.metadata)">{{ c('downloadModelMetadata') }}</button>
					</details>
					<small v-if="model.missing" class="model-warning">{{ c('missingModel') }}</small>
					<small v-if="selected.includes(model.id)">{{ c('localDisabled') }}</small>
					<small v-if="inheritedByModel.has(model.id)" class="model-warning">{{ c('inherited', { groups: inheritedByModel.get(model.id)!.join('、') }) }}</small>
				</div>
			</div>
		</div>
		<output v-else-if="!loading" class="control-hint" >{{ (() => { if (search.trim()) { return c('noMatches'); } if (sources.length) { return c('emptyCatalog'); } return c('noSources'); })() }}</output>
		<p v-if="inherited.length" class="control-hint">{{ c('inheritedHint') }}</p>
	</section>
</template>

<style scoped>
.restriction-selector { display: grid; gap: 10px; min-width: 0; padding: 14px; border: 1px solid var(--color-line); border-radius: 10px; background: var(--color-surface); }
.restriction-heading { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 6px; }
.restriction-heading h3 { display: flex; gap: 7px; align-items: center; margin: 0; font-size: 13px; font-weight: 650; }
.restriction-heading > span { color: var(--color-ink-faint); font-size: 11px; }
.catalog-toolbar { display: flex; min-width: 0; flex-wrap: wrap; align-items: flex-end; gap: 8px; }
.catalog-toolbar .btn { min-height: 32px; font-size: 11px; }
.source-label { display: grid; flex: 1 1 180px; gap: 6px; min-width: 0; font-size: 11px; color: var(--color-ink-muted); }
.source-label .input { width: 100%; min-width: 0; }
.model-search { width: 100%; min-width: 0; }
.model-choices { max-height: 270px; overflow-y: auto; overscroll-behavior: contain; border: 1px solid var(--color-line); border-radius: 8px; }
.model-choice { display: flex; align-items: flex-start; gap: 10px; min-width: 0; padding: 10px; cursor: pointer; }
.model-choice + .model-choice { border-top: 1px solid var(--color-line); }
.model-choice:hover { background: var(--color-surface-2); }
.model-choice input { flex-shrink: 0; margin-top: 3px; accent-color: var(--color-accent); }
.model-choice.inherited { cursor: default; }
.model-copy { display: grid; min-width: 0; gap: 3px; overflow-wrap: anywhere; }
.model-copy strong { font-size: 12px; font-weight: 600; }
.model-copy code, .model-copy small { color: var(--color-ink-faint); font-size: 10px; }
.model-copy .model-warning { color: var(--color-warn); }
.model-capabilities { display: flex; flex-wrap: wrap; gap: 3px 8px; }
.model-metadata { margin-top: 2px; color: var(--color-ink-faint); font-size: 10px; }
.model-metadata summary { cursor: pointer; }
.model-metadata pre { max-height: 180px; overflow: auto; margin: 4px 0; padding: 6px; white-space: pre-wrap; overflow-wrap: anywhere; background: var(--color-surface-2); border-radius: 4px; }
.model-metadata .btn { min-height: 24px; padding: 2px 6px; font-size: 10px; }
.control-hint, .control-warning { margin: 0; font-size: 11px; line-height: 1.6; overflow-wrap: anywhere; }
.control-hint { color: var(--color-ink-faint); }.control-warning { color: var(--color-warn); }
@media (max-width: 390px) { .restriction-selector { padding: 10px; } .source-label { flex-basis: 100%; } .catalog-toolbar .btn { flex: 1; padding-inline: 6px; } }
</style>
