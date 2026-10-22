package rules

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

type compiledCondition struct {
	raw      Condition
	path     string
	children []*compiledCondition
	re       *regexp.Regexp
}
type compiledAction struct {
	raw       Action
	path      string
	re        *regexp.Regexp
	predicate *compiledCondition
	children  []compiledAction
	otherwise []compiledAction
	functions map[string][]compiledAction
	keep      *compiledCondition
}
type compiledRule struct {
	raw     Rule
	path    string
	when    *compiledCondition
	actions []compiledAction
}

var actionSpecs = actionCapabilities()
var conditionSpecs = conditionCapabilities()

const mustBeTypeMessage = "must be %s"

func lookupCapability(caps []Capability, id string) (Capability, bool) {
	for _, c := range caps {
		if c.ID == id {
			return c, true
		}
	}
	return Capability{}, false
}
func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
func conditionMap(c Condition) map[string]any {
	m := map[string]any{"op": c.Op}
	if c.Source != "" {
		m["source"] = c.Source
	}
	if c.Path != "" {
		m["path"] = c.Path
	}
	if c.Encoding != "" {
		m["encoding"] = c.Encoding
	}
	if c.ValuePresent || c.Value != nil {
		m["value"] = c.Value
	}
	if c.Conditions != nil {
		m["conditions"] = c.Conditions
	}
	if c.CaseInsensitive {
		m["case_insensitive"] = true
	}
	if c.DotAll {
		m["dot_all"] = true
	}
	if c.Multiline {
		m["multiline"] = true
	}
	for k := range c.present {
		if _, ok := m[k]; ok {
			continue
		}
		switch k {
		case "source":
			m[k] = c.Source
		case "path":
			m[k] = c.Path
		case "encoding":
			m[k] = c.Encoding
		case "conditions":
			m[k] = c.Conditions
		case "case_insensitive":
			m[k] = c.CaseInsensitive
		case "dot_all":
			m[k] = c.DotAll
		case "multiline":
			m[k] = c.Multiline
		}
	}
	return m
}
func (c Condition) MarshalJSON() ([]byte, error) { return json.Marshal(conditionMap(c)) }

