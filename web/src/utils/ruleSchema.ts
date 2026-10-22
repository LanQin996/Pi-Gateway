import {editorId} from './editorId'
import type {LocalizedHelp, Rule, RuleCapability, RuleField, RulePhase, RuleRenderer, RuleSchema} from '../api/rules'

export const help = (en: string, zh: string): LocalizedHelp => ({en, 'zh-CN': zh})
const phases: RulePhase[] = ['request', 'response_event', 'response_body']
const field = (name: string, type: RuleRenderer, en: string, zh: string, extra: Partial<RuleField> = {}): RuleField => ({
    name,
    type,
    description: help(en, zh), ...extra
})
const flag = (name: string, en: string, zh: string, value = false) => field(name, 'boolean', en, zh, {default: value})
const choice = (name: string, values: string[], en: string, zh: string) => field(name, 'enum', en, zh, {
    enum: values,
    default: values[0]
})
const pointer = (name = 'path') => field(name, 'string', 'Strict JSON Pointer. Empty selects the root; /input/0 selects an item; ~0 escapes ~ and ~1 escapes /. Missing is not null. Root deletion is prohibited.', '严格 JSON Pointer。空串选根；/input/0 选数组项；~0 转义 ~、~1 转义 /；不存在不同于 null；不允许删除根。', {
    default: '',
    required: true,
    examples: ['/input/0', '/metadata/a~1b']
})
const value = (name = 'value') => field(name, 'value', 'Any JSON literal or {$ref:{source,path,encoding}} reference. Objects containing reserved keys can be wrapped in {$literal:value}.', '任意 JSON 字面量或 {$ref:{source,path,encoding}} 引用。含保留键的对象可用 {$literal:value} 包装。', {
    required: true,
    default: null,
    examples: [{enabled: true}, {
        $ref: {
            source: 'context',
            path: '/model',
            encoding: 'value'
        }
    }, {$literal: {$ref: 'literal object key'}}]
})
const source = () => choice('source', ['current', 'client', 'context', 'item'], 'current: previous rules output; client: original read-only JSON; context: facts; item: current array element (predicates only). Moving from read-only sources is forbidden.', 'current：前序规则输出；client：只读原始 JSON；context：事实；item：仅数组谓词内当前项。禁止从只读来源移动。')
const encoding = () => choice('encoding', ['value', 'json'], 'value preserves the JSON type; json explicitly serializes the entire selected value to text. Use json to regex-match an object.', 'value 保持 JSON 类型；json 明确序列化选定值为文本。对整段对象做正则匹配时使用 json。')
const missing = () => choice('on_missing', ['ignore', 'error'], 'ignore leaves a missing path unchanged; error fails the whole rule (rollback follows on_error). null is present, not missing.', 'ignore 对不存在路径不操作；error 使整条规则失败（按 on_error 回滚）。null 是存在值，不是缺失。')
const parents = () => flag('create_parents', 'Create missing object parents. Existing incompatible parent types are errors; this does not bypass protected-field checks.', '创建缺失的父对象；已存在但类型不兼容的父节点报错；不会绕过受保护字段检查。')
const regex = () => [flag('case_insensitive', 'Go RE2 case-insensitive matching. Default false; migrated legacy expressions explicitly retain their flags.', 'Go RE2 忽略大小写。默认关闭；旧规则迁移会明确保留旧标志。'), flag('dot_all', 'Allow . to match newlines (Go RE2 s flag). Default false.', '允许点号匹配换行（Go RE2 s 标志），默认关闭。'), flag('multiline', 'Make ^ and $ match line boundaries (Go RE2 m flag). Default false.', '使 ^ 与 $ 匹配每行边界（Go RE2 m 标志），默认关闭。')]
const strings = (name: string, en: string, zh: string) => field(name, 'strings', en, zh, {default: []})
const capability = (id: string, en: string, zh: string, fields: RuleField[], allowed = phases): RuleCapability => ({
    id,
    description: help(en, zh),
    fields,
    phases: allowed
})

