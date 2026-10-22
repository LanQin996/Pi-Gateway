// Package capture records the data actually exchanged with the upstream so a
// full client/backend analysis is possible after the fact.
package capture

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
)

// Recorder implements upstream.Sink and accumulates one store.Capture.
//
// It is written from a single request goroutine plus the upstream reader, so all
// mutation goes through a mutex.
type Recorder struct {
	mu sync.Mutex

	cap *store.Capture

	maxBytes       int64
	maxFrames      int
	includeHeaders bool
	startedAt      time.Time

	seq           int
	truncated     bool
	seenBytes     int64
	traceBytes    int
	ruleEventID   string
	latestInbound int
}

// Options controls recorder behaviour.
type Options struct {
	MaxBytesPerRecord int64
	IncludeHeaders    bool
}

const bearerPrefix = "Bearer "

// New creates a recorder for a capture scaffold (account/key/transport/model
// already filled in by the caller).
func New(scaffold *store.Capture, opts Options) *Recorder {
	if opts.MaxBytesPerRecord <= 0 {
		opts.MaxBytesPerRecord = 64 << 20
	}
	c := scaffold
	if c == nil {
		c = &store.Capture{}
	}
	if c.Outcome == "" {
		c.Outcome = store.OutcomePending
	}
	return &Recorder{
		cap:            c,
		maxBytes:       opts.MaxBytesPerRecord,
		maxFrames:      defaultMaxFrames,
		includeHeaders: opts.IncludeHeaders,
		startedAt:      time.Now(),
		latestInbound:  -1,
	}
}

// defaultMaxFrames caps how many frames one record may hold, so a pathological
// stream cannot grow a single capture without bound.
const defaultMaxFrames = 20000

// Client-hop directions. The upstream hop keeps Pi's original "out"/"in"
// directions; the client hop uses these so one record shows both sides of the
// gateway (client -> gateway -> upstream and back).
const (
	// DirClientIn is traffic the client sent to the gateway.
	DirClientIn = "client_in"
	// DirClientOut is traffic the gateway sent back to the client.
	DirClientOut = "client_out"
)

// OnClientRequest records the request exactly as the client sent it — headers and
// body — before any Pi normalisation, so the client hop is auditable alongside the
// upstream hop.
func (r *Recorder) OnClientRequest(headers http.Header, raw []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(raw) > 0 {
		r.appendFrameLocked(DirClientIn, store.KindRequestBody, "", raw, nil)
	}
	if len(headers) == 0 {
		return
	}
	if r.maxFrames > 0 && len(r.cap.ResponseFrames) >= r.maxFrames {
		r.truncated = true
		return
	}
	frame := store.Frame{
		Seq:   r.seq,
		Dir:   DirClientIn,
		Kind:  store.KindHandshakeRequest,
		AtMS:  time.Since(r.startedAt).Milliseconds(),
		Bytes: len(raw),
		Data:  r.clipHeadersLocked(headers),
	}
	r.seq++
	r.cap.ResponseFrames = append(r.cap.ResponseFrames, frame)
}

// OnClientResponseHeaders records the status and headers committed to the client.
// Status is frame metadata; header names and values share the record's payload
// budget and credential sanitization with the upstream headers.
func (r *Recorder) OnClientResponseHeaders(status int, headers http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.maxFrames > 0 && len(r.cap.ResponseFrames) >= r.maxFrames {
		r.truncated = true
		return
	}
	data := map[string]any{"status": status}
	if r.includeHeaders {
		data["headers"] = r.clipHeadersLocked(headers)
	}
	r.cap.ResponseFrames = append(r.cap.ResponseFrames, store.Frame{
		Seq:  r.seq,
		Dir:  DirClientOut,
		Kind: store.KindHandshakeResponse,
		AtMS: time.Since(r.startedAt).Milliseconds(),
		Data: data,
	})
	r.seq++
}

// Capture returns the live capture (safe to read after the request finished).
func (r *Recorder) Capture() *store.Capture {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cap
}

// ---- upstream.Sink ----

