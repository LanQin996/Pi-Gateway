import assert from 'node:assert/strict'
import {readFileSync} from 'node:fs'
import ts from 'typescript'
import * as vue from 'vue'
import {compileScript, parse} from '@vue/compiler-sfc'
import {renderToString} from '@vue/server-renderer'

const read = path => readFileSync(new URL(path, import.meta.url), 'utf8')

function load(source, imports) {
    const compiled = ts.transpileModule(source, {
        compilerOptions: {
            module: ts.ModuleKind.CommonJS,
            target: ts.ScriptTarget.ES2022
        }
    })
    const exports = {}
    new Function('exports', 'require', compiled.outputText)(exports, name => {
        assert.ok(Object.hasOwn(imports, name), `Unexpected import: ${name}`)
        return imports[name]
    })
    return exports
}

const locale = vue.ref('en')
const translations = load(read('./src/components/accounts/accountControlsLocale.ts'), {
    '../../i18n': {useI18n: () => ({locale})},
})
const inertVue = {
    ...vue, watch: () => {
    }, onBeforeUnmount: () => {
    }
}
const catalog = load(read('./src/components/accounts/useAccountModelCatalog.ts'), {
    vue: inertVue,
    '../../api/client': {
        ApiError: class extends Error {
        }, api: new Proxy({}, {
            get: () => () => {
                throw new Error('Unexpected network access')
            }
        })
    },
    './accountControlsLocale': translations,
})
const {modelCapabilities, manualCatalogModel, modelMetadataJSON, modelMetadataPreview, useAccountModelCatalog} = catalog
let checked = 0

async function check(name, run) {
    try {
        await run()
        checked++
    } catch (error) {
        throw new Error(`Model catalog check failed: ${name}`, {cause: error})
    }
}

const account = id => ({id, name: `Account ${id}`, email: ''})

function models(entries, ids = entries.map((_, i) => i + 1)) {
    const state = useAccountModelCatalog(vue.ref(ids.map(account)))
    state.catalogs.value = Object.fromEntries(entries.map((model, i) => [i + 1, {
        models: [model],
        fetched_at: 0,
        attempted_at: 0,
        error: ''
    }]))
    return state.models.value
}

