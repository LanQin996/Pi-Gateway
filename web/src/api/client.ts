import type {
    Account,
    AccountGroup,
    AccountModelTestResult,
    AccountStats,
    ApiKey,
    Capture,
    ConsumeResetResult,
    KeyStats,
    Middleware,
    MiddlewareRow,
    ModelCatalog,
    ModelStats,
    OAuthFlow,
    OfficialUsageResponse,
    OfficialUsageSync,
    Overview,
    ProxyList,
    ProxyTestResult,
    QuotaView,
    Rule,
    RuleList,
    RuleSimulationRequest,
    RuleSimulationResult,
    RuleValidation,
    SavedProxy,
    SavedProxyTest,
    Settings,
    SettingsResponse,
    StatsBucket,
    StatsSummary,
    UsageQuery,
    UsageRecord,
} from './types'
import {translateNow} from '../i18n'
import type {BackendRuleSchema} from '../utils/ruleSchemaAdapter'
import type {CaptureSimulationRequest, CaptureSimulationResponse} from './ruleSimulation'

const TOKEN_KEY = 'pi-gateway-token'

// An ApiError carries the HTTP status so callers can react to 401 specially.
export class ApiError extends Error {
    status: number
    payload: unknown

    constructor(status: number, message: string, payload?: unknown) {
        super(message)
        this.name = 'ApiError'
        this.status = status
        this.payload = payload
    }
}

export function getToken(): string {
    return localStorage.getItem(TOKEN_KEY) ?? ''
}

export function setToken(token: string) {
    localStorage.setItem(TOKEN_KEY, token)
}

