// Editor-only identity, never use for credentials or server-managed rule IDs.
let counter = 0
// Prefer secure entropy for the per-realm namespace as well as individual IDs.
// Legacy contexts without crypto still need local editor IDs; their fallback
// namespace keeps a non-security random salt across realms with coarsened clocks.
const session = editorSession()

function editorSession(): string {
    try {
        const bytes = globalThis.crypto.getRandomValues(new Uint32Array(4))
        return Array.from(bytes, value => value.toString(36)).join('-')
    } catch {
        const origin = globalThis.performance?.timeOrigin ?? Date.now()
        const salt = Math.random().toString(36).slice(2) || 'session'
        return `${salt}-${origin.toString(36)}-${(globalThis.performance?.now() ?? 0).toString(36)}`
    }
}

export function editorId(): string {
    const crypto = globalThis.crypto
    try {
        if (typeof crypto?.randomUUID === 'function') return crypto.randomUUID()
    } catch { /* restricted context */
    }
    try {
        if (typeof crypto?.getRandomValues === 'function') {
            const bytes = crypto.getRandomValues(new Uint8Array(16))
            bytes[6] = (bytes[6]! & 15) | 64
            bytes[8] = (bytes[8]! & 63) | 128
            const hex = Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('')
            return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
        }
    } catch { /* unavailable randomness; uniqueness is sufficient for editor IDs */
    }
    return `editor-${session}-${Date.now().toString(36)}-${(++counter).toString(36)}`
}
