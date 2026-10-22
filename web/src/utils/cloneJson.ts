/**
 * Detach JSON wire data, including nested Vue proxies, without serializing it.
 * Object undefined fields are omitted; array holes/undefined and non-finite
 * numbers normalize to null, matching the JSON wire representation.
 */
export function cloneJson<T>(value: T): T {
    const ancestors = new Set<object>()
    const result = copyValue(value)
    if (result === undefined) throw new TypeError('Expected a JSON wire value')
    return result as T

    function copyValue(input: unknown): unknown {
        if (input === null || typeof input === 'string' || typeof input === 'boolean') return input
        if (typeof input === 'number') {
            if (!Number.isFinite(input)) return null
            return input === 0 ? 0 : input // JSON normalizes negative zero.
        }
        if (typeof input === 'bigint') throw new TypeError('BigInt is not a JSON wire value')
        if (typeof input !== 'object') return undefined
        const prototype = Object.getPrototypeOf(input)
        if (!Array.isArray(input) && prototype !== null && prototype !== Object.prototype) {
            throw new TypeError('Expected a plain JSON wire object')
        }
        if (ancestors.has(input)) throw new TypeError('Circular JSON wire value')
        ancestors.add(input)
        try {
            if (Array.isArray(input)) return Array.from(input, item => copyValue(item) ?? null)
            return Object.fromEntries(Object.keys(input).flatMap(key => {
                const child = copyValue((input as Record<string, unknown>)[key])
                return child === undefined ? [] : [[key, child]]
            }))
        } finally {
            ancestors.delete(input)
        }
    }
}
