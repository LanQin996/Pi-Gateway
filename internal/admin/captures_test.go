package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"pi-gateway/internal/store"
)

func TestHandleExportCapturesLimit(t *testing.T) {
	db, err := store.Open(":memory:")
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	defer db.Close()
	for i := 0; i < 1005; i++ {
		{
			err := db.InsertCapture(context.Background(), &store.Capture{}, 0)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	}
	s := &Server{store: db}
	for _, tt := range []struct {
		raw  string
		want int
	}{
		{"1", 1}, {"250", 250}, {"500", 500}, {"501", 501}, {"1001", 1001}, {"5000", 1005},
		{"", 1000}, {"nope", 1000}, {"0", 1000}, {"-1", 1000}, {"5001", 1000},
	} {
		t.Run("limit="+tt.raw, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handleExportCaptures(w, httptest.NewRequest("GET", "/?limit="+tt.raw, nil))
			reportTestFailure(t, w.Code != 200, func() {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			})
			dec := json.NewDecoder(w.Body)
			count := 0
			for {
				var capture store.Capture
				err := dec.Decode(&capture)
				if err == io.EOF {
					break
				}
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
				count++
			}
			reportTestFailure(t, count != tt.want, func() {
				t.Fatalf("exported %d, want %d", count, tt.want)
			})
		})
	}
}

func TestExportCaptureLimit(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{name: "valid", raw: "250", want: 250},
		{name: "zero", raw: "0", want: 1000},
		{name: "negative", raw: "-1", want: 1000},
		{name: "too large", raw: "5001", want: 1000},
		{name: "invalid", raw: "nope", want: 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			{
				got := exportCaptureLimit(tt.raw)
				reportTestFailure(t, got != tt.want, func() {
					t.Fatalf("exportCaptureLimit(%q) = %d, want %d", tt.raw, got, tt.want)
				})
			}
		})
	}
}
