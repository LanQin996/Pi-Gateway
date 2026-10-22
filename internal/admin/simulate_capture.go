package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"pi-gateway/internal/rules"
	"pi-gateway/internal/rulescapture"
	"pi-gateway/internal/rulesruntime"
	"pi-gateway/internal/store"
)

const simulationPreviewBudget = 1 << 20
const simulationDiagnosticBudget = 512 << 10

type captureSimulationRequest struct {
	CaptureID       int64           `json:"capture_id"`
	Rule            json.RawMessage `json:"rule"`
	Phase           string          `json:"phase"`
	Scope           string          `json:"scope"`
	FrameSeq        *int            `json:"frame_seq,omitempty"`
	Batch           bool            `json:"batch,omitempty"`
	AfterSeq        *int            `json:"after_seq,omitempty"`
	Limit           int             `json:"limit,omitempty"`
	ExpectedVersion *int64          `json:"expected_version,omitempty"`
	EnableDraft     bool            `json:"enable_draft,omitempty"`
	BatchToken      string          `json:"batch_token,omitempty"`
}
type captureSimulationResult struct {
	FrameSeq         *int         `json:"frame_seq,omitempty"`
	EventType        string       `json:"event_type,omitempty"`
	Source           string       `json:"source"`
	Warnings         []string     `json:"warnings,omitempty"`
	Before           any          `json:"before"`
	Result           rules.Result `json:"result"`
	Error            string       `json:"error,omitempty"`
	PreviewTruncated bool         `json:"preview_truncated,omitempty"`
}
type captureSimulationResponse struct {
	Valid        bool                      `json:"valid"`
	CaptureID    int64                     `json:"capture_id"`
	DraftRuleID  string                    `json:"draft_rule_id"`
	RulesVersion int64                     `json:"rules_version"`
	Phase        string                    `json:"phase"`
	Results      []captureSimulationResult `json:"results"`
	Warnings     []string                  `json:"warnings,omitempty"`
	HasMore      bool                      `json:"has_more"`
	NextAfterSeq *int                      `json:"next_after_seq,omitempty"`
	Total        int                       `json:"total"`
	BatchToken   string                    `json:"batch_token,omitempty"`
	Errors       []map[string]any          `json:"errors,omitempty"`
}

type simulationSample struct {
	sample rulescapture.Sample
	err    error
}

func (s *Server) handleSimulateCapture(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var req captureSimulationRequest
	if err := decodeRuleRequest(r, &req); err != nil {
		captureSimulationError(w, 400, "", err.Error(), "invalid_request")
		return
	}
	if err := validateCaptureSimulation(&req); err != nil {
		captureSimulationValidation(w, err)
		return
	}
	draft, valid := parseSimulationDraft(w, req)
	if !valid {
		return
	}
	// Do NOT use Service.Load or Ensure here: both can migrate legacy rows.
	snapshot, valid := s.loadSimulationSnapshot(w, ctx, req)
	if !valid {
		return
	}
	version := snapshot.Version
	engine, savedEngine, draft, valid := compileSimulationEngines(w, snapshot, draft, req)
	if !valid {
		return
	}
	c, valid := s.loadSimulationCapture(w, ctx, req.CaptureID)
	if !valid {
		return
	}
	rt := s.simulationSettings()
	token := simulationToken(req, c, snapshot, draft, rt)
	if req.BatchToken != "" && req.BatchToken != token {
		captureSimulationError(w, 409, batchTokenFieldPath, "draft, sample or settings changed; restart simulation", "batch_conflict")
		return
	}
	// An uninitialized saved ruleset cannot explain legacy event transformations.
	if !validateSimulationCheckpoint(w, snapshot, req, c) {
		return
	}
	samples, total, hasMore, err := selectSimulationSamples(ctx, c, rt, savedEngine, req)
	if err != nil {
		captureSimulationError(w, 422, captureIDFieldPath, err.Error(), "sample_unavailable")
		return
	}
	out := captureSimulationResponse{Valid: true, CaptureID: c.ID, DraftRuleID: draft.ID, RulesVersion: version, Phase: req.Phase, Total: total, HasMore: hasMore, Results: []captureSimulationResult{}}
	if req.Batch {
		out.BatchToken = token
	}
	if c.Truncated {
		out.Warnings = append(out.Warnings, "capture_truncated")
	}
	if len(samples) > 0 && hasMore {
		out.NextAfterSeq = samples[len(samples)-1].sample.FrameSeq
	}
	previewLimit := min(64<<10, simulationPreviewBudget/max(1, len(samples)*2))
	diagnosticLimit := max(1, simulationDiagnosticBudget/max(1, len(samples)*2))
	for _, selected := range samples {
		item := applySimulationSample(ctx, engine, req.Phase, selected, previewLimit, diagnosticLimit)
		out.Results = append(out.Results, item)
	}
	// A single immutable snapshot was used for every event in this response.
	writeJSON(w, http.StatusOK, out)
}