// ValidateRule performs the same validation as Compile without publishing an engine.
func ValidateRule(r Rule, path string) error { _, e := compileRule(r, path); return e }
func compileRule(r Rule, path string) (compiledRule, error) {
	if r.OnError == "" {
		r.OnError = OnErrorAbort
	}
	if err := validateRuleDefinition(r, path); err != nil {
		return compiledRule{}, err
	}
	nodes := 0
	when, e := compileCondition(r.When, joinPointer(path, "when"), r.Phase, false, 0, &nodes)
	if e != nil {
		return compiledRule{}, e
	}
	out := compiledRule{raw: r, path: path, when: when}
	ids := map[string]bool{}
	for i, a := range r.Actions {
		p := fmt.Sprintf("%s/actions/%d", path, i)
		if a.ID == "" {
			return compiledRule{}, invalid(p+"/id", "must be nonempty")
		}
		if ids[a.ID] {
			return compiledRule{}, invalid(p+"/id", "duplicate action ID %q", a.ID)
		}
		ids[a.ID] = true
		ca, e := compileAction(a, p, r.Phase, &nodes)
		if e != nil {
			return compiledRule{}, e
		}
		out.actions = append(out.actions, ca)
	}
	if err := validateCalls(out.actions, nil, map[string]bool{}, 0); err != nil {
		return compiledRule{}, err
	}
	return out, nil
}
func validateRuleDefinition(r Rule, path string) error {
	bad := func(k, msg string) error {
		return invalid(joinPointer(path, k), "%s", msg)
	}
	if r.SchemaVersion != 1 && r.SchemaVersion != SchemaVersion {
		return bad("schema_version", "unsupported schema version")
	}
	if strings.TrimSpace(r.Name) == "" {
		return bad("name", "must be a nonempty string")
	}
	if !contains(phases, r.Phase) {
		return bad("phase", "unknown phase")
	}
	if err := validateRuleMetadata(r, path); err != nil {
		return err
	}
	if r.OnError != OnErrorAbort && r.OnError != OnErrorSkipRule {
		return bad("on_error", "must be abort or skip_rule")
	}
	if len(r.Actions) > MaxActionsPerRule {
		return bad("actions", fmt.Sprintf("maximum %d actions per rule", MaxActionsPerRule))
	}

	return nil
}
func validateFields(params map[string]any, fs []FieldSpec, path string, phase string, item bool) (map[string]any, error) {
	var err error
	params, err = normalizeFieldParams(params, path)
	if err != nil {
		return nil, err
	}
	known := map[string]FieldSpec{}
	out := make(map[string]any, len(fs))
	for _, f := range fs {
		known[f.Name] = f
	}
	for k := range params {
		if _, ok := known[k]; !ok {
			return nil, invalid(joinPointer(path, k), "unknown or inapplicable field")
		}
	}
	for _, f := range fs {
		value, present, err := validateOneField(params, f, path, phase, item)
		if err != nil {
			return nil, err
		}
		if present {
			out[f.Name] = value
		}

	}
	return out, nil
}
func validateFieldValue(v any, f FieldSpec, path, phase string, item bool) error {
	switch f.Type {
	case "string", "pointer":
		if err := validateFieldString(v, f, path, phase, item); err != nil {
			return err
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return invalid(path, mustBeTypeMessage, f.Type)
		}
	case "integer":
		if err := validateFieldInteger(v, f, path, phase, item); err != nil {
			return err
		}
	case "string_array", "pointer_array":
		if err := validateFieldStringArray(v, f, path, phase, item); err != nil {
			return err
		}
	case "value":
		return validateExpression(v, path, phase, item, 0)
	case "value_array":
		if err := validateFieldValueArray(v, f, path, phase, item); err != nil {
			return err
		}
	case "condition":
		_, e := asCondition(v, path)
		return e
	case "action_array":
		_, err := decodeActions(v, path)
		return err
	case "condition_array":
		if err := validateFieldConditionArray(v, f, path, phase, item); err != nil {
			return err
		}
	default:
		return invalid(path, "unsupported schema field type %q", f.Type)
	}
	return validateLiteral(v, path, 0)
}

