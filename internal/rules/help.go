package rules

import "sort"

func fieldExamples(f FieldSpec) []any {
	if len(f.Enum) > 0 {
		out := []any{}
		for _, v := range f.Enum {
			out = append(out, v)
		}
		return out
	}
	switch f.Type {
	case "pointer":
		return []any{"/input/0/content/0/text", "/metadata/a~1b"}
	case "pointer_array":
		return []any{[]any{"/metadata/private", "/input/0"}}
	case "boolean":
		return []any{true, false}
	case "integer":
		n := int64(0)
		if f.Minimum != nil {
			n = *f.Minimum
		}
		return []any{n}
	case "value":
		if f.Name == "functions" {
			return []any{map[string]any{"filter": []any{map[string]any{"id": "remove-private", "type": "json_remove", "params": map[string]any{"paths": []any{"/metadata/private"}}}}}}
		}
		if f.Name == "value" && f.Label == "表达式 / Expression" {
			return []any{expr("eq", 1, 1)}
		}
		return []any{map[string]any{"enabled": true}, refExpr("client", "/metadata"), expr("length", refExpr("current", inputPointer))}
	case "value_array":
		return []any{[]any{"hello", 42}}
	case "string_array":
		return []any{[]any{"metadata", "truncation"}}
	case "condition":
		return []any{map[string]any{"op": "type", "source": "current", "path": inputPointer, "value": "array"}}
	case "condition_array":
		return []any{[]any{map[string]any{"op": "exists", "path": "/metadata"}}}
	case "action_array":
		return []any{[]any{map[string]any{"id": "set-value", "type": "json_set", "params": map[string]any{"path": "/metadata/policy", "value": "example", "create_parents": true}}}}
	}
	return defaultFieldExamples(f)
}
func defaultFieldExamples(f FieldSpec) []any {
	if f.Default != nil {
		if s, ok := f.Default.(string); ok && s != "" {
			return []any{s}
		}
	}
	switch f.Name {
	case "pattern":
		return []any{"(?s)<tag>.*?</tag>"}
	case "replacement":
		return []any{"${1}-updated", ""}
	case "name", "bind":
		return []any{"message"}
	case "model":
		return []any{"example-model"}
	case "created_at", "updated_at":
		return []any{"2026-10-07T00:00:00Z"}
	}
	return []any{"example"}
}
func expressionHelp(v string) string {
	m := map[string]string{
		"array":          "array(a, b, …)：构造数组，0–256 项。例如 array(1, 2) → [1,2]。",
		"object":         "object(key, value, …)：构造对象，键必须为不重复字符串。例如 object(\"enabled\", true) → {\"enabled\":true}。",
		"coalesce":       "coalesce(a, b, …)：返回首个存在且非 null 的值，保留 false、0 和空字符串；全部缺失返回 null。例如 coalesce(缺失, \"default\") → \"default\"。",
		"concat":         "concat(a, b, …)：拼接字符串；不做隐式转换。例如 concat(\"a\", \"b\") → \"ab\"。",
		"and":            "and(a, b, …)：所有布尔值成立才返回 true，遇 false 短路。例如 and(true, false) → false。",
		"or":             "or(a, b, …)：任一布尔值成立返回 true，遇 true 短路。例如 or(false, true) → true。",
		"not":            "not(bool)：布尔取反。例如 not(true) → false。",
		"get":            "get(value, pointer)：用动态 JSON Pointer 读取值；缺失保持缺失。例如 get({\"a\":1}, \"/a\") → 1。",
		"index":          "index(array, index)：读取从 0 开始的数组下标；越界为缺失。例如 index([\"a\"], 0) → \"a\"。",
		"join":           "join(array, separator)：连接字符串数组。例如 join([\"a\",\"b\"], \"\\n\") → \"a\\nb\"。",
		"length":         "length(value)：字符串按 Unicode 字符计数，数组按元素计数，对象按键计数。例如 length(\"中文\") → 2。",
		"keys":           "keys(object)：返回按名称排序的对象键数组。例如 keys({\"b\":2,\"a\":1}) → [\"a\",\"b\"]。",
		"exists":         "exists(value)：判断引用或计算结果是否存在，null 仍存在。例如 exists(缺失) → false。",
		"eq":             "eq(a, b)：JSON 深度相等，严格区分类型。例如 eq(1, \"1\") → false。",
		"ne":             "ne(a, b)：JSON 深度不相等，严格区分类型。例如 ne(1, 2) → true。",
		"contains":       "contains(value, part)：字符串子串、数组元素或对象键。例如 contains([1,2], 2) → true。",
		"add":            "add(a, b)：数值加法。例如 add(2, 3) → 5。",
		"subtract":       "subtract(a, b)：数值减法。例如 subtract(5, 2) → 3。",
		"gt":             "gt(a, b)：数值大于。例如 gt(2, 1) → true。",
		"gte":            "gte(a, b)：数值大于等于。例如 gte(2, 2) → true。",
		"lt":             "lt(a, b)：数值小于。例如 lt(1, 2) → true。",
		"lte":            "lte(a, b)：数值小于等于。例如 lte(2, 2) → true。",
		"type":           "type(value)：返回 missing、null、boolean、number、string、array 或 object。例如 type(缺失) → \"missing\"。",
		"trim":           "trim(string)：移除首尾 Unicode 空白。例如 trim(\" a \") → \"a\"。",
		"lower":          "lower(string)：转为小写。例如 lower(\"ABC\") → \"abc\"。",
		"upper":          "upper(string)：转为大写。例如 upper(\"abc\") → \"ABC\"。",
		"string":         "string(value)：数字或布尔值转字符串，字符串原样；对象、数组和 null 报错。例如 string(42) → \"42\"。",
		"pointer_escape": "pointer_escape(string)：转义一个路径段，~ → ~0，/ → ~1。例如 pointer_escape(\"a/b\") → \"a~1b\"。",
		"json_parse":     "json_parse(string)：解析严格 JSON；重复键和非法数值报错。例如 json_parse(\"[1]\") → [1]。",
		"json_stringify": "json_stringify(value)：序列化紧凑 JSON，字符串带引号。例如 json_stringify([1]) → \"[1]\"。",
		"slice":          "slice(string, start, end)：按 Unicode 字符截取 [start,end)，超出长度截断。例如 slice(\"abcd\", 1, 3) → \"bc\"。",
		"regex_replace":  "regex_replace(text, pattern, replacement)：Go RE2 全部替换，支持 $1、${name} 捕获组。例如 regex_replace(\"abc\", \"(b)\", \"${1}${1}\") → \"abbc\"。",
	}
	return m[v]
}

