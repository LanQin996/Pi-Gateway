// Types mirroring the Go API payloads.

export interface Account {
    /** False for an imported Codex credential awaiting ChatGPT authorization. */
    has_chatgpt_credential?: boolean
    id: number
    name: string
    email: string
    account_id: string
    plan_type: string
    expires_at: number
    enabled: boolean
    weight: number
    cooldown_429_seconds?: number
    concurrency: number
    proxy_url: string
    proxy_id: number | null
    proxy_display: string
    // upstream_protocol selects the protocol used toward the backend:
    // "" inherits the key's transport, then the global default; explicit values override both.
    // "sse" forces SSE; "ws" forces a reused WebSocket connection.
    // It does not restrict what clients may use.
    upstream_protocol: string
    // codex_linked reports whether the optional Codex credential is attached
    // for quota reads and Codex catalog sync. Generation still uses ChatGPT.
    codex_linked: boolean
    // codex_account_id is the linked Codex account id, possibly empty.
    codex_account_id: string
    status: string
    last_error: string
    last_refresh_at: number
    last_used_at: number
    request_count: number
    error_count: number
    created_at: number
    updated_at: number
    inflight: number
    has_refresh_token: boolean
    token_expires_in_ms: number
    quota: QuotaView
    group_ids: number[]
    disabled_models: string[]
    supplemental_models?: string[]
    inherited_model_restrictions: Array<{ model: string; group_names: string[] }>
}

export interface ApiKey {
    id: number
    name: string
    key: string
    enabled: boolean
    strategy: string
    transport: string
    last_used_at: number
    request_count: number
    created_at: number
    updated_at: number
    account_names: string[] | null
    label?: string
    max_concurrency?: number
    requests_per_minute?: number
    daily_limit_usd?: number
    weekly_limit_usd?: number
    group_ids?: number[]
}

export interface AccountGroup {
    switch_on_429?: string
    id: number
    name: string
    enabled: boolean
    notes: string
    disabled_models: string[]
    account_ids: number[]
    created_at: number
    updated_at: number
}

export type ModelMetadata = Record<string, unknown>

export type ModelCatalogOrigin = 'chatgpt' | 'codex'

export interface UpstreamModel {
    id: string
    name: string
    description?: string
    source?: 'upstream' | 'manual'
    /** Provider origins, distinct from source account names; legacy payloads may omit this. */
    origins?: ModelCatalogOrigin[]
    /** Original provider fields or unverified gateway defaults for manual entries. */
    metadata?: ModelMetadata
}

export interface ModelCatalogSource {
    models: UpstreamModel[]
    fetched_at: number
    attempted_at: number
    error: string
    skipped?: boolean
    skip_reason?: string
}

export interface ModelCatalog {
    models: UpstreamModel[]
    fetched_at: number
    attempted_at: number
    error: string
    source_catalogs?: Partial<Record<ModelCatalogOrigin, ModelCatalogSource>>
}

export interface AccountModelTestResult {
    ok: boolean
    model: string
    transport: string
    output: string
    latency_ms: number
    first_token_ms: number | null
    error?: string
    upstream_status?: number
    upstream_body?: string
    upstream_body_truncated?: boolean
    upstream_event?: string
}

export interface StatsRange {
    start: number;
    end: number
}

export type NullableMetric = number | null

export interface StatsSummary {
    request_count: number
    success_count: number
    failure_count: number
    cancelled_count: number
    incomplete_count: number
    input_tokens: NullableMetric
    output_tokens: NullableMetric
    cached_tokens: NullableMetric
    cache_write_tokens: NullableMetric
    reasoning_tokens: NullableMetric
    total_tokens: NullableMetric
    cached_token_rate: NullableMetric
    cache_hit_request_rate: NullableMetric
    avg_latency_ms: NullableMetric
    ttft_p50_ms: NullableMetric
    ttft_p90_ms: NullableMetric
    ttft_p95_ms: NullableMetric
    output_tps_p50: NullableMetric
    output_tps_p90: NullableMetric
    total_cost_usd: NullableMetric
    total_cost_micros: NullableMetric
}

export interface StatsBucket {
    bucket_start: number
    request_count: number
    input_tokens: NullableMetric
    output_tokens: NullableMetric
    cached_tokens: NullableMetric
    total_tokens: NullableMetric
    avg_latency_ms: NullableMetric
    ttft_ms: NullableMetric
    output_tps: NullableMetric
    cost_usd: NullableMetric
    cost_micros?: NullableMetric
}

export interface StatsBreakdown extends Omit<StatsBucket, 'bucket_start'> {
    key: string
    id: number
    success_count: number
    failure_count: number
    share: number
}

export type ModelStats = StatsBreakdown
export type KeyStats = StatsBreakdown
export type AccountStats = StatsBreakdown
export type UsageOutcome = 'running' | 'succeeded' | 'failed' | 'cancelled' | 'incomplete'

