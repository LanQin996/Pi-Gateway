<script setup lang="ts">
import {useId, computed} from 'vue'
import { ruleLabel, ruleLabels } from '../../utils/ruleLabels'
import type { RuleAction, RuleFieldError, RuleSchema } from '../../api/rules'
import { useI18n } from '../../i18n'
import { localHelp, newAction } from '../../utils/ruleSchema'
import { moveItem } from '../../utils/ruleEditor'
import RuleNodeFields from './RuleNodeFields.vue'
const formId = useId()
const props = withDefaults(defineProps<{ modelValue: RuleAction[]; schema: RuleSchema; phase: string; errors?: RuleFieldError[]; compact?: boolean; pathOffset?: number; sample?: unknown; basePath?: string; idPrefix?: string }>(), {pathOffset: 0})
const emit = defineEmits<{ 'update:modelValue': [value: RuleAction[]] }>()
const { t, locale } = useI18n()
const actionPath=(index:number)=>`${props.basePath ?? '/actions'}/${index+props.pathOffset}`
const available = computed(() => props.schema.actions.filter(c => c.id !== 'sequence' && (!c.phases?.length || c.phases.includes(props.phase as never))))
function set(value: RuleAction[]) { emit('update:modelValue', value) }
function update(index: number, key: string, value: unknown) { set(props.modelValue.map((a, i) => i === index ? {...a, [key]: value} as RuleAction : a)) }
function changeType(index: number, type: string) { set(props.modelValue.map((a, i) => i === index ? {...newAction(type, props.schema), id: a.id} : a)) }
function add(type: string) { set([...props.modelValue, newAction(type, props.schema)]) }
function remove(index: number) { set(props.modelValue.filter((_, i) => i !== index)) }
</script>
<template>
  <div class="space-y-3">
    <div v-for="(action, index) in modelValue" :key="action.id" class="rule-action-card rounded-lg border border-[color:var(--color-line)] p-3" :class="{'is-compact': compact}" :data-rule-path="actionPath(index)">
      <div class="flex flex-wrap items-start gap-2">
        <span class="rounded bg-[color:var(--color-surface-2)] px-2 py-1 font-mono text-xs">{{ index + 1 }}</span>
        <label class="sr-only" :for="`${formId}-${action.id}-type`">{{ t('rules.actionType') }}</label><select :id="`${formId}-${action.id}-type`" class="input min-w-0 flex-1" :value="action.type" @change="changeType(index, ($event.target as HTMLSelectElement).value)"><option v-for="cap in available" :key="cap.id" :value="cap.id">{{ (() => { if (ruleLabels.action?.[cap.id]) { return ruleLabel('action',cap.id,locale); } return cap.label?.split(' / ')[locale==='zh-CN'?0:1] ?? cap.id; })() }}</option></select>
        <button class="btn btn-ghost !px-2" type="button" :disabled="index === 0" :title="t('rules.up')" @click="set(moveItem(modelValue, index, -1))">↑</button>
        <button class="btn btn-ghost !px-2" type="button" :disabled="index === modelValue.length - 1" :title="t('rules.down')" @click="set(moveItem(modelValue, index, 1))">↓</button>
        <button class="btn btn-ghost !px-2" type="button" :title="t('rules.remove')" @click="remove(index)">×</button>
      </div>
      <details class="mt-2 text-[11px]"><summary>{{ t('rules.canvas.details') }}</summary><label class="mt-2 block" :for="`${formId}-${action.id}`">{{ t('rules.actionID') }}<input :id="`${formId}-${action.id}`" class="input mt-1 font-mono" :value="action.id" required @change="update(index, 'id', ($event.target as HTMLInputElement).value)" /></label></details>
      <p v-for="error in (errors ?? []).filter(e => e.path === `${actionPath(index)}/id` || e.path === `${actionPath(index)}/type` || e.path === `${actionPath(index)}/params`)" :key="error.path" role="alert" class="text-xs text-red-500">{{ error.path }}: {{ error.message }}</p>
      <p class="mt-2 text-[11px] text-[color:var(--color-ink-muted)]">{{ localHelp(schema.actions.find(c => c.id === action.type)?.description, locale) }}</p>
      <div class="rule-action-fields">
        <RuleNodeFields :node-id="`${idPrefix ?? 'step'}-${action.id}`" kind="action" :model-value="action" :schema="schema" :phase="phase as import('../../api/rules').RulePhase" :path="actionPath(index)" :errors="errors" :sample="sample" @update:model-value="update(index, 'params', ($event as RuleAction).params)" />
      </div>
    </div>
    <div class="flex flex-wrap gap-2">
      <select :id="`add-rule-action${basePath ? `-${basePath}` : ''}`" class="input max-w-xs" @change="add(($event.target as HTMLSelectElement).value); ($event.target as HTMLSelectElement).value = ''"><option value="">{{ t('rules.addAction') }}</option><option v-for="cap in available" :key="cap.id" :value="cap.id">{{ (() => { if (ruleLabels.action?.[cap.id]) { return ruleLabel('action',cap.id,locale); } return cap.label?.split(' / ')[locale==='zh-CN'?0:1] ?? cap.id; })() }}</option></select>
      <span v-if="!modelValue.length" class="text-xs text-[color:var(--color-ink-muted)]">{{ t('rules.noActions') }}</span>
    </div>
  </div>
</template>

<style scoped>
.rule-action-fields { display: grid; gap: 10px; margin-top: 10px; }
.rule-action-card { min-width: 0; }
.rule-action-card.is-compact { padding: 8px; background: var(--color-surface); }
.rule-action-card.is-compact > p { display: none; }
</style>
