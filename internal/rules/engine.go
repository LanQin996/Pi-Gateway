package rules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Engine is an immutable snapshot, safe for concurrent request and response execution.
type Engine struct{ phases map[string][]compiledRule }

// Compile validates every rule (including disabled rules), snapshots all values and
// precompiles every regular expression. There is deliberately no rule-count limit.
func Compile(definitions []Rule) (*Engine, error) {
	e := &Engine{phases: map[string][]compiledRule{}}
	ids := map[string]bool{}
	for i, r := range definitions {
		path := fmt.Sprintf("/%d", i)
		if r.ID != "" && ids[r.ID] {
			return nil, invalid(path+"/id", "duplicate rule ID %q", r.ID)
		}
		ids[r.ID] = true
		cr, err := compileDetachedRule(r, path)
		if err != nil {
			return nil, err
		}
		e.phases[r.Phase] = append(e.phases[r.Phase], cr)
	}
	for phase := range e.phases {
		sort.SliceStable(e.phases[phase], func(i, j int) bool {
			a, b := e.phases[phase][i].raw, e.phases[phase][j].raw
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			if a.OrderIndex != b.OrderIndex {
				return a.OrderIndex < b.OrderIndex
			}
			return a.ID < b.ID
		})
	}
	return e, nil
}

func compileDetachedRule(r Rule, path string) (compiledRule, error) {
	if r.OnError == "" {
		r.OnError = OnErrorAbort
	}
	r.Actions = append([]Action{}, r.Actions...)
	for j := range r.Actions {
		if r.Actions[j].Params == nil {
			r.Actions[j].Params = map[string]any{}
		}
	}
	// A JSON round trip detaches recursively aliased slices/maps and retains explicit null.
	raw, err := json.Marshal(r)
	if err != nil {
		return compiledRule{}, invalid(path, "invalid JSON rule: %v", err)
	}
	if len(raw) > MaxRuleBytes {
		return compiledRule{}, invalid(path, "rule exceeds %d bytes", MaxRuleBytes)
	}
	parsedRule, err := ParseRule(raw)
	if err != nil {
		return compiledRule{}, prefixError(path, err)
	}
	cr, err := compileRule(parsedRule, path)
	if err != nil {
		return compiledRule{}, err
	}

	return cr, nil
}

// HasPhase reports whether an enabled rule exists in this phase (including no-op rules).
func (e *Engine) HasPhase(phase string) bool {
	if e == nil {
		return false
	}
	for _, r := range e.phases[phase] {
		if r.raw.Enabled {
			return true
		}
	}
	return false
}

