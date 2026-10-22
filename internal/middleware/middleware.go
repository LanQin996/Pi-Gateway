// Package middleware implements the pre-flight pipeline that runs between the
// client request and the upstream call. Middlewares may rewrite or drop the
// request before any bytes reach the backend.
package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"pi-gateway/internal/piwire"
)

// Decision tells the chain what to do with the request after a middleware ran.
type Decision int

const (
	// Continue keeps processing the request.
	Continue Decision = iota
	// Drop discards the request; the client receives Reason as an error.
	Drop
)

// Request is the mutable upstream request handed to middlewares.
type Request struct {
	// Body is the ordered Pi-shaped request body. Mutations are reflected upstream.
	Body *piwire.OrderedMap

	// Model is the resolved upstream model id.
	Model string
	// SessionID is the session identifier used for prompt_cache_key and headers.
	SessionID string

	// AccountID / APIKeyID identify the routing decision that already happened.
	AccountID int64
	APIKeyID  int64

	// Meta carries free-form facts a middleware may set for logging/capture.
	Meta map[string]any
}

// Result reports what a middleware changed, for the capture record.
type Result struct {
	Changes []string
	Dropped bool
	Reason  string
}

// Middleware is one stage of the pre-flight chain.
type Middleware interface {
	// Name is the stable identifier used in configuration and the web UI.
	Name() string
	// Describe returns a one-line human summary shown in the UI.
	Describe() string
	// DefaultConfig returns the default JSON config for this middleware.
	DefaultConfig() string
	// Apply mutates the request and reports what it did.
	Apply(ctx context.Context, req *Request, cfg map[string]any) (Result, error)
}

// Chain executes middlewares in order.
type Chain struct {
	entries []entry
}

type entry struct {
	mw      Middleware
	enabled bool
	order   int
	cfg     map[string]any
	rawCfg  string
}

// Registration is a middleware plus its persisted state.
type Registration struct {
	Name    string
	Enabled bool
	Order   int
	Config  string
}

// NewChain composes the enabled middlewares. Unknown names are ignored, and any
// middleware not present in the persisted rows is included with its defaults.
func NewChain(regs []Registration, registry map[string]Middleware) (*Chain, error) {
	state := map[string]Registration{}
	for _, r := range regs {
		state[r.Name] = r
	}

	var entries []entry
	for name, mw := range registry {
		r, ok := state[name]
		if !ok {
			// Not configured yet: run with defaults so behaviour is predictable.
			entries = append(entries, entry{mw: mw, enabled: true, order: 100, cfg: map[string]any{}, rawCfg: mw.DefaultConfig()})
			continue
		}
		cfg := map[string]any{}
		if strings.TrimSpace(r.Config) != "" {
			if err := json.Unmarshal([]byte(r.Config), &cfg); err != nil {
				return nil, fmt.Errorf("middleware %s: invalid config JSON: %w", name, err)
			}
		}
		entries = append(entries, entry{mw: mw, enabled: r.Enabled, order: r.Order, cfg: cfg, rawCfg: r.Config})
	}

	// Deterministic execution: order index, then name.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].order != entries[j].order {
			return entries[i].order < entries[j].order
		}
		return entries[i].mw.Name() < entries[j].mw.Name()
	})

	return &Chain{entries: entries}, nil
}

// Apply runs the chain. It stops at the first middleware that drops the request.
func (c *Chain) Apply(ctx context.Context, req *Request) (Result, error) {
	var total Result
	if req.Meta == nil {
		req.Meta = map[string]any{}
	}
	for _, e := range c.entries {
		if !e.enabled {
			continue
		}
		res, err := e.mw.Apply(ctx, req, e.cfg)
		if err != nil {
			return total, fmt.Errorf("middleware %s: %w", e.mw.Name(), err)
		}
		if res.Dropped {
			total.Dropped = true
			total.Reason = res.Reason
			total.Changes = append(total.Changes, e.mw.Name()+": dropped ("+res.Reason+")")
			return total, nil
		}
		for _, ch := range res.Changes {
			total.Changes = append(total.Changes, e.mw.Name()+": "+ch)
		}
	}
	return total, nil
}

// Names returns the ordered, enabled middleware names.
func (c *Chain) Names() []string {
	out := []string{}
	for _, e := range c.entries {
		if e.enabled {
			out = append(out, e.mw.Name())
		}
	}
	return out
}

