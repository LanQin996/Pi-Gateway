package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func isComputed(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, ok = m["$expr"]
	return ok
}
func isFlowAction(t string) bool {
	return contains([]string{"sequence", "if", "for_each", "walk", "scope", "call", "let"}, t)
}
func IsPhase(t string) bool { return contains(phases, t) }

func flowCapabilities() []Capability {
	steps := field("steps", "action_array", "内部步骤 / Steps", "按数组顺序执行；支持嵌套分支、遍历、变量；失败回滚整条规则。", []any{}, true)
	missing := field("on_missing", "string", "缺失路径 / Missing", "ignore 跳过不存在的目标；error 回滚规则；null 不是缺失。", "ignore", false, "ignore", "error")
	return []Capability{
		{ID: "sequence", Label: "顺序 / Sequence", Description: "执行内部步骤，顺序决定数据依赖。", Phases: phases, Fields: []FieldSpec{steps}},
		{ID: "if", Label: "分支 / Branch", Description: "成立执行 then，否则执行 else；分支可以嵌套。", Phases: phases, Fields: []FieldSpec{field("predicate", "condition", "条件 / Condition", "读取执行到此处时的数据。", nil, true), field("then", "action_array", "成立 / Then", "成立时顺序执行的步骤。", []any{}, true), field("else", "action_array", "不成立 / Else", "不成立时执行；空数组不操作。", []any{}, false)}},
		{ID: "for_each", Label: "逐项处理 / For each", Description: "以原始元素快照迭代数组或对象；current 是元素本身，original 是该元素原值；变量作用域隔离。", Phases: phases, Fields: []FieldSpec{ptr("path", true), field("bind", "string", "绑定名称 / Binding", "vars 中保存原始 value、index/key、path；嵌套遍历使用不同名称。", "item", false), steps, field("keep", "condition", "保留条件 / Keep", "在修改后判断；不成立删除元素；原始下标保持稳定。", map[string]any{"op": "always"}, false), missing}},
		{ID: "walk", Label: "递归遍历 / Walk", Description: "深度优先处理选定子树；先处理子节点再处理父节点；predicate 决定是否运行内部步骤。", Phases: phases, Fields: []FieldSpec{ptr("path", true), field("predicate", "condition", "节点条件 / Predicate", "只选择指定类型或具有指定字段的节点。", map[string]any{"op": "always"}, false), steps, field("keep", "condition", "保留条件 / Keep", "执行后不成立删除节点；禁止删除遍历根。", map[string]any{"op": "always"}, false), missing}},
		{ID: "let", Label: "保存变量 / Variable", Description: "变量只在当前规则和当前作用域中有效；不会写入请求。", Phases: phases, Fields: []FieldSpec{requiredString("name", "名称 / Name", "字母或下划线开头，后续可用数字，最多 64 字符。"), field("value", "value", "值 / Value", "字面量、引用或计算表达式；读取不存在的值报错。", nil, true)}},
		{ID: "scope", Label: "规则片段 / Fragments", Description: "定义局部可调用片段；片段由普通步骤组成，编译检查调用目标与递归。", Phases: phases, Fields: []FieldSpec{field("functions", "value", "片段 / Functions", "对象键为片段名，值为步骤数组；调用可通过 vars 传参。例如 {filter: [{id: remove, type: json_remove, params: {paths: [/metadata]}}]}。", map[string]any{}, true), steps}},
		{ID: "call", Label: "调用片段 / Call", Description: "在最近的 scope 中查找片段；禁止递归调用。", Phases: phases, Fields: []FieldSpec{requiredString("name", "片段名 / Fragment", "例如 filter；必须在外层 scope 的 functions 中定义。")}},
	}
}

