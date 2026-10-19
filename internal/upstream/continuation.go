package upstream

import (
	"bytes"
	"encoding/json"

	"pi-gateway/internal/codexbridge"
	"pi-gateway/internal/piwire"
)

// wsContinuation mirrors Pi's CachedWebSocketContinuationState: it lets a reused
// session connection send only the new input items, as
// `{...body, previous_response_id: <last>, input: <delta>}`.
type wsContinuation struct {
	// bodyExceptInput is the previous body minus input/previous_response_id, used to
	// prove the new turn is a continuation of the cached one.
	bodyExceptInput []byte
	lastInput       []any
	// lastResponseItems are the assistant items the previous response produced.
	lastResponseItems []any
	lastResponseID    string
	codexBridge       codexbridge.Profile
	// Pooled baselines retain serialized arrays, not decoded object graphs.
	// This makes the retained byte budget independent of JSON map overhead.
	encodedInput         []byte
	encodedResponseItems []byte
}

// size counts the complete retained JSON baseline; a caller must discard the
// entire baseline on overflow rather than retain a prefix that might miscompare.
func (c *wsContinuation) size() int64 {
	if c.encodedInput != nil {
		return int64(len(c.bodyExceptInput) + len(c.encodedInput) + len(c.encodedResponseItems) + len(c.lastResponseID))
	}
	input, err := json.Marshal(c.lastInput)
	if err != nil {
		return 1 << 62
	}
	output, err := json.Marshal(c.lastResponseItems)
	if err != nil {
		return 1 << 62
	}
	c.encodedInput, c.encodedResponseItems = input, output
	c.lastInput, c.lastResponseItems = nil, nil
	return int64(len(c.bodyExceptInput) + len(input) + len(output) + len(c.lastResponseID))
}

// bodyExceptInput serialises body without the input and previous_response_id keys,
// in a stable order, so two turns can be compared structurally.
func bodyExceptInput(body []byte) ([]byte, []any, bool) {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, nil, false
	}
	input, _ := decoded["input"].([]any)
	delete(decoded, "input")
	delete(decoded, "previous_response_id")
	raw, err := piwire.MarshalWithPreferredOrder(decoded)
	if err != nil {
		return nil, nil, false
	}
	return raw, input, true
}

// inputDelta returns the items beyond the cached baseline
// (previous input followed by the previous response's items), or nil when the new
// input is not an extension of that baseline.
func inputDelta(input []any, cont *wsContinuation) []any {
	lastInput, lastOutput := cont.lastInput, cont.lastResponseItems
	if cont.encodedInput != nil {
		if json.Unmarshal(cont.encodedInput, &lastInput) != nil || json.Unmarshal(cont.encodedResponseItems, &lastOutput) != nil {
			return nil
		}
	}
	baseline := make([]any, 0, len(lastInput)+len(lastOutput))
	baseline = append(baseline, lastInput...)
	baseline = append(baseline, lastOutput...)

	if len(input) < len(baseline) {
		return nil
	}
	for i := range baseline {
		current, previous := input[i], baseline[i]
		if cont.codexBridge.Enabled() {
			// Canonical only projects registered bridge identities and empty markers.
			// The request and raw upstream baseline must remain untouched.
			current = codexbridge.Canonical(current, cont.codexBridge)
			previous = codexbridge.Canonical(previous, cont.codexBridge)
		}
		if !jsonEqual(current, previous) {
			return nil
		}
	}
	return input[len(baseline):]
}

func jsonEqual(a, b any) bool {
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(ra, rb)
}

// buildDeltaBody rewrites a full request body into the incremental continuation Pi
// sends on a reused socket. It reports ok=false when the cached state does not apply.
func buildDeltaBody(body []byte, cont *wsContinuation, profiles ...codexbridge.Profile) ([]byte, bool) {
	var profile codexbridge.Profile
	if len(profiles) > 0 {
		profile = profiles[0]
	}
	if cont == nil || cont.lastResponseID == "" || !profile.Valid() || !cont.codexBridge.Valid() || profile != cont.codexBridge {
		return nil, false
	}
	exceptInput, input, ok := bodyExceptInput(body)
	if !ok || !bytes.Equal(exceptInput, cont.bodyExceptInput) {
		return nil, false
	}
	var original map[string]any
	if err := json.Unmarshal(body, &original); err != nil {
		return nil, false
	}
	if previous, present := original["previous_response_id"]; present && previous != nil {
		// Explicit continuation is already incremental. Never strip its prefix,
		// even when it happens to equal part of the previous complete input.
		return nil, false
	}
	delta := inputDelta(input, cont)
	if delta == nil {
		return nil, false
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, false
	}
	decoded["previous_response_id"] = cont.lastResponseID
	decoded["input"] = delta
	out, err := piwire.MarshalWithPreferredOrder(decoded)
	if err != nil {
		return nil, false
	}
	return out, true
}

// responseItemsFromTerminal extracts the assistant items a completed response
// produced, excluding tool-output items (matching Pi's continuation state).
func responseItemsFromTerminal(event map[string]any) []any {
	response, ok := event["response"].(map[string]any)
	if !ok {
		return nil
	}
	output, ok := response["output"].([]any)
	if !ok {
		return nil
	}
	items := make([]any, 0, len(output))
	for _, raw := range output {
		if item, ok := raw.(map[string]any); ok {
			if t, _ := item["type"].(string); t == "function_call_output" || t == "custom_tool_call_output" {
				continue
			}
		}
		items = append(items, raw)
	}
	return items
}
