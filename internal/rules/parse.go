package rules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var ruleKeys = map[string]bool{"schema_version": true, "id": true, "name": true, "description": true, "enabled": true, "priority": true, "order_index": true, "phase": true, "when": true, "actions": true, "stop_after_match": true, "on_error": true, "revision": true, "created_at": true, "updated_at": true, "legacy_name": true, "source": true}
var conditionKeys = map[string]bool{"op": true, "source": true, "path": true, "encoding": true, "value": true, "conditions": true, "case_insensitive": true, "dot_all": true, "multiline": true}
var actionKeys = map[string]bool{"id": true, "type": true, "params": true}

func strictObject(data []byte, allowed map[string]bool, path string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, invalid(path, "invalid JSON object: %v", err)
	}
	if raw == nil {
		return nil, invalid(path, "must be a JSON object")
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, invalid(path, trailingJSONValueMessage)
	} else if err != io.EOF {
		return nil, invalid(path, trailingJSONValueMessage)
	}
	for k := range raw {
		if !allowed[k] {
			return nil, invalid(joinPointer(path, k), "unknown field")
		}
	}
	return raw, nil
}

func ParseRule(data []byte) (Rule, error) {
	if len(data) > MaxRuleBytes {
		return Rule{}, invalid("", "rule exceeds %d bytes", MaxRuleBytes)
	}
	if err := checkJSON(data, ""); err != nil {
		return Rule{}, err
	}
	raw, err := strictObject(data, ruleKeys, "")
	if err != nil {
		return Rule{}, err
	}
	for _, k := range []string{"schema_version", "name", "phase", "when", "actions"} {
		if _, ok := raw[k]; !ok {
			return Rule{}, invalid("/"+k, "required field is missing")
		}
	}
	r := Rule{SchemaVersion: SchemaVersion, Enabled: true, Priority: 100, Phase: PhaseRequest, When: Condition{Op: "always"}, Actions: []Action{}, StopAfterMatch: false, OnError: OnErrorAbort}
	for k, v := range raw {
		p := joinPointer("", k)
		switch k {
		case "schema_version":
			r.SchemaVersion, err = decodeInt(v, p)
		case "id":
			err = decodeString(v, p, &r.ID)
		case "name":
			err = decodeString(v, p, &r.Name)
		case "description":
			err = decodeString(v, p, &r.Description)
		case "enabled":
			err = decodeBool(v, p, &r.Enabled)
		case "priority":
			r.Priority, err = decodeInt(v, p)
		case "order_index":
			r.OrderIndex, err = decodeInt64(v, p)
		case "phase":
			err = decodeString(v, p, &r.Phase)
		case "when":
			err = decodeJSON(v, p, &r.When)
		case "actions":
			var items []json.RawMessage
			err = decodeJSONArray(v, p, &items)
			if err == nil {
				r.Actions = make([]Action, len(items))
				for i, b := range items {
					if err = decodeJSON(b, fmt.Sprintf("%s/%d", p, i), &r.Actions[i]); err != nil {
						break
					}
				}
			}
		case "stop_after_match":
			err = decodeBool(v, p, &r.StopAfterMatch)
		case "on_error":
			err = decodeString(v, p, &r.OnError)
		case "revision":
			r.Revision, err = decodeInt64(v, p)
		case "created_at":
			err = decodeString(v, p, &r.CreatedAt)
		case "updated_at":
			err = decodeString(v, p, &r.UpdatedAt)
		case "legacy_name":
			err = decodeString(v, p, &r.LegacyName)
		case "source":
			err = decodeString(v, p, &r.Source)
		}
		if err != nil {
			return Rule{}, err
		}
	}
	if err := ValidateRule(r, ""); err != nil {
		return Rule{}, err
	}
	return r, nil
}

