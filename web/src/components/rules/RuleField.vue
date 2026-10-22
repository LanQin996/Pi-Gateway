<script setup lang="ts">
import { computed, ref } from 'vue'
import type { RuleCondition, RuleField, RuleFieldError, RuleSchema, JsonValue } from '../../api/rules'
import { useI18n } from '../../i18n'
import { defaultForField, localHelp } from '../../utils/ruleSchema'
import { moveItem } from '../../utils/ruleEditor'
import { ruleLabel, ruleFieldLabel, ruleOptionLabel } from '../../utils/ruleLabels'
import { samplePathOptions } from '../../utils/samplePaths'
import ValueEditor from './ValueEditor.vue'
import ParameterHelp from './ParameterHelp.vue'
import ActionEditor from './ActionEditor.vue'
import ConditionEditor from './ConditionEditor.vue'

const props = defineProps<{ field: RuleField; modelValue: unknown; path: string; schema: RuleSchema; errors?: RuleFieldError[]; sample?: unknown; phase?: string; compact?: boolean; idPrefix?: string; itemContext?: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [value: unknown] }>()
const { t, locale } = useI18n()
const set = (value: unknown) => emit('update:modelValue', value)
const fieldErrors = computed(() => (props.errors ?? []).filter(e => e.path === props.path || e.path.startsWith(`${props.path}/`) && !['condition', 'condition_array', 'action_array'].includes(props.field.type)))
const controlPrefix = computed(() => props.idPrefix ? props.idPrefix + '-' : '')
const controlId = computed(() => `rule-field-${controlPrefix.value}${props.path}`)
const samplePathId = computed(() => `sample-path-${controlPrefix.value}${props.path}`)
const hoverHelp = computed(() => `${localHelp(props.field.description, locale.value)}\n${(props.field.examples ?? []).map(v => JSON.stringify(v)).join('\n')}`)
const fieldLabel = computed(() => ruleFieldLabel(props.field, locale.value))
const selectedHelp = computed(() => props.field.enum_help?.[String(fieldValue.value)])
// Missing optional fields stay missing until a control is actually changed.
const fieldValue = computed(() => props.compact && props.modelValue === undefined ? defaultForField(props.field) : props.modelValue)
const foldOverride = ref<boolean | null>(null)
const initialLong = ref(isLong(props.modelValue))
function isLong(value: unknown) {
  if (typeof value === 'string') { return value.length > 180 || value.split('\n').length > 3 }
  if (Array.isArray(value) && value.length > 3) { return true }
  if (value && typeof value === 'object') { return JSON.stringify(value).length > 180 }
  return false
}
const foldable = computed(() => props.compact && !props.field.enum?.length && !['number', 'boolean'].includes(props.field.type))
const folded = computed(() => foldable.value && (foldOverride.value ?? initialLong.value) && !fieldErrors.value.length)
const preview = computed(() => {
  const value = props.modelValue
  if (value === undefined) { return t('rules.default') }
  const text = typeof value === 'string' ? value : JSON.stringify(value)
  return (text ?? '').replace(/\s+/g, ' ').slice(0, 96)
})
const items = computed<unknown[]>(() => Array.isArray(fieldValue.value) ? fieldValue.value : [])
const visibleItems = items
const samplePaths = computed(() => props.sample !== undefined && props.field.type === 'string' && ['path','source_path','target_path'].includes(props.field.name) ? samplePathOptions(props.sample) : [])
function selectSamplePath(event: Event) {
  const select = event.target as HTMLSelectElement
  if (samplePaths.value.some(option=>option.path===select.value)) { set(select.value) }
  select.value = '__sample_path_placeholder__'
}
function updateItem(index: number, value: unknown) { set(items.value.map((x, i) => i === index ? value : x)) }
function textInput(event: Event, index?: number) {
  if (props.compact && (event as InputEvent).isComposing) { return }
  const value = (event.target as HTMLInputElement | HTMLTextAreaElement).value
  if (index === undefined) { set(value) }
  else { updateItem(index, value) }
}
function numberInput(event: Event) {
  const input = event.target as HTMLInputElement
  const number = Number(input.value)
  const valid = input.value.trim() !== '' && Number.isSafeInteger(number)
  input.setCustomValidity(valid ? '' : t('rules.numberError'))
  if (valid) { set(number) }
}
</script>
<template>
  <div class="rule-field space-y-1.5" :class="{'rule-field-compact': compact, 'rule-field-wide': ['value','values','strings','condition','condition_array','action_array'].includes(field.type), 'is-folded': folded}" :data-rule-path="path" :data-renderer="field.type">
    <div class="rule-field-heading flex flex-wrap items-center gap-2">
      <label :id="`${controlId}-label`" :for="controlId" class="text-[12px] font-medium" :title="`${localHelp(field.description, locale)}\n${(field.examples ?? []).map(v=>JSON.stringify(v)).join('\n')}`">{{ fieldLabel }}</label><ParameterHelp :field="field" />
      <span v-if="!compact" class="text-[10px] text-[color:var(--color-ink-faint)]">{{ field.required ? t('rules.required') : t('rules.optional') }} · {{ ruleOptionLabel(field.type, locale) }}</span>
      <button v-if="!field.required && !field.readonly" class="rule-field-presence btn btn-ghost text-[10px]" type="button" @click="set(modelValue === undefined ? defaultForField(field) : undefined)">{{ (() => { if (compact) { if (modelValue === undefined) { return t('rules.default'); } return t('rules.unsetShort'); } if (modelValue === undefined) { return t('rules.include'); } return t('rules.unset'); })() }}</button>
      <button v-if="foldable" type="button" class="rule-field-fold" :aria-expanded="!folded" :aria-label="t(folded ? 'rules.canvas.expandField' : 'rules.canvas.collapseField', {name: fieldLabel})" @click="foldOverride = !folded">{{ folded ? '▸' : '▾' }}</button>
      <details v-if="!compact" class="text-[11px] text-[color:var(--color-ink-muted)]">
        <summary class="cursor-pointer">{{ t('rules.fieldHelp') }}</summary>
        <div class="my-2 space-y-1 whitespace-normal rounded border border-[color:var(--color-line)] p-2">
          <p>{{ localHelp(field.description, locale) }}</p>
          <p>{{ t('rules.default') }}: <code>{{ field.default === undefined ? t('rules.none') : JSON.stringify(field.default) }}</code></p>
          <p v-if="field.enum?.length">{{ t('rules.options') }}: {{ field.enum.map(option => ruleOptionLabel(option, locale)).join(' / ') }}</p>
          <p v-if="field.min !== undefined">{{ t('rules.minimum') }}: {{ field.min }}</p><p v-if="field.max !== undefined">{{ t('rules.maximum') }}: {{ field.max }}</p>
          <p v-if="field.depends_on"><code>{{ JSON.stringify(field.depends_on) }}</code></p>
          <p v-for="(example, i) in field.examples" :key="i">{{ t('rules.examples') }}: <code>{{ JSON.stringify(example) }}</code></p>
        </div>
      </details>
    </div>
    <button v-if="folded" type="button" class="rule-field-preview" :title="hoverHelp" :aria-label="t('rules.canvas.expandField', {name: fieldLabel})" @click="foldOverride = false">{{ preview || '…' }}</button>
    <div :style="folded ? {display: 'none'} : undefined" class="rule-field-control">
    <div v-if="samplePaths.length" data-testid="sample-field-picker" class="space-y-1">
      <label v-if="!compact" :for="samplePathId" class="text-[11px]">{{ t('rules.canvas.samplePaths') }} · {{ ruleLabel('field',field.name,locale) }}</label>
      <select :id="samplePathId" :aria-label="`${t('rules.canvas.samplePaths')} · ${ruleLabel('field', field.name, locale)}`" :data-testid="`sample-path-select-${field.name}`" class="input" value="__sample_path_placeholder__" @change="selectSamplePath">
        <option value="__sample_path_placeholder__" disabled>{{ t('rules.canvas.samplePaths') }}</option>
        <option v-for="option in samplePaths" :key="option.path" :value="option.path">{{ option.path === '' ? t('rules.canvas.samplePathRoot') : option.label }}</option>
      </select>
      <p v-if="!compact" class="text-[10px] text-[color:var(--color-ink-muted)]">{{ t('rules.canvas.samplePathHint') }}</p>
    </div>
    <div v-if="!compact && modelValue === undefined && !field.required" class="text-[11px] text-[color:var(--color-ink-faint)]">{{ t('rules.none') }}</div>
    <template v-if="compact || modelValue !== undefined || field.required">
      <select v-if="field.enum?.length" :id="controlId" :title="`${hoverHelp}\n${selectedHelp ?? ''}`" class="input" :disabled="field.readonly" :value="fieldValue" :aria-invalid="!!fieldErrors.length" @change="set(($event.target as HTMLSelectElement).value)"><option v-for="option in field.enum" :key="option" :value="option" :title="field.enum_help?.[option]">{{ ruleOptionLabel(option, locale) }}</option></select>
      <textarea v-else-if="field.type === 'string'" :id="controlId" :title="hoverHelp" class="input" :rows="compact ? 1 : 2" :readonly="field.readonly" :value="String(fieldValue ?? '')" :required="field.non_empty && (!compact || modelValue !== undefined || field.required)" :aria-invalid="!!fieldErrors.length" @input="textInput" @compositionend="compact && textInput($event)" />
      <input v-else-if="field.type === 'number'" :id="controlId" :title="hoverHelp" class="input" type="number" step="1" :required="!compact || modelValue !== undefined || field.required" :readonly="field.readonly" :min="field.min" :max="field.max" :value="fieldValue ?? 0" :aria-invalid="!!fieldErrors.length" @input="numberInput" />
      <input v-else-if="field.type === 'boolean'" :id="controlId" :title="hoverHelp" type="checkbox" :disabled="field.readonly" :checked="!!fieldValue" @change="set(($event.target as HTMLInputElement).checked)" />
      <ValueEditor v-else-if="field.type === 'value'" :model-value="(fieldValue ?? null) as JsonValue" :schema="schema" :compact="compact" :id-prefix="controlId" @update:model-value="set" />
      <ConditionEditor v-else-if="field.type === 'condition'" :model-value="(fieldValue ?? {op:'always'}) as RuleCondition" :schema="schema" :path="path" :errors="errors" :sample="sample" :compact="compact" :id-prefix="idPrefix" :item-context="itemContext" @update:model-value="set" />
      <fieldset v-else-if="field.type === 'strings' || field.type === 'values'" :id="controlId" class="space-y-2"  :aria-labelledby="`${controlId}-label`">
        <div v-for="(item, index) in visibleItems" :key="index" class="rule-list-row">
          <input v-if="field.type === 'strings'" :id="idPrefix ? `${controlId}-${index}` : undefined" class="input min-w-0 flex-1" :value="item" :aria-label="`${ruleLabel('field', field.name, locale)} ${index + 1}`" @input="textInput($event, index)" @compositionend="compact && textInput($event, index)" />
          <ValueEditor v-else class="min-w-0 flex-1" :model-value="item as JsonValue" :schema="schema" :compact="compact" :id-prefix="`${controlId}-${index}`" @update:model-value="updateItem(index, $event)" />
          <div class="rule-list-buttons"><button type="button" class="btn btn-ghost" :disabled="index === 0" :title="t('rules.up')" @click="set(moveItem(items, index, -1))">↑</button>
          <button type="button" class="btn btn-ghost" :disabled="index === items.length - 1" :title="t('rules.down')" @click="set(moveItem(items, index, 1))">↓</button>
          <button type="button" class="btn btn-ghost" :title="t('rules.remove')" @click="set(items.filter((_, i) => i !== index))">×</button></div>
        </div>
        <button type="button" class="btn btn-ghost" @click="set([...items, field.type === 'strings' ? '' : null])">{{ t('rules.addItem') }}</button>
      </fieldset>
    </template>
    <p v-if="selectedHelp && !compact" class="text-[11px] text-[color:var(--color-ink-muted)]" data-testid="selected-option-help">{{ localHelp(selectedHelp, locale) }}</p>
    <div v-if="field.type === 'condition_array'" class="space-y-2">
      <div v-for="(item,index) in items" :key="index" class="flex items-start gap-1">
        <ConditionEditor class="min-w-0 flex-1" :model-value="item as RuleCondition" :schema="schema" :path="`${path}/${index}`" :errors="errors" :sample="sample" :compact="compact" :id-prefix="`${controlId}-${index}`" @update:model-value="updateItem(index,$event)" />
        <div class="flex flex-col gap-1">
          <button type="button" class="btn btn-ghost" :disabled="index === 0" :title="t('rules.up')" @click="set(moveItem(items,index,-1))">↑</button>
          <button type="button" class="btn btn-ghost" :disabled="index === items.length - 1" :title="t('rules.down')" @click="set(moveItem(items,index,1))">↓</button>
          <button type="button" class="btn btn-ghost" :title="t('rules.remove')" @click="set(items.filter((_,i)=>i!==index))">×</button>
        </div>
      </div>
      <button type="button" class="btn btn-ghost" @click="set([...items,{op:'always'}])">{{ t('rules.addItem') }}</button>
    </div>
    <ActionEditor v-if="field.type === 'action_array'" :model-value="(fieldValue ?? []) as import('../../api/rules').RuleAction[]" :schema="schema" :phase="phase ?? 'request'" :base-path="path" :errors="errors" :sample="sample" :compact="compact" :id-prefix="idPrefix" @update:model-value="set" />
    </div>
    <p v-for="error in fieldErrors" :key="error.path + error.message" class="text-xs text-red-500" role="alert">{{ error.path }}: {{ error.message }}</p>
  </div>
