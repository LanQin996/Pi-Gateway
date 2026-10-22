package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"pi-gateway/internal/middleware"
	"pi-gateway/internal/rules"
	"pi-gateway/internal/rulesruntime"
	"pi-gateway/internal/store"
)

// ruleWriteRequest keeps editable JSON intact until the engine's lossless parser
// can reject unsafe numbers, unknown fields and invalid language constructs.
type ruleWriteRequest struct {
	Rule             json.RawMessage `json:"rule"`
	ExpectedRevision int64           `json:"expected_revision"`
	ExpectedVersion  *int64          `json:"expected_version,omitempty"`
}

// decodeRuleRequest reuses the common JSON decoder while enforcing complete,
// bounded input and unknown-envelope rejection only for the new rule endpoints.
func decodeRuleRequest(r *http.Request, dst any) error {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return fmt.Errorf("request body exceeds 1 MiB")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if !json.Valid(data) {
		return fmt.Errorf("invalid JSON body: expected exactly one complete JSON value")
	}
	copyRequest := r.Clone(r.Context())
	copyRequest.Body = io.NopCloser(bytes.NewReader(data))
	var raw json.RawMessage
	if err := decodeJSON(copyRequest, &raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	return decoder.Decode(dst)
}

func (s *Server) ensureRules(w http.ResponseWriter, r *http.Request) bool {
	if err := rulesruntime.New(s.store).Ensure(r.Context()); err != nil {
		writeRuleError(w, err)
		return false
	}
	return true
}

func writeRuleError(w http.ResponseWriter, err error) {
	var validation *rules.ValidationError
	var conflict *store.RuleConflict
	switch {
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "conflict": conflict})
	case errors.Is(err, store.ErrRuleConflict), errors.Is(err, rulesruntime.ErrMigration):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrRuleNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.As(err, &validation):
		writeJSON(w, http.StatusBadRequest, map[string]any{"valid": false, "error": err.Error(), "errors": []any{validation}})
	case errors.Is(err, store.ErrRuleInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func positiveRuleQuery(r *http.Request, name string, fallback int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return n, nil
}

func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	page, err := positiveRuleQuery(r, "page", 1)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	pageSize, err := positiveRuleQuery(r, "page_size", 50)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.URL.Query().Has("limit") {
		pageSize, err = positiveRuleQuery(r, "limit", 50)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	filter := store.RuleListFilter{Page: page, PageSize: pageSize, Search: r.URL.Query().Get("search"), Phase: r.URL.Query().Get("phase")}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		offset, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || offset < 0 {
			writeErr(w, http.StatusBadRequest, "offset must be a nonnegative integer")
			return
		}
		filter.Offset = &offset
	}
	if filter.Search == "" {
		filter.Search = r.URL.Query().Get("q")
	}
	if filter.Phase != "" && !rules.IsPhase(filter.Phase) {
		writeErr(w, http.StatusBadRequest, "invalid phase")
		return
	}
	if value := r.URL.Query().Get("enabled"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "enabled must be a boolean")
			return
		}
		filter.Enabled = &enabled
	}
	if !s.ensureRules(w, r) {
		return
	}
	result, err := s.store.ListRules(r.Context(), filter)
	if err != nil {
		writeRuleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func snapshotRule(snapshot *store.RuleSetSnapshot, id string) *store.RuleRow {
	for _, row := range snapshot.Rules {
		if row.ID == id {
			return row
		}
	}
	return nil
}

func (s *Server) handleGetRule(w http.ResponseWriter, r *http.Request) {
	if !s.ensureRules(w, r) {
		return
	}
	snapshot, err := s.store.LoadRuleSet(r.Context())
	if err != nil {
		writeRuleError(w, err)
		return
	}
	row := snapshotRule(snapshot, r.PathValue("id"))
	if row == nil {
		writeRuleError(w, store.ErrRuleNotFound)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(row.Revision, 10)))
	writeJSON(w, http.StatusOK, map[string]any{"rule": row, "version": snapshot.Version})
}

