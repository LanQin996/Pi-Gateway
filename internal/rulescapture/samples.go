package rulescapture

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"pi-gateway/internal/capture"
	"pi-gateway/internal/piwire"
	"pi-gateway/internal/rules"
	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
)

const MaxSampleBytes = 8 << 20

type Sample struct {
	FrameSeq  *int
	EventType string
	Source    string
	Warnings  []string
	Input     *rules.Input
}

// FrameJSON deliberately rejects Text even when it is valid JSON: Text can be a
// clipped, syntactically valid prefix. Recorder only assigns Data to full payloads.
func FrameJSON(f store.Frame) ([]byte, error) {
	if f.Data == nil || f.Text != "" {
		return nil, fmt.Errorf("frame %d is missing or truncated", f.Seq)
	}
	raw, err := json.Marshal(f.Data)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxSampleBytes {
		return nil, fmt.Errorf("frame %d exceeds sample byte limit", f.Seq)
	}
	if string(raw) == "null" {
		return nil, fmt.Errorf("frame %d has no payload", f.Seq)
	}
	return raw, nil
}

func checkpoint(c *store.Capture, phase, kind string) (*Sample, bool, error) {
	for _, f := range c.ResponseFrames {
		if f.Dir != "internal" || f.Kind != kind || f.Type != phase {
			continue
		}
		raw, err := FrameJSON(f)
		if err != nil {
			return nil, true, err
		}
		var input rules.Input
		if err = json.Unmarshal(raw, &input); err != nil {
			return nil, true, err
		}
		if kind == capture.KindRuleInput && input.Body == nil {
			return nil, true, fmt.Errorf("%s checkpoint has no body", phase)
		}
		// A checkpoint is trusted recorded context, not settings supplied by the caller.
		return &Sample{FrameSeq: seqPointer(f.Seq), Source: "checkpoint", Input: &input}, true, nil
	}
	return nil, false, nil
}

// ClientRequest never reads capture.RequestBody: that is the final upstream body.
func ClientRequest(c *store.Capture) (map[string]any, *int, error) {
	var lastErr error
	for _, kind := range []string{store.KindRequestBody, store.KindWSFrame} {
		for _, f := range c.ResponseFrames {
			if f.Dir != capture.DirClientIn || f.Kind != kind {
				continue
			}
			raw, err := FrameJSON(f)
			if err != nil {
				lastErr = err
				continue
			}
			if kind == store.KindWSFrame {
				client, err := UnwrapWS(raw)
				if err != nil {
					lastErr = err
					continue
				}
				return client, seqPointer(f.Seq), nil
			}
			var client map[string]any
			if err := json.Unmarshal(raw, &client); err != nil || client == nil {
				lastErr = fmt.Errorf("client request must be a JSON object")
				continue
			}
			return client, seqPointer(f.Seq), nil
		}
	}
	if lastErr != nil {
		return nil, nil, lastErr
	}
	return nil, nil, fmt.Errorf("no complete client_in request or response.create frame is available")
}

func Request(c *store.Capture, rt *store.Settings) (*Sample, error) {
	prior, found, checkpointErr := checkpoint(c, rules.PhaseRequest, capture.KindRuleInput)
	if found && checkpointErr == nil {
		markMissingContext(prior.Input, false)
		return prior, nil
	}
	client, seq, err := ClientRequest(c)
	if err != nil {
		return nil, err
	}
	hints, headersKnown := sessionHints(c)
	cacheKey := piwire.DeriveSessionID(client, hints, nil)
	built, err := NormalizeRequest(client, rt, cacheKey)
	if err != nil {
		return nil, err
	}
	facts, missing := baseContext(c, false)
	facts["original_model"], facts["model"] = built.Model, built.Model
	missing = append(missing, "/context/request_path", "/context/request_method")
	if !headersKnown && cacheKey == "" {
		missing = append(missing, "/current/prompt_cache_key")
	}
	warnings := []string{"input_reconstructed_current_config", "context_partial"}
	if found {
		warnings = append(warnings, "checkpoint_unavailable")
	}
	return &Sample{FrameSeq: seq, Source: "reconstructed", Warnings: warnings, Input: &rules.Input{Body: built.Body, ClientBody: client, Model: built.Model, Context: facts, Unavailable: missing}}, nil
}