func ParseRules(data []byte) ([]Rule, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, invalid("", "must be a JSON rule array")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raws []json.RawMessage
	if err := dec.Decode(&raws); err != nil {
		return nil, invalid("", "invalid JSON rule list: %v", err)
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, invalid("", trailingJSONValueMessage)
	} else if err != io.EOF {
		return nil, invalid("", trailingJSONValueMessage)
	}
	out := make([]Rule, 0, len(raws))
	for i, raw := range raws {
		r, err := ParseRule(raw)
		if err != nil {
			if ve, ok := err.(*ValidationError); ok {
				ve.Path = fmt.Sprintf("/%d%s", i, ve.Path)
			}
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (c *Condition) UnmarshalJSON(data []byte) error {
	raw, err := strictObject(data, conditionKeys, "")
	if err != nil {
		return err
	}
	*c = Condition{present: map[string]bool{}}
	for k := range raw {
		c.present[k] = true
	}
	if v, ok := raw["op"]; ok {
		if err = decodeString(v, "/op", &c.Op); err != nil {
			return err
		}
	}
	if v, ok := raw["source"]; ok {
		if err = decodeString(v, "/source", &c.Source); err != nil {
			return err
		}
	}
	if v, ok := raw["path"]; ok {
		if err = decodeString(v, "/path", &c.Path); err != nil {
			return err
		}
	}
	if v, ok := raw["encoding"]; ok {
		if err = decodeString(v, "/encoding", &c.Encoding); err != nil {
			return err
		}
	}
	if v, ok := raw["value"]; ok {
		c.ValuePresent = true
		c.Value, err = decodeAny(v, "/value")
		if err != nil {
			return err
		}
	}
	if v, ok := raw["conditions"]; ok {
		var items []json.RawMessage
		if err = decodeJSONArray(v, "/conditions", &items); err != nil {
			return err
		}
		c.Conditions = make([]Condition, len(items))
		for i, b := range items {
			if err = decodeJSON(b, fmt.Sprintf("/conditions/%d", i), &c.Conditions[i]); err != nil {
				return err
			}
		}
	}
	if v, ok := raw["case_insensitive"]; ok {
		if err = decodeBool(v, "/case_insensitive", &c.CaseInsensitive); err != nil {
			return err
		}
	}
	if v, ok := raw["dot_all"]; ok {
		if err = decodeBool(v, "/dot_all", &c.DotAll); err != nil {
			return err
		}
	}
	if v, ok := raw["multiline"]; ok {
		if err = decodeBool(v, "/multiline", &c.Multiline); err != nil {
			return err
		}
	}
	return nil
}

func (a *Action) UnmarshalJSON(data []byte) error {
	raw, err := strictObject(data, actionKeys, "")
	if err != nil {
		return err
	}
	*a = Action{}
	if v, ok := raw["id"]; ok {
		if err = decodeString(v, "/id", &a.ID); err != nil {
			return err
		}
	}
	if v, ok := raw["type"]; ok {
		if err = decodeString(v, "/type", &a.Type); err != nil {
			return err
		}
	}
	if v, ok := raw["params"]; ok {
		if string(v) == "null" {
			return invalid("/params", "must be an object")
		}
		if err = decodeJSON(v, "/params", &a.Params); err != nil {
			return err
		}
	}
	return nil
}

func decodeJSON(raw json.RawMessage, path string, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return prefixError(path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return invalid(path, trailingJSONValueMessage)
	} else if err != io.EOF {
		return invalid(path, "invalid value: %v", err)
	}
	return nil
}
func decodeJSONArray(raw json.RawMessage, path string, out *[]json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '[' {
		return invalid(path, "must be an array")
	}
	return decodeJSON(raw, path, out)
}
func decodeAny(raw json.RawMessage, path string) (any, error) {
	var v any
	if err := decodeJSON(raw, path, &v); err != nil {
		return nil, err
	}
	return normalizeNumbers(v, path)
}
func normalizeNumbers(v any, path string) (any, error) {
	switch t := v.(type) {
	case json.Number:
		return normalizeNumber(t.String(), path)
	case []any:
		for i, x := range t {
			n, e := normalizeNumbers(x, joinPointer(path, strconv.Itoa(i)))
			if e != nil {
				return nil, e
			}
			t[i] = n
		}
	case map[string]any:
		for k, x := range t {
			n, e := normalizeNumbers(x, joinPointer(path, k))
			if e != nil {
				return nil, e
			}
			t[k] = n
		}
	}
	return v, nil
}
func normalizeNumber(s, path string) (any, error) {
	if strings.ContainsAny(s, ".eE") {
		f, e := strconv.ParseFloat(s, 64)
		if e != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, invalid(path, "number must be finite")
		}
		if math.Trunc(f) == f && math.Abs(f) > float64(MaxSafeInteger) {
			return nil, invalid(path, unsafeIntegerMessage)
		}
		return f, nil
	}
	i, e := strconv.ParseInt(s, 10, 64)
	if e != nil || i > MaxSafeInteger || i < -MaxSafeInteger {
		return nil, invalid(path, unsafeIntegerMessage)
	}
	return i, nil
}
func decodeInt(raw json.RawMessage, path string) (int, error) {
	i, e := decodeInt64(raw, path)
	if e != nil {
		return 0, e
	}
	if int64(int(i)) != i {
		return 0, invalid(path, "integer out of range")
	}
	return int(i), nil
}
func decodeInt64(raw json.RawMessage, path string) (int64, error) {
	var n json.Number
	if err := decodeJSON(raw, path, &n); err != nil {
		return 0, invalid(path, "must be an integer")
	}
	s := n.String()
	if strings.ContainsAny(s, ".eE") {
		return 0, invalid(path, "must be an integer")
	}
	i, e := strconv.ParseInt(s, 10, 64)
	if e != nil || i > MaxSafeInteger || i < -MaxSafeInteger {
		return 0, invalid(path, unsafeIntegerMessage)
	}
	return i, nil
}
func decodeString(raw json.RawMessage, path string, out *string) error {
	var s any
	if err := decodeJSON(raw, path, &s); err != nil {
		return err
	}
	v, ok := s.(string)
	if !ok {
		return invalid(path, "must be a string")
	}
	*out = v
	return nil
}
func decodeBool(raw json.RawMessage, path string, out *bool) error {
	var v any
	if err := decodeJSON(raw, path, &v); err != nil {
		return err
	}
	b, ok := v.(bool)
	if !ok {
		return invalid(path, "must be a boolean")
	}
	*out = b
	return nil
}
func joinPointer(base, key string) string {
	esc := strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
	if base == "" {
		return "/" + esc
	}
	return base + "/" + esc
}
func pointerSegments(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, invalid(path, "must be an RFC 6901 JSON Pointer")
	}
	parts := strings.Split(path[1:], "/")
	if len(parts) > MaxDepth {
		return nil, invalid(path, "JSON Pointer exceeds %d segments", MaxDepth)
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		value, err := decodePointerSegment(p, path)
		if err != nil {
			return nil, err
		}
		out[i] = value
	}
	return out, nil
}
func validRegex(pattern string, ci, dot, multi bool) (*regexp.Regexp, error) {
	prefix := ""
	if ci {
		prefix += "(?i)"
	}
	if dot {
		prefix += "(?s)"
	}
	if multi {
		prefix += "(?m)"
	}
	re, err := regexp.Compile(prefix + pattern)
	if err != nil {
		return nil, err
	}
	return re, nil
}

func decodePointerSegment(p, path string) (string, error) {
	var b strings.Builder
	for j := 0; j < len(p); j++ {
		if p[j] != '~' {
			b.WriteByte(p[j])
			continue
		}
		if j+1 >= len(p) || (p[j+1] != '0' && p[j+1] != '1') {
			return "", invalid(path, "invalid ~ escape")
		}
		j++
		if p[j] == '0' {
			b.WriteByte('~')
		} else {
			b.WriteByte('/')
		}
	}
	return b.String(), nil
}