func asArray(v any) ([]any, bool) {
	switch a := v.(type) {
	case []any:
		return a, true
	case []string:
		r := make([]any, len(a))
		for i, x := range a {
			r[i] = x
		}
		return r, true
	}
	return nil, false
}
func validateLiteral(v any, path string, depth int) error {
	if depth > MaxDepth {
		return invalid(path, "maximum JSON depth %d exceeded", MaxDepth)
	}
	switch x := v.(type) {
	case nil, string, bool:
		return nil
	case json.Number:
		_, e := normalizeNumber(x.String(), path)
		return e
	case []Condition:
		return nil
	case Condition:
		return nil
	case []string:
		return nil
	case []any:
		for i, y := range x {
			if e := validateLiteral(y, fmt.Sprintf("%s/%d", path, i), depth+1); e != nil {
				return e
			}
		}
		return nil
	case map[string]any:
		for k, y := range x {
			if e := validateLiteral(y, joinPointer(path, k), depth+1); e != nil {
				return e
			}
		}
		return nil
	}
	n, ok := number(v)
	if !ok {
		return invalid(path, "unsupported JSON value %T", v)
	}
	if math.Trunc(n) == n && math.Abs(n) > float64(MaxSafeInteger) {
		return invalid(path, "integer exceeds JavaScript safe range")
	}
	return nil
}
func validateExpression(v any, path, phase string, item bool, depth int) error {
	if depth > MaxDepth {
		return invalid(path, "maximum expression depth exceeded")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return validateLiteral(v, path, depth)
	}
	if expr, exists := m["$expr"]; exists {
		if len(m) != 1 {
			return invalid(path, "$expr must be the only key")
		}
		return validateComputed(expr, path+"/$expr", phase, item, depth+1)
	}
	if literal, exists := m["$literal"]; exists {
		if len(m) != 1 {
			return invalid(path, "$literal must be the only key")
		}
		return validateLiteral(literal, path+"/$literal", depth+1)
	}
	if ref, exists := m["$ref"]; exists {
		if len(m) != 1 {
			return invalid(path, "$ref must be the only key")
		}
		return validateReference(ref, path, phase, item)
	}
	return validateLiteral(v, path, depth)
}
func validateSource(source, p, path, phase string, item bool) error {
	if source == "item" && !item {
		return invalid(path+"/source", "item is only available inside array predicates")
	}
	parts, e := pointerSegments(p)
	if e != nil {
		return invalid(path+"/path", "%v", e)
	}
	if source == "context" && len(parts) > 0 {
		if parts[0] == "item_index" && !item {
			return invalid(path+"/path", "item_index is only available inside array predicates")
		}
		if phase != PhaseResponseEvent && phase != PhaseResponseBody && phase != PhaseUpstreamHeaders && contains([]string{"account_id", "upstream_protocol", "event_type"}, parts[0]) {
			return invalid(path+"/path", "%s is unavailable in request phase", parts[0])
		}
	}
	return nil
}
func asCondition(v any, path string) (Condition, error) {
	if c, ok := v.(Condition); ok {
		return c, nil
	}
	b, e := json.Marshal(v)
	if e != nil {
		return Condition{}, invalid(path, "invalid condition: %v", e)
	}
	var c Condition
	if e = json.Unmarshal(b, &c); e != nil {
		return Condition{}, prefixError(path, e)
	}
	return c, nil
}
func prefixError(path string, e error) error {
	if ve, ok := e.(*ValidationError); ok {
		return &ValidationError{Path: path + ve.Path, Message: ve.Message}
	}
	return invalid(path, "%v", e)
}
func compileCondition(c Condition, path, phase string, item bool, depth int, nodes *int) (*compiledCondition, error) {
	*nodes++
	if *nodes > MaxConditionNodes {
		return nil, invalid(path, "maximum %d condition nodes per rule exceeded", MaxConditionNodes)
	}
	if depth > MaxDepth {
		return nil, invalid(path, "maximum condition depth %d exceeded", MaxDepth)
	}
	capability, ok := lookupCapability(conditionSpecs, c.Op)
	if !ok {
		return nil, invalid(path+"/op", "unknown condition operator %q", c.Op)
	}
	m := conditionMap(c)
	delete(m, "op")
	if _, e := validateFields(m, capability.Fields, path, phase, item); e != nil {
		return nil, e
	}
	if e := validateSource(c.Source, c.Path, path, phase, item); e != nil {
		return nil, e
	}
	out := &compiledCondition{raw: c, path: path}
	if err := out.compileChildren(phase, item, depth, nodes); err != nil {
		return nil, err
	}
	if err := out.validateComparison(); err != nil {
		return nil, err
	}

	return out, nil
}
func isReference(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, ok = m["$ref"]
	return ok
}
func expressionLiteral(v any) any {
	if m, ok := v.(map[string]any); ok {
		if x, exists := m["$literal"]; exists {
			return x
		}
	}
	return v
}
func compileAction(a Action, path, phase string, nodes *int) (compiledAction, error) {
	return compileScopedAction(a, path, phase, nodes, false, 0)
}
func compileScopedAction(a Action, path, phase string, nodes *int, item bool, depth int) (compiledAction, error) {
	if depth > MaxDepth {
		return compiledAction{}, invalid(path, "maximum flow depth exceeded")
	}

	capability, ok := lookupCapability(actionSpecs, a.Type)
	if !ok {
		return compiledAction{}, invalid(path+"/type", "unknown action %q", a.Type)
	}
	if !contains(capability.Phases, phase) {
		return compiledAction{}, invalid(path+"/type", "%s is not allowed in %s", a.Type, phase)
	}
	p, e := validateFields(a.Params, capability.Fields, path+"/params", phase, item)
	if e != nil {
		return compiledAction{}, e
	}
	a.Params = p
	if isLegacyAction(a.Type) {
		expanded, err := expandLegacyAction(a)
		if err != nil {
			return compiledAction{}, err
		}
		return compileScopedAction(Action{ID: a.ID, Type: "sequence", Params: map[string]any{"steps": expanded}}, path, phase, nodes, item, depth+1)
	}
	out := compiledAction{raw: a, path: path}
	if isFlowAction(a.Type) {
		if e := compileFlow(&out, phase, nodes, item, depth); e != nil {
			return out, e
		}
	}
	for _, validate := range []func(string, *int) error{out.validateVariable, out.compileArrayPredicate, out.validateArrayPosition, out.compileTextPattern, out.validateRemovalPaths, out.validateTransferPaths, out.validateMergeValue} {
		if err := validate(phase, nodes); err != nil {
			return out, err
		}
	}
	if e := validateProtectedAction(a, phase, path); e != nil {
		return out, e
	}
	return out, nil
}
func stringParam(p map[string]any, k string) string { s, _ := p[k].(string); return s }
func boolParam(p map[string]any, k string) bool     { b, _ := p[k].(bool); return b }

