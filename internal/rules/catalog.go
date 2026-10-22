package rules

import "encoding/json"

var phases = []string{PhaseClientRequest, PhaseRequestNormalize, PhaseRequest, PhaseRequestFinalize, PhaseUpstreamHeaders, PhaseResponseEvent, PhaseResponseBody}

const (
	MaxDepth                 = 64
	MaxConditionNodes        = 1024
	MaxActionsPerRule        = 256
	MaxPayloadBytes          = 64 << 20
	MaxRuleBytes             = 1 << 20
	MaxTraceChanges          = 128
	MaxTraceValueBytes       = MaxPayloadBytes
	MaxSafeInteger     int64 = 9007199254740991
)

func field(name, typ, label, desc string, def any, required bool, enums ...string) FieldSpec {
	control := typ
	if len(enums) > 0 {
		control = "select"
	}
	f := FieldSpec{Name: name, Type: typ, Label: label, Description: desc, Default: def, Required: required, Enum: enums, Control: control, Help: map[string]string{"zh-CN": desc, "en": desc}}
	f.Examples = fieldExamples(f)
	if len(enums) > 0 {
		f.EnumHelp = map[string]string{}
		for _, option := range enums {
			f.EnumHelp[option] = optionHelp(option)
		}
	}
	return f
}
func requiredString(name, label, desc string) FieldSpec {
	f := field(name, "string", label, desc, nil, true)
	f.NonEmpty = true
	return f
}
func bounded(f FieldSpec, minimum, maximum int64) FieldSpec {
	f.Minimum = &minimum
	f.Maximum = &maximum
	return f
}
func ptr(name string, required bool) FieldSpec {
	return field(name, "pointer", "路径 / Pointer", "严格 JSON Pointer；空字符串选择根；~0 表示 ~，~1 表示 /；数组仅接受规范的非负下标。Strict JSON Pointer.", "", required)
}
func regexFields() []FieldSpec {
	return []FieldSpec{
		field("case_insensitive", "boolean", "忽略大小写 / Ignore case", "Go RE2 (?i)，默认区分大小写。", false, false),
		field("dot_all", "boolean", "点号跨行 / Dot all", "Go RE2 (?s)，使点号匹配换行。", false, false),
		field("multiline", "boolean", "多行锚点 / Multiline", "Go RE2 (?m)，使 ^ 和 $ 匹配每行边界。", false, false),
	}
}
func selectorFields() []FieldSpec {
	return []FieldSpec{
		field("source", "string", sourceFieldLabel, "current 当前载荷；original 阶段原始输入；client 原始请求；context 只读事实；vars 局部变量；item 当前遍历元素。", "current", false, "current", "original", "client", "context", "vars", "item"),
		ptr("path", false),
		field("encoding", "string", "取值方式 / Encoding", "value 保留原 JSON 类型；json 显式序列化为紧凑 JSON 字符串（字符串含引号）。", "value", false, "value", "json"),
	}
}
func actionCapabilities() []Capability {
	req := []string{PhaseClientRequest, PhaseRequestNormalize, PhaseRequest, PhaseRequestFinalize}
	all := phases
	missing := field("on_missing", "string", "缺失路径 / Missing path", "ignore 不操作；error 使整条规则回滚。", "ignore", false, "ignore", "error")
	parents := field("create_parents", "boolean", "创建父对象 / Create parents", "缺失父节点创建为对象，不猜测数组；已有标量父节点始终报错。", false, false)
	value := field("value", "value", valueFieldLabel, "所有 JSON 字面量或 {$ref:{source,path,encoding}}；{$literal:...} 用于转义保留对象键。", nil, true)
	caps := []Capability{
		{ID: "rewrite_model", Label: "修改模型 / Rewrite model", Description: "同时修改载荷 model 和最终路由模型。", Phases: req, Fields: []FieldSpec{requiredString("model", "目标模型 / Model", "非空模型名称，不跳过外层模型权限校验。")}},
		{ID: "drop_environment_context", Label: "清理环境内容 / Strip environment", Description: "兼容配置在保存时展开为底层文本与遍历步骤。", Phases: req, Fields: []FieldSpec{field("content_item_kind", "string", "内容标识 / Content kind", "空字符串使用默认 Codex 环境类型。", "environments.environment_context", false), field("also_strip_from_instructions", "boolean", "清理指令 / Strip instructions", "同时清除 instructions 的 environment_context XML。", true, false)}},
		{ID: "drop_input_items", Label: "删除输入项 / Drop input items", Description: "类型或模式任一匹配就删除整项；空 types 和 pattern 不操作。", Phases: req, Fields: append([]FieldSpec{field("types", "string_array", "输入类型 / Types", "精确匹配输入对象的 type。", []any{}, false), field("pattern", "string", "模式 / Pattern", "Go RE2 正则；空字符串不按模式过滤。", "", false), field("target", "string", "匹配范围 / Target", "json：对象紧凑 JSON、字符串原文（兼容旧行为）；text：字符串或 content/text 的可见文本。", "json", false, "json", "text")}, regexFields()...)},
		{ID: "drop_tools", Label: "删除工具 / Drop tools", Description: "删除最后一个工具后移除 tools 字段；空条件不操作。", Phases: req, Fields: []FieldSpec{field("types", "string_array", "工具类型 / Types", "按 type 排除内置工具，递归处理 namespace，并清理对应 tool_choice。", []any{}, false), field("names", "string_array", "工具名 / Names", "精确匹配工具 name。", []any{}, false), field("drop_all", "boolean", "全部删除 / Drop all", "优先于 names，删除整个 tools 字段。", false, false)}},
		{ID: "passthrough_fields", Label: "透传客户端字段 / Passthrough fields", Description: "仅补入当前缺失且客户端非 null 的顶层字段；外层规范化仍可能剔除字段。", Phases: req, Fields: []FieldSpec{field("fields", "string_array", "字段 / Fields", "顶层字段名称，不是 JSON Pointer；空名称跳过。", []any{}, false)}},
		{ID: "set_reasoning", Label: "推理参数 / Set reasoning", Description: "字符串不硬编码模型枚举，空值不覆盖已有值。", Phases: req, Fields: []FieldSpec{field("effort", "string", "推理强度 / Effort", "例如 low、medium、high；由上游模型决定合法取值。", "", false), field("summary", "string", "推理摘要 / Summary", "例如 auto、concise；空值不覆盖。", "", false)}},
		{ID: "json_set", Label: "设置 JSON / Set JSON", Description: "根节点可替换；数组只能替换现有下标或在长度位置追加，不允许稀疏数组。", Phases: all, Fields: []FieldSpec{ptr("path", true), value, parents, field("if_exists", "string", "已存在 / If exists", "overwrite 替换；keep 保持原值（包括 null）。", "overwrite", false, "overwrite", "keep")}},
		{ID: "json_remove", Label: "删除 JSON / Remove JSON", Description: "按给定顺序删除字段或数组项；禁止删除根。", Phases: all, Fields: []FieldSpec{field("paths", "pointer_array", "删除路径 / Paths", "逐个删除，数组删除会改变后续下标。", []any{}, true), missing}},
		{ID: "json_merge", Label: "合并对象 / Merge objects", Description: "目标和值都必须为对象，缺失目标仅在 create_parents=true 时创建。", Phases: all, Fields: []FieldSpec{ptr("path", true), value, field("mode", "string", "合并方式 / Mode", "shallow 仅顶层；deep 递归合并对象。", "shallow", false, "shallow", "deep"), field("array_mode", "string", "数组方式 / Arrays", "replace 替换；append 按顺序追加，适用于当前合并层。", "replace", false, "replace", "append"), parents}},
		{ID: "json_transfer", Label: "复制或移动 / Transfer JSON", Description: "move 仅允许 current，禁止移动根或移动到自身子树。", Phases: all, Fields: []FieldSpec{field("source", "string", sourceFieldLabel, "来源仅支持 current、client 或 context。", "current", false, "current", "client", "context"), ptr("source_path", true), ptr("target_path", true), field("operation", "string", "操作 / Operation", "copy 深拷贝；move 在成功写入后删除来源。", "copy", false, "copy", "move"), field("overwrite", "boolean", "覆盖 / Overwrite", "false 时目标已存在则报错并回滚。", true, false), parents, missing}},
		{ID: "array_insert", Label: "插入数组 / Insert array", Description: "values 每项独立支持字面量或引用；index 为原数组 0..length。", Phases: all, Fields: []FieldSpec{ptr("path", true), field("values", "value_array", "插入值 / Values", "数组元素使用完整 JSON 值编辑器和引用表达式。", []any{}, true), field("position", "string", "位置 / Position", "prepend 开头；append 末尾；index 指定位置。", "append", false, "prepend", "append", "index"), bounded(field("index", "integer", "下标 / Index", "position=index 时必填；否则不能提供。", nil, false), 0, MaxSafeInteger), field("create_if_missing", "boolean", "缺失时创建 / Create missing", "缺失路径建立数组与对象父路径。", false, false)}},
		{ID: "array_filter", Label: "过滤数组 / Filter array", Description: "item 是原始数组元素，context/item_index 是过滤前下标，剩余元素顺序不变。", Phases: all, Fields: []FieldSpec{ptr("path", true), field("predicate", "condition", "元素条件 / Predicate", "复用完整条件 AST，允许 source=item。", nil, true), field("mode", "string", "保留方式 / Mode", "remove_matches 删除匹配；keep_matches 仅保留匹配。", "remove_matches", false, "remove_matches", "keep_matches"), missing}},
		{ID: "text_replace", Label: "替换文本 / Replace text", Description: "只修改字符串；缺失可忽略，非字符串报错。", Phases: all, Fields: append([]FieldSpec{ptr("path", true), field("match", "string", "匹配方式 / Match", "literal 不展开捕获组；regex 使用 Go RE2 替换语义。", "literal", false, "literal", "regex"), requiredString("pattern", "匹配模式 / Pattern", "不能为空；regex 捕获组示例 (hello) → ${1}!。"), field("replacement", "string", "替换文本 / Replacement", "regex 中 $1 或 ${name} 引用分组，$$ 表示字面量美元符号。", "", false), field("replace_all", "boolean", "全部替换 / Replace all", "false 仅替换第一个匹配。", true, false), missing}, regexFields()...)},
		{ID: "reject_request", Label: "拒绝请求 / Reject request", Description: "终止当前请求，不访问上游；默认 403。", Phases: req, Fields: []FieldSpec{bounded(field("status", "integer", "HTTP 状态 / Status", "仅允许 400..499。", 403, false), 400, 499), field("message", "string", "原因 / Message", "返回客户端的安全拒绝原因，不应包含机密。", "request blocked by rule policy", false)}},
		{ID: "drop_event", Label: "丢弃内容事件 / Drop event", Description: "仅允许 output_text.delta、refusal.delta、reasoning_text.delta、reasoning_summary_text.delta；错误、终态、工具调用及其他事件不可丢弃。", Phases: []string{PhaseResponseEvent}, Fields: []FieldSpec{}},
	}
	for i := range caps {
		for j := range caps[i].Fields {
			if caps[i].ID == "array_insert" && caps[i].Fields[j].Name == "index" {
				caps[i].Fields[j].DependsOn = map[string]any{"position": "index"}
			}
		}
	}
	for i := range caps {
		if isLegacyAction(caps[i].ID) {
			caps[i].Deprecated = true
		}
	}
	return append(caps, flowCapabilities()...)
}
func conditionCapabilities() []Capability {
	out := []Capability{{ID: "test", Label: "计算条件 / Expression test", Description: "表达式结果必须为布尔值；支持动态下标和变量。", Phases: phases, Fields: []FieldSpec{field("value", "value", "表达式 / Expression", "例如 {$expr:{op:eq,args:[1,1]}}；结果为 true 才成立。", nil, true)}}, {ID: "always", Label: "始终 / Always", Description: "显式无条件匹配；不能附加任何字段。", Phases: phases, Fields: []FieldSpec{}}}
	for _, op := range []string{"all", "any", "not"} {
		out = append(out, Capability{ID: op, Label: map[string]string{"all": "全部 / All", "any": "任一 / Any", "not": "取反 / Not"}[op], Description: "all/any 必须非空；not 必须恰好一个子条件。", Phases: phases, Fields: []FieldSpec{field("conditions", "condition_array", "子条件 / Conditions", "递归条件列表，最多 64 层、每条规则最多 1024 条件节点。", nil, true)}})
	}
	labels := map[string]string{"eq": "等于 / Equal", "ne": "不等于 / Not equal", "exists": "存在 / Exists", "not_exists": "不存在 / Missing", "contains": "包含 / Contains", "not_contains": "不包含 / Not contains", "starts_with": "前缀 / Prefix", "ends_with": "后缀 / Suffix", "in": "属于 / In", "not_in": "不属于 / Not in", "regex": "正则匹配 / Regex", "not_regex": "正则不匹配 / Not regex", "gt": "大于 / Greater", "gte": "大于等于 / At least", "lt": "小于 / Less", "lte": "小于等于 / At most", "type": "JSON 类型 / Type"}
	for _, op := range []string{"eq", "ne", "exists", "not_exists", "contains", "not_contains", "starts_with", "ends_with", "in", "not_in", "regex", "not_regex", "gt", "gte", "lt", "lte", "type"} {
		fs := selectorFields()
		if op != "exists" && op != "not_exists" {
			f := field("value", "value", "比较值 / Value", "支持任意 JSON 或引用；缺失不同于 null，除存在性判断外缺失路径始终不匹配。", nil, true)
			if op == "type" {
				f = field("value", "string", "JSON 类型 / Type", "严格 JSON 类型（number 包括整数）；不存在使用 not_exists。", nil, true, "null", "boolean", "number", "string", "array", "object")
			}
			if op == "regex" || op == "not_regex" {
				f = field("value", "string", "正则 / Pattern", "Go RE2 字面量正则；预编译，不允许动态引用。", nil, true)
			}
			fs = append(fs, f)
		}
		if op == "regex" || op == "not_regex" {
			fs = append(fs, regexFields()...)
		}
		out = append(out, Capability{ID: op, Label: labels[op], Description: "严格类型，无隐式类型转换；contains 支持字符串子串、数组元素或对象键；in 的比较值必须是数组。", Phases: phases, Fields: fs})
	}
	return out
}

