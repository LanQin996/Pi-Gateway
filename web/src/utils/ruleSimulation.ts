import type {Capture, CaptureFrame} from '../api/types'
import type {CaptureSimulationSample} from '../api/ruleSimulation'

const own = (value: object, key: string) => Object.hasOwn(value, key)

/** This is a selection list, not a claim that a truncated frame can be replayed. */
export function recordedRuleEvents(capture: Capture): CaptureFrame[] {
    return (capture.response_frames ?? []).filter(frame => frame.dir === 'in'
        && (frame.kind === 'sse_event' || frame.kind === 'ws_frame'))
        .sort((left, right) => left.seq - right.seq)
}

export function preferredRuleEvent(frames: CaptureFrame[], requested?: number): number | undefined {
    if (requested !== undefined && frames.some(frame => frame.seq === requested)) return requested
    return frames.find(frame => frame.type === 'response.output_text.delta')?.seq ?? frames[0]?.seq
}

export function positiveCaptureID(value: unknown): number | undefined {
    if (typeof value !== 'string' && typeof value !== 'number') return
    if (!/^\d+$/.test(String(value))) return
    const id = Number(value)
    return Number.isSafeInteger(id) && id > 0 ? id : undefined
}

export function frameSequence(value: unknown): number | undefined {
    if (typeof value !== 'string' && typeof value !== 'number') return
    if (!/^\d+$/.test(String(value))) return
    const seq = Number(value)
    return Number.isSafeInteger(seq) && seq >= 0 ? seq : undefined
}

export function simulationSnapshots(sample: CaptureSimulationSample) {
    return {
        before: {available: own(sample, 'before'), value: sample.before},
        after: {available: own(sample.result, 'body'), value: sample.result.body},
    }
}

export interface SimulationChange {
    path: string
    operation: 'add' | 'remove' | 'replace'
    beforeExists: boolean
    afterExists: boolean
    before?: unknown
    after?: unknown
}

/** Bounded structural comparison of final values; never infer final changes from intermediate traces. */
export function simulationChanges(before: unknown, after: unknown, maximum = 200) {
    const changes: SimulationChange[] = []
    let visited = 0
    let limited = false
    const part = (key: string) => key.replaceAll('~', '~0').replaceAll('/', '~1')
    const walk = (left: unknown, right: unknown, path: string, beforeExists: boolean, afterExists: boolean, depth: number) => {
        if (++visited > 10_000 || depth > 64 || changes.length >= maximum) {
            limited = true;
            return
        }
        if (beforeExists === afterExists && Object.is(left, right)) return
        if (beforeExists && afterExists && left !== null && right !== null && typeof left === 'object' && typeof right === 'object' && Array.isArray(left) === Array.isArray(right)) {
            const a = left as Record<string, unknown>, b = right as Record<string, unknown>
            const keys = new Set([...Object.keys(a), ...Object.keys(b)])
            for (const key of keys) {
                walk(a[key], b[key], `${path}/${part(key)}`, own(a, key), own(b, key), depth + 1)
                if (limited) break
            }
            return
        }
        let operation: SimulationChange['operation'] = 'replace'
        if (!beforeExists) operation = 'add'
        else if (!afterExists) operation = 'remove'
        changes.push({
            path,
            operation,
            beforeExists,
            afterExists,
            before: left,
            after: right
        })
    }
    walk(before, after, '', true, true, 0)
    return {changes, limited}
}

/** A small, non-recursive summary; large container payloads are not serialized for every row. */
export function samplePreview(value: unknown, maximum = 130): string {
    if (typeof value === 'string') return value.slice(0, maximum) + (value.length > maximum ? '…' : '')
    if (value === null) return 'null'
    if (typeof value === 'number' || typeof value === 'boolean') return String(value)
    if (Array.isArray(value)) return `[${value.length}]`
    if (value && typeof value === 'object') {
        const object = value as Record<string, unknown>
        for (const key of ['delta', 'text', 'model', 'message']) {
            if (typeof object[key] === 'string') return samplePreview(object[key], maximum)
        }
        return `{ ${Object.keys(object).slice(0, 6).join(', ')}${Object.keys(object).length > 6 ? ', …' : ''} }`
    }
    return ''
}