export function clearToken() {
    localStorage.removeItem(TOKEN_KEY)
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const headers = new Headers(init.headers)
    const token = getToken()
    if (token) headers.set('Authorization', `Bearer ${token}`)
    if (init.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json')

    const response = await fetch(path, {...init, headers})
    const text = await response.text()
    if (!response.ok) throw responseError(response, text, token)

    if (!text) return undefined as T
    return JSON.parse(text) as T
}

function responseError(response: Response, text: string, token: string): ApiError {
    let message = text || response.statusText
    let payload: unknown
    try {
        const parsed = JSON.parse(text)
        payload = parsed
        const error = typeof parsed?.error === 'string' ? parsed.error : parsed?.error?.message
        if (typeof error === 'string' && error) message = error
    } catch {
        // Not JSON; use the raw text.
    }

    if (response.status === 401) message = authenticationMessage(message, token)
    return new ApiError(response.status, message, payload)

}

function authenticationMessage(message: string, token: string): string {
    // The backend currently distinguishes these 401s by message, not an error code.
    // A wrong login/current password must not invalidate a working session.
    if (message === 'authentication required') {
        if (token && getToken() === token) clearToken()
        message = translateNow(token ? 'common.sessionExpired' : 'common.authenticationRequired')
    } else if (message === 'invalid username or password') {
        message = translateNow('common.authenticationFailed')
    } else if (message === 'current password is incorrect') {
        message = translateNow('settings.currentPasswordIncorrect')
    }
    return message
}

export interface AccountOperationResult {
    index: number
    id?: number
    name?: string
    status: 'success' | 'failed' | 'created' | 'linked' | 'skipped'
    error?: string
}

export const api = {
    officialUsage(startDate: string, endDate: string, accountId?: number) {
        const query = new URLSearchParams({start_date: startDate, end_date: endDate})
        if (accountId) query.set('account_id', String(accountId))
        return request<OfficialUsageResponse>(`/api/usage/official?${query}`)
    },
    refreshOfficialUsage(accountId: number) {
        return request<{
            sync: OfficialUsageSync;
            error: string
        }>(`/api/accounts/${accountId}/official-usage/refresh`, {method: 'POST'})
    },
    // auth
    async login(username: string, password: string) {
        return request<{token: string; username: string; expires_at: number}>('/api/auth/login', {
            method: 'POST',
            body: JSON.stringify({username, password}),
        })
    },
    async logout() {
        return request<{ok: boolean}>('/api/auth/logout', {method: 'POST'})
    },
    async me() {
        return request<{username: string}>('/api/auth/me')
    },
    async changePassword(currentPassword: string, newPassword: string) {
        return request<{ok: boolean}>('/api/settings/password', {
            method: 'POST',
            body: JSON.stringify({current_password: currentPassword, new_password: newPassword}),
        })
    },

    // dashboard
    async overview() {
        return request<Overview>('/api/overview')
    },

    // accounts
    async listAccounts() {
        return request<{accounts: Account[]}>('/api/accounts')
    },
    async batchAccounts(ids: number[], action: 'enable' | 'disable' | 'recover' | 'add_groups', groupIds: number[] = []) {
        return request<{results: AccountOperationResult[]}>('/api/accounts/batch', {
            method: 'POST', body: JSON.stringify({ids, action, group_ids: groupIds}),
        })
    },
    async exportAccounts(ids: number[]) {
        return request<unknown>('/api/accounts/export', {method: 'POST', body: JSON.stringify({ids})})
    },
    async importAccounts(format: string, data: unknown, targetAccountId?: number) {
        return request<{format: string; results: AccountOperationResult[]}>('/api/accounts/import', {
            method: 'POST', body: JSON.stringify({format, data, target_account_id: targetAccountId}),
        })
    },
    async startOAuth(payload: {
        kind: 'chatgpt' | 'codex';
        name?: string;
        proxy_url?: string;
        proxy_id?: number | null;
        account_id?: string
    }) {
        return request<{flow: OAuthFlow}>('/api/oauth/start', {
            method: 'POST',
            body: JSON.stringify(payload),
        })
    },
    async oauthStatus(flowId: string) {
        return request<{flow: OAuthFlow; account?: Account}>(`/api/oauth/${flowId}`)
    },
    async completeOAuth(flowId: string, input: string) {
        return request<{flow: OAuthFlow; account?: Account}>(`/api/oauth/${flowId}/complete`, {
            method: 'POST',
            body: JSON.stringify({input}),
        })
    },
    async importAccount(payload: {
        name?: string;
        credential_type?: string;
        oauth_client_id?: string;
        refresh_token?: string;
        access_token?: string;
        proxy_url?: string;
        proxy_id?: number | null
    }) {
        return request<{account: Account}>('/api/accounts', {
            method: 'POST',
            body: JSON.stringify(payload),
        })
    },
    async updateAccount(id: number, patch: Partial<Account>) {
        return request<{account: Account}>(`/api/accounts/${id}`, {
            method: 'PATCH',
            body: JSON.stringify(patch),
        })
    },
    async deleteAccount(id: number) {
        return request<{ok: boolean}>(`/api/accounts/${id}`, {method: 'DELETE'})
    },
    async unlinkCodex(id: number) {
        return request<{account: Account}>(`/api/accounts/${id}/codex`, {method: 'DELETE'})
    },
    async refreshAccount(id: number) {
        return request<{account: Account}>(`/api/accounts/${id}/refresh`, {method: 'POST'})
    },

    async recoverAccount(id: number) {
        return request<{account: Account}>(`/api/accounts/${id}/recover`, {method: 'POST'})
    },
    async testProxy(id: number, proxyUrl?: string) {
        return request<ProxyTestResult>(
            `/api/accounts/${id}/test-proxy`,
            {method: 'POST', body: JSON.stringify({proxy_url: proxyUrl ?? null})},
        )
    },

    async getAccountModels(id: number, signal?: AbortSignal) {
        return request<ModelCatalog>(`/api/accounts/${id}/models`, {signal})
    },
    async refreshAccountModels(id: number, signal?: AbortSignal) {
        return request<ModelCatalog>(`/api/accounts/${id}/models/refresh`, {method: 'POST', signal})
    },
    async testAccountModel(id: number, payload: {model: string; message: string}, signal?: AbortSignal) {
        return request<AccountModelTestResult>(`/api/accounts/${id}/test-model`, {
            method: 'POST', body: JSON.stringify(payload), signal,
        })
    },

    // Saved proxies: secrets are write-only; an empty edit URL preserves them.
    async listProxies(params: {search?: string; page?: number; page_size?: number} = {}) {
        const query = new URLSearchParams()
        for (const [key, value] of Object.entries(params)) if (value !== undefined) query.set(key, String(value))
        return request<ProxyList>(`/api/proxies?${query}`)
    },
    async createProxy(payload: {name: string; url: string}) {
        return request<{proxy: SavedProxy}>('/api/proxies', {method: 'POST', body: JSON.stringify(payload)})
    },
    async updateProxy(id: number, payload: {name?: string; url?: string}) {
        return request<{proxy: SavedProxy}>(`/api/proxies/${id}`, {method: 'PATCH', body: JSON.stringify(payload)})
    },
    async deleteProxy(id: number) {
        return request<{ok: boolean}>(`/api/proxies/${id}`, {method: 'DELETE'})
    },
    async testSavedProxy(id: number) {
        return request<{proxy: SavedProxy; test: SavedProxyTest}>(`/api/proxies/${id}/test`, {method: 'POST'})
    },
    async probeProxy(url: string) {
        return request<{test: SavedProxyTest}>('/api/proxies/probe', {method: 'POST', body: JSON.stringify({url})})
    },
    async listProxyAccounts(id: number, params: {search?: string; page?: number; page_size?: number} = {}) {
        const query = new URLSearchParams()
        for (const [key, value] of Object.entries(params)) if (value !== undefined) query.set(key, String(value))
        return request<{
            accounts: Account[];
            total: number;
            page: number;
            page_size: number
        }>(`/api/proxies/${id}/accounts?${query}`)
    },
    async assignProxyAccounts(id: number, account_ids: number[]) {
        return request<{ok: boolean}>(`/api/proxies/${id}/accounts`, {
            method: 'PUT',
            body: JSON.stringify({account_ids})
        })
    },

    // quota
    async getQuota(id: number) {
        return request<{quota: QuotaView}>(`/api/accounts/${id}/quota`)
    },
    async refreshQuota(id: number) {
        return request<{quota: QuotaView; fetched: boolean; error?: string}>(`/api/accounts/${id}/quota/refresh`, {
            method: 'POST',
        })
    },
    async consumeResetCredit(id: number, creditId?: string) {
        return request<{quota: QuotaView; result?: ConsumeResetResult; fetched: boolean; error?: string}>(
            `/api/accounts/${id}/reset-credits/consume`,
            {method: 'POST', body: JSON.stringify({credit_id: creditId ?? ''})},
        )
    },

    // keys
    async listKeys() {
        return request<{keys: ApiKey[]}>('/api/keys')
    },
    async createKey(payload: Partial<ApiKey>) {
        return request<{key: ApiKey}>('/api/keys', {method: 'POST', body: JSON.stringify(payload)})
    },
    async updateKey(id: number, patch: Partial<ApiKey>) {
        return request<{key: ApiKey}>(`/api/keys/${id}`, {method: 'PATCH', body: JSON.stringify(patch)})
    },
    async deleteKey(id: number) {
        return request<{ok: boolean}>(`/api/keys/${id}`, {method: 'DELETE'})
    },
    async listAccountGroups() {
        return request<{groups: AccountGroup[]}>('/api/account-groups')
    },
    async createAccountGroup(payload: {
        name: string;
        notes?: string;
        enabled?: boolean;
        account_ids?: number[];
        switch_on_429?: string;
        disabled_models?: string[]
    }) {
        return request<{group: AccountGroup}>('/api/account-groups', {method: 'POST', body: JSON.stringify(payload)})
    },
    async updateAccountGroup(id: number, patch: Partial<AccountGroup>) {
        return request<{group: AccountGroup}>(`/api/account-groups/${id}`, {
            method: 'PATCH',
            body: JSON.stringify(patch)
        })
    },
    async deleteAccountGroup(id: number) {
        return request<{ok: boolean}>(`/api/account-groups/${id}`, {method: 'DELETE'})
    },

    // Statistics and usage follow the snake_case admin API contract.
    async statsSummary(start: number, end: number) {
        return request<StatsSummary>(`/api/stats/summary?start=${start}&end=${end}`)
    },
    async statsTrend(start: number, end: number, bucket: 'hour' | 'day') {
        return request<{buckets: StatsBucket[]}>(`/api/stats/trend?start=${start}&end=${end}&bucket=${bucket}`)
    },
    async statsModels(start: number, end: number) {
        return request<{items: ModelStats[]}>(`/api/stats/models?start=${start}&end=${end}`)
    },
    async statsKeys(start: number, end: number) {
        return request<{items: KeyStats[]}>(`/api/stats/keys?start=${start}&end=${end}`)
    },
    async statsAccounts(start: number, end: number) {
        return request<{items: AccountStats[]}>(`/api/stats/accounts?start=${start}&end=${end}`)
    },
    async usageRecords(params: UsageQuery) {
        const query = new URLSearchParams()
        for (const [key, value] of Object.entries(params)) {
            if (value !== undefined && value !== '' && value !== null) query.set(key, String(value))
        }
        return request<{records: UsageRecord[]; total: number}>(`/api/usage/records?${query.toString()}`)
    },

    // captures
    async listCaptures(params: Record<string, string | number | undefined>) {
        const query = new URLSearchParams()
        for (const [key, value] of Object.entries(params)) {
            if (value !== undefined && value !== '' && value !== null) query.set(key, String(value))
        }
        return request<{captures: Capture[]; total: number; limit: number; offset: number}>(
            `/api/captures?${query.toString()}`,
        )
    },
    async getCapture(id: number) {
        return request<{capture: Capture}>(`/api/captures/${id}`)
    },
    async deleteCapture(id: number) {
        return request<{ok: boolean}>(`/api/captures/${id}`, {method: 'DELETE'})
    },
    async clearCaptures(accountId?: number, unassigned = false) {
        let query = accountId ? `?account_id=${accountId}` : ''
        if (unassigned) query = '?unassigned=true'
        return request<{deleted: number}>(`/api/captures${query}`, {method: 'DELETE'})
    },
    exportUrl(params: Record<string, string | number | undefined>) {
        const query = new URLSearchParams()
        for (const [key, value] of Object.entries(params)) {
            if (value !== undefined && value !== '' && value !== null) query.set(key, String(value))
        }
        // The token is passed as a query parameter because this is a plain download.
        query.set('token', getToken())
        return `/api/captures/export?${query.toString()}`
    },

    // settings
    async getSettings() {
        return request<SettingsResponse>('/api/settings')
    },
    async putSettings(settings: Settings) {
        return request<{settings: Settings}>('/api/settings', {method: 'PUT', body: JSON.stringify(settings)})
    },
    // unified JSON rules management; the old /api/middlewares client remains below for compatibility.
    async listRules(params: {
        search?: string;
        phase?: string;
        enabled?: boolean;
        limit?: number;
        offset?: number
    } = {}) {
        const query = new URLSearchParams()
        for (const [key, value] of Object.entries(params)) if (value !== undefined && value !== '' && key !== 'limit' && key !== 'offset') query.set(key, String(value))
        const pageSize = params.limit ?? 20
        query.set('page_size', String(pageSize))
        query.set('page', String(Math.floor((params.offset ?? 0) / pageSize) + 1))
        const result = await request<RuleList & {page?: number; page_size?: number}>(`/api/rules?${query.toString()}`)
        return {
            ...result,
            limit: result.page_size ?? result.limit ?? pageSize,
            offset: result.page ? (result.page - 1) * (result.page_size ?? pageSize) : result.offset ?? params.offset ?? 0
        }
    },
    async getRule(id: string) {
        return request<{rule: Rule; version?: number}>(`/api/rules/${encodeURIComponent(id)}`)
    },
    async createRule(rule: Rule, expected_version?: number) {
        return request<{rule: Rule; version?: number}>('/api/rules', {
            method: 'POST',
            body: JSON.stringify({rule, expected_version})
        })
    },
    async updateRule(id: string, rule: Rule, expected_revision?: number) {
        return request<{rule: Rule; version?: number}>(`/api/rules/${encodeURIComponent(id)}`, {
            method: 'PUT',
            body: JSON.stringify({rule, expected_revision})
        })
    },
    async deleteRule(id: string, expected_revision?: number) {
        const query = expected_revision === undefined ? '' : `?expected_revision=${encodeURIComponent(String(expected_revision))}`
        return request<{
            ok: boolean;
            version?: number
        }>(`/api/rules/${encodeURIComponent(id)}${query}`, {method: 'DELETE'})
    },
    async duplicateRule(id: string, expected_revision?: number, name?: string) {
        return request<{
            rule: Rule;
            version?: number
        }>(`/api/rules/${encodeURIComponent(id)}/duplicate`, {
            method: 'POST',
            body: JSON.stringify({expected_revision, name})
        })
    },
    async moveRule(id: string, expected_revision: number, direction: 'up' | 'down', expected_version: number) {
        return request<{rules: Rule[]; version?: number}>('/api/rules/reorder', {
            method: 'POST',
            body: JSON.stringify({id, expected_revision, direction, expected_version})
        })
    },
    async reorderRules(items: Array<{
        id: string;
        expected_revision?: number;
        order_index?: number;
        priority?: number
    }>, expected_version?: number) {
        return request<{rules: Rule[]; version?: number}>('/api/rules/reorder', {
            method: 'POST',
            body: JSON.stringify({items, expected_version})
        })
    },
    async batchRules(items: Array<{
        id: string;
        expected_revision?: number
    }>, enabled: boolean, expected_version?: number) {
        return request<{rules: Rule[]; version?: number}>('/api/rules/batch', {
            method: 'POST',
            body: JSON.stringify({items, enabled, expected_version})
        })
    },
    async getRuleSchema() {
        return request<BackendRuleSchema>('/api/rules/schema')
    },
    async validateRule(rule: Rule | Rule[], asBatch = false) {
        return request<RuleValidation & {rules?: Rule[]}>('/api/rules/validate', {
            method: 'POST',
            body: JSON.stringify(asBatch ? {rules: rule} : {rule})
        })
    },
    async simulateRules(payload: RuleSimulationRequest | {rules: Rule[]; phase: string; input: unknown}) {
        return request<{
            valid: boolean;
            result?: RuleSimulationResult;
            errors?: Array<{path: string; message: string}>;
            error?: string
        }>('/api/rules/simulate', {method: 'POST', body: JSON.stringify(payload)})
    },
    async simulateCapture(payload: CaptureSimulationRequest, signal?: AbortSignal) {
        return request<CaptureSimulationResponse>('/api/rules/simulate-capture', {
            method: 'POST', body: JSON.stringify(payload), signal,
        })
    },
    async listMiddlewares() {
        return request<{middlewares: Middleware[]}>('/api/middlewares')
    },
    async updateMiddleware(name: string, patch: {
        enabled?: boolean;
        order_index?: number;
        config?: string;
        expected_revision?: number
    }) {
        return request<{middleware: MiddlewareRow}>(`/api/middlewares/${name}`, {
            method: 'PUT',
            body: JSON.stringify(patch),
        })
    },
}
