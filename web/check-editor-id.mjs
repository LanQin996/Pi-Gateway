import assert from 'node:assert/strict'
import {webcrypto} from 'node:crypto'
import {loadTestModule} from './test-module-loader.mjs'

const descriptor = Object.getOwnPropertyDescriptor(globalThis, 'crypto')
const unavailableMessage = /Secure random values are unavailable.*crypto\.getRandomValues/
try {
    Object.defineProperty(globalThis, 'crypto', {configurable: true, get() { throw new Error('crypto access blocked') }})
    const unavailableRealm = await loadTestModule('./src/utils/editorId.ts', {})
    assert.throws(() => unavailableRealm.editorId(), unavailableMessage, 'blocked crypto must fail on demand, not module import')
    for (const unavailable of [undefined, {}, {getRandomValues() { throw new Error('entropy source blocked') }}]) {
        Object.defineProperty(globalThis, 'crypto', {configurable: true, value: unavailable})
        assert.throws(() => unavailableRealm.editorId(), unavailableMessage, 'never replace unavailable crypto with weak randomness')
    }
    let calls = 0
    const httpCrypto = {getRandomValues(bytes) {
        assert.equal(this, httpCrypto, 'preserve the WebCrypto receiver')
        calls++
        return webcrypto.getRandomValues(bytes)
    }}
    Object.defineProperty(globalThis, 'crypto', {configurable: true, value: httpCrypto})
    const firstRealm = await loadTestModule('./src/utils/editorId.ts', {})
    const secondRealm = await loadTestModule('./src/utils/editorId.ts', {})
    assert.equal(calls, 0, 'module loading must not request entropy')
    assert.equal(httpCrypto.randomUUID, undefined, 'ordinary HTTP need not expose randomUUID')
    const ids = Array.from({length: 10000}, (_, index) => (index % 2 ? firstRealm : secondRealm).editorId())
    assert.equal(calls, ids.length, 'each ID must use fresh cryptographic entropy')
    for (const id of ids) assert.match(id, /^[\da-f]{8}-[\da-f]{4}-4[\da-f]{3}-[89ab][\da-f]{3}-[\da-f]{12}$/)
    assert.equal(new Set(ids).size, ids.length, 'independent editor modules must not collide in this sample')
    const cryptoWithBothMethods = {
        getRandomValues(bytes) { bytes.fill(255); return bytes },
        randomUUID() { assert.fail('editor IDs must use getRandomValues only') },
    }
    Object.defineProperty(globalThis, 'crypto', {configurable: true, value: cryptoWithBothMethods})
    assert.equal(firstRealm.editorId(), 'ffffffff-ffff-4fff-bfff-ffffffffffff', 'set UUIDv4 version and variant bits')
    cryptoWithBothMethods.getRandomValues = bytes => { bytes.fill(0); return bytes }
    assert.equal(firstRealm.editorId(), '00000000-0000-4000-8000-000000000000', 'set version and variant without relying on input bits')
} finally {
    if (descriptor) Object.defineProperty(globalThis, 'crypto', descriptor)
    else delete globalThis.crypto
}
console.log('PASS: getRandomValues-only editor IDs, HTTP without randomUUID, deferred crypto errors, UUIDv4 bits and 10000 distinct IDs')
