import {cloneJson} from './cloneJson'
import type {JsonValue, Rule, RuleCondition, RuleField, RuleFieldError} from '../api/rules'
import {defaultForField, ruleSchema} from './ruleSchema'
import {graphSignature, restoreGraphLayout, type RuleGraph, ruleToGraph} from './ruleGraph'

export const clone = cloneJson

export interface RuleDraft {
    rule: Rule;
    code: string;
    mode: 'steps' | 'visual' | 'code';
    base: string;
    revision?: number;
    errors: RuleFieldError[];
    isNew?: boolean;
    graph?: RuleGraph;
    graphBase?: string;
    graphUndo?: RuleGraph[];
    graphRedo?: RuleGraph[]
}

export function createDraft(rule: Rule, isNew = false): RuleDraft {
    const code = stringifyRule(rule);
    const graph = restoreGraphLayout(ruleToGraph(rule))
    return {
        rule: clone(rule),
        code,
        mode: 'steps',
        base: code,
        revision: rule.revision,
        errors: [],
        isNew,
        graph,
        graphBase: graphSignature(graph),
        graphUndo: [],
        graphRedo: []
    }
}

export function draftDirty(draft: RuleDraft): boolean {
    return !!draft.isNew || (draft.mode === 'code' ? draft.code : stringifyRule(draft.rule)) !== draft.base || !!(draft.graph && draft.graphBase && graphSignature(draft.graph) !== draft.graphBase)
}

export const pointerPart = (key: string | number) => String(key).replaceAll('~', '~0').replaceAll('/', '~1')
export const atPath = (base: string, key: string | number) => `${base}/${pointerPart(key)}`

export function safeParseJSON(text: string): JsonValue {
    // Validate structure and numeric lexemes before JSON.parse can discard duplicate keys
    // or round unsafe integers. This is JSON parsing only, never expression evaluation.
    let offset = 0
    const whitespace = () => {
        while (/\s/.test(text[offset] ?? '') && offset < text.length) offset++
    }
    const fail = (message: string, path: string): never => {
        throw new RuleInputError([{path, message: `${message} at character ${offset}`}])
    }

    function string(path: string): string {
        const start = offset++
        let escaped = false
        while (offset < text.length) {
            const c = text[offset++]
            if (escaped) escaped = false
            else if (c === '\\') escaped = true
            else if (c === '"') {
                try {
                    return JSON.parse(text.slice(start, offset)) as string
                } catch {
                    return fail('Invalid JSON string', path)
                }
            }
        }
        return fail('Unterminated string', path)
    }

    function value(path: string, depth: number) {
        if (depth > 208) fail('JSON nesting budget exceeded', path)
        whitespace()
        const c = text[offset]
        if (c === '"') {
            string(path);
            return
        }
        if (c === '{' || c === '[') {
            container(path, depth, c)
            return
        }
        const number = /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/.exec(text.slice(offset))
        if (number) {
            const parsed = Number(number[0])
            if (!Number.isFinite(parsed) || Number.isInteger(parsed) && !Number.isSafeInteger(parsed)) fail('Number must be finite; integers must be safe', path)
            offset += number[0].length;
            return
        }
        for (const literal of ['true', 'false', 'null']) if (text.startsWith(literal, offset)) {
            offset += literal.length;
            return
        }
        fail('Expected JSON value', path)
    }

    function container(path: string, depth: number, c: string): void {
        const object = c === '{', close = object ? '}' : ']'
        const seen = new Set<string>()
        offset++;
        whitespace()
        if (text[offset] === close) {
            offset++;
            return
        }
        let index = 0
        for (; ;) {
            whitespace()
            let child = atPath(path, index++)
            if (object) child = objectKey(path, seen)
            value(child, depth + 1);
            whitespace()
            if (text[offset] === close) {
                offset++;
                return
            }
            if (text[offset++] !== ',') fail('Expected comma or closing delimiter', path)
        }
    }

    function objectKey(path: string, seen: Set<string>): string {
        if (text[offset] !== '"') fail('Expected object key', path)
        const key = string(path);
        const child = atPath(path, key)
        if (seen.has(key)) fail('Duplicate object key', child)
        seen.add(key);
        whitespace()
        if (text[offset++] !== ':') fail('Expected colon', child)
        return child
    }

    value('', 0);
    whitespace()
    if (offset !== text.length) fail('Unexpected trailing content', '')
    return JSON.parse(text) as JsonValue
}

export function stringifyRule(rule: Rule): string {
    return JSON.stringify(rule, null, 2)
}

export function parseRule(text: string, schema = ruleSchema): Rule {
    const raw = safeParseJSON(text)
    const errors = validateRule(raw, schema)
    if (errors.length) throw new RuleInputError(errors)
    return raw as unknown as Rule
}