func editableRule(raw json.RawMessage, id string) (json.RawMessage, error) {
	definition, err := rules.ParseRule(raw)
	if err != nil {
		return nil, err
	}
	definition.ID = id
	// Never persist client-supplied revision, ordering or provenance.
	definition.Revision, definition.OrderIndex = 0, 0
	definition.CreatedAt, definition.UpdatedAt, definition.LegacyName, definition.Source = "", "", "", ""
	if _, err := rules.Compile([]rules.Rule{definition}); err != nil {
		return nil, err
	}
	return rulesruntime.EditableJSON(definition)
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var body ruleWriteRequest
	if err := decodeRuleRequest(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ExpectedVersion == nil || *body.ExpectedVersion < 0 {
		writeErr(w, http.StatusBadRequest, "expected_version is required for create")
		return
	}
	id := uuid.NewString()
	raw, err := editableRule(body.Rule, id)
	if err != nil {
		writeRuleError(w, err)
		return
	}
	if !s.ensureRules(w, r) {
		return
	}
	snapshot, err := s.store.PublishRules(r.Context(), body.ExpectedVersion, []store.RuleChange{{Kind: "create", ID: id, Rule: raw, Source: "user"}}, rulesruntime.ValidateSnapshot)
	if err != nil {
		writeRuleError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), "rule.created", id)
	writeJSON(w, http.StatusCreated, map[string]any{"rule": snapshotRule(snapshot, id), "version": snapshot.Version})
}

func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	var body ruleWriteRequest
	if err := decodeRuleRequest(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ExpectedRevision < 1 {
		writeErr(w, http.StatusBadRequest, expectedRevisionRequiredMessage)
		return
	}
	id := r.PathValue("id")
	raw, err := editableRule(body.Rule, id)
	if err != nil {
		writeRuleError(w, err)
		return
	}
	if !s.ensureRules(w, r) {
		return
	}
	snapshot, err := s.store.PublishRules(r.Context(), body.ExpectedVersion, []store.RuleChange{{Kind: "update", ID: id, ExpectedRevision: body.ExpectedRevision, Rule: raw}}, rulesruntime.ValidateSnapshot)
	if err != nil {
		writeRuleError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), "rule.updated", id)
	writeJSON(w, http.StatusOK, map[string]any{"rule": snapshotRule(snapshot, id), "version": snapshot.Version})
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExpectedRevision int64  `json:"expected_revision"`
		ExpectedVersion  *int64 `json:"expected_version,omitempty"`
	}
	if r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0 {
		if err := decodeRuleRequest(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if raw := r.URL.Query().Get("expected_revision"); raw != "" {
		revision, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || (body.ExpectedRevision != 0 && body.ExpectedRevision != revision) {
			writeErr(w, http.StatusBadRequest, "invalid or contradictory expected_revision")
			return
		}
		body.ExpectedRevision = revision
	}
	if body.ExpectedRevision < 1 {
		writeErr(w, http.StatusBadRequest, expectedRevisionRequiredMessage)
		return
	}
	if !s.ensureRules(w, r) {
		return
	}
	id := r.PathValue("id")
	snapshot, err := s.store.PublishRules(r.Context(), body.ExpectedVersion, []store.RuleChange{{Kind: "delete", ID: id, ExpectedRevision: body.ExpectedRevision}}, rulesruntime.ValidateSnapshot)
	if err != nil {
		writeRuleError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), "rule.deleted", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": snapshot.Version})
}

func (s *Server) handleDuplicateRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExpectedRevision int64  `json:"expected_revision"`
		ExpectedVersion  *int64 `json:"expected_version,omitempty"`
		Name             string `json:"name,omitempty"`
	}
	if err := decodeRuleRequest(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ExpectedRevision < 1 {
		writeErr(w, http.StatusBadRequest, expectedRevisionRequiredMessage)
		return
	}
	if !s.ensureRules(w, r) {
		return
	}
	id := uuid.NewString()
	snapshot, err := s.store.PublishRules(r.Context(), body.ExpectedVersion, []store.RuleChange{{Kind: "duplicate", ID: r.PathValue("id"), NewID: id, ExpectedRevision: body.ExpectedRevision, Name: body.Name}}, rulesruntime.ValidateSnapshot)
	if err != nil {
		writeRuleError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), "rule.duplicated", r.PathValue("id")+" -> "+id)
	writeJSON(w, http.StatusCreated, map[string]any{"rule": snapshotRule(snapshot, id), "version": snapshot.Version})
}