export const actionCapabilities: RuleCapability[] = [
    capability('rewrite_model', 'Set the final routed model and body.model together. Model restrictions remain enforced.', '同时改写路由模型与 body.model，仍受模型限制检查。', [field('model', 'string', 'Non-empty target model ID.', '非空目标模型 ID。', {
        required: true,
        default: 'gpt-5',
        examples: ['gpt-5']
    })], ['request']),
    capability('drop_environment_context', 'Remove environment XML/metadata with the existing gateway stripping semantics.', '按网关既有语义清理环境 XML／元数据。', [field('content_item_kind', 'string', 'Input content item kind to clean; normally input_text.', '需要清理的内容项类型，通常为 input_text。', {default: 'input_text'}), flag('also_strip_from_instructions', 'Also strip environment content from instructions.', '同时从 instructions 清理环境内容。')], ['request']),
    capability('drop_input_items', 'Drop whole input items matching any listed type or pattern. Both empty means no-op.', '类型或模式任一匹配即删除整个输入项；两者都空则不操作。', [strings('types', 'Input item types. Empty list performs no type filtering.', '输入项类型，空列表不按类型过滤。'), field('pattern', 'string', 'Go RE2 pattern; empty means no pattern filter.', 'Go RE2 模式，空串不按模式过滤。', {default: ''}), choice('target', ['json', 'text'], 'json serializes the whole item; text matches its textual content.', 'json 匹配整项序列化 JSON；text 匹配文本内容。'), ...regex()], ['request']),
    capability('drop_tools', 'Remove named tools; removing the final tool removes the tools field.', '删除指定工具；最后一个工具被删除时移除 tools 字段。', [strings('types', 'Exact tool types, including built-ins. Recurses through namespaces and clears excluded tool choices.', '精确工具类型，包括内置工具；递归处理 namespace 并清理对应工具选择。'), strings('names', 'Exact tool names; empty is no-op unless types or drop_all is set.', '精确工具名；空列表不操作，除非 drop_all 开启。'), flag('drop_all', 'Remove every tool, regardless of names.', '删除全部工具，忽略 names 列表。')], ['request']),
    capability('passthrough_fields', 'Copy only missing, non-null fields from original client JSON. Gateway normalization may still remove prohibited fields.', '仅从客户端原始 JSON 补入缺失且非 null 字段；网关规范化仍可能剔除不允许的字段。', [strings('fields', 'Top-level field names, not JSON Pointers. Empty means no-op.', '顶层字段名（不是 JSON Pointer）；空列表不操作。')], ['request']),
    capability('set_reasoning', 'Override reasoning strings without treating the current model catalogue as an exhaustive enum.', '覆盖推理参数字符串，不将当前模型目录硬编码为完整枚举。', [field('effort', 'string', 'Reasoning effort string; empty does not override.', '推理强度字符串；空串不覆盖。', {default: ''}), field('summary', 'string', 'Reasoning summary string; empty does not override.', '推理摘要字符串；空串不覆盖。', {default: ''})], ['request']),
    capability('json_set', 'Set a JSON value. Protected identity, auth and usage fields remain immutable.', '设置 JSON 值，身份、鉴权与计量等受保护字段仍不可修改。', [pointer(), value(), parents(), choice('if_exists', ['overwrite', 'keep'], 'overwrite replaces an existing value; keep preserves it (including null).', 'overwrite 替换已有值；keep 保留已有值（含 null）。')]),
    capability('json_remove', 'Delete nested object fields or array elements. The root cannot be deleted.', '删除嵌套对象字段或数组项，禁止删除根。', [strings('paths', 'JSON Pointers processed in listed order; array removal changes subsequent indices.', '按列表顺序处理 JSON Pointer；删除数组项后后续索引会变化。'), missing()]),
    capability('json_merge', 'Merge an object into the selected object. Both resolved value and target must be objects.', '将对象合并到所选对象；解析后的值与目标都必须是对象。', [pointer(), value(), choice('mode', ['shallow', 'deep'], 'shallow replaces top-level keys; deep recursively merges objects.', 'shallow 覆盖顶层键；deep 递归合并对象。'), choice('array_mode', ['replace', 'append'], 'replace replaces arrays; append concatenates them in original order.', 'replace 替换数组；append 保持顺序追加。'), parents()]),
    capability('json_transfer', 'Copy a value from a source, or move within current payload only.', '从来源复制值；移动仅允许发生在当前载荷内部。', [source(), pointer('source_path'), pointer('target_path'), choice('operation', ['copy', 'move'], 'copy leaves the source intact; move removes it. Cannot move a node into its own descendant.', 'copy 保留来源；move 删除来源；禁止将节点移动到其子节点。'), flag('overwrite', 'Allow replacing an existing target.', '允许覆盖已存在的目标。'), parents(), missing()]),
    capability('array_insert', 'Insert resolved JSON values without reordering existing elements.', '插入解析后的 JSON 值，不改变既有元素相对顺序。', [pointer(), field('values', 'values', 'Ordered JSON literals/references to insert. Empty list is no-op.', '按顺序插入的 JSON 字面量／引用；空列表不操作。', {
        default: [],
        required: true
    }), choice('position', ['prepend', 'append', 'index'], 'prepend/append select endpoints; index selects a zero-based insertion slot.', 'prepend／append 选两端；index 指定从 0 开始的插入位置。'), field('index', 'number', 'Required only for position=index. Integer from 0 through array length.', '仅 position=index 时使用；整数范围为 0 至数组长度。', {
        min: 0,
        default: 0
    }), flag('create_if_missing', 'Create an empty target array when missing; existing non-arrays still fail.', '路径不存在时创建空数组；已存在的非数组仍报错。')]),
    capability('array_filter', 'Evaluate a full condition per item and keep remaining items in original order.', '对每个元素运行完整条件，保持保留元素的相对顺序。', [pointer(), field('predicate', 'condition', 'Recursive condition with item source and context /item_index (original index).', '支持 item 来源和 context /item_index（原始索引）的递归条件。', {
        default: {op: 'always'},
        required: true
    }), choice('mode', ['remove_matches', 'keep_matches'], 'Remove matching elements or keep only matching elements.', '删除命中元素，或仅保留命中元素。'), missing()]),
    capability('text_replace', 'Replace text at a string target. Non-strings fail; serialization is not implicit.', '替换字符串目标文本，非字符串报错，不做隐式序列化。', [pointer(), choice('match', ['literal', 'regex'], 'literal matches exact text; regex uses Go RE2, not browser RegExp.', 'literal 精确匹配文本；regex 使用 Go RE2，而不是浏览器 RegExp。'), field('pattern', 'string', 'Text or Go RE2 pattern. No backreferences or lookbehind.', '文本或 Go RE2 模式；不支持反向引用或后顾。', {
        required: true,
        default: ''
    }), field('replacement', 'string', 'Replacement text; regex captures: $1 or ${name}; $$ inserts $.', '替换文本；正则捕获组：$1 或 ${name}；$$ 插入 $。', {
        required: true,
        default: '',
        examples: ['${1}-redacted']
    }), flag('replace_all', 'Replace all matches; false replaces only the first.', '替换全部匹配；关闭时仅替换首个。', true), ...regex(), missing()]),
    capability('reject_request', 'Block this request before upstream/account selection, with a retained no-account trace.', '在选择账号／访问上游前阻断请求，并保留无账号规则追踪。', [field('status', 'number', 'HTTP error status in the allowed 4xx range; default 403.', '允许的 4xx HTTP 状态码，默认 403。', {
        min: 400,
        max: 499,
        default: 403
    }), field('message', 'string', 'Safe client-facing rejection reason; do not include secrets.', '对客户端可见的安全拒绝原因，不要包含秘密。', {default: 'Blocked by rule'})], ['request']),
    capability('drop_event', 'Drop ordinary non-terminal content events only. Errors, terminal events and tool-call correlation must remain intact.', '仅丢弃普通非终态内容事件；禁止丢弃错误、终态或破坏工具调用关联。', [], ['response_event']),
]
const compareFields = () => [source(), pointer(), encoding()]
const groupDescriptions = {
    all: ['Every child must match; non-empty group.', '全部子条件必须匹配；组不能为空。'],
    any: ['At least one child must match; non-empty group.', '至少一个子条件匹配；组不能为空。'],
    not: ['Negate exactly one child condition.', '对恰好一个子条件取反。'],
}