// Catalog returns a detached schema. The same capability constructors drive validation.
func Catalog() CatalogSpec {
	fields := []FieldSpec{
		bounded(field("schema_version", "integer", "版本 / Version", "新规则使用 2；旧版本 1 自动展开兼容动作。", 2, true), 1, 2), field("id", "string", "规则 ID / ID", "服务端生成稳定 ID，草稿允许为空。", "", false), field("name", "string", "名称 / Name", "名称可重复，非空。", nil, true), field("description", "string", "说明 / Description", "可选说明。", "", false), field("enabled", "boolean", "启用 / Enabled", "关闭的规则仍需通过校验。", true, false), bounded(field("priority", "integer", "优先级 / Priority", "越小越先执行。", 100, false), -2147483648, 2147483647), bounded(field("order_index", "integer", "同优先级顺序 / Order", "服务端管理的稳定顺序；再按 ID 排序。", 0, false), -MaxSafeInteger, MaxSafeInteger), field("phase", "string", "阶段 / Phase", "响应 delta 与最终快照分别执行，不自动同步。", PhaseRequest, true, phases...), field("when", "condition", "条件 / When", "显式使用 always 表示无条件。", nil, true), field("actions", "action_array", "动作 / Actions", "按顺序执行；空数组明确不操作。", []any{}, true), field("stop_after_match", "boolean", "命中后停止 / Stop", "成功执行（包括无变化）后停止当前阶段。", false, false), field("on_error", "string", "错误策略 / On error", "abort 返回错误；skip_rule 原子回滚整条规则后继续。", OnErrorAbort, false, OnErrorAbort, OnErrorSkipRule), bounded(field("revision", "integer", "修订号 / Revision", "服务端乐观并发版本。", 0, false), 0, MaxSafeInteger), field("created_at", "string", "创建时间 / Created", "服务端 RFC3339。", "", false), field("updated_at", "string", "更新时间 / Updated", "服务端 RFC3339。", "", false), field("legacy_name", "string", "迁移来源 / Legacy name", "旧中间件名称，仅用于兼容关联。", "", false), field("source", "string", sourceFieldLabel, "服务端管理的来源元数据，不参与执行。", "", false),
	}
	ctx := []FieldSpec{}
	for _, x := range []struct{ name, typ, desc string }{{"original_model", "string", "规则执行前模型，只读。"}, {"model", "string", "当前模型。"}, {"request_path", "string", "请求路径；由 API 提供。"}, {"request_method", "string", "HTTP 方法；由 API 提供。"}, {"api_key_id", "integer", "鉴权后 API Key ID。"}, {"client_protocol", "string", "客户端协议。"}, {"account_id", "integer", "仅响应阶段可用，request 禁止引用。"}, {"upstream_protocol", "string", "仅响应阶段可用。"}, {"event_type", "string", "仅响应阶段可用。"}, {"item_index", "integer", "遍历元素或数组谓词中的原始下标；删除前下标稳定。"}, {"settings", "value", "当前设置，例如 /settings/default_model、/settings/model_mappings、/settings/user_agent、/settings/timeout_seconds。"}, {"client_headers", "value", "小写请求头名称映射到字符串数组；凭据已移除。例如 /client_headers/user-agent。"}, {"wire_session_id", "string", "客户端显式会话或缓存标识；缺失时为空字符串。"}, {"transport", "string", "仅 upstream_headers：sse 或 websocket。"}, {"credential_account_id", "string", "仅 upstream_headers：已选上游凭据对应的账号标识；不包含访问令牌。"}, {"compact", "boolean", "网关确认的压缩操作事实；发送 schema 在 request_finalize 选择。"}} {
		ctx = append(ctx, field(x.name, x.typ, x.name, x.desc, nil, false))
	}
	spec := CatalogSpec{SchemaVersion: SchemaVersion, Phases: append([]string(nil), phases...), RuleFields: fields, Actions: actionCapabilities(), Conditions: conditionCapabilities(), ValueExpressions: []Capability{
		{ID: "literal", Label: "字面量 / Literal", Description: "任意 JSON：null、布尔、数值、字符串、数组、对象；不隐式解释字符串。整数限定 JS 安全范围。", Phases: phases, Fields: []FieldSpec{field("value", "value", valueFieldLabel, "对象如含 $ref、$expr 或 $literal 键，使用 literal_escape 包装。", nil, true)}},
		{ID: "reference", Label: "引用 / Reference", Description: "序列化为 {$ref:{source,path,encoding}}；不存在的引用在动作中报错回滚，在条件中不匹配。", Phases: phases, Fields: selectorFields()},
		{ID: "literal_escape", Label: "显式字面量 / Literal escape", Description: "序列化为 {$literal:值}，值内部永不求值。", Phases: phases, Fields: []FieldSpec{field("value", "value", valueFieldLabel, "完整保留含表达式保留键的对象。", nil, true)}},
	}, ContextFields: ctx, Examples: catalogExamples(), Limits: map[string]int{"max_execution_steps": MaxExecutionSteps, "max_depth": MaxDepth, "max_condition_nodes_per_rule": MaxConditionNodes, "max_actions_per_rule": MaxActionsPerRule, "max_payload_bytes": MaxPayloadBytes, "max_rule_bytes": MaxRuleBytes, "max_trace_changes_per_action": MaxTraceChanges, "max_trace_value_bytes": MaxTraceValueBytes}}
	spec.Examples = append(spec.Examples, DefaultProfile()...)
	spec.ValueExpressions = append(spec.ValueExpressions, computedCapabilities()...)
	// Also detach nested enum/default/phase slices from the immutable validator schema.
	raw, err := json.Marshal(spec)
	if err != nil {
		panic(err)
	}
	var out CatalogSpec
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}
func catalogExamples() []Rule {
	base := func(id, phase string, when Condition, actions ...Action) Rule {
		return Rule{SchemaVersion: 1, ID: id, Name: id, Enabled: true, Priority: 100, Phase: phase, When: when, Actions: actions, OnError: OnErrorAbort}
	}
	return []Rule{
		base("example-model", PhaseRequest, Condition{Op: "eq", Source: "context", Path: "/model", Value: "old-model"}, Action{ID: "rewrite", Type: "json_set", Params: map[string]any{"path": "/model", "value": "new-model"}}),
		base("example-block", PhaseRequest, Condition{Op: "regex", Source: "current", Encoding: "json", Value: "secret", CaseInsensitive: true}, Action{ID: "reject", Type: "reject_request", Params: map[string]any{"status": 403, "message": "request blocked by rule policy"}}),
		base("example-event", PhaseResponseEvent, Condition{Op: "eq", Source: "context", Path: "/event_type", Value: "response.output_text.delta"}, Action{ID: "replace", Type: "text_replace", Params: map[string]any{"path": "/delta", "match": "regex", "pattern": "(hello)", "replacement": "${1}!"}}),
		base("example-filter", PhaseRequest, Condition{Op: "always"}, Action{ID: "filter", Type: "array_filter", Params: map[string]any{"path": "/input", "predicate": map[string]any{"op": "eq", "source": "item", "path": "/type", "value": "reasoning"}}}),
		base("example-copy", PhaseRequest, Condition{Op: "exists", Source: "client", Path: metadataPointer}, Action{ID: "copy", Type: "json_set", Params: map[string]any{"path": metadataPointer, "value": map[string]any{"$ref": map[string]any{"source": "client", "path": metadataPointer}}}}),
		base("example-tools", PhaseRequest, Condition{Op: "always"}, Action{ID: "drop", Type: "array_filter", Params: map[string]any{"path": "/tools", "predicate": map[string]any{"op": "eq", "source": "item", "path": "/name", "value": "dangerous"}}}),
		base("example-multi", PhaseRequest, Condition{Op: "eq", Source: "context", Path: "/api_key_id", Value: 7}, Action{ID: "set", Type: "json_set", Params: map[string]any{"path": "/metadata/policy", "value": "safe", "create_parents": true}}, Action{ID: "reasoning", Type: "json_set", Params: map[string]any{"path": "/reasoning/effort", "value": "low", "create_parents": true}}),
		base("example-terminal", PhaseRequest, Condition{Op: "always"}, Action{ID: "reject", Type: "reject_request", Params: map[string]any{"status": 403, "message": "policy denied"}}),
		base("example-body", PhaseResponseBody, Condition{Op: "always"}, Action{ID: "replace", Type: "text_replace", Params: map[string]any{"path": "/output_text", "match": "literal", "pattern": "secret", "replacement": "redacted"}}),
	}
}
