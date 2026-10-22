<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref } from 'vue'
import { BarChart3, Link2, MoreHorizontal, Pencil, Power, RefreshCw, Trash2, Unlink, Send } from 'lucide-vue-next'
import type { Account } from '../../api/types'
import { useI18n } from '../../i18n'
import { useAccountControls } from './accountControlsLocale'

const props = defineProps<{ account: Account; quotaBusy: boolean }>()
const emit = defineEmits<{ edit: [account: Account]; delete: [account: Account]; recover: [account: Account]; refresh: [account: Account]; quota: [account: Account]; refreshQuota: [account: Account]; link: [account: Account]; unlink: [account: Account]; toggle: [account: Account]; test: [account: Account] }>()
const { t } = useI18n()
const { c } = useAccountControls()
const open = ref(false)
const trigger = ref<HTMLButtonElement>()
const panel = ref<HTMLElement>()
const position = ref({ top: '0px', left: '0px' })
const items = computed(() => [
	{ label: c('manualRecover'), icon: RefreshCw, action: () => emit('recover', props.account) },
	{ label: c('modelTest'), icon: Send, action: () => emit('test', props.account) },
	{ label: c(props.account.enabled ? 'disableAccount' : 'enableAccount'), icon: Power, action: () => emit('toggle', props.account) },
	{ label: t('accounts.refreshToken'), icon: RefreshCw, action: () => emit('refresh', props.account) },
	...(props.account.codex_linked ? [
		{ label: t('quota.viewDetails'), icon: BarChart3, action: () => emit('quota', props.account) },
		{ label: t('quota.refresh'), icon: RefreshCw, disabled: props.quotaBusy, action: () => emit('refreshQuota', props.account) },
		{ label: t('accounts.unlinkCodex'), icon: Unlink, action: () => emit('unlink', props.account) },
	] : [{ label: t('accounts.linkCodex'), icon: Link2, action: () => emit('link', props.account) }]),
])
function close(restoreFocus = false) {
	open.value = false
	document.removeEventListener('pointerdown', outside)
	window.removeEventListener('resize', dismiss)
	window.removeEventListener('scroll', scroll, true)
	if (restoreFocus) { trigger.value?.focus() }
}
function dismiss() { close() }
function scroll(event: Event) { if (!(event.target instanceof Node) || !panel.value?.contains(event.target)) { close() } }
function outside(event: PointerEvent) {
	if (event.target instanceof Node && !panel.value?.contains(event.target) && !trigger.value?.contains(event.target)) { close() }
}
async function toggle() {
	if (open.value) { return close(true) }
	const rect = trigger.value?.getBoundingClientRect()
	if (!rect) { return }
	position.value = { left: `${Math.max(8, Math.min(rect.right - 208, window.innerWidth - 216))}px`, top: `${rect.bottom + 6}px` }
	open.value = true
	await nextTick()
	const height = panel.value?.offsetHeight ?? 0
	if (rect.bottom + height + 14 > window.innerHeight) { position.value.top = `${Math.max(8, rect.top - height - 6)}px` }
	panel.value?.querySelector<HTMLButtonElement>('button:not(:disabled)')?.focus()
	document.addEventListener('pointerdown', outside)
	window.addEventListener('resize', dismiss)
	window.addEventListener('scroll', scroll, true)
}
function navigate(event: KeyboardEvent) {
	if (event.key === 'Escape') { event.preventDefault(); close(true); return }
	if (event.key === 'Tab') { close(); return }
	if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) { return }
	event.preventDefault()
	const buttons = [...(panel.value?.querySelectorAll<HTMLButtonElement>('button:not(:disabled)') ?? [])]
	const current = buttons.indexOf(document.activeElement as HTMLButtonElement)
	const index = (() => {
    if (event.key === 'Home') {
        return 0
    }
    if (event.key === 'End') {
        return buttons.length - 1
    }
    return (current + (event.key === 'ArrowDown' ? 1 : -1) + buttons.length) % buttons.length
})()
	buttons[index]?.focus()
}
onBeforeUnmount(() => close())
</script>

<template>
	<div class="account-actions">
		<button type="button" class="btn btn-ghost action-icon edit-action" :title="t('accounts.edit')" :aria-label="t('accounts.edit')" @click="emit('edit', account)"><Pencil :size="14" /></button>
		<button type="button" class="btn btn-ghost action-icon delete-action" :title="t('accounts.delete')" :aria-label="t('accounts.delete')" @click="emit('delete', account)"><Trash2 :size="14" /></button>
		<button ref="trigger" type="button" class="btn btn-ghost action-icon" :title="t('accounts.more')" :aria-label="t('accounts.more')" aria-haspopup="menu" :aria-expanded="open" @click="toggle"><MoreHorizontal :size="16" /></button>
		<Teleport to="body"><div v-if="open" ref="panel" class="action-menu" :style="position" role="menu" :aria-label="t('accounts.more')" @keydown="navigate"><button v-for="item in items" :key="item.label" type="button" role="menuitem" :disabled="'disabled' in item && item.disabled" @click="close(true); item.action()"><component :is="item.icon" :size="14" />{{ item.label }}</button></div></Teleport>
	</div>
</template>

<style scoped>
.account-actions { display: flex; gap: 4px; align-items: center; }
.action-icon { width: 28px; min-height: 28px; padding: 0; border-radius: 6px; }
.edit-action { color: var(--color-accent); }
.delete-action { color: var(--color-danger); }
.action-menu { position: fixed; z-index: 100; width: 208px; max-height: calc(100dvh - 16px); overflow-y: auto; border-radius: 10px; padding: 6px; border: 1px solid var(--color-line); background: var(--color-surface); box-shadow: var(--shadow-card); }
.action-menu button { width: 100%; display: flex; gap: 8px; align-items: center; border: 0; border-radius: 6px; padding: 9px 8px; background: transparent; color: var(--color-ink); font-size: 12px; text-align: left; }
.action-menu button svg { flex-shrink: 0; color: var(--color-ink-faint); }
.action-menu button:hover, .action-menu button:focus-visible { background: var(--color-surface-2); }
.action-menu button:disabled { opacity: .5; cursor: not-allowed; }
</style>