function comparisonFields(id: string): RuleField[] {
    const fields = compareFields()
    if (['exists', 'not_exists'].includes(id)) return fields
    if (id === 'type') return [...fields, field('value', 'enum', 'JSON type; missing values require not_exists.', 'JSON 类型；缺失值请使用 not_exists。', {
        required: true, enum: ['null', 'boolean', 'number', 'string', 'array', 'object'], default: 'string',
    })]
    if (['regex', 'not_regex'].includes(id)) return [...fields, field('value', 'string', 'Static Go RE2 expression; dynamic references are prohibited.', '静态 Go RE2 正则，不允许动态引用。', {
        required: true, default: '',
    }), ...regex()]
    return [...fields, value()]
}

export const conditionCapabilities: RuleCapability[] = [
    capability('always', 'Always match. Use this explicitly instead of an empty group.', '始终匹配，应明确使用此类型，不依赖空组。', []),
    ...Object.entries(groupDescriptions).map(([op, [en, zh]]) => capability(op, en!, zh!, [])),
    ...[
        ['eq', 'Equal JSON values (types are significant)', 'JSON 值相等（区分类型）'], ['ne', 'Not equal', 'JSON 值不相等'],
        ['exists', 'Path exists, including explicit null', '路径存在（含显式 null）'], ['not_exists', 'Path is missing; null does not match', '路径不存在（null 不匹配）'],
        ['contains', 'Contains a substring or array member', '包含子字符串或数组元素'], ['not_contains', 'Does not contain', '不包含'],
        ['starts_with', 'String starts with', '字符串前缀'], ['ends_with', 'String ends with', '字符串后缀'],
        ['in', 'Selected value belongs to the supplied set', '所选值属于给定集合'], ['not_in', 'Selected value does not belong to the set', '所选值不属于集合'],
        ['regex', 'Go RE2 pattern matches a string (use json encoding for objects)', 'Go RE2 匹配字符串（对象请使用 json 编码）'], ['not_regex', 'Go RE2 pattern does not match', 'Go RE2 不匹配'],
        ['gt', 'Numeric greater than', '数值大于'], ['gte', 'Numeric greater than or equal', '数值大于等于'], ['lt', 'Numeric less than', '数值小于'], ['lte', 'Numeric less than or equal', '数值小于等于'],
        ['type', 'JSON type is null, boolean, number, string, array or object', 'JSON 类型为 null、boolean、number、string、array 或 object'],
    ].map(([id, en, zh]) => capability(id!, en!, zh!, comparisonFields(id!))),
]
export const ruleSchema: RuleSchema = {
    schema_version: 1, phases, sources: ['current', 'client', 'context', 'item'], encodings: ['value', 'json'],
    rule_fields: [field('schema_version', 'number', 'Language version; currently exactly 1. Unknown versions cannot be edited.', '规则语言版本，当前仅支持 1；拒绝未知版本。', {
        required: true,
        default: 1,
        min: 1,
        max: 1
    }), field('name', 'string', 'Human-readable name; duplicate names are allowed. ID is server-managed.', '可读名称，可重名；ID 由服务端管理。', {
        required: true,
        default: ''
    }), field('description', 'string', 'Optional explanation; empty is valid.', '规则说明，可为空。', {default: ''}), flag('enabled', 'Disabled rules remain stored but are not executed.', '停用规则仍保留但不执行。', true), field('priority', 'number', 'Lower values run earlier within a phase; ties use stable persisted order. Use adjacent move for actual reordering.', '每阶段内数值越小越先执行；相同值采用持久化稳定顺序；相邻移动交换真实顺序。', {
        required: true,
        default: 0,
        min: -2147483648,
        max: 2147483647
    }), choice('phase', phases, 'request: before routing; response_event: each deliverable event; response_body: non-streaming final JSON only.', 'request：路由前；response_event：每个可交付事件；response_body：仅非流式最终 JSON。'), field('when', 'condition', 'Recursive match expression. Reads output from earlier successful rules.', '递归匹配表达式，读取前序成功规则输出。', {required: true}), field('actions', 'values', 'Ordered action records with stable IDs. Empty means intentional no-op.', '带稳定 ID 的有序动作记录；空列表明确不操作。', {
        required: true,
        default: []
    }), flag('stop_after_match', 'Stop only this phase after successful matching execution.', '成功命中并执行后，仅停止当前阶段。'), choice('on_error', ['abort', 'skip_rule'], 'abort stops execution; skip_rule atomically rolls back every action in this rule.', 'abort 中止执行；skip_rule 原子回滚本规则全部动作。')],
    condition_fields: [field('op', 'enum', 'Recursive boolean/comparison operator.', '递归布尔／比较操作。', {enum: conditionCapabilities.map(c => c.id)}), field('conditions', 'condition', 'Ordered nested conditions; not requires exactly one.', '有序子条件，not 必须恰好一个。'), ...compareFields(), value(), ...regex()],
    value_fields: [field('value', 'value', 'Any literal JSON value including null; $literal escapes reserved object keys.', '任意 JSON 字面量，包括 null；$literal 转义保留对象键。'), source(), pointer(), encoding()],
    conditions: conditionCapabilities, actions: actionCapabilities,
}

