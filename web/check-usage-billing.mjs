import {loadTestModule} from './test-module-loader.mjs'
import assert from 'node:assert/strict'
import {fileURLToPath} from 'node:url'
import {build} from 'esbuild'
import * as vue from 'vue'
import {renderToString} from '@vue/server-renderer'


const bundle = await build({
    entryPoints: [fileURLToPath(new URL('./src/i18n/messages.ts', import.meta.url))],
    bundle: true,
    write: false,
    platform: 'node',
    format: 'esm',
    logLevel: 'silent'
})
const messages = await import(`data:text/javascript;base64,${Buffer.from(bundle.outputFiles[0].text).toString('base64')}`)
let locale = 'en'
const t = (key, params) => messages.translate(locale, key, params)
const i18n = {currentLocale: () => locale, useI18n: () => ({t})}
const money = await loadTestModule('./src/api/stats-format.ts', {'../i18n': i18n})

const badges = (await loadTestModule('./src/components/usage/UsageRequestBadges.vue', {vue, '../../i18n': i18n})).default
const billing = (await loadTestModule('./src/components/usage/UsageBillingDetails.vue', {vue, '../../i18n': i18n, '../../api/stats-format': money})).default
const render = (component, record) => renderToString(vue.createSSRApp({render: () => vue.h(component, {record})}))
const row = {
    reasoning_effort: 'xhigh', requested_service_tier: 'priority', service_tier: 'priority', cost_usd: 2.1,
    billing_details: {
        version: 'model-pricing-v2', tier: 'fast', tier_source: 'upstream', price_source: 'builtin',
        long_context: true, context_threshold: 272000,
        base_rates: {input: 2, cached_input: 0.2, cache_write: 2.5, output: 10},
        context_rates: {input: 4, cached_input: 0.4, cache_write: 5, output: 15},
        effective_rates: {input: 8, cached_input: 0.8, cache_write: 10, output: 30},
        context_multipliers: {input: 2, cached_input: 2, cache_write: 2, output: 1.5},
        tier_multipliers: {input: 2, cached_input: 2, cache_write: 2, output: 2},
        base_cost_micros: 600000, context_cost_micros: 1050000, total_cost_micros: 2100000,
    },
}
for (const lang of ['en', 'zh-CN']) {
    locale = lang
    for (const [index, effort] of ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'].entries()) {
        const html = await render(badges, {...row, reasoning_effort: effort})
        assert.ok(html.includes(`reasoning-level-${index}`), html)
        assert.ok(html.includes('fast-badge'))
    }
    for (const tier of ['', 'default', 'standard', 'flex', 'auto']) {
        assert.ok(!(await render(badges, {...row, service_tier: tier})).includes('fast-badge'), tier)
    }
    const html = await render(billing, row)
    for (const key of ['usage.originalCost', 'usage.contextMultipliers', 'usage.fastRates', 'usage.totalCost']) assert.ok(html.includes(t(key)))
    for (const cost of [0.6, 1.05, 2.1]) assert.ok(html.includes(money.money(cost)))
    assert.ok(html.includes('×1.5'))
    assert.ok(html.includes('priority'))
    const historical = await render(billing, {...row, billing_details: null})
    assert.ok(historical.includes(t('usage.historicalBilling')))
    assert.ok(!historical.includes('×2'))
    const unconfirmed = await render(billing, {
        ...row,
        service_tier: '',
        billing_details: {...row.billing_details, tier_source: 'default'}
    })
    assert.ok(unconfirmed.includes(t('usage.tierFallbackHint')))
}
console.log('PASS: reasoning badge level colors, actual Fast-only badges, stored billing rates/costs, unconfirmed tiers and historical records in both languages')
