<script setup lang="ts">
import { computed, ref } from 'vue'
import type { RuleCondition, RuleFieldError, RuleSchema } from '../../api/rules'
import { useI18n } from '../../i18n'
import { defaultCondition, moveItem } from '../../utils/ruleEditor'
import { defaultForField, localHelp } from '../../utils/ruleSchema'
import { ruleLabel } from '../../utils/ruleLabels'
import { currentSamplePathField } from '../../utils/samplePaths'
import RuleField from './RuleField.vue'
const props = withDefaults(defineProps<{ modelValue: RuleCondition; schema: RuleSchema; path?: string; errors?: RuleFieldError[]; compact?: boolean; sample?: unknown; idPrefix?: string; itemContext?: boolean }>(), { path: '/when' })
const emit = defineEmits<{ 'update:modelValue': [value: RuleCondition] }>()
const { t, locale } = useI18n()
const cap = computed(() => props.schema.conditions.find(c => c.id === props.modelValue.op))
const group = computed(() => ['all', 'any', 'not'].includes(props.modelValue.op))
const children = computed(() => props.modelValue.conditions ?? [])
const showOptions = ref(false)
const primary = (f: import('../../api/rules').RuleField) => props.itemContext && f.name === 'source' && props.modelValue.source === 'item' ? false : f.required || ['path', 'source'].includes(f.name) || (props.modelValue as unknown as Record<string, unknown>)[f.name] !== undefined && JSON.stringify((props.modelValue as unknown as Record<string, unknown>)[f.name]) !== JSON.stringify(defaultForField(f)) || props.errors?.some(e => e.path.startsWith(`${props.path}/${f.name}`))
const hiddenCount = computed(() => (cap.value?.fields ?? []).filter(f => !primary(f)).length)
const visibleFields = computed(() => (cap.value?.fields ?? []).filter(f => !props.compact || showOptions.value || primary(f)))
function field(name: string, value: unknown) { const next = {...props.modelValue} as Record<string, unknown>; if (value === undefined) { delete next[name]; } else { next[name] = value; } emit('update:modelValue', next as unknown as RuleCondition) }
function updateChild(index: number, value: RuleCondition) { field('conditions', children.value.map((c, i) => i === index ? value : c)) }
function changeOp(op: string) {
  const next = defaultCondition(op, props.schema)
  const allowed = new Set(props.schema.conditions.find(c => c.id === op)?.fields.map(f => f.name))
  for (const [key, value] of Object.entries(props.modelValue)) if (allowed.has(key)) { (next as unknown as Record<string, unknown>)[key] = value }
  if (props.itemContext && allowed.has('source') && props.modelValue.source === 'item') { next.source = 'item' }
  emit('update:modelValue', next)
}
</script>
<template>
  <div class="rule-condition-card space-y-3 rounded-lg border border-[color:var(--color-line)] p-3" :class="{'is-compact': compact}" :data-rule-path="path" data-renderer="condition">
    <div class="flex flex-wrap items-start gap-2">
      <label class="min-w-0 flex-1 text-[11px] text-[color:var(--color-ink-muted)]">{{ t('rules.conditionType') }}
        <select class="input mt-1" :title="localHelp(cap?.description, locale)" :value="modelValue.op" @change="changeOp(($event.target as HTMLSelectElement).value)"><option v-for="item in schema.conditions" :key="item.id" :value="item.id">{{ ruleLabel('condition', item.id, locale) }}</option></select>
      </label>
      <p v-if="!compact" class="min-w-0 flex-[2] pt-4 text-[11px] text-[color:var(--color-ink-muted)]">{{ localHelp(cap?.description, locale) }}</p>
    </div>
    <template v-if="group">
      <div v-for="(child, index) in children" :key="index" class="flex items-start gap-1">
        <ConditionEditor class="min-w-0 flex-1" :model-value="child" :schema="schema" :path="`${path}/conditions/${index}`" :errors="errors" :sample="sample" :compact="compact" :item-context="itemContext" :id-prefix="idPrefix ? `${idPrefix}-${index}` : undefined" @update:model-value="updateChild(index, $event)" />
        <div v-if="modelValue.op !== 'not'" class="flex flex-col gap-1">
          <button type="button" class="btn btn-ghost !px-2" :disabled="index === 0" :title="t('rules.up')" @click="field('conditions', moveItem(children, index, -1))">↑</button>
          <button type="button" class="btn btn-ghost !px-2" :disabled="index === children.length - 1" :title="t('rules.down')" @click="field('conditions', moveItem(children, index, 1))">↓</button>
          <button type="button" class="btn btn-ghost !px-2" :title="t('rules.remove')" @click="field('conditions', children.filter((_, i) => i !== index))">×</button>
        </div>
      </div>
      <button v-if="modelValue.op !== 'not' || !children.length" type="button" class="btn" @click="field('conditions', [...children, {op:'always'}])">{{ t('rules.addCondition') }}</button>
    </template>
    <div v-else class="rule-condition-fields">
      <RuleField v-for="f in visibleFields" :key="f.name" :compact="compact" :id-prefix="idPrefix" :field="f" :model-value="(modelValue as unknown as Record<string, unknown>)[f.name]" :schema="schema" :path="`${path}/${f.name}`" :errors="errors" :sample="currentSamplePathField(f, modelValue.source) ? sample : undefined" @update:model-value="field(f.name, $event)" />
    </div>
    <button v-if="compact && hiddenCount && !group" type="button" class="condition-options" :aria-expanded="showOptions" @click="showOptions = !showOptions">{{ t(showOptions ? 'rules.canvas.lessParameters' : 'rules.canvas.showParameters', {count: hiddenCount}) }}</button>
    <p v-for="error in (errors ?? []).filter(e => e.path === path || e.path === `${path}/op` || e.path === `${path}/conditions`)" :key="error.path + error.message" role="alert" class="text-xs text-red-500">{{ error.path }}: {{ error.message }}</p>
  </div>
</template>

<style scoped>
.rule-condition-card { min-width: 0; }
.rule-condition-fields { display: grid; gap: 10px; }
.rule-condition-card.is-compact { padding: 8px; background: var(--color-surface); margin-top: 0; }
.rule-condition-card.is-compact > div:first-child { display: block; }
.rule-condition-card.is-compact > div:first-child > p { display: none; }

.rule-condition-card.is-compact .rule-condition-fields { gap: 2px; }
.condition-options { width: 100%; min-height: 26px; font-size: 11px; color: var(--color-ink-muted); background: var(--color-surface-2); border-radius: 4px; }
</style>