func validateCaptureSimulation(req *captureSimulationRequest) error {
	bad := func(path, message string) error { return &rules.ValidationError{Path: path, Message: message} }
	if req.CaptureID <= 0 {
		return bad(captureIDFieldPath, "positive capture_id is required")
	}
	if !rules.IsPhase(req.Phase) {
		return bad(phaseFieldPath, "invalid phase")
	}
	if len(req.Rule) == 0 || string(req.Rule) == "null" {
		return bad("/rule", "rule is required")
	}
	if req.Scope == "" {
		req.Scope = "ruleset"
	}
	if req.Scope != "ruleset" && req.Scope != "rule" {
		return bad("/scope", "scope must be ruleset or rule")
	}
	if req.Limit == 0 {
		req.Limit = 50
	}
	if req.Limit < 1 || req.Limit > 200 {
		return bad("/limit", "limit must be between 1 and 200")
	}
	return validateSimulationSelection(req)
}

// Creation appends after the global maximum, exactly like store.applyRuleChanges;
// replacement keeps server-owned ordering/revision while accepting draft priority.
func simulationDefinitions(saved []rules.Rule, draft rules.Rule, scope string, enable bool) ([]rules.Rule, rules.Rule) {
	if enable {
		draft.Enabled = true
	}
	definitions := append([]rules.Rule(nil), saved...)
	next := int64(0)
	found := -1
	for i, r := range saved {
		if r.OrderIndex >= next {
			next = r.OrderIndex + 1
		}
		if r.ID == draft.ID {
			found = i
		}
	}
	if found >= 0 {
		draft.OrderIndex = saved[found].OrderIndex
		draft.Revision = saved[found].Revision
	} else {
		draft.OrderIndex = next
	}
	if scope == "rule" {
		return []rules.Rule{draft}, draft
	}
	if found >= 0 {
		definitions[found] = draft
	} else {
		definitions = append(definitions, draft)
	}
	return definitions, draft
}

func selectSimulationSamples(ctx context.Context, c *store.Capture, rt *store.Settings, saved *rules.Engine, req captureSimulationRequest) ([]simulationSample, int, bool, error) {
	if req.Phase != rules.PhaseResponseEvent {
		return selectNonEventSample(ctx, c, rt, saved, req)
	}
	frames := rulescapture.EventFrames(c)
	if len(frames) == 0 {
		return nil, 0, false, fmt.Errorf("no upstream response events were recorded")
	}
	selected := selectSimulationFrames(frames, req)
	if !req.Batch && len(selected) == 0 {
		return nil, 0, false, fmt.Errorf("requested inbound event frame is unavailable")
	}
	hasMore := len(selected) > req.Limit
	if hasMore {
		selected = selected[:req.Limit]
	}
	return readSimulationSamples(ctx, c, req, selected, len(frames), hasMore)
}