// OnRequestHeaders records the headers built for the upstream call.
//
// These are the headers this gateway generated, which is exactly what an operator
// needs to verify the Pi fingerprint was reproduced.
func (r *Recorder) OnRequestHeaders(rawURL string, headers http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cap.URL = rawURL
	if r.includeHeaders {
		r.cap.RequestHeaders = r.clipHeadersLocked(headers)
	}
}

// OnRequestBody records the JSON sent upstream (uncompressed form).
func (r *Recorder) OnRequestBody(raw []byte, compressed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cap.RequestBytes = int64(len(raw))
	if compressed {
		r.cap.RequestBytes = -int64(len(raw))
	}
	r.cap.RequestBody = r.clipBytes(raw)
}

// OnResponseHeaders records the upstream status line headers.
func (r *Recorder) OnResponseHeaders(status int, headers http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cap.Status = status
	if r.includeHeaders {
		r.cap.ResponseHeaders = r.clipHeadersLocked(headers)
	}
}

// OnFrame appends one direction-stamped wire frame.
func (r *Recorder) OnFrame(dir, kind, eventType string, raw []byte, parsed map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appendFrameLocked(dir, kind, eventType, raw, parsed)
}

// appendFrameLocked appends one frame, honouring the byte budget and the frame
// cap. Callers must hold r.mu.
func (r *Recorder) appendFrameLocked(dir, kind, eventType string, raw []byte, parsed map[string]any) {
	// Every real inbound payload begins a new event. Never carry a dropped event's
	// association into a later successful delivery (including when frames are full).
	if dir == "in" && rulePayloadKind(kind) {
		r.ruleEventID = ""
		r.latestInbound = -1
	}
	if r.maxFrames > 0 && len(r.cap.ResponseFrames) >= r.maxFrames {
		r.truncated = true
		return
	}

	frame := store.Frame{
		Seq:   r.seq,
		Dir:   dir,
		Kind:  kind,
		Type:  r.clip(eventType),
		AtMS:  time.Since(r.startedAt).Milliseconds(),
		Bytes: len(raw),
	}
	r.seq++
	if dir == DirClientOut && rulePayloadKind(kind) && r.ruleEventID != "" {
		if r.traceRemainingLocked() >= len(r.ruleEventID) {
			frame.RuleEventID = r.ruleEventID
			r.chargeTraceLocked(len(r.ruleEventID))
		} else {
			r.cap.RulesTraceTruncated = true
		}
	}
	if dir == "in" && rulePayloadKind(kind) {
		r.latestInbound = len(r.cap.ResponseFrames)
	}

	r.fillFramePayloadLocked(&frame, raw, parsed)
	r.cap.ResponseFrames = append(r.cap.ResponseFrames, frame)

	// ResponseText is the client-visible SSE hop only. The upstream "in" event
	// is already represented by ResponseFrames, so recording both would duplicate
	// every event and charge the same bytes twice. Format only the portion that
	// fits the remaining budget.
	if dir == DirClientOut && kind == "sse_event" && len(raw) > 0 {
		r.cap.ResponseText = r.appendSSETextLocked(r.cap.ResponseText, raw)
	}

}

// OnUsage records token accounting from the terminal event.
func (r *Recorder) OnUsage(usage upstream.Usage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cap.PromptTokens = usage.InputTokens
	r.cap.CompletionTokens = usage.OutputTokens
	r.cap.TotalTokens = usage.TotalTokens
}

// OnResponseID records the upstream response id.
func (r *Recorder) OnResponseID(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Copy the retained prefix so a short ID does not pin an oversized input.
	r.cap.ResponseID = strings.Clone(r.clip(id))
}

// Note appends an informational frame (middleware decisions, transport
// fallbacks, ...) so the analysis view explains what happened between the client
// request and the upstream call.
func (r *Recorder) Note(kind, text string) {
	r.OnFrame("out", "note", kind, []byte(text), nil)
}

// ---- finalisation ----

