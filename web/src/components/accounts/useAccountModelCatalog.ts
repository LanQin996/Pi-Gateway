import {computed, onBeforeUnmount, ref, type Ref, watch} from 'vue'
import {api, ApiError} from '../../api/client'
import type {Account, ModelCatalog, ModelCatalogOrigin, ModelMetadata, UpstreamModel} from '../../api/types'
import {useAccountControls} from './accountControlsLocale'

export interface CatalogModel extends UpstreamModel {
    /** Source account names, not provider origins. */
    sources: string[]
}

export const modelCatalogOrigins = ['chatgpt', 'codex'] as const
export const modelCatalogOriginNames: Record<ModelCatalogOrigin, string> = {chatgpt: 'ChatGPT', codex: 'Codex'}

function mergeModelOrigins(...sources: Array<UpstreamModel['origins']>): UpstreamModel['origins'] {
    const origins = modelCatalogOrigins.filter(origin => sources.some(source => source?.includes(origin)))
    return origins.length ? origins : undefined
}

export function modelOriginNames(model: UpstreamModel): string {
    return (mergeModelOrigins(model.origins) ?? []).map(origin => modelCatalogOriginNames[origin]).join(' / ')
}

export function isCodexOnlyModel(model: UpstreamModel): boolean {
    return model.source !== 'manual' && model.origins?.includes('codex') === true && !model.origins?.includes('chatgpt')
}

export interface ModelCapability {
    key: string
    value: string
}

const capabilityText = (value: unknown): string | undefined => {
        if (typeof value !== 'string' || !value.trim()) { return undefined }
        return value.length > 160 ? `${value.slice(0, 160)}…` : value
    }
const capabilityList = (value: unknown, reasoning = false): string | undefined => {
        if (!Array.isArray(value)) { return undefined }
        if (!value.length) { return '[]' }
        const items = value.slice(0, 16).flatMap(item => {
            const value = capabilityText(reasoning && item && typeof item === 'object' && !Array.isArray(item) ? item.effort : item)
            return value === undefined ? [] : [value]
        })
        return (() => {
    if (items.length) {
        return `${items.join(', ')}${value.length > 16 ? ', …' : ''}`
    }
    return undefined
})()
    }

// Render a bounded, plain-text view, never arbitrary provider objects or HTML.
// Unsafe integers are omitted because JSON parsing may already have rounded them.
function capabilityValue(key: string, raw: unknown): string | undefined {
        if (key === 'supported_reasoning_levels') { return capabilityList(raw, true) }
        else if (key === 'input_modalities' || key === 'output_modalities') { return capabilityList(raw) }
        else if (key === 'context_window' || key === 'max_context_window' || key === 'max_output_tokens') { return typeof raw === 'number' && Number.isSafeInteger(raw) && raw >= 0 ? String(raw) : undefined }
        else if (key.startsWith('supports_')) { return typeof raw === 'boolean' ? String(raw) : undefined }
        else { return capabilityText(raw) }
}

export function modelCapabilities(metadata: ModelMetadata | undefined): ModelCapability[] {
    if (!metadata || typeof metadata !== 'object' || Array.isArray(metadata)) { return [] }
    const capabilities: ModelCapability[] = []
    for (const key of ['supported_reasoning_levels', 'default_reasoning_level', 'supports_reasoning_summary_parameter', 'default_reasoning_summary', 'context_window', 'max_context_window', 'max_output_tokens', 'input_modalities', 'output_modalities', 'supports_parallel_tool_calls']) {
        if (!Object.hasOwn(metadata, key)) { continue }
        const raw = metadata[key]
        const value = capabilityValue(key, raw)
        if (value !== undefined) { capabilities.push({key, value}) }
    }
    return capabilities
}

export function manualCatalogModel(id: string): CatalogModel {
    return {
        id, name: id, source: 'manual', sources: [],
        metadata: {
            supported_reasoning_levels: ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'].map(effort => ({
                effort,
                description: effort
            })),
            default_reasoning_level: 'medium',
            supports_reasoning_summary_parameter: true,
            default_reasoning_summary: 'auto',
        },
    }
}

export function modelMetadataJSON(metadata: unknown): string | undefined {
    try {
        // Preserve every JSON value and unknown key. Non-JSON runtime values
        // such as cycles fail explicitly rather than inventing provider metadata.
        return JSON.stringify(metadata, null, 2)
    } catch {
        return undefined
    }
}

export function modelMetadataPreview(metadata: unknown) {
    const json = modelMetadataJSON(metadata)
    return {text: json?.slice(0, 20_000) ?? '', truncated: (json?.length ?? 0) > 20_000, failed: json === undefined}
}

function isMetadata(value: unknown): value is ModelMetadata {
    return !!value && typeof value === 'object' && !Array.isArray(value)
}

function isModelCatalog(value: unknown): value is ModelCatalog {
    if (!value || typeof value !== 'object') { return false }
    const candidate = value as Partial<ModelCatalog>
    return Array.isArray(candidate.models) && candidate.models.every(model => model && typeof model.id === 'string' && typeof model.name === 'string')
        && typeof candidate.fetched_at === 'number' && typeof candidate.attempted_at === 'number' && typeof candidate.error === 'string'
}