func decodeActions(v any, path string) ([]Action, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, invalid(path, "%v", err)
	}
	var raws []json.RawMessage
	if err = decodeJSONArray(b, path, &raws); err != nil {
		return nil, err
	}
	out := make([]Action, len(raws))
	ids := map[string]bool{}
	for i, raw := range raws {
		p := fmt.Sprintf("%s/%d", path, i)
		if err = decodeJSON(raw, p, &out[i]); err != nil {
			return nil, err
		}
		if out[i].ID == "" || ids[out[i].ID] {
			return nil, invalid(p+"/id", "nonempty unique step ID required")
		}
		ids[out[i].ID] = true
	}
	return out, nil
}

type flowCompiler struct {
	action *compiledAction
	phase  string
	nodes  *int
	item   bool
	depth  int
}

func compileFlow(a *compiledAction, phase string, nodes *int, item bool, depth int) error {
	if a.raw.Type == "for_each" && !variableName.MatchString(stringParam(a.raw.Params, "bind")) {
		return invalid(a.path+"/params/bind", "invalid binding name")
	}
	compiler := flowCompiler{a, phase, nodes, item, depth}
	for _, compile := range []func() error{compiler.children, compiler.predicate, compiler.functions, compiler.keep} {
		if err := compile(); err != nil {
			return err
		}
	}
	return nil
}
func (c flowCompiler) list(v any, path string) ([]compiledAction, error) {
	raw, err := decodeActions(v, path)
	if err != nil {
		return nil, err
	}
	out := []compiledAction{}
	for i, action := range raw {
		*c.nodes++
		if *c.nodes > MaxConditionNodes {
			return nil, invalid(path, "maximum flow nodes exceeded")
		}
		compiled, err := compileScopedAction(action, fmt.Sprintf("%s/%d", path, i), c.phase, c.nodes, c.item || c.action.raw.Type == "for_each" || c.action.raw.Type == "walk", c.depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, compiled)
	}
	return out, nil
}
func (c flowCompiler) children() error {
	a := c.action
	var err error
	if v, ok := a.raw.Params["steps"]; ok {
		a.children, err = c.list(v, a.path+"/params/steps")
		if err != nil {
			return err
		}
	}
	if a.raw.Type != "if" {
		return nil
	}
	a.children, err = c.list(a.raw.Params["then"], a.path+"/params/then")
	if err != nil {
		return err
	}
	a.otherwise, err = c.list(a.raw.Params["else"], a.path+"/params/else")
	return err
}
func (c flowCompiler) predicate() error {
	a := c.action
	v, ok := a.raw.Params["predicate"]
	if !ok {
		return nil
	}
	raw, err := asCondition(v, a.path+"/params/predicate")
	if err != nil {
		return err
	}
	a.predicate, err = compileCondition(raw, a.path+"/params/predicate", c.phase, c.item || a.raw.Type == "walk", c.depth, c.nodes)
	return err
}
func (c flowCompiler) functions() error {
	a := c.action
	if a.raw.Type != "scope" {
		return nil
	}
	m, ok := a.raw.Params["functions"].(map[string]any)
	if !ok {
		return invalid(a.path+functionsParameterPointer, "must be an object of step arrays")
	}
	a.functions = map[string][]compiledAction{}
	for name, v := range m {
		if !variableName.MatchString(name) {
			return invalid(a.path+functionsParameterPointer, "invalid fragment name")
		}
		body, err := c.list(v, joinPointer(a.path+functionsParameterPointer, name))
		if err != nil {
			return err
		}
		a.functions[name] = body
	}
	return nil
}
func (c flowCompiler) keep() error {
	a := c.action
	v, ok := a.raw.Params["keep"]
	if !ok {
		return nil
	}
	raw, err := asCondition(v, a.path+"/params/keep")
	if err != nil {
		return err
	}
	a.keep, err = compileCondition(raw, a.path+"/params/keep", c.phase, true, c.depth, c.nodes)
	return err
}

