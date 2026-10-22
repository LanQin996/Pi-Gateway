package admin

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"pi-gateway/internal/accounts"
	"pi-gateway/internal/pricing"
	"pi-gateway/internal/quota"
	"pi-gateway/internal/store"
)

type accountCostSeed struct {
	accountID, start, completed int64
	cost                        *int64
	outcome, sent               string
}

func seedAccountCost(t *testing.T, db *store.Store, seed accountCostSeed) {
	accountID, start, completed, cost, outcome, sent := seed.accountID, seed.start, seed.completed, seed.cost, seed.outcome, seed.sent
	t.Helper()
	record := &store.UsageRecord{AccountID: accountID, StartedAt: start, Model: "gpt-6-sol", RequestKind: "compaction"}
	id, err := db.StartUsageRecord(context.Background(), record)
	reportTestFailure(t, err != nil, func() {
		t.Fatal(err)
	})
	if outcome == "running" {
		return
	}
	record.Outcome, record.UpstreamSendState, record.CompletedAt, record.CostMicros = outcome, sent, completed, cost
	{
		err := db.FinishUsageRecord(context.Background(), id, record)
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
	}
}

func TestQuotaCostWindowBoundariesAndEstimate(t *testing.T) {
	db := newAdminTestStore(t)
	s := &Server{store: db}
	const start int64 = 1_800_000_000_000
	const asOf = start + 3_600_000
	window := quota.Window{LimitID: "codex", Role: "primary", UsedPercent: 25, ResetAt: start + 18_000_000, WindowSeconds: 18000}
	// Retained account total includes older and newer requests. The estimate only
	// includes requests started in this cycle and settled by its quota snapshot.
	seedAccountCost(t, db, accountCostSeed{accountID: 1, start: start - 1, completed: start, cost: ptr(90_000_000), outcome: "succeeded", sent: "sent"})
	seedAccountCost(t, db, accountCostSeed{accountID: 1, start: start, completed: start + 100, cost: ptr(4_000_000), outcome: "succeeded", sent: "sent"})
	seedAccountCost(t, db, accountCostSeed{accountID: 1, start: start + 200, completed: asOf, cost: ptr(6_000_000), outcome: "cancelled", sent: "sent"})
	seedAccountCost(t, db, accountCostSeed{accountID: 1, start: asOf, completed: asOf + 100, cost: ptr(70_000_000), outcome: "succeeded", sent: "sent"})
	seedAccountCost(t, db, accountCostSeed{accountID: 1, start: asOf - 100, completed: asOf + 1, cost: ptr(80_000_000), outcome: "succeeded", sent: "sent"})
	seedAccountCost(t, db, accountCostSeed{accountID: 2, start: start, completed: start + 1, cost: ptr(30_000_000), outcome: "succeeded", sent: "sent"})
	seedAccountCost(t, db, accountCostSeed{accountID: 1, start: start + 300, completed: start + 301, cost: nil, outcome: "failed", sent: "not_sent"})
	seedAccountCost(t, db, accountCostSeed{accountID: 1, start: start + 400, completed: 0, cost: nil, outcome: "running", sent: "sent"})
	for i := 0; i < 2; i++ {
		got := s.buildQuotaWindowCost(context.Background(), 1, window, asOf, asOf+1000)
		reportTestFailure(t, got.Usage == nil || got.Usage.CostMicros != 10_000_000 || got.Usage.PricedRequests != 2 || got.Usage.UnpricedRequests != 0 || got.StartAt != start || got.AsOf != asOf || got.UnavailableReason != "", func() {
			t.Fatalf("window cost: %+v", got)
		})
		reportTestFailure(t, got.EstimatedTotalUSD == nil || *got.EstimatedTotalUSD != 40 || got.EstimatedRemainingUSD == nil || *got.EstimatedRemainingUSD != 30, func() {
			t.Fatalf("estimate: %+v", got)
		})
	}
	all, err := db.AccountUsageCost(context.Background(), 1, store.UsageRange{}, 0)
	reportTestFailure(t, err != nil || all.CostMicros != 250_000_000 || all.PricedRequests != 5 || all.UnpricedRequests != 0, func() {
		t.Fatalf("total: %+v %v", all, err)
	})
}