// GET reads snapshots only. The upstream request is deliberately confined to refresh().
function mergeCatalogEntry(merged: Map<string, CatalogModel>, model: UpstreamModel, sourceName: string) {
                const old = merged.get(model.id)
                if (old) {
                    if (!old.sources.includes(sourceName)) { old.sources.push(sourceName) }
                    old.origins = mergeModelOrigins(old.origins, model.origins)
                    if (old.source === 'manual' && model.source !== 'manual') {
                        // Replace the entire entry, not just its label: manual defaults must
                        // never leak into a real upstream model with missing capabilities.
                        merged.set(model.id, {
                            ...model,
                            name: model.name || model.id,
                            sources: old.sources,
                            origins: old.origins
                        })
                    } else if ((old.source === 'manual') === (model.source === 'manual') && isMetadata(model.metadata) && (old.metadata === undefined || isMetadata(old.metadata))) {
                        // Fill only absent top-level keys. Explicit null/false/[] and
                        // conflicting objects/arrays belong to the primary source.
                        old.metadata = {...model.metadata, ...old.metadata}
                    }
                } else { merged.set(model.id, {
                    ...model,
                    name: model.name || model.id,
                    sources: [sourceName],
                    origins: mergeModelOrigins(model.origins)
                }) }
}

export function useAccountModelCatalog(sources: Ref<Account[]>) {
    const {c} = useAccountControls()
    const catalogs = ref<Record<number, ModelCatalog>>({})
    const errors = ref<Record<number, string>>({})
    const loading = ref(false)
    const refreshing = ref(false)
    const sourceId = ref<number | null>(null)
    let version = 0
    let cacheController: AbortController | undefined
    let refreshController: AbortController | undefined
    const accountName = (account: Account) => account.name || account.email || c('unnamedAccount')
    const sourceKey = computed(() => sources.value.map(account => account.id).sort((a, b) => a - b).join(','))
    const models = computed<CatalogModel[]>(() => {
        const merged = new Map<string, CatalogModel>()
        // Lowest account id wins same-source conflicts, independently of UI order
        // and asynchronous cache response order.
        for (const source of [...sources.value].sort((a, b) => a.id - b.id)) {
            for (const model of catalogs.value[source.id]?.models ?? []) {
                if (!model.id) { continue }
                mergeCatalogEntry(merged, model, accountName(source))
            }
        }
        return [...merged.values()].sort((a, b) => a.name.localeCompare(b.name))
    })
    const issues = computed(() => sources.value.flatMap(account => {
        const catalog = catalogs.value[account.id]
        const sourceWarnings = modelCatalogOrigins.flatMap(origin => {
            const error = catalog?.source_catalogs?.[origin]?.error
            return error ? [`${modelCatalogOriginNames[origin]}: ${error}`] : []
        })
        const message = errors.value[account.id] || catalog?.error || sourceWarnings.join('; ')
        return message ? [`${accountName(account)}: ${message}`] : []
    }))
    const selectedCatalog = computed(() => sourceId.value === null ? undefined : catalogs.value[sourceId.value])

    function abort() {
        version++
        cacheController?.abort()
        refreshController?.abort()
        loading.value = false
        refreshing.value = false
    }

    async function readCache() {
        abort()
        const current = version
        const controller = new AbortController()
        cacheController = controller
        loading.value = true
        const queue = [...sources.value]

        // Bound concurrent cache reads for large account pools.
        async function worker() {
            while (queue.length && !controller.signal.aborted) {
                const account = queue.shift()!
                try {
                    const catalog = await api.getAccountModels(account.id, controller.signal)
                    if (current !== version) { return }
                    catalogs.value = {...catalogs.value, [account.id]: catalog}
                    delete errors.value[account.id]
                } catch (err) {
                    if (controller.signal.aborted || current !== version) { return }
                    errors.value[account.id] = err instanceof Error ? err.message : c('catalogFailed')
                }
            }
        }

        await Promise.all(Array.from({length: Math.min(4, queue.length)}, worker))
        if (current === version) { loading.value = false }
    }

    async function refresh() {
        if (sourceId.value === null || refreshing.value || loading.value) { return }
        const id = sourceId.value
        const current = version
        const controller = new AbortController()
        refreshController = controller
        refreshing.value = true
        try {
            const catalog = await api.refreshAccountModels(id, controller.signal)
            if (current !== version) { return }
            catalogs.value = {...catalogs.value, [id]: catalog}
            delete errors.value[id]
        } catch (err) {
            if (!controller.signal.aborted && current === version) {
                // A failed upstream refresh still carries the persisted last-success snapshot.
                if (err instanceof ApiError && isModelCatalog(err.payload)) { catalogs.value = {
                    ...catalogs.value,
                    [id]: err.payload
                } }
                errors.value[id] = err instanceof Error ? err.message : c('catalogFailed')
            }
        } finally {
            if (current === version) { refreshing.value = false }
        }
    }

    watch(sourceKey, () => {
        if (!sources.value.some(account => account.id === sourceId.value)) { sourceId.value = sources.value[0]?.id ?? null }
        void readCache()
    }, {immediate: true})
    onBeforeUnmount(abort)
    return {
        models,
        catalogs,
        sourceId,
        selectedCatalog,
        issues,
        loading,
        refreshing,
        readCache,
        refresh,
        abort,
        accountName
    }
}
