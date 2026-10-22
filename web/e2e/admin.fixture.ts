import {readFileSync} from 'node:fs'
import {expect, type Page, type Route, test as base} from '@playwright/test'
import type {Rule, RulePhase} from '../src/api/rules'
import type {Capture} from '../src/api/types'

const TOKEN = 'fixture-admin-token-not-a-real-secret'
const clone = <T>(value: T): T => structuredClone(value)

export function fixtureRule(overrides: Partial<Rule> = {}): Rule {
    return {
        schema_version: 2,
        id: 'rule-request',
        name: 'Fixture request rule',
        description: 'Public synthetic browser regression fixture',
        enabled: true,
        priority: 10,
        order_index: 0,
        phase: 'request',
        when: {
            op: 'any', conditions: [
                {op: 'eq', source: 'context', path: '/model', value: 'fixture-old-model'},
                {op: 'exists', source: 'current', path: '/input'},
            ]
        },
        actions: [
            {
                id: 'action-set',
                type: 'json_set',
                params: {path: '/metadata/reviewed', value: false, create_parents: true}
            },
            {
                id: 'action-model',
                type: 'text_replace',
                params: {path: '/model', pattern: 'fixture-old-model', replacement: 'fixture-new-model'}
            },
        ],
        stop_after_match: false,
        on_error: 'skip_rule',
        revision: 1,
        source: 'user',
        created_at: '2026-01-01T00:00:00Z',
        updated_at: '2026-01-01T00:00:00Z',
        ...overrides,
    }
}

export const beforeRequest = {
    model: 'fixture-old-model',
    input: [{role: 'user', content: 'Synthetic public capture, no sensitive data.'}],
    metadata: {reviewed: true, count: 0, nullable: null},
}

function fixtureCapture(id: number, overrides: Partial<Capture> = {}): Capture {
    const frames = [1, 2, 3].map(seq => ({
        seq, dir: 'in' as const, kind: 'sse_event', type: 'response.output_text.delta',
        at_ms: seq * 20, bytes: 60,
        data: {
            type: 'response.output_text.delta',
            delta: `fixture event ${seq}`,
            item_id: 'fixture-message',
            output_index: 0,
            content_index: 0
        },
    }))
    return {
        id,
        account_id: 7,
        account_name: 'Fixture account',
        api_key_id: 3,
        api_key_name: 'Fixture key',
        client_transport: 'http',
        upstream_transport: 'sse',
        url: '/v1/responses',
        model: 'fixture-old-model',
        session_id: 'fixture-session',
        status: 200,
        outcome: 'ok',
        error: '',
        duration_ms: 80,
        ttfb_ms: 20,
        request_headers: [{name: 'content-type', value: 'application/json'}],
        request_body: JSON.stringify(beforeRequest),
        request_bytes: 128,
        response_headers: [],
        response_frames: frames,
        response_text: 'fixture response',
        response_id: 'fixture-response',
        prompt_tokens: 4,
        completion_tokens: 2,
        total_tokens: 6,
        truncated: false,
        created_at: 1_767_225_600_000 + id,
        ...overrides,
    }
}

export interface SimulationRequest {
    capture_id: number
    rule: Rule
    phase: RulePhase
    scope: 'rule' | 'ruleset'
    frame_seq?: number
    batch?: boolean
    after_seq?: number
    limit?: number
    expected_version?: number
    enable_draft?: boolean
    batch_token?: string
}

export interface RecordedRequest {
    method: string;
    path: string;
    query: string;
    body: any
}

export interface MockResponse {
    status?: number;
    body: unknown
}

export function simulationResponse(request: SimulationRequest, options: {hasMore?: boolean; source?: string} = {}) {
    const seq = request.frame_seq ?? (request.after_seq ? request.after_seq + 1 : 1)
    const before = request.phase === 'response_event'
        ? {type: 'response.output_text.delta', delta: `fixture event ${seq}`}
        : clone(beforeRequest)
    const after = request.phase === 'response_event'
        ? {...before, delta: `modified event ${seq}`}
        : {...before, model: 'fixture-new-model', metadata: {...beforeRequest.metadata, reviewed: false}}
    const ruleID = request.rule.id ?? 'draft-rule'
    return {
        valid: true, capture_id: request.capture_id, draft_rule_id: ruleID, rules_version: 7, phase: request.phase,
        results: [{
            ...(request.phase === 'response_event' ? {frame_seq: seq, event_type: 'response.output_text.delta'} : {}),
            source: options.source ?? (request.phase === 'response_event' ? 'recorded_event' : 'checkpoint'),
            before,
            result: {
                body: after, model: 'fixture-new-model', changed: true, blocked: false, dropped: false,
                traces: request.rule.actions.map((action, index) => ({
                    rule_id: ruleID, rule_name: request.rule.name, action_id: action.id, action_type: action.type,
                    action_index: index, phase: request.phase, matched: true, status: 'changed',
                    changes: [simulationChange(request.phase, index, seq)],
                })),
                condition_traces: [
                    {rule_id: ruleID, path: '/when', status: 'matched', matched: true},
                    {rule_id: ruleID, path: '/when/conditions/0', status: 'matched', matched: true},
                    {rule_id: ruleID, path: '/when/conditions/1', status: 'skipped'},
                ],
            },
        }],
        total: request.batch ? 3 : 1,
        has_more: options.hasMore ?? false,
        next_after_seq: seq,
        ...(request.batch ? {batch_token: 'fixture-same-version-and-draft-token'} : {}),
    }
}