func TestQuotaCostUnavailableAndZero(t *testing.T) {
	const start int64 = 1_800_000_000_000
	const asOf = start + 3_600_000
	for _, tc := range []struct {
		name, want                string
		percent                   float64
		cost                      *int64
		seed                      bool
		asOf, now, reset, seconds int64
	}{
		{"zero cost", "", 25, ptr(0), true, asOf, asOf, start + 18_000_000, 18000},
		{"exhausted", "", 100, ptr(10_000_000), true, asOf, asOf, start + 18_000_000, 18000},
		{"unknown price", "", 25, nil, true, asOf, asOf, start + 18_000_000, 18000},
		{"no history", "", 25, nil, false, asOf, asOf, start + 18_000_000, 18000},
		{"zero percent", "no_consumption", 0, ptr(10), true, asOf, asOf, start + 18_000_000, 18000},
		{"invalid percent", "no_consumption", math.NaN(), ptr(10), true, asOf, asOf, start + 18_000_000, 18000},
		{"expired", "expired", 25, ptr(10), true, asOf, start + 18_000_000, start + 18_000_000, 18000},
		{"missing timestamp", "missing_snapshot", 25, ptr(10), true, 0, asOf, start + 18_000_000, 18000},
		{"previous cycle", "missing_snapshot", 25, ptr(10), true, start - 1, asOf, start + 18_000_000, 18000},
		{"future snapshot", "missing_snapshot", 25, ptr(10), true, asOf + 1, asOf, start + 18_000_000, 18000},
		{"missing duration", "missing_window", 25, ptr(10), true, asOf, asOf, start + 18_000_000, 0},
		{"overflow duration", "missing_window", 25, ptr(10), true, asOf, asOf, start + 18_000_000, math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newAdminTestStore(t)
			if tc.seed {
				seedAccountCost(t, db, accountCostSeed{accountID: 1, start: start, completed: start + 100, cost: tc.cost, outcome: "succeeded", sent: "sent"})
			}
			s := &Server{store: db}
			got := s.buildQuotaWindowCost(context.Background(), 1, quota.Window{LimitID: "codex", Role: "primary", UsedPercent: tc.percent, ResetAt: tc.reset, WindowSeconds: tc.seconds}, tc.asOf, tc.now)
			reportTestFailure(t, got.UnavailableReason != tc.want, func() {
				t.Fatalf("reason = %q, want %q", got.UnavailableReason, tc.want)
			})
			reportTestFailure(t, tc.want != "" && (got.EstimatedTotalUSD != nil || got.EstimatedRemainingUSD != nil), func() {
				t.Fatalf("unexpected estimate: %+v", got)
			})
			reportTestFailure(t, (tc.name == "zero cost" || tc.name == "unknown price" || tc.name == "no history") && (got.EstimatedTotalUSD == nil || *got.EstimatedTotalUSD != 0), func() {
				t.Fatal("measured zero must remain zero")
			})
			reportTestFailure(t, tc.name == "exhausted" && (got.EstimatedRemainingUSD == nil || *got.EstimatedRemainingUSD != 0), func() {
				t.Fatal("exhausted quota must have zero remaining value")
			})
		})
	}
}

func TestQuotaCostProjectsPartialUsage(t *testing.T) {
	db := newAdminTestStore(t)
	const start int64 = 1_800_000_000_000
	const asOf = start + 3_600_000
	seedAccountCost(t, db, accountCostSeed{accountID: 1, start: start, completed: start + 100, cost: ptr(10_000_000), outcome: "succeeded", sent: "sent"})
	seedAccountCost(t, db, accountCostSeed{accountID: 1, start: start + 200, completed: start + 300, cost: nil, outcome: "succeeded", sent: "sent"})
	s := &Server{store: db}
	got := s.buildQuotaWindowCost(t.Context(), 1, quota.Window{LimitID: "codex", Role: "secondary", UsedPercent: 25, ResetAt: start + 604800000, WindowSeconds: 604800}, asOf, asOf+1000)
	reportTestFailure(t, got.UnavailableReason != "" || got.Usage.UnpricedRequests != 1 || got.EstimatedTotalUSD == nil || *got.EstimatedTotalUSD != 40 || got.EstimatedRemainingUSD == nil || *got.EstimatedRemainingUSD != 30, func() {
		t.Fatalf("partial usage must still project known cost: %+v", got)
	})
}