func (out *compiledAction) validateVariable(phase string, nodes *int) error {
	a := out.raw
	p, path := a.Params, out.path
	if a.Type == "let" && !variableName.MatchString(stringParam(p, "name")) {
		return invalid(path+"/params/name", "invalid variable name")
	}

	return nil
}

func (out *compiledAction) compileArrayPredicate(phase string, nodes *int) error {
	a := out.raw
	p, path := a.Params, out.path
	if a.Type == "array_filter" {
		c, e := asCondition(p["predicate"], path+"/params/predicate")
		if e != nil {
			return e
		}
		out.predicate, e = compileCondition(c, path+"/params/predicate", phase, true, 0, nodes)
		if e != nil {
			return e
		}
	}

	return nil
}

func (out *compiledAction) validateArrayPosition(phase string, nodes *int) error {
	a := out.raw
	p, path := a.Params, out.path
	if a.Type == "array_insert" {
		_, has := p["index"]
		if p["position"] == "index" && !has {
			return invalid(path+"/params/index", "required when position is index")
		}
		if p["position"] != "index" && has {
			return invalid(path+"/params/index", "only valid when position is index")
		}
	}

	return nil
}

func (out *compiledAction) compileTextPattern(phase string, nodes *int) error {
	a := out.raw
	p, path := a.Params, out.path
	var e error
	if a.Type != "text_replace" && a.Type != "drop_input_items" {
		return nil
	}

	pattern := stringParam(p, "pattern")
	if a.Type == "drop_input_items" || p["match"] == "regex" {
		if pattern != "" {
			out.re, e = validRegex(pattern, boolParam(p, "case_insensitive"), boolParam(p, "dot_all"), boolParam(p, "multiline"))
			if e != nil {
				return invalid(path+"/params/pattern", "invalid regular expression: %v", e)
			}
		}
	} else {
		for _, k := range []string{"case_insensitive", "dot_all", "multiline"} {
			if boolParam(p, k) {
				return invalid(path+"/params/"+k, "requires match=regex")
			}
		}
	}

	return nil
}

func (out *compiledAction) validateRemovalPaths(phase string, nodes *int) error {
	a := out.raw
	p, path := a.Params, out.path
	if a.Type == "json_remove" {
		for i, v := range p["paths"].([]any) {
			if v == "" {
				return invalid(fmt.Sprintf("%s/params/paths/%d", path, i), "root removal is forbidden")
			}
		}
	}

	return nil
}

