<script setup lang="ts">
import {computed, ref} from 'vue'
import type {RuleField} from '../../api/rules'
import {useI18n} from '../../i18n'
import {localHelp} from '../../utils/ruleSchema'
import {ruleFieldLabel, ruleOptionLabel} from '../../utils/ruleLabels'
const props=defineProps<{field:RuleField}>()
const {t,locale}=useI18n()
const pinned=ref(false)
const exampleText=computed(()=>(props.field.examples??[]).map(v=>`${t('rules.examples')}: ${JSON.stringify(v)}`).join('\n'))
const plain=computed(()=>`${localHelp(props.field.description,locale.value)}\n${t('rules.default')}: ${props.field.default===undefined?t('rules.none'):JSON.stringify(props.field.default)}\n${exampleText.value}`)
</script>
<template>
  <span class="parameter-help" :class="{pinned}" @keydown.esc="pinned=false">
    <button type="button" class="help-trigger" :title="plain" :aria-label="`${ruleFieldLabel(field,locale)}: ${t('rules.fieldHelp')}`" :aria-expanded="pinned" @click="pinned=!pinned">?</button>
    <span class="help-popover" role="tooltip">
      <strong>{{ ruleFieldLabel(field,locale) }}</strong>
      <span>{{ localHelp(field.description,locale) }}</span>
      <span>{{ field.required?t('rules.required'):t('rules.optional') }} · {{ ruleOptionLabel(field.type,locale) }}</span>
      <span>{{ t('rules.default') }}: <code>{{ field.default===undefined?t('rules.none'):JSON.stringify(field.default) }}</code></span>
      <span v-if="field.min!==undefined || field.max!==undefined">{{ field.min ?? '−∞' }} … {{ field.max ?? '∞' }}</span>
      <span v-if="field.depends_on">{{ locale==='zh-CN'?'生效条件':'Applies when' }}: <code>{{ JSON.stringify(field.depends_on) }}</code></span>
      <span v-for="option in field.enum" :key="option"><code>{{ option }}</code>: {{ field.enum_help?.[option] }}</span>
      <span v-for="(example,i) in field.examples" :key="i">{{ t('rules.examples') }}: <code>{{ JSON.stringify(example) }}</code></span>
    </span>
  </span>
</template>
<style scoped>
.parameter-help{position:relative;display:inline-flex;align-items:center}.help-trigger{border:1px solid var(--color-line);border-radius:50%;width:1.15rem;height:1.15rem;font-size:.7rem;cursor:pointer;color:var(--color-ink-muted)}.help-popover{display:none;position:absolute;top:100%;left:0;z-index:100;width:min(27rem,80vw);max-height:24rem;overflow:auto;background:var(--color-surface-elevated,var(--color-canvas));color:var(--color-ink);border:1px solid var(--color-line);border-radius:.5rem;padding:.75rem;box-shadow:0 8px 24px #0003;font-size:.75rem;line-height:1.6;white-space:normal}.help-popover>span{display:block;margin-top:.35rem;overflow-wrap:anywhere}.parameter-help:hover .help-popover,.parameter-help:focus-within .help-popover,.parameter-help.pinned .help-popover{display:block}
</style>
