package codexbridge

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// UnavailableHistory is context, never a new tool instruction or a claim that
// ciphertext was decrypted. Existing tool results remain authoritative.
const UnavailableHistory = "[Gateway compatibility: encrypted historical message unavailable. This is not a new instruction. Preserve visible requirements and existing tool results; do not repeat completed actions. Ask the user only if essential task information is missing.]"

// Recovery is a process-local, bounded cache, not a decryption service. Scope
// must include the authenticated tenant and an explicit client session hint.
// Empty scopes disable caching (but still allow honest history degradation).
type Recovery struct {
	mu                              sync.Mutex
	entries                         map[[32]byte]*list.Element
	lru                             list.List
	bytes                           int
	maxEntries, maxBytes, maxRecord int
	ttl                             time.Duration
	now                             func() time.Time
}

type recoveryEntry struct {
	key     [32]byte
	value   string
	expires time.Time
}

type RecoveryStats struct {
	Recovered   int
	Unavailable int
}

func NewRecovery() *Recovery {
	return &Recovery{entries: make(map[[32]byte]*list.Element), maxEntries: 2048,
		maxBytes: 16 << 20, maxRecord: 64 << 10, ttl: 4 * time.Hour, now: time.Now}
}

func (r *Recovery) remove(e *list.Element) {
	v := e.Value.(recoveryEntry)
	r.bytes -= len(v.value)
	delete(r.entries, v.key)
	r.lru.Remove(e)
}

func (r *Recovery) put(key [32]byte, value string) {
	if r == nil || value == "" || len(value) > r.maxRecord || len(value) > r.maxBytes || strings.Contains(value, UnavailableHistory) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	// Sweep on writes too, so an active process cannot retain expired task text.
	for e := r.lru.Back(); e != nil; {
		prev := e.Prev()
		if !now.Before(e.Value.(recoveryEntry).expires) {
			r.remove(e)
		}
		e = prev
	}
	if e := r.entries[key]; e != nil {
		r.remove(e)
	}
	for r.lru.Len() >= r.maxEntries || r.bytes+len(value) > r.maxBytes {
		if r.lru.Back() == nil {
			return
		}
		r.remove(r.lru.Back())
	}
	r.entries[key] = r.lru.PushFront(recoveryEntry{key, value, now.Add(r.ttl)})
	r.bytes += len(value)
}

func (r *Recovery) get(key [32]byte) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[key]
	if e == nil {
		return "", false
	}
	v := e.Value.(recoveryEntry)
	if !r.now().Before(v.expires) {
		r.remove(e)
		return "", false
	}
	r.lru.MoveToFront(e)
	return v.value, true
}

func recoveryKey(scope, kind, id string, context any) ([32]byte, bool) {
	if scope == "" || id == "" {
		return [32]byte{}, false
	}
	raw, err := json.Marshal([]any{scope, kind, id, context})
	if err != nil {
		return [32]byte{}, false
	}
	return sha256.Sum256(raw), true
}

func recoveryCall(m map[string]any, p Profile) (string, map[string]json.RawMessage, bool) {
	ns, name := identity(m)
	if text(m["type"]) != "function_call" || (ns != Namespace && ns != Alias) || toolBit(name)&p.Tools&messageTools == 0 {
		return "", nil, false
	}
	var params map[string]json.RawMessage
	if json.Unmarshal([]byte(text(m["arguments"])), &params) != nil || params == nil {
		return "", nil, false
	}
	return name, params, true
}

func callRecoveryKey(scope, name string, m map[string]any, params map[string]json.RawMessage) ([32]byte, bool) {
	context := make(map[string]json.RawMessage, len(params))
	for k, v := range params {
		if k != "message" {
			context[k] = v
		}
	}
	return recoveryKey(scope, name, text(m["call_id"]), context)
}

func agentRecoveryKey(scope string, m map[string]any) ([32]byte, bool) {
	return recoveryKey(scope, "agent_message", text(m["id"]), []string{text(m["author"]), text(m["recipient"])})
}