func (s *Server) simulationSettings() *store.Settings {
	if s.settings != nil {
		return s.settings.Get()
	}
	return &store.Settings{}
}
func hasBodyCheckpoint(c *store.Capture) bool {
	for _, f := range c.ResponseFrames {
		if f.Dir == "internal" && f.Kind == "rule_input" && f.Type == rules.PhaseResponseBody {
			return true
		}
	}
	return false
}
func simulationToken(req captureSimulationRequest, c *store.Capture, snapshot *store.RuleSetSnapshot, draft rules.Rule, rt *store.Settings) string {
	h := sha256.New()
	// Hash only bounded identities and metadata; never re-encode capture payloads into
	// the token calculation. The token is not persisted and carries no sample data.
	frames := make([][5]any, 0, min(len(c.ResponseFrames), 20000))
	for _, f := range c.ResponseFrames {
		frames = append(frames, [5]any{f.Seq, f.Dir, f.Kind, f.Type, f.Bytes})
	}
	metadata := []any{c.ID, c.CreatedAt, c.Truncated, frames, snapshot.Version, req.Rule, draft.ID, draft.Revision, req.Scope, req.Phase, req.EnableDraft, rt.DefaultModel, rt.ModelMappings, rt.ReasoningEffort, rt.ReasoningSummary}
	_ = json.NewEncoder(h).Encode(metadata)
	return hex.EncodeToString(h.Sum(nil))
}
func simulationPreview(value any, limit int) (any, bool) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, true
	}
	if len(raw) <= limit {
		return value, false
	}
	// Return an explicitly marked preview, never partial JSON masquerading as data.
	// JSON escaping can expand one byte to six; reserve overhead accordingly.
	prefix := max(0, (limit-256)/6)
	return map[string]any{"preview": strings.ToValidUTF8(string(raw[:min(prefix, len(raw))]), "\uFFFD"), "bytes": len(raw), "truncated": true}, true
}
func boundedSimulationText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return strings.ToValidUTF8(text[:limit], "\uFFFD")
}
func simulationErrorText(err error) string { return boundedSimulationText(err.Error(), 2048) }
func captureSimulationValidation(w http.ResponseWriter, err error) {
	var validation *rules.ValidationError
	if errors.As(err, &validation) {
		captureSimulationError(w, 400, validation.Path, validation.Message, "invalid_rule")
		return
	}
	captureSimulationError(w, 400, "/rule", err.Error(), "invalid_rule")
}
func captureSimulationError(w http.ResponseWriter, status int, path, msg, code string) {
	writeJSON(w, status, map[string]any{"valid": false, "results": []any{}, "has_more": false, "total": 0, "errors": []map[string]any{{"path": path, "message": msg, "code": code}}})
}

func applySimulationSample(ctx context.Context, engine *rules.Engine, phase string, selected simulationSample, previewLimit, diagnosticLimit int) captureSimulationResult {
	sample := selected.sample
	item := captureSimulationResult{FrameSeq: sample.FrameSeq, EventType: sample.EventType, Source: sample.Source, Warnings: sample.Warnings}
	if selected.err != nil {
		item.Error = simulationErrorText(selected.err)
		item.Warnings = append(item.Warnings, "sample_unavailable")
		return item
	}
	input := sample.Input
	input.Trace = true
	input.ConditionTrace = true
	input.TraceMaxBytes = diagnosticLimit
	input.MaxTraces = 512
	result, applyErr := engine.Apply(ctx, phase, input)
	item.Result = result
	before, beforeCut := simulationPreview(input.Body, previewLimit)
	after, afterCut := simulationPreview(result.Body, previewLimit)
	item.Before, item.Result.Body = before, after
	item.PreviewTruncated = beforeCut || afterCut
	if item.PreviewTruncated {
		item.Warnings = append(item.Warnings, "preview_truncated")
	}
	if result.TraceOmitted > 0 || result.ConditionTraceOmitted > 0 {
		item.Warnings = append(item.Warnings, "diagnostics_truncated")
	}
	if applyErr != nil {
		item.Error = simulationErrorText(applyErr)
		var missing *rules.MissingInputError
		if errors.As(applyErr, &missing) {
			item.Warnings = append(item.Warnings, "context_unavailable")
		}
	}
	return item
}

