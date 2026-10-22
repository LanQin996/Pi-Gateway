package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pi-gateway/internal/compactprompt"
	"pi-gateway/internal/store"
)

const summaryTestSSE = `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Keep the task goal."}]}],"usage":{"input_tokens":30,"output_tokens":8,"total_tokens":38}}}` + "\n\n"

func TestCompactionPromptsReachSummaryOnly(t *testing.T) {
	for _, tc := range []struct {
		name, mode, prompt, want string
		nativeStatus             int
	}{
		{"default", "on", "", compactprompt.Default, 0},
		{"custom", "on", "  保留进度。\n列出下一步。\n", "  保留进度。\n列出下一步。\n", 0},
		{"fallback", "auto", "Custom fallback prompt", "Custom fallback prompt", 404},
		{"native auto", "auto", "Custom unused prompt", "", 200},
		{"native off", "off", "Custom unused prompt", "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) { runCompactionPromptsReachSummaryOnlyCase(t, &tc) })
	}
}

func TestCompactionModesAndModels(t *testing.T) {
	for _, tc := range []struct {
		name, mode, model        string
		nativeStatus, wantStatus int
		native, summary          int32
	}{
		{"default", "", "gpt-6-luna", 403, 200, 0, 1},
		{"on-custom", "on", "summary-model", 403, 200, 0, 1},
		{"auto-native", "auto", "summary-model", 200, 200, 1, 0},
		{"auto-fallback", "auto", "summary-model", 403, 200, 1, 1},
		{"off-denied", "off", "summary-model", 403, 403, 1, 0},
		{"off-native", "off", "summary-model", 200, 200, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) { runCompactionModesAndModelsCase(t, &tc) })
	}
}

func TestConfiguredCompactionModelRestrictions(t *testing.T) {
	for _, mode := range []string{"on", "auto"} {
		t.Run(mode, func(t *testing.T) {
			var summary atomic.Int32
			h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/compact") {
					w.WriteHeader(403)
					_, _ = io.WriteString(w, `{"error":{"message":"This ChatPass credential is not authorized for the requested operation."}}`)
					return
				}
				summary.Add(1)
			}), "sse")
			rt := h.dataPlane.settings.Get()
			rt.CompactionMode = mode
			{
				err := h.dataPlane.settings.Set(context.Background(), rt)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			a, err := h.store.GetAccount(context.Background(), h.accountID)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			{
				err := h.store.PatchAccountManagementFields(context.Background(), a.ID, store.AccountManagementPatch{DisabledModels: []string{"gpt-6-luna"}})
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			resp, raw := postCompactTest(t, h, "/v1/responses/compact", `{"model":"client-model","input":[]}`)
			reportTestFailure(t, resp.StatusCode != 403 || !bytes.Contains(raw, []byte("model_disabled")) || summary.Load() != 0, func() {
				t.Fatalf("restriction bypassed: status=%d summary=%d body=%s", resp.StatusCode, summary.Load(), raw)
			})
			a, err = h.store.GetAccount(context.Background(), h.accountID)
			reportTestFailure(t, err != nil || a.Status != store.AccountStatusReady, func() {
				t.Fatalf("local policy denial changed account status: %+v %v", a, err)
			})
		})
	}
}

