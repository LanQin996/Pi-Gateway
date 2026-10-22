import {pathToFileURL} from 'node:url'
import {createRequire} from 'node:module'
import assert from 'node:assert/strict'
import {readFileSync} from 'node:fs'
import ts from 'typescript'

const diffDependencyURL = pathToFileURL(createRequire(import.meta.url).resolve('diff')).href

function compile(name) {
    return ts.transpileModule(readFileSync(new URL(`./src/utils/${name}.ts`, import.meta.url), 'utf8'), {
        compilerOptions: {target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.ES2020},
    }).outputText
}

const moduleURL = source => `data:text/javascript;base64,${Buffer.from(source.replace("from 'diff'", "from '" + diffDependencyURL + "'")).toString('base64')}`
const diffURL = moduleURL(compile('captureDiff'))
const {
    frameSnapshot,
    requestSnapshots,
    responseHeaderSnapshots,
    captureRows,
    hopProtocol,
    streamSnapshot,
    payloadPreview,
    ruleSources,
    ruleTraces,
    ruleChangeSnapshot,
    lastRuleWriters,
    groupCaptureRows,
    groupRuleTraces,
} = await import(
    moduleURL(compile('captureInspection').replace("from './captureDiff'", `from '${diffURL}'`)),
    )
const headers = [{name: 'Content-Type', value: 'application/json'}]
const repeated = Array.from({length: 2000}, (_, i) => ({
    key: String(i),
    frame: {
        seq: i,
        dir: 'in',
        kind: 'sse_event',
        type: 'response.output_text.delta',
        data: {item_id: 'same', delta: String(i)}
    }
}))
assert.equal(groupCaptureRows(repeated).length, 1)
assert.equal(groupCaptureRows(repeated)[0].rows.length, 2000)
assert.equal(groupCaptureRows([...repeated, {
    key: 'done',
    frame: {seq: 2000, dir: 'in', kind: 'sse_event', type: 'response.completed'}
}, ...repeated]).length, 3)
assert.equal(groupCaptureRows([repeated[0], {
    key: 'other',
    frame: {...repeated[1].frame, data: {item_id: 'different'}}
}]).length, 2)
const groupedRules = groupRuleTraces({
    rule_traces: Array.from({length: 2000}, (_, i) => ({
        rule_id: 'r',
        phase: 'response_event',
        action_id: 'a',
        event_id: String(i),
        event_type: 'response.output_text.delta',
        status: 'changed',
        changes: [{path: `/input/${i}/text`, operation: 'replace'}]
    }))
})
assert.equal(groupedRules.length, 1)
assert.equal(groupedRules[0].steps.length, 1)
assert.equal(groupedRules[0].steps[0].count, 2000)
const varyingChanges = groupRuleTraces({
    rule_traces: [1, 20, 1000].map(count => ({
        rule_id: 'r',
        action_id: 'a',
        status: 'changed',
        changes: Array.from({length: count}, (_, i) => ({path: `/input/${i}/text`, operation: 'replace'}))
    }))
})
assert.equal(varyingChanges[0].steps.length, 1)
assert.equal(varyingChanges[0].steps[0].count, 3)
assert.equal(varyingChanges[0].changes, 1021)
assert.equal(groupRuleTraces({
    rule_traces: [{rule_id: 'r', action_index: 0}, {
        rule_id: 'r',
        action_index: 1
    }]
})[0].steps.length, 2)
assert.equal(groupCaptureRows([repeated[0], {key: 'out', frame: {...repeated[1].frame, dir: 'client_out'}}]).length, 2)
assert.equal(groupCaptureRows([repeated[0], {key: 'ws', frame: {...repeated[1].frame, kind: 'ws_frame'}}]).length, 2)
assert.equal(groupCaptureRows([{key: 'error-1', frame: {...repeated[0].frame, type: 'error'}}, {
    key: 'error-2',
    frame: {...repeated[1].frame, type: 'error'}
}]).length, 2)
assert.equal(groupRuleTraces({
    rule_traces: [{rule_id: 'r', source_kind: 'gateway'}, {
        rule_id: 'r',
        source_kind: 'rule'
    }]
}).length, 2)
const event = {type: 'response.completed', response: {id: 'response-local', output: []}}
const frame = (seq, dir, kind, data, type = '') => ({seq, dir, kind, type, data, at_ms: seq, bytes: 20})
const capture = (frames = [], extra = {}) => ({
    client_transport: 'sse', upstream_transport: 'sse', request_body: '', response_text: '',
    request_headers: [], response_headers: [], response_frames: frames, truncated: false, status: 200, ...extra,
})
let checked = 0