func validateCalls(actions []compiledAction, scopes []map[string][]compiledAction, active map[string]bool, depth int) error {
	if depth > MaxDepth {
		return invalid("/actions", "maximum fragment depth exceeded")
	}
	for _, a := range actions {
		if err := validateActionCalls(a, scopes, active, depth); err != nil {
			return err
		}
	}
	return nil
}
func validateActionCalls(a compiledAction, scopes []map[string][]compiledAction, active map[string]bool, depth int) error {
	if a.raw.Type == "scope" {
		return validateScopeCalls(a, scopes, active, depth)
	}
	if a.raw.Type == "call" {
		if err := validateFragmentCall(a, scopes, active, depth); err != nil {
			return err
		}
	}
	if err := validateCalls(a.children, scopes, active, depth+1); err != nil {
		return err
	}
	return validateCalls(a.otherwise, scopes, active, depth+1)
}
func validateScopeCalls(a compiledAction, scopes []map[string][]compiledAction, active map[string]bool, depth int) error {
	next := append(append([]map[string][]compiledAction{}, scopes...), a.functions)
	for name, body := range a.functions {
		key := fmt.Sprintf("%d:%s", len(next)-1, name)
		active[key] = true
		err := validateCalls(body, next, active, depth+1)
		delete(active, key)
		if err != nil {
			return err
		}
	}
	return validateCalls(a.children, next, active, depth+1)
}
func validateFragmentCall(a compiledAction, scopes []map[string][]compiledAction, active map[string]bool, depth int) error {
	name := stringParam(a.raw.Params, "name")
	for i := len(scopes) - 1; i >= 0; i-- {
		body, ok := scopes[i][name]
		if !ok {
			continue
		}
		key := fmt.Sprintf("%d:%s", i, name)
		if active[key] {
			return invalid(a.path, "recursive fragment call")
		}
		active[key] = true
		err := validateCalls(body, scopes[:i+1], active, depth+1)
		delete(active, key)
		return err
	}
	return invalid(a.path+"/params/name", "unknown fragment %q", name)
}

func (s *evaluation) runSteps(ctx context.Context, actions []compiledAction) (terminal, error) {
	for i, a := range actions {
		if err := s.tick(ctx); err != nil {
			return terminal{}, err
		}
		start := time.Now()
		var before any
		if s.nestedTraces != nil && s.nestedTraces.canCollect() {
			before, _ = cloneJSON(s.body)
		}
		out, err := a.execute(ctx, s)
		if err == nil {
			err = ensurePayload(s.body)
		}
		s.recordStepTrace(a, i, start, before, err)
		if err != nil {
			return terminal{}, prefixError(a.path, err)
		}
		if out.blocked || out.dropped {
			return out, nil
		}
	}
	return terminal{}, nil
}

type runtimeFragment struct {
	steps     []compiledAction
	functions map[string]*runtimeFragment
}

func (a compiledAction) executeFlow(ctx context.Context, s *evaluation) (terminal, error) {
	switch a.raw.Type {
	case "let":
		return a.executeFlowLet(ctx, s)
	case "sequence":
		return s.runSteps(ctx, a.children)
	case "if":
		return a.executeFlowBranch(ctx, s)
	case "scope":
		return a.executeFlowScope(ctx, s)
	case "call":
		return a.executeFlowCall(ctx, s)
	case "for_each", "walk":
		return a.executeFlowTraversal(ctx, s)
	}
	return terminal{}, fmt.Errorf("unknown flow")
}

func (a compiledAction) executeFlowLet(ctx context.Context, s *evaluation) (terminal, error) {
	p := a.raw.Params
	v, err := s.requiredExpression(ctx, p["value"])
	if err == nil {
		s.vars[stringParam(p, "name")] = v
	}
	return terminal{}, err
}

func (a compiledAction) executeFlowBranch(ctx context.Context, s *evaluation) (terminal, error) {
	match, err := a.predicate.matches(ctx, s)
	if err != nil {
		return terminal{}, err
	}
	if match {
		return s.runSteps(ctx, a.children)
	}
	return s.runSteps(ctx, a.otherwise)
}

