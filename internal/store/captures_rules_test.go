package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCaptureRuleTraceRoundTripAndLightweightList(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	raw := json.RawMessage(`{"rule_id":"deleted-rule","rule_name":"historical name","status":"changed","changes":[{"path":"/text","before_exists":true,"after_exists":true,"before":null,"after":"new"}]}`)
	c := &Capture{AccountID: 0, Outcome: OutcomeError, RequestBody: "actual-request", ResponseText: "actual-response", RulesVersion: 42, RulesTraceTruncated: true, RulesTraceOmitted: 3, RuleTraces: []json.RawMessage{raw}, ResponseFrames: []Frame{{Seq: 2, Dir: "in", Kind: KindSSEEvent, RuleEventID: "event-2"}}}
	requireDataNoError(t, s.InsertCapture(ctx, c, 10))
	got, err := s.GetCapture(ctx, c.ID)
	requireDataTruef(t, !(err != nil || got == nil), "get capture: %+v %v", got, err)
	requireDataTruef(t, !(got.RulesVersion != 42 || !got.RulesTraceTruncated || got.RulesTraceOmitted != 3 || len(got.RuleTraces) != 1 || string(got.RuleTraces[0]) != string(raw)), "lost trace metadata: %+v", got)
	requireDataCondition(t, !(len(got.ResponseFrames) != 1 || got.ResponseFrames[0].RuleEventID != "event-2"), "frame event association was not persisted")
	rows, total, err := s.ListCaptures(ctx, CaptureFilter{})
	reportStoreTestFailure(t, err != nil || total != 1 || len(rows) != 1, func() {
		t.Fatalf("list: total=%d rows=%d err=%v", total, len(rows), err)
	})
	requireDataCondition(t, !(len(rows[0].RuleTraces) != 0 || rows[0].RuleTraces == nil || len(rows[0].ResponseFrames) != 0 || rows[0].RequestBody != "" || rows[0].ResponseText != ""), "summary listing loaded heavy payload")
	requireDataCondition(t, !(rows[0].RulesVersion != 42 || rows[0].RulesTraceOmitted != 3 || !rows[0].RulesTraceTruncated), "summary listing lost lightweight metadata")
	// Export uses IncludePayloads and must retain independent rule provenance.
	export, _, err := s.ListCaptures(ctx, CaptureFilter{IncludePayloads: true})
	requireDataTruef(t, !(err != nil || len(export) != 1 || len(export[0].RuleTraces) != 1), "export omitted rule sources: %+v %v", export, err)
	encoded, err := json.Marshal(export[0])
	requireDataTruef(t, !(err != nil || !strings.Contains(string(encoded), `"rule_traces":[{`) || strings.Contains(string(encoded), `"rule_traces":"`)), "traces were encoded as a string: %s %v", encoded, err)
}

func TestCaptureUnassignedRetentionFilterAndSafeClear(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insert := func(accountID int64) *Capture {
		t.Helper()
		c := &Capture{AccountID: accountID, Model: "test", Outcome: OutcomeError}
		requireDataNoError(t, s.InsertCapture(ctx, c, 2))
		return c
	}
	assigned := insert(7)
	first := insert(0)
	insert(0)
	last := insert(0)
	{
		got, err := s.GetCapture(ctx, first.ID)
		requireDataTruef(t, !(err != nil || got != nil), "unassigned history was not trimmed: %+v %v", got, err)
	}
	{
		got, err := s.GetCapture(ctx, assigned.ID)
		requireDataTruef(t, !(err != nil || got == nil), "unassigned trim deleted assigned history: %+v %v", got, err)
	}
	rows, total, err := s.ListCaptures(ctx, CaptureFilter{Unassigned: true})
	requireDataTruef(t, !(err != nil || total != 2 || len(rows) != 2 || rows[0].ID != last.ID), "unassigned filter failed: %+v %d %v", rows, total, err)
	for _, c := range rows {
		requireDataCondition(t, c.AccountID == 0, "assigned row escaped unassigned scope")
	}
	// Explicit unassigned scope must never fall through to all rows.
	_, total, err = s.ListCaptures(ctx, CaptureFilter{Unassigned: true, AccountID: 7})
	requireDataTruef(t, !(err != nil || total != 2), "conflicting filter escaped unassigned scope: %d %v", total, err)
	deleted, err := s.ClearUnassignedCaptures(ctx)
	requireDataTruef(t, !(err != nil || deleted != 2), "clear unassigned = %d %v", deleted, err)
	{
		got, err := s.GetCapture(ctx, assigned.ID)
		requireDataTrue(t, !(err != nil || got == nil), "unassigned clear deleted assigned history")
	}
	rows, total, err = s.ListCaptures(ctx, CaptureFilter{})
	requireDataTruef(t, !(err != nil || total != 1 || len(rows) != 1), "remaining capture count = %d %v", total, err)
	requireDataCondition(t, rows[0].RuleTraces != nil, "empty historical trace must serialize to [], not null")
	deleted, err = s.ClearCaptures(ctx, 0)
	requireDataTruef(t, !(err != nil || deleted != 1), "historical clear-all behavior changed: %d %v", deleted, err)
}

func TestCaptureRejectsInvalidTraceJSON(t *testing.T) {
	s := openTestStore(t)
	if err := s.InsertCapture(context.Background(), &Capture{RuleTraces: []json.RawMessage{json.RawMessage(`{"bad":`)}}, 0); err == nil {
		t.Fatal("invalid raw trace was stored")
	}
}
