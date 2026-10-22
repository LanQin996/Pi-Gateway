import {fileURLToPath, pathToFileURL} from 'node:url'
import {createRequire} from 'node:module'
import assert from 'node:assert/strict'
import {existsSync, readFileSync} from 'node:fs'
import {compileScript, parse} from '@vue/compiler-sfc'
import {createRenderer, nextTick} from 'vue'
import ts from 'typescript'

const diffDependencyURL = pathToFileURL(createRequire(import.meta.url).resolve('diff')).href

// Mount the real Vue SFCs with a minimal in-memory host. No browser, stubs of the
// trace component, network calls, generated files or new dependencies are needed.
const asModule = source => `data:text/javascript;base64,${Buffer.from(source.replace("from 'diff'", "from '" + diffDependencyURL + "'")).toString('base64')}`
const transpile = source => ts.transpileModule(source, {
    compilerOptions: {
        target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.ES2020,
    }
}).outputText
const translationsURL = asModule(transpile(readFileSync(new URL('./src/i18n/captureRulesMessages.ts', import.meta.url), 'utf8')))
const localeURL = asModule(String.raw`import {captureRulesEn as messages} from '${translationsURL}';
export const useI18n = () => ({locale: {value:'en-US'}, t: (key, params={}) => (messages[key] ?? key).replace(/\{(\w+)\}/g, (_,name) => String(params[name] ?? ''))});`)
const modules = new Map()