export interface UsageRecord {
    id: number
    request_id: string
    api_key_name: string
    account_name: string
    model: string
    request_kind?: string
    reasoning_effort?: string
    requested_service_tier?: string
    service_tier?: string
    service_priority?: string
    billing_details?: BillingDetails | null
    client_transport: string
    upstream_transport: string
    outcome: UsageOutcome
    status_code: number
    error_code: string
    input_tokens: NullableMetric
    cached_tokens: NullableMetric
    cache_write_tokens?: NullableMetric
    output_tokens: NullableMetric
    reasoning_tokens: NullableMetric
    total_tokens: NullableMetric
    first_token_ms: NullableMetric
    latency_ms: NullableMetric
    output_tps?: NullableMetric
    cost_usd: NullableMetric
    cost_micros?: NullableMetric
    started_at: number
}

export interface BillingRates {
    input: number
    cached_input: number
    cache_write: number
    output: number
}

export interface BillingDetails {
    version: string
    tier: string
    tier_source: string
    price_source: string
    unavailable_reason?: string
    long_context: boolean
    context_threshold: number
    base_rates: BillingRates | null
    context_rates: BillingRates | null
    effective_rates: BillingRates | null
    context_multipliers: BillingRates | null
    tier_multipliers: BillingRates | null
    base_cost_micros: number | null
    context_cost_micros: number | null
    total_cost_micros: number | null
}

export interface UsageQuery extends StatsRange {
    api_key_id?: number
    account_id?: number
    model?: string
    outcome?: UsageOutcome | ''
    search?: string
    limit: number
    offset: number
}

export interface OfficialUsageDay {
    account_id: number
    workspace_id: string
    day: string
    credits: number | null
    uncached_input_tokens: number | null
    cached_input_tokens: number | null
    output_tokens: number | null
    total_tokens: number | null
    users: number
    threads: number
    turns: number
    settled: boolean
    synced_at: number
}

export interface OfficialUsageSync {
    workspace_id: string
    synced_at: number
    attempted_at: number
    error?: string
}

export interface OfficialUsageResponse {
    start_date: string
    end_date: string
    timezone: string
    items: OfficialUsageDay[]
    sync: OfficialUsageSync[]
    total_credits: number | null
    equivalent_usd: number | null
    credits_per_usd: number
    missing_credit_days: number
    pending_days: number
    workspace_count: number
}

export interface CaptureHeader {
    name: string
    value: string
}

export interface CaptureRuleChange {
    path: string
    operation: string
    before_exists?: boolean
    after_exists?: boolean
    before?: unknown
    after?: unknown
    truncated?: boolean
}

export interface CaptureRuleTrace {
    rule_id?: string
    rule_name?: string
    revision?: number
    priority?: number
    phase?: string
    action_id?: string
    action_type?: string
    type?: string
    action_index?: number
    index?: number
    matched?: boolean
    status?: string
    rolled_back?: boolean
    error?: string
    duration_ns?: number
    changes?: CaptureRuleChange[]
    omitted_changes?: number
    event_type?: string
    event_id?: string
    sequence?: unknown
    upstream_seq?: number
    rules_version?: number
    source?: string
    source_kind?: string
    trace_truncated?: boolean
}

export interface CaptureFrame {
    seq: number
    dir: 'out' | 'in' | 'client_in' | 'client_out' | 'internal'
    kind: string
    type?: string
    at_ms: number
    bytes: number
    data?: unknown
    text?: string
    rule_event_id?: string
}

export interface Capture {
    id: number
    account_id: number
    account_name: string
    api_key_id: number
    api_key_name: string
    client_transport: string
    upstream_transport: string
    url: string
    model: string
    session_id: string
    status: number
    outcome: string
    error: string
    duration_ms: number
    ttfb_ms: number
    request_headers: CaptureHeader[]
    request_body: string
    request_bytes: number
    response_headers: CaptureHeader[]
    response_frames: CaptureFrame[]
    rule_traces?: CaptureRuleTrace[]
    rules_version?: number
    rules_trace_truncated?: boolean
    rules_trace_omitted?: number
    response_text: string
    response_id: string
    prompt_tokens: number
    completion_tokens: number
    total_tokens: number
    truncated: boolean
    created_at: number
}

export interface Settings {
    compaction_mode?: 'auto' | 'on' | 'off'
    compaction_model?: string
    compaction_prompt?: string
    switch_on_429?: boolean
    account_cooldown_seconds?: number
    max_attempts?: number
    upstream_transport: string
    capture_enabled: boolean
    capture_limit: number
    default_model: string
    model_mappings: Record<string, string>
    default_strategy: string
    max_concurrent_per_account: number
    refresh_margin_seconds: number
    reasoning_default_effort: string
    reasoning_default_summary: string
    default_proxy_url: string
    user_agent: string
}

