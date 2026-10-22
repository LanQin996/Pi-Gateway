// JSON-only rule language. Persistence fields round-trip unchanged and are read-only in the visual editor.
export type JsonValue = null | boolean | number | string | JsonValue[] | { [key: string]: JsonValue }
export type RulePhase = 'client_request' | 'request_normalize' | 'request' | 'request_finalize' | 'upstream_headers' | 'response_event' | 'response_body'
export type RuleSource = 'current' | 'original' | 'client' | 'context' | 'vars' | 'item'
export type RuleEncoding = 'value' | 'json'

export interface ValueReference {
    source?: RuleSource;
    path?: string;
    encoding?: RuleEncoding
}

export interface RuleCondition {
    op: string
    conditions?: RuleCondition[]
    source?: RuleSource
    path?: string
    encoding?: RuleEncoding
    value?: JsonValue
    pattern?: string
    case_insensitive?: boolean
    dot_all?: boolean
    multiline?: boolean
}

export interface RuleAction {
    id: string;
    type: string;
    params: Record<string, unknown>
}

export interface Rule {
    schema_version: number
    id?: string
    name: string
    description: string
    enabled: boolean
    priority: number
    order_index?: number
    phase: RulePhase
    when: RuleCondition
    actions: RuleAction[]
    stop_after_match: boolean
    on_error: 'abort' | 'skip_rule'
    revision?: number
    created_at?: string
    updated_at?: string
    legacy_name?: string
    source?: string
}

export type RuleRecord = Rule

export interface RuleList {
    rules: RuleRecord[];
    total: number;
    limit: number;
    offset: number;
    version: number
}

export interface RuleFieldError {
    path: string;
    message: string;
    code?: string
}

export interface RuleValidation {
    valid: boolean;
    errors?: RuleFieldError[];
    rule?: Rule
}

export type LocalizedHelp = { en: string; 'zh-CN': string }
export type RuleRenderer =
    'string'
    | 'number'
    | 'boolean'
    | 'enum'
    | 'strings'
    | 'value'
    | 'values'
    | 'condition'
    | 'condition_array'
    | 'action_array'

export interface RuleField {
    name: string
    label?: string
    enum_help?: Record<string,string>
    type: RuleRenderer
    required?: boolean
    default?: unknown
    enum?: string[]
    min?: number
    max?: number
    description: LocalizedHelp
    examples?: unknown[]
    depends_on?: Record<string, unknown>
    non_empty?: boolean
    readonly?: boolean
}

export interface RuleCapability {
    id: string
    label?: string
    phases?: RulePhase[]
    fields: RuleField[]
    description: LocalizedHelp
}

export interface RuleSchema {
    schema_version: number
    phases: RulePhase[]
    sources: RuleSource[]
    encodings: RuleEncoding[]
    rule_fields: RuleField[]
    condition_fields: RuleField[]
    value_fields: RuleField[]
    value_expressions?: RuleCapability[]
    context_fields?: RuleField[]
    conditions: RuleCapability[]
    actions: RuleCapability[]
    examples?: Rule[]
}

export interface RuleSimulationRequest {
    rule: Rule
    phase: RulePhase
    input: {
        body: JsonValue;
        client_body?: JsonValue;
        context?: Record<string, JsonValue>;
        model?: string;
        event_type?: string
    }
}

export interface RuleSimulationResult {
    output?: JsonValue
    matched?: boolean
    changed?: boolean
    blocked?: boolean
    dropped?: boolean
    errors?: RuleFieldError[]
    trace?: unknown

    [key: string]: unknown
}
