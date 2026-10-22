package capture

import (
	"encoding/json"
	"time"

	"pi-gateway/internal/piwire"
	"pi-gateway/internal/rules"
	"pi-gateway/internal/store"
)

const (
	KindRuleInput     = "rule_input"
	KindRuleContext   = "rule_context"
	MaxRuleInputBytes = 256 << 10
)

// OnRuleInput stores an all-or-nothing snapshot inside the existing frame budget.
// Incomplete snapshots contain no payload, so valid JSON prefixes cannot be replayed.
func (r *Recorder) OnRuleInput(phase string, input *rules.Input, contextOnly bool) {
	if input == nil {
		return
	}
	snapshot := *input
	snapshot.Trace = false
	snapshot.ConditionTrace = false
	snapshot.TraceMaxBytes = 0
	snapshot.MaxTraces = 0
	kind := KindRuleInput
	if contextOnly {
		kind = KindRuleContext
		snapshot.Body = nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.maxFrames > 0 && len(r.cap.ResponseFrames) >= r.maxFrames {
		r.truncated = true
		return
	}
	frame := store.Frame{Seq: r.seq, Dir: "internal", Kind: kind, Type: phase, AtMS: time.Since(r.startedAt).Milliseconds()}
	r.seq++
	limit := MaxRuleInputBytes
	if remaining := r.remainingBytesLocked(); remaining >= 0 && remaining < int64(limit) {
		limit = int(remaining)
	}
	left := limit
	// Check a lower size bound before Marshal: a multi-megabyte request must not
	// cause a second unbounded allocation merely to discover the snapshot is full.
	fits := checkpointFits(snapshot.Body, &left, 0) && checkpointFits(snapshot.ClientBody, &left, 0) && checkpointFits(snapshot.Context, &left, 0) && checkpointFits(snapshot.Model, &left, 0)
	if fits {
		raw, err := json.Marshal(&snapshot)
		frame.Bytes = len(raw)
		if err == nil && len(raw) <= limit {
			r.allow(len(raw))
			frame.Data = json.RawMessage(raw)
		}
	}
	if frame.Data == nil {
		r.truncated = true
	}
	r.cap.ResponseFrames = append(r.cap.ResponseFrames, frame)
}

// This cheap lower bound permits at most a small JSON-escaping expansion during
// Marshal. Unknown types are omitted rather than invoking arbitrary marshalers.
func checkpointFits(value any, left *int, depth int) bool {
	*left -= 2
	if *left < 0 || depth > 128 {
		return false
	}
	switch v := value.(type) {
	case nil, bool, float64, float32, int, int64, int32, uint, uint64:
	case string:
		*left -= len(v)
	case json.Number:
		*left -= len(v)
	case json.RawMessage:
		*left -= len(v)
	case map[string]any:
		for key, child := range v {
			*left -= len(key)
			if !checkpointFits(child, left, depth+1) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !checkpointFits(child, left, depth+1) {
				return false
			}
		}
	case *piwire.OrderedMap:
		return checkpointOrderedMapFits(v, left, depth)
	default:
		return false
	}
	return *left >= 0
}

func checkpointOrderedMapFits(v *piwire.OrderedMap, left *int, depth int) bool {
	if v != nil {
		for _, key := range v.Keys() {
			*left -= len(key)
			child, _ := v.Get(key)
			if !checkpointFits(child, left, depth+1) {
				return false
			}
		}
	}
	return *left >= 0
}
