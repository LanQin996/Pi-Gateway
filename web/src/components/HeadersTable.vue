<script setup lang="ts">
import type { CaptureHeader } from '../api/types'
import { useI18n } from '../i18n'
const { t } = useI18n()

const props = defineProps<{
	headers: CaptureHeader[]
	label?: string
}>()
</script>

<template>
	<div class="code-panel table-scroll">
		<div v-if="props.label" class="code-header">
			<span>{{ props.label }}</span>
		</div>
		<table v-if="props.headers.length" class="w-full">
			<tbody>
				<tr v-for="(header, index) in props.headers" :key="index" class="row-hover">
					<th scope="row" class="td text-left font-normal w-56 border-b font-mono text-[12px] text-[color:var(--color-ink-muted)]">{{ header.name }}</th>
					<td class="td border-b font-mono text-[12px] break-all text-[color:var(--color-ink)]">{{ header.value }}</td>
				</tr>
			</tbody>
		</table>
		<p v-else class="px-3 py-2 text-[12px] text-[color:var(--color-ink-faint)]">{{ t('capture.noHeaders') }}</p>
	</div>
</template>