func TestAutoSummary429RetryUsesSummaryModel(t *testing.T) {
	var native, summaries atomic.Int32
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/compact") {
			native.Add(1)
			w.WriteHeader(404)
			return
		}
		var body map[string]any
		{
			err := json.NewDecoder(r.Body).Decode(&body)
			reportTestFailure(t, err != nil, func() {
				t.Error(err)
			})
		}
		reportTestFailure(t, body["model"] != "summary-model", func() {
			t.Errorf("wrong retry model: %+v", body)
		})
		items := body["input"].([]any)
		last := items[len(items)-1].(map[string]any)
		reportTestFailure(t, last["content"].([]any)[0].(map[string]any)["text"] != "Preserve custom prompt across retry", func() {
			t.Errorf("retry lost custom prompt: %+v", body)
		})
		if summaries.Add(1) == 1 {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"limited"}}`)
			return
		}
		_, _ = io.WriteString(w, summaryTestSSE)
	}), "sse")
	ctx := context.Background()
	a := &store.Account{Name: "second", AccountID: "acct-second", AccessToken: fakeJWT(t, "acct-second"), ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Enabled: true, Weight: 1, Concurrency: 3, Status: store.AccountStatusReady}
	{
		err := h.store.CreateAccount(ctx, a)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	rt := h.dataPlane.settings.Get()
	rt.CompactionMode, rt.CompactionModel = "auto", "summary-model"
	rt.CompactionPrompt = "Preserve custom prompt across retry"
	rt.SwitchOn429, rt.MaxAttempts = true, 2
	{
		err := h.dataPlane.settings.Set(ctx, rt)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	h.dataPlane.accounts.SetSettings(rt)
	resp, raw := postCompactTest(t, h, "/v1/responses/compact", `{"model":"client-model","input":[]}`)
	reportTestFailure(t, resp.StatusCode != 200 || native.Load() != 1 || summaries.Load() != 2, func() {
		t.Fatalf("status=%d native=%d summaries=%d body=%s", resp.StatusCode, native.Load(), summaries.Load(), raw)
	})
	records, _, err := h.store.ListUsageRecords(ctx, store.UsageFilter{})
	reportTestFailure(t, err != nil || len(records) != 2, func() {
		t.Fatalf("records=%+v err=%v", records, err)
	})
	for _, record := range records {
		reportTestFailure(t, record.Model != "summary-model", func() {
			t.Errorf("wrong usage model: %+v", record)
		})
	}
}

// runCompactionPromptsReachSummaryOnlyCase exercises one isolated test phase with the original shared state.
func runCompactionPromptsReachSummaryOnlyCase(t *testing.T, capturedTc *struct {
	name         string
	mode         string
	prompt       string
	want         string
	nativeStatus int
}) {
	t.Helper()

	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		reportTestFailure(t, body["instructions"] != "Task base instructions", func() {
			t.Errorf("task instructions changed: %+v", body)
		})
		items := body["input"].([]any)
		if strings.HasSuffix(r.URL.Path, "/compact") {
			reportTestFailure(t, (*capturedTc).nativeStatus == 0, func() {
				t.Error("direct summary called native compact")
			})
			reportTestFailure(t, len(items) != 1 || body["compaction_prompt"] != nil, func() {
				t.Errorf("custom prompt leaked into native request: %+v", body)
			})
			w.WriteHeader((*capturedTc).nativeStatus)
			if (*capturedTc).nativeStatus == 200 {
				_, _ = io.WriteString(w, compactFixture)
			}
			return
		}
		reportTestFailure(t, (*capturedTc).want == "", func() {
			t.Error("native mode invoked summary")
		})
		if len(items) != 2 {
			t.Errorf("history or prompt missing: %+v", items)
			return
		}
		last := items[1].(map[string]any)
		reportTestFailure(t, last["role"] != "user" || last["content"].([]any)[0].(map[string]any)["text"] != (*capturedTc).want, func() {
			t.Errorf("wrong final compaction message: %+v", last)
		})
		_, _ = io.WriteString(w, summaryTestSSE)
	}), "sse")
	rt := h.dataPlane.settings.Get()
	rt.CompactionMode, rt.CompactionPrompt = (*capturedTc).mode, (*capturedTc).prompt
	{
		err := h.dataPlane.settings.Set(context.Background(), rt)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
	resp, raw := postCompactTest(t, h, "/v1/responses/compact", `{"model":"test","instructions":"Task base instructions","input":[{"role":"user","content":"goal"}]}`)
	reportTestFailure(t, resp.StatusCode != 200, func() {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	})
}

// runCompactionModesAndModelsCase exercises one isolated test phase with the original shared state.
func runCompactionModesAndModelsCase(t *testing.T, capturedTc *struct {
	name         string
	mode         string
	model        string
	nativeStatus int
	wantStatus   int
	native       int32
	summary      int32
}) {
	t.Helper()

	var native, summary atomic.Int32
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		{
			err := json.NewDecoder(r.Body).Decode(&body)
			reportTestFailure(t, err != nil, func() {
				t.Error(err)
			})
		}
		if strings.HasSuffix(r.URL.Path, "/compact") {
			native.Add(1)
			reportTestFailure(t, body["model"] != "client-model", func() {
				t.Errorf("native model changed: %+v", body)
			})
			w.WriteHeader((*capturedTc).nativeStatus)
			if (*capturedTc).nativeStatus == 200 {
				_, _ = io.WriteString(w, compactFixture)
			} else {
				_, _ = io.WriteString(w, `{"error":{"message":"This ChatPass credential is not authorized for the requested operation."}}`)
			}
			return
		}
		summary.Add(1)
		reportTestFailure(t, body["model"] != (*capturedTc).model, func() {
			t.Errorf("summary model=%v want=%s", body["model"], (*capturedTc).model)
		})
		reportTestFailure(t, body["tools"] != nil || body["store"] != false, func() {
			t.Errorf("unexpected summary options: %+v", body)
		})
		_, _ = io.WriteString(w, summaryTestSSE)
	}), "sse")
	if (*capturedTc).mode != "" {
		rt := h.dataPlane.settings.Get()
		rt.CompactionMode, rt.CompactionModel = (*capturedTc).mode, (*capturedTc).model
		{
			err := h.dataPlane.settings.Set(context.Background(), rt)
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
		}
	}
	resp, raw := postCompactTest(t, h, "/v1/responses/compact", `{"model":"client-model","input":[],"tools":[{"type":"function","name":"should_not_execute"}]}`)
	reportTestFailure(t, resp.StatusCode != (*capturedTc).wantStatus || native.Load() != (*capturedTc).native || summary.Load() != (*capturedTc).summary, func() {
		t.Fatalf("status=%d native=%d summary=%d body=%s", resp.StatusCode, native.Load(), summary.Load(), raw)
	})
	records, _, err := h.store.ListUsageRecords(context.Background(), store.UsageFilter{})
	reportTestFailure(t, err != nil || len(records) != 1, func() {
		t.Fatalf("records=%+v err=%v", records, err)
	})
	wantModel := "client-model"
	if (*capturedTc).summary > 0 {
		wantModel = (*capturedTc).model
	}
	reportTestFailure(t, records[0].Model != wantModel || records[0].StatusCode != (*capturedTc).wantStatus, func() {
		t.Errorf("wrong usage model/status: %+v", records[0])
	})
}