// Mirror the wire defaults; the live server schema supplies authoritative constraints.
for (const cap of conditionCapabilities) {
    for (const f of cap.fields) if (['source', 'path', 'encoding'].includes(f.name)) f.required = false
    if (['all', 'any', 'not'].includes(cap.id)) cap.fields.push(field('conditions', 'condition', 'Ordered recursive children; all/any non-empty, not exactly one.', '有序递归子条件；all／any 非空，not 恰好一个。', {required: true}))
}
for (const cap of actionCapabilities) for (const f of cap.fields) {
    if (cap.id === 'drop_environment_context' && f.name === 'content_item_kind') {
        f.default = 'environments.environment_context';
        f.description = help('Environment content kind; empty uses environments.environment_context.', '环境内容标识；空串使用 environments.environment_context。')
    }
    if (cap.id === 'drop_environment_context' && f.name === 'also_strip_from_instructions') f.default = true
    if (cap.id === 'json_transfer' && f.name === 'source') f.enum = ['current', 'client', 'context']
    if (cap.id === 'json_transfer' && f.name === 'overwrite') f.default = true
    if (cap.id === 'json_remove' && f.name === 'paths') f.required = true
    if (cap.id === 'array_insert' && f.name === 'position') f.default = 'append'
    if (cap.id === 'array_insert' && f.name === 'index') {
        delete f.default;
        f.max = Number.MAX_SAFE_INTEGER;
        f.depends_on = {position: 'index'}
    }
    if (cap.id === 'text_replace' && f.name === 'replacement') f.required = false
    if ((cap.id === 'text_replace' && f.name === 'pattern') || (cap.id === 'rewrite_model' && f.name === 'model')) f.non_empty = true
    if (cap.id === 'reject_request' && f.name === 'message') f.default = 'request blocked by rule policy'
}
ruleSchema.rule_fields.find(f => f.name === 'name')!.non_empty = true
ruleSchema.rule_fields.find(f => f.name === 'priority')!.default = 100