function simulationChange(phase: string, index: number, seq: number) {
    const change = {operation: 'replace', before_exists: true, after_exists: true}
    if (phase === 'response_event') return {...change, path: '/delta', before: `fixture event ${seq}`, after: `modified event ${seq}`}
    if (index) return {...change, path: '/model', before: 'fixture-old-model', after: 'fixture-new-model'}
    return {...change, path: '/metadata/reviewed', before: true, after: false}
}

export class AdminFixture {
    readonly requests: RecordedRequest[] = []
    readonly unexpected: string[] = []
    readonly pageErrors: string[] = []
    rules = [fixtureRule(), fixtureRule({
        id: 'rule-event', name: 'Fixture event rule', phase: 'response_event',
        actions: [{id: 'event-action', type: 'json_set', params: {path: '/delta', value: 'modified event'}}]
    })]
    captures = [fixtureCapture(101), fixtureCapture(102, {model: 'fixture-second-model'})]
    deletedCaptures = new Set<number>()
    version = 7
    conflictNextSave = false
    private readonly catalog = JSON.parse(readFileSync(process.env.RULES_E2E_CATALOG!, 'utf8'))

    constructor(readonly page: Page) {
    }

    simulate: (request: SimulationRequest) => MockResponse | Promise<MockResponse> = request => ({body: simulationResponse(request)})

    async install() {
        this.page.on('pageerror', error => this.pageErrors.push(error.message))
        await this.page.addInitScript(({token}) => {
            localStorage.setItem('pi-gateway-token', token)
            localStorage.setItem('pi-gateway-locale', 'zh-CN')
            localStorage.setItem('pi-gateway-theme', 'light')
        }, {token: TOKEN})
        await this.page.route(url => new URL(url).pathname.startsWith('/api/'), route => this.route(route))
        // Offline test fixtures must never forward real upstream requests.
        await this.page.route(url => new URL(url).pathname.startsWith('/v1/'), async route => {
            this.unexpected.push(`Forbidden upstream request ${route.request().url()}`)
            await route.abort('blockedbyclient')
        })
    }

    async open(path = '/rules') {
        await this.page.goto(path)
        await expect(this.page.getByRole('button', {name: /^(新增规则|New rule)$/})).toBeVisible()
        await expect.poll(() => this.requests.some(request => request.path === '/api/rules/schema')).toBe(true)
    }

    async editRule(name = 'Fixture request rule', mode: 'visual' | 'steps' = 'visual') {
        await this.page.locator('article').filter({has: this.page.getByRole('heading', {name, exact: true})})
            .getByRole('button', {name: '编辑', exact: true}).click()
        if (mode === 'visual') {
            await this.page.locator('form.rule-editor').getByRole('button', {name: /^(图形编辑|Visual editor)$/}).click()
            await expect(this.page.getByTestId('rule-canvas')).toBeVisible()
        }
    }

    simulationRequests() {
        return this.requests.filter(request => request.path === '/api/rules/simulate-capture').map(request => request.body as SimulationRequest)
    }