func optionHelp(v string) string {
	m := map[string]string{
		"current": "读取前序步骤的结果。", "original": "读取当前阶段或遍历元素开始处理时的原始值。", "client": "读取最初客户端 body，只读。", "context": "读取网关事实；阶段决定可用字段。", "vars": "读取局部变量，如 /message/index。", "item": "读取当前遍历元素的原始值。",
		"ignore": "目标不存在时跳过；null 仍视为存在。", "error": "目标不存在时报错并回滚整条规则。", "abort": "失败回滚当前规则并终止阶段。", "skip_rule": "失败回滚当前规则后继续下一条。", "overwrite": "替换已存在值，包括 null。", "keep": "保留已存在值，包括 null。", "value": "保留 JSON 类型。", "json": "序列化为 JSON 字符串，字符串包含引号。", "literal": "按原文匹配，不解释捕获组。", "regex": "使用 Go RE2 正则。", "remove_matches": "删除条件成立的数组元素。", "keep_matches": "只保留条件成立的数组元素。",
		"client_request": "规范化之前的客户端 body。", "request_normalize": "协议请求构建，输入为客户端 body。", "request": "规范化后的请求；路由前。", "request_finalize": "路由前的协议字段修正。", "upstream_headers": "发送前请求头，context/transport 为 sse 或 websocket。", "response_event": "逐个响应事件；不会自动同步终态快照。", "response_body": "非流式聚合后的正文。",
	}
	if s, ok := m[v]; ok {
		return s
	}
	return "选择 " + v + "；具体行为与其他参数关系见本字段说明。"
}
func computedCapabilities() []Capability {
	names := []string{}
	for name := range expressionArity {
		names = append(names, name)
	}
	sort.Strings(names)
	capability := Capability{ID: "computed", Label: "计算值 / Computed value", Description: "{$expr:{op,args}}；所有参数支持嵌套引用与计算；对象使用 object 的键值对数组构造，普通 JSON 对象永远是字面量。", Phases: phases, Fields: []FieldSpec{field("op", "string", "运算 / Operator", "get 动态 JSON Pointer；index 读取数组下标；object 为键值对；regex_replace 的三个参数为原文、RE2 模式、替换值。", nil, true, names...), field("args", "value_array", "参数 / Arguments", "参数按顺序求值；and/or/coalesce 短路；不存在与 null 区分。", nil, true)}}
	for _, name := range names {
		capability.Fields[0].EnumHelp[name] = expressionHelp(name)
	}
	capability.Fields[1].Examples = []any{[]any{refExpr("current", inputPointer), "\n"}, []any{refExpr("current", "/model"), "example-model"}}
	return []Capability{capability}
}
