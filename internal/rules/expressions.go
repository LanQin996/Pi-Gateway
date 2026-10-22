package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const MaxExecutionSteps = 100000

func (s *evaluation) tick(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.steps == nil {
		n := 0
		s.steps = &n
	}
	*s.steps++
	if *s.steps > MaxExecutionSteps {
		return fmt.Errorf("execution exceeds %d steps", MaxExecutionSteps)
	}
	return nil
}

// Expressions are explicit: ordinary JSON objects remain literal. Each argument
// is itself a literal, reference or expression, preserving missing versus null.
func (s *evaluation) compute(ctx context.Context, e map[string]any) (any, bool, error) {
	if err := s.tick(ctx); err != nil {
		return nil, false, err
	}
	op := stringParam(e, "op")
	raw, _ := e["args"].([]any)
	args, present, early, err := s.expressionArguments(ctx, op, raw)
	if err != nil {
		return nil, false, err
	}
	if early != nil {
		return early.value, true, nil
	}
	if op == "coalesce" {
		return nil, true, nil
	}
	if op == "exists" {
		return present[0], true, nil
	}
	for _, exists := range present {
		if !exists {
			return nil, false, nil
		}
	}
	switch op {
	case "join":
		return computeJoin(op, args)
	case "array":
		return args, true, nil
	case "object":
		return computeObject(op, args)
	case "get":
		return computeGet(op, args)
	case "index":
		return computeIndex(op, args)
	case "type":
		return jsonType(args[0]), true, nil
	case "length":
		return computeLength(op, args)
	case "keys":
		return computeKeys(op, args)
	case "eq":
		return jsonEqual(args[0], args[1]), true, nil
	case "ne":
		return !jsonEqual(args[0], args[1]), true, nil
	case "contains":
		return computeContains(op, args)
	case "and":
		return true, true, nil
	case "or":
		return false, true, nil
	case "not":
		return computeNot(op, args)
	case "add", "subtract", "gt", "gte", "lt", "lte":
		return computeAdd(op, args)
	case "concat":
		return computeConcat(op, args)
	case "trim", "lower", "upper", "string", "pointer_escape", "json_parse", "json_stringify", "slice", "regex_replace":
		return computeTrim(op, args)
	}
	return nil, false, fmt.Errorf("unknown expression %q", op)
}

type computedValue struct{ value any }

func (s *evaluation) expressionArguments(ctx context.Context, op string, raw []any) ([]any, []bool, *computedValue, error) {
	args := make([]any, len(raw))
	present := make([]bool, len(raw))
	for i, value := range raw {
		v, exists, err := s.expression(ctx, value)
		if err != nil {
			return nil, nil, nil, err
		}
		args[i], present[i] = v, exists
		early, err := shortCircuitExpression(op, v, exists)
		if err != nil || early != nil {
			return nil, nil, early, err
		}
	}
	return args, present, nil, nil
}
func shortCircuitExpression(op string, v any, exists bool) (*computedValue, error) {
	if op == "type" && !exists {
		return &computedValue{"missing"}, nil
	}
	if op == "coalesce" && exists && v != nil {
		return &computedValue{v}, nil
	}
	if op != "and" && op != "or" {
		return nil, nil
	}
	b, ok := v.(bool)
	if !exists || !ok {
		return nil, fmt.Errorf("%s requires boolean arguments", op)
	}
	if op == "and" && !b || op == "or" && b {
		return &computedValue{b}, nil
	}
	return nil, nil
}

var expressionArity = map[string][2]int{
	"array": {0, 256}, "object": {0, 256}, "coalesce": {1, 256}, "concat": {1, 256}, "and": {1, 256}, "or": {1, 256},
	"join": {2, 2}, "get": {2, 2}, "index": {2, 2}, "eq": {2, 2}, "ne": {2, 2}, "contains": {2, 2}, "add": {2, 2}, "subtract": {2, 2}, "gt": {2, 2}, "gte": {2, 2}, "lt": {2, 2}, "lte": {2, 2},
	"type": {1, 1}, "length": {1, 1}, "keys": {1, 1}, "exists": {1, 1}, "not": {1, 1}, "trim": {1, 1}, "lower": {1, 1}, "upper": {1, 1}, "string": {1, 1}, "pointer_escape": {1, 1}, "json_parse": {1, 1}, "json_stringify": {1, 1}, "slice": {3, 3}, "regex_replace": {3, 3},
}