    private async route(route: Route) {
        const request = route.request()
        const url = new URL(request.url())
        const path = url.pathname
        const method = request.method()
        const body = request.postData() ? request.postDataJSON() : undefined
        this.requests.push({method, path, query: url.search, body: clone(body)})
        const reply = (value: unknown, status = 200) => route.fulfill({status, json: value})
        if (path === '/api/auth/login') return reply({
            token: TOKEN,
            username: 'fixture-admin',
            expires_at: 4_102_444_800
        })
        if (request.headers().authorization !== `Bearer ${TOKEN}`) return reply({error: 'authentication required'}, 401)
        const staticResponses: Record<string, () => unknown> = {
            '/api/auth/me': () => ({username: 'fixture-admin'}),
            '/api/auth/logout': () => ({ok: true}),
            '/api/rules/schema': () => (this.catalog),
            '/api/rules/validate': () => ({valid: true, rule: body.rule}),
            '/api/accounts': () => ({accounts: [{id: 7, name: 'Fixture account', status: 'active'}]}),
            '/api/keys': () => ({keys: [{id: 3, name: 'Fixture key'}]}),
        }
        if (Object.hasOwn(staticResponses, path)) return reply(staticResponses[path]!())
        if (path === '/api/rules/simulate-capture') {
            if (this.deletedCaptures.has(body.capture_id)) return reply({error: 'capture not found'}, 404)
            const response = await this.simulate(body)
            return reply(response.body, response.status)
        }
        const listRules = () => {
            const pageSize = Number(url.searchParams.get('page_size') ?? 20)
            const page = Number(url.searchParams.get('page') ?? 1)
            const search = url.searchParams.get('search') ?? ''
            const phase = url.searchParams.get('phase')
            const filtered = this.rules.filter(rule => rule.name.includes(search) && (!phase || rule.phase === phase))
            return reply({
                rules: clone(filtered.slice((page - 1) * pageSize, page * pageSize)),
                total: filtered.length,
                page,
                page_size: pageSize,
                version: this.version
            })
        }
        if (path === '/api/rules' && method === 'GET') return listRules()
        if (path === '/api/rules' && method === 'POST') {
            const rule = {
                ...clone(body.rule),
                id: `created-rule-${this.rules.length}`,
                revision: 1,
                order_index: this.rules.length
            }
            this.rules.push(rule)
            return reply({rule, version: ++this.version})
        }
        const ruleID = /^\/api\/rules\/([^/]+)$/.exec(path)?.[1]
        const existingRule = (id: string) => {
            const index = this.rules.findIndex(rule => rule.id === id)
            if (index < 0) return reply({error: 'rule not found'}, 404)
            if (method === 'GET') return reply({rule: clone(this.rules[index]), version: this.version})
            if (method === 'PUT') {
                if (this.conflictNextSave) {
                    this.conflictNextSave = false
                    this.rules[index] = {
                        ...this.rules[index]!,
                        revision: this.rules[index]!.revision! + 1,
                        description: 'Changed by another administrator'
                    }
                    this.version++
                    return reply({error: 'rule revision conflict'}, 409)
                }
                if (body.expected_revision !== this.rules[index]!.revision) return reply({error: 'rule revision conflict'}, 409)
                this.rules[index] = {...clone(body.rule), id, revision: this.rules[index]!.revision! + 1}
                return reply({rule: clone(this.rules[index]), version: ++this.version})
            }
        }
        if (ruleID && (method === 'GET' || method === 'PUT')) return existingRule(ruleID)
        const captureResponse = () => {
            if (path === '/api/captures') {
                const limit = Number(url.searchParams.get('limit') ?? 20)
                const offset = Number(url.searchParams.get('offset') ?? 0)
                const search = url.searchParams.get('search') ?? ''
                const filtered = this.captures.filter(capture => !search || capture.model.includes(search))
                // Summaries intentionally omit body/frames; selecting a capture must load its details.
                const summaries = filtered.slice(offset, offset + limit).map(({
                    request_body,
                    response_frames,
                    response_text,
                    ...summary
                }) => summary)
                return reply({captures: summaries, total: filtered.length, limit, offset})
            }
            const captureID = /^\/api\/captures\/(\d+)$/.exec(path)?.[1]
            if (captureID) {
                const id = Number(captureID)
                const capture = this.captures.find(value => value.id === id)
                return !capture || this.deletedCaptures.has(id) ? reply({error: 'capture not found'}, 404) : reply({capture: clone(capture)})
            }
            this.unexpected.push(`${method} ${path}`)
            return reply({error: `Unmocked administrator endpoint: ${method} ${path}`}, 501)
        }
        if (path.startsWith('/api/captures') && method === 'GET') return captureResponse()
        this.unexpected.push(`${method} ${path}`)
        return reply({error: `Unmocked administrator endpoint: ${method} ${path}`}, 501)
    }
}

export const test = base.extend<{admin: AdminFixture}>({
    admin: async ({page}, use) => {
        const admin = new AdminFixture(page)
        await admin.install()
        await use(admin)
        expect(admin.unexpected, 'No fixture may silently access an unmocked API or upstream').toEqual([])
        expect(admin.pageErrors, 'The real Vue page must not throw browser exceptions').toEqual([])
    },
})
export {expect} from '@playwright/test'
