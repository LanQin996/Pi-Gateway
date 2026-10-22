package capture

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"pi-gateway/internal/store"
)

// TestRecorderCapturesBothHops verifies a single capture records the client hop
// (what the client sent and received) as well as the upstream hop, which is the
// requirement for monitoring the gateway in both directions.
func TestRecorderCapturesBothHops(t *testing.T) {
	rec := New(&store.Capture{}, Options{IncludeHeaders: true})

	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-abcdefghijklmnopqrstuvwxyz")
	headers.Set("X-Api-Key", "gateway-secret-value-1234567890")
	rec.OnClientRequest(headers, []byte(`{"model":"gpt-5.1-codex"}`))
	rec.OnFrame(DirClientOut, "ws_frame", "response.output_text.delta", []byte(`{"type":"response.output_text.delta"}`), nil)
	rec.OnFrame("out", "ws_frame", "response.create", []byte(`{"type":"response.create"}`), nil)
	rec.OnFrame("in", "ws_frame", "response.completed", []byte(`{"type":"response.completed"}`), nil)

	got := rec.Capture()

	dirs := map[string]int{}
	for _, f := range got.ResponseFrames {
		dirs[f.Dir]++
	}
	if dirs[DirClientIn] < 2 {
		t.Errorf("client_in frames = %d, want the request body and headers", dirs[DirClientIn])
	}
	if dirs[DirClientOut] != 1 {
		t.Errorf("client_out frames = %d, want 1", dirs[DirClientOut])
	}
	if dirs["in"] != 1 || dirs["out"] != 1 {
		t.Errorf("upstream hop frames = in:%d out:%d, want 1 each", dirs["in"], dirs["out"])
	}

	// The client's own body must be preserved verbatim on the client hop.
	var sawClientBody bool
	for _, f := range got.ResponseFrames {
		if f.Dir == DirClientIn && f.Kind == store.KindRequestBody && frameDataString(f) == `{"model":"gpt-5.1-codex"}` {
			sawClientBody = true
		}
	}
	if !sawClientBody {
		t.Errorf("client request body was not captured: %+v", got.ResponseFrames)
	}
}

// frameDataString renders a frame payload as its original text regardless of
// whether it was stored parsed or as raw text.
func frameDataString(f store.Frame) string {
	if raw, ok := f.Data.(json.RawMessage); ok {
		return string(raw)
	}
	if f.Data != nil {
		if encoded, err := json.Marshal(f.Data); err == nil {
			return string(encoded)
		}
	}
	return f.Text
}

// TestRecorderRedactsCredentials verifies secrets never reach storage in cleartext.
func TestRecorderRedactsCredentials(t *testing.T) {
	const bearer = "sk-abcdefghijklmnopqrstuvwxyz"
	const apiKey = "gateway-secret-value-1234567890"

	rec := New(&store.Capture{}, Options{IncludeHeaders: true})
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+bearer)
	headers.Set("X-Api-Key", apiKey)
	headers.Set("Cookie", "session="+apiKey)
	rec.OnClientRequest(headers, nil)

	encoded, err := json.Marshal(rec.Capture())
	if err != nil {
		t.Fatalf("marshal capture: %v", err)
	}
	for _, secret := range []string{bearer, apiKey} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("capture leaked a credential (%q): %s", secret, encoded)
		}
	}
}

// TestRecorderEnforcesByteBudget verifies MaxBytesPerRecord actually bounds the
// stored payloads, including parsed frames (which previously bypassed the budget).
func TestRecorderEnforcesByteBudget(t *testing.T) {
	rec := New(&store.Capture{}, Options{MaxBytesPerRecord: 128})

	big := map[string]any{"type": "response.output_text.delta", "delta": strings.Repeat("x", 4096)}
	rec.OnFrame(DirClientOut, "ws_frame", "response.output_text.delta", nil, big)

	got := rec.Finalize(store.OutcomeOK, "")
	if !got.Truncated {
		t.Errorf("oversized frame did not mark the record truncated")
	}
	total := 0
	for _, f := range got.ResponseFrames {
		total += len(f.Text)
		if raw, ok := f.Data.(json.RawMessage); ok {
			total += len(raw)
		}
	}
	if total > 128 {
		t.Errorf("stored payload bytes = %d, want <= 128", total)
	}
}

