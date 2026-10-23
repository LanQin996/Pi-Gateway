package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pi-gateway/internal/store"
)

// completionCancelWriter deterministically models clients that close as soon as
// the terminal event is flushed, rather than waiting for the HTTP handler to exit.
type completionCancelWriter struct {
	*httptest.ResponseRecorder
	cancel       context.CancelFunc
	trigger      string
	failTerminal bool
}

func (w *completionCancelWriter) Write(raw []byte) (int, error) {
	if w.failTerminal && strings.Contains(string(raw), "response.completed") {
		w.cancel()
		return 0, context.Canceled
	}
	return w.ResponseRecorder.Write(raw)
}

func (w *completionCancelWriter) Flush() {
	w.ResponseRecorder.Flush()
	if strings.Contains(w.Body.String(), w.trigger) {
		w.cancel()
	}
}

func TestStreamCompletionSurvivesClientClose(t *testing.T) {
	for _, tc := range []struct {
		name         string
		trigger      string
		failTerminal bool
		wantOutcome  string
	}{
		{"after terminal delivery", "response.completed", false, "succeeded"},
		{"before terminal delivery", "response.output_text.delta", false, "cancelled"},
		{"terminal write fails", "response.completed", true, "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, sseUpstream(&upstreamCapture{}), "sse")
			t.Cleanup(h.dataPlane.Close)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequest(http.MethodPost, "/v1/responses",
				strings.NewReader(`{"model":"test","stream":true,"input":[]}`)).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+h.key)
			w := &completionCancelWriter{
				ResponseRecorder: httptest.NewRecorder(), cancel: cancel,
				trigger: tc.trigger, failTerminal: tc.failTerminal,
			}
			h.dataPlane.handleResponsesHTTP(w, r)
			if ctx.Err() == nil {
				t.Fatal("fixture did not cancel the request")
			}
			rows, _, err := h.store.ListUsageRecords(context.Background(), store.UsageFilter{})
			if err != nil || len(rows) != 1 {
				t.Fatalf("usage records: count=%d err=%v", len(rows), err)
			}
			if rows[0].Outcome != tc.wantOutcome {
				t.Fatalf("outcome=%q want=%q error=%q", rows[0].Outcome, tc.wantOutcome, rows[0].ErrorMessage)
			}
			if tc.wantOutcome == "succeeded" {
				if rows[0].ErrorMessage != "" || strings.Contains(w.Body.String(), "client_closed_request") {
					t.Fatalf("completed stream gained a cancellation error: %s", w.Body.String())
				}
				if rows[0].TotalTokens == nil || *rows[0].TotalTokens != 18 {
					t.Fatalf("completed usage was lost: %+v", rows[0])
				}
			}
		})
	}
}
