import assert from 'node:assert/strict'
import {mkdtemp, readFile, realpath, rm, writeFile} from 'node:fs/promises'
import {tmpdir} from 'node:os'
import {extname, join, relative, isAbsolute} from 'node:path'
import {fileURLToPath, pathToFileURL} from 'node:url'
import {build} from 'esbuild'
import {compileScript, parse} from '@vue/compiler-sfc'

const sourceRoot = await realpath(fileURLToPath(new URL('./src/', import.meta.url)))
const dependencies = new Map()
let sequence = 0
export function testDependency(id, name) {
    const imports = dependencies.get(id)
    assert.ok(imports && Object.hasOwn(imports, name), `Unexpected import: ${name}`)
    return imports[name]
}

/** Native ESM loader for repository source only; never accepts executable source strings. */
export async function loadTestModule(path, imports) {
    const entry = await realpath(fileURLToPath(new URL(path, import.meta.url)))
    const inside = relative(sourceRoot, entry)
    assert.ok(inside && !inside.startsWith('..') && !isAbsolute(inside), 'Test entry must be inside web/src')
    assert.ok(['.ts', '.vue'].includes(extname(entry)), 'Only local TypeScript/Vue source is supported')
    const id = ++sequence
    const directory = await mkdtemp(join(tmpdir(), 'pi-source-check-'))
    dependencies.set(id, imports)
    try {
        const bundled = await build({
            entryPoints: [entry], bundle: true, write: false, format: 'esm', platform: 'node', logLevel: 'silent',
            plugins: [{name: 'explicit-test-dependencies', setup(build) {
                build.onResolve({filter: /.*/}, args => {
                    if (args.kind === 'entry-point') return {path: entry}
                    if (args.path === import.meta.url) return {path: args.path, external: true}
                    assert.ok(Object.hasOwn(imports, args.path), `Unexpected import: ${args.path}`)
                    return {path: args.path, namespace: 'test-dependency'}
                })
                build.onLoad({filter: /.*/, namespace: 'test-dependency'}, args => {
                    const names = Object.keys(imports[args.path]).filter(name => name !== 'default')
                    return {contents: `import {testDependency} from ${JSON.stringify(import.meta.url)};\nconst dependency = testDependency(${id}, ${JSON.stringify(args.path)});\nexport default dependency.default;\n` + names.map((name, index) => `const value${index} = dependency[${JSON.stringify(name)}]; export {value${index} as ${JSON.stringify(name)}};`).join('\n'), loader: 'js'}
                })
                build.onLoad({filter: /\.vue$/}, async args => {
                    const {descriptor, errors} = parse(await readFile(args.path, 'utf8'), {filename: args.path})
                    assert.deepEqual(errors, [])
                    return {contents: compileScript(descriptor, {id: inside, inlineTemplate: true}).content, loader: 'ts'}
                })
            }}],
        })
        const module = join(directory, 'entry.mjs')
        await writeFile(module, bundled.outputFiles[0].text, {mode: 0o600})
        return await import(pathToFileURL(module).href)
    } finally {
        dependencies.delete(id)
        await rm(directory, {recursive: true, force: true})
    }
}