// TestRecorderEnforcesFrameCap verifies a pathological stream cannot grow one
// record without bound.
func TestRecorderEnforcesFrameCap(t *testing.T) {
	rec := New(&store.Capture{}, Options{})
	rec.maxFrames = 3
	for i := 0; i < 50; i++ {
		rec.OnFrame("in", "ws_frame", "response.output_text.delta", []byte(`{"type":"response.output_text.delta"}`), nil)
	}
	got := rec.Finalize(store.OutcomeOK, "")
	if len(got.ResponseFrames) > 3 {
		t.Errorf("frames = %d, want <= 3", len(got.ResponseFrames))
	}
	if !got.Truncated {
		t.Errorf("frame cap did not mark record truncated")
	}
}

func TestRecorderResponseTextChargesOnlyNewContent(t *testing.T) {
	rec := New(&store.Capture{}, Options{MaxBytesPerRecord: 128})
	frames := [][]byte{
		[]byte(`{"a":1}`),
		[]byte(`{"b":2}`),
		[]byte(`{"c":3}`),
	}
	var wantText strings.Builder
	var previousSeen int64
	for i, raw := range frames {
		rec.OnFrame(DirClientOut, "sse_event", "", raw, nil)
		formatted := formatSSEFrame(raw)
		wantText.Write(formatted)
		got := rec.Capture()
		wantDelta := int64(len(raw) + len(formatted))
		if delta := rec.seenBytes - previousSeen; delta != wantDelta {
			t.Fatalf("frame %d charged %d bytes, want only new content %d", i, delta, wantDelta)
		}
		if !strings.HasPrefix(got.ResponseText, wantText.String()) {
			t.Fatalf("frame %d changed the historical response text prefix: got %q want prefix %q", i, got.ResponseText, wantText.String())
		}
		previousSeen = rec.seenBytes
	}
	got := rec.Finalize(store.OutcomeOK, "")
	if got.Truncated {
		t.Fatal("complete multi-frame response text was marked truncated")
	}
	if got.ResponseText != wantText.String() {
		t.Fatalf("response text = %q, want %q", got.ResponseText, wantText.String())
	}
}

func TestRecorderBudgetIncludesHeadersAndFrameType(t *testing.T) {
	clientRec := New(&store.Capture{}, Options{MaxBytesPerRecord: 12})
	clientHeaders := http.Header{}
	clientHeaders.Set("X-Client-Header", "client-value")
	clientRec.OnClientRequest(clientHeaders, nil)
	clientGot := clientRec.Finalize(store.OutcomeOK, "")
	if !clientGot.Truncated || len(clientGot.ResponseFrames) == 0 {
		t.Fatalf("client headers did not consume budget: %+v", clientGot)
	}
	if frame, ok := clientGot.ResponseFrames[0].Data.([]store.Header); !ok || len(frame) == 0 {
		t.Fatalf("client headers were not captured as a budgeted frame: %#v", clientGot.ResponseFrames[0].Data)
	}

	for _, tc := range []struct {
		name string
		call func(*Recorder)
		get  func(*store.Capture) []store.Header
	}{
		{name: "upstream request headers", call: func(r *Recorder) {
			r.OnRequestHeaders("https://upstream.test", http.Header{"X-Upstream": []string{"request-value"}})
		}, get: func(c *store.Capture) []store.Header { return c.RequestHeaders }},
		{name: "upstream response headers", call: func(r *Recorder) {
			r.OnResponseHeaders(http.StatusOK, http.Header{"X-Upstream": []string{"response-value"}})
		}, get: func(c *store.Capture) []store.Header { return c.ResponseHeaders }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := New(&store.Capture{}, Options{MaxBytesPerRecord: 12, IncludeHeaders: true})
			tc.call(rec)
			got := rec.Finalize(store.OutcomeOK, "")
			if !got.Truncated || len(tc.get(got)) == 0 || len(tc.get(got)[0].Value) >= len("request-value") {
				t.Fatalf("headers did not consume and truncate budget: %+v", got)
			}
		})
	}

	typeRec := New(&store.Capture{}, Options{MaxBytesPerRecord: 12})
	typeRec.OnFrame("in", "ws_frame", "very-long-frame-type", []byte(`{"ok":true}`), nil)
	typeGot := typeRec.Finalize(store.OutcomeOK, "")
	if !typeGot.Truncated || len(typeGot.ResponseFrames) != 1 || len(typeGot.ResponseFrames[0].Type) >= len("very-long-frame-type") {
		t.Fatalf("Frame.Type did not consume budget and truncate: %+v", typeGot.ResponseFrames)
	}
}