// Finalize closes out the record with an outcome and duration.
func (r *Recorder) Finalize(outcome, errMessage string) *store.Capture {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cap.Outcome = outcome
	r.cap.Error = r.clip(errMessage)
	r.cap.DurationMS = time.Since(r.startedAt).Milliseconds()
	r.cap.Truncated = r.truncated
	if r.cap.CreatedAt == 0 {
		r.cap.CreatedAt = store.NowMS()
	}
	if r.cap.RequestHeaders == nil {
		r.cap.RequestHeaders = []store.Header{}
	}
	if r.cap.ResponseHeaders == nil {
		r.cap.ResponseHeaders = []store.Header{}
	}
	if r.cap.ResponseFrames == nil {
		r.cap.ResponseFrames = []store.Frame{}
	}
	if r.cap.RuleTraces == nil {
		r.cap.RuleTraces = []json.RawMessage{}
	}
	return r.cap
}

// SetTTFB records the time to first upstream byte.
func (r *Recorder) SetTTFB(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cap.TTFBMS == 0 {
		r.cap.TTFBMS = d.Milliseconds()
	}
}

// NoteTTFB is a convenience for callers that observe the first event.
func (r *Recorder) NoteTTFB() {
	r.SetTTFB(time.Since(r.startedAt))
}

// clip stores one new standalone value within the remaining record budget.
func (r *Recorder) clip(s string) string {
	return s[:r.allow(len(s))]
}

// clipBytes avoids converting an oversized raw payload to a whole string before
// the budget has decided how much can be retained.
func (r *Recorder) clipBytes(raw []byte) string {
	return string(raw[:r.allow(len(raw))])
}

func (r *Recorder) remainingBytesLocked() int64 {
	if r.maxBytes <= 0 {
		return -1
	}
	remaining := r.maxBytes - r.seenBytes
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (r *Recorder) canMarshalParsedLocked(rawLen int) bool {
	remaining := r.remainingBytesLocked()
	return remaining < 0 || (remaining > 0 && int64(rawLen) <= remaining)
}

// appendSSETextLocked appends one formatted SSE frame while charging only the
// new text. When the formatted frame cannot fit, it emits only the permitted
// prefix instead of allocating the complete formatted payload first.
func (r *Recorder) appendSSETextLocked(existing string, raw []byte) string {
	if len(raw) == 0 {
		return existing
	}
	if r.remainingBytesLocked() == 0 {
		r.truncated = true
		return existing
	}
	formattedLen := sseFrameLength(raw)
	allowed := r.allowBytes(formattedLen)
	if allowed == 0 {
		return existing
	}
	if int64(allowed) == formattedLen {
		return existing + string(formatSSEFrame(raw))
	}
	return existing + string(formatSSEFramePrefix(raw, allowed))
}

// allowBytes is the int64 form used when a formatted representation is larger
// than the input payload.
func (r *Recorder) allowBytes(n int64) int {
	if n <= 0 {
		return 0
	}
	if r.maxBytes <= 0 {
		return int(n)
	}
	remaining := r.remainingBytesLocked()
	if remaining <= 0 {
		r.truncated = true
		return 0
	}
	if n <= remaining {
		r.seenBytes += n
		return int(n)
	}
	r.seenBytes = r.maxBytes
	r.truncated = true
	return int(remaining)
}

func sseFrameLength(raw []byte) int64 {
	contentLen := int64(len(raw))
	lines := int64(1)
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '\n':
			contentLen--
			lines++
		case '\r':
			contentLen--
			lines++
			if i+1 < len(raw) && raw[i+1] == '\n' {
				contentLen--
				i++
			}
		}
	}
	// Each normalized line contributes its content, "data: ", and a newline;
	// the extra trailing newline terminates the SSE event.
	return contentLen + lines*7 + 1
}

func formatSSEFramePrefix(raw []byte, limit int) []byte {
	if limit <= 0 {
		return nil
	}
	out := make([]byte, 0, limit)
	for i := 0; ; {
		out = appendSSEPrefixChunk(out, []byte("data: "), limit)
		if len(out) >= limit {
			return out
		}
		end := sseLineEnd(raw, i, limit-len(out))
		out = appendSSEPrefixChunk(out, raw[i:end], limit)
		if len(out) >= limit {
			return out
		}
		out = append(out, '\n')
		if end >= len(raw) {
			return appendSSEPrefixChunk(out, []byte{'\n'}, limit)
		}
		i = afterSSENewline(raw, end)
	}
}

