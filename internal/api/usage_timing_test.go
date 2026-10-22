package api

import (
	"testing"
	"time"
)

func TestUsageTimingMatchesCodex2APILooseFirstEvent(t *testing.T) {
	start := time.UnixMilli(10000)
	tracker := &usageTracker{startedMS: start.UnixMilli()}
	tracker.noteUpstreamStart(start.Add(200 * time.Millisecond))
	for _, event := range []string{"response.created", "response.in_progress", "response.failed", "response.completed", "error"} {
		tracker.noteFrame(event, start.Add(300*time.Millisecond))
		reportTestFailure(t, tracker.firstTokenMS != 0, func() {
			t.Fatalf("%s marked first output", event)
		})
	}
	tracker.noteFrame("response.output_item.added", start.Add(700*time.Millisecond))
	reportTestFailure(t, tracker.firstTokenMS != 700 || tracker.firstEventMS != 100, func() {
		t.Fatalf("timings=%+v", tracker)
	})
	tracker.noteFrame("response.reasoning_summary_text.delta", start.Add(900*time.Millisecond))
	tracker.noteFrame("response.output_text.delta", start.Add(1500*time.Millisecond))
	reportTestFailure(t, tracker.firstTokenMS != 700, func() {
		t.Fatal("later text replaced reasoning/structure start")
	})
}
