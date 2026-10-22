<script setup lang="ts">
import { Box, Network, Terminal, Radio } from 'lucide-vue-next'
import type { Overview } from '../../api/types'
import { useI18n } from '../../i18n'
import { transportDescription, transportLabel } from '../../utils/uiOptions'
import Badge from '../Badge.vue'

defineProps<{ overview: Overview | null; error: string }>()
const { t } = useI18n()
</script>

<template>
	<article class="card identity-card">
		<h2>{{ t('dashboard.upstreamIdentity') }}</h2>
		<section v-if="overview" class="identity-body">
			<header><span class="identity-product"><i><Box :size="15" /></i>Pi Gateway</span><Badge tone="info">{{ t('dashboard.configured') }}</Badge></header>
			<div class="identity-primary"><strong>{{ overview.upstream.originator || '—' }}</strong><span>{{ t('dashboard.originator') }}</span><code :title="overview.upstream.user_agent">{{ overview.upstream.user_agent || '—' }}</code></div>
			<dl class="identity-attributes"><div><dt><Radio :size="13" />{{ t('dashboard.transport') }}</dt><dd>{{ transportLabel(overview.upstream.transport) }}<p class="transport-description">{{ transportDescription(overview.upstream.transport) }}</p></dd></div><div><dt><Network :size="13" />{{ t('dashboard.pooledSockets') }}</dt><dd>{{ overview.upstream.websocket_pool }}</dd></div></dl>
			<footer><span><Terminal :size="13" />{{ t('dashboard.baseUrl') }}</span><code>{{ overview.upstream.base_url || '—' }}</code></footer>
		</section>
		<output v-else class="identity-empty" >{{ error || t('stats.loading') }}</output>
	</article>
</template>

<style scoped>
.identity-card { display: flex; flex-direction: column; min-height: 380px; padding: 24px; }
.identity-card h2 { font-size: 20px; line-height: 1.15; font-weight: 750; margin: 0 0 24px; }
.identity-body { display: grid; flex: 1; min-width: 0; align-content: space-between; gap: 18px; padding: 20px 24px; border-radius: 12px; background: var(--color-surface-2); }
.identity-body header { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 12px; }
.identity-product { display: flex; align-items: center; gap: 8px; font-size: 12px; font-weight: 700; }
.identity-product i { width: 28px; height: 28px; display: inline-flex; align-items: center; justify-content: center; border-radius: 8px; color: var(--color-ink-muted); background: var(--color-surface-3); }
.identity-primary { min-width: 0; }
.identity-primary strong { font-family: var(--font-mono); font-size: 27px; line-height: 1.05; font-weight: 750; }
.identity-primary > span { margin-left: 10px; color: var(--color-ink-faint); font-size: 10px; }
.identity-primary code { display: block; margin-top: 12px; font-family: var(--font-mono); font-size: 11px; line-height: 1.6; color: var(--color-ink-muted); overflow-wrap: anywhere; }
.identity-attributes { display: flex; flex-wrap: wrap; gap: 20px 28px; margin: 0; }
.identity-attributes dt, .identity-body footer > span { display: flex; align-items: center; gap: 6px; color: var(--color-ink-faint); font-size: 10px; font-weight: 650; }
.identity-attributes > div { min-width: 0; max-width: 100%; }
.identity-attributes dd { margin: 8px 0 0; font-size: 16px; line-height: 1.4; font-weight: 750; overflow-wrap: anywhere; }
.transport-description { max-width: 280px; margin: 6px 0 0; color: var(--color-ink-faint); font-size: 11px; line-height: 1.5; font-weight: 400; overflow-wrap: anywhere; }
.identity-body footer { display: grid; gap: 6px; }
.identity-body footer code { overflow-wrap: anywhere; color: var(--color-ink-muted); font-size: 10px; font-family: var(--font-mono); }
.identity-empty { display: grid; flex: 1; place-content: center; border-radius: 12px; background: var(--color-surface-2); color: var(--color-ink-faint); font-size: 12px; }
@media (max-width: 599px) { .identity-card { padding: 20px 16px; } .identity-body { padding: 20px; } }
</style>
