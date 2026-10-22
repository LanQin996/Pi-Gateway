<script setup lang="ts">
import {computed, onBeforeUnmount, ref} from 'vue'
import {Handle, Position, type NodeProps} from '@vue-flow/core'
import type {Rule, RuleAction, RuleCondition, RuleFieldError, RulePhase, RuleSchema} from '../../api/rules'
import {actionFlowPorts, graphValueInputs, type RuleGraphValue, type RuleGraphNode} from '../../utils/ruleGraph'
import {useI18n} from '../../i18n'
import {ruleLabel, ruleDisplayName, ruleOptionLabel} from '../../utils/ruleLabels'
import {localHelp} from '../../utils/ruleSchema'
import RuleNodeFields from './RuleNodeFields.vue'
import RuleValueNodeFields from './RuleValueNodeFields.vue'

type Data = {
    kind: RuleGraphNode['kind']
    path: string
    data: RuleGraphNode['data']
    schema: RuleSchema
    diagnostics?: Record<string, string>
    errors?: RuleFieldError[]
    sample?: unknown
    phase?: RulePhase
    valueConnections?: Record<string,string>
    conditionConnections?: Record<string,string>
}
const props = defineProps<NodeProps<Data>>()
const emit = defineEmits<{
    'update:data': [value: RuleGraphNode['data']]
    'quick-add': [value: {kind: 'condition' | 'action'; mode?: 'auto' | 'wrap'; sourceHandle?: `flow:${string}`}]
    'focus-predicate': []
    'edit-start': []
    'edit-end': []
    'value-input': [path:string]
    'focus-value': [id:string]
    'move-argument': [path:string, direction:number]
}>()
const {locale, t} = useI18n()
const item = computed(() => props.data.data)
const valueInputs = computed(()=>graphValueInputs({kind:props.data.kind,data:item.value}))
const flowPorts = computed(() => props.data.kind === 'action' ? actionFlowPorts(item.value as RuleAction) : [])
function flowLabel(port: string) { return port.startsWith('functions/') ? port.slice(10).replaceAll('~1', '/').replaceAll('~0', '~') : t(`rules.canvas.flow.${port}`) }
const group = computed(() => props.data.kind === 'condition' && ['all', 'any', 'not'].includes((item.value as RuleCondition).op))
const childCount = computed(() => (item.value as RuleCondition).conditions?.length ?? 0)
const title = computed(() => (() => {
    if (props.data.kind === 'value') {
        if ((item.value as RuleGraphValue).mode === 'computed') {
            return ruleOptionLabel(((item.value as RuleGraphValue).value as {$expr:{op:string}}).$expr.op,locale.value)
        }
        return t(`rules.canvas.value.${(item.value as RuleGraphValue).mode}`)
    }
    if (props.data.kind === 'rule') {
        return ruleDisplayName(item.value as Rule, locale.value) || t('rules.add')
    }
    if (props.data.kind === 'condition') {
        return ruleLabel('condition', (item.value as RuleCondition).op, locale.value)
    }
    return ruleLabel('action', (item.value as RuleAction).type, locale.value)
})())
const summary = computed(() => {
    if (props.data.kind === 'value') { return title.value }
    if (props.data.kind === 'rule') { return ruleLabel('phase', (item.value as Rule).phase, locale.value) }
    if (props.data.kind === 'condition') { return (item.value as RuleCondition).path ? String((item.value as RuleCondition).path) : t('rules.when') }
    const action = item.value as RuleAction
    return String(action.params.path ?? action.params.model ?? localHelp(props.data.schema.actions.find(c => c.id === action.type)?.description, locale.value))
})
const status = computed(() => props.data.diagnostics?.[props.data.path])
const editing = ref(false)
function startEditing() {
    if (editing.value) { return }
    editing.value = true
    emit('edit-start')
}
function endEditing() {
    if (!editing.value) { return }
    editing.value = false
    emit('edit-end')
}
function focusOut(event: FocusEvent) {
    const area = event.currentTarget as HTMLElement
    if (event.relatedTarget instanceof Node && area.contains(event.relatedTarget)) { return }
    endEditing()
}
function keyboard(event: KeyboardEvent) {
    event.stopPropagation()
    // IME confirmation and textarea newlines must retain their native behavior.
    if (event.isComposing || event.keyCode === 229) { return }
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'd') { event.preventDefault() }
    const target = event.target as HTMLElement
    if (event.key === 'Enter' && !target.closest('textarea,button,summary,[contenteditable="true"]')) { event.preventDefault() }
}
onBeforeUnmount(endEditing)
</script>
<template>
  <div class="rule-graph-node" :class="`rule-graph-${data.kind}`" :data-node-id="id" :data-testid="`rule-node-${id}`">
    <Handle v-if="data.kind === 'rule' || group" id="boolean-in" type="target" :position="Position.Left" :title="t('rules.canvas.booleanPort')" />
    <Handle v-if="data.kind === 'action'" id="action-in" class="action-handle" type="target" :position="Position.Left" :title="t('rules.canvas.actionPort')" />
    <Handle v-if="data.kind === 'condition'" id="boolean-out" type="source" :position="Position.Right" />
    <Handle v-if="data.kind === 'value'" id="value-out" type="source" :position="Position.Right" :title="t('rules.canvas.valuePort')" />
    <Handle v-if="data.kind === 'rule' || data.kind === 'action'" id="action-out" type="source" :position="Position.Right" />
    <div class="rule-node-header rule-node-drag-handle" :title="summary">
      <span class="rule-node-kind">{{ (() => { if (data.kind === 'value') { return t('rules.canvas.valuePort'); } if (data.kind === 'rule') { return t('rules.canvas.rule'); } if (data.kind === 'condition') { return t('rules.when'); } return t('rules.canvas.action'); })() }}</span>
      <span class="rule-graph-node-title">{{ title }}</span>
    </div>
    <div class="rule-node-ports">
      <span>{{ (() => { if (data.kind === 'action') { return t('rules.canvas.actionPort'); } if (data.kind === 'rule' || group) { return t('rules.canvas.booleanPort'); } return ''; })() }}</span>
      <span>{{ (() => { if (data.kind === 'value') { return t('rules.canvas.valuePort'); } if (data.kind === 'condition') { return t('rules.canvas.booleanPort'); } return t('rules.canvas.actionPort'); })() }}</span>
    </div>

    <div v-if="status" class="rule-graph-node-status">{{ status }}</div>
    <div
      class="rule-node-controls nodrag nopan nowheel"
      data-testid="node-inline"
      @pointerdown.stop
      @pointerup.stop
      @mousedown.stop
      @touchstart.stop
      @click.stop
      @dblclick.stop
      @wheel.stop
      @keydown="keyboard"
      @keyup.stop
      @focusin="startEditing"
      @focusout="focusOut"
    >
      <RuleValueNodeFields v-if="data.kind==='value'" :node-id="id" :model-value="item as RuleGraphValue" :schema="data.schema" @update:model-value="emit('update:data',$event)" />
      <RuleNodeFields v-else flow-graph :node-id="id" :kind="data.kind" :path="data.path" :model-value="item" :schema="data.schema" :errors="data.errors" :sample="data.sample" :phase="data.phase" @update:model-value="emit('update:data', $event)" />
      <div v-if="valueInputs.length" class="rule-value-inputs">
       <div v-for="input in valueInputs" :key="input.path" class="rule-value-input" :data-value-input="input.path">
        <Handle :id="`value:${input.path}`" type="target" :position="Position.Left" class="value-input-handle" :title="input.index===undefined ? ruleLabel('field',input.label,locale) : `${t('rules.canvas.argument')} ${input.index+1}`" />
        <span>{{input.index===undefined ? ruleLabel('field',input.label,locale) : `${t('rules.canvas.argument')} ${input.index+1}`}}</span>
        <button v-if="data.valueConnections?.[input.path]" type="button" class="value-link" @click="emit('focus-value',data.valueConnections[input.path]!)">{{t('rules.canvas.openValue')}}</button>
        <button v-else type="button" class="value-link" @click="emit('value-input',input.path)">{{String(JSON.stringify(input.value)).slice(0,50)}} · +</button>
        <div v-if="input.index!==undefined" class="argument-buttons">
         <button type="button" :disabled="input.index===0" :title="t('rules.up')" @click="emit('move-argument',input.path,-1)">↑</button>
         <button type="button" :disabled="input.index===valueInputs.length-1" :title="t('rules.down')" @click="emit('move-argument',input.path,1)">↓</button>
         <button type="button" :title="t('rules.remove')" @click="emit('move-argument',input.path,0)">×</button>
        </div>
       </div>
      </div>
      <div v-for="(source,field) in data.conditionConnections" :key="field" class="rule-value-input">
       <Handle :id="`predicate:${field}`" type="target" :position="Position.Left" class="value-input-handle" />
       <span>{{ruleLabel('field',field,locale)}}</span><button type="button" class="value-link" @click="emit('focus-value',source)">{{t('rules.canvas.openValue')}}</button>
      </div>
      <div v-if="flowPorts.length" class="rule-node-flow-ports">
        <div v-for="port in flowPorts" :key="port" class="rule-node-flow-port">
          <span>{{ flowLabel(port) }}</span><button type="button" class="btn btn-ghost" :data-testid="`flow-add-${port}`" @click="emit('quick-add', {kind:'action', sourceHandle:`flow:${port}`})">+ {{ t('rules.canvas.addNode') }}</button>
          <Handle :id="`flow:${port}`" class="action-handle flow-handle" type="source" :position="Position.Right" :title="flowLabel(port)" />
        </div>
      </div>
      <button v-if="data.kind === 'action' && (item as RuleAction).type === 'array_filter'" type="button" class="rule-node-predicate-link" @click="emit('focus-predicate')">{{ t('rules.canvas.editItemCondition') }}</button>
      <div class="rule-node-actions">
        <button v-if="data.kind === 'rule' || group && ((item as RuleCondition).op !== 'not' || !childCount)" type="button" class="btn btn-ghost" data-testid="node-add-condition" @click="emit('quick-add', {kind: 'condition', mode: 'auto'})">{{ data.kind === 'rule' ? t('rules.addCondition') : t('rules.canvas.addChild') }}</button>
        <button v-if="data.kind === 'condition'" type="button" class="btn btn-ghost" data-testid="node-wrap-condition" @click="emit('quick-add', {kind: 'condition', mode: 'wrap'})">{{ t('rules.canvas.wrapCondition') }}</button>
        <button v-if="data.kind === 'rule' || data.kind === 'action'" type="button" class="btn btn-ghost" data-testid="node-add-action" @click="emit('quick-add', {kind: 'action', mode: 'auto'})">{{ t('rules.canvas.addAfter') }}</button>
      </div>
    </div>
    <Handle v-if="data.kind === 'action' && (item as RuleAction).type === 'array_filter'" id="predicate:predicate" type="target" :position="Position.Bottom" :title="t('rules.canvas.predicatePort')" />
  </div>
