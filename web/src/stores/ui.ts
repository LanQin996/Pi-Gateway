import {defineStore} from 'pinia'
import {onScopeDispose, ref, watch} from 'vue'
import {formatDateTime, translateNow} from '../i18n'

export interface Toast {
    id: number
    kind: 'info' | 'success' | 'error'
    message: string
}

let nextId = 1

// Appearance preferences are local to this browser, never gateway settings.
export const useUiStore = defineStore('ui', () => {
    const preferred = window.matchMedia('(prefers-color-scheme: dark)')
    const read = (key: string) => {
        try {
            return localStorage.getItem(key)
        } catch {
            return null
        }
    }
    const storedTheme = read('pi-gateway-theme')
    const hasThemePreference = ref(storedTheme === 'light' || storedTheme === 'dark')
    const systemTheme = preferred.matches ? 'dark' : 'light'
    const theme = ref<'light' | 'dark'>(storedTheme === 'light' || storedTheme === 'dark' ? storedTheme : systemTheme)
    const sidebarCollapsed = ref(read('pi-gateway-sidebar') === 'collapsed')
    const persist = (key: string, value: string) => {
        try {
            localStorage.setItem(key, value)
        } catch { /* Appearance still works when storage is unavailable. */
        }
    }
    watch(theme, value => {
        const root = document.documentElement
        root.dataset.theme = value
        root.dataset.themeColor = 'relay-blue'
        root.style.colorScheme = value
    }, {immediate: true, flush: 'sync'})
    watch(sidebarCollapsed, value => persist('pi-gateway-sidebar', value ? 'collapsed' : 'expanded'))
    const followSystem = (event: MediaQueryListEvent) => {
        if (!hasThemePreference.value) theme.value = event.matches ? 'dark' : 'light'
    }
    preferred.addEventListener('change', followSystem)
    onScopeDispose(() => preferred.removeEventListener('change', followSystem))

    function toggleTheme() {
        hasThemePreference.value = true
        theme.value = theme.value === 'dark' ? 'light' : 'dark'
        persist('pi-gateway-theme', theme.value)
    }

    return {
        theme, sidebarCollapsed, toggleTheme, toggleSidebar: () => {
            sidebarCollapsed.value = !sidebarCollapsed.value
        }
    }
})

export const useToastStore = defineStore('toast', () => {
    const toasts = ref<Toast[]>([])

    function push(kind: Toast['kind'], message: string) {
        const id = nextId++
        toasts.value.push({id, kind, message})
        setTimeout(() => dismiss(id), kind === 'error' ? 8000 : 4000)
    }

    function dismiss(id: number) {
        toasts.value = toasts.value.filter((t) => t.id !== id)
    }

    return {
        toasts,
        dismiss,
        info: (m: string) => push('info', m),
        success: (m: string) => push('success', m),
        error: (m: string) => push('error', m),
    }
})

// formatBytes renders a byte count for tables.
export function formatBytes(bytes: number): string {
    if (!bytes || bytes < 0) return '—'
    const units = ['B', 'KB', 'MB', 'GB']
    let value = bytes
    let unit = 0
    while (value >= 1024 && unit < units.length - 1) {
        value /= 1024
        unit++
    }
    return `${value.toFixed(value < 10 && unit > 0 ? 1 : 0)} ${units[unit]}`
}

// formatDuration renders milliseconds compactly.
export function formatDuration(ms: number): string {
    if (!ms || ms < 0) return '—'
    if (ms < 1000) return `${ms} ms`
    return `${(ms / 1000).toFixed(2)} s`
}

// formatRelative renders a past timestamp relative to now.
export function formatRelative(ms: number): string {
    if (!ms) return '—'
    const delta = Date.now() - ms
    if (delta < 60_000) return translateNow('common.justNow')
    if (delta < 3_600_000) return `${Math.floor(delta / 60_000)}m`
    if (delta < 86_400_000) return `${Math.floor(delta / 3_600_000)}h`
    return `${Math.floor(delta / 86_400_000)}d`
}

// formatUntil renders a countdown for token or window expiry.
export function formatUntil(ms: number): string {
    if (!Number.isFinite(ms)) return '—'
    if (ms <= 0) return translateNow('common.expired')
    const hours = Math.floor(ms / 3_600_000)
    if (hours >= 24) return `${Math.floor(hours / 24)}d ${hours % 24}h`
    if (hours >= 1) return `${hours}h ${Math.floor((ms % 3_600_000) / 60_000)}m`
    return `${Math.max(1, Math.floor(ms / 60_000))}m`
}

// formatTime renders an absolute timestamp.
export function formatTime(ms: number): string {
    return formatDateTime(ms)
}

// formatPercent renders a quota percentage without trailing noise.
export function formatPercent(value: number): string {
    if (!Number.isFinite(value)) return '—'
    return `${Number.isInteger(value) ? value : value.toFixed(1)}%`
}