async function compileModule(url) {
    if (modules.has(url.href)) return modules.get(url.href)
    let source = readFileSync(url, 'utf8')
    if (url.pathname.endsWith('.vue')) {
        const {descriptor, errors} = parse(source, {filename: fileURLToPath(url)})
        assert.deepEqual(errors, [])
        source = compileScript(descriptor, {id: url.pathname, inlineTemplate: true}).content
    }
    source = transpile(source)
    const imports = [...source.matchAll(/\bfrom\s*(['"])([^'"]+)\1/g)]
    for (const match of imports) {
        const specifier = match[2]
        let replacement
        if (specifier.endsWith('/i18n')) replacement = localeURL
        else if (specifier.startsWith('.')) {
            let resolved = new URL(specifier, url)
            if (!existsSync(resolved)) resolved = new URL(`${specifier}.ts`, url)
            replacement = await compileModule(resolved)
        } else replacement = import.meta.resolve(specifier)
        source = source.replace(match[0], `from '${replacement}'`)
    }
    const compiled = asModule(source)
    modules.set(url.href, compiled)
    return compiled
}

const Panel = (await import(await compileModule(new URL('./src/components/capture/RuleTracePanel.vue', import.meta.url)))).default
const Diff = (await import(await compileModule(new URL('./src/components/capture/CaptureDiff.vue', import.meta.url)))).default
const node = (type, text = '') => ({type, text, props: {}, children: [], parent: null})
const renderer = createRenderer({
    createElement: type => node(type),
    createText: text => node('#text', text),
    createComment: text => node('#comment', text),
    setText: (element, text) => {
        element.text = text
    },
    setElementText: (element, text) => {
        element.text = text;
        element.children = []
    },
    patchProp: (element, key, _old, value) => {
        element.props[key] = value
    },
    parentNode: element => element.parent,
    nextSibling: element => element.parent?.children[element.parent.children.indexOf(element) + 1] ?? null,
    insert: (element, parent, anchor = null) => {
        if (element.parent) element.parent.children.splice(element.parent.children.indexOf(element), 1)
        const index = anchor ? parent.children.indexOf(anchor) : -1
        parent.children.splice(index < 0 ? parent.children.length : index, 0, element)
        element.parent = parent
    },
    remove: element => {
        if (element.parent) element.parent.children.splice(element.parent.children.indexOf(element), 1)
    },
})
const text = element => element.type === '#comment' ? '' : element.text + element.children.map(text).join(' ')
const all = (element, predicate) => [...(predicate(element) ? [element] : []), ...element.children.flatMap(child => all(child, predicate))]
const hasClass = name => element => String(element.props.class ?? '').split(' ').includes(name)

function mount(Component, props) {
    const root = node('root')
    const app = renderer.createApp(Component, props)
    app.mount(root)
    return {root, close: () => app.unmount()}
}

const capture = extra => ({rules_version: 7, rule_traces: [], response_frames: [], truncated: false, ...extra})
const change = {
    path: '/text',
    operation: 'replace',
    before_exists: true,
    after_exists: true,
    before: 'old',
    after: 'new'
}
const trace = extra => ({
    rule_id: 'historical-id',
    rule_name: 'historical name',
    revision: 4,
    phase: 'request',
    priority: 12,
    action_id: 'action-1',
    action_type: 'replace_text',
    action_index: 0,
    matched: true,
    status: 'changed',
    changes: [change], ...extra
})
let checks = 0

async function check(name, action) {
    await action();
    checks++;
    console.log(`PASS: ${name}`)
}

async function expandTraces(root) {
    for (const step of all(root, hasClass('trace-step'))) step.props.onToggle({target: {open: true}})
    await nextTick()
    for (const step of all(root, hasClass('trace-step'))) step.props.onToggle({target: {open: true}})
    await nextTick()
}
await check('real trace panel distinguishes legacy provenance and truncated omissions', async () => {
    const old = mount(Panel, {capture: capture({rules_version: 0})})
    assert.match(text(old.root), /Historical capture has no recorded rule sources/)
    old.close()
    const clipped = mount(Panel, {capture: capture({rules_trace_truncated: true, rules_trace_omitted: 8})})
    assert.match(text(clipped.root), /8 records omitted/)
    assert.doesNotMatch(text(clipped.root), /Historical capture/)
    clipped.close()
})
await check('real trace expansion displays historical metadata and local before/after diff', async () => {
    const mounted = mount(Panel, {capture: capture({rule_traces: [trace({})]})})
    assert.match(text(mounted.root), /historical name/)
    assert.equal(all(mounted.root, hasClass('capture-diff')).length, 0, 'closed trace must not compute/render local diffs')
    await expandTraces(mounted.root)
    const output = text(mounted.root)
    for (const expected of ['historical-id', 'Revision 4', 'Priority 12', 'Ruleset version 7', 'action-1']) assert.ok(output.includes(expected), expected)
    assert.equal(all(mounted.root, hasClass('capture-diff')).length, 1)
    mounted.close()
})
await check('no-op and rollback traces never render an actual-change diff', async () => {
    const mounted = mount(Panel, {capture: capture({rule_traces: [trace({status: 'no_change'}), trace({rolled_back: true})]})})
    await expandTraces(mounted.root)
    assert.match(text(mounted.root), /Matched without changes/)
    assert.match(text(mounted.root), /Rolled back/)
    assert.equal(all(mounted.root, hasClass('capture-diff')).length, 0)
    mounted.close()
})
await check('blocked errors and gateway processing stay distinct from user rule sources', async () => {
    const mounted = mount(Panel, {
        capture: capture({
            rule_traces: [trace({
                status: 'blocked',
                error: 'safe failure'
            }), trace({source_kind: 'gateway', rule_name: 'Pi shape repair'})]
        })
    })
    await expandTraces(mounted.root)
    assert.match(text(mounted.root), /Request blocked by rule/)
    assert.match(text(mounted.root), /safe failure/)
    assert.match(text(mounted.root), /Gateway processing: Pi shape repair/)
    assert.equal(all(mounted.root, element => element.type === 'a').length, 0, 'historical/gateway traces must not claim a current editor link')
    mounted.close()
})
await check('event associations show only real captured upstream and client frame sequences', async () => {
    const record = capture({
        rule_traces: [trace({phase: 'response_event', event_id: 'e1'}), trace({
            phase: 'response_event',
            event_id: 'dropped',
            status: 'dropped'
        })],
        response_frames: [{seq: 12, dir: 'in', rule_event_id: 'e1'}, {
            seq: 15,
            dir: 'client_out',
            rule_event_id: 'e1'
        }, {seq: 18, dir: 'in', rule_event_id: 'dropped'}]
    })
    const mounted = mount(Panel, {capture: record})
    await expandTraces(mounted.root)
    assert.match(text(mounted.root), /#12.*#15/)
    assert.match(text(mounted.root), /#18.*Not recorded/)
    mounted.close()
})
await check('trace pagination mounts more actual execution steps without losing order', async () => {
    const mounted = mount(Panel, {
        capture: capture({
            rule_traces: Array.from({length: 51}, (_, index) => trace({
                rule_id: `rule-${index}`, rule_name: `step-${index}`,
                status: 'no_change'
            }))
        })
    })
    assert.equal(all(mounted.root, hasClass('trace-step')).length, 50)
    all(mounted.root, element => element.type === 'button').find(button => text(button).includes('Show more')).props.onClick()
    await nextTick()
    assert.equal(all(mounted.root, hasClass('trace-step')).length, 51)
    assert.match(text(mounted.root), /step-50/)
    mounted.close()
})
await check('total diff displays provided sources and missing-attribution warning without inventing row provenance', async () => {
    const mounted = mount(Diff, {
        title: 'total',
        before: {available: true, value: {text: 'old'}},
        after: {available: true, value: {text: 'new'}},
        beforeLabel: 'before',
        afterLabel: 'after',
        sources: ['historical rule · Priority 4 · Ruleset version 7'],
        sourceNote: 'step-level only',
        initialOpen: true
    })
    assert.match(text(mounted.root), /Recorded change sources/)
    assert.match(text(mounted.root), /historical rule/)
    assert.match(text(mounted.root), /step-level only/)
    const rows = all(mounted.root, hasClass('diff-line'))
    assert.ok(rows.length > 0)
    assert.ok(rows.every(row => !text(row).includes('historical rule')), 'final lines must not acquire guessed rule attribution')
    mounted.close()
})
console.log(`PASS: ${checks} real capture component checks`)