func validateSimulationSelection(req *captureSimulationRequest) error {
	bad := func(path, message string) error { return &rules.ValidationError{Path: path, Message: message} }
	if req.Phase != rules.PhaseResponseEvent && (req.Batch || req.FrameSeq != nil || req.AfterSeq != nil) {
		return bad(phaseFieldPath, "frame selection and batch pagination require response_event")
	}
	if req.FrameSeq != nil && (*req.FrameSeq < 0 || req.Batch || req.AfterSeq != nil) {
		return bad("/frame_seq", "frame_seq must be nonnegative and cannot be combined with pagination")
	}
	if req.AfterSeq != nil {
		if !req.Batch || *req.AfterSeq < 0 {
			return bad("/after_seq", "after_seq must be nonnegative and requires batch")
		}
		if req.ExpectedVersion == nil {
			return bad("/expected_version", "subsequent pages require expected_version")
		}
		if req.BatchToken == "" {
			return bad(batchTokenFieldPath, "subsequent pages require the previous batch_token")
		}
	}
	if len(req.BatchToken) > 128 {
		return bad(batchTokenFieldPath, "invalid batch token")
	}
	return nil
}

func selectNonEventSample(ctx context.Context, c *store.Capture, rt *store.Settings, saved *rules.Engine, req captureSimulationRequest) ([]simulationSample, int, bool, error) {
	var sample *rulescapture.Sample
	var err error
	if req.Phase == rules.PhaseRequest {
		sample, err = rulescapture.Request(c, rt)
	} else if req.Phase == rules.PhaseResponseBody {
		sample, err = rulescapture.ResponseBody(ctx, c, saved)
	} else {
		sample, err = rulescapture.PipelineSample(c, req.Phase)
	}
	if err != nil {
		return nil, 0, false, err
	}
	return []simulationSample{{sample: *sample}}, 1, false, nil
}

func selectSimulationFrames(frames []store.Frame, req captureSimulationRequest) []store.Frame {
	selected := []store.Frame{}
	for _, frame := range frames {
		if req.FrameSeq != nil && frame.Seq != *req.FrameSeq {
			continue
		}
		if req.AfterSeq != nil && frame.Seq <= *req.AfterSeq {
			continue
		}
		selected = append(selected, frame)
		if !req.Batch {
			break
		}
	}
	return selected
}

func readSimulationSamples(ctx context.Context, c *store.Capture, req captureSimulationRequest, selected []store.Frame, total int, hasMore bool) ([]simulationSample, int, bool, error) {
	source, err := rulescapture.NewEventSource(c)
	if err != nil {
		return nil, 0, false, err
	}
	samples := make([]simulationSample, 0, len(selected))
	inputBytes := 0
	for _, f := range selected {
		if err := ctx.Err(); err != nil {
			return nil, 0, false, err
		}
		selectedSample, size, readErr := readSimulationFrame(source, f, req.Batch)
		if readErr != nil {
			return nil, 0, false, readErr
		}
		if selectedSample.err == nil {
			if req.Batch && len(samples) > 0 && inputBytes+size > rulescapture.MaxSampleBytes {
				hasMore = true
				break
			}
			inputBytes += size
		}
		sample, err := &selectedSample.sample, selectedSample.err
		samples = append(samples, simulationSample{sample: *sample, err: err})
	}
	return samples, total, hasMore, nil
}

func parseSimulationDraft(w http.ResponseWriter, req captureSimulationRequest) (rules.Rule, bool) {
	drafts, err := parseDraftRules(req.Rule, nil)
	if err != nil {
		captureSimulationValidation(w, err)
		return rules.Rule{}, false
	}
	draft := drafts[0]
	if draft.Phase != req.Phase {
		captureSimulationError(w, 400, phaseFieldPath, "phase must match the draft rule", "phase_mismatch")
		return rules.Rule{}, false
	}
	if _, err = rules.Compile([]rules.Rule{draft}); err != nil {
		captureSimulationValidation(w, err)
		return rules.Rule{}, false
	}
	return draft, true
}

