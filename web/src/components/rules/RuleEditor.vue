<script setup lang="ts">
import { copyText } from "../../utils/clipboard"
import {computed, ref, shallowRef, toRaw, watch, onBeforeUnmount} from 'vue'
import {api, ApiError} from '../../api/client'
import type {Rule, RuleAction, RuleFieldError, RuleSchema} from '../../api/rules'
import type {RuleCanvasDiagnostics, RuleDebugFocus} from '../../api/ruleSimulation'
import {useI18n} from '../../i18n'
import {useToastStore} from '../../stores/ui'
import {clone, parseRule, RuleInputError, stringifyRule, validateRule, type RuleDraft} from '../../utils/ruleEditor'
import {graphErrors, graphPathMap, graphSignature, graphToRule, persistGraphLayout, replaceGraphNode, ruleToGraph, type RuleGraph} from '../../utils/ruleGraph'
import {readonlyRuleFields} from '../../utils/ruleSchemaAdapter'
import {localHelp} from '../../utils/ruleSchema'
import {ruleLabel, ruleDisplayName} from '../../utils/ruleLabels'
import RuleField from './RuleField.vue'
import ConditionEditor from './ConditionEditor.vue'
import ActionEditor from './ActionEditor.vue'
import RuleHelp from './RuleHelp.vue'
import RuleCanvas from './RuleCanvas.vue'
import CaptureRuleDebugger from './CaptureRuleDebugger.vue'
const props = defineProps<{modelValue: RuleDraft; schema: RuleSchema; busy?: boolean; conflict?: Rule | null}>()
const emit = defineEmits<{'update:modelValue': [value: RuleDraft]; save: [rule: Rule]; close: []; reload: []; rebase: []}>()
const {t, locale} = useI18n()
const toast = useToastStore()
const form = ref<HTMLFormElement>()
const canvas = ref<InstanceType<typeof RuleCanvas>>()
const rule = computed(() => props.modelValue.rule)
const graph = computed(() => toRaw(props.modelValue.graph) ?? ruleToGraph(rule.value))
const paths = computed(() => graphPathMap(graph.value))
const graphProblems = computed(() => graphErrors(graph.value, props.schema))
const debuggerDisabled = computed(() => !!props.busy || graphProblems.value.length > 0 || props.modelValue.mode === 'code')
const sample = shallowRef<unknown>()
let editingNode: string | undefined
let inlineHistoryRecorded = false
watch(debuggerDisabled, disabled=>{if(disabled) { sample.value=undefined }}, {flush:'sync'})
onBeforeUnmount(()=>persistGraphLayout(graph.value))
function receiveSample(value:unknown) { sample.value=debuggerDisabled.value ? undefined : value }
function receiveDiagnostics(value:RuleCanvasDiagnostics|null) { diagnostics.value=value; if(!value){ sample.value=undefined } }
const validating = ref(false)
const diagnostics = shallowRef<RuleCanvasDiagnostics|null>(null)
const diagnosticLabels = computed(() => {
    const result: Record<string,string> = {}
    if (!diagnostics.value) { return result }
    const current = diagnostics.value
    for (const condition of current.conditions ?? []) if (condition.rule_id === current.ruleId) { result[condition.path] = ruleLabel('status',condition.status,locale.value) }
    for (const trace of current.traces ?? []) {
        if (trace.rule_id !== current.ruleId) { continue }
        const action = graph.value.nodes.find(n => n.kind === 'action' && (n.data as RuleAction).id === trace.action_id)
        const path = action ? paths.value[action.id] : undefined
        if (path) { result[path] = ruleLabel('status',(() => {
    if (trace.rolled_back) {
        return 'rolled_back'
    }
    if (trace.error) {
        return 'failed'
    }
    return trace.status ?? (trace.matched === false ? 'not_matched' : 'success')
})(),locale.value) }
    }
    return result
})
const phaseHelp = computed(() => props.schema.rule_fields.find(f => f.name === 'phase')?.enum_help ?? {})
function patch(fields:Partial<RuleDraft>) {emit('update:modelValue',{...props.modelValue,...fields})}
function updateGraph(next:RuleGraph, history=true) {
    const changed = graphSignature(next) !== graphSignature(graph.value)
    persistGraphLayout(next)
    if (!changed) {patch({graph:next}); return}
    const fields:Partial<RuleDraft>={graph:next,errors:[]}
    if (history) {fields.graphUndo=[...(props.modelValue.graphUndo ?? []),clone(graph.value)].slice(-50); fields.graphRedo=[]}
    try {fields.rule=graphToRule(next,props.schema,false); fields.code=stringifyRule(fields.rule)} catch { /* retain the last representable rule alongside this invalid graph */ }
    diagnostics.value=null; sample.value=undefined; patch(fields)
}
function updateRule(next:Rule) {
 if(props.modelValue.mode==='steps'){
 diagnostics.value=null; sample.value=undefined
 patch({rule:next,code:stringifyRule(next),graph:ruleToGraph(next,graph.value),errors:[]})
 } else { updateGraph(ruleToGraph(next,graph.value)) }
}
function updateField(name:string,value:unknown) {
 if(props.modelValue.mode==='steps'){const next={...rule.value} as unknown as Record<string,unknown>;if(value===undefined){ delete next[name]; }else { next[name]=value; }updateRule(next as unknown as Rule);return}
    const root = graph.value.nodes.find(n=>n.kind==='rule')!
    const data = {...root.data} as unknown as Record<string,unknown>
    if(value===undefined) { delete data[name]; } else { data[name]=value }
    updateGraph(replaceGraphNode(graph.value,root.id,data as unknown as Rule))
}
function beginNodeEdit(id:string) {
    if(editingNode===id){ return }
    editingNode=id;inlineHistoryRecorded=false
    if(!graph.value.selected.includes(id)){ updateGraph({...graph.value,selected:[id]},false) }
}
function endNodeEdit(id?:string) {
    if(id!==undefined&&editingNode!==id){ return }
    editingNode=undefined;inlineHistoryRecorded=false
}
function updateNodeById(id:string,data:import('../../utils/ruleGraph').RuleGraphNode['data']) {
    if(!graph.value.nodes.some(node=>node.id===id)){ return }
    const next=replaceGraphNode(graph.value,id,data)
    const changed=graphSignature(next)!==graphSignature(graph.value)
    updateGraph(next,editingNode!==id||!inlineHistoryRecorded)
    if(changed&&editingNode===id){ inlineHistoryRecorded=true }
}
function undo(redo=false) {
    endNodeEdit()
    const stack=redo?props.modelValue.graphRedo:props.modelValue.graphUndo
    const next=stack?.at(-1); if(!next) { return }
    const other=redo?props.modelValue.graphUndo:props.modelValue.graphRedo
    const fields:Partial<RuleDraft>={graph:clone(next),errors:[],graphUndo:redo?[...(other??[]),clone(graph.value)].slice(-50):stack!.slice(0,-1),graphRedo:redo?stack!.slice(0,-1):[...(other??[]),clone(graph.value)].slice(-50)}
    try {fields.rule=graphToRule(next,props.schema,false);fields.code=stringifyRule(fields.rule)} catch {}
    diagnostics.value=null; sample.value=undefined;patch(fields)
}
function reportErrors(error:unknown) {
    let errors:RuleFieldError[]=[]
    if(error instanceof RuleInputError) { errors=error.errors }
    else if(error instanceof ApiError && error.payload && typeof error.payload==='object') {
        const payload=error.payload as {errors?:RuleFieldError[]}
        if(Array.isArray(payload.errors)) { errors=payload.errors }
    }
    if(!errors.length) { errors=[{path:'',message:error instanceof Error?error.message:String(error)}] }
    patch({errors})
}
function synchronize():Rule|undefined {
    try {
        if(form.value && !form.value.reportValidity()) { return }
        const next=(() => {
    if (props.modelValue.mode==='code') {
        return parseRule(props.modelValue.code,props.schema)
    }
    if (props.modelValue.mode==='steps') {
        return rule.value
    }
    return graphToRule(graph.value,props.schema)
})()
        const errors=validateRule(next,props.schema)
        if(errors.length) {patch({errors});return}
        patch({rule:next,errors:[]});return next
    } catch(error) {reportErrors(error)}
}
function switchMode(mode:'steps'|'visual'|'code') {
    if(mode===props.modelValue.mode) { return }
    const next=synchronize();if(!next) { return }
    patch({rule:next,code:stringifyRule(next),graph:mode==='visual'?ruleToGraph(next,graph.value):graph.value,mode,errors:[]})
}
function formatCode() {
    try {const next=parseRule(props.modelValue.code,props.schema);patch({rule:next,code:stringifyRule(next),graph:ruleToGraph(next,graph.value),errors:[]})}catch(error){reportErrors(error)}
}
async function validate() {
    const next=synchronize();if(!next){ return }
    validating.value=true
    try{const result=await api.validateRule(next);patch({errors:result.errors??[]});if(result.valid){ toast.success(t('rules.validate')) }}catch(error){reportErrors(error)}finally{validating.value=false}
}
function save(){const next=synchronize();if(next){ emit('save',next) }}
function loadExample(example:Rule) {
    if(!confirm(t('rules.exampleConfirm'))){ return }
    const next=clone(example)
    for(const key of readonlyRuleFields){const current=(rule.value as unknown as Record<string,unknown>)[key];if(current===undefined){ delete(next as unknown as Record<string,unknown>)[key]; }else{ (next as unknown as Record<string,unknown>)[key]=current }}
    updateRule(next)
}
async function copyCode(){try{const next=synchronize();if(next){ await copyText(props.modelValue.mode==='code'?props.modelValue.code:stringifyRule(next)) }}catch{toast.error(t('common.clipboardBlocked'))}}
function focusPath(path:string){const entry=Object.entries(paths.value).filter(([,value])=>path===value||path.startsWith(`${value}/`)).sort((a,b)=>b[1].length-a[1].length)[0];if(entry){canvas.value?.focus(entry[0])}}
function debugFocus(focus:RuleDebugFocus){const action=graph.value.nodes.find(n=>n.kind==='action'&&(n.data as RuleAction).id===focus.actionId);if(action){canvas.value?.focus(action.id)}else if(focus.path){ focusPath(focus.path) }}
const shownErrors=computed(()=>[...props.modelValue.errors,...(props.modelValue.mode==='visual'?graphProblems.value.filter(error=>error.code?.startsWith('graph.')):[])].filter((error,index,all)=>all.findIndex(e=>e.path===error.path&&e.message===error.message)===index))
</script>
<template>
  <form ref="form" class="space-y-4 rule-editor" @submit.prevent="save">
    <details class="rounded-lg border border-[color:var(--color-line)] p-3 text-xs" open data-testid="rule-pipeline">
      <summary class="cursor-pointer font-medium">{{ locale === 'zh-CN' ? '规则执行流水线' : 'Rule execution pipeline' }}</summary>
      <ol class="mt-2 flex flex-wrap gap-2">
        <li v-for="phase in schema.phases" :key="phase" class="rounded border border-[color:var(--color-line)] px-2 py-1" :class="phase === rule.phase ? 'font-semibold ring-1 ring-[color:var(--color-accent)]' : ''" :title="phaseHelp[phase]">
          <span>{{ ruleLabel('phase', phase, locale) }}</span><span class="mt-1 block max-w-64 text-[color:var(--color-ink-muted)]">{{ localHelp(phaseHelp[phase], locale) }}</span>
        </li>
      </ol>
      <p class="mt-2 text-[color:var(--color-ink-muted)]">{{ locale === 'zh-CN' ? '请求按上述阶段执行；响应事件逐项处理，响应正文用于非流式聚合结果。同阶段优先级越小越先执行，步骤从上到下读取前一步结果。协议默认规则优先级为 -1000；在它之后的规则可以覆盖结果。' : 'Requests follow these stages. Response events run individually; response body rules process non-streaming aggregates. Lower priority runs first within a stage; steps read the preceding result. Protocol defaults use priority -1000; later rules can override their result.' }}</p>
    </details>
    <div class="flex flex-wrap items-center justify-between gap-2">
      <h2 class="text-base font-semibold">{{ ruleDisplayName(rule,locale) || t('rules.add') }}</h2>
      <div class="flex flex-wrap gap-2"><button type="button" class="btn" @click="emit('close')">{{ t('rules.cancel') }}</button><button class="btn btn-primary" :disabled="busy" type="submit">{{ t('rules.save') }}</button></div>
    </div>
    <div v-if="conflict" class="notice space-y-2" role="alert"><p>{{ t('rules.conflict') }}</p><details><summary>{{ t('rules.serverVersion') }}</summary><pre class="max-h-72 overflow-auto whitespace-pre-wrap text-xs">{{ JSON.stringify(conflict,null,2) }}</pre></details><div class="flex flex-wrap gap-2"><button type="button" class="btn" @click="emit('reload')">{{ t('rules.useServer') }}</button><button type="button" class="btn" @click="emit('rebase')">{{ t('rules.rebase') }}</button></div></div>
    <div class="flex flex-wrap gap-2"><button type="button" class="btn" :aria-pressed="modelValue.mode==='steps'" @click="switchMode('steps')">{{ locale==='zh-CN'?'步骤':'Steps' }}</button><button type="button" class="btn" :aria-pressed="modelValue.mode==='visual'" @click="switchMode('visual')">{{ t('rules.graphical') }}</button><button type="button" class="btn" :aria-pressed="modelValue.mode==='code'" @click="switchMode('code')">{{ t('rules.code') }}</button><button type="button" class="btn" :disabled="validating||busy" @click="validate">{{ t('rules.validate') }}</button></div>
    <div v-if="shownErrors.length" class="rounded border border-red-500 p-3 text-xs text-red-500" role="alert"><h3 class="font-medium">{{ t('rules.errors') }}</h3><div v-for="(error,index) in shownErrors" :key="index" class="mt-1 break-words"><button type="button" class="text-left underline" @click="focusPath(error.path)">{{ error.code?.startsWith('graph.') ? t(`rules.${error.code}`) : t('rules.canvas.error') }}</button><details><summary>{{ t('rules.canvas.details') }}</summary><code>{{ error.path||'/' }}</code>: {{ error.message }}</details></div></div>
    <template v-if="modelValue.mode==='steps'">
      <p class="notice text-xs">{{ locale==='zh-CN'?'客户端输入 → 请求构建 → 路由前规则 → 协议字段 → 账号选择 → 请求头 → 发送；响应事件 → 流式输出或正文聚合。':'Client input → request construction → request rules → protocol fields → account selection → headers → send; response events → stream or aggregate.' }}</p>
      <div class="grid gap-3 sm:grid-cols-2"><RuleField v-for="field in schema.rule_fields.filter(f=>!f.readonly && !['when','actions','schema_version'].includes(f.name))" :key="field.name" :field="field" :model-value="(rule as unknown as Record<string,unknown>)[field.name]" :schema="schema" :path="`/${field.name}`" :errors="modelValue.errors" @update:model-value="updateField(field.name,$event)" /></div>
      <ConditionEditor :model-value="rule.when" :schema="schema" path="/when" :errors="modelValue.errors" :sample="sample" @update:model-value="updateField('when',$event)" />
      <ActionEditor :model-value="rule.actions" :schema="schema" :phase="rule.phase" :errors="modelValue.errors" :sample="sample" @update:model-value="updateField('actions',$event)" />
    </template>
    <template v-else-if="modelValue.mode==='visual'">
      <RuleCanvas ref="canvas" :model-value="graph" :schema="schema" :diagnostics="diagnosticLabels" :errors="[...modelValue.errors,...graphProblems]" :sample="sample" :can-undo="!!modelValue.graphUndo?.length" :can-redo="!!modelValue.graphRedo?.length" @update:model-value="updateGraph" @edit="updateNodeById" @edit-start="beginNodeEdit" @edit-end="endNodeEdit" @undo="undo()" @redo="undo(true)" />
    </template>
    <div v-else class="space-y-2"><div class="flex gap-2"><button type="button" class="btn" @click="formatCode">{{ t('rules.format') }}</button><button type="button" class="btn" @click="copyCode">{{ t('rules.copyCode') }}</button></div><textarea data-testid="rule-code" class="input min-h-[28rem] font-mono text-xs" spellcheck="false" :value="modelValue.code" :aria-label="t('rules.code')" @input="patch({code:($event.target as HTMLTextAreaElement).value})" /></div>
    <CaptureRuleDebugger :rule="rule" :schema="schema" :disabled="debuggerDisabled" @diagnostics="receiveDiagnostics" @focus="debugFocus" @errors="patch({errors:$event})" @sample="receiveSample" />
    <RuleHelp :schema="schema" @example="loadExample" />
  </form>
</template>
<style scoped>
.rule-editor { min-width: 0; }
.rule-editor > * { min-width: 0; }
</style>