export class RuleInputError extends Error {
    constructor(public errors: RuleFieldError[]) {
        super(errors.map(e => `${e.path || '/'}: ${e.message}`).join('\n'))
    }
}

const object = (v: unknown): v is Record<string, unknown> => !!v && typeof v === 'object' && !Array.isArray(v)

export function validateRule(value: unknown, schema = ruleSchema): RuleFieldError[] {
    const errors: RuleFieldError[] = []
    const error = (path: string, message: string) => errors.push({path, message})

    function keys(v: Record<string, unknown>, allowed: string[], path: string) {
        for (const key of Object.keys(v)) if (!allowed.includes(key)) error(atPath(path, key), 'Unknown field; cannot silently discard it')
    }

    function json(v: unknown, path: string) {
        if (typeof v === 'number' && (!Number.isFinite(v) || Number.isInteger(v) && !Number.isSafeInteger(v))) error(path, 'Number must be finite; integers must be safe')
        else if (Array.isArray(v)) v.forEach((x, i) => json(x, atPath(path, i)))
        else if (object(v)) Object.entries(v).forEach(([k, x]) => json(x, atPath(path, k)))
        else if (v !== null && !['number', 'boolean', 'string'].includes(typeof v)) error(path, 'Expected JSON value')
    }

    function pointer(v: unknown, path: string) {
        if (typeof v !== 'string' || v !== '' && !v.startsWith('/') || typeof v === 'string' && /~(?:[^01]|$)/.test(v)) error(path, 'Expected strict JSON Pointer (empty root, /segment, ~0, ~1)')
    }

    function expression(v: unknown, path: string) {
        json(v, path)
        if (!object(v)) return
        if (Object.hasOwn(v, '$expr')) {
            if (Object.keys(v).length !== 1 || !object(v.$expr)) {error(path, '$expr must be the only key and contain an object'); return }
            keys(v.$expr, ['op', 'args'], `${path}/$expr`)
            if (typeof v.$expr.op !== 'string' || !Array.isArray(v.$expr.args)) {error(path, 'Expression requires op and args'); return }
            v.$expr.args.forEach((arg, i) => expression(arg, `${path}/$expr/args/${i}`))
        } else if (Object.hasOwn(v, '$ref')) {
            if (Object.keys(v).length !== 1 || !object(v.$ref)) {
                error(path, '$ref must be the only key and contain an object');
                return
            }
            referenceExpression(v.$ref, path)
        } else if (Object.hasOwn(v, '$literal')) {
            if (Object.keys(v).length !== 1) error(path, '$literal must be the only key')
            else json(v.$literal, `${path}/$literal`)
        }
    }

    function referenceExpression(reference: Record<string, unknown>, path: string) {
        keys(reference, ['source', 'path', 'encoding'], `${path}/$ref`)
        if (reference.source !== undefined && !schema.sources.includes(reference.source as never)) error(`${path}/$ref/source`, 'Unknown source')
        if (reference.path !== undefined) pointer(reference.path, `${path}/$ref/path`)
        if (reference.encoding !== undefined && !schema.encodings.includes(reference.encoding as never)) error(`${path}/$ref/encoding`, 'Unknown encoding')
    }

    function field(v: unknown, f: RuleField, path: string) {
        if (v === undefined) {
            if (f.required) error(path, 'Required field');
            return
        }
        if (f.enum && (typeof v !== 'string' || !f.enum.includes(v))) error(path, 'Unknown option')
        if (f.non_empty && (typeof v !== 'string' || !v.trim())) error(path, 'Must not be empty')
        fieldType(v, f, path)
        if (['path', 'source_path', 'target_path'].includes(f.name)) pointer(v, path)
    }

    function numberField(v: unknown, f: RuleField, path: string) {
        const bounds = [f.min === undefined ? '' : ' ≥ ' + f.min, f.max === undefined ? '' : ' ≤ ' + f.max].join('')
        if (typeof v !== 'number' || !Number.isSafeInteger(v) || f.min !== undefined && v < f.min || f.max !== undefined && v > f.max) error(path, 'Expected safe integer' + bounds)
    }

    function stringsField(v: unknown, f: RuleField, path: string) {
        if (!Array.isArray(v) || v.some(x => typeof x !== 'string')) error(path, 'Expected string array')
        if (f.name === 'paths' && Array.isArray(v)) v.forEach((x, i) => pointer(x, atPath(path, i)))
    }

    function fieldType(v: unknown, f: RuleField, path: string) {
        switch (f.type) {
            case 'value': expression(v, path); break
            case 'values':
                if (!Array.isArray(v)) error(path, 'Expected array')
                else v.forEach((x, i) => expression(x, atPath(path, i)))
                break
            case 'condition': condition(v, path); break
            case 'action_array': actions(v, path); break
            case 'strings': stringsField(v, f, path); break
            case 'number': numberField(v, f, path); break
            case 'boolean':
                if (typeof v !== 'boolean') error(path, 'Expected boolean')
                break
            default:
                if (typeof v !== 'string') error(path, `Expected ${f.type}`)
        }
    }

    function condition(v: unknown, path: string) {
        if (!object(v)) {
            error(path, 'Expected condition');
            return
        }
        const cap = schema.conditions.find(c => c.id === v.op)
        if (!cap) {
            error(atPath(path, 'op'), 'Unknown condition operator');
            return
        }
        if (['all', 'any', 'not'].includes(String(v.op))) {
            keys(v, ['op', 'conditions'], path)
            if (!Array.isArray(v.conditions) || !v.conditions.length || v.op === 'not' && v.conditions.length !== 1) error(atPath(path, 'conditions'), v.op === 'not' ? 'not requires exactly one child' : 'Group must not be empty')
            else v.conditions.forEach((c, i) => condition(c, atPath(atPath(path, 'conditions'), i)))
        } else {
            keys(v, ['op', ...cap.fields.map(f => f.name)], path)
            cap.fields.forEach(f => field(v[f.name], f, atPath(path, f.name)))
        }
    }

    if (!object(value)) return [{path: '', message: 'Expected rule object'}]
    keys(value, [...schema.rule_fields.map(f => f.name), 'id', 'revision', 'order_index', 'created_at', 'updated_at', 'legacy_name', 'source'], '')
    if (![1, schema.schema_version].includes(Number(value.schema_version))) error('/schema_version', 'Unsupported schema version')
    for (const f of schema.rule_fields) if (f.name !== 'actions') field(value[f.name], f, atPath('', f.name))
    const phase = value.phase
    function actions(list: unknown, base: string) {
        if (!Array.isArray(list)) error(base, 'Expected ordered action array')
        else {
            const ids = new Set<string>()
            list.forEach((a, i) => {
                const path = `${base}/${i}`
                if (!object(a)) {
                    error(path, 'Expected action');
                    return
                }
                keys(a, ['id', 'type', 'params'], path)
                if (typeof a.id !== 'string' || !a.id || ids.has(a.id)) error(`${path}/id`, 'Action ID must be non-empty and unique')
                else ids.add(a.id)
                const cap = schema.actions.find(c => c.id === a.type)
                if (!cap) {
                    error(`${path}/type`, 'Unknown action type');
                    return
                }
                if (!cap.phases?.includes(phase as never)) error(`${path}/type`, 'Action not supported in this phase')
                if (!object(a.params)) {
                    error(`${path}/params`, 'Expected parameter object');
                    return
                }
                keys(a.params, cap.fields.map(f => f.name), `${path}/params`)
                cap.fields.forEach(f => field((a.params as Record<string, unknown>)[f.name], f, `${path}/params/${f.name}`))
            })
        }
    }
    actions(value.actions, "/actions")
    return errors
}