func validateComputed(v any, path, phase string, item bool, depth int) error {
	e, ok := v.(map[string]any)
	if !ok {
		return invalid(path, "must be an expression object")
	}
	for k := range e {
		if k != "op" && k != "args" {
			return invalid(joinPointer(path, k), "unknown expression field")
		}
	}
	op := stringParam(e, "op")
	limits, ok := expressionArity[op]
	if !ok {
		return invalid(path+"/op", "unknown expression operator")
	}
	args, ok := e["args"].([]any)
	if !ok || len(args) < limits[0] || len(args) > limits[1] || op == "object" && len(args)%2 != 0 {
		return invalid(path+"/args", "invalid argument count for %s", op)
	}
	for i, arg := range args {
		if err := validateExpression(arg, fmt.Sprintf("%s/args/%d", path, i), phase, item, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func invalidExpressionArguments(op string) (any, bool, error) {
	return nil, false, fmt.Errorf("invalid argument types for %s", op)
}
func computeJoin(op string, args []any) (any, bool, error) {
	items, ok := args[0].([]any)
	separator, sepOK := args[1].(string)
	if !ok || !sepOK {
		return invalidExpressionArguments(op)
	}
	parts := make([]string, len(items))
	for i, item := range items {
		part, ok := item.(string)
		if !ok {
			return invalidExpressionArguments(op)
		}
		parts[i] = part
	}
	return strings.Join(parts, separator), true, nil

}

func computeObject(op string, args []any) (any, bool, error) {
	out := map[string]any{}
	for i := 0; i < len(args); i += 2 {
		k, ok := args[i].(string)
		if !ok {
			return invalidExpressionArguments(op)
		}
		if _, duplicate := out[k]; duplicate {
			return nil, false, fmt.Errorf("duplicate object key %q", k)
		}
		out[k] = args[i+1]
	}
	return out, true, nil

}

func computeGet(op string, args []any) (any, bool, error) {
	p, ok := args[1].(string)
	if !ok {
		return invalidExpressionArguments(op)
	}
	return pointerGet(args[0], p)

}

func computeIndex(op string, args []any) (any, bool, error) {
	a, ok := args[0].([]any)
	n, numeric := number(args[1])
	if !ok || !numeric || n != float64(int(n)) || n < 0 {
		return invalidExpressionArguments(op)
	}
	if n >= float64(len(a)) {
		return nil, false, nil
	}
	return a[int(n)], true, nil

}

func computeLength(op string, args []any) (any, bool, error) {
	switch v := args[0].(type) {
	case string:
		return len([]rune(v)), true, nil
	case []any:
		return len(v), true, nil
	}
	if m, ok := object(args[0]); ok {
		return len(m), true, nil
	}
	return invalidExpressionArguments(op)

}

func computeKeys(op string, args []any) (any, bool, error) {
	if _, ok := object(args[0]); !ok {
		return invalidExpressionArguments(op)
	}
	out := []any{}
	for _, key := range keys(args[0]) {
		out = append(out, key)
	}
	return out, true, nil

}

func computeContains(op string, args []any) (any, bool, error) {
	v, ok := containsValue(args[0], args[1])
	if !ok {
		return invalidExpressionArguments(op)
	}
	return v, true, nil

}

func computeNot(op string, args []any) (any, bool, error) {
	b, ok := args[0].(bool)
	if !ok {
		return invalidExpressionArguments(op)
	}
	return !b, true, nil

}

func computeAdd(op string, args []any) (any, bool, error) {
	a, ao := number(args[0])
	b, bo := number(args[1])
	if !ao || !bo {
		return invalidExpressionArguments(op)
	}
	switch op {
	case "add":
		return a + b, true, nil
	case "subtract":
		return a - b, true, nil
	case "gt":
		return a > b, true, nil
	case "gte":
		return a >= b, true, nil
	case "lt":
		return a < b, true, nil
	default:
		return a <= b, true, nil
	}

}

func computeConcat(op string, args []any) (any, bool, error) {
	var out strings.Builder
	for _, v := range args {
		t, ok := v.(string)
		if !ok {
			return invalidExpressionArguments(op)
		}
		out.WriteString(t)
	}
	return out.String(), true, nil

}

func computeTrim(op string, args []any) (any, bool, error) {
	if op == "json_stringify" {
		b, err := json.Marshal(args[0])
		return string(b), err == nil, err
	}
	if op == "string" {
		return computeString(op, args)
	}
	t, ok := args[0].(string)
	if !ok {
		return invalidExpressionArguments(op)
	}
	switch op {
	case "trim":
		return strings.TrimSpace(t), true, nil
	case "lower":
		return strings.ToLower(t), true, nil
	case "upper":
		return strings.ToUpper(t), true, nil
	case "pointer_escape":
		return strings.ReplaceAll(strings.ReplaceAll(t, "~", "~0"), "/", "~1"), true, nil
	case "json_parse":
		v, err := decodeAny([]byte(t), "/expression")
		return v, err == nil, err
	case "slice":
		return computeStringSlice(op, t, args)
	case "regex_replace":
		p, a := args[1].(string)
		replacement, b := args[2].(string)
		if !a || !b {
			return invalidExpressionArguments(op)
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, false, err
		}
		return re.ReplaceAllString(t, replacement), true, nil
	}
	return nil, false, fmt.Errorf("unknown expression %q", op)
}

func computeString(op string, args []any) (any, bool, error) {
	switch v := args[0].(type) {
	case string:
		return v, true, nil
	case bool:
		return strconv.FormatBool(v), true, nil
	}
	if n, ok := number(args[0]); ok {
		return strconv.FormatFloat(n, 'f', -1, 64), true, nil
	}
	return invalidExpressionArguments(op)
}

func computeStringSlice(op, t string, args []any) (any, bool, error) {
	start, a := number(args[1])
	end, b := number(args[2])
	r := []rune(t)
	if !a || !b || start < 0 || end < start || start != float64(int(start)) || end != float64(int(end)) {
		return invalidExpressionArguments(op)
	}
	if end > float64(len(r)) {
		end = float64(len(r))
	}
	if start > end {
		start = end
	}
	return string(r[int(start):int(end)]), true, nil

}
