package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pi-gateway/internal/compactprompt"
	"pi-gateway/internal/egress"
)

func TestSummaryCompactionFallbackAndExpansion(t *testing.T) {
	for _, direct := range []bool{true, false} {
		t.Run(map[bool]string{true: "direct", false: "trigger"}[direct], func(t *testing.T) { runSummaryCompactionFallbackAndExpansionCase(t, &direct) })
	}
}

func TestCompactionFallbackScope(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 405, 429, 500, 501} {
		f := &FailureError{Status: status, Message: "unrelated failure"}
		want := status == 404 || status == 405 || status == 501
		reportTestFailure(t, compactFallbackAllowed(f) != want, func() {
			t.Errorf("status=%d", status)
		})
	}
}

func TestSummaryCompactionEmptyOrFailedOutput(t *testing.T) {
	for _, terminal := range []string{
		`{"type":"response.completed","response":{"status":"completed","output":[]}}`,
		`{"type":"response.incomplete","response":{"status":"incomplete","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}]}}`,
		`{"type":"response.failed","response":{"status":"failed","error":{"code":"usage_limit_reached","message":"quota exhausted"}}}`,
	} {
		t.Run(terminal, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/compact") {
					w.WriteHeader(404)
					return
				}
				io.WriteString(w, "data: "+terminal+"\n\n")
			}))
			defer server.Close()
			c := testClient(t, server.URL+"/responses")
			events := 0
			_, err := c.Stream(context.Background(), &Request{Compact: true, Body: []byte(`{"model":"test","input":[]}`)}, func(*Event) error { events++; return nil })
			reportTestFailure(t, err == nil || events != 0, func() {
				t.Fatalf("events=%d err=%v", events, err)
			})
		})
	}
}

func TestSummaryCompactionStreamedOutput(t *testing.T) {
	const item = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"用户的目标与后续工作。"}]}`
	for _, tc := range []struct {
		name     string
		terminal string
		want     string
	}{
		{"empty terminal output", `{"status":"completed","output":[]}`, "用户的目标与后续工作。"},
		{"missing terminal output", `{"status":"completed"}`, "用户的目标与后续工作。"},
		{"terminal output takes precedence", `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Canonical summary."}]}]}`, "Canonical summary."},
		{"incomplete terminal rejects streamed text", `{"status":"incomplete","output":[]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"用户的目标与后续工作。"}`+"\n\n")
				io.WriteString(w, `data: {"type":"response.output_text.done","output_index":0,"content_index":0,"text":"用户的目标与后续工作。"}`+"\n\n")
				io.WriteString(w, `data: {"type":"response.output_item.done","output_index":0,"item":`+item+"}\n\n")
				io.WriteString(w, `data: {"type":"response.completed","response":`+tc.terminal+"}\n\n")
			}))
			defer server.Close()
			c := testClient(t, server.URL+"/responses")
			var terminal map[string]any
			events := 0
			result, err := c.Stream(context.Background(), &Request{Compact: true, CompactDirect: true, CompactionMode: "on", ClientKeyID: 7, Body: []byte(`{"model":"test","input":[]}`)}, func(e *Event) error {
				events++
				reportTestFailure(t, e.Type == "response.output_text.delta" || e.Type == "response.output_text.done", func() {
					t.Error("summary leaked as answer")
				})
				if e.Type == EventResponseCompleted {
					terminal = e.Data["response"].(map[string]any)
				}
				return nil
			})
			if tc.want == "" {
				reportTestFailure(t, err == nil || events != 0, func() {
					t.Fatalf("incomplete summary succeeded: events=%d err=%v", events, err)
				})
				return
			}
			reportTestFailure(t, err != nil || terminal == nil, func() {
				t.Fatalf("result=%+v terminal=%+v err=%v", result, terminal, err)
			})
			output := terminal["output"].([]any)
			encrypted := output[0].(map[string]any)["encrypted_content"].(string)
			got, err := c.compactCodec.open(encrypted, 7)
			reportTestFailure(t, err != nil || got != tc.want, func() {
				t.Fatalf("summary=%q want=%q err=%v", got, tc.want, err)
			})
		})
	}
}

