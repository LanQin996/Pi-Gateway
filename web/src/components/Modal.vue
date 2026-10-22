<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, useId, watch } from 'vue'
import { X } from 'lucide-vue-next'
import { useI18n } from '../i18n'

const props = withDefaults(defineProps<{ open: boolean; title: string; width?: string }>(), { width: 'max-w-xl' })
const emit = defineEmits<{ close: [] }>()
const { t } = useI18n()
const titleId = useId()
const panel = ref<HTMLElement>()
let returnFocus: HTMLElement | null = null
function onKeydown(event: KeyboardEvent) {
	if (!props.open) { return }
	if (event.key === 'Escape') { event.preventDefault(); emit('close'); return }
	if (event.key !== 'Tab') { return }
	const items = Array.from(panel.value?.querySelectorAll<HTMLElement>('button:not(:disabled), [href], input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex="0"]') ?? []).filter(el => el.offsetParent !== null)
	const first = items[0]
	const last = items[items.length - 1]
	if (!first) { event.preventDefault(); panel.value?.focus(); return }
	if (event.shiftKey && (document.activeElement === first || document.activeElement === panel.value)) { event.preventDefault(); last?.focus() }
	else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus() }
}
watch(() => props.open, async open => {
	if (open) {
		returnFocus = document.activeElement as HTMLElement | null
		window.addEventListener('keydown', onKeydown)
		await nextTick()
		panel.value?.focus()
	} else {
		window.removeEventListener('keydown', onKeydown)
		returnFocus?.focus()
	}
}, { immediate: true })
onBeforeUnmount(() => window.removeEventListener('keydown', onKeydown))
</script>

<template>
	<Teleport to="body">
		<div v-if="open" class="fixed inset-0 z-50 grid place-items-center overflow-hidden p-3 sm:p-6" @wheel.stop @touchmove.stop>
			<button type="button" tabindex="-1" class="modal-mask" :aria-label="t('common.close')" @click="emit('close')" />
			<dialog open ref="panel" class="modal-panel outline-none m-0 relative" :class="width" aria-modal="true" :aria-labelledby="titleId" tabindex="-1">
				<div class="modal-header">
					<h2 :id="titleId">{{ title }}</h2>
					<button type="button" class="btn btn-ghost btn-icon btn-sm" :aria-label="t('common.close')" @click="emit('close')"><X /></button>
				</div>
				<div class="modal-body"><slot /></div>
				<div v-if="$slots.footer" class="modal-footer"><slot name="footer" /></div>
			</dialog>
		</div>
	</Teleport>
</template>
