package rules

import (
	"context"
	"fmt"
	"strings"
)

type terminal struct {
	blocked bool
	status  int
	reason  string
	dropped bool
}

func (a compiledAction) execute(ctx context.Context, s *evaluation) (terminal, error) {
	if e := ctx.Err(); e != nil {
		return terminal{}, e
	}
	p := a.raw.Params
	path := stringParam(p, "path")
	switch a.raw.Type {
	case "sequence", "if", "for_each", "walk", "scope", "call", "let":
		return a.executeFlow(ctx, s)
	case "json_set":
		return a.executeJsonSet(ctx, s, p, path)
	case "json_remove":
		return a.executeJsonRemove(s, p, path)
	case "json_merge":
		return a.executeJsonMerge(ctx, s, p, path)
	case "json_transfer":
		return terminal{}, a.transfer(s)
	case "array_insert":
		return a.executeArrayInsert(ctx, s, p, path)
	case "array_filter":
		return a.executeArrayFilter(ctx, s, p, path)
	case "text_replace":
		return a.executeTextReplace(s, p, path)
	case "reject_request":
		status, _ := number(p["status"])
		return terminal{blocked: true, status: int(status), reason: stringParam(p, "message")}, nil
	case "drop_event":
		return a.executeDropEvent(s, p, path)
	}
	return terminal{}, fmt.Errorf("unsupported action %q", a.raw.Type)
}
func onMissing(p map[string]any) error {
	if p["on_missing"] == "error" {
		return fmt.Errorf("path does not exist")
	}
	return nil
}
func mergeObjects(target, incoming any, deep, appendArrays bool) {
	for _, k := range keys(incoming) {
		v, _ := objGet(incoming, k)
		old, exists := objGet(target, k)
		if exists && mergeObjectMember(target, k, old, v, deep, appendArrays) {
			continue
		}
		objSet(target, k, v)
	}
}
func (a compiledAction) transfer(s *evaluation) error {
	p := a.raw.Params
	source := stringParam(p, "source")
	sp := stringParam(p, "source_path")
	tp := stringParam(p, "target_path")
	v, exists, e := s.selectValue(source, sp, "value")
	if e != nil {
		return e
	}
	if !exists {
		return onMissing(p)
	}
	if source == "current" && sp == tp {
		return nil
	}
	if _, found, e := s.selectValue("current", tp, ""); e != nil {
		return e
	} else if found && !boolParam(p, "overwrite") {
		return fmt.Errorf("target already exists")
	}
	v, e = cloneJSON(v)
	if e != nil {
		return e
	}
	if p["operation"] == "move" {
		s.body, e = pointerRemove(s.body, sp, true)
		if e != nil {
			return e
		}
	}
	s.body, e = pointerSet(s.body, tp, v, boolParam(p, "create_parents"), false)
	return e
}

func (a compiledAction) executeJsonSet(ctx context.Context, s *evaluation, p map[string]any, path string) (terminal, error) {
	if p["if_exists"] == "keep" {
		if _, exists, e := s.selectValue("current", path, ""); e != nil {
			return terminal{}, e
		} else if exists {
			return terminal{}, nil
		}
	}
	v, e := s.requiredExpression(ctx, p["value"])
	if e != nil {
		return terminal{}, e
	}
	s.body, e = pointerSet(s.body, path, v, boolParam(p, "create_parents"), p["if_exists"] == "keep")
	return terminal{}, e

}

func (a compiledAction) executeJsonRemove(s *evaluation, p map[string]any, path string) (terminal, error) {
	for _, raw := range p["paths"].([]any) {
		if err := s.requireAvailable("current", raw.(string)); err != nil {
			return terminal{}, err
		}
		var e error
		s.body, e = pointerRemove(s.body, raw.(string), p["on_missing"] == "error")
		if e != nil {
			return terminal{}, e
		}
	}
	return terminal{}, nil

}

func (a compiledAction) executeJsonMerge(ctx context.Context, s *evaluation, p map[string]any, path string) (terminal, error) {
	incoming, e := s.requiredExpression(ctx, p["value"])
	if e != nil {
		return terminal{}, e
	}
	if _, ok := object(incoming); !ok {
		return terminal{}, fmt.Errorf("merge value must be an object")
	}
	target, exists, e := s.selectValue("current", path, "")
	if e != nil {
		return terminal{}, e
	}
	if !exists {
		if !boolParam(p, "create_parents") {
			return terminal{}, fmt.Errorf("merge target does not exist")
		}
		target = map[string]any{}
	}
	if _, ok := object(target); !ok {
		return terminal{}, fmt.Errorf("merge target must be an object")
	}
	mergeObjects(target, incoming, p["mode"] == "deep", p["array_mode"] == "append")
	s.body, e = pointerSet(s.body, path, target, boolParam(p, "create_parents"), false)
	return terminal{}, e

}