function check(name, fn) {
    fn();
    checked++;
    console.log(`PASS: ${name}`)
}

check('false/null/empty payloads remain recorded; absent payloads do not', () => {
    for (const data of [null, false, 0, '', {}, []]) assert.deepEqual(frameSnapshot({data}), {
        available: true,
        value: data
    })
    assert.deepEqual(frameSnapshot(), {available: false})
    assert.deepEqual(frameSnapshot({}), {available: false})
    assert.deepEqual(frameSnapshot({text: ''}), {available: true, value: ''})
})
check('old records never substitute the upstream request for the client request', () => {
    const requests = requestSnapshots(capture([], {
        request_body: '{"input":"upstream only"}',
        request_headers: headers
    }))
    assert.equal(requests.beforeBody.available, false)
    assert.equal(requests.beforeHeaders.available, false)
    assert.equal(requests.afterBody.available, true)
    assert.equal(requests.afterHeaders.available, true)
})
check('original and gateway request headers and payloads keep their own directions', () => {
    const original = {input: 'hello'}
    const requests = requestSnapshots(capture([
        frame(0, 'client_in', 'handshake_request', [{name: 'User-Agent', value: 'curl/8.8.0'}]),
        frame(1, 'client_in', 'request_body', original),
    ], {request_body: '{"input":"hello","stream":true}', request_headers: [{name: 'User-Agent', value: 'pi'}]}))
    assert.deepEqual(requests.beforeBody.value, original)
    assert.match(requests.afterBody.value, /stream/)
    assert.equal(requests.beforeHeaders.value[0].value, 'curl/8.8.0')
    assert.equal(requests.afterHeaders.value[0].value, 'pi')
})
check('WS uses the original response.create envelope and merges only its exact outgoing copy', () => {
    const body = {input: 'hello'}
    const envelope = {type: 'response.create', ...body}
    const record = capture([
        frame(0, 'client_in', 'request_body', body),
        frame(1, 'client_in', 'ws_frame', envelope, 'response.create'),
        frame(2, 'out', 'ws_frame', envelope, 'response.create'),
        frame(3, 'out', 'ws_frame', {...envelope, input: 'retry'}, 'response.create'),
    ], {request_body: JSON.stringify(envelope), client_transport: 'ws', upstream_transport: 'websocket-cached'})
    assert.deepEqual(requestSnapshots(record).beforeBody.value, envelope)
    assert.deepEqual(captureRows(record, true).map(row => row.frame.seq), [3])
    assert.equal(captureRows(record, false).length, 4)
    assert.equal(hopProtocol(record, 'client'), 'ws')
})
check('response headers/status are not borrowed from the upstream when client headers were not captured', () => {
    const old = capture([], {response_headers: headers, status: 403})
    assert.equal(responseHeaderSnapshots(old).before.available, true)
    assert.equal(responseHeaderSnapshots(old).after.available, false)
    assert.equal(responseHeaderSnapshots(old).status, undefined)
    const next = capture([frame(1, 'client_out', 'handshake_response', {status: 502})], old)
    next.response_frames = [frame(1, 'client_out', 'handshake_response', {status: 502})]
    assert.equal(responseHeaderSnapshots(next).status, 502)
    assert.equal(responseHeaderSnapshots(next).after.available, false)
    next.response_frames.push(frame(2, 'client_out', 'handshake_response', {status: 200, headers: []}))
    assert.deepEqual(responseHeaderSnapshots(next).after, {available: true, value: []})
})
check('unchanged response events merge once across the client header record', () => {
    const record = capture([
        frame(0, 'in', 'sse_event', event, event.type),
        frame(1, 'client_out', 'handshake_response', {status: 200, headers}),
        frame(2, 'client_out', 'sse_event', event, event.type),
    ])
    const rows = captureRows(record, true)
    assert.equal(rows.length, 1)
    assert.equal(rows[0].before.dir, 'in')
    assert.equal(rows[0].after.dir, 'client_out')
    assert.equal(captureRows(record, false).length, 3)
})
check('HTTP rejections pair the upstream error with the actual HTTP or WS client error', () => {
    for (const kind of ['http_response', 'ws_frame']) {
        const record = capture([
            frame(0, 'in', 'http_error_body', {upstream_only: true}),
            frame(1, 'client_out', kind, {error: {message: 'gateway error'}}, 'error'),
        ], {status: 403})
        const rows = captureRows(record, true)
        assert.equal(rows.length, 1)
        assert.deepEqual(rows[0].before.data, {upstream_only: true})
        assert.equal(rows[0].after.data.error.message, 'gateway error')
        assert.equal(streamSnapshot(record, 'upstream').text, '')
        assert.equal(streamSnapshot(record, 'client').text, '')
    }
})
check('changed response events and aggregated terminal response can be compared', () => {
    const before = frame(0, 'in', 'sse_event', event, event.type)
    const modified = frame(1, 'client_out', 'sse_event', {...event, injected: true}, event.type)
    assert.equal(captureRows(capture([before, modified]), true).length, 1)
    const aggregate = frame(1, 'client_out', 'http_response', event.response, 'response')
    const rows = captureRows(capture([before, aggregate]), true)
    assert.equal(rows.length, 1)
    assert.deepEqual(rows[0].after.data, event.response)
})
check('unrelated, incomplete and truncated frames are never silently merged', () => {
    const before = frame(0, 'in', 'sse_event', event, event.type)
    const after = frame(2, 'client_out', 'sse_event', event, event.type)
    assert.equal(captureRows(capture([before, after], {truncated: true}), true).length, 2)
    assert.equal(captureRows(capture([before, frame(1, 'out', 'ws_frame', {retry: true}), after]), true).length, 3)
    assert.equal(captureRows(capture([before, frame(1, 'client_out', 'http_response', {unrelated: true}, 'other')]), true).length, 2)
    assert.equal(captureRows(capture([before, frame(1, 'client_out', 'sse_event', undefined, event.type)]), true).length, 2)
})
check('old SSE text belongs to the client only; upstream SSE is explicitly reconstructed', () => {
    const record = capture([frame(0, 'in', 'sse_event', event, event.type)], {response_text: 'data: client-only\n\n'})
    const before = streamSnapshot(record, 'upstream'), after = streamSnapshot(record, 'client')
    assert.equal(before.reconstructed, true)
    assert.match(before.text, /response.completed/)
    assert.doesNotMatch(before.text, /client-only/)
    assert.equal(after.reconstructed, false)
    assert.equal(after.text, record.response_text)
})
check('WS to SSE uses independent per-hop protocols and response payloads', () => {
    const record = capture([
        frame(0, 'in', 'ws_frame', event, event.type),
        frame(1, 'client_out', 'sse_event', event, event.type),
    ], {upstream_transport: 'websocket-cached'})
    assert.equal(hopProtocol(record, 'upstream'), 'ws')
    assert.equal(hopProtocol(record, 'client'), 'sse')
    assert.equal(streamSnapshot(record, 'upstream').text, '')
    assert.equal(streamSnapshot(record, 'upstream').socket.length, 1)
    assert.match(streamSnapshot(record, 'client').text, /data: /)
    assert.equal(captureRows(record, true).length, 1)
})
check('stream:false is HTTP even without a saved response', () => {
    const record = capture([frame(0, 'client_in', 'request_body', {stream: false})])
    assert.equal(hopProtocol(record, 'client'), 'http')
    assert.equal(streamSnapshot(record, 'client').text, '')
})
check('large SSE payloads retain their full recorded text', () => {
    const text = 'data: ' + 'x'.repeat(250_000)
    const client = streamSnapshot(capture([], {response_text: text}), 'client')
    assert.equal(client.text, text)
    assert.equal(client.limited, false)
    const upstream = streamSnapshot(capture([frame(0, 'in', 'sse_event', {text})]), 'upstream')
    assert.ok(upstream.text.includes(text))
    assert.equal(upstream.limited, false)
})
check('unknown future records remain visible and preserve order', () => {
    const record = capture([frame(3, 'in', '__proto__', 'unknown'), frame(4, 'in', 'future_event', 'visible')])
    assert.deepEqual(captureRows(record, true).map(row => row.frame.seq), [3, 4])
})
check('individual payload previews preserve large and small JSON', () => {
    assert.deepEqual(payloadPreview({ok: true}), {text: '{\n  "ok": true\n}', limited: false})
    assert.deepEqual(payloadPreview('x'.repeat(100_001)), {text: 'x'.repeat(100_001), limited: false})
    assert.deepEqual(payloadPreview(''), {text: '', limited: false})
})
check('binary WebSocket frames still identify the protocol', () => {
    const record = capture([frame(0, 'in', 'ws_binary', 'bytes')], {upstream_transport: ''})
    assert.equal(hopProtocol(record, 'upstream'), 'ws')
    assert.equal(streamSnapshot(record, 'upstream').socket.length, 1)
})
check('explicit event IDs pair interleaved and truncated records without guessing', () => {
    const first = {...frame(0, 'in', 'sse_event', {delta: 'first'}, 'delta'), rule_event_id: 'first'}
    const second = {...frame(1, 'in', 'sse_event', {delta: 'second'}, 'delta'), rule_event_id: 'second'}
    const out = {...frame(2, 'client_out', 'http_response', {result: 'changed'}, 'response'), rule_event_id: 'first'}
    const rows = captureRows(capture([first, second, out], {truncated: true}), true)
    assert.equal(rows.length, 2)
    assert.equal(rows[0].before.rule_event_id, 'first')
    assert.equal(rows[0].after.rule_event_id, 'first')
    assert.equal(rows[1].frame.rule_event_id, 'second')
})
check('different, one-sided, and duplicated event IDs never fall back to content matching', () => {
    const before = {...frame(0, 'in', 'sse_event', event, event.type), rule_event_id: 'before'}
    const after = {...frame(1, 'client_out', 'sse_event', event, event.type), rule_event_id: 'after'}
    assert.equal(captureRows(capture([before, after]), true).length, 2)
    assert.equal(captureRows(capture([before, {...after, rule_event_id: undefined}]), true).length, 2)
    assert.equal(captureRows(capture([before, {...before, seq: 1}, {
        ...after,
        seq: 2,
        rule_event_id: 'before'
    }]), true).length, 3)
    assert.equal(captureRows(capture([before, {...after, rule_event_id: 'before'}, {
        ...after,
        seq: 2,
        rule_event_id: 'before'
    }]), true).length, 3)
})
check('dropped events remain visible and never manufacture a client delivery', () => {
    const dropped = {...frame(0, 'in', 'sse_event', {delta: 'drop'}, 'delta'), rule_event_id: 'drop'}
    const next = {...frame(1, 'in', 'sse_event', {delta: 'next'}, 'delta'), rule_event_id: 'next'}
    const delivered = {...frame(2, 'client_out', 'sse_event', {delta: 'next'}, 'delta'), rule_event_id: 'next'}
    const rows = captureRows(capture([dropped, next, delivered]), true)
    assert.equal(rows.length, 2)
    assert.equal(rows[0].frame.rule_event_id, 'drop')
    assert.equal(rows[1].before.rule_event_id, 'next')
})
check('rule sources preserve historical identity and exclude no-op, failed, and rolled-back actions', () => {
    const record = capture([], {
        rules_version: 17, rule_traces: [
            {rule_id: 'old', rule_name: 'old name', phase: 'request', status: 'changed'},
            {rule_id: 'noop', phase: 'request', matched: true, status: 'no_change'},
            {rule_id: 'failed', phase: 'request', status: 'error'},
            {rule_id: 'rolled', phase: 'request', status: 'changed', rolled_back: true},
            {rule_id: 'response', phase: 'response_event', status: 'changed', event_id: 'second'},
            {rule_id: 'body', phase: 'response_body', status: 'changed', event_id: 'second'},
            {rule_id: 'gateway:normalize', source_kind: 'gateway', phase: 'request', status: 'changed'},
        ]
    })
    assert.deepEqual(ruleSources(record, 'request').map(trace => trace.rule_id), ['old', 'gateway:normalize'])
    assert.equal(ruleSources(record, 'request')[0].rule_name, 'old name')
    assert.deepEqual(ruleSources(record, undefined, 'second').map(trace => trace.rule_id), ['response', 'body'])
    assert.deepEqual(ruleSources(record), [])
    assert.deepEqual(ruleSources(record, 'response_event', 'first'), [])
    assert.deepEqual(ruleTraces(capture()), [])
    assert.deepEqual(ruleSources(capture(), 'request'), [])
})
check('change snapshots distinguish absent, null, false and unrecorded values', () => {
    assert.deepEqual(ruleChangeSnapshot({before_exists: false, before: null}, 'before'), {
        available: true,
        value: {exists: false}
    })
    assert.deepEqual(ruleChangeSnapshot({before_exists: true, before: null}, 'before'), {
        available: true,
        value: {exists: true, value: null}
    })
    assert.deepEqual(ruleChangeSnapshot({after_exists: true, after: false}, 'after'), {
        available: true,
        value: {exists: true, value: false}
    })
    assert.deepEqual(ruleChangeSnapshot({before_exists: true}, 'before'), {available: false})
    assert.deepEqual(ruleChangeSnapshot({}, 'before'), {available: false})
})
check('last writer follows execution order and refuses positional or incomplete attribution', () => {
    const change = path => ({path, operation: 'replace', before_exists: true, after_exists: true, before: 1, after: 2})
    const trace = (path, extra = {}) => ({phase: 'request', status: 'changed', changes: [change(path)], ...extra})
    const record = capture([], {rule_traces: [trace('/same'), trace('/same'), trace('/array/0')]})
    assert.deepEqual([...lastRuleWriters(record)], ['1:0'])
    assert.deepEqual([...lastRuleWriters({...record, rules_trace_truncated: true})], [])
    assert.deepEqual([...lastRuleWriters({...record, truncated: true})], [])
    assert.deepEqual([...lastRuleWriters(capture([], {rule_traces: [trace('/same'), trace('/same', {omitted_changes: 1})]}))], [])
    assert.deepEqual([...lastRuleWriters(capture([], {rule_traces: [trace('/parent/child'), trace('/parent')]}))], ['1:0'])
    assert.deepEqual([...lastRuleWriters(capture([], {rule_traces: [trace('/parent'), trace('/parent/child')]}))], ['1:0'])
    assert.deepEqual([...lastRuleWriters(capture([], {rule_traces: [trace('/x', {event_id: 'a'}), trace('/x', {event_id: 'b'})]}))], ['1:0', '0:0'])
})
console.log(`PASS: ${checked} capture inspection checks`)
await import('./check-capture-rule-traces.mjs')