func (s *Server) handleReorderRules(w http.ResponseWriter, r *http.Request) {
	var body reorderRuleRequest
	if err := decodeRuleRequest(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Direction != "" || body.ID != "" {
		if len(body.Items) != 0 || body.ID == "" || body.ExpectedRevision < 1 || body.ExpectedVersion == nil || (body.Direction != "up" && body.Direction != "down") {
			writeErr(w, http.StatusBadRequest, "adjacent movement requires id, expected_revision, expected_version and direction up/down, without items")
			return
		}
		changes := []store.RuleChange{{Kind: "move_" + body.Direction, ID: body.ID, ExpectedRevision: body.ExpectedRevision}}
		s.publishRuleBatch(w, r, body.ExpectedVersion, changes, "rules.reordered")
		return
	}
	changes, ok := reorderRuleChanges(w, &body)
	if !ok {
		return
	}
	s.publishRuleBatch(w, r, body.ExpectedVersion, changes, "rules.reordered")
}

func (s *Server) handleBatchRules(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExpectedVersion *int64 `json:"expected_version,omitempty"`
		Enabled         *bool  `json:"enabled"`
		Items           []struct {
			ID               string `json:"id"`
			ExpectedRevision int64  `json:"expected_revision"`
		} `json:"items"`
	}
	if err := decodeRuleRequest(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "enabled is required")
		return
	}
	changes := make([]store.RuleChange, 0, len(body.Items))
	for _, item := range body.Items {
		if item.ExpectedRevision < 1 {
			writeErr(w, http.StatusBadRequest, "each item requires expected_revision")
			return
		}
		changes = append(changes, store.RuleChange{Kind: "patch", ID: item.ID, ExpectedRevision: item.ExpectedRevision, Enabled: body.Enabled})
	}
	s.publishRuleBatch(w, r, body.ExpectedVersion, changes, "rules.batch_updated")
}

func (s *Server) publishRuleBatch(w http.ResponseWriter, r *http.Request, version *int64, changes []store.RuleChange, action string) {
	if len(changes) == 0 {
		writeErr(w, http.StatusBadRequest, "items must not be empty")
		return
	}
	if !s.ensureRules(w, r) {
		return
	}
	snapshot, err := s.store.PublishRules(r.Context(), version, changes, rulesruntime.ValidateSnapshot)
	if err != nil {
		writeRuleError(w, err)
		return
	}
	out := make([]*store.RuleRow, 0, len(snapshot.ChangedIDs))
	ids := snapshot.ChangedIDs
	for _, id := range ids {
		if row := snapshotRule(snapshot, id); row != nil {
			out = append(out, row)
		}
	}
	_ = s.store.RecordAudit(r.Context(), action, strings.Join(ids, ","))
	writeJSON(w, http.StatusOK, map[string]any{"rules": out, "version": snapshot.Version})
}

func parseDraftRules(single json.RawMessage, multiple []json.RawMessage) ([]rules.Rule, error) {
	if len(single) != 0 {
		if multiple != nil {
			return nil, &rules.ValidationError{Path: "/rules", Message: "supply rule or rules, not both"}
		}
		multiple = []json.RawMessage{single}
	}
	if multiple == nil {
		return nil, &rules.ValidationError{Path: "/rules", Message: "rule or rules is required"}
	}
	result := make([]rules.Rule, 0, len(multiple))
	for i, raw := range multiple {
		definition, err := rules.ParseRule(raw)
		if err != nil {
			var validation *rules.ValidationError
			if errors.As(err, &validation) && len(single) == 0 {
				return nil, &rules.ValidationError{Path: fmt.Sprintf("/rules/%d%s", i, validation.Path), Message: validation.Message}
			}
			return nil, err
		}
		if definition.ID == "" {
			definition.ID = fmt.Sprintf("draft-%d", i+1)
		}
		result = append(result, definition)
	}
	return result, nil
}

