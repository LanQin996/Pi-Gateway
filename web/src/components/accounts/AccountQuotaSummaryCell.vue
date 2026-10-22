<script setup lang="ts">
import { computed } from 'vue'
import type { Account, QuotaWindow } from '../../api/types'
import { formatTime, formatUntil } from '../../stores/ui'
import { RotateCcw } from 'lucide-vue-next'
import { useI18n } from '../../i18n'
import QuotaCostSummary from '../QuotaCostSummary.vue'
const { t } = useI18n()

const props = defineProps<{ account: Account; onOpen: (account: Account) => void; onLink: (account: Account) => void }>()
const windows = computed(() => [props.account.quota?.session, props.account.quota?.weekly].filter((item): item is QuotaWindow => Boolean(item)))
const credits = computed(() => props.account.quota?.report?.credits)
const creditBalance = computed(() => {
	const balance = credits.value?.balance?.trim()
	// Avoid Number conversion: credit balances can contain high-precision decimals.
	if (!balance || !/^[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?$/.test(balance)) { return '' }
	return /[1-9]/.test(balance.split(/[eE]/)[0] ?? '') ? balance : ''
})
const showCredits = computed(() => Boolean(credits.value?.unlimited || creditBalance.value))
const resetCount = computed(() => props.account.quota?.reset_credits?.available_count ?? 0)
function costFor(window: QuotaWindow) {
	return props.account.quota?.window_costs?.find(cost => cost.limit_id === window.limit_id && cost.role === window.role)
}
function tone(window: QuotaWindow) { return (() => {
    if (window.limit_reached || window.used_percent >= 100) {
        return 'danger'
    }
    if (window.used_percent >= 80) {
        return 'warn'
    }
    return 'success'
})() }
function label(window: QuotaWindow) { return (() => {
    if (window.kind === '5h') {
        return '5h'
    }
    if (window.kind === '7d') {
        return '7d'
    }
    return window.kind
})() }
</script>

<template>
	<div class="quota-cell">
		<template v-if="!account.codex_linked">
			<span class="quota-muted">—</span>
			<button class="btn btn-ghost quota-link" type="button" @click="onLink(account)">{{ t('accounts.linkCodex') }}</button>
			<QuotaCostSummary :usage="account.quota?.cost" compact />
		</template>
		<template v-else>
			<button class="quota-button" type="button" :title="t('quota.viewDetails')" @click="onOpen(account)">
				<span class="quota-overview">
					<span class="quota-windows">
						<template v-if="windows.length">
							<span v-for="window in windows" :key="window.role" class="quota-window">
								<span class="quota-line"><span class="quota-window-heading"><span class="quota-window-label">{{ label(window) }}</span><QuotaCostSummary v-if="costFor(window)" :usage="costFor(window)?.usage" :window-cost="costFor(window)" :cycle-label="label(window)" compact /></span><strong :class="`quota-${tone(window)}`">{{ Math.round(window.used_percent) }}%</strong></span>
								<span class="quota-track"><i :class="`quota-${tone(window)}`" :style="{ width: `${Math.min(100, Math.max(0, window.used_percent))}%` }" /></span>
								<span v-if="window.reset_at" class="quota-reset" :title="t('quota.resetsAt', {time: formatTime(window.reset_at)})">{{ formatUntil(window.reset_at - Date.now()) }}</span>
							</span>
						</template>
						<span v-else class="quota-muted">{{ t('quota.never') }}</span>
					</span>
					<span v-if="showCredits" class="quota-balance"><span>{{ t('quota.credits') }}</span><strong v-if="credits?.unlimited">{{ t('quota.creditsUnlimited') }}</strong><strong v-else>{{ creditBalance }}</strong></span>
				</span>
				<span v-if="resetCount > 0" class="quota-meta">
					<span v-if="resetCount > 0" class="quota-credit" :title="t('quota.resetCreditsAvailable', { count: resetCount })"><RotateCcw :size="10" aria-hidden="true" />{{ t('quota.resetCredits') }}: {{ resetCount }}</span>
				</span>
				<QuotaCostSummary v-if="!windows.length" :usage="account.quota?.cost" compact />
			</button>
		</template>
	</div>
</template>

<style scoped>
.quota-cell { width: 248px; max-width: 100%; min-width: 0; min-height: 52px; display: grid; align-content: center; gap: 4px; }
.quota-button { width: 100%; min-width: 0; padding: 4px 0; border: 0; background: transparent; color: inherit; text-align: left; cursor: pointer; }
.quota-overview { display: flex; flex-wrap: wrap; align-items: center; gap: 6px 10px; }
.quota-windows { flex: 1 0 100%; min-width: 0; }
.quota-window { display: block; }
.quota-window + .quota-window { margin-top: 7px; }
.quota-window-heading { display: inline-flex; flex-wrap: wrap; align-items: baseline; gap: 8px; min-width: 0; }
.quota-window-label { flex-shrink: 0; font-family: var(--font-mono); }
.quota-balance { display: grid; gap: 3px; max-width: 48%; color: var(--color-ink-faint); font-size: 9px; line-height: 1.4; white-space: normal; overflow-wrap: anywhere; }
.quota-balance strong { color: var(--color-ink-muted); font-family: var(--font-mono); font-size: 11px; font-weight: 650; }
.quota-meta { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 6px; margin-top: 6px; font-size: 10px; line-height: 1.4; }
.quota-credit { display: inline-flex; max-width: 100%; align-items: center; gap: 4px; color: var(--color-ink-faint); white-space: normal; overflow-wrap: anywhere; }
.quota-credit svg { flex-shrink: 0; }

.quota-line { display: flex; justify-content: space-between; gap: 8px; color: var(--color-ink-faint); font-size: 10px; line-height: 1; }
.quota-line strong { font-family: var(--font-mono); font-size: 11px; font-weight: 700; }
.quota-track { display: block; height: 4px; margin-top: 5px; overflow: hidden; border-radius: 999px; background: var(--color-surface-3); }
.quota-track i { display: block; height: 100%; border-radius: inherit; background: currentColor; }
.quota-reset, .quota-muted { color: var(--color-ink-faint); font-size: 10px; }
.quota-link { min-height: 24px; padding: 3px 4px; font-size: 11px; justify-self: start; }
.quota-success { color: var(--color-success); }
.quota-warn { color: var(--color-warn); }
.quota-danger { color: var(--color-danger); }

</style>
