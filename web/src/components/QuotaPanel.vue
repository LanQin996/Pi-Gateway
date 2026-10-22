<script setup lang="ts">
import { computed, ref } from 'vue'
import type { QuotaView, QuotaWindow } from '../api/types'
import { useI18n, formatNumber } from '../i18n'
import { formatRelative, formatTime, formatUntil } from '../stores/ui'
import Badge from './Badge.vue'
import QuotaCostSummary from './QuotaCostSummary.vue'
import { Infinity as InfinityIcon, Link2, RotateCcw } from 'lucide-vue-next'

const props = defineProps<{
	quota: QuotaView | null
	accountName: string
	// busy disables the reset action while a request is in flight.
	busy?: boolean
	// compact hides the reset-credit details (used inside a table cell).
	compact?: boolean
	// codexLinked tells whether the account has the optional Codex credential.
	// Quota can only be read/consumed when it is attached; when it is false the
	// panel shows guidance to link Codex instead of the refresh/consume actions.
	codexLinked?: boolean
}>()

const emit = defineEmits<{ refresh: []; consume: [creditId: string]; linkCodex: [] }>()

const { t } = useI18n()

// hasCodex defaults to true so existing callers keep the previous behaviour.
const hasCodex = computed(() => props.codexLinked !== false)

// windowsFor renders the primary bucket's 5h/7d pair plus any extra limits.
const primaryWindows = computed<QuotaWindow[]>(() => {
	const windows = props.quota?.windows ?? []
	return windows.filter((w) => w.limit_id === 'codex')
})

const extraWindows = computed<QuotaWindow[]>(() => {
	const windows = props.quota?.windows ?? []
	return windows.filter((w) => w.limit_id !== 'codex')
})

const credits = computed(() => props.quota?.report?.credits ?? null)
const resetCredits = computed(() => props.quota?.reset_credits ?? null)
const availableResets = computed(() => resetCredits.value?.available_count ?? 0)

const expanded = ref(false)

function costFor(window: QuotaWindow) {
	return props.quota?.window_costs?.find((cost) => cost.limit_id === window.limit_id && cost.role === window.role)
}

function barTone(window: QuotaWindow): string {
	if (window.limit_reached || window.used_percent >= 100) { return 'var(--color-danger)' }
	if (window.used_percent >= 80) { return 'var(--color-warn)' }
	return 'var(--color-success)'
}

function labelFor(window: QuotaWindow): string {
	if (window.kind === '5h') { return '5h' }
	if (window.kind === '7d') { return '7d' }
	if (window.window_seconds > 0) {
		const hours = window.window_seconds / 3600
		return hours >= 48 ? `${Math.round(hours / 24)}d` : `${Math.round(hours)}h`
	}
	return window.kind
}

function usedLabel(window: QuotaWindow): string {
	const percent = Number.isInteger(window.used_percent) ? window.used_percent : Number(window.used_percent.toFixed(1))
	return `${percent}%`
}

function resetLabel(window: QuotaWindow): string {
	if (!window.reset_at) { return '' }
	return t('quota.resetsIn', { when: formatUntil(window.reset_at - Date.now()) })
}

function confirmConsume() {
	if (!confirm(t('quota.useResetConfirm', { name: props.accountName }))) { return }
	emit('consume', '')
}
</script>