export const rendererKinds: RuleRenderer[] = ['string', 'number', 'boolean', 'enum', 'strings', 'value', 'values', 'condition']

export function localHelp(value: LocalizedHelp | string | undefined, locale: string): string {
    if (typeof value === 'string') return value
    return value?.[locale === 'zh-CN' ? 'zh-CN' : 'en'] ?? ''
}

export function defaultForField(f: RuleField): unknown {
    // Catalog values can be Vue proxies when rendered inside reactive drafts.
    const copy = (value: unknown) => JSON.parse(JSON.stringify(value))
    if (f.non_empty && (f.default === undefined || f.default === '') && f.examples?.length) return copy(f.examples[0])
    if (f.default !== undefined) return copy(f.default)
    switch (f.type) {
        case 'boolean': return false
        case 'number': return 0
        case 'strings':
        case 'values':
        case 'action_array':
        case 'condition_array': return []
        case 'value': return null
        case 'condition': return {op: 'always'}
        default: return f.enum?.[0] ?? ''
    }
}

export function newRule(): Rule {
    return {
        schema_version: 2,
        name: '',
        description: '',
        enabled: true,
        priority: 100,
        phase: 'request',
        when: {op: 'always'},
        actions: [],
        stop_after_match: false,
        on_error: 'abort'
    }
}

export function newAction(type: string, schema = ruleSchema) {
    const cap = schema.actions.find(c => c.id === type);
    return {
        id: editorId(),
        type,
        params: Object.fromEntries((cap?.fields ?? []).filter(f => f.required || f.default !== undefined).map(f => [f.name, defaultForField(f)]))
    }
}
