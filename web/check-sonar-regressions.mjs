import assert from 'node:assert/strict'
import {chmodSync, mkdtempSync, mkdirSync, rmSync, writeFileSync} from 'node:fs'
import {tmpdir} from 'node:os'
import {join} from 'node:path'
import {reactive} from 'vue'
import {loadTestModule} from './test-module-loader.mjs'
import {resolveGoBinary} from './go-toolchain.mjs'

const {cloneJson} = await loadTestModule('./src/utils/cloneJson.ts', {})
const original = reactive({nested: reactive({array: [1, undefined, Number.NaN], omitted: undefined}), truth: false})
const clone = cloneJson(original)
assert.deepEqual(clone, {nested: {array: [1, null, null]}, truth: false})
clone.nested.array[0] = 9
assert.equal(original.nested.array[0], 1)
const reserved = JSON.parse('{"__proto__":{"safe":true},"constructor":"data"}')
assert.deepEqual(cloneJson(reserved), reserved)
assert.equal(Object.getPrototypeOf(cloneJson(reserved)), Object.prototype)
const repeated = {x: 1}
assert.deepEqual(cloneJson([repeated, repeated]), [{x: 1}, {x: 1}])
const cyclic = {}; cyclic.self = cyclic
assert.throws(() => cloneJson(cyclic), /Circular/)
assert.throws(() => cloneJson(1n), /BigInt/)
assert.throws(() => cloneJson(new Date()), /plain JSON/)
assert.throws(() => cloneJson(undefined), /JSON wire/)
assert.ok(Object.is(cloneJson(-0), 0))
assert.deepEqual(cloneJson(new Array(2)), [null, null])
await assert.rejects(loadTestModule('../go.mod', {}), /inside web\/src/)
const descriptor = Object.getOwnPropertyDescriptor(globalThis, 'crypto')
const performanceDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'performance')
const realNow = Date.now, realRandom = Math.random
try {
    Object.defineProperty(globalThis, 'crypto', {configurable: true, value: undefined})
    Object.defineProperty(globalThis, 'performance', {configurable: true, value: {timeOrigin: 1000, now: () => 0}})
    Date.now = () => 1000
    let entropy = 0
    Math.random = () => (++entropy) / 4
    const firstRealm = await loadTestModule('./src/utils/editorId.ts', {})
    const secondRealm = await loadTestModule('./src/utils/editorId.ts', {})
    assert.notEqual(firstRealm.editorId(), secondRealm.editorId(), 'independent editor realms retain entropy even when all clocks match')
    const ids = Array.from({length: 2000}, () => firstRealm.editorId())
    assert.equal(new Set(ids).size, ids.length, 'local counter stays unique without crypto and with fixed clocks')
} finally {
    Date.now = realNow; Math.random = realRandom
    if (descriptor) Object.defineProperty(globalThis, 'crypto', descriptor)
    else delete globalThis.crypto
    if (performanceDescriptor) Object.defineProperty(globalThis, 'performance', performanceDescriptor)
    else delete globalThis.performance
}
await assert.rejects(loadTestModule('./src/utils/ruleEditor.ts', {}), /Unexpected import/)
const directory = mkdtempSync(join(tmpdir(), 'pi-toolchain-check-'))
try {
    assert.throws(() => resolveGoBinary({PI_GO_BINARY: 'relative/go'}), /absolute/)
    assert.throws(() => resolveGoBinary({PATH: '.'}), /No trusted/)
    const bin = join(directory, 'bin'); mkdirSync(bin)
    const compiler = join(bin, process.platform === 'win32' ? 'go.exe' : 'go')
    writeFileSync(compiler, 'test-only marker, never executed', {mode: 0o700})
    assert.equal(resolveGoBinary({PI_GO_BINARY: compiler}), compiler)
    if (process.platform !== 'win32') {
        chmodSync(bin, 0o777)
        assert.throws(() => resolveGoBinary({PI_GO_BINARY: compiler}), /writable/)
        assert.throws(() => resolveGoBinary({PATH: bin}), /No trusted/)
        chmodSync(bin, 0o755)
        chmodSync(directory, 0o777)
        assert.throws(() => resolveGoBinary({PI_GO_BINARY: compiler}), /ancestors.*writable/)
        assert.throws(() => resolveGoBinary({PATH: bin}), /No trusted/)
        chmodSync(directory, 0o1777)
        assert.equal(resolveGoBinary({PI_GO_BINARY: compiler}), compiler, 'trusted-owner sticky ancestor protects its trusted-owner child')

    }
} finally { rmSync(directory, {recursive: true, force: true}) }
console.log('PASS: nested reactive JSON clone, reserved keys/cycles, source-only native module loader and absolute trusted Go resolution')