const manual = manualCatalogModel('same')
const real = {
    id: 'same',
    name: 'Real',
    source: 'upstream',
    metadata: {supports_reasoning_summary_parameter: false, supported_reasoning_levels: [], unknown: {keep: true}}
}
await check('manual defaults contain seven levels but no fabricated context/modalities', () => {
    assert.deepEqual(manual.metadata.supported_reasoning_levels.map(x => x.effort), ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'])
    assert.deepEqual(Object.keys(manual.metadata).sort((a, b) => a.localeCompare(b)), ['default_reasoning_level', 'default_reasoning_summary', 'supported_reasoning_levels', 'supports_reasoning_summary_parameter'])
    assert.equal(manual.metadata.default_reasoning_level, 'medium')
    assert.equal(manual.metadata.default_reasoning_summary, 'auto')
    assert.equal(manual.source, 'manual')
})
await check('real upstream replaces manual defaults in either order', () => {
    for (const entries of [[manual, real], [real, manual]]) {
        for (const ids of [[1, 2], [2, 1]]) {
            const merged = models(entries, ids)
            assert.equal(merged.length, 1)
            assert.equal(merged[0].name, 'Real')
            assert.deepEqual(merged[0].metadata, real.metadata)
            assert.deepEqual(merged[0].sources, ['Account 1', 'Account 2'])
        }
    }
})
await check('legacy upstream without metadata never inherits manual defaults', () => {
    for (const upstream of [{id: 'same', name: 'Legacy'}, {
        id: 'same',
        name: 'Empty',
        source: 'upstream',
        metadata: {}
    }]) {
        for (const entries of [[manual, upstream], [upstream, manual]]) assert.deepEqual(models(entries)[0].metadata, upstream.metadata)
    }
})
await check('same-source uses lowest account id and fills only absent top-level keys', () => {
    for (const source of ['manual', 'upstream', undefined]) {
        const primary = {
            id: 'same',
            name: 'Primary',
            source,
            metadata: {nullable: null, bool: false, list: [], obj: {primary: true}}
        }
        const secondary = {
            id: 'same',
            name: 'Secondary',
            source,
            metadata: {nullable: 1, bool: true, list: ['max'], obj: {secondary: true}, extra: ['text']}
        }
        const before = JSON.stringify([primary, secondary])
        for (const ids of [[1, 2], [2, 1]]) {
            const merged = models([primary, secondary], ids)[0]
            assert.equal(merged.name, 'Primary')
            assert.deepEqual(merged.metadata, {...primary.metadata, extra: ['text']})
        }
        assert.equal(JSON.stringify([primary, secondary]), before)
    }
})
await check('absent metadata is filled from same source without manual pollution', () => {
    const empty = {id: 'same', name: 'Primary', source: 'upstream'}
    assert.deepEqual(models([empty, real])[0].metadata, real.metadata)
    assert.deepEqual(models([manual, empty, real])[0].metadata, real.metadata)
})
await check('summary preserves false/empty arrays and rejects invalid or unsafe values', () => {
    for (const value of [undefined, null, [], 'bad', 3, true]) assert.deepEqual(modelCapabilities(value), [])
    assert.deepEqual(modelCapabilities({
        supports_reasoning_summary_parameter: false,
        input_modalities: [],
        context_window: 0
    }), [
        {key: 'supports_reasoning_summary_parameter', value: 'false'}, {
            key: 'context_window',
            value: '0'
        }, {key: 'input_modalities', value: '[]'},
    ])
    assert.deepEqual(modelCapabilities({
        context_window: Number.MAX_SAFE_INTEGER + 1,
        max_output_tokens: -1,
        supports_reasoning_summary_parameter: 'false',
        default_reasoning_level: {},
        default_reasoning_summary: null,
        input_modalities: [null, {}, false]
    }), [])
    assert.deepEqual(modelCapabilities(Object.create({default_reasoning_level: 'hidden'})), [])
})
await check('summary accepts new reasoning values and bounds arrays/strings', () => {
    assert.deepEqual(modelCapabilities({supported_reasoning_levels: [{effort: 'future-level'}, 'max', null, {effort: {}}]}), [{
        key: 'supported_reasoning_levels',
        value: 'future-level, max'
    }])
    const long = modelCapabilities({
        default_reasoning_level: 'a'.repeat(10000),
        supported_reasoning_levels: new Array(1000).fill('max')
    })
    assert.equal(long[0].value.split(', ').length, 17)
    assert.equal(long[1].value.length, 161)
})
await check('full JSON preserves unknown fields, anomalous types, null, false and empty arrays', () => {
    const metadata = {
        unknown: {n: null, b: false, a: []},
        default_reasoning_level: {unexpected: true},
        supported_reasoning_levels: false
    }
    assert.deepEqual(JSON.parse(modelMetadataJSON(metadata)), metadata)
    assert.equal(modelMetadataPreview(metadata).text, modelMetadataJSON(metadata))
    assert.equal(modelMetadataPreview(metadata).truncated, false)
    assert.equal(modelMetadataPreview(metadata).failed, false)
    const cyclic = {};
    cyclic.self = cyclic
    assert.equal(modelMetadataPreview(cyclic).failed, true)
})
await check('preview truncation is explicit and full local JSON remains available', () => {
    const metadata = {unknown: 'a'.repeat(30000)}
    const preview = modelMetadataPreview(metadata)
    assert.equal(preview.text.length, 20000)
    assert.equal(preview.truncated, true)
    assert.deepEqual(JSON.parse(modelMetadataJSON(metadata)), metadata)
})
const selectorText = read('./src/components/accounts/ModelRestrictionSelector.vue')
const {descriptor} = parse(selectorText)
const componentSource = compileScript(descriptor, {id: 'catalog-regression', inlineTemplate: true}).content
let fixtures = []
const sourceStatusDescriptor = parse(read('./src/components/accounts/ModelCatalogSourceStatus.vue')).descriptor
const sourceStatus = load(compileScript(sourceStatusDescriptor, {id: 'catalog-source-status-regression', inlineTemplate: true}).content, {
    vue,
    '../../stores/ui': {formatTime: String},
    './accountControlsLocale': translations,
    './useAccountModelCatalog': catalog,
})
const Selector = load(componentSource, {
    './ModelCatalogSourceStatus.vue': sourceStatus,
    vue,
    'lucide-vue-next': Object.fromEntries(['RefreshCw', 'Search', 'ShieldBan'].map(name => [name, {render: () => vue.h('svg')}])),
    '../../stores/ui': {formatTime: String},
    './accountControlsLocale': translations,
    './useAccountModelCatalog': {
        ...catalog,
        useAccountModelCatalog: sources => ({...useAccountModelCatalog(sources), models: vue.ref(fixtures)})
    },
}).default
const render = props => renderToString(vue.h(Selector, {sources: [], modelValue: [], ...props}))
await check('real selector escapes metadata markup and renders unknown fields', async () => {
    fixtures = [{
        id: 'danger',
        name: 'Danger',
        sources: [],
        metadata: {unknown: '<img src=x onerror=alert(1)>', nested: {value: false}}
    }]
    const html = await render()
    assert.ok(html.includes('<details'))
    assert.ok(html.includes('Complete model metadata'))
    assert.ok(html.includes('&lt;img'))
    assert.ok(!html.includes('<img'))
    assert.ok(html.includes('&quot;unknown&quot;'))
    assert.ok(!selectorText.includes('v-html'))
})
await check('unsaved manual models show unverified gateway defaults in both languages', async () => {
    fixtures = []
    for (const lang of ['en', 'zh-CN']) {
        locale.value = lang
        const html = await render({supplementalModels: ['unsaved']})
        assert.ok(html.includes(lang === 'en' ? 'gateway reasoning defaults' : '网关默认值'))
        for (const value of ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'auto']) assert.ok(html.includes(value))
    }
    locale.value = 'en'
})
await check('large metadata shows truncation/download, old catalogs and restrictions still render', async () => {
    fixtures = [{id: 'large', name: 'Large', sources: [], metadata: {unknown: 'a'.repeat(30000)}}, {
        id: 'old',
        name: 'Old',
        sources: []
    }]
    const html = await render({modelValue: ['old'], inherited: [{model: 'large', group_names: ['Group']}]})
    assert.ok(html.includes('Preview truncated'))
    assert.ok(html.includes('Download complete metadata'))
    assert.equal((html.match(/<details/g) ?? []).length, 1)
    assert.equal((html.match(/type="checkbox" checked/g) ?? []).length, 2)
    assert.ok(!html.includes('a'.repeat(20001)))
    assert.ok(selectorText.includes('URL.createObjectURL'))
    assert.ok(!selectorText.includes('fetch('))
})
await check('settings expose seven reasoning options', () => {
    const settings = read('./src/views/SettingsView.vue')
    for (const effort of ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max']) assert.ok(settings.includes(`<option value="${effort}">${effort}</option>`))
})
console.log(`PASS: ${checked} model catalog checks (merge precedence, metadata safety/details, manual defaults, SSR, settings)`)