func (a compiledAction) executeFlowScope(ctx context.Context, s *evaluation) (terminal, error) {
	old := s.functions
	next := map[string]*runtimeFragment{}
	for k, v := range old {
		next[k] = v
	}
	for k, v := range a.functions {
		next[k] = &runtimeFragment{steps: v, functions: next}
	}
	oldVars := s.vars
	s.vars = map[string]any{}
	for k, v := range oldVars {
		s.vars[k] = v
	}
	defer func() { s.vars = oldVars }()
	s.functions = next
	defer func() { s.functions = old }()
	return s.runSteps(ctx, a.children)
}

func (a compiledAction) executeFlowCall(ctx context.Context, s *evaluation) (terminal, error) {
	p := a.raw.Params
	fragment := s.functions[stringParam(p, "name")]
	if fragment == nil {
		return terminal{}, fmt.Errorf("unknown fragment")
	}
	old := s.functions
	s.functions = fragment.functions
	defer func() { s.functions = old }()
	return s.runSteps(ctx, fragment.steps)
}

func (a compiledAction) executeFlowTraversal(ctx context.Context, s *evaluation) (terminal, error) {
	p := a.raw.Params
	path := stringParam(p, "path")
	v, exists, err := s.selectValue("current", path, "")
	if err != nil {
		return terminal{}, err
	}
	if !exists {
		return terminal{}, onMissing(p)
	}
	var out terminal
	if a.raw.Type == "walk" {
		var keep bool
		v, keep, out, err = a.walk(ctx, s, v, path, 0)
		if err == nil && !keep {
			err = fmt.Errorf("cannot delete traversal root")
		}
	} else {
		v, out, err = a.each(ctx, s, v, path)
	}
	if err == nil {
		s.body, err = pointerSet(s.body, path, v, false, false)
	}
	return out, err
}
func scoped(s *evaluation, v any, path string, index int, key, bind string) evaluation {
	n := *s
	n.body = v
	n.original, _ = cloneJSON(v)
	n.item = n.original
	n.index = index
	n.hasItem = true
	n.itemPath = s.itemPath + path
	n.vars = map[string]any{}
	for k, v := range s.vars {
		n.vars[k] = v
	}
	if bind != "" {
		n.vars[bind] = map[string]any{"value": n.original, "index": index, "key": key, "path": n.itemPath}
	}
	return n
}
func (a compiledAction) each(ctx context.Context, s *evaluation, v any, path string) (any, terminal, error) {
	if arr, ok := v.([]any); ok {
		return a.eachArray(ctx, s, arr, path)
	}
	if _, ok := object(v); ok {
		return a.eachObject(ctx, s, v, path)
	}
	return nil, terminal{}, fmt.Errorf("for_each target must be array or object")
}
func (a compiledAction) eachArray(ctx context.Context, s *evaluation, arr []any, path string) (any, terminal, error) {
	out := []any{}
	for i, item := range arr {
		if err := s.tick(ctx); err != nil {
			return nil, terminal{}, err
		}
		n := scoped(s, item, fmt.Sprintf("%s/%d", path, i), i, "", stringParam(a.raw.Params, "bind"))
		keep, t, err := a.runEachItem(ctx, &n)
		if err != nil {
			return nil, t, err
		}
		if keep {
			out = append(out, n.body)
		}
	}
	return out, terminal{}, nil
}
func (a compiledAction) eachObject(ctx context.Context, s *evaluation, v any, path string) (any, terminal, error) {
	out := map[string]any{}
	for i, k := range keys(v) {
		if err := s.tick(ctx); err != nil {
			return nil, terminal{}, err
		}
		item, _ := objGet(v, k)
		n := scoped(s, item, joinPointer(path, k), i, k, stringParam(a.raw.Params, "bind"))
		keep, t, err := a.runEachItem(ctx, &n)
		if err != nil {
			return nil, t, err
		}
		if keep {
			out[k] = n.body
		}
	}
	return out, terminal{}, nil
}
func (a compiledAction) runEachItem(ctx context.Context, n *evaluation) (bool, terminal, error) {
	t, err := n.runSteps(ctx, a.children)
	if err != nil {
		return false, t, err
	}
	keep, err := a.keep.matches(ctx, n)
	if err != nil {
		return false, t, err
	}
	if t.blocked || t.dropped {
		return false, t, fmt.Errorf("terminal result is not allowed inside traversal")
	}
	return keep, t, nil
}
func (a compiledAction) walk(ctx context.Context, s *evaluation, v any, path string, depth int) (any, bool, terminal, error) {
	if depth > MaxDepth {
		return nil, false, terminal{}, fmt.Errorf("maximum traversal depth exceeded")
	}
	if err := s.tick(ctx); err != nil {
		return nil, false, terminal{}, err
	}
	original, err := cloneJSON(v)
	if err != nil {
		return nil, false, terminal{}, err
	}
	v, _, nestedTerminal, err := a.walkChildren(ctx, s, v, path, depth)
	if err != nil {
		return nil, false, nestedTerminal, err
	}
	n := scoped(s, v, path, 0, "", "")
	n.original, n.item = original, original
	match, err := a.predicate.matches(ctx, &n)
	if err != nil {
		return nil, false, terminal{}, err
	}
	if !match {
		return v, true, terminal{}, nil
	}
	t, err := n.runSteps(ctx, a.children)
	if err != nil {
		return nil, false, t, err
	}
	if t.blocked || t.dropped {
		return nil, false, t, fmt.Errorf("terminal result is not allowed inside traversal")
	}
	keep, err := a.keep.matches(ctx, &n)
	return n.body, keep, t, err
}

