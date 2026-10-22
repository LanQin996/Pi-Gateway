// Editor-only identity, never use for credentials or server-managed rule IDs.
// getRandomValues supports ordinary HTTP; randomUUID requires a secure context.
// Do not request entropy at module load, so unsupported contexts fail only when
// an editor operation actually requests an ID, never by silently using weak randomness.
export function editorId(): string {
    const bytes = new Uint8Array(16)
    try {
        globalThis.crypto.getRandomValues(bytes)
    } catch {
        throw new Error('Secure random values are unavailable: the editor requires crypto.getRandomValues().')
    }
    bytes[6] = (bytes[6]! & 15) | 64
    bytes[8] = (bytes[8]! & 63) | 128
    const hex = Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('')
    return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}
