<script setup lang="ts">
import {computed, nextTick, onBeforeUnmount, onMounted, ref, watch} from 'vue'
import type {RulePhase, RuleSchema} from '../../api/rules'
import {useI18n} from '../../i18n'
import {editorId} from '../../utils/editorId'
import {ruleLabel, ruleOptionLabel} from '../../utils/ruleLabels'
import {localHelp} from '../../utils/ruleSchema'

const props = defineProps<{
    open: boolean
    position: {x: number; y: number}
    schema: RuleSchema
    phase: RulePhase
    kind?: 'condition' | 'action' | 'value'
    wrap?: boolean
    context: string
}>()
const emit = defineEmits<{
    select: [value: {kind: 'condition' | 'action' | 'value'; type: string}]
    combination: [value: 'tool' | 'tools' | 'field' | 'model']
    close: []
}>()
const {t, locale} = useI18n()
const popup = ref<HTMLElement>()
const input = ref<HTMLInputElement>()
const query = ref('')
const filter = ref<'' | 'condition' | 'action'>('')
const active = ref(0)
const listID = `node-search-${editorId()}`
const viewport = ref({width: 1024, height: 768})
const entries = computed(() => {
    const text = query.value.trim().toLocaleLowerCase()
    if (props.kind==='value') {
        const computed = props.schema.value_expressions?.find(cap=>cap.id==='computed')?.fields.find(field=>field.name==='op')
        return ['literal','reference',...(computed?.enum??[])].map(type=>({kind:'value' as const,type,label:['literal','reference'].includes(type)?t(`rules.canvas.value.${type}`):ruleOptionLabel(type,locale.value),help:computed?.enum_help?.[type]??t('rules.canvas.valueHelp')})).filter(entry=>!text||[entry.type,entry.label,entry.help].some(value=>value.toLowerCase().includes(text)))
    }
    const modules = ([['condition', props.schema.conditions], ['action', props.schema.actions]] as const).flatMap(([kind, capabilities]) => {
        if ((props.kind && props.kind !== kind) || (filter.value && filter.value !== kind)) { return [] }
        return capabilities.filter(capability => {
            if (kind === 'action' && capability.id === 'sequence') { return false }
            if (props.wrap && (kind !== 'condition' || !['all', 'any', 'not'].includes(capability.id))) { return false }
            if (kind === 'action' && capability.phases?.length && !capability.phases.includes(props.phase)) { return false }
            return !text || [capability.id, ruleLabel(kind, capability.id, 'zh-CN'), ruleLabel(kind, capability.id, 'en'), localHelp(capability.description, 'zh-CN'), localHelp(capability.description, 'en')].some(value => value.toLocaleLowerCase().includes(text))
        }).map(capability => ({kind, type: capability.id, label: ruleLabel(kind, capability.id, locale.value), help: localHelp(capability.description, locale.value)}))
    })
    const combinations = (() => {
    if (!props.wrap && props.kind !== 'condition' && filter.value !== 'condition') {
        return ([['tool', 'removeTool'], ['tools', 'removeAllTools'], ['field', 'removeField'], ['model', 'setModel']] as const).filter(([, label]) => !text || ['zh-CN', 'en'].some(lang => (lang === 'zh-CN' ? {removeTool:'按名称移除工具',removeAllTools:'移除全部工具',removeField:'移除字段',setModel:'设置模型'} : {removeTool:'Remove tool by name',removeAllTools:'Remove all tools',removeField:'Remove a field',setModel:'Set model'})[label].toLowerCase().includes(text))).map(([type, label]) => ({kind: 'action' as const, type, combination: type, label: t(`rules.canvas.${label}`), help: t('rules.canvas.combinationHint')}))
    }
    return []
})()
    return [...combinations, ...modules]
})
const positionStyle = computed(() => {
    const width = Math.min(360, viewport.value.width - 24)
    const height = Math.min(480, viewport.value.height - 24)
    return {left: `${Math.max(12, Math.min(props.position.x, viewport.value.width - width - 12))}px`, top: `${Math.max(12, Math.min(props.position.y, viewport.value.height - height - 12))}px`, width: `${width}px`}
})
function resize() { viewport.value = {width: window.innerWidth, height: window.innerHeight} }
function outside(event: PointerEvent) {
    if (props.open && event.target instanceof Node && !popup.value?.contains(event.target)) { emit('close') }
}
function choose(index: number) {
    const entry = entries.value[index]
    if (entry && 'combination' in entry) { emit('combination', entry.combination as 'tool' | 'tools' | 'field' | 'model') }
    else if (entry) { emit('select', {kind: entry.kind, type: entry.type}) }
}
function keydown(event: KeyboardEvent) {
    if (event.isComposing) { return }
    if (!['ArrowDown', 'ArrowUp', 'Enter', 'Escape', 'Home', 'End'].includes(event.key)) { return }
    event.preventDefault()
    event.stopPropagation()
    if (event.key === 'Escape') { emit('close') }
    else if (event.key === 'Enter') { choose(active.value) }
    else if (event.key === 'Home') { active.value = 0 }
    else if (event.key === 'End') { active.value = Math.max(0, entries.value.length - 1) }
    else if (entries.value.length) { active.value = (active.value + (event.key === 'ArrowDown' ? 1 : -1) + entries.value.length) % entries.value.length }
}
watch(entries, () => { active.value = 0 })
watch(active, async () => {
    await nextTick()
    popup.value?.querySelector(`[data-search-index="${active.value}"]`)?.scrollIntoView({block: 'nearest'})
})
watch(() => props.open, async open => {
    if (!open) { return }
    query.value = ''
    filter.value = ''
    active.value = 0
    resize()
    await nextTick()
    input.value?.focus()
})
onMounted(() => { resize(); window.addEventListener('resize', resize); document.addEventListener('pointerdown', outside, true) })
onBeforeUnmount(() => { window.removeEventListener('resize', resize); document.removeEventListener('pointerdown', outside, true) })
</script>

