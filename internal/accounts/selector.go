package accounts

import (
	"math"
	"sort"
	"time"

	"pi-gateway/internal/store"
)

// Strategy selects how eligible accounts are ordered.
type Strategy string

const (
	StrategySmart              Strategy = "smart"
	StrategyQuotaResetPriority Strategy = "quota_reset_priority"
	StrategyRoundRobin         Strategy = "round_robin"
	StrategySticky             Strategy = "sticky"
	StrategyLeastInflight      Strategy = "least_inflight"
	StrategySingle             Strategy = "single"
)

// SmartWeights are tenths, not percentages. Queue is reserved: this scheduler
// has no per-account queue signal and therefore uses a neutral value of one.
type SmartWeights struct {
	Load               int     `json:"load"`
	Quota              int     `json:"quota"`
	Health             int     `json:"health"`
	Latency            int     `json:"latency"`
	Reset              int     `json:"reset"`
	Queue              int     `json:"queue"`
	PreferHigherWeight bool    `json:"prefer_higher_weight"`
	ScoreTolerance     float64 `json:"score_tolerance"`
}

func DefaultSmartWeights() SmartWeights {
	return SmartWeights{Load: 10, Quota: 8, Health: 10, Latency: 5}
}

// Candidate timestamps are Unix milliseconds, matching store.Account.
type Candidate struct {
	Account            *store.Account
	InFlight           int
	LastStartedAt      int64
	QuotaResetAt       int64
	QuotaRemainingRank float64
	FailureRate        float64
	FirstOutputMS      float64
	CooldownUntil      int64
}

// Select returns a new slice; callers serialize access to cursor. Eligibility
// (including the request interval) belongs to Manager. Active cooldowns and
// excluded IDs are also rejected here for direct callers. Sticky preference is
// applied by Manager, which owns the bounded per-key/session hint caches.
func Select(candidates []Candidate, strategy Strategy, w SmartWeights, excluded map[int64]bool, cursor *uint64) []Candidate {
	return selectAt(candidates, strategy, w, excluded, cursor, time.Now(), 0)
}

func selectAt(candidates []Candidate, strategy Strategy, w SmartWeights, excluded map[int64]bool, cursor *uint64, now time.Time, defaultLimit int) []Candidate {
	out := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Account != nil && !excluded[c.Account.ID] && c.CooldownUntil <= now.UnixMilli() && c.Account.CooldownUntil <= now.UnixMilli() {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return out
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Account.Weight != out[j].Account.Weight {
			return out[i].Account.Weight > out[j].Account.Weight
		}
		return out[i].Account.ID < out[j].Account.ID
	})
	if !w.PreferHigherWeight {
		end := 1
		for end < len(out) && out[end].Account.Weight == out[0].Account.Weight {
			end++
		}
		out = out[:end]
	}
	// Each weight tier is ordered independently, so a low-weight high score
	// cannot outrank the preferred tier. One scheduling decision advances once.
	var position uint64
	if cursor != nil {
		position = *cursor
	}
	orderWeightTiers(out, strategy, w, position, now, defaultLimit)
	if cursor != nil && (advancesCursor(strategy)) {
		*cursor++
	}
	if strategy == StrategySingle {
		return out[:1]
	}
	return out
}

func knownStrategy(s Strategy) bool {
	switch s {
	case StrategySmart, StrategyQuotaResetPriority, StrategyRoundRobin, StrategySticky, StrategyLeastInflight, StrategySingle:
		return true
	}
	return false
}

func orderTier(out []Candidate, strategy Strategy, w SmartWeights, cursor uint64, now time.Time, defaultLimit int) {
	switch strategy {
	case StrategySticky, StrategySingle:
		return // ID order is the legacy fallback.
	case StrategyLeastInflight:
		sort.SliceStable(out, func(i, j int) bool { return out[i].InFlight < out[j].InFlight })
	case StrategyQuotaResetPriority:
		orderQuotaResetTier(out, defaultLimit)
	case StrategySmart:
		scores := make(map[int64]float64, len(out))
		for _, c := range out {
			scores[c.Account.ID] = smartScore(c, w, now, defaultLimit)
		}
		sort.SliceStable(out, func(i, j int) bool { return scores[out[i].Account.ID] > scores[out[j].Account.ID] })
		top := scores[out[0].Account.ID]
		end := 1
		tolerance := math.Max(0, w.ScoreTolerance)
		for end < len(out) && top-scores[out[end].Account.ID] <= tolerance {
			end++
		}
		sort.Slice(out[:end], func(i, j int) bool { return out[i].Account.ID < out[j].Account.ID })
		rotate(out[:end], cursor)
	default:
		rotate(out, cursor)
	}
}

func rotate(out []Candidate, cursor uint64) {
	if len(out) < 2 {
		return
	}
	start := int(cursor % uint64(len(out)))
	copy(out, append(append([]Candidate(nil), out[start:]...), out[:start]...))
}

func clamp(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(0, math.Min(1, v))
}

func utilization(c Candidate, defaultLimit int) float64 {
	limit := c.Account.Concurrency
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit <= 0 {
		return 0
	}
	return clamp(float64(c.InFlight) / float64(limit))
}

func smartScore(c Candidate, w SmartWeights, now time.Time, defaultLimit int) float64 {
	latency := 1.0
	if c.FirstOutputMS > 0 {
		latency = 10000 / (10000 + c.FirstOutputMS)
	}
	reset := 0.0
	if c.QuotaResetAt > now.UnixMilli() {
		reset = 3600 / (3600 + float64(c.QuotaResetAt-now.UnixMilli())/1000)
	}
	return (float64(w.Load)*(1-utilization(c, defaultLimit)) + float64(w.Quota)*clamp(c.QuotaRemainingRank) + float64(w.Health)*(1-clamp(c.FailureRate)) + float64(w.Latency)*latency + float64(w.Reset)*reset + float64(w.Queue)) / 10
}

func orderQuotaResetTier(out []Candidate, defaultLimit int) {
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.QuotaResetAt <= 0) != (b.QuotaResetAt <= 0) {
			return a.QuotaResetAt > 0
		}
		if a.QuotaResetAt != b.QuotaResetAt {
			return a.QuotaResetAt < b.QuotaResetAt
		}
		au, bu := utilization(a, defaultLimit), utilization(b, defaultLimit)
		if au != bu {
			return au < bu
		}
		if a.LastStartedAt != b.LastStartedAt {
			return a.LastStartedAt < b.LastStartedAt
		}
		return a.Account.ID < b.Account.ID
	})
}

func orderWeightTiers(out []Candidate, strategy Strategy, w SmartWeights, cursor uint64, now time.Time, defaultLimit int) {
	for start := 0; start < len(out); {
		end := start + 1
		for end < len(out) && out[end].Account.Weight == out[start].Account.Weight {
			end++
		}
		orderTier(out[start:end], strategy, w, cursor, now, defaultLimit)
		start = end
	}
}

func advancesCursor(strategy Strategy) bool {
	return strategy == StrategySmart || strategy == StrategyRoundRobin || !knownStrategy(strategy)
}
