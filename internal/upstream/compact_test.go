package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"pi-gateway/internal/egress"
)

func TestCompactionURLPreservesEscapedPathAndQuery(t *testing.T) {
	got, err := compactionURL("https://example.test/a%2Fb/responses/?route=x%2Fy")
	reportTestFailure(t, err != nil || got != "https://example.test/a%2Fb/responses/compact?route=x%2Fy", func() {
		t.Fatalf("url=%s err=%v", got, err)
	})
}

func TestCompactionMalformedResultsEmitNoSuccess(t *testing.T) {
	for _, body := range []string{
		`{`, `null`,
		`{"object":"response","output":[]}`,
		`{"object":"response.compaction","output":[{"type":"message"}]}`,
		`{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":""}]}`,
		`{"object":"response.compaction","output":[false]}`,
		`{"error":{"message":"logical failure"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
			defer server.Close()
			c := testClient(t, server.URL+"/responses")
			events := 0
			_, err := c.Stream(context.Background(), &Request{Compact: true, Body: []byte(`{"model":"test","input":[]}`)}, func(*Event) error { events++; return nil })
			reportTestFailure(t, err == nil || events != 0, func() {
				t.Fatalf("invalid compact result succeeded: events=%d err=%v", events, err)
			})
		})
	}
}

func TestCompactionBodyReadIsBoundedByTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	c := New(Config{SSEURL: server.URL + "/responses", IdleTimeout: 50 * time.Millisecond}, egress.NewFactory(egress.Options{}))
	defer c.Close()
	start := time.Now()
	_, err := c.Stream(context.Background(), &Request{Compact: true, Body: []byte(`{"model":"test","input":[]}`)}, func(*Event) error { t.Error("timeout emitted success"); return nil })
	reportTestFailure(t, err == nil || time.Since(start) > time.Second, func() {
		t.Fatalf("timeout err=%v elapsed=%s", err, time.Since(start))
	})
}
