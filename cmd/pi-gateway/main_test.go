package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"pi-gateway/internal/config"
	"pi-gateway/internal/store"
)

// responseWriterOnly deliberately exposes no optional interfaces.
type responseWriterOnly struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *responseWriterOnly) Header() http.Header         { return w.header }
func (w *responseWriterOnly) WriteHeader(status int)      { w.status = status }
func (w *responseWriterOnly) Write(p []byte) (int, error) { return w.body.Write(p) }

func TestStatusRecorderReadFromWithoutReaderFrom(t *testing.T) {
	underlying := &responseWriterOnly{header: make(http.Header)}
	recorder := &statusRecorder{ResponseWriter: underlying}
	want := "fallback body"
	// Hide WriterTo on the source too, forcing io.Copy to exercise ReaderFrom.
	src := struct{ io.Reader }{Reader: bytes.NewBufferString(want)}
	n, err := io.Copy(recorder, src)
	if err != nil || n != int64(len(want)) {
		t.Fatalf("copy = %d, %v; want %d, nil", n, err, len(want))
	}
	if got := underlying.body.String(); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if !recorder.wrote || recorder.status != http.StatusOK || underlying.status != http.StatusOK {
		t.Fatalf("recorded = %v/%d, underlying = %d; want true/200/200", recorder.wrote, recorder.status, underlying.status)
	}
}

func TestWithRequestLoggingWebSocketUpgradeRoundTrip(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := withRequestLogging(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r, nil, 1024, 1024)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.WriteMessage(messageType, payload)
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	url := "ws" + srv.URL[len("http"):]
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("websocket dial through production wrapper failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte("round-trip")); err != nil {
		t.Fatalf("write websocket message: %v", err)
	}
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read websocket message: %v", err)
	}
	if messageType != websocket.TextMessage || string(payload) != "round-trip" {
		t.Fatalf("echo = type %d payload %q, want text round-trip", messageType, payload)
	}
}

func TestStartupHintUsesModernChatGPTBasePath(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	cfg := config.Default()
	printStartup(logger, cfg, "", st)
	logs := output.String()
	want := "pi_base_url_hint=http://" + cfg.Addr() + "/v1"
	if !strings.Contains(logs, want) || strings.Contains(logs, "/backend-api") {
		t.Fatalf("startup must advertise the modern ChatGPT provider path: %s", logs)
	}
}