func (out *compiledAction) validateTransferPaths(phase string, nodes *int) error {
	a := out.raw
	p, path := a.Params, out.path
	if a.Type != "json_transfer" {
		return nil
	}
	source := stringParam(p, "source")
	sp := stringParam(p, "source_path")
	tp := stringParam(p, "target_path")
	if e := validateSource(source, sp, path+"/params", phase, false); e != nil {
		if ve, ok := e.(*ValidationError); ok && ve.Path == path+"/params/path" {
			ve.Path = path + "/params/source_path"
		}
		return e
	}
	if p["operation"] == "move" {
		if source != "current" {
			return invalid(path+"/params/source", "move requires current source")
		}
		if sp == "" {
			return invalid(path+"/params/source_path", "moving root is forbidden")
		}
		if strings.HasPrefix(tp, sp+"/") {
			return invalid(path+"/params/target_path", "cannot move into source subtree")
		}
	}

	return nil
}

func (out *compiledAction) validateMergeValue(phase string, nodes *int) error {
	a := out.raw
	p, path := a.Params, out.path
	if a.Type == "json_merge" && !isReference(p["value"]) && !isComputed(p["value"]) {
		if _, ok := object(expressionLiteral(p["value"])); !ok {
			return invalid(path+"/params/value", "merge value must be an object or reference")
		}
	}

	return nil
}

func validateFieldString(v any, f FieldSpec, path, phase string, item bool) error {
	s, ok := v.(string)
	if !ok {
		return invalid(path, mustBeTypeMessage, f.Type)
	}
	if f.NonEmpty && (s == "" || (f.Name == "model" && strings.TrimSpace(s) == "")) {
		return invalid(path, "must be nonempty")
	}
	if len(f.Enum) > 0 && !contains(f.Enum, s) {
		return invalid(path, "must be one of %s", strings.Join(f.Enum, ", "))
	}
	if f.Type == "pointer" {
		if _, e := pointerSegments(s); e != nil {
			return invalid(path, "%v", e)
		}
	}

	return nil
}

func validateFieldInteger(v any, f FieldSpec, path, phase string, item bool) error {
	n, ok := number(v)
	if !ok || math.Trunc(n) != n || math.Abs(n) > float64(MaxSafeInteger) {
		return invalid(path, "must be a JavaScript-safe integer")
	}
	if f.Minimum != nil && n < float64(*f.Minimum) {
		return invalid(path, "must be >= %d", *f.Minimum)
	}
	if f.Maximum != nil && n > float64(*f.Maximum) {
		return invalid(path, "must be <= %d", *f.Maximum)
	}

	return nil
}

func validateFieldStringArray(v any, f FieldSpec, path, phase string, item bool) error {
	a, ok := asArray(v)
	if !ok {
		return invalid(path, mustBeTypeMessage, f.Type)
	}
	for i, x := range a {
		typ := "string"
		if f.Type == "pointer_array" {
			typ = "pointer"
		}
		if e := validateFieldValue(x, FieldSpec{Type: typ}, fmt.Sprintf("%s/%d", path, i), phase, item); e != nil {
			return e
		}
	}

	return nil
}

func validateFieldValueArray(v any, f FieldSpec, path, phase string, item bool) error {
	a, ok := asArray(v)
	if !ok {
		return invalid(path, mustBeTypeMessage, f.Type)
	}
	for i, x := range a {
		if e := validateExpression(x, fmt.Sprintf("%s/%d", path, i), phase, item, 0); e != nil {
			return e
		}
	}

	return nil
}

func validateFieldConditionArray(v any, f FieldSpec, path, phase string, item bool) error {
	switch v.(type) {
	case []Condition, []any:
	default:
		return invalid(path, mustBeTypeMessage, f.Type)
	}

	return nil
}

