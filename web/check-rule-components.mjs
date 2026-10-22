// Real Vue SFC interaction tests with Vue's custom renderer; no browser or new dependency needed.
import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import Module, {createRequire} from 'node:module'
import {dirname, join} from 'node:path'
import {fileURLToPath} from 'node:url'
import {build} from 'esbuild'
import {compileScript, parse} from '@vue/compiler-sfc'

const root = dirname(fileURLToPath(import.meta.url))
const require = createRequire(import.meta.url)
const {createRenderer, h, shallowRef, nextTick} = require('vue')
const {createPinia} = require('pinia')

export async function checkRuleComponents(schema) {
    globalThis.document = {documentElement: {lang: 'en'}, createElement: () => ({})}
    globalThis.localStorage = {
        getItem: () => 'en', setItem() {
        }, removeItem() {
        }
    }
    const bundle = await build({
        stdin: {
            contents: `export {default as JsonValueEditor} from './src/components/rules/JsonValueEditor.vue'; export {default as ValueEditor} from './src/components/rules/ValueEditor.vue'; export {default as RuleField} from './src/components/rules/RuleField.vue'; export {default as ConditionEditor} from './src/components/rules/ConditionEditor.vue'; export {default as ActionEditor} from './src/components/rules/ActionEditor.vue'; export {default as RuleEditor} from './src/components/rules/RuleEditor.vue'; export * from './src/utils/ruleEditor'; export * from './src/utils/ruleSchema';`,
            resolveDir: root,
            loader: 'ts'
        },
        bundle: true, write: false, format: 'cjs', platform: 'node', external: ['vue', 'pinia'], logLevel: 'silent',
        plugins: [{
            name: 'sfc', setup(plugin) {
                plugin.onLoad({filter: /\.vue$/}, async ({path}) => {
                    // Vue Flow and capture loading require real browser DOM/router; tested in Playwright.
                    // This renderer still exercises the real RuleEditor and every schema form control.
                    if (/[/\\](RuleCanvas|CaptureRuleDebugger)\.vue$/.test(path)) return {
                        contents: 'export default {render(){return null}}',
                        loader: 'js'
                    }
                    const {descriptor} = parse(await readFile(path, 'utf8'), {filename: path})
                    const compiled = compileScript(descriptor, {
                        id: path,
                        inlineTemplate: true,
                        templateOptions: {compilerOptions: {hoistStatic: false, cacheHandlers: false}}
                    })
                    return {contents: compiled.content, loader: 'ts', resolveDir: dirname(path)}
                })
            }
        }],
    })
    const compiled = new Module(join(root, 'virtual-rule-components.cjs'))
    compiled.filename = join(root, 'virtual-rule-components.cjs')
    compiled.paths = Module._nodeModulePaths(root)
    compiled._compile(bundle.outputFiles[0].text, compiled.filename)
    const components = compiled.exports
    const node = (type, text = '') => ({
        type,
        text,
        props: {},
        children: [],
        parent: null,
        value: '',
        checked: false,
        validityMessage: '',
        setCustomValidity(message) {
            this.validityMessage = message
        },
        reportValidity() {
            return !this.validityMessage
        },
        addEventListener() {
        },
        removeEventListener() {
        },
        setAttribute() {
        },
        removeAttribute() {
        }
    })
    const renderer = createRenderer({
        createElement: type => node(type),
        createText: text => node('#text', text),
        createComment: text => node('#comment', text),
        setText: (n, text) => {
            n.text = text
        },
        setElementText: (n, text) => {
            n.text = text;
            n.children = []
        },
        parentNode: n => n.parent,
        nextSibling: n => n.parent?.children[n.parent.children.indexOf(n) + 1] ?? null,
        insert: (n, parent, anchor) => {
            if (n.parent) n.parent.children.splice(n.parent.children.indexOf(n), 1);
            n.parent = parent;
            const i = anchor ? parent.children.indexOf(anchor) : -1;
            if (i < 0) parent.children.push(n); else parent.children.splice(i, 0, n)
        },
        remove: n => {
            if (n.parent) n.parent.children.splice(n.parent.children.indexOf(n), 1);
            n.parent = null
        },
        patchProp: (n, key, old, value) => {
            n.props[key] = value;
            if (key === 'value') n.value = value
        },
    })
    const walk = n => [n, ...n.children.flatMap(walk)]
    const text = n => n.text + n.children.map(text).join('')
    const control = (root, predicate) => {
        const found = walk(root).find(node => predicate(node));
        assert.ok(found, 'Expected rendered control');
        return found
    }
    const clickLabel = async (root, label) => fire(control(root, n => n.type === 'button' && text(n).trim() === label), 'onClick')

    async function fire(n, key, value) {
        assert.ok(!n.props.disabled && !n.props.readonly, `Control must be editable: ${n.type}`);
        assert.ok(n.props[key], `Missing ${key} on ${n.type}`);
        const target = Object.assign(n, {value: value ?? n.value});
        const event = {
            target, currentTarget: target, preventDefault() {
            }, stopPropagation() {
            }
        };
        const callbacks = Array.isArray(n.props[key]) ? n.props[key] : [n.props[key]];
        for (const fn of callbacks) await fn(event);
        await nextTick()
    }

    function mount(component, value, props = {}) {
        const state = shallowRef(value), host = node('root')
        const app = renderer.createApp({
            setup: () => () => h(component, {
                ...props,
                modelValue: state.value,
                'onUpdate:modelValue': next => {
                    state.value = next
                }
            })
        })
        app.use(createPinia());
        app.mount(host)
        return {host, state, app, unmount: () => app.unmount()}
    }

    // Independent wire-format oracle; do not reuse production cloneJson here.
    const normalized = value => JSON.parse(JSON.stringify(value))
    let fieldsEdited = 0, enumsEdited = 0, capabilitiesRendered = 0, roundtrips = 0

    async function editField(field) {
        if (field.readonly) return
        const value = components.defaultForField(field)
        const mounted = mount(components.RuleField, value, {field, schema, path: '/field'})
        const {host, state} = mounted
        if (field.enum?.length) {
            const select = control(host, n => n.type === 'select')
            assert.deepEqual(walk(select).filter(n => n.type === 'option').map(n => n.props.value), field.enum)
            for (const option of field.enum) {
                await fire(select, 'onChange', option);
                assert.equal(state.value, option);
                enumsEdited++
            }
        } else if (field.type === 'string') {
            await fire(control(host, n => n.type === 'textarea'), 'onInput', 'edited / ~\nline');
            assert.equal(state.value, 'edited / ~\nline')
        } else if (field.type === 'number') {
            const number = field.min ?? 7;
            await fire(control(host, n => n.type === 'input'), 'onInput', String(number));
            assert.equal(state.value, number)
        } else if (field.type === 'boolean') {
            const input = control(host, n => n.type === 'input');
            input.checked = !value;
            await fire(input, 'onChange');
            assert.equal(state.value, !value)
        } else if (field.type === 'strings') {
            await clickLabel(host, 'Add item');
            const inputs = walk(host).filter(n => n.type === 'input');
            await fire(inputs.at(-1), 'onInput', '/nested/0');
            assert.equal(state.value.at(-1), '/nested/0')
        } else if (field.type === 'value') {
            await fire(control(host, n => n.type === 'select' && n.props['aria-label'] === 'JSON type'), 'onChange', 'boolean');
            const input = control(host, n => n.type === 'input');
            input.checked = true;
            await fire(input, 'onChange');
            assert.equal(state.value, true)
        } else if (field.type === 'values') {
            await clickLabel(host, 'Add item');
            assert.equal(state.value.at(-1), null);
            await fire(control(host, n => n.type === 'select' && n.props['aria-label'] === 'JSON type'), 'onChange', 'string');
            await fire(control(host, n => n.type === 'textarea'), 'onInput', 'two\nlines');
            assert.equal(state.value.at(-1), 'two\nlines')
        } else if (field.type === 'condition') {
            await fire(control(host, n => n.type === 'select'), 'onChange', 'not');
            assert.deepEqual(normalized(state.value), {op: 'not', conditions: [{op: 'always'}]})
        } else if (field.type === 'action_array') {
            await fire(control(host, n => n.props.id?.startsWith('add-rule-action')), 'onChange', 'json_set');
            assert.equal(state.value.at(-1).type, 'json_set')
        } else if (field.type === 'condition_array') {
            await clickLabel(host, 'Add item');
            assert.equal(state.value.at(-1).op, 'always')
        } else assert.fail(`No real component test for renderer ${field.type}`)
        fieldsEdited++;
        mounted.unmount()
        if (!field.required) {
            const optional = mount(components.RuleField, undefined, {field, schema, path: '/optional'});
            await clickLabel(optional.host, 'Set optional field');
            assert.notEqual(optional.state.value, undefined);
            await clickLabel(optional.host, 'Use default / omit field');
            assert.equal(optional.state.value, undefined);
            optional.unmount()
        }
    }

    for (const field of schema.rule_fields) if (!['when', 'actions'].includes(field.name)) await editField(field)
    await checkCapabilities()

    async function checkCapabilities() {
        for (const cap of schema.actions) {
            const action = components.newAction(cap.id, schema)
            const mounted = mount(components.ActionEditor, [action], {schema, phase: cap.phases[0]})
            for (const button of walk(mounted.host).filter(n => n.type === 'button' && text(n).startsWith('Options ·'))) await fire(button, 'onClick')
            for (const field of cap.fields) assert.ok(walk(mounted.host).some(n => n.props['data-rule-path'] === `/actions/0/params/${field.name}`), `${cap.id}.${field.name}: not rendered`)
            mounted.unmount();
            capabilitiesRendered++
            for (const field of cap.fields) await editField(field)
        }
        await checkConditions()
    }

    async function checkConditions() {
        for (const cap of schema.conditions) {
            const condition = components.defaultCondition(cap.id, schema)
            const mounted = mount(components.ConditionEditor, condition, {schema})
            const options = walk(control(mounted.host, n => n.type === 'select')).filter(n => n.type === 'option').map(n => n.props.value)
            assert.deepEqual(options, schema.conditions.map(c => c.id))
            if (['all', 'any', 'not'].includes(cap.id)) assert.ok(walk(mounted.host).some(n => n.props['data-rule-path'] === '/when/conditions/0'))
            else for (const field of cap.fields) assert.ok(walk(mounted.host).some(n => n.props['data-rule-path'] === `/when/${field.name}`))
            mounted.unmount();
            capabilitiesRendered++
            for (const field of cap.fields) await editField(field)
        }
    }
    // Create nested object/array/scalar values using only actual component controls.
    const json = mount(components.JsonValueEditor, null)
    await fire(control(json.host, n => n.type === 'select'), 'onChange', 'object')
    await clickLabel(json.host, 'Add property')
    await fire(control(json.host, n => n.type === 'textarea' && n.props['aria-label'] === 'Property name'), 'onChange', '')
    let selects = walk(json.host).filter(n => n.type === 'select')
    await fire(selects[1], 'onChange', 'array')
    await clickLabel(json.host, 'Add item')
    selects = walk(json.host).filter(n => n.type === 'select')
    await fire(selects[2], 'onChange', 'number')
    await fire(control(json.host, n => n.type === 'input' && n.props.type === 'number'), 'onInput', '42')
    assert.deepEqual(normalized(json.state.value), {'': [42]})
    const numeric = control(json.host, n => n.type === 'input' && n.props.type === 'number')
    await fire(numeric, 'onInput', '9007199254740993');
    assert.equal(numeric.reportValidity(), false);
    assert.deepEqual(normalized(json.state.value), {'': [42]})
    json.unmount()
    // Reference union plus all source/encoding enum branches; literal reserved keys are retained.
    const value = mount(components.ValueEditor, null, {schema})
    await fire(control(value.host, n => n.type === 'select'), 'onChange', 'reference')
    for (const source of schema.sources) {
        selects = walk(value.host).filter(n => n.type === 'select');
        await fire(selects[1], 'onChange', source);
        assert.equal(value.state.value.$ref.source, source)
    }
    await fire(control(value.host, n => n.type === 'textarea'), 'onInput', '/a~1b/~0')
    for (const encoding of schema.encodings) {
        selects = walk(value.host).filter(n => n.type === 'select');
        await fire(selects[2], 'onChange', encoding);
        assert.equal(value.state.value.$ref.encoding, encoding)
    }
    await fire(control(value.host, n => n.type === 'select'), 'onChange', 'escape')
    assert.ok(Object.hasOwn(value.state.value, '$literal'));
    value.unmount()
    const expression = mount(components.ValueEditor, {
        $expr: {
            op: 'concat', args: [
                {$ref: {source: 'current', path: '/model'}},
                {$expr: {op: 'concat', args: ['suffix']}}
            ]
        }
    }, {schema, idPrefix: 'nested-expression'})
    const expressionIDs = walk(expression.host).map(n => n.props.id).filter(Boolean)
    assert.equal(new Set(expressionIDs).size, expressionIDs.length, 'nested expressions must have unique control IDs')
    assert.ok(walk(expression.host).some(n => n.props['data-testid'] === 'selected-option-help' && text(n).includes('concat(')), 'selected operator must show argument order and example')
    expression.unmount()
    // Type change is one atomic update; stable IDs and ordering survive.
    const actions = mount(components.ActionEditor, [components.newAction('json_set', schema), components.newAction('json_remove', schema)], {
        schema,
        phase: 'request'
    })
    const firstID = actions.state.value[0].id
    await fire(control(actions.host, n => n.type === 'select'), 'onChange', 'text_replace')
    assert.equal(actions.state.value[0].type, 'text_replace');
    assert.equal(actions.state.value[0].id, firstID);
    assert.ok(Object.hasOwn(actions.state.value[0].params, 'pattern'));
    assert.ok(!Object.hasOwn(actions.state.value[0].params, 'value'))
    await fire(control(actions.host, n => n.type === 'button' && n.props.title === 'Move down'), 'onClick')
    assert.equal(actions.state.value[1].id, firstID);
    actions.unmount()
    await checkRoundtrips()

    async function checkRoundtrips() {
        // Mount the complete editor and exercise its code/visual mode handlers, not just JSON.parse.
        const fixtures = [...(schema.examples ?? [])]
        for (const cap of schema.actions) {
            const action = components.newAction(cap.id, schema)
            if (cap.id === 'rewrite_model') action.params.model = 'test-model'
            if (cap.id === 'text_replace') action.params.pattern = 'hello'
            if (cap.id === 'json_merge') action.params.value = {a: [null, true, 'x']}
            fixtures.push({...components.newRule(), name: cap.id, phase: cap.phases[0], actions: [action]})
        }
        for (const fixture of fixtures) {
            const state = components.createDraft(fixture)
            const editor = mount(components.RuleEditor, state, {schema})
            await clickLabel(editor.host, 'JSON code')
            assert.equal(editor.state.value.mode, 'code')
            assert.deepEqual(components.safeParseJSON(editor.state.value.code), normalized(fixture))
            await clickLabel(editor.host, 'Visual editor')
            assert.equal(editor.state.value.mode, 'visual', JSON.stringify(editor.state.value.errors))
            assert.deepEqual(normalized(editor.state.value.rule), normalized(fixture))
            const changedDescription = `graph edit ${roundtrips}\nmultiline`
            // Canvas fields are covered by real browser tests; this custom renderer stubs Vue Flow.
            await clickLabel(editor.host, 'Steps')
            if (fixture.description === undefined) {
                const description = control(editor.host, n => n.props['data-rule-path'] === '/description')
                await clickLabel(description, 'Set optional field')
            }
            await fire(control(editor.host, n => n.type === 'textarea' && n.props.id === 'rule-field-/description'), 'onInput', changedDescription)
            await clickLabel(editor.host, 'JSON code')
            assert.deepEqual(components.safeParseJSON(editor.state.value.code), normalized({
                ...fixture,
                description: changedDescription
            }));
            roundtrips++
            if (roundtrips === 1) {
                const before = normalized(editor.state.value.rule);
                await fire(control(editor.host, n => n.type === 'textarea' && n.props['aria-label'] === 'JSON code'), 'onInput', '{broken');
                await clickLabel(editor.host, 'Visual editor');
                assert.equal(editor.state.value.mode, 'code');
                assert.equal(editor.state.value.code, '{broken');
                assert.deepEqual(normalized(editor.state.value.rule), before)
            }
            editor.unmount()
        }
    }
    console.log(`PASS: real Vue controls: ${capabilitiesRendered} capabilities, ${fieldsEdited} field edits, ${enumsEdited} enum options, recursive JSON/reference creation, ${roundtrips} code→visual→code roundtrips and invalid-code retention`)
}