<template>
    <Teleport to="body">
        <dialog open v-if="open" ref="popup" class="rule-node-search nodrag nopan nowheel" :style="positionStyle" data-testid="node-search" :aria-label="t('rules.canvas.addNode')" @keydown="keydown" @pointerdown.stop @wheel.stop @contextmenu.prevent>
            <header><strong>{{ t('rules.canvas.addNode') }}</strong><button type="button" class="btn btn-ghost" :aria-label="t('common.close')" @click="emit('close')">×</button></header>
            <p class="search-context" data-testid="node-search-context">{{ context }}</p>
            <input ref="input" v-model="query" class="input" data-testid="node-search-input" :placeholder="t('rules.canvas.search')" :aria-label="t('rules.canvas.search')" role="combobox" :aria-controls="listID" aria-expanded="true" aria-autocomplete="list" :aria-activedescendant="entries.length ? `${listID}-${active}` : undefined" autocomplete="off" />
            <div v-if="!kind && !wrap" class="search-filters">
                <button v-for="item in (['', 'condition', 'action'] as const)" :key="item" type="button" :class="{active: filter === item}" :aria-pressed="filter === item" @click="filter = item; input?.focus()">{{ t((() => { if (item === '') { return 'rules.canvas.allModules'; } if (item === 'condition') { return 'rules.addCondition'; } return 'rules.addAction'; })()) }}</button>
            </div>
            <div :id="listID" class="search-results" role="listbox" :aria-label="t('rules.canvas.searchResults')">
                <button v-for="(entry, index) in entries" :id="`${listID}-${index}`" :key="`${entry.kind}:${entry.type}`" :data-testid="`node-search-${entry.kind}-${entry.type}`" :data-search-index="index" class="search-entry" :class="{active: index === active}" type="button" role="option" :aria-selected="index === active" @pointermove="active = index" @click="choose(index)">
                    <span class="entry-title"><span class="entry-dot" :class="entry.kind" />{{ entry.label }}</span>
                    <small>{{ entry.help }}</small>
                </button>
                <p v-if="!entries.length" class="search-empty">{{ t('rules.canvas.noSearchResults') }}</p>
            </div>
            <footer>{{ t('rules.canvas.searchKeys') }}</footer>
        </dialog>
    </Teleport>
</template>

<style scoped>
.rule-node-search { margin: 0; position: fixed; z-index: 100; isolation: isolate; display: flex; flex-direction: column; gap: 9px; padding: 12px; max-height: min(480px, calc(100dvh - 24px)); overflow: hidden; border: 1px solid var(--color-line); border-radius: 10px; color: var(--color-ink); background: var(--color-surface-elevated, var(--color-surface, var(--color-canvas))); box-shadow: 0 12px 40px rgb(0 0 0 / 24%); }
.rule-node-search header { display: flex; justify-content: space-between; align-items: center; font-size: 14px; }
.search-context, .search-empty, footer { margin: 0; font-size: 11px; color: var(--color-ink-muted); line-height: 1.5; overflow-wrap: anywhere; }
.search-filters { display: flex; gap: 5px; }
.search-filters button { padding: 4px 8px; font-size: 11px; border-radius: 5px; }
.search-filters button.active, .search-entry.active { background: var(--color-accent-soft); color: var(--color-accent); }
.search-results { overflow-y: auto; min-height: 0; flex: 1; }
.search-entry { display: grid; gap: 3px; width: 100%; padding: 9px; text-align: left; border-radius: 6px; }
.entry-title { display: flex; align-items: center; gap: 8px; font-size: 13px; font-weight: 600; }
.entry-dot { width: 8px; height: 8px; border-radius: 50%; background: var(--color-accent); }
.entry-dot.action { border-radius: 2px; background: #d97706; }
.search-entry small { color: var(--color-ink-muted); font-size: 11px; line-height: 1.4; display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; overflow: hidden; }
</style>
