<script setup lang="ts">
import { computed, ref, type PropType } from 'vue'
import type { JsonValue, RuleSchema } from '../../api/rules'
import { useI18n } from '../../i18n'
import RuleField from './RuleField.vue'
import JsonValueEditor from './JsonValueEditor.vue'
import {ruleLabel, ruleOptionLabel} from '../../utils/ruleLabels'

// JSON empty strings must not undergo Vue's Boolean-prop coercion.
const props = defineProps({modelValue: {type: null as unknown as PropType<JsonValue>, required: true}, schema: Object as PropType<RuleSchema>, allowReference: {type:Boolean, default:true}, compact:Boolean, idPrefix:String})
const emit = defineEmits<{ 'update:modelValue': [value: JsonValue] }>()
const { t, locale } = useI18n()
const schema = computed(() => props.schema)
const reference = computed(() => {
  const value = props.modelValue
  return value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === 1 && '$ref' in value ? (value as { $ref: Record<string, JsonValue> }).$ref : null
})
const literalValue = computed<JsonValue>(() => {
  const value = props.modelValue
  if (value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === 1 && '$literal' in value) { return (value as { $literal: JsonValue }).$literal }
  return value as JsonValue
})
function setLiteral(value: JsonValue) {
  if (value && typeof value==='object' && !Array.isArray(value) && ('$expr' in value || '$ref' in value || '$literal' in value)) { emit('update:modelValue',{$literal:value}) }
  else { emit('update:modelValue', value) }
}
function setReference() { emit('update:modelValue', { $ref: { source: 'current', path: '', encoding: 'value' } }) }
function setLiteralEscape(value: JsonValue) { emit('update:modelValue', { $literal: value }) }
function updateReference(key: string, value: JsonValue) {
  const current = reference.value ?? { source: 'current', path: '', encoding: 'value' }
  const next = {...current}
  if (value === undefined) { delete next[key] }
  else { next[key] = value }
  emit('update:modelValue', { $ref: next })
}
function modeChange(mode: string) {
  if(mode==='computed'){ emit('update:modelValue',{$expr:{op:'concat',args:['']}}) }
  else if (mode === 'reference') { setReference() }
  else if (mode === 'escape') { setLiteralEscape(literalValue.value) }
  else { setLiteral(literalValue.value) }
}
const computedValue=computed(()=>{const v=props.modelValue;return v&&typeof v==='object'&&!Array.isArray(v)&&'$expr' in v?v.$expr as Record<string,JsonValue>:null})
function updateComputed(key:string,value:unknown){const next={...(computedValue.value??{op:'concat',args:[]})};if(value===undefined){ delete next[key]; }else { next[key]=value as JsonValue; }emit('update:modelValue',{$expr:next})}
const literalOptions = ref(false)
const showValueOptions = ref(false)
const mode = computed(() => (() => {
    if (computedValue.value) {
        return 'computed'
    }
    if (reference.value) {
        return 'reference'
    }
    if (props.modelValue && typeof props.modelValue === 'object' && !Array.isArray(props.modelValue) && Object.keys(props.modelValue).length === 1 && '$literal' in props.modelValue) {
        return 'escape'
    }
    return 'literal'
})())
const sources = computed(() => schema.value?.sources ?? ['current', 'client', 'context', 'item'])
</script>
<template>
  <div class="space-y-2" :class="{'value-editor-compact': compact}">
    <button v-if="compact" class="literal-type-toggle" type="button" :aria-expanded="showValueOptions" :title="t('rules.valueKind')" @click="showValueOptions = !showValueOptions">{{ t('rules.valueKind') }}: {{ t((() => { if (mode === 'literal') { return 'rules.literal'; } if (mode === 'reference') { return 'rules.reference'; } return 'rules.valueKind'; })()) }} {{ showValueOptions ? '▾' : '▸' }}</button>
    <div v-if="!compact || showValueOptions" class="value-mode-bar flex flex-wrap items-center gap-2">
      <label :for="idPrefix" class="text-[11px] text-[color:var(--color-ink-muted)]">{{ t('rules.valueKind') }}</label>
      <select :id="idPrefix" class="input !w-auto !py-1 text-[12px]" :aria-label="t('rules.valueKind')" :value="mode" @change="modeChange(($event.target as HTMLSelectElement).value)">
        <option value="literal">{{ t('rules.literal') }}</option>
        <option v-if="allowReference" value="reference">{{ t('rules.reference') }}</option>
        <option value="computed">{{ locale==='zh-CN'?'计算表达式':'Computed expression' }}</option>
        <option value="escape">{{ t('rules.literal') }} ($literal)</option>
      </select>
    </div>
    <div v-if="mode==='computed' && computedValue" class="space-y-2">
      <RuleField v-for="field in schema?.value_expressions?.find(c=>c.id==='computed')?.fields ?? []" :key="field.name" :field="field" :model-value="computedValue[field.name]" :schema="schema!" :path="`/$expr/${field.name}`" :compact="compact" :id-prefix="idPrefix" @update:model-value="updateComputed(field.name,$event)" />
    </div>
    <div v-else-if="mode==='reference' && reference" class="space-y-2">
      <RuleField v-for="field in schema?.value_fields ?? []" :key="field.name" :field="field" :model-value="reference[field.name]" :schema="schema!" :path="`/$ref/${field.name}`" :compact="compact" :id-prefix="idPrefix" @update:model-value="updateReference(field.name,$event as JsonValue)" />
    </div>
    <textarea v-else-if="compact && mode === 'literal' && typeof literalValue === 'string' && !literalOptions" :id="idPrefix ? `${idPrefix}-literal-value` : undefined" class="input" rows="1" :aria-label="ruleLabel('field', 'value', locale)" :value="literalValue" @input="!(($event as InputEvent).isComposing) && setLiteral(($event.target as HTMLTextAreaElement).value)" @compositionend="setLiteral(($event.target as HTMLTextAreaElement).value)" />
    <button v-if="compact && showValueOptions && mode === 'literal' && typeof literalValue === 'string'" class="literal-type-toggle" type="button" :aria-expanded="literalOptions" @click="literalOptions = !literalOptions">{{ t('rules.valueType') }}: {{ ruleOptionLabel('string', locale) }} · {{ literalOptions ? '▾' : '▸' }}</button>
    <JsonValueEditor v-if="['literal', 'escape'].includes(mode) && (!compact || mode !== 'literal' || typeof literalValue !== 'string' || literalOptions)" :model-value="literalValue" :compact="compact" :id-prefix="idPrefix ? `${idPrefix}-literal` : undefined" @update:model-value="mode === 'escape' ? setLiteralEscape($event) : setLiteral($event)" />
  </div>
</template>

<style scoped>
.value-editor-compact { min-width: 0; }
.value-mode-bar { min-width: 0; display: grid; grid-template-columns: auto minmax(0, 1fr); }
.value-mode-bar label { white-space: nowrap; }
.value-mode-bar .input { width: 100%; }
.literal-type-toggle { display: block; padding: 0; color: var(--color-ink-faint); background: transparent; border: 0; font-size: 10px; text-align: left; cursor: pointer; }
</style>