export interface StaticConfig {
    upstream_base_url: string
    sse_zstd: boolean
    oauth_callback_port: number
    timezone: string
    capture_persist: boolean
    max_bytes_per_record: number
    database: string
}

export interface SettingsResponse {
    compaction_models?: string[]
    compaction_models_error?: boolean
    current: Settings
    defaults: Settings
    transports: string[]
    strategies: string[]
    static: StaticConfig
}

export type {
    Rule,
    RuleRecord,
    RuleSchema,
    RuleCondition,
    RuleAction,
    JsonValue,
    RuleFieldError,
    RuleList,
    RuleSimulationRequest,
    RuleSimulationResult,
    RuleValidation
} from './rules'

export interface Middleware {
    revision?: number
    compatible?: boolean
    rule_id?: string
    name: string
    description: string
    enabled: boolean
    order_index: number
    config: string
    default_config: string
    configured: boolean
}

// PUT /api/middlewares/{name} returns the persisted database row, not the
// enriched middleware object returned by GET /api/middlewares.
export interface MiddlewareRow {
    name: string
    enabled: boolean
    order_index: number
    config: string
    updated_at: number
}

export interface SavedProxyTest {
    success: boolean
    status?: number
    latency_ms: number
    error?: string
    tested_at: number
}

export interface SavedProxy {
    id: number
    name: string
    url: string
    scheme: string
    account_count: number
    last_test?: SavedProxyTest | null
    created_at: number
    updated_at: number
}

export interface ProxyList {
    items: SavedProxy[]
    total: number
    page: number
    page_size: number
}

export interface ProxyTestResult {
    ok: boolean
    status?: number
    latency_ms?: number
    error?: string
    proxy?: string
}

export interface OAuthFlow {
    id: string
    auth_url: string
    redirect_uri: string
    status: 'pending' | 'exchanging' | 'completed' | 'failed'
    error?: string
    account_id?: string
    email?: string
    plan_type?: string
    // kind distinguishes the model-access login ("chatgpt") from the optional
    // Codex credential link for quota and catalog sync ("codex").
    kind: 'chatgpt' | 'codex'
    created_at: string
    expires_at: string
    callback_available: boolean
}

export interface CaptureStats {
    total: number
    ok: number
    errors: number
    avg_ms: number
    tokens_in: number
    tokens_out: number
}

export interface AuditEvent {
    id: number
    action: string
    detail: string
    created_at: number
}

// ---- quota ----

export interface QuotaWindow {
    limit_id: string
    limit_name?: string
    role: 'primary' | 'secondary'
    // kind is derived from the window duration: 5h, 7d, 30d or other.
    kind: string
    used_percent: number
    reset_at: number
    window_seconds: number
    limit_reached: boolean
}

export interface QuotaCredits {
    has_credits: boolean
    unlimited: boolean
    // balance is a string so large decimals keep full precision.
    balance: string | null
    overage_limit_reached: boolean
}

export interface QuotaSpendControl {
    reached: boolean
}

export interface QuotaReport {
    plan_type: string
    allowed: boolean
    limit_reached: boolean
    windows: QuotaWindow[] | null
    credits?: QuotaCredits
    spend_control?: QuotaSpendControl
    raw?: unknown
    fetched_at: number
    error?: string
}

export interface ResetCredit {
    id: string
    status?: string
    title?: string
    expires_at?: string
    reset_type?: string
}

export interface ResetCredits {
    available_count: number
    credits: ResetCredit[] | null
    fetched_at: number
    error?: string
}

export interface QuotaView {
    cost: AccountUsageCost | null
    window_costs: QuotaWindowCost[]
    report: QuotaReport | null
    reset_credits: ResetCredits | null
    windows: QuotaWindow[] | null
    session: QuotaWindow | null
    weekly: QuotaWindow | null
    updated_at: number
    error?: string
    available: boolean
}

export interface AccountUsageCost {
    cost_micros: number
    priced_requests: number
    unpriced_requests: number
}

export interface QuotaWindowCost {
    limit_id: string
    role: string
    start_at: number
    as_of: number
    usage: AccountUsageCost | null
    estimated_total_usd: number | null
    estimated_remaining_usd: number | null
    unavailable_reason?: string
}

export interface ConsumeResetResult {
    code: string
    credit?: ResetCredit
}

export interface Overview {
    accounts: {
        total: number
        enabled: number
        by_status: Record<string, number>
        expiring_soon: Array<{ id: number; name: string; expires_at: number; status: string }>
    }
    keys: number
    keys_enabled: number
    captures: CaptureStats
    upstream: {
        base_url: string
        transport: string
        websocket_pool: number
        user_agent: string
        originator: string
    }
    settings: Settings
    recent_captures: Capture[] | null
    audit: AuditEvent[] | null
}