export function conditionSummary(c: RuleCondition): string {
    if (['all', 'any', 'not'].includes(c.op)) return `${c.op}(${c.conditions?.map(conditionSummary).join(', ') ?? ''})`
    const suffix = c.source ? ` ${c.source}${c.path ?? ''}` : ''
    return c.op + suffix
}

export function defaultCondition(op: string, schema = ruleSchema): RuleCondition {
    if (['all', 'any', 'not'].includes(op)) return {op, conditions: [{op: 'always'}]}
    const fields = schema.conditions.find(c => c.id === op)?.fields ?? []
    return {op, ...Object.fromEntries(fields.filter(f => f.required || f.default !== undefined).map(f => [f.name, defaultForField(f)]))}
}

export function setObjectProperty(value: JsonValue, oldKey: string | null, newKey: string, next: JsonValue): JsonValue {
    const entries = object(value) ? Object.entries(value) : []
    if (newKey !== oldKey && entries.some(([key]) => key === newKey)) throw new Error('Duplicate property')
    return Object.fromEntries(oldKey === null ? [...entries, [newKey, next]] : entries.map(([key, val]) => key === oldKey ? [newKey, next] : [key, val])) as JsonValue
}

export function moveItem<T>(items: T[], index: number, delta: number): T[] {
    const target = index + delta;
    if (target < 0 || target >= items.length) return [...items];
    const next = [...items];
    [next[index], next[target]] = [next[target]!, next[index]!];
    return next
}

export function expressionLiteral(value: JsonValue): JsonValue {
    return value && typeof value === 'object' && !Array.isArray(value) && ('$ref' in value || '$literal' in value || '$expr' in value) ? {$literal: value} : value
}