// ---- built-in middlewares ----

// Registry is the set of available middlewares.
func Registry() map[string]Middleware {
	list := []Middleware{
		&DropEnvironmentContext{},
		&DropInputItems{},
		&DropTools{},
		&DropFields{},
		&RewriteModel{},
		&PassthroughFields{},
		&SetReasoning{},
		&BlockPrompt{},
	}
	out := make(map[string]Middleware, len(list))
	for _, mw := range list {
		out[mw.Name()] = mw
	}
	return out
}

// DropEnvironmentContext removes Codex CLI's <environment_context> block.
//
// The official Codex client injects an <environment_context> item (cwd, sandbox
// policy, timezone, ...) into the conversation. Pi never sends it. Since the goal
// is to be indistinguishable from Pi on the wire, the block is removed rather
// than rewritten.
type DropEnvironmentContext struct{}

func (m *DropEnvironmentContext) Name() string { return "drop_environment_context" }
func (m *DropEnvironmentContext) Describe() string {
	return "Remove Codex <environment_context> blocks and their metadata from instructions/input"
}
func (m *DropEnvironmentContext) DefaultConfig() string {
	return `{"content_item_kind":"environments.environment_context","also_strip_from_instructions":true}`
}

var (
	envContextTagRE = regexp.MustCompile(`(?is)<environment_context>.*?</environment_context>`)
	envContextOpen  = regexp.MustCompile(`(?is)<environment_context>.*$`)
)

func (m *DropEnvironmentContext) Apply(_ context.Context, req *Request, cfg map[string]any) (Result, error) {
	var res Result
	kind := "environments.environment_context"
	if v, ok := cfg["content_item_kind"].(string); ok && v != "" {
		kind = v
	}
	alsoInstructions := true
	if v, ok := cfg["also_strip_from_instructions"].(bool); ok {
		alsoInstructions = v
	}

	input, _ := req.Body.Get("input")
	if arr, ok := input.([]any); ok {
		filtered, removed, cleaned := stripEnvironmentItems(arr, kind)
		if removed > 0 || cleaned > 0 {
			req.Body.Set("input", filtered)
			appendEnvironmentChanges(&res, removed, cleaned)
		}
	} else if s, ok := input.(string); ok && envContextTagRE.MatchString(s) {
		// Response inputs may be a bare string as well as an array. Without this
		// branch the environment block would be forwarded verbatim, leaking local
		// context upstream.
		req.Body.Set("input", strings.TrimSpace(envContextTagRE.ReplaceAllString(s, "")))
		res.Changes = append(res.Changes, "stripped environment_context from string input")
	}

	if alsoInstructions {
		stripEnvironmentInstructions(req, &res)
	}
	return res, nil
}

// stripEnvironmentItems removes content items whose declared kind is the
// environment context, plus any item whose text still contains the XML block.
func stripEnvironmentItems(items []any, kind string) ([]any, int, int) {
	out := make([]any, 0, len(items))
	removed, cleaned := 0, 0
	for _, item := range items {
		include, count := stripEnvironmentItem(item, kind)
		cleaned += count
		if !include {
			removed++
			continue
		}
		out = append(out, item)
	}
	return out, removed, cleaned
}

func stripEnvironmentItem(item any, kind string) (bool, int) {
	obj, ok := item.(map[string]any)
	if !ok {
		return true, 0
	}
	kinds := contentItemKinds(obj)
	content, contentOK := obj["content"].([]any)
	if !contentOK {
		return stripEnvironmentTextItem(obj, kind, kinds)
	}
	kept, cleaned := stripEnvironmentParts(content, kind, kinds)
	if len(kept) == 0 {
		return false, cleaned
	}
	if len(kept) != len(content) {
		obj["content"] = kept
	}
	stripKindMetadata(obj, len(kept))
	return true, cleaned
}

func stripEnvironmentTextItem(obj map[string]any, kind string, kinds []string) (bool, int) {
	cleaned := 0
	if text, ok := obj["content"].(string); ok && envContextTagRE.MatchString(text) {
		trimmed := strings.TrimSpace(envContextTagRE.ReplaceAllString(text, ""))
		if trimmed == "" {
			return false, 0
		}
		obj["content"] = trimmed
		cleaned++
	}
	return !dropFullyEmptyContentItem(obj, kind, kinds), cleaned
}