type marshalProbe struct {
	calls *int32
}

func (p marshalProbe) MarshalJSON() ([]byte, error) {
	atomic.AddInt32(p.calls, 1)
	return []byte(`"encoded"`), nil
}

func TestRecorderParsedFrameChecksRawSizeBeforeMarshal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		used      int
		eventType string
		raw       []byte
		wantCalls int32
		wantText  string
		truncated bool
	}{
		{name: "over total budget", raw: []byte(strings.Repeat("x", 1024)), wantText: strings.Repeat("x", 64), truncated: true},
		{name: "over remaining budget", used: 61, raw: []byte("1234"), wantText: "123", truncated: true},
		{name: "type consumes remaining budget", used: 58, eventType: "event", raw: []byte("12"), wantText: "1", truncated: true},
		{name: "exhausted with raw", used: 64, raw: []byte("x"), truncated: true},
		{name: "exhausted without raw", used: 64, truncated: true},
		{name: "fitting frame marshals", raw: []byte("{}"), wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var marshalCalls int32
			rec := New(&store.Capture{}, Options{MaxBytesPerRecord: 64})
			rec.OnRequestBody([]byte(strings.Repeat("u", tc.used)), false)
			parsed := map[string]any{"payload": marshalProbe{calls: &marshalCalls}}
			rec.OnFrame("in", "ws_frame", tc.eventType, tc.raw, parsed)
			got := rec.Finalize(store.OutcomeOK, "")
			{
				calls := atomic.LoadInt32(&marshalCalls)
				reportTestFailure(t, calls != tc.wantCalls, func() {
					t.Fatalf("parsed payload marshal calls = %d, want %d", calls, tc.wantCalls)
				})
			}
			reportTestFailure(t, got.Truncated != tc.truncated, func() {
				t.Fatalf("truncated = %v, want %v", got.Truncated, tc.truncated)
			})
			frame := got.ResponseFrames[0]
			reportTestFailure(t, tc.wantCalls == 0 && (frame.Data != nil || frame.Text != tc.wantText), func() {
				t.Fatalf("raw fallback = %#v, want text %q without parsed data", frame, tc.wantText)
			})
			reportTestFailure(t, tc.wantCalls == 1 && frameDataString(frame) != `{"payload":"encoded"}`, func() {
				t.Fatalf("fitting parsed payload = %q", frameDataString(frame))
			})
		})
	}
}

func TestRecorderResponseIDUsesBudget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		used      int
		id        string
		want      string
		truncated bool
	}{
		{name: "tiny budget", id: strings.Repeat("response-id-", 1<<16), want: "resp", truncated: true},
		{name: "remaining budget", used: 2, id: "response-id", want: "re", truncated: true},
		{name: "exhausted", used: 4, id: "response-id", truncated: true},
		{name: "exact", id: "resp", want: "resp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := New(&store.Capture{}, Options{MaxBytesPerRecord: 4})
			rec.OnRequestBody([]byte(strings.Repeat("x", tc.used)), false)
			rec.OnResponseID(tc.id)
			got := rec.Finalize(store.OutcomeOK, "")
			if got.ResponseID != tc.want || got.Truncated != tc.truncated || rec.seenBytes != 4 {
				t.Fatalf("response id = %q, truncated = %v, seenBytes = %d; want %q, %v, 4", got.ResponseID, got.Truncated, rec.seenBytes, tc.want, tc.truncated)
			}
		})
	}
}

func TestRecorderAllowExactBudgetDoesNotTruncate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		truncated bool
	}{
		{name: "exact", body: "abc"},
		{name: "one byte over", body: "abcd", truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := New(&store.Capture{}, Options{MaxBytesPerRecord: 3})
			rec.OnRequestBody([]byte(tc.body), false)
			before := rec.truncated
			if got := rec.allow(0); got != 0 || rec.truncated != before {
				t.Fatalf("allow(0) = %d, truncated changed from %v to %v", got, before, rec.truncated)
			}
			got := rec.Finalize(store.OutcomeOK, "")
			if got.RequestBody != "abc" || got.Truncated != tc.truncated || rec.seenBytes != 3 {
				t.Fatalf("body = %q, truncated = %v, seenBytes = %d; want abc, %v, 3", got.RequestBody, got.Truncated, rec.seenBytes, tc.truncated)
			}
		})
	}
}