func (s *Server) handleValidateRules(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Rule  json.RawMessage   `json:"rule"`
		Rules []json.RawMessage `json:"rules"`
	}
	if err := decodeRuleRequest(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	definitions, err := parseDraftRules(body.Rule, body.Rules)
	if err == nil {
		_, err = rules.Compile(definitions)
	}
	if err != nil {
		writeRuleError(w, err)
		return
	}
	out := map[string]any{"valid": true, "rules": definitions, "errors": []any{}}
	if len(body.Rule) != 0 {
		out["rule"] = definitions[0]
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSimulateRules(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Rule  json.RawMessage   `json:"rule"`
		Rules []json.RawMessage `json:"rules"`
		Phase string            `json:"phase"`
		Input *rules.Input      `json:"input"`
	}
	if err := decodeRuleRequest(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Input == nil {
		writeErr(w, http.StatusBadRequest, "input is required")
		return
	}
	if !rules.IsPhase(body.Phase) {
		writeErr(w, http.StatusBadRequest, "invalid phase")
		return
	}
	definitions, err := parseDraftRules(body.Rule, body.Rules)
	if err != nil {
		writeRuleError(w, err)
		return
	}
	compiled, err := rules.Compile(definitions)
	if err != nil {
		writeRuleError(w, err)
		return
	}
	body.Input.Trace = true
	body.Input.ConditionTrace = true
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := compiled.Apply(ctx, body.Phase, body.Input)
	out := map[string]any{"valid": true, "result": result, "errors": []any{}}
	if err != nil {
		out["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRulesSchema(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, rules.Catalog())
}

// Legacy compatibility does not initialize rules: an invalid legacy row must
// remain repairable while the data plane is still using its old chain.
func (s *Server) handleListLegacyRules(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.store.LoadRuleSet(r.Context())
	if err != nil {
		writeRuleError(w, err)
		return
	}
	if !snapshot.LegacyMigrated {
		s.handleListMiddlewares(w, r)
		return
	}
	oldRows, err := s.store.ListMiddlewares(r.Context())
	if err != nil {
		writeRuleError(w, err)
		return
	}
	oldByName := map[string]*store.MiddlewareRow{}
	for _, row := range oldRows {
		oldByName[row.Name] = row
	}
	byName := map[string]*store.RuleRow{}
	for _, row := range snapshot.Rules {
		if row.LegacyName != "" {
			byName[row.LegacyName] = row
		}
	}
	registry := middleware.Registry()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		mw, row, old := registry[name], byName[name], oldByName[name]
		if old == nil {
			old = &store.MiddlewareRow{Name: name, Enabled: true, OrderIndex: 100, Config: mw.DefaultConfig()}
		}
		item := map[string]any{"name": name, "description": mw.Describe(), "enabled": false, "order_index": old.OrderIndex,
			"config": old.Config, "default_config": mw.DefaultConfig(), "configured": row != nil, "compatible": false}
		if row != nil {
			item["enabled"], item["order_index"] = row.Enabled, row.Priority
			item["rule_id"], item["revision"] = row.ID, row.Revision
			item["compatible"] = legacyRuleCompatible(r.Context(), row, old) == nil
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"middlewares": out, "version": snapshot.Version})
}

func legacyRuleCompatible(ctx context.Context, current *store.RuleRow, old *store.MiddlewareRow) error {
	converted, err := rulesruntime.ConvertLegacy(ctx, []*store.MiddlewareRow{old})
	if err != nil {
		return err
	}
	for _, candidate := range converted {
		if candidate.LegacyName != current.LegacyName {
			continue
		}
		a, err := legacySemanticJSON(current.Rule)
		if err != nil {
			return err
		}
		b, err := legacySemanticJSON(candidate.Rule)
		if err != nil {
			return err
		}
		if bytes.Equal(a, b) {
			return nil
		}
		break
	}
	return fmt.Errorf("%w: migrated rule was edited beyond the legacy configuration; use /api/rules", store.ErrRuleConflict)
}

func legacySemanticJSON(raw json.RawMessage) ([]byte, error) {
	definition, err := rules.ParseRule(raw)
	if err != nil {
		return nil, err
	}
	encoded, err := rulesruntime.EditableJSON(definition)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	for _, key := range []string{"name", "description", "enabled", "priority"} {
		delete(fields, key)
	}
	return json.Marshal(fields)
}

func (s *Server) handleUpdateLegacyRule(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	mw, exists := middleware.Registry()[name]
	if !exists {
		writeErr(w, http.StatusNotFound, "unknown middleware: "+name)
		return
	}
	var body struct {
		Enabled          *bool   `json:"enabled"`
		OrderIndex       *int    `json:"order_index"`
		Config           *string `json:"config"`
		ExpectedRevision int64   `json:"expected_revision"`
	}
	if err := decodeRuleRequest(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateOptionalMiddlewareConfig(body.Config); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	snapshot, err := s.store.LoadRuleSet(r.Context())
	if err != nil {
		writeRuleError(w, err)
		return
	}
	old, err := s.store.GetMiddleware(r.Context(), name)
	if err != nil {
		writeRuleError(w, err)
		return
	}
	if old == nil {
		old = &store.MiddlewareRow{Name: name, Enabled: true, OrderIndex: 100, Config: mw.DefaultConfig()}
	}
	current, compatible := currentLegacyRule(w, r, snapshot, old, name, body.ExpectedRevision)
	if !compatible {
		return
	}
	if body.Enabled != nil {
		old.Enabled = *body.Enabled
	}
	if body.OrderIndex != nil {
		old.OrderIndex = *body.OrderIndex
	}
	if body.Config != nil {
		old.Config = *body.Config
	}
	if !snapshot.LegacyMigrated {
		s.repairLegacyRule(w, r, old, name)
		return
	}
	converted, err := rulesruntime.ConvertLegacy(r.Context(), []*store.MiddlewareRow{old})
	if err != nil {
		writeRuleError(w, err)
		return
	}
	raw, convertedOK := convertedLegacyRule(w, converted, current, name)
	if !convertedOK {
		return
	}
	published, err := s.store.PublishRules(r.Context(), nil, []store.RuleChange{{Kind: "update", ID: current.ID,
		ExpectedRevision: body.ExpectedRevision, Rule: raw, Legacy: old}}, rulesruntime.ValidateSnapshot)
	if err != nil {
		writeRuleError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), "middleware.updated", name+" rule="+current.ID)
	writeJSON(w, http.StatusOK, map[string]any{"middleware": old, "rule": snapshotRule(published, current.ID), "version": published.Version})
}

type reorderRuleRequest struct {
	ID               string `json:"id,omitempty"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
	Direction        string `json:"direction,omitempty"`
	ExpectedVersion  *int64 `json:"expected_version,omitempty"`
	Items            []struct {
		ID               string `json:"id"`
		ExpectedRevision int64  `json:"expected_revision"`
		OrderIndex       *int64 `json:"order_index"`
		Priority         *int   `json:"priority,omitempty"`
		Direction        string `json:"direction,omitempty"`
	} `json:"items"`
}

func reorderRuleChanges(w http.ResponseWriter, body *reorderRuleRequest) ([]store.RuleChange, bool) {
	changes := make([]store.RuleChange, 0, len(body.Items))
	for _, item := range body.Items {
		if item.ExpectedRevision < 1 {
			writeErr(w, http.StatusBadRequest, "each item requires expected_revision")
			return nil, false
		}
		if item.Direction != "" {
			if len(body.Items) != 1 || body.ExpectedVersion == nil || item.OrderIndex != nil || item.Priority != nil || (item.Direction != "up" && item.Direction != "down") {
				writeErr(w, http.StatusBadRequest, "direction requires one item, expected_version, up/down and no order_index/priority")
				return nil, false
			}
			changes = append(changes, store.RuleChange{Kind: map[string]string{"up": "move_up", "down": "move_down"}[item.Direction], ID: item.ID, ExpectedRevision: item.ExpectedRevision})
			continue
		}
		if item.OrderIndex == nil {
			writeErr(w, http.StatusBadRequest, "each item requires order_index or direction")
			return nil, false
		}
		changes = append(changes, store.RuleChange{Kind: "patch", ID: item.ID, ExpectedRevision: item.ExpectedRevision, OrderIndex: item.OrderIndex, Priority: item.Priority})
	}
	return changes, true
}

func currentLegacyRule(w http.ResponseWriter, r *http.Request, snapshot *store.RuleSetSnapshot, old *store.MiddlewareRow, name string, expectedRevision int64) (*store.RuleRow, bool) {
	var current *store.RuleRow
	if snapshot.LegacyMigrated {
		for _, row := range snapshot.Rules {
			if row.LegacyName == name {
				current = row
				break
			}
		}
		if current == nil {
			writeRuleError(w, fmt.Errorf("%w: migrated rule was deleted; use /api/rules to create a new rule", store.ErrRuleConflict))
			return nil, false
		}
		if expectedRevision < 1 {
			writeErr(w, http.StatusConflict, "expected_revision is required after migration; reload the middleware list")
			return nil, false
		}
		if err := legacyRuleCompatible(r.Context(), current, old); err != nil {
			writeRuleError(w, err)
			return nil, false
		}
		old.Enabled, old.OrderIndex = current.Enabled, current.Priority
	}

	return current, true
}

func convertedLegacyRule(w http.ResponseWriter, converted []*store.RuleRow, current *store.RuleRow, name string) (json.RawMessage, bool) {
	var raw json.RawMessage
	for _, row := range converted {
		if row.LegacyName == name {
			definition, err := rules.ParseRule(row.Rule)
			if err != nil {
				writeRuleError(w, err)
				return nil, false
			}
			definition.Name, definition.Description = current.Name, current.Description
			raw, err = rulesruntime.EditableJSON(definition)
			if err != nil {
				writeRuleError(w, err)
				return nil, false
			}
			break
		}
	}
	if raw == nil {
		writeRuleError(w, fmt.Errorf("%w: legacy converter did not return %s", store.ErrRuleInvalid, name))
		return nil, false
	}
	return raw, true
}

func validateOptionalMiddlewareConfig(config *string) error {
	if config == nil {
		return nil
	}
	return validateMiddlewareConfig(*config)
}

func (s *Server) repairLegacyRule(w http.ResponseWriter, r *http.Request, old *store.MiddlewareRow, name string) {
	if err := s.store.RepairLegacyMiddleware(r.Context(), old); err != nil {
		writeRuleError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), "middleware.updated", name)
	writeJSON(w, http.StatusOK, map[string]any{"middleware": old})
}
