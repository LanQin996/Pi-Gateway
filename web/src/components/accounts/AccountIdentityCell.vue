<script setup lang="ts">
import { computed } from 'vue'
import type { Account } from '../../api/types'
import { useAccountControls } from './accountControlsLocale'
const props = defineProps<{ account: Account }>()
const { c } = useAccountControls()
const displayTitle = computed(() => props.account.name || props.account.email || c('unnamedAccount'))
const secondary = computed(() => props.account.email || '—')
const initial = computed(() => displayTitle.value.slice(0, 1).toUpperCase())
const tone = computed(() => {
	const tones = ['info', 'success', 'warn', 'danger'] as const
	let hash = 0
	for (const char of displayTitle.value) hash = (hash * 31 + char.codePointAt(0)!) >>> 0
	return tones[hash % tones.length]
})
</script>

<template>
	<div class="identity-cell">
		<span class="identity-avatar" :class="`tone-${tone}`">{{ initial }}</span>
		<div class="identity-copy">
			<div class="identity-title"><strong :title="displayTitle">{{ displayTitle }}</strong></div>
			<span :title="secondary">{{ secondary }}</span>
		</div>
	</div>
</template>

<style scoped>
.identity-cell { min-width: 220px; display: flex; align-items: center; gap: 10px; }
.identity-avatar { width: 36px; height: 36px; flex-shrink: 0; display: inline-flex; align-items: center; justify-content: center; border-radius: 8px; font-weight: 800; }
.identity-copy { min-width: 0; flex: 1; }
.identity-title { display: flex; min-width: 0; align-items: center; gap: 8px; }
.identity-title strong { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; font-weight: 700; }
.identity-copy > span { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; margin-top: 3px; color: var(--color-ink-faint); font-family: var(--font-mono); font-size: 11px; }
.tone-info { color: var(--color-info); background: var(--color-info-soft); }
.tone-success { color: var(--color-success); background: var(--color-success-soft); }
.tone-warn { color: var(--color-warn); background: var(--color-warn-soft); }
.tone-danger { color: var(--color-danger); background: var(--color-danger-soft); }
</style>