func TestQuotaCostSnapshotTimestampAndModelLedger(t *testing.T) {
	db := newAdminTestStore(t)
	s := &Server{store: db, accounts: accounts.New(db, nil, nil, accounts.Options{})}
	now := time.Now().UnixMilli()
	asOf := now - 1000
	a := &store.Account{ID: 1, CodexRefreshToken: "linked", QuotaUpdatedAt: now, QuotaError: "refresh failed"}
	window := quota.Window{LimitID: "codex", Role: "secondary", Kind: "7d", UsedPercent: 25, ResetAt: now + 3600000, WindowSeconds: 604800}
	var total int64
	for _, model := range []string{"gpt-6-sol", "gpt-6-luna"} {
		rule, _, ok := pricing.Lookup(model, "", nil)
		reportTestFailure(t, !ok, func() {
			t.Fatalf("missing fixture price for %s", model)
		})
		cost, ok := pricing.Cost(rule, 1000000, 500000, 0, 10000)
		reportTestFailure(t, !ok, func() {
			t.Fatal("cannot price fixture")
		})
		total += cost
		seedAccountCost(t, db, accountCostSeed{accountID: a.ID, start: asOf - 100, completed: asOf - 1, cost: &cost, outcome: "succeeded", sent: "sent"})
	}
	seedAccountCost(t, db, accountCostSeed{accountID: a.ID, start: asOf + 1, completed: now - 1, cost: ptr(100000000), outcome: "succeeded", sent: "sent"})
	report := &quota.Report{FetchedAt: asOf, Windows: []quota.Window{window, {LimitID: "code_review", Role: "primary", UsedPercent: 25, ResetAt: window.ResetAt, WindowSeconds: window.WindowSeconds}}}
	setSnapshot := func() {
		raw, err := json.Marshal(quota.Snapshot{Report: report})
		reportTestFailure(t, err != nil, func() {
			t.Fatal(err)
		})
		a.QuotaJSON = string(raw)
	}
	setSnapshot()
	got := s.buildQuotaView(context.Background(), a)
	reportTestFailure(t, len(got.WindowCosts) != 1 || got.WindowCosts[0].Usage.CostMicros != total || got.WindowCosts[0].AsOf != asOf, func() {
		t.Fatalf("snapshot cost: %+v", got.WindowCosts)
	})
	reportTestFailure(t, got.Cost == nil || got.Cost.CostMicros != total+100000000, func() {
		t.Fatalf("account cost: %+v", got.Cost)
	})
	report.FetchedAt = 0
	setSnapshot()
	got = s.buildQuotaView(context.Background(), a)
	reportTestFailure(t, got.WindowCosts[0].UnavailableReason != "missing_snapshot", func() {
		t.Fatal("failed refresh time used as report timestamp")
	})
	a.QuotaError = ""
	setSnapshot()
	got = s.buildQuotaView(context.Background(), a)
	reportTestFailure(t, got.WindowCosts[0].AsOf != now || got.WindowCosts[0].Usage.CostMicros != total+100000000, func() {
		t.Fatal("legacy successful snapshot did not use account timestamp")
	})
	a.CodexRefreshToken = ""
	got = s.buildQuotaView(context.Background(), a)
	reportTestFailure(t, got.Report != nil || len(got.WindowCosts) != 0 || got.Cost == nil || got.Cost.CostMicros != total+100000000, func() {
		t.Fatal("unlinked accounts must keep model cost without stale quota estimates")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got = s.buildQuotaView(ctx, a)
	reportTestFailure(t, got.Cost != nil, func() {
		t.Fatal("failed cost query shown as measured zero")
	})
}
