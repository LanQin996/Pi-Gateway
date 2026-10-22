import {resolveGoBinary} from '../go-toolchain.mjs'
import {spawnSync} from 'node:child_process'
import {mkdtempSync, rmSync, writeFileSync} from 'node:fs'
import {tmpdir} from 'node:os'
import {join} from 'node:path'
import {fileURLToPath} from 'node:url'

export default function setup() {
    const exported = spawnSync(resolveGoBinary(), ['test', './internal/rules', '-run', '^TestExportCatalog$', '-count=1', '-v'], {
        cwd: fileURLToPath(new URL('../../', import.meta.url)),
        encoding: 'utf8',
        timeout: 120_000,
        maxBuffer: 4 * 1024 * 1024,
        env: {...process.env, RULES_EXPORT_CATALOG: '1'},
    })
    if (exported.error || exported.status !== 0) {
        throw new Error(`Cannot export the real Go rule catalog: ${exported.error ?? ''}\n${exported.stdout}\n${exported.stderr}`)
    }
    const line = exported.stdout.split(/\r?\n/).find(value => value.startsWith('RULES_CATALOG_JSON='))
    if (!line) throw new Error('TestExportCatalog did not emit RULES_CATALOG_JSON')
    const catalog = JSON.parse(line.slice('RULES_CATALOG_JSON='.length))
    const directory = mkdtempSync(join(tmpdir(), 'pi-gateway-rules-e2e-'))
    const path = join(directory, 'catalog.json')
    writeFileSync(path, JSON.stringify(catalog))
    process.env.RULES_E2E_CATALOG = path
    const defaultsLine = exported.stdout.split(/\r?\n/).find(value => value.startsWith('RULES_DEFAULTS_JSON='))
    if (!defaultsLine) throw new Error('TestExportCatalog did not emit RULES_DEFAULTS_JSON')
    const defaultsPath = join(directory, 'defaults.json')
    writeFileSync(defaultsPath, JSON.stringify(JSON.parse(defaultsLine.slice('RULES_DEFAULTS_JSON='.length))))
    process.env.RULES_E2E_DEFAULTS = defaultsPath
    return () => rmSync(directory, {recursive: true, force: true})
}