func stripEnvironmentParts(content []any, kind string, kinds []string) ([]any, int) {
	kept := make([]any, 0, len(content))
	cleaned := 0
	for i, part := range content {
		if i < len(kinds) && kinds[i] == kind {
			continue
		}
		include, changed := stripEnvironmentPart(part)
		if !include {
			continue
		}
		if changed {
			cleaned++
		}
		kept = append(kept, part)
	}
	return kept, cleaned
}

func stripEnvironmentPart(part any) (bool, bool) {
	obj, ok := part.(map[string]any)
	if !ok {
		return true, false
	}
	text, ok := obj["text"].(string)
	if !ok || !envContextTagRE.MatchString(text) {
		return true, false
	}
	trimmed := strings.TrimSpace(envContextTagRE.ReplaceAllString(text, ""))
	if trimmed == "" {
		return false, false
	}
	obj["text"] = trimmed
	return true, true
}

func dropFullyEmptyContentItem(obj map[string]any, kind string, kinds []string) bool {
	if len(kinds) == 0 {
		return false
	}
	for _, k := range kinds {
		if k != kind {
			return false
		}
	}
	return true
}

func contentItemKinds(obj map[string]any) []string {
	meta, ok := obj["internal_chat_message_metadata_passthrough"].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := meta["content_item_kinds"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// stripKindMetadata trims the parallel metadata array so it stays aligned with content.
func stripKindMetadata(obj map[string]any, keptLen int) {
	meta, ok := obj["internal_chat_message_metadata_passthrough"].(map[string]any)
	if !ok {
		return
	}
	kinds, ok := meta["content_item_kinds"].([]any)
	if !ok {
		return
	}
	if len(kinds) == keptLen {
		return
	}
	if keptLen == 0 {
		delete(obj, "internal_chat_message_metadata_passthrough")
		return
	}
	// Without the removed indices recorded we cannot safely realign; drop the
	// metadata rather than risk a mismatched payload.
	delete(obj, "internal_chat_message_metadata_passthrough")
}

// DropInputItems drops input items by type, or by a text pattern.
type DropInputItems struct{}

func (m *DropInputItems) Name() string { return "drop_input_items" }
func (m *DropInputItems) Describe() string {
	return "Drop input items matching a type list or a text/JSON pattern"
}
func (m *DropInputItems) DefaultConfig() string {
	return `{"types":[],"pattern":"","mode":"drop_item","target":"text"}`
}

func (m *DropInputItems) Apply(_ context.Context, req *Request, cfg map[string]any) (Result, error) {
	var res Result
	types := stringSlice(cfg["types"])
	pattern := ""
	if v, ok := cfg["pattern"].(string); ok {
		pattern = v
	}
	if len(types) == 0 && pattern == "" {
		return res, nil
	}

	var re *regexp.Regexp
	if pattern != "" {
		var err error
		re, err = regexp.Compile("(?is)" + pattern)
		if err != nil {
			return res, fmt.Errorf("invalid pattern: %w", err)
		}
	}

	input, _ := req.Body.Get("input")
	arr, ok := input.([]any)
	if !ok {
		return res, nil
	}

	kept := make([]any, 0, len(arr))
	dropped := 0
	for _, item := range arr {
		if inputItemMatches(item, types, re) {
			dropped++
			continue
		}
		kept = append(kept, item)
	}
	if dropped > 0 {
		req.Body.Set("input", kept)
		res.Changes = append(res.Changes, fmt.Sprintf("dropped %d input item(s)", dropped))
	}
	return res, nil
}

// DropTools removes named tools from the request.
type DropTools struct{}

func (m *DropTools) Name() string { return "drop_tools" }
func (m *DropTools) Describe() string {
	return "Remove tools by name (or all tools) before the request is sent"
}
func (m *DropTools) DefaultConfig() string { return `{"names":[],"drop_all":false}` }

func (m *DropTools) Apply(_ context.Context, req *Request, cfg map[string]any) (Result, error) {
	var res Result
	dropAll := false
	if v, ok := cfg["drop_all"].(bool); ok {
		dropAll = v
	}
	names := stringSlice(cfg["names"])
	types := stringSlice(cfg["types"])
	if !dropAll && len(names) == 0 && len(types) == 0 {
		return res, nil
	}

	tools, ok := req.Body.Get("tools")
	if !ok {
		return res, nil
	}
	arr, ok := tools.([]any)
	if !ok {
		return res, nil
	}
	if dropAll {
		req.Body.Delete("tools")
		res.Changes = append(res.Changes, fmt.Sprintf("dropped all %d tool(s)", len(arr)))
		return res, nil
	}

	filter := toolFilter{names: names, types: types}
	kept := filter.apply(arr)
	removed := filter.removed
	if removed > 0 {
		if len(kept) == 0 {
			req.Body.Delete("tools")
		} else {
			req.Body.Set("tools", kept)
		}
		res.Changes = append(res.Changes, fmt.Sprintf("dropped %d tool(s)", removed))
	}
	removeExcludedToolChoice(req, &res, types, names, removed, len(kept))
	return res, nil
}

// DropFields removes arbitrary top-level body fields.
//
// This is empty by default: the builder already removes documented unsupported
// fields; operators can remove additional client options here.
type DropFields struct{}

func (m *DropFields) Name() string     { return "drop_fields" }
func (m *DropFields) Describe() string { return "Delete specific top-level body fields" }
func (m *DropFields) DefaultConfig() string {
	return `{"fields":[]}`
}

func (m *DropFields) Apply(_ context.Context, req *Request, cfg map[string]any) (Result, error) {
	var res Result
	for _, f := range stringSlice(cfg["fields"]) {
		if f == "" {
			continue
		}
		if req.Body.Has(f) {
			req.Body.Delete(f)
			res.Changes = append(res.Changes, "dropped field "+f)
		}
	}
	return res, nil
}

// RewriteModel forces or remaps the upstream model.
type RewriteModel struct{}

func (m *RewriteModel) Name() string          { return "rewrite_model" }
func (m *RewriteModel) Describe() string      { return "Override the upstream model id" }
func (m *RewriteModel) DefaultConfig() string { return `{"model":""}` }

func (m *RewriteModel) Apply(_ context.Context, req *Request, cfg map[string]any) (Result, error) {
	var res Result
	model, _ := cfg["model"].(string)
	if strings.TrimSpace(model) == "" {
		return res, nil
	}
	if req.Model == model {
		return res, nil
	}
	req.Model = model
	req.Body.Set("model", model)
	res.Changes = append(res.Changes, "model -> "+model)
	return res, nil
}

// PassthroughFields restores missing fields from the original client payload.
type PassthroughFields struct{}

func (m *PassthroughFields) Name() string { return "passthrough_fields" }
func (m *PassthroughFields) Describe() string {
	return "Restore missing client body fields verbatim, subject to route constraints"
}
func (m *PassthroughFields) DefaultConfig() string { return `{"fields":[]}` }

func (m *PassthroughFields) Apply(_ context.Context, req *Request, cfg map[string]any) (Result, error) {
	var res Result
	for _, f := range stringSlice(cfg["fields"]) {
		if f == "" || req.Body.Has(f) {
			continue
		}
		if v, ok := req.Meta["client_body"]; ok {
			if body, ok := v.(map[string]any); ok {
				if fv, ok := body[f]; ok && fv != nil {
					req.Body.Set(f, fv)
					res.Changes = append(res.Changes, "passed through field "+f)
				}
			}
		}
	}
	return res, nil
}

// SetReasoning pins the reasoning effort/summary sent upstream.
type SetReasoning struct{}

func (m *SetReasoning) Name() string          { return "set_reasoning" }
func (m *SetReasoning) Describe() string      { return "Force reasoning effort and summary" }
func (m *SetReasoning) DefaultConfig() string { return `{"effort":"","summary":""}` }

func (m *SetReasoning) Apply(_ context.Context, req *Request, cfg map[string]any) (Result, error) {
	var res Result
	effort, _ := cfg["effort"].(string)
	summary, _ := cfg["summary"].(string)
	if effort == "" && summary == "" {
		return res, nil
	}
	existing := map[string]any{}
	if v, ok := req.Body.Get("reasoning"); ok {
		if obj, ok := v.(map[string]any); ok {
			existing = obj
		}
	}
	if effort != "" {
		existing["effort"] = effort
	}
	if summary != "" {
		existing["summary"] = summary
	}
	req.Body.Set("reasoning", existing)
	res.Changes = append(res.Changes, "reasoning overridden")
	return res, nil
}

// BlockPrompt drops the whole request when the payload matches a pattern.
type BlockPrompt struct{}

func (m *BlockPrompt) Name() string { return "block_prompt" }
func (m *BlockPrompt) Describe() string {
	return "Refuse the request when the body matches a pattern (request is never sent)"
}
func (m *BlockPrompt) DefaultConfig() string { return `{"pattern":""}` }

func (m *BlockPrompt) Apply(_ context.Context, req *Request, cfg map[string]any) (Result, error) {
	pattern, _ := cfg["pattern"].(string)
	if strings.TrimSpace(pattern) == "" {
		return Result{}, nil
	}
	re, err := regexp.Compile("(?is)" + pattern)
	if err != nil {
		return Result{}, fmt.Errorf("invalid pattern: %w", err)
	}
	raw := mustJSON(req.Body)
	if re.MatchString(raw) {
		return Result{Dropped: true, Reason: "request blocked by middleware policy"}, nil
	}
	return Result{}, nil
}

// ---- helpers ----

func stringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if t == "" {
			return nil
		}
		parts := strings.Split(t, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	default:
		return nil
	}
}

func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

func removeExcludedToolChoice(req *Request, res *Result, types, names []string, removed, keptCount int) {
	if choice, ok := req.Body.Get("tool_choice"); ok {
		obj, isObject := choice.(map[string]any)
		matched := false
		if isObject {
			kind, _ := obj["type"].(string)
			name, _ := obj["name"].(string)
			matched = containsString(types, kind) || containsString(names, name)
		} else if value, ok := choice.(string); ok {
			matched = containsString(types, value) || (value == "required" && removed > 0 && keptCount == 0)
		}
		if matched {
			req.Body.Delete("tool_choice")
			res.Changes = append(res.Changes, "dropped excluded tool_choice")
		}
	}
}

type toolFilter struct {
	names, types []string
	removed      int
}

func (f *toolFilter) apply(items []any) []any {
	kept := make([]any, 0, len(items))
	for _, tool := range items {
		filtered, include := f.filterOne(tool)
		if !include {
			f.removed++
			continue
		}
		kept = append(kept, filtered)
	}
	return kept
}

func (f *toolFilter) filterOne(tool any) (any, bool) {
	obj, ok := tool.(map[string]any)
	if !ok {
		return tool, true
	}
	name, _ := obj["name"].(string)
	kind, _ := obj["type"].(string)
	if containsString(f.names, name) || containsString(f.types, kind) {
		return nil, false
	}
	children, ok := obj["tools"].([]any)
	if !ok || kind != "namespace" {
		return tool, true
	}
	snapshot := make(map[string]any, len(obj))
	for key, value := range obj {
		snapshot[key] = value
	}
	filtered := f.apply(children)
	if len(filtered) == 0 {
		return nil, false
	}
	snapshot["tools"] = filtered
	return snapshot, true
}

func inputItemMatches(item any, types []string, pattern *regexp.Regexp) bool {
	if obj, ok := item.(map[string]any); ok {
		kind, _ := obj["type"].(string)
		if kind != "" && containsString(types, kind) {
			return true
		}
		return pattern != nil && pattern.MatchString(mustJSON(obj))
	}
	text, ok := item.(string)
	return ok && pattern != nil && pattern.MatchString(text)
}

func stripEnvironmentInstructions(req *Request, res *Result) {
	value, _ := req.Body.Get("instructions")
	text, ok := value.(string)
	if !ok || !envContextTagRE.MatchString(text) {
		return
	}
	req.Body.Set("instructions", strings.TrimSpace(envContextTagRE.ReplaceAllString(text, "")))
	res.Changes = append(res.Changes, "stripped environment_context from instructions")
}

func appendEnvironmentChanges(res *Result, removed, cleaned int) {
	if removed > 0 {
		res.Changes = append(res.Changes, fmt.Sprintf("removed %d environment_context item(s)", removed))
	}
	if cleaned > 0 {
		res.Changes = append(res.Changes, fmt.Sprintf("stripped inline environment_context from %d item(s)", cleaned))
	}
}