</template>
<style scoped>
.rule-graph-node { --node-tint: var(--color-accent); position: relative; box-sizing: border-box; width: 400px; border: 1px solid var(--color-line-strong); border-radius: 10px; background: var(--color-surface-elevated); color: var(--color-ink); box-shadow: 0 4px 18px rgb(0 0 0 / 12%); }
.rule-graph-condition { --node-tint: #818cf8; }
.rule-graph-action { --node-tint: #d29958; }
.rule-graph-value { --node-tint: #22a68c; }
.rule-value-inputs {display:grid;gap:6px;margin-top:8px}
.rule-value-input {position:relative;display:grid;grid-template-columns:70px minmax(0,1fr) auto;align-items:center;gap:6px;min-height:32px;padding:4px 6px;border:1px solid var(--color-line);border-radius:5px;font-size:11px}
.value-link {min-width:0;text-align:left;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;color:var(--color-accent)}
.argument-buttons {display:flex;gap:2px}
.argument-buttons button {width:24px;height:24px;border:1px solid var(--color-line);border-radius:4px;flex:none}
.argument-buttons button:disabled {opacity:.35}
.rule-value-input :deep(.value-input-handle) {top:50%;left:-20px;background:#22a68c}
.rule-node-header { display: flex; align-items: center; gap: 10px; height: 42px; padding: 0 16px; border-radius: 9px 9px 0 0; border-bottom: 1px solid var(--color-line); background: color-mix(in srgb, var(--node-tint) 14%, var(--color-surface-elevated)); }
.rule-node-kind { flex-shrink: 0; font-size: 10px; color: var(--color-ink-muted); }
.rule-graph-node-title { flex: 1; min-width: 0; font-size: 13px; font-weight: 650; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.rule-node-drag-handle { cursor: grab; }
.rule-node-drag-handle:active { cursor: grabbing; }
.rule-node-ports { display: flex; justify-content: space-between; gap: 16px; padding: 9px 16px; color: var(--color-ink-muted); font-size: 10px; border-bottom: 1px solid var(--color-line); }
.rule-graph-node-status { margin: 8px 12px 0; border-radius: 4px; padding: 4px 8px; background: var(--color-accent-soft); color: var(--color-accent); font-size: 11px; }
.rule-node-controls { container-type: inline-size; min-width: 0; padding: 10px 12px 12px; cursor: default; }
.rule-node-controls > :deep(.rule-node-fields > .rule-node-parameter-body) { max-height: 400px; overflow-y: auto; overflow-x: hidden; overscroll-behavior: contain; scrollbar-width: thin; padding-right: 4px; }
.rule-node-predicate-link { width: 100%; min-height: 28px; padding: 4px 8px; border: 1px solid var(--color-line); border-radius: 5px; background: var(--color-surface-2); color: var(--color-accent); text-align: left; font-size: 11px; cursor: pointer; }
.rule-node-flow-ports { display:grid; gap:6px; margin-top:8px; }
.rule-node-flow-port { position:relative; display:flex; align-items:center; justify-content:space-between; gap:8px; padding:6px 8px; border:1px solid var(--color-line); border-radius:6px; font-size:11px; background:var(--color-surface-2); }
.rule-node-flow-port :deep(.flow-handle) { top:50%; right:-20px; }
.rule-node-actions { display: flex; flex-wrap: wrap; gap: 6px; padding-top: 10px; margin-top: 10px; border-top: 1px solid var(--color-line); }
.rule-node-actions .btn { flex: 1; white-space: nowrap; justify-content: center; padding: 5px 8px; font-size: 11px; min-height: 28px; background: var(--color-surface-2); }
:deep(.vue-flow__handle) { top: 60px; }
:deep(.vue-flow__handle-bottom) { top: auto; }
</style>
