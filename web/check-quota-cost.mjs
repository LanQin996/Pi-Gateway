import {loadTestModule} from './test-module-loader.mjs'
import assert from 'node:assert/strict'
import * as vue from 'vue'
import {renderToString} from '@vue/server-renderer'
import {build} from 'esbuild'
import {fileURLToPath} from 'node:url'


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
const i18n = {
    translateNow: t,
    currentLocale: () => locale,
    useI18n: () => ({t}),
    formatDateTime: value => `time:${value}`
}
const ui = await loadTestModule('./src/stores/ui.ts', {
    pinia: {
        defineStore: () => {
        }
    }, vue, '../i18n': i18n
})
const money = await loadTestModule('./src/api/stats-format.ts', {'../i18n': i18n})
const component = (await loadTestModule('./src/components/QuotaCostSummary.vue', {vue, '../api/stats-format': money, '../i18n': i18n, '../stores/ui': ui})).default
const render = props => renderToString(vue.createSSRApp({render: () => vue.h(component, props)}))
const usage = {cost_micros: 10000000, priced_requests: 2, unpriced_requests: 0}
const windowCost = {
    limit_id: 'codex',
    role: 'secondary',
    start_at: 1,
    as_of: 200,
    usage,
    estimated_total_usd: 40,
    estimated_remaining_usd: 30
}

for (const lang of ['en', 'zh-CN']) {
    locale = lang
    assert.equal(ui.formatUntil(3 * 86400000 + 7 * 3600000), '3d 7h')
    assert.equal(ui.formatUntil(2 * 86400000), '2d 0h')
    assert.equal(ui.formatUntil(86400000), '1d 0h')
    assert.equal(ui.formatUntil(3600000 + 2 * 60000), '1h 2m')
    assert.equal(ui.formatUntil(1), '1m')
    assert.equal(ui.formatUntil(0), t('common.expired'))
    assert.equal(ui.formatUntil(-1), t('common.expired'))
    assert.equal(ui.formatUntil(Number.NaN), '—')
    const html = await render({usage, windowCost})
    for (const amount of [10, 40, 30]) assert.ok(html.includes(money.money(amount)), html)
    for (const key of ['quota.windowCost', 'quota.estimatedTotal', 'quota.estimatedRemaining', 'quota.costHint', 'quota.costEstimateHint']) assert.ok(html.includes(t(key)), key)
    assert.ok(html.includes('time:200'))
    const partial = {...usage, unpriced_requests: 2}
    const partialHTML = await render({
        usage: partial,
        windowCost: {...windowCost, usage: partial},
        compact: true,
        cycleLabel: '7d'
    })
    const visibleText = textContent(partialHTML)
    for (const key of ['quota.usedShort', 'quota.estimateShort']) assert.ok(visibleText.includes(t(key)))
    assert.ok(visibleText.includes(money.money(10)))
    assert.ok(visibleText.includes(money.money(40)))
    assert.ok(!visibleText.includes(t('quota.unpricedRequests', {count: 2})))
    assert.ok(!visibleText.includes(t('quota.costEstimateHint')))
    assert.ok(partialHTML.includes(t('quota.unpricedRequests', {count: 2})))
    assert.ok(partialHTML.includes('cost-used') && partialHTML.includes('cost-estimate'))
    const missing = {...usage, cost_micros: 0, priced_requests: 0, unpriced_requests: 2}
    const zero = await render({
        usage: missing,
        windowCost: {...windowCost, usage: missing, estimated_total_usd: 0, estimated_remaining_usd: 0}
    })
    assert.ok(zero.includes(money.money(0)))
    assert.ok(!zero.includes('—'))
    const unavailable = await render({
        usage: null,
        windowCost: {
            ...windowCost,
            usage: null,
            estimated_total_usd: null,
            estimated_remaining_usd: null,
            unavailable_reason: 'missing_snapshot'
        }
    })
    assert.ok(unavailable.includes('—'))
    assert.ok(unavailable.includes(t('quota.costMissingSnapshot')))
    const precision = await render({usage: {...usage, cost_micros: 1234567}})
    assert.ok(textContent(precision).includes(money.money(1.23)))
    assert.ok(precision.includes(money.money(1.234567)))

}
console.log('PASS: quota USD cost/estimate rendering in both languages, partial/missing/zero costs and day/hour countdown boundaries')

function textContent(html) {
    let result = '', start = 0
    while (start < html.length) {
        const open = html.indexOf('<', start)
        if (open === -1) return result + html.slice(start)
        const close = html.indexOf('>', open + 1)
        if (close === -1) return result + html.slice(start)
        result += html.slice(start, open)
        start = close + 1
    }
    return result
}
