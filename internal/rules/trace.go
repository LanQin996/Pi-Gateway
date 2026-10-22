package rules

import (
	"encoding/json"
	"fmt"
	"strings"
)

type traceCollector struct {
	traces    []Trace
	omitted   int
	enabled   bool
	maxBytes  int
	maxTraces int
	usedBytes int
}

func newTraceCollector(in *Input) *traceCollector {
	c := &traceCollector{enabled: in != nil && in.Trace, maxBytes: in.TraceMaxBytes, maxTraces: in.MaxTraces}
	if c.maxBytes <= 0 {
		c.maxBytes = 1 << 20
	}
	if c.maxTraces <= 0 {
		c.maxTraces = 4096
	}
	return c
}
func (c *traceCollector) remainingBytes() int { return c.maxBytes - c.usedBytes }
func (c *traceCollector) remainingCount() int { return c.maxTraces - len(c.traces) }
func (c *traceCollector) canCollect() bool {
	return c.enabled && len(c.traces) < c.maxTraces && c.usedBytes < c.maxBytes
}
func (c *traceCollector) add(t Trace) {
	if !c.enabled {
		return
	}
	if !c.canCollect() {
		c.omitted++
		return
	}
	t = boundTrace(t)
	b, _ := json.Marshal(t)
	if len(b) > c.maxBytes-c.usedBytes {
		c.omitted++
		return
	}
	c.traces = append(c.traces, t)
	c.usedBytes += len(b)
}
func (c *traceCollector) merge(other *traceCollector) {
	if !c.enabled || other == nil {
		return
	}
	for _, t := range other.traces {
		c.add(t)
	}
	c.omitted += other.omitted
}
func boundTrace(t Trace) Trace {
	for i := range t.Changes {
		change := &t.Changes[i]
		before, bt := boundValue(change.Before, change.Path)
		after, at := boundValue(change.After, change.Path)
		change.Before = before
		change.After = after
		change.Truncated = change.Truncated || bt || at
	}
	if len(t.Error) > 512 {
		t.Error = t.Error[:512]
	}
	return t
}
func sensitiveKey(k string) bool {
	k = strings.ReplaceAll(strings.ToLower(k), "-", "_")
	return contains([]string{"authorization", "proxy_authorization", "api_key", "x_api_key", "openai_api_key", "id_token", "client_secret", "access_token", "refresh_token", "password", "secret", "cookie", "set_cookie"}, k)
}
func redact(v any) any {
	if m, ok := object(v); ok {
		out := map[string]any{}
		for k, x := range m {
			if sensitiveKey(k) {
				out[k] = "[REDACTED]"
			} else {
				out[k] = redact(x)
			}
		}
		return out
	}
	if a, ok := v.([]any); ok {
		out := make([]any, len(a))
		for i, x := range a {
			out[i] = redact(x)
		}
		return out
	}
	return v
}
func boundValue(v any, path string) (any, bool) {
	parts, _ := pointerSegments(path)
	for _, k := range parts {
		if sensitiveKey(k) {
			return "[REDACTED]", false
		}
	}
	b, e := json.Marshal(v)
	if e != nil {
		return "<unserializable>", true
	}
	if len(b) > MaxTraceValueBytes {
		return fmt.Sprintf("<omitted %d bytes>", len(b)), true
	}
	return redact(v), false
}

type structuralDiffer struct {
	changes []Change
	omitted int
}

func structuralDiff(before, after any) ([]Change, int) {
	differ := structuralDiffer{changes: []Change{}}
	differ.walk(before, after, "")
	return differ.changes, differ.omitted
}

func (d *structuralDiffer) add(change Change) {
	if len(d.changes) >= MaxTraceChanges {
		d.omitted++
		return
	}
	d.changes = append(d.changes, change)
}

func (d *structuralDiffer) walk(a, b any, path string) {
	if jsonEqual(a, b) {
		return
	}
	_, aObject := object(a)
	_, bObject := object(b)
	if aObject && bObject {
		d.objects(a, b, path)
		return
	}
	aa, aArray := a.([]any)
	bb, bArray := b.([]any)
	if aArray && bArray {
		d.arrays(aa, bb, path)
		return
	}
	d.add(Change{Path: path, Operation: "replace", BeforeExists: true, AfterExists: true, Before: a, After: b})
}

func (d *structuralDiffer) objects(a, b any, path string) {
	seen := map[string]bool{}
	for _, key := range keys(a) {
		seen[key] = true
		av, _ := objGet(a, key)
		bv, exists := objGet(b, key)
		childPath := joinPointer(path, key)
		if !exists {
			d.add(Change{Path: childPath, Operation: "remove", BeforeExists: true, Before: av})
			continue
		}
		d.walk(av, bv, childPath)
	}
	for _, key := range keys(b) {
		if seen[key] {
			continue
		}
		value, _ := objGet(b, key)
		d.add(Change{Path: joinPointer(path, key), Operation: "add", AfterExists: true, After: value})
	}
}

func nextEqual(values []any, start int, target any) int {
	for index := start; index < len(values); index++ {
		if jsonEqual(values[index], target) {
			return index
		}
	}
	return -1
}

func (d *structuralDiffer) removeArrayRange(values []any, start, end int, path string) {
	for index := start; index < end; index++ {
		d.add(Change{Path: fmt.Sprintf("%s/%d", path, index), Operation: "remove", BeforeExists: true, Before: values[index]})
	}
}

func (d *structuralDiffer) addArrayRange(values []any, start, end int, path string) {
	for index := start; index < end; index++ {
		d.add(Change{Path: fmt.Sprintf("%s/%d", path, index), Operation: "add", AfterExists: true, After: values[index]})
	}
}

func (d *structuralDiffer) arrays(before, after []any, path string) {
	i, j := 0, 0
	for i < len(before) && j < len(after) {
		if jsonEqual(before[i], after[j]) {
			i++
			j++
			continue
		}
		// Align surviving messages after edits rather than replacing every later item.
		left := nextEqual(before, i+1, after[j])
		right := nextEqual(after, j+1, before[i])
		switch {
		case left >= 0 && (right < 0 || left-i <= right-j):
			d.removeArrayRange(before, i, left, path)
			i = left
		case right >= 0:
			d.addArrayRange(after, j, right, path)
			j = right
		default:
			d.walk(before[i], after[j], fmt.Sprintf("%s/%d", path, j))
			i++
			j++
		}
	}
	d.removeArrayRange(before, i, len(before), path)
	d.addArrayRange(after, j, len(after), path)
}