// Apply evaluates a single phase. Each matched rule commits atomically; both abort
// and skip_rule roll back all actions of the failing rule, never prior committed rules.
func (e *Engine) Apply(ctx context.Context, phase string, in *Input) (Result, error) {
	if !contains(phases, phase) {
		return Result{}, invalid("/phase", "unknown phase")
	}
	if in == nil {
		return Result{}, invalid("/input", "input is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{Body: in.Body, Model: in.Model}, err
	}
	if !e.HasPhase(phase) {
		return Result{Body: in.Body, Model: in.Model}, nil
	}
	s, err := newPhaseEvaluation(phase, in)
	if err != nil {
		return Result{}, err
	}
	run := phaseExecution{input: in, phase: phase, state: s, collector: newTraceCollector(in), baseline: protectedProjection(s.body, phase), result: Result{Body: s.body, Model: s.model}}
	for _, rule := range e.phases[phase] {
		if err := ctx.Err(); err != nil {
			return run.finish(err)
		}
		if !rule.raw.Enabled {
			continue
		}
		stop, err := run.applyRule(ctx, rule)
		if err != nil || stop {
			return run.finish(err)
		}
	}
	return run.finish(nil)
}

type phaseExecution struct {
	input     *Input
	phase     string
	state     *evaluation
	collector *traceCollector
	baseline  map[string]any
	result    Result
}

func newPhaseEvaluation(phase string, in *Input) (*evaluation, error) {
	body, err := cloneJSON(in.Body)
	if err != nil {
		return nil, invalid("/body", "%v", err)
	}
	if err = ensurePayload(body); err != nil {
		return nil, invalid("/body", "%v", err)
	}
	client, err := cloneJSON(in.ClientBody)
	if err != nil {
		return nil, invalid("/client_body", "%v", err)
	}
	facts := map[string]any{}
	if in.Context != nil {
		c, err := cloneJSON(in.Context)
		if err != nil {
			return nil, invalid("/context", "%v", err)
		}
		facts = c.(map[string]any)
	}
	steps := 0
	original, _ := cloneJSON(body)
	s := &evaluation{original: original, vars: map[string]any{}, steps: &steps, body: body, client: client, facts: facts, model: in.Model, originalModel: in.Model, eventType: in.EventType, phase: phase, conditions: newConditionCollector(in), unavailable: in.Unavailable}
	if phase == PhaseRequest {
		if err := s.validateRequestModel(in.Model); err != nil {
			return nil, err
		}
	}
	if original, ok := facts["original_model"].(string); ok {
		s.originalModel = original
	}
	if phase == PhaseResponseEvent {
		if err := s.validateResponseEvent(); err != nil {
			return nil, err
		}
	}

	return s, nil
}

func (run *phaseExecution) finish(err error) (Result, error) {
	s, res := run.state, &run.result
	res.Body, res.Model = s.body, s.model
	res.Changed = !jsonEqual(run.input.Body, s.body) || run.input.Model != s.model
	res.Traces, res.TraceOmitted = run.collector.traces, run.collector.omitted
	if s.conditions != nil {
		res.ConditionTraces, res.ConditionTraceOmitted = s.conditions.traces, s.conditions.omitted
	}
	return *res, err
}

func (run *phaseExecution) applyRule(ctx context.Context, r compiledRule) (bool, error) {
	s := run.state
	s.vars = map[string]any{}
	s.ruleID, s.rulePath = r.raw.ID, r.path
	start := time.Now()
	matched, matchErr := r.when.matches(ctx, s)
	if matchErr != nil {
		return false, run.matchFailure(ctx, r, start, matchErr)
	}
	if !matched {
		run.recordRuleTrace(r, start, false, "not_matched")
		return false, nil
	}
	if len(r.actions) == 0 {
		run.recordRuleTrace(r, start, true, "no_change")
		return r.raw.StopAfterMatch, nil
	}
	candidate := *s
	candidate.vars = map[string]any{}
	var err error
	candidate.body, err = cloneJSON(s.body)
	if err != nil {
		return false, err
	}
	pending := newTraceCollector(run.input)
	pending.maxBytes, pending.maxTraces = run.collector.remainingBytes(), run.collector.remainingCount()
	pending.enabled = run.collector.enabled
	candidate.nestedTraces, candidate.traceRule = pending, &r
	execution := ruleExecution{phase: run, rule: r, candidate: &candidate, pending: pending}
	if err := execution.actions(ctx); err != nil {
		return false, err
	}
	if execution.actionErr != nil {
		return false, execution.rollback(ctx)
	}
	run.state = &candidate
	run.collector.merge(pending)
	if execution.outcome.blocked {
		run.result.Blocked, run.result.Status, run.result.Reason = true, execution.outcome.status, execution.outcome.reason
		return true, nil
	}
	if execution.outcome.dropped {
		run.result.Dropped = true
		return true, nil
	}
	return r.raw.StopAfterMatch, nil
}

func (run *phaseExecution) recordRuleTrace(rule compiledRule, start time.Time, matched bool, status string) {
	trace := baseTrace(rule, run.state)
	trace.Matched, trace.Status, trace.DurationNS = matched, status, time.Since(start).Nanoseconds()
	run.collector.add(trace)
}

func (run *phaseExecution) matchFailure(ctx context.Context, rule compiledRule, start time.Time, err error) error {
	trace := baseTrace(rule, run.state)
	trace.Status, trace.Error, trace.DurationNS = "error", err.Error(), time.Since(start).Nanoseconds()
	run.collector.add(trace)
	var missing *MissingInputError
	if errors.As(err, &missing) {
		return err
	}
	if ctx.Err() != nil || rule.raw.OnError != OnErrorSkipRule {
		return prefixError(rule.path+"/when", err)
	}
	return nil
}

type ruleExecution struct {
	phase     *phaseExecution
	rule      compiledRule
	candidate *evaluation
	pending   *traceCollector
	actionErr error
	errorPath string
	outcome   terminal
}

func (run *ruleExecution) actions(ctx context.Context) error {
	for index, action := range run.rule.actions {
		if err := run.candidate.tick(ctx); err != nil {
			return err
		}
		if err := run.action(ctx, index, action); err != nil {
			return err
		}
		if run.actionErr != nil || run.outcome.blocked || run.outcome.dropped {
			break
		}
	}
	return nil
}

func (run *ruleExecution) action(ctx context.Context, index int, action compiledAction) error {
	start := time.Now()
	var before any
	beforeModel := run.candidate.model
	if run.pending.canCollect() {
		var err error
		before, err = cloneJSON(run.candidate.body)
		if err != nil {
			return err
		}
	}
	run.outcome, run.actionErr = action.execute(ctx, run.candidate)
	if run.actionErr == nil {
		run.actionErr = run.validateCandidate()
	}
	trace := baseTrace(run.rule, run.candidate)
	trace.ActionPath, trace.ActionID, trace.ActionType = action.path, action.raw.ID, action.raw.Type
	trace.ActionIndex, trace.Matched, trace.DurationNS = index, true, time.Since(start).Nanoseconds()
	if run.actionErr != nil {
		trace.Status, trace.Error, trace.RolledBack = "error", run.actionErr.Error(), true
		run.pending.add(trace)
		run.errorPath = action.path
		return nil
	}
	trace.Status = "no_change"
	if run.pending.canCollect() {
		trace.Changes, trace.OmittedChanges = structuralDiff(before, run.candidate.body)
		if len(trace.Changes) > 0 || trace.OmittedChanges > 0 || beforeModel != run.candidate.model {
			trace.Status = "changed"
		}
	}
	if run.outcome.blocked {
		trace.Status = "blocked"
	}
	if run.outcome.dropped {
		trace.Status = "dropped"
	}
	run.pending.add(trace)
	return nil
}

func (run *ruleExecution) validateCandidate() error {
	if err := ensurePayload(run.candidate.body); err != nil {
		return err
	}
	if err := compareProjection(run.phase.baseline, protectedProjection(run.candidate.body, run.phase.phase)); err != nil {
		return err
	}
	if run.phase.phase == PhaseRequest {
		return syncModel(run.candidate, run.phase.state)
	}
	if run.phase.phase == PhaseResponseEvent {
		if _, ok := object(run.candidate.body); !ok {
			return fmt.Errorf("response event must remain an object")
		}
	}
	return nil
}

func (run *ruleExecution) rollback(ctx context.Context) error {
	for i := range run.pending.traces {
		trace := &run.pending.traces[i]
		trace.RolledBack, trace.Changes, trace.OmittedChanges = true, nil, 0
		if trace.Status != "error" {
			trace.Status = "rolled_back"
		} else if run.rule.raw.OnError == OnErrorSkipRule {
			trace.Status = "skipped"
		}
	}
	run.phase.collector.merge(run.pending)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var missing *MissingInputError
	if errors.As(run.actionErr, &missing) {
		return run.actionErr
	}
	if run.rule.raw.OnError == OnErrorSkipRule {
		return nil
	}
	return prefixError(run.errorPath, run.actionErr)
}

func syncModel(s, previous *evaluation) error {
	if _, ok := object(s.body); !ok {
		return fmt.Errorf("request body must remain an object")
	}
	v, ok := objGet(s.body, "model")
	if !ok {
		if _, had := objGet(previous.body, "model"); had {
			return fmt.Errorf("request model cannot be removed")
		}
		return nil
	}
	str, ok := v.(string)
	if !ok || strings.TrimSpace(str) == "" {
		return fmt.Errorf("request model must be a nonempty string")
	}
	s.model = str
	return nil
}
func compareProjection(a, b map[string]any) error {
	for _, k := range keys(a) {
		v := a[k]
		w, ok := b[k]
		if !ok || !jsonEqual(v, w) {
			return fmt.Errorf("protected field %s was changed or removed", k)
		}
	}
	for _, k := range keys(b) {
		if _, ok := a[k]; !ok {
			return fmt.Errorf("protected field %s was added", k)
		}
	}
	return nil
}
func baseTrace(r compiledRule, s *evaluation) Trace {
	eventID, _ := s.facts["event_id"].(string)
	return Trace{Source: "rule", RuleID: r.raw.ID, RuleName: r.raw.Name, Revision: r.raw.Revision, Priority: r.raw.Priority, Phase: r.raw.Phase, ActionIndex: -1, EventType: s.eventType, EventID: eventID, Sequence: s.facts["sequence"]}
}

func (s *evaluation) validateRequestModel(expectedModel string) error {

	if _, ok := object(s.body); !ok {
		return invalid("/body", "request body must be an object")
	}
	if m, ok := objGet(s.body, "model"); ok {
		model, ok := m.(string)
		if !ok || strings.TrimSpace(model) == "" {
			return invalid("/body/model", "model must be a nonempty string")
		}
		if expectedModel != "" && expectedModel != model {
			return invalid("/model", "does not match body model")
		}
		s.model = model
		s.originalModel = model
	}

	return nil
}
func (s *evaluation) validateResponseEvent() error {

	if _, ok := object(s.body); !ok {
		return invalid("/body", "response event must be an object")
	}
	if t, ok := objGet(s.body, "type"); ok {
		str, ok := t.(string)
		if !ok {
			return invalid("/body/type", "event type must be a string")
		}
		if s.eventType == "" {
			s.eventType = str
		} else if s.eventType != str {
			return invalid("/event_type", "does not match body type")
		}
	}

	return nil
}