</template>
<style scoped>
.rule-field-compact { display: grid; grid-template-columns: 118px minmax(0, 1fr); align-items: center; column-gap: 10px; row-gap: 4px; min-width: 0; padding: 5px 0; border-bottom: 1px solid var(--color-line); writing-mode: horizontal-tb; }
.rule-field-compact:last-child { border-bottom: 0; }
.rule-field-compact.rule-field > * { min-width: 0; margin-top: 0; }
.rule-field-presence { padding: 2px 6px; }
.rule-field-heading { gap: 4px; align-items: center; }
.rule-field-compact .rule-field-heading { flex-wrap: nowrap; }
.rule-field-compact .rule-field-heading > label { flex: 1; min-width: 0; font-size: 11px; line-height: 1.5; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.rule-field-compact .rule-field-heading :deep(.parameter-help) { flex-shrink: 0; }
.rule-field-compact .rule-field-presence { flex-shrink: 0; padding: 1px 4px; font-size: 9px; min-height: 18px; color: var(--color-ink-faint); white-space: nowrap; display: none; }

.rule-field-compact .rule-field-heading:hover .rule-field-presence, .rule-field-compact .rule-field-heading:focus-within .rule-field-presence { display: inline-flex; }
.rule-field-compact.rule-field-wide { grid-template-columns: minmax(0, 1fr); }
.rule-field-compact.rule-field-wide .rule-field-heading > label { flex: 0 1 auto; }
.rule-field-control { min-width: 0; container-type: inline-size; }
.rule-list-row {display:grid;grid-template-columns:minmax(0,1fr) auto;align-items:start;gap:6px}
.rule-list-buttons {display:flex;gap:2px;padding-top:2px}
.rule-field-control .rule-list-buttons .btn {width:26px;height:26px;min-width:26px;padding:0;display:flex;align-items:center;justify-content:center}
.rule-field-fold { flex-shrink: 0; display: inline-flex; align-items: center; justify-content: center; width: 20px; height: 22px; border: 0; border-radius: 4px; color: var(--color-ink-muted); background: var(--color-surface-2); cursor: pointer; }
.rule-field-preview { display: block; width: 100%; min-height: 28px; text-align: left; padding: 4px 8px; border: 1px solid var(--color-line); border-radius: 5px; background: var(--color-surface-2); color: var(--color-ink-muted); font-family: var(--font-mono); font-size: 11px; line-height: 18px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; cursor: pointer; }
.rule-field-compact :deep(.input) { width: 100%; min-width: 0; min-height: 30px; padding: 5px 8px; font-size: 11px; line-height: 18px; border: 1px solid var(--color-line); border-radius: 6px; background: var(--color-surface-2); box-shadow: none; }
.rule-field-compact :deep(textarea) { resize: vertical; max-height: 200px; }
.rule-field-compact :deep(select) { max-width: 100%; text-overflow: ellipsis; }
.rule-field-compact :deep(input[type='checkbox']) { display: block; margin: 6px 0 6px auto; width: 16px; height: 16px; }
.rule-field-compact :deep(.btn) { font-size: 10px; min-width: 0; min-height: 24px; padding: 3px 6px; white-space: nowrap; }
.rule-field-compact [role='alert'] { grid-column: 1 / -1; overflow-wrap: anywhere; font-size: 10px; }
@container (max-width: 290px) { .rule-field-compact { grid-template-columns: minmax(0, 1fr); } .rule-field-compact .rule-field-heading > label { flex: 0 1 auto; } }
</style>
