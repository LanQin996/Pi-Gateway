import assert from 'node:assert/strict'
import {readdir, readFile} from 'node:fs/promises'
import {dirname, extname, join, relative} from 'node:path'
import {fileURLToPath} from 'node:url'
import {build} from 'esbuild'
import ts from 'typescript'
import {parse as parseSFC} from '@vue/compiler-sfc'
import {baseParse} from '@vue/compiler-dom'

const root = join(dirname(fileURLToPath(import.meta.url)), 'src')
const cataloguePath = join(root, 'i18n/messages.ts')
const source = await readFile(cataloguePath, 'utf8')
const bundled = await build({
    stdin: {contents: `${source}\nexport { en, zh }\n`, loader: 'ts', resolveDir: dirname(cataloguePath)},
    bundle: true, write: false, platform: 'node', format: 'esm', logLevel: 'silent',
})
const {
    en,
    zh,
    translate
} = await import(`data:text/javascript;base64,${Buffer.from(bundled.outputFiles[0].text).toString('base64')}`)
const keys = Object.keys(en)
const failures = []
const placeholders = value => [...String(value).matchAll(/\{(\w+)\}/g)].map(match => match[1]).sort((a, b) => a.localeCompare(b))
for (const key of keys) {
    if (!Object.hasOwn(zh, key)) failures.push(`${key}: missing zh-CN entry`)
    else {
        if (!String(zh[key]).trim() || !String(en[key]).trim()) failures.push(`${key}: empty translation`)
        try {
            assert.deepEqual(placeholders(zh[key]), placeholders(en[key]))
        } catch {
            failures.push(`${key}: translation placeholders differ`)
        }
    }
}
for (const key of Object.keys(zh)) if (!Object.hasOwn(en, key)) failures.push(`${key}: missing English entry`)

function verifyKey(key, location) {
    if (!Object.hasOwn(en, key) || !Object.hasOwn(zh, key)) failures.push(`${location}: missing ${key}`)
}

function inspectCode(code, location) {
    const tree = ts.createSourceFile(location + '.ts', code, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS)

    function inspectArgument(node) {
        if (ts.isConditionalExpression(node)) {
            inspectArgument(node.whenTrue);
            inspectArgument(node.whenFalse);
            return
        }
        if (ts.isTemplateExpression(node) || ts.isCallExpression(node) || ts.isElementAccessExpression(node)) return
        if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) verifyKey(node.text, location)
        else ts.forEachChild(node, inspectArgument)
    }

    function visit(node) {
        if (ts.isCallExpression(node) && ts.isIdentifier(node.expression) && ['t', 'translateNow'].includes(node.expression.text) && node.arguments[0]) {
            // Conditional t(flag ? 'a' : 'b') is checked in both branches. A computed
            // template key is covered by the explicit finite option matrix below.
            if (!ts.isTemplateExpression(node.arguments[0])) inspectArgument(node.arguments[0])
        }
        ts.forEachChild(node, visit)
    }

    visit(tree)
}

function inspectTemplate(node, location) {
    if (node.type === 5) inspectCode(`(${node.content.content})`, location)
    for (const prop of node.props ?? []) if (prop.type === 7 && prop.exp) inspectCode(`(${prop.exp.content})`, location)
    for (const child of node.children ?? []) inspectTemplate(child, location)
}

async function inspect(dir) {
    const entries = await readdir(dir, {withFileTypes: true})
    await Promise.all(entries.map(entry => inspectEntry(dir, entry)))
}

async function inspectEntry(dir, entry) {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) return inspect(path)
    if (!['.ts', '.vue'].includes(extname(path)) || path.startsWith(join(root, 'i18n'))) return
    const text = await readFile(path, 'utf8')
    const location = relative(root, path)
    if (extname(path) !== '.vue') return inspectCode(text, location)
    const {descriptor} = parseSFC(text)
    for (const script of [descriptor.script, descriptor.scriptSetup]) if (script) inspectCode(script.content, location)
    if (descriptor.template) inspectTemplate(baseParse(descriptor.template.content), location)
}

await inspect(root)
const dynamic = {
    transport: ['passthrough', 'sse', 'websocket', 'websocket-cached', 'auto'],
    strategy: ['smart', 'quota_reset_priority', 'round_robin', 'sticky', 'least_inflight', 'single'],
}
for (const [kind, values] of Object.entries(dynamic)) {
    for (const value of values) for (const part of ['label', 'description']) verifyKey(`ui.${kind}.${value}.${part}`, 'uiOptions dynamic values')
}
for (const value of ['ready', 'expired', 'invalid', 'banned', 'quota_exhausted', 'unknown']) verifyKey(`ui.accountStatus.${value}`, 'account status options')
for (const key of ['accounts.disable', 'accounts.enable']) {
    verifyKey(key, 'account toggle actions')
    assert.notEqual(translate('zh-CN', key), key)
}
if (failures.length) {
    console.error([...new Set(failures)].join('\n'))
    process.exitCode = 1
} else console.log(`PASS: ${keys.length} bilingual keys, placeholders, conditional calls and dynamic option names`)