func (a compiledAction) executeArrayInsert(ctx context.Context, s *evaluation, p map[string]any, path string) (terminal, error) {
	v, exists, e := s.selectValue("current", path, "")
	if e != nil {
		return terminal{}, e
	}
	if !exists {
		if !boolParam(p, "create_if_missing") {
			return terminal{}, fmt.Errorf("array does not exist")
		}
		v = []any{}
	}
	arr, ok := v.([]any)
	if !ok {
		return terminal{}, fmt.Errorf("target must be an array")
	}
	values := make([]any, 0, len(p["values"].([]any)))
	for _, x := range p["values"].([]any) {
		v, e := s.requiredExpression(ctx, x)
		if e != nil {
			return terminal{}, e
		}
		values = append(values, v)
	}
	index := len(arr)
	if p["position"] == "prepend" {
		index = 0
	}
	if p["position"] == "index" {
		n, _ := number(p["index"])
		if n > float64(len(arr)) {
			return terminal{}, fmt.Errorf("index exceeds array length")
		}
		index = int(n)
	}
	out := make([]any, 0, len(arr)+len(values))
	out = append(out, arr[:index]...)
	out = append(out, values...)
	out = append(out, arr[index:]...)
	s.body, e = pointerSet(s.body, path, out, boolParam(p, "create_if_missing"), false)
	return terminal{}, e

}

func (a compiledAction) executeArrayFilter(ctx context.Context, s *evaluation, p map[string]any, path string) (terminal, error) {
	v, exists, e := s.selectValue("current", path, "")
	if e != nil {
		return terminal{}, e
	}
	if !exists {
		return terminal{}, onMissing(p)
	}
	arr, ok := v.([]any)
	if !ok {
		return terminal{}, fmt.Errorf("target must be an array")
	}
	kept := make([]any, 0, len(arr))
	for i, item := range arr {
		nested := *s
		nested.item = item
		nested.index = i
		nested.hasItem = true
		match, e := a.predicate.matches(ctx, &nested)
		if e != nil {
			return terminal{}, e
		}
		if match == (p["mode"] == "keep_matches") {
			kept = append(kept, item)
		}
	}
	s.body, e = pointerSet(s.body, path, kept, false, false)
	return terminal{}, e

}

func (a compiledAction) executeTextReplace(s *evaluation, p map[string]any, path string) (terminal, error) {
	v, exists, e := s.selectValue("current", path, "")
	if e != nil {
		return terminal{}, e
	}
	if !exists {
		return terminal{}, onMissing(p)
	}
	text, ok := v.(string)
	if !ok {
		return terminal{}, fmt.Errorf("text target must be a string")
	}
	pattern := stringParam(p, "pattern")
	replacement := stringParam(p, "replacement")
	updated := text
	if p["match"] == "regex" {
		if boolParam(p, "replace_all") {
			updated = a.re.ReplaceAllString(text, replacement)
		} else if match := a.re.FindStringSubmatchIndex(text); match != nil {
			updated = text[:match[0]] + string(a.re.ExpandString(nil, replacement, text, match)) + text[match[1]:]
		}
	} else {
		n := 1
		if boolParam(p, "replace_all") {
			n = -1
		}
		updated = strings.Replace(text, pattern, replacement, n)
	}
	s.body, e = pointerSet(s.body, path, updated, false, false)
	return terminal{}, e

}

func (a compiledAction) executeDropEvent(s *evaluation, p map[string]any, path string) (terminal, error) {
	if !droppableEvent(s.eventType, s.body) {
		return terminal{}, fmt.Errorf("event %q cannot be dropped: terminal, error and tool associations are protected", s.eventType)
	}
	return terminal{dropped: true}, nil
}

func mergeObjectMember(target any, k string, old, v any, deep, appendArrays bool) bool {
	if oa, ok := old.([]any); ok && appendArrays {
		if va, ok := v.([]any); ok {
			objSet(target, k, append(oa, va...))
			return true
		}
	}
	if deep {
		_, a := object(old)
		_, b := object(v)
		if a && b {
			mergeObjects(old, v, deep, appendArrays)
			return true
		}
	}
	return false
}