func TestRecorderSSETextStoresBudgetedPrefix(t *testing.T) {
	for _, raw := range []string{"hello", "a\nb", "a\r\nb\rc\n", "\n\r\n\r", "end\r\n"} {
		formatted := string(formatSSEFrame([]byte(raw)))
		for remaining := 0; remaining <= len(formatted)+1; remaining++ {
			rec := New(&store.Capture{}, Options{MaxBytesPerRecord: int64(len(raw) + remaining)})
			rec.OnFrame(DirClientOut, "sse_event", "", []byte(raw), nil)
			got := rec.Finalize(store.OutcomeOK, "")
			stored := min(remaining, len(formatted))
			if got.ResponseText != formatted[:stored] {
				t.Fatalf("raw %q, remaining %d: text = %q, want %q", raw, remaining, got.ResponseText, formatted[:stored])
			}
			if got.Truncated != (remaining < len(formatted)) {
				t.Fatalf("raw %q, remaining %d: truncated = %v", raw, remaining, got.Truncated)
			}
			if want := int64(len(raw) + stored); rec.seenBytes != want {
				t.Fatalf("raw %q, remaining %d: seenBytes = %d, want %d", raw, remaining, rec.seenBytes, want)
			}
		}
	}
}

func TestRecorderSSETextLargeFrameAllocatesOnlyPrefix(t *testing.T) {
	raw := []byte(strings.Repeat("x\r\n", 1<<20))
	rec := New(&store.Capture{}, Options{MaxBytesPerRecord: 16})
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := rec.appendSSETextLocked("previous", raw)
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("SSE prefix allocated %d bytes, want under 1 MiB", allocated)
	}
	if want := "previousdata: x\ndata: x\n"; got != want {
		t.Fatalf("SSE prefix = %q, want %q", got, want)
	}
	if !rec.truncated || rec.seenBytes != 16 {
		t.Fatalf("truncated = %v, seenBytes = %d; want true, 16", rec.truncated, rec.seenBytes)
	}
}

func TestRecorderBudgetExhaustedLargeFrames(t *testing.T) {
	rec := New(&store.Capture{}, Options{MaxBytesPerRecord: 8})
	rec.OnFrame("in", "ws_frame", "", []byte("12345678"), nil)
	huge := []byte(strings.Repeat("x", 8<<20))
	var marshalCalls int32
	parsed := map[string]any{"payload": marshalProbe{calls: &marshalCalls}}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	rec.OnFrame(DirClientOut, "sse_event", "delta", huge, parsed)
	rec.OnFrame(DirClientOut, "sse_event", "delta", huge, parsed)
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("exhausted frames allocated %d bytes, want under 1 MiB", allocated)
	}
	if calls := atomic.LoadInt32(&marshalCalls); calls != 0 {
		t.Fatalf("exhausted frames marshaled parsed payload %d times", calls)
	}
	got := rec.Finalize(store.OutcomeOK, "")
	if !got.Truncated {
		t.Fatal("large frames after budget exhaustion did not mark truncated")
	}
	if len(got.ResponseText) != 0 {
		t.Fatalf("large SSE frames after exhaustion stored %d response-text bytes", len(got.ResponseText))
	}
	for i, frame := range got.ResponseFrames[1:] {
		if frame.Data != nil || frame.Text != "" {
			t.Fatalf("large frame %d stored payload after exhaustion: %#v", i+1, frame)
		}
	}
}

func TestSSEFramePrefixMatchesAllBoundaries(t *testing.T) {
	for _, raw := range []string{"", "hello", "a\nb", "a\r\nb\r", "\n\r\n", "你好\nworld"} {
		full := formatSSEFrame([]byte(raw))
		for limit := 0; limit <= len(full)+2; limit++ {
			got := formatSSEFramePrefix([]byte(raw), limit)
			want := full[:min(len(full), limit)]
			reportTestFailure(t, string(got) != string(want), func() {
				t.Fatalf("prefix raw=%q limit=%d got=%q want=%q", raw, limit, got, want)
			})
		}
	}
}

func TestSSELineEndRespectsScanBudget(t *testing.T) {
	raw := []byte(strings.Repeat("x", 1<<20) + "\n")
	if got := sseLineEnd(raw, 3, 5); got != 8 {
		t.Fatalf("scan ended at %d, want budget boundary 8", got)
	}
	if got := formatSSEFramePrefix(raw, 9); string(got) != "data: xxx" {
		t.Fatalf("bounded long-line prefix = %q", got)
	}
}
