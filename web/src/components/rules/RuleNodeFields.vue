<script setup lang="ts">
import {computed, ref} from 'vue'
import type {RuleAction, RuleCondition, RuleField, RuleFieldError, RuleSchema, RulePhase} from '../../api/rules'
import {complexGraphCondition, graphValueMode, type RuleGraphNode} from '../../utils/ruleGraph'
import {pointerPart} from '../../utils/ruleEditor'
import {currentSamplePathField} from '../../utils/samplePaths'
import {ruleLabel} from '../../utils/ruleLabels'
import {defaultForField} from '../../utils/ruleSchema'
import {readonlyRuleFields} from '../../utils/ruleSchemaAdapter'
import {useI18n} from '../../i18n'
import RuleFieldEditor from './RuleField.vue'

const props = defineProps<{
  nodeId: string
  kind: RuleGraphNode['kind']
  path: string
  modelValue: RuleGraphNode['data']
  schema: RuleSchema
  errors?: RuleFieldError[]
  sample?: unknown
  phase?: RulePhase
  flowGraph?: boolean
}>()
const emit = defineEmits<{'update:modelValue': [value: RuleGraphNode['data']]}>()
const {t, locale} = useI18n()
const group = computed(() => props.kind === 'condition' && ['all', 'any', 'not'].includes((props.modelValue as RuleCondition).op))
const values = computed<Record<string, unknown>>(() => props.kind === 'action' ? (props.modelValue as RuleAction).params : props.modelValue as unknown as Record<string, unknown>)
const fields = computed(() => {
  if (props.kind === 'rule') {
    return props.schema.rule_fields.filter(field => !readonlyRuleFields.includes(field.name) && !['when', 'actions', 'schema_version'].includes(field.name))
  }
  const registered = props.kind === 'condition'
    ? props.schema.conditions.find(cap => cap.id === (props.modelValue as RuleCondition).op)?.fields ?? []
    : props.schema.actions.find(cap => cap.id === (props.modelValue as RuleAction).type)?.fields ?? []
  return registered.filter(field => (!group.value || field.name !== 'conditions') && (!props.flowGraph || field.type !== 'action_array' && !((props.modelValue as RuleAction).type === 'scope' && field.name === 'functions') && !(field.type==='condition' && complexGraphCondition(values.value[field.name])) && !(props.kind === 'action' && ['value','values'].includes(field.name)) && !(props.kind === 'condition' && field.name === 'value' && graphValueMode(values.value.value))))
})
const collapsed = ref(false)
const showAll = ref(false)
const primaryNames = computed(() => (() => {
    if (props.kind === 'rule') {
        return ['name', 'enabled', 'phase', 'priority']
    }
    if (props.kind === 'condition') {
        return ['path', 'source']
    }
    if (props.kind === 'action' && (props.modelValue as RuleAction).type === 'text_replace') {
        return ['path', 'match', 'pattern', 'replacement']
    }
    return []
})())
function isPrimary(field: RuleField) {
 if (field.required || primaryNames.value.includes(field.name) || (props.errors ?? []).some(error => error.path === fieldPath(field) || error.path.startsWith(`${fieldPath(field)}/`))) { return true }
 const actual = values.value[field.name]
 return actual !== undefined && JSON.stringify(actual) !== JSON.stringify(defaultForField(field))
}
const hiddenFields = computed(() => fields.value.filter(field => !isPrimary(field)))
const visibleFields = computed(() => showAll.value ? fields.value : fields.value.filter(isPrimary))
const childCount = computed(() => (props.modelValue as RuleCondition).conditions?.length ?? 0)
const predicate = computed(() => props.kind === 'action' && (props.modelValue as RuleAction).type === 'array_filter' ? values.value.predicate as RuleCondition | undefined : undefined)
const fieldPath = (field: RuleField) => `${props.path}${props.kind === 'action' ? '/params' : ''}/${pointerPart(field.name)}`
const idPrefix = computed(() => `node-${encodeURIComponent(props.nodeId)}`)
function fieldSample(field: RuleField) {
  const source = props.kind === 'action' && field.name === 'target_path' ? 'current' : values.value.source
  return field.type === 'condition' || field.type === 'action_array' || props.kind !== 'rule' && currentSamplePathField(field, source) ? props.sample : undefined
}
function updateField(field: RuleField, value: unknown) {
  // modelValue is the full graphNodeValue, so predicate/group subtrees survive.
  const next = {...values.value}
  if (value === undefined) { delete next[field.name] }
  else { next[field.name] = value }
  emit('update:modelValue', props.kind === 'action'
    ? {...props.modelValue as RuleAction, params: next}
    : next as unknown as RuleGraphNode['data'])
}
const nodeErrors = computed(() => (props.errors ?? []).filter(error => {
  if (fields.value.some(field => error.path === fieldPath(field) || error.path.startsWith(`${fieldPath(field)}/`))) { return false }
  if (error.path === props.path) { return true }
  if (props.kind === 'rule') { return error.path.startsWith(`${props.path}/`) && !error.path.slice(props.path.length + 1).includes('/') && !['/when', '/actions'].includes(error.path) }
  if (props.kind === 'condition') { return error.path === `${props.path}/op` || error.path === `${props.path}/conditions` }
  return error.path.startsWith(`${props.path}/`) && !error.path.startsWith(`${props.path}/params/predicate/`)
}))
</script>
<template>
  <div class="rule-node-fields" :data-testid="`rule-node-fields-${nodeId}`">
    <button v-if="fields.length" type="button" class="rule-node-parameter-toggle" data-testid="node-parameter-toggle" :aria-expanded="!collapsed" :aria-label="t(collapsed ? 'rules.canvas.expandParameters' : 'rules.canvas.collapseParameters')" @click="collapsed = !collapsed">
      <span>{{ t('rules.canvas.parameterCount', {count: fields.length}) }}</span><span aria-hidden="true">{{ collapsed ? '▸' : '▾' }}</span>
    </button>
    <div :style="collapsed ? {display: 'none'} : undefined" class="rule-node-parameter-body">
    <p v-if="group" class="rule-node-field-summary" data-testid="rule-node-condition-count">{{ ruleLabel('field', 'conditions', locale) }}: {{ childCount }}</p>
      <RuleFieldEditor
        v-for="field in visibleFields"
        :key="field.name"
        :field="field"
        :model-value="values[field.name]"
        :schema="schema"
        :path="fieldPath(field)"
        :errors="errors"
        :phase="phase"
        :sample="fieldSample(field)"
        :item-context="kind === 'action' && (modelValue as RuleAction).type === 'array_filter' && field.name === 'predicate'"
        compact
        :id-prefix="idPrefix"
        @update:model-value="updateField(field, $event)"
      />
    <button v-if="hiddenFields.length" type="button" class="rule-node-show-parameters" :aria-expanded="showAll" @click="showAll = !showAll">{{ t(showAll ? 'rules.canvas.lessParameters' : 'rules.canvas.showParameters', {count: hiddenFields.length}) }}</button>
    </div>
    <p v-if="kind === 'action' && (modelValue as RuleAction).type === 'array_filter'" class="rule-node-field-summary" data-testid="rule-node-predicate-summary">
      {{ ruleLabel('field', 'predicate', locale) }}: {{ predicate ? ruleLabel('condition', predicate.op, locale) : t('rules.none') }}<template v-if="predicate?.conditions"> · {{ ruleLabel('field', 'conditions', locale) }}: {{ predicate.conditions.length }}</template>
    </p>
    <p v-for="error in nodeErrors" :key="error.path + error.message" class="rule-node-field-error" role="alert">{{ error.path }}: {{ error.message }}</p>
  </div>
</template>
<style scoped>
.rule-node-fields { display: grid; gap: .4rem; min-width: 0; }
.rule-node-parameter-body { min-width: 0; display: grid; gap: 4px; }
.rule-node-parameter-toggle { display: flex; align-items: center; justify-content: space-between; gap: 10px; min-height: 28px; padding: 3px 0; border: 0; background: transparent; color: var(--color-ink-muted); font-size: 11px; font-weight: 650; cursor: pointer; white-space: nowrap; }
.rule-node-parameter-toggle:hover, .rule-node-show-parameters:hover { color: var(--color-accent); }
.rule-node-show-parameters { min-height: 28px; padding: 4px 8px; border: 1px solid var(--color-line); border-radius: 5px; background: var(--color-surface-2); color: var(--color-ink-muted); font-size: 11px; cursor: pointer; text-align: center; }
.rule-node-field-summary { color: var(--color-ink-muted); font-size: .68rem; overflow-wrap: anywhere; }
.rule-node-field-error { color: #ef4444; font-size: .65rem; overflow-wrap: anywhere; }
</style>