func (a compiledAction) walkChildren(ctx context.Context, s *evaluation, v any, path string, depth int) (any, bool, terminal, error) {
	if arr, ok := v.([]any); ok {
		return a.walkArray(ctx, s, arr, path, depth)
	}
	if _, ok := object(v); ok {
		return a.walkObject(ctx, s, v, path, depth)
	}
	return v, true, terminal{}, nil
}
func (a compiledAction) walkArray(ctx context.Context, s *evaluation, arr []any, path string, depth int) (any, bool, terminal, error) {
	out := []any{}
	for i, item := range arr {
		next, keep, t, err := a.walk(ctx, s, item, fmt.Sprintf("%s/%d", path, i), depth+1)
		if err != nil {
			return nil, false, t, err
		}
		if keep {
			out = append(out, next)
		}
	}
	return out, true, terminal{}, nil

}
func (a compiledAction) walkObject(ctx context.Context, s *evaluation, v any, path string, depth int) (any, bool, terminal, error) {
	for _, k := range keys(v) {
		item, _ := objGet(v, k)
		next, keep, t, err := a.walk(ctx, s, item, joinPointer(path, k), depth+1)
		if err != nil {
			return nil, false, t, err
		}
		if keep {
			objSet(v, k, next)
		} else {
			objDelete(v, k)
		}
	}
	return v, true, terminal{}, nil
}

func (s *evaluation) recordStepTrace(a compiledAction, i int, start time.Time, before any, err error) {
	if s.nestedTraces == nil || s.traceRule == nil {
		return
	}
	t := baseTrace(*s.traceRule, s)
	t.ActionID = a.raw.ID
	t.ActionType = a.raw.Type
	t.ActionIndex = i
	t.ActionPath = a.path
	t.ItemPath = s.itemPath
	t.Matched = true
	t.Status = "no_change"
	t.DurationNS = time.Since(start).Nanoseconds()
	if s.nestedTraces.canCollect() {
		t.Changes, t.OmittedChanges = structuralDiff(before, s.body)
		if len(t.Changes) > 0 || t.OmittedChanges > 0 {
			t.Status = "changed"
		}
	}
	if err != nil {
		t.Status = "error"
		t.Error = err.Error()
	}
	s.nestedTraces.add(t)
}