// runSummaryCompactionFallbackAndExpansionCase exercises one isolated test phase with the original shared state.
func runSummaryCompactionFallbackAndExpansionCase(t *testing.T, capturedDirect *bool) {
	t.Helper()

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.HasSuffix(r.URL.Path, "/compact") {
			w.WriteHeader(403)
			io.WriteString(w, `{"error":{"message":"This ChatPass credential is not authorized for the requested operation."}}`)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		reportTestFailure(t, body["model"] != "test" || body["stream"] != true || body["store"] != false || body["tools"] != nil || body["previous_response_id"] != nil, func() {
			t.Errorf("bad summary request: %+v", body)
		})
		reportTestFailure(t, body["instructions"] != "task instructions", func() {
			t.Error("task instructions not preserved")
		})
		items := body["input"].([]any)
		reportTestFailure(t, len(items) != 3 || items[0].(map[string]any)["role"] != "user" || items[1].(map[string]any)["role"] != "assistant", func() {
			t.Error("history not preserved")
		})
		last := items[len(items)-1].(map[string]any)
		reportTestFailure(t, last["role"] != "user" || last["content"].([]any)[0].(map[string]any)["text"] != compactprompt.Default, func() {
			t.Error("Codex default prompt not appended as user message")
		})
		io.WriteString(w, `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Keep the user goal and remaining work."}]}],"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}}}`+"\n\n")
	}))
	defer server.Close()
	key := bytes.Repeat([]byte{42}, 32)
	c := New(Config{SSEURL: server.URL + "/responses", CompactionKey: key}, egress.NewFactory(egress.Options{}))
	defer c.Close()
	req := &Request{Compact: true, CompactDirect: (*capturedDirect), ClientKeyID: 7, Body: []byte(`{"model":"test","instructions":"task instructions","input":[{"role":"user","content":"goal"},{"role":"assistant","content":"progress"}]}`)}
	var terminal map[string]any
	_, err := c.Stream(context.Background(), req, func(e *Event) error {
		reportTestFailure(t, e.Type == "response.output_text.delta", func() {
			t.Error("summary leaked as answer")
		})
		if e.Type == EventResponseCompleted {
			terminal = e.Data["response"].(map[string]any)
		}
		return nil
	})
	reportTestFailure(t, err != nil || calls != 2, func() {
		t.Fatalf("calls=%d err=%v", calls, err)
	})
	reportTestFailure(t, (*capturedDirect) && terminal["object"] != "response.compaction", func() {
		t.Fatalf("bad envelope: %+v", terminal)
	})
	item := terminal["output"].([]any)[0].(map[string]any)
	encrypted := item["encrypted_content"].(string)
	reportTestFailure(t, strings.Contains(encrypted, "remaining work") || !strings.HasPrefix(encrypted, summaryPrefix), func() {
		t.Fatal("summary not encrypted")
	})
	next, _ := json.Marshal(map[string]any{"model": "test", "input": []any{item, map[string]any{"role": "user", "content": "continue"}}})
	// A fresh client with the same persisted key can read the item.
	reopened := New(Config{CompactionKey: key}, egress.NewFactory(egress.Options{}))
	defer reopened.Close()
	expanded, err := reopened.ExpandCompactionBody(next, 7)
	reportTestFailure(t, err != nil || !bytes.Contains(expanded, []byte("remaining work")) || bytes.Contains(expanded, []byte(summaryPrefix)), func() {
		t.Fatalf("expansion=%s err=%v", expanded, err)
	})
	{
		_, err := reopened.ExpandCompactionBody(next, 8)
		reportTestFailure(t, err == nil, func() {
			t.Fatal("another API key decrypted summary")
		})
	}
	item["encrypted_content"] = summaryPrefix + "invalid"
	tampered, _ := json.Marshal(map[string]any{"input": []any{item}})
	{
		_, err := reopened.ExpandCompactionBody(tampered, 7)
		reportTestFailure(t, err == nil, func() {
			t.Fatal("accepted tampered item")
		})
	}
}