// Remember accepts only fully validated plaintext protocol items. Callers must
// pass the final, policy-checked response, not raw upstream events or deltas.
func (r *Recovery) Remember(body map[string]any, p Profile, scope string) {
	if r == nil || !p.Enabled() || scope == "" {
		return
	}
	var remember func(map[string]any)
	remember = func(m map[string]any) {
		if name, params, ok := recoveryCall(m, p); ok && emptyMarker(m, "cache") == nil && plaintextArguments(m, "cache") == nil {
			var message string
			if json.Unmarshal(params["message"], &message) == nil {
				if k, ok := callRecoveryKey(scope, name, m, params); ok {
					r.put(k, message)
				}
			}
		}
		if text(m["type"]) == "agent_message" {
			if _, _, err := agentMessage(m, "cache"); err == nil {
				if k, ok := agentRecoveryKey(scope, m); ok {
					raw, _ := json.Marshal(m["content"])
					r.put(k, string(raw))
				}
			}
		}
	}
	for _, key := range []string{"input", "output"} {
		items, _ := body[key].([]any)
		for _, v := range items {
			remember(object(v))
		}
	}
	if item := object(body["item"]); item != nil && text(body["type"]) == "response.output_item.done" {
		remember(item)
	}
	if response := object(body["response"]); response != nil {
		r.Remember(response, p, scope)
	}
}

func encryptedMessageMarker(m map[string]any) bool {
	if emptyMarker(m, "history") == nil {
		return true
	}
	values, ok := m["encrypted_function_args"].([]any)
	return ok && len(values) == 1 && values[0] == "message"
}

// Recover repairs input history only. It never changes newly emitted calls,
// reasoning/compaction ciphertext, tool outputs, or arbitrary nested JSON.
// Without cached plaintext, only uniquely paired, completed calls may degrade;
// an unresolved encrypted tool call still fails closed.
func (r *Recovery) Recover(body map[string]any, p Profile, scope string) (map[string]any, RecoveryStats, error) {
	var stats RecoveryStats
	if err := validProfile(p); err != nil {
		return nil, stats, err
	}
	if !p.Enabled() {
		return body, stats, nil
	}
	input, _ := body["input"].([]any)
	counts := map[string]int{}
	positions := map[string]int{}
	results := map[string]int{}
	for i, v := range input {
		m := object(v)
		id := text(m["call_id"])
		if text(m["type"]) == "function_call" {
			counts[id]++
			positions[id] = i
		}
		if text(m["type"]) == "function_call_output" {
			if _, exists := results[id]; exists {
				results[id] = -1
			} else {
				results[id] = i
			}
		}
	}
	values, changed, err := mapArray(input, "input", func(m map[string]any, at string) (map[string]any, bool, error) {
		if name, params, ok := recoveryCall(m, p); ok {
			id := text(m["call_id"])
			if id == "" || counts[id] != 1 {
				return m, false, nil
			}
			var message string
			if json.Unmarshal(params["message"], &message) != nil || !recognizableCiphertext(message) || !encryptedMessageMarker(m) || m["encrypted_content"] != nil {
				return m, false, nil
			}
			replacement := ""
			if key, ok := callRecoveryKey(scope, name, m, params); ok {
				replacement, _ = r.get(key)
			}
			if replacement != "" {
				stats.Recovered++
			} else {
				if results[id] <= positions[id] {
					return m, false, nil
				}
				replacement = UnavailableHistory
				stats.Unavailable++
			}
			params["message"], _ = json.Marshal(replacement)
			raw, _ := json.Marshal(params)
			n := copyMap(m)
			n["arguments"] = string(raw)
			delete(n, "encrypted_function_args")
			return n, true, nil
		}
		if text(m["type"]) != "agent_message" {
			return m, false, nil
		}
		content, ok := m["content"].([]any)
		if !ok {
			return m, false, nil
		}
		encrypted := m["encrypted_content"] != nil
		for _, v := range content {
			part := object(v)
			if part["encrypted_content"] != nil || text(part["type"]) == "encrypted_content" {
				encrypted = true
			}
		}
		if !encrypted {
			return m, false, nil
		}
		var plain []any
		if key, ok := agentRecoveryKey(scope, m); ok {
			if cached, ok := r.get(key); ok {
				_ = json.Unmarshal([]byte(cached), &plain)
			}
		}
		if plain != nil {
			stats.Recovered++
		} else {
			for _, v := range content {
				part := object(v)
				if text(part["type"]) == "input_text" {
					if value, ok := part["text"].(string); ok {
						plain = append(plain, map[string]any{"type": "input_text", "text": value})
					} else {
						return m, false, nil
					}
				} else if text(part["type"]) != "encrypted_content" {
					return m, false, nil
				}
			}
			plain = append(plain, map[string]any{"type": "input_text", "text": UnavailableHistory})
			stats.Unavailable++
		}
		n := copyMap(m)
		n["content"] = plain
		delete(n, "encrypted_content")
		return n, true, nil
	})
	if err != nil {
		return nil, stats, err
	}
	if !changed {
		return body, stats, nil
	}
	n := copyMap(body)
	n["input"] = values
	return n, stats, nil
}