<template>
	<div class="space-y-3">
		<!-- Header -->
		<div class="flex flex-wrap items-center justify-between gap-2">
			<div class="flex items-center gap-2">
				<h3 class="m-0 text-cp-lg font-heavy text-cp-text">{{ t('quota.title') }}</h3>
				<Badge v-if="quota?.report?.plan_type" tone="info">{{ quota.report.plan_type }}</Badge>
				<Badge v-if="quota?.report?.limit_reached" tone="danger">{{ t('quota.limitReached') }}</Badge>
			</div>
			<div class="flex items-center gap-2">
				<span class="text-[11px] text-[color:var(--color-ink-faint)]">
					{{
						quota?.updated_at
							? t('quota.updated', { when: formatRelative(quota.updated_at) })
							: t('quota.never')
					}}
				</span>
				<button
					v-if="hasCodex"
					class="btn btn-ghost !px-1.5 !py-0.5"
					:disabled="busy"
					@click="emit('refresh')"
				>
					<RotateCcw class="h-3.5 w-3.5" />
					{{ busy ? t('quota.refreshing') : t('quota.refresh') }}
				</button>
			</div>
		</div>

		<!-- Quota requires the optional Codex credential. -->
		<div
			v-if="!hasCodex"
			class="flex flex-wrap items-center justify-between gap-2 rounded-cp bg-cp-fill-quaternary p-3"
		>
			<p class="text-[12px] text-[color:var(--color-ink-muted)]">{{ t('quota.codexRequired') }}</p>
			<button class="btn !py-0.5" @click="emit('linkCodex')">
				<Link2 class="h-3.5 w-3.5" />
				{{ t('quota.linkCodex') }}
			</button>
		</div>

		<p v-if="quota?.error" class="text-[11px] text-[color:var(--color-warn)]">
			{{ t('quota.fetchFailed', { error: quota.error }) }}
		</p>

		<!-- Windows -->
		<div class="inset-panel space-y-1.5">
			<QuotaCostSummary :usage="quota?.cost" />
		</div>
		<div v-if="hasCodex && (primaryWindows.length || extraWindows.length)" class="grid gap-3" :class="compact ? '' : 'sm:grid-cols-2'">
			<div v-for="window in [...primaryWindows, ...(expanded ? extraWindows : [])]" :key="`${window.limit_id}:${window.role}`" class="rounded-cp bg-cp-fill-quaternary p-3">
				<div class="flex flex-wrap items-center justify-between gap-2 text-[12px]">
					<span class="flex flex-wrap items-baseline gap-2">
						<span class="text-[color:var(--color-ink-muted)]">{{ labelFor(window) }}</span>
                        <QuotaCostSummary v-if="costFor(window)" :usage="costFor(window)?.usage" :window-cost="costFor(window)" :cycle-label="labelFor(window)" compact />
						<span v-if="window.limit_id !== 'codex'" class="text-[color:var(--color-ink-faint)]">
							· {{ window.limit_name || window.limit_id }}
						</span>
					</span>
					<span class="flex items-center gap-2">
						<span class="font-mono">{{ usedLabel(window) }}</span>
						<span v-if="window.reset_at" class="text-[11px] text-[color:var(--color-ink-faint)]">
							{{ resetLabel(window) }}
						</span>
					</span>
				</div>
                <progress class="quota-progress mt-2 h-1.5 w-full" :value="Math.min(100, Math.max(0, window.used_percent))" :max="100" :aria-label="labelFor(window)" :aria-valuetext="usedLabel(window)" :style="{'--quota-progress-color': barTone(window)}" />

				<div v-if="window.reset_at" class="mt-0.5 text-[10px] text-[color:var(--color-ink-faint)]">
					{{ t('quota.resetsAt', { time: formatTime(window.reset_at) }) }}
				</div>
			</div>

			<button v-if="extraWindows.length" class="text-[11px] text-[color:var(--color-info)] hover:underline" @click="expanded = !expanded">
				{{ expanded ? '−' : '+' }} {{ t('quota.modelLimits') }} ({{ extraWindows.length }})
			</button>
		</div>
		<p v-else-if="hasCodex" class="text-[12px] text-[color:var(--color-ink-faint)]">{{ t('quota.noQuota') }}</p>

		<!-- Credits + reset credits -->
		<div v-if="hasCodex" class="grid gap-2 sm:grid-cols-2">
			<div class="inset-panel">
				<div class="text-cp-xs font-emphasis text-cp-text-secondary">{{ t('quota.credits') }}</div>
				<div class="mt-1 flex items-center gap-1.5 text-[13px]">
					<template v-if="credits?.unlimited">
						<InfinityIcon class="h-3.5 w-3.5 text-[color:var(--color-accent)]" />
						<span>{{ t('quota.creditsUnlimited') }}</span>
					</template>
					<template v-else-if="credits?.balance">
						<span class="font-mono">{{ credits.balance }}</span>
					</template>
					<template v-else>
						<span class="text-[color:var(--color-ink-faint)]">{{ t('quota.noCredits') }}</span>
					</template>
				</div>
			</div>

			<div class="inset-panel">
				<div class="text-cp-xs font-emphasis text-cp-text-secondary">{{ t('quota.resetCredits') }}</div>
				<div class="mt-1 flex items-center justify-between gap-2">
					<Badge :tone="availableResets > 0 ? 'success' : 'neutral'">
						{{
							availableResets > 0
								? t('quota.resetCreditsAvailable', { count: formatNumber(availableResets) })
								: t('quota.resetCreditsNone')
						}}
					</Badge>
					<button
						v-if="!compact && hasCodex"
						class="btn !py-0.5"
						:disabled="busy || !quota?.available"
						:title="t('quota.resetCreditsHint')"
						@click="confirmConsume"
					>
						{{ t('quota.useReset') }}
					</button>
				</div>
			</div>
		</div>

		<!-- Individual reset cards -->
		<ul v-if="hasCodex && !compact && resetCredits?.credits?.length" class="space-y-1">
			<li
				v-for="credit in resetCredits.credits"
				:key="credit.id"
				class="flex items-center justify-between gap-3 rounded-cp bg-cp-fill-quaternary px-3 py-2.5 text-cp-xs"
			>
				<span class="flex items-center gap-2">
					<Badge :tone="credit.status === 'available' ? 'success' : 'neutral'">{{ credit.status || 'unknown' }}</Badge>
					<span class="text-[color:var(--color-ink-muted)]">{{ credit.title || credit.id }}</span>
				</span>
				<span class="flex items-center gap-2">
					<span v-if="credit.expires_at" class="text-[color:var(--color-ink-faint)]">
						{{ t('quota.expiresAt', { when: credit.expires_at }) }}
					</span>
					<button class="btn !py-0.5" :disabled="busy" @click="emit('consume', credit.id)">
						{{ t('quota.useReset') }}
					</button>
				</span>
			</li>
		</ul>
	</div>
</template>

<style scoped>
.quota-progress { display: block; appearance: none; border: 0; border-radius: 999px; overflow: hidden; background: var(--color-line); accent-color: var(--quota-progress-color); }
.quota-progress::-webkit-progress-bar { background: var(--color-line); border-radius: inherit; }
.quota-progress::-webkit-progress-value { background: var(--quota-progress-color); border-radius: inherit; }
.quota-progress::-moz-progress-bar { background: var(--quota-progress-color); border-radius: inherit; }
</style>