func validateRuleMetadata(r Rule, path string) error {
	bad := func(k, msg string) error { return invalid(joinPointer(path, k), "%s", msg) }
	if int64(r.Priority) < -2147483648 || int64(r.Priority) > 2147483647 {
		return bad("priority", "must be an integer in -2147483648..2147483647")
	}
	if r.OrderIndex < -MaxSafeInteger || r.OrderIndex > MaxSafeInteger {
		return bad("order_index", "must be a safe integer")
	}
	if r.Revision < 0 || r.Revision > MaxSafeInteger {
		return bad("revision", "must be a nonnegative safe integer")
	}
	for k, v := range map[string]string{"created_at": r.CreatedAt, "updated_at": r.UpdatedAt} {
		if v != "" {
			if _, e := time.Parse(time.RFC3339Nano, v); e != nil {
				return bad(k, "must be an RFC3339 timestamp")
			}
		}
	}

	return nil
}

func normalizeFieldParams(params map[string]any, path string) (map[string]any, error) {
	// Normalize programmatically generated recipe ASTs exactly like submitted JSON.
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, invalid(path, "%v", err)
		}
		v, err := decodeAny(raw, path)
		if err != nil {
			return nil, err
		}
		return v.(map[string]any), nil
	}

	return params, nil
}

func validateOneField(params map[string]any, f FieldSpec, path, phase string, item bool) (any, bool, error) {
	v, exists := params[f.Name]
	p := joinPointer(path, f.Name)
	if !exists {
		if f.Required {
			return nil, false, invalid(p, "required field is missing")
		}
		if f.Default != nil {
			return mustClone(f.Default), true, nil
		}
		return nil, false, nil
	}
	if e := validateFieldValue(v, f, p, phase, item); e != nil {
		return nil, false, e
	}
	c, e := cloneJSON(v)
	if e != nil {
		return nil, false, invalid(p, "%v", e)
	}
	return c, true, nil
}

func (out *compiledCondition) compileChildren(phase string, item bool, depth int, nodes *int) error {
	c, path := out.raw, out.path
	if c.Op == "all" || c.Op == "any" || c.Op == "not" {
		if len(c.Conditions) == 0 {
			return invalid(path+"/conditions", "empty groups are forbidden; use always")
		}
		if c.Op == "not" && len(c.Conditions) != 1 {
			return invalid(path+"/conditions", "not requires exactly one condition")
		}
		for i, ch := range c.Conditions {
			cc, e := compileCondition(ch, fmt.Sprintf("%s/conditions/%d", path, i), phase, item, depth+1, nodes)
			if e != nil {
				return e
			}
			out.children = append(out.children, cc)
		}
	}
	return nil
}

func (out *compiledCondition) validateComparison() error {
	c, path := out.raw, out.path

	if c.Op == "regex" || c.Op == "not_regex" {
		re, e := validRegex(c.Value.(string), c.CaseInsensitive, c.DotAll, c.Multiline)
		if e != nil {
			return invalid(path+valuePointer, "invalid regular expression: %v", e)
		}
		out.re = re
	}
	if contains([]string{"gt", "gte", "lt", "lte"}, c.Op) && !isReference(c.Value) && !isComputed(c.Value) {
		if _, ok := number(expressionLiteral(c.Value)); !ok {
			return invalid(path+valuePointer, "numeric comparison requires a number or reference")
		}
	}
	if (c.Op == "in" || c.Op == "not_in") && !isReference(c.Value) && !isComputed(c.Value) {
		if _, ok := expressionLiteral(c.Value).([]any); !ok {
			return invalid(path+valuePointer, "membership requires an array or reference")
		}
	}
	return nil
}

func validateReference(ref any, path, phase string, item bool) error {
	r, ok := ref.(map[string]any)
	if !ok {
		return invalid(path+"/$ref", "must be an object")
	}
	if _, e := validateFields(r, selectorFields(), path+"/$ref", phase, item); e != nil {
		return e
	}
	source, _ := r["source"].(string)
	p, _ := r["path"].(string)
	return validateSource(source, p, path+"/$ref", phase, item)
}
