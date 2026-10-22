// Package keylimit enforces per-client-API-key admission: concurrency, requests
// per minute and daily/weekly spend budgets.
//
// The counters live in this process. A deployment running more than one replica
// therefore enforces these limits loosely (each replica counts only its own
// traffic); the reference implementation keeps the same counters in Redis
// precisely so that this is exact across replicas. Budget usage is the exception:
// it is read from and settled in the database, so it stays correct everywhere.
package keylimit

import (
	"context"
	"math"
	"sync"
	"time"

	"pi-gateway/internal/store"
)

// Limits bounds one key. A zero value means "unlimited" for every field, which
// matches the reference implementation (0 is not a rejection).
type Limits struct {
	MaxConcurrency    int
	RequestsPerMinute int
	DailyLimitMicros  int64
	WeeklyLimitMicros int64
}

// Decision reports whether a request may proceed.
type Decision struct {
	Allowed    bool
	Reason     string // concurrency | rpm | budget
	RetryAfter time.Duration
}

// LimitsFromKey derives the effective limits from a stored key.
func LimitsFromKey(k *store.APIKey) Limits {
	if k == nil {
		return Limits{}
	}
	return Limits{
		MaxConcurrency:    k.MaxConcurrency,
		RequestsPerMinute: k.RequestsPerMinute,
		DailyLimitMicros:  usdToMicros(k.DailyLimitUSD),
		WeeklyLimitMicros: usdToMicros(k.WeeklyLimitUSD),
	}
}

func usdToMicros(usd float64) int64 {
	if usd <= 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0
	}
	return int64(math.Round(usd * 1e6))
}

const rateWindow = time.Minute

// Limiter tracks in-flight requests and the rolling one-minute counter.
type Limiter struct {
	store *store.Store
	now   func() time.Time

	mu     sync.Mutex
	active map[int64]int
	recent map[int64][]time.Time
}

// NewLimiter creates a limiter backed by st for budget checks. A nil store
// disables budget checks (useful in tests); concurrency and RPM still work.
func NewLimiter(st *store.Store) *Limiter {
	return &Limiter{
		store:  st,
		now:    time.Now,
		active: map[int64]int{},
		recent: map[int64][]time.Time{},
	}
}

// SetClock replaces the time source (tests only).
func (l *Limiter) SetClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now != nil {
		l.now = now
	}
}

// Admit checks the limits and reserves a concurrency slot when allowed. The
// returned release function must be called when the request finishes; it is
// idempotent, so calling it more than once is safe. When the request is rejected
// no slot is taken and release is still safe to call.
//
// A rejection is immediate rather than queued: the caller decides whether to
// retry after RetryAfter, so one key can never block the whole gateway.
func (l *Limiter) Admit(ctx context.Context, keyID int64, lim Limits, now time.Time) (Decision, func(), error) {
	noop := func() { /* No reservation exists to release after rejection. */ }

	l.mu.Lock()
	stamp := now
	// Concurrency first: it is the cheapest and the most likely rejection.
	if lim.MaxConcurrency > 0 && l.active[keyID] >= lim.MaxConcurrency {
		l.mu.Unlock()
		return Decision{Reason: "concurrency", RetryAfter: time.Second}, noop, nil
	}
	// Rolling one-minute window.
	cutoff := stamp.Add(-rateWindow)
	kept := prune(l.recent[keyID], cutoff)
	if lim.RequestsPerMinute > 0 && len(kept) >= lim.RequestsPerMinute {
		l.recent[keyID] = kept
		l.mu.Unlock()
		retry := time.Second
		if len(kept) > 0 {
			retry = kept[0].Add(rateWindow).Sub(stamp)
			if retry < 0 {
				retry = 0
			}
		}
		return Decision{Reason: "rpm", RetryAfter: retry}, noop, nil
	}
	l.active[keyID]++
	l.recent[keyID] = append(kept, stamp)
	l.mu.Unlock()

	// Budget is checked after reserving so the check can touch the database
	// without holding the mutex; a rejection rolls the reservation back.
	if dec, err := l.checkBudget(ctx, keyID, lim, now); err != nil {
		l.rollback(keyID, stamp)
		return Decision{}, noop, err
	} else if !dec.Allowed {
		l.rollback(keyID, stamp)
		return dec, noop, nil
	}

	return Decision{Allowed: true}, l.releaseFunc(keyID), nil
}

func (l *Limiter) releaseFunc(keyID int64) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.active[keyID] > 0 {
				l.active[keyID]--
			}
			if l.active[keyID] == 0 {
				delete(l.active, keyID)
			}
		})
	}
}

func (l *Limiter) checkBudget(ctx context.Context, keyID int64, lim Limits, now time.Time) (Decision, error) {
	if l.store == nil || (lim.DailyLimitMicros <= 0 && lim.WeeklyLimitMicros <= 0) {
		return Decision{Allowed: true}, nil
	}
	windows, err := l.store.KeyBudgetStatus(ctx, keyID, now.UnixMilli())
	if err != nil {
		return Decision{}, err
	}
	for _, w := range windows {
		switch w.Period {
		case "daily":
			if lim.DailyLimitMicros > 0 && w.UsedMicros >= lim.DailyLimitMicros {
				return Decision{Reason: "budget", RetryAfter: untilNextWindow(now, w.Period)}, nil
			}
		case "weekly":
			if lim.WeeklyLimitMicros > 0 && w.UsedMicros >= lim.WeeklyLimitMicros {
				return Decision{Reason: "budget", RetryAfter: untilNextWindow(now, w.Period)}, nil
			}
		}
	}
	return Decision{Allowed: true}, nil
}

// untilNextWindow mirrors the store's UTC natural-day / Monday-start-week windows.
func untilNextWindow(now time.Time, period string) time.Duration {
	t := now.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	var next time.Time
	if period == "weekly" {
		week := day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
		next = week.AddDate(0, 0, 7)
	} else {
		next = day.AddDate(0, 0, 1)
	}
	if d := next.Sub(t); d > 0 {
		return d
	}
	return 0
}

// Active reports the current in-flight count for a key (diagnostics/tests).
func (l *Limiter) Active(keyID int64) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active[keyID]
}

func (l *Limiter) rollback(keyID int64, stamp time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[keyID] > 0 {
		l.active[keyID]--
	}
	if l.active[keyID] == 0 {
		delete(l.active, keyID)
	}
	// Drop the exact timestamp this attempt appended, from the back so a
	// concurrent append is never removed.
	items := l.recent[keyID]
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Equal(stamp) {
			items = append(items[:i], items[i+1:]...)
			break
		}
	}
	if len(items) == 0 {
		delete(l.recent, keyID)
	} else {
		l.recent[keyID] = items
	}
}

func prune(items []time.Time, cutoff time.Time) []time.Time {
	kept := items[:0]
	for _, t := range items {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}