func sessionHints(c *store.Capture) ([]string, bool) {
	// Header frames have no individual completeness bit in older records. A
	// truncated capture cannot prove a missing/short header was recorded fully.
	if c.Truncated {
		return nil, false
	}
	for _, f := range c.ResponseFrames {
		if f.Dir != capture.DirClientIn || f.Kind != store.KindHandshakeRequest {
			continue
		}
		raw, err := FrameJSON(f)
		if err != nil {
			continue
		}
		var headers []store.Header
		if json.Unmarshal(raw, &headers) != nil {
			continue
		}
		h := http.Header{}
		for _, header := range headers {
			h.Add(header.Name, header.Value)
		}
		return []string{h.Get("x-client-request-id"), h.Get("session-id"), h.Get("session_id"), h.Get("x-session-id")}, true
	}
	return nil, false
}

func baseContext(c *store.Capture, response bool) (map[string]any, []string) {
	facts := map[string]any{}
	missing := []string{}
	if c.APIKeyID > 0 {
		facts["api_key_id"] = c.APIKeyID
	} else {
		missing = append(missing, "/context/api_key_id")
	}
	if c.ClientTransport != "" {
		facts["client_protocol"] = c.ClientTransport
	} else {
		missing = append(missing, "/context/client_protocol")
	}
	if response {
		if c.AccountID > 0 {
			facts["account_id"] = c.AccountID
		} else {
			missing = append(missing, "/context/account_id")
		}
		if c.UpstreamTransport != "" {
			facts["upstream_protocol"] = c.UpstreamTransport
		} else {
			missing = append(missing, "/context/upstream_protocol")
		}
		if c.Model != "" {
			facts["model"] = c.Model
		} else {
			missing = append(missing, "/context/model")
		}
	}
	return facts, missing
}

func responseInput(c *store.Capture) (*rules.Input, []string, error) {
	if saved, found, err := checkpoint(c, rules.PhaseResponseEvent, capture.KindRuleContext); found && err == nil {
		markMissingContext(saved.Input, true)
		return saved.Input, nil, nil
	}
	facts, missing := baseContext(c, true)
	var client any
	if saved, found, err := checkpoint(c, rules.PhaseRequest, capture.KindRuleInput); found && err == nil {
		client = saved.Input.ClientBody
		for _, key := range []string{"original_model", "request_path", "request_method"} {
			if v, ok := saved.Input.Context[key]; ok {
				facts[key] = v
			} else {
				missing = append(missing, "/context/"+key)
			}
		}
	} else {
		if raw, _, err := ClientRequest(c); err == nil {
			client = raw
		}
		missing = append(missing, "/context/original_model", "/context/request_path", "/context/request_method")
	}
	if client == nil {
		missing = append(missing, "/client")
	}
	return &rules.Input{ClientBody: client, Model: c.Model, Context: facts, Unavailable: missing}, []string{"context_partial"}, nil
}

func IsEventFrame(f store.Frame) bool {
	return f.Dir == "in" && (f.Kind == store.KindSSEEvent || f.Kind == store.KindWSFrame)
}

// EventFrames includes incomplete events so pagination never silently hides a
// captured event. Event returns an explicit unavailable error for those frames.
func EventFrames(c *store.Capture) []store.Frame {
	frames := []store.Frame{}
	for _, f := range c.ResponseFrames {
		if IsEventFrame(f) {
			frames = append(frames, f)
		}
	}
	sort.SliceStable(frames, func(i, j int) bool { return frames[i].Seq < frames[j].Seq })
	return frames
}

// EventSource decodes shared client/context once, not once per batch event.
// The engine borrows and clones this immutable context on each Apply.
type EventSource struct {
	input    *rules.Input
	warnings []string
}

func NewEventSource(c *store.Capture) (*EventSource, error) {
	input, warnings, err := responseInput(c)
	if err != nil {
		return nil, err
	}
	return &EventSource{input: input, warnings: warnings}, nil
}

func Event(c *store.Capture, f store.Frame) (*Sample, error) {
	source, err := NewEventSource(c)
	if err != nil {
		return nil, err
	}
	return source.Event(f)
}

