<script setup lang="ts">
import {computed, ref, watch} from 'vue'
import type {RuleGraphValue} from '../../utils/ruleGraph'
import type {RuleSchema, JsonValue} from '../../api/rules'
import {useI18n} from '../../i18n'
import RuleField from './RuleField.vue'
const props = defineProps<{modelValue: RuleGraphValue; schema: RuleSchema; nodeId: string}>()
const emit = defineEmits<{'update:modelValue':[value:RuleGraphValue]}>()
const {locale,t} = useI18n()
const expression = computed(() => (props.modelValue.value as {$expr?:{op:string;args:JsonValue[]}})?.$expr)
const reference = computed(() => (props.modelValue.value as {$ref?:Record<string,unknown>})?.$ref)
const literal = ref(''), error = ref('')
const literalHelp = computed(()=>locale.value==='zh-CN'?'JSON 字面量，例如字符串、42、false、null、[]、{}；对象内部不求值。':'JSON literal, e.g. string, 42, false, null, [], {}; object contents are not evaluated.')
watch(() => props.modelValue.value, value => { literal.value = JSON.stringify(value, null, 2); error.value = '' }, {immediate:true})
function setLiteral() {
  try { emit('update:modelValue',{...props.modelValue,value:JSON.parse(literal.value)}); error.value='' }
  catch {error.value=locale.value==='zh-CN'?'请输入有效 JSON，例如 "文本"、0、false 或 null。':'Enter valid JSON, e.g. "text", 0, false or null.'}
}
function updateReference(key:string,value:unknown) {
 const next={...reference.value}; if(value===undefined)delete next[key];else next[key]=value
 emit('update:modelValue',{mode:'reference',value:{$ref:next} as JsonValue})
}
function addArgument() {
 if(expression.value)emit('update:modelValue',{mode:'computed',value:{$expr:{...expression.value,args:[...expression.value.args,null]}}})
 else emit('update:modelValue',{mode:'values',value:[...props.modelValue.value as JsonValue[],null]})
}
</script>
<template>
 <div class="value-node-fields">
  <RuleField v-if="expression" compact :id-prefix="nodeId" :schema="schema" :field="schema.value_expressions!.find(cap=>cap.id==='computed')!.fields.find(field=>field.name==='op')!" :model-value="expression.op" path="/$expr/op" @update:model-value="emit('update:modelValue',{mode:'computed',value:{$expr:{...expression!,op:$event as string}}})" />
  <template v-else-if="reference">
   <RuleField v-for="field in schema.value_fields" :key="field.name" compact :id-prefix="nodeId" :schema="schema" :field="field" :model-value="reference[field.name]" :path="`/$ref/${field.name}`" @update:model-value="updateReference(field.name,$event)" />
  </template>
  <template v-else-if="modelValue.mode==='literal'">
   <label :for="`literal-${nodeId}`">{{locale==='zh-CN'?'JSON 值':'JSON value'}}</label>
   <textarea :id="`literal-${nodeId}`" v-model="literal" class="input literal-input" rows="3" spellcheck="false" :title="literalHelp" @change="setLiteral" />
   <p v-if="error" role="alert">{{error}}</p>
  </template>
  <button v-if="expression || modelValue.mode==='values'" type="button" class="btn" data-testid="value-add-argument" @click="addArgument">+ {{t('rules.addItem')}}</button>
 </div>
</template>
<style scoped>
.value-node-fields {display:grid;gap:8px;min-width:0;font-size:12px}
.literal-input {font-family:monospace;resize:vertical;max-height:220px}
.value-node-fields .btn {height:28px;font-size:11px}
[role='alert'] {color:#ef4444}
</style>