func (s *Server) loadSimulationSnapshot(w http.ResponseWriter, ctx context.Context, req captureSimulationRequest) (*store.RuleSetSnapshot, bool) {
	snapshot, err := s.store.LoadRuleSet(ctx)
	if err != nil {
		captureSimulationError(w, 500, "/rules", "could not read saved rules", "rules_unavailable")
		return nil, false
	}
	version := snapshot.Version
	if req.ExpectedVersion != nil && *req.ExpectedVersion != version {
		captureSimulationError(w, 409, "/expected_version", "rules version changed; restart simulation", "version_conflict")
		return nil, false
	}
	if !snapshot.LegacyMigrated && req.Scope == "ruleset" {
		captureSimulationError(w, 422, "/scope", "saved rules have not been migrated; use rule scope or initialize rules first (simulation never migrates)", "rules_not_initialized")
		return nil, false
	}
	return snapshot, true
}

func compileSimulationEngines(w http.ResponseWriter, snapshot *store.RuleSetSnapshot, draft rules.Rule, req captureSimulationRequest) (*rules.Engine, *rules.Engine, rules.Rule, bool) {
	saved, err := rulesruntime.DecodeSnapshot(snapshot)
	if err != nil {
		captureSimulationValidation(w, err)
		return nil, nil, draft, false
	}
	runDefinitions, draft := simulationDefinitions(saved, draft, req.Scope, req.EnableDraft)
	engine, err := rules.Compile(runDefinitions)
	if err != nil {
		captureSimulationValidation(w, err)
		return nil, nil, draft, false
	}
	savedEngine, err := rules.Compile(saved)
	if err != nil {
		captureSimulationValidation(w, err)
		return nil, nil, draft, false
	}
	return engine, savedEngine, draft, true
}

func (s *Server) loadSimulationCapture(w http.ResponseWriter, ctx context.Context, id int64) (*store.Capture, bool) {
	c, err := s.store.GetCapture(ctx, id)
	if err != nil {
		captureSimulationError(w, 500, captureIDFieldPath, "could not read capture", "capture_unavailable")
		return nil, false
	}
	if c == nil {
		captureSimulationError(w, 404, captureIDFieldPath, "capture was not found or has been cleaned up", "capture_not_found")
		return nil, false
	}
	if c.Outcome == store.OutcomePending || c.Outcome == "" {
		captureSimulationError(w, 422, captureIDFieldPath, "capture is still being recorded", "capture_pending")
		return nil, false
	}
	return c, true
}

func validateSimulationCheckpoint(w http.ResponseWriter, snapshot *store.RuleSetSnapshot, req captureSimulationRequest, c *store.Capture) bool {
	if !snapshot.LegacyMigrated && req.Phase == rules.PhaseResponseBody {
		if !hasBodyCheckpoint(c) {
			captureSimulationError(w, 422, captureIDFieldPath, "legacy response event rules cannot be reconstructed before migration", "rules_not_initialized")
			return false
		}
	}
	return true
}

func readSimulationFrame(source *rulescapture.EventSource, f store.Frame, batch bool) (simulationSample, int, error) {
	sample, err := source.Event(f)
	if err != nil {
		if !batch {
			return simulationSample{}, 0, err
		}
		seq := f.Seq
		sample = &rulescapture.Sample{FrameSeq: &seq, EventType: boundedSimulationText(f.Type, 512), Source: "recorded_event"}
		return simulationSample{sample: *sample, err: err}, 0, nil
	}
	raw, marshalErr := json.Marshal(sample.Input.Body)
	if marshalErr != nil {
		return simulationSample{}, 0, marshalErr
	}
	return simulationSample{sample: *sample}, len(raw), nil
}
