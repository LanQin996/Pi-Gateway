package codexbridge

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func recoveryCipher() string {
	b := make([]byte, 73)
	b[0] = 0x80
	return base64.URLEncoding.EncodeToString(b)
}
func recoveryFixture(cipher bool, paired bool) map[string]any {
	message := "original task with Unicode 原文\nline two"
	if cipher {
		message = recoveryCipher()
	}
	args, _ := json.Marshal(map[string]any{"message": message, "target": "/root/worker"})
	input := []any{call(Namespace, "send_message", string(args))}
	if paired {
		input = append(input, map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "already delivered"})
	}
	return map[string]any{"input": input}
}

func TestRecoveryCachedAndIsolated(t *testing.T) {
	r := NewRecovery()
	p := Profile{Version: 1, Tools: ToolSendMessage}
	plain := recoveryFixture(false, true)
	cipher := recoveryFixture(true, true)
	r.Remember(plain, p, "tenant:1/session:a")
	before := serialized(t, cipher)
	out, stats, err := r.Recover(cipher, p, "tenant:1/session:a")
	if err != nil || stats.Recovered != 1 || stats.Unavailable != 0 || !reflect.DeepEqual(out, plain) {
		t.Fatalf("cache recovery: stats=%+v err=%v", stats, err)
	}
	if serialized(t, cipher) != before {
		t.Fatal("original client history mutated")
	}
	for _, scope := range []string{"tenant:2/session:a", "tenant:1/session:b", ""} {
		out, stats, err = r.Recover(cipher, p, scope)
		if err != nil || stats.Recovered != 0 || stats.Unavailable != 1 || !strings.Contains(serialized(t, out), UnavailableHistory) {
			t.Fatalf("isolation: %+v %v", stats, err)
		}
		if strings.Contains(serialized(t, out), recoveryCipher()) {
			t.Fatal("cipher forwarded")
		}
	}
	// Same call ID but a different recipient must not borrow cached task text.
	item := object(cipher["input"].([]any)[0])
	item["arguments"] = strings.ReplaceAll(text(item["arguments"]), "/root/worker", "/root/other")
	_, stats, _ = r.Recover(cipher, p, "tenant:1/session:a")
	if stats.Recovered != 0 || stats.Unavailable != 1 {
		t.Fatal("recipient isolation failed")
	}
}

func TestRecoveryFailClosedAndIdempotent(t *testing.T) {
	r := NewRecovery()
	p := Profile{Version: 1, Tools: ToolSendMessage}
	for _, kind := range []string{"unpaired", "duplicate_call", "duplicate_result", "result_before_call", "malformed_marker", "other_encrypted_field"} {
		t.Run(kind, func(t *testing.T) {
			body := recoveryFixture(true, kind != "unpaired")
			input := body["input"].([]any)
			item := object(input[0])
			switch kind {
			case "duplicate_call":
				body["input"] = append(input, item)
			case "duplicate_result":
				body["input"] = append(input, input[1])
			case "result_before_call":
				body["input"] = []any{input[1], input[0]}
			case "malformed_marker":
				item["encrypted_function_args"] = false
			case "other_encrypted_field":
				item["encrypted_function_args"] = []any{"target"}
			}
			out, stats, err := r.Recover(body, p, "scope")
			if err != nil || stats != (RecoveryStats{}) {
				t.Fatalf("unsafe recovery: %+v %v", stats, err)
			}
			_, _, err = Forward(out, p)
			requireCode(t, err, "codex_collaboration_encrypted")
		})
	}
	body := recoveryFixture(true, true)
	object(body["input"].([]any)[0])["encrypted_function_args"] = []any{"message"}
	out, stats, err := r.Recover(body, p, "")
	if err != nil || stats.Unavailable != 1 {
		t.Fatalf("paired recovery failed: %+v %v", stats, err)
	}
	if _, _, err := Forward(out, p); err != nil {
		t.Fatal(err)
	}
	again, stats, err := r.Recover(out, p, "")
	if err != nil || stats != (RecoveryStats{}) || !reflect.DeepEqual(again, out) {
		t.Fatal("not idempotent")
	}
	r.Remember(out, p, "scope")
	if len(r.entries) != 0 {
		t.Fatal("unavailable placeholder entered cache")
	}
	// The strict response path is deliberately unchanged.
	_, _, err = RestoreBody(map[string]any{"output": recoveryFixture(true, false)["input"]}, p)
	requireCode(t, err, "codex_collaboration_encrypted")
}

func TestRecoveryAgentMessageAndOpaqueData(t *testing.T) {
	r := NewRecovery()
	p := Profile{Version: 1}
	item := map[string]any{"type": "agent_message", "id": "agent-1", "author": "/root/child", "recipient": "/root", "content": []any{map[string]any{"type": "input_text", "text": "Visible task attribution"}, map[string]any{"type": "encrypted_content", "encrypted_content": "private-cipher"}}}
	body := map[string]any{"input": []any{item, map[string]any{"type": "reasoning", "encrypted_content": "keep-reasoning"}, map[string]any{"type": "compaction", "encrypted_content": "keep-compaction"}}}
	out, stats, err := r.Recover(body, p, "scope")
	if err != nil || stats.Unavailable != 1 {
		t.Fatalf("agent recovery: %+v %v", stats, err)
	}
	if _, _, err := Forward(out, p); err != nil {
		t.Fatal(err)
	}
	raw := serialized(t, out)
	if strings.Contains(raw, "private-cipher") || !strings.Contains(raw, "Visible task attribution") || !strings.Contains(raw, "keep-reasoning") || !strings.Contains(raw, "keep-compaction") {
		t.Fatal("wrong recovery boundaries")
	}
	plain := copyMap(item)
	plain["content"] = []any{map[string]any{"type": "input_text", "text": "exact plaintext agent message"}}
	r.Remember(map[string]any{"input": []any{plain}}, p, "scope")
	out, stats, err = r.Recover(body, p, "scope")
	if err != nil || stats.Recovered != 1 || !reflect.DeepEqual(object(out["input"].([]any)[0])["content"], plain["content"]) {
		t.Fatal("agent cache recovery failed")
	}
}

func TestRecoveryBoundsTTLAndConcurrency(t *testing.T) {
	r := NewRecovery()
	r.maxEntries = 2
	r.maxBytes = 8
	r.maxRecord = 4
	now := time.Unix(1000, 0)
	r.now = func() time.Time { return now }
	k1 := shaKey(1)
	k2 := shaKey(2)
	k3 := shaKey(3)
	r.put(k1, "1234")
	r.put(k2, "5678")
	r.put(k3, "longer than record")
	if len(r.entries) != 2 || r.bytes != 8 {
		t.Fatal("bounds failed")
	}
	r.get(k1)
	r.put(k3, "abcd")
	if _, ok := r.get(k2); ok {
		t.Fatal("LRU eviction failed")
	}
	now = now.Add(r.ttl)
	if _, ok := r.get(k1); ok {
		t.Fatal("expired entry recovered")
	}
	r.put(k2, "new")
	if len(r.entries) != 1 {
		t.Fatal("expired entries not swept on write")
	}
	r = NewRecovery()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.put(shaKey(i), "plain")
				r.get(shaKey(i))
			}
		}(i)
	}
	wg.Wait()
}
func shaKey(i int) [32]byte { var k [32]byte; k[0] = byte(i); return k }