func (source *EventSource) Event(f store.Frame) (*Sample, error) {
	if !IsEventFrame(f) {
		return nil, fmt.Errorf("frame %d is not an upstream SSE/WS event", f.Seq)
	}
	raw, err := FrameJSON(f)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return nil, fmt.Errorf("frame %d is not a JSON event object", f.Seq)
	}
	typ, ok := body["type"].(string)
	if !ok || strings.TrimSpace(typ) == "" || len(typ) > 512 || typ == "response.create" {
		return nil, fmt.Errorf("frame %d has invalid event type", f.Seq)
	}
	if f.Type != "" && f.Type != typ {
		return nil, fmt.Errorf("frame %d event type does not match recorded payload", f.Seq)
	}
	input := *source.input
	input.Body, input.EventType = body, typ
	return &Sample{FrameSeq: seqPointer(f.Seq), EventType: typ, Source: "recorded_event", Warnings: append([]string(nil), source.warnings...), Input: &input}, nil
}

// ResponseBody only reconstructs a proven, successful stream:false aggregate.
// Saved event rules are replayed through the same engine before envelope removal.
func ResponseBody(ctx context.Context, c *store.Capture, events *rules.Engine) (*Sample, error) {
	if saved, found, err := checkpoint(c, rules.PhaseResponseBody, capture.KindRuleInput); found {
		if err != nil {
			return nil, fmt.Errorf("response body checkpoint is unavailable: %w", err)
		}
		markMissingContext(saved.Input, true)
		return saved, nil
	}
	if c.Truncated || c.Outcome != store.OutcomeOK || c.Status >= 400 {
		return nil, fmt.Errorf("a complete successful aggregate response is required")
	}
	client, _, err := ClientRequest(c)
	if err != nil {
		return nil, fmt.Errorf("cannot establish aggregate phase: %w", err)
	}
	stream, ok := client["stream"].(bool)
	if !ok || stream || c.ClientTransport == "ws" || strings.Contains(c.ClientTransport, "websocket") {
		return nil, fmt.Errorf("response_body phase was not established (requires recorded stream:false HTTP request)")
	}
	if !hasDeliveredAggregate(c) {
		return nil, fmt.Errorf("record lacks evidence of a successful aggregate HTTP response")
	}
	source, err := NewEventSource(c)
	if err != nil {
		return nil, err
	}
	terminal, chosen, err := reconstructTerminal(ctx, c, source, events)
	if err != nil {
		return nil, err
	}
	if terminal == nil {
		return nil, fmt.Errorf("complete terminal response event is missing")
	}
	chosen.Input.Body = AggregatePayload(terminal)
	chosen.Input.EventType = ""
	chosen.Source = "reconstructed"
	chosen.EventType = ""
	chosen.Warnings = append(chosen.Warnings, "aggregate_reconstructed", "current_event_rules_replayed")
	return chosen, nil
}

func seqPointer(seq int) *int { return &seq }

func PipelineSample(c *store.Capture, phase string) (*Sample, error) {
	sample, found, err := checkpoint(c, phase, capture.KindRuleInput)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("no complete %s checkpoint available", phase)
	}
	return sample, nil
}

func hasDeliveredAggregate(c *store.Capture) bool {
	for _, frame := range c.ResponseFrames {
		if frame.Dir != capture.DirClientOut || frame.Kind != store.KindHTTPResponse || frame.Type != "response" {
			continue
		}
		if _, err := FrameJSON(frame); err == nil {
			return true
		}
	}
	return false
}

func reconstructTerminal(ctx context.Context, c *store.Capture, source *EventSource, events *rules.Engine) (map[string]any, *Sample, error) {
	var terminal map[string]any
	var chosen *Sample
	for _, frame := range EventFrames(c) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		sample, err := source.Event(frame)
		if err != nil {
			return nil, nil, err
		}
		if reconstructionErrorEvent(sample.EventType) {
			return nil, nil, fmt.Errorf("error events cannot reconstruct response_body input")
		}
		result, err := events.Apply(ctx, rules.PhaseResponseEvent, sample.Input)
		if err != nil {
			return nil, nil, fmt.Errorf("response event reconstruction failed: %w", err)
		}
		if result.Dropped {
			continue
		}
		body, ok := result.Body.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("event rules did not produce an object")
		}
		if upstream.IsTerminal(sample.EventType) {
			terminal = body
			chosen = sample
		}
	}
	return terminal, chosen, nil
}

func reconstructionErrorEvent(eventType string) bool {
	return eventType == upstream.EventError || eventType == upstream.EventResponseFailed
}
