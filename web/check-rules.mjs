import assert from 'node:assert/strict'
import {spawnSync} from 'node:child_process'
import {build} from 'esbuild'
import {fileURLToPath} from 'node:url'
import {dirname, join} from 'node:path'
import {checkRuleComponents} from './check-rule-components.mjs'
import {checkRuleApi} from './check-rule-api.mjs'
import {checkRuleGraph} from './check-rule-graph.mjs'

await import('./check-editor-id.mjs')

const projectRoot = join(dirname(fileURLToPath(import.meta.url)), '..')
const exported = spawnSync('go', ['test', './internal/rules', '-run', 'TestExportCatalog', '-v'], {
    cwd: projectRoot, encoding: 'utf8', env: {...process.env, RULES_EXPORT_CATALOG: '1'},
})
if (exported.status !== 0) {
    console.error(exported.stdout + exported.stderr);
    process.exit(exported.status ?? 1)
}
const line = `${exported.stdout}\n${exported.stderr}`.split('\n').find(value => value.startsWith('RULES_CATALOG_JSON='))
if (!line) {
    console.error('RULES_CATALOG_JSON was not emitted by TestExportCatalog');
    process.exit(1)
}
const backend = JSON.parse(line.slice('RULES_CATALOG_JSON='.length))
const bundled = await build({
    entryPoints: ['src/utils/ruleSchemaAdapter.ts', 'src/utils/ruleEditor.ts'],
    bundle: true,
    write: false,
    format: 'esm',
    platform: 'node',
    outdir: 'out',
    logLevel: 'silent'
})
const modules = {}
const loaded = await Promise.all(bundled.outputFiles.map(output => import(`data:text/javascript;base64,${Buffer.from(output.text).toString('base64')}`)))
Object.assign(modules, ...loaded)
const schema = modules.adaptSchema(backend)
assert.equal(schema.rule_fields.find(f => f.name === 'source').readonly, true)
assert.equal(schema.conditions.find(c => c.id === 'eq').fields.find(f => f.name === 'source').readonly, false)
assert.throws(() => modules.adaptSchema({
    ...backend,
    actions: backend.actions.map(c => c.id === 'json_merge' ? {
        ...c,
        fields: c.fields.map(f => f.name === 'mode' ? {...f, enum: [...f.enum, 'unknown_mode']} : f)
    } : c)
}))
for (const invalid of ['9007199254740993', '9007199254740993e0', '1e309', '{"x":1,"x":2}', String.raw`{"x":1,"\u0078":2}`, '[1,]']) assert.throws(() => modules.safeParseJSON(invalid), invalid)
for (const value of [null, false, true, 0, 0.125, Number.MAX_SAFE_INTEGER, 'two\nlines', [], {}, {
    $literal: {
        $ref: {source: 'literal'},
        constructor: null
    }
}, JSON.parse('{"__proto__":{"safe":true},"":[]}')]) assert.deepEqual(modules.safeParseJSON(JSON.stringify(value)), value)
assert.throws(() => modules.adaptSchema({...backend, schema_version: 3}))
assert.throws(() => modules.adaptSchema({
    ...backend,
    actions: [...backend.actions, {id: 'future_unknown_action', fields: []}]
}))
assert.throws(() => modules.adaptSchema({
    ...backend,
    actions: backend.actions.map((cap) => cap.id!=='json_set' ? cap : {
        ...cap,
        fields: [...cap.fields, {name: 'future_unknown_parameter', type: 'string'}]
    })
}))
const missing = []
const rendererKinds = new Set(['string', 'number', 'boolean', 'enum', 'strings', 'value', 'values', 'condition', 'condition_array', 'action_array'])
const checkField = (field, path) => {
    if (!field.name) missing.push(`${path}: missing field name`)
    const renderer = field.enum?.length ? 'enum' : modules.backendRenderers[field.type]
    if (!rendererKinds.has(renderer)) missing.push(`${path}.${field.name}: missing renderer ${field.type}`)
    if (!field.description) missing.push(`${path}.${field.name}: missing bilingual help metadata`)
    if (field.enum?.some(value => typeof value !== 'string')) missing.push(`${path}.${field.name}: enum values must be strings`)
}
for (const field of backend.rule_fields) checkField(field, 'rule_fields')
for (const capability of [...backend.conditions, ...backend.actions, ...backend.value_expressions]) {
    if (!capability.label || !capability.description) missing.push(`${capability.id}: missing capability metadata`)
    for (const field of capability.fields) checkField(field, capability.id)
}
for (const field of backend.context_fields) checkField(field, 'context_fields')
assert.equal(new Set(backend.actions.map(x => x.id)).size, backend.actions.length, 'duplicate backend action IDs')
assert.equal(new Set(backend.conditions.map(x => x.id)).size, backend.conditions.length, 'duplicate backend condition IDs')
assert.equal(backend.actions.length, 22, 'backend action coverage changed; update UI intentionally')
assert.equal(backend.conditions.length, 22, 'backend condition coverage changed; update UI intentionally')
const examples = (backend.examples ?? []).map(example => {
    const normalized = {...example};
    delete normalized.id;
    delete normalized.revision;
    delete normalized.order_index;
    delete normalized.created_at;
    delete normalized.updated_at;
    delete normalized.source;
    delete normalized.legacy_name;
    return normalized
})
for (const example of examples) {
    const errors = modules.validateRule(example, schema)
    assert.deepEqual(errors, [], `${example.name}: ${JSON.stringify(errors)}`)
    const code = modules.stringifyRule(example)
    const parsed = modules.parseRule(code, schema)
    assert.deepEqual(parsed, example, `${example.name}: code → AST → code changed AST`)
    assert.deepEqual(JSON.parse(modules.stringifyRule(parsed)), JSON.parse(code), `${example.name}: normalized code differs`)
}
if (missing.length) {
    console.error(missing.join('\n'));
    process.exitCode = 1
} else {
    for (const section of ['rule_fields', 'context_fields']) {
        for (const raw of backend[section]) {
            const rendered = schema[section].find(f => f.name === raw.name);
            assert.ok(rendered, `${section}.${raw.name}`);
            assert.deepEqual(rendered.enum, raw.enum);
            assert.ok(rendered.description.en.trim() && rendered.description['zh-CN'].trim())
        }
    }
    for (const section of ['actions', 'conditions', 'value_expressions']) for (const raw of backend[section].filter(c=>!c.deprecated)) {
        const rendered = schema[section].find(c => c.id === raw.id);
        assert.ok(rendered,raw.id);
        assert.deepEqual(rendered.fields.map(f => f.name), raw.fields.map(f => f.name));
        assert.deepEqual(rendered.phases, raw.phases)
        for (const f of raw.fields) {
            const renderedField = rendered.fields.find(x => x.name === f.name);
            assert.deepEqual(renderedField.enum, f.enum);
            assert.ok(renderedField.description.en.trim() && renderedField.description['zh-CN'].trim())
        }
    }
    console.log(`PASS: backend Catalog ${backend.conditions.length} conditions, ${backend.actions.length} actions, ${backend.rule_fields.length} rule fields; renderer/help coverage and ${examples.length} AST roundtrips`)
    await checkRuleGraph(schema, backend)
    await checkRuleComponents(schema)
    await checkRuleApi()
    await import('./check-rule-simulation.mjs')
}