func appendSSEPrefixChunk(out, chunk []byte, limit int) []byte {
	return append(out, chunk[:min(len(chunk), limit-len(out))]...)
}

func sseLineEnd(raw []byte, start, remaining int) int {
	// Do not scan an unbounded upstream line after the capture budget is exhausted.
	end := start + min(len(raw)-start, remaining)
	for start < end && raw[start] != '\n' && raw[start] != '\r' {
		start++
	}
	return start
}

func afterSSENewline(raw []byte, start int) int {
	if raw[start] == '\r' && start+1 < len(raw) && raw[start+1] == '\n' {
		return start + 2
	}
	return start + 1
}

func (r *Recorder) clipHeadersLocked(headers http.Header) []store.Header {
	flat := flattenHeaders(headers)
	out := make([]store.Header, 0, len(flat))
	for _, h := range flat {
		name := r.clip(h.Name)
		value := r.clip(h.Value)
		if name == "" && h.Name != "" && r.truncated {
			break
		}
		out = append(out, store.Header{Name: name, Value: value})
	}
	return out
}

// allow reserves up to n bytes from the per-record budget, returning how many may
// actually be stored and flagging the record truncated when content is discarded.
func (r *Recorder) allow(n int) int {
	if n <= 0 {
		return 0
	}
	return r.allowBytes(int64(n))
}

// flattenHeaders converts a header map into an ordered, stable slice so exports
// are diff-friendly.
func flattenHeaders(h http.Header) []store.Header {
	if h == nil {
		return []store.Header{}
	}
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]store.Header, 0, len(names))
	for _, name := range names {
		for _, v := range h[name] {
			out = append(out, store.Header{Name: name, Value: sanitizeHeaderValue(name, v)})
		}
	}
	// Multi-line values would break single-line log rendering.
	for i := range out {
		out[i].Value = strings.ReplaceAll(out[i].Value, "\n", "\\n")
	}
	return out
}

// sanitizeHeaderValue keeps authorization material out of stored captures while
// preserving enough shape to verify what was sent.
func sanitizeHeaderValue(name, value string) string {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization":
		if strings.HasPrefix(value, bearerPrefix) {
			token := strings.TrimPrefix(value, bearerPrefix)
			return bearerPrefix + maskToken(token)
		}
		return maskToken(value)
	case "x-api-key", "api-key", "openai-api-key", "cookie", "set-cookie":
		return maskToken(value)
	default:
		return value
	}
}

func maskToken(token string) string {
	if len(token) <= 16 {
		return "***"
	}
	return token[:8] + "..." + token[len(token)-6:]
}

// formatSSEFrame prefixes every payload line with data:, preserving valid SSE
// framing even when an upstream JSON payload contains embedded newlines.
func formatSSEFrame(raw []byte) []byte {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return []byte(b.String())
}

func (r *Recorder) fillFramePayloadLocked(frame *store.Frame, raw []byte, parsed map[string]any) {
	// Store the parsed payload when the frame is JSON, otherwise the raw text.
	// If the raw event cannot fit in the remaining budget, avoid marshaling the
	// decoded map at all: the raw-size check bounds the temporary representation.
	switch {
	case parsed != nil && r.canMarshalParsedLocked(len(raw)):
		if encoded, err := json.Marshal(parsed); err == nil {
			if allowed := r.allow(len(encoded)); allowed == len(encoded) {
				frame.Data = json.RawMessage(encoded)
			} else {
				frame.Text = string(encoded[:allowed])
			}
		}
	case len(raw) > 0:
		allowed := r.allow(len(raw))
		if allowed == len(raw) && json.Valid(raw) {
			frame.Data = json.RawMessage(raw)
		} else {
			frame.Text = string(raw[:allowed])
		}
	case parsed != nil:
		// The parsed payload has no raw fallback and no budget remains, so it
		// was dropped without allocating its encoded representation.
		r.truncated = true
	}
}
