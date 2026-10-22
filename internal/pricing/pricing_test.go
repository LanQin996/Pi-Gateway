package pricing

import (
	"math"
	"reflect"
	"testing"
)

func TestLookupKnownModel(t *testing.T) {
	// 顺序为 input、cached、cacheWrite、output，单位 USD/1M token。
	cases := []struct {
		model string
		want  [4]float64
	}{
		{"gpt-6-astra", [4]float64{10, 1, 12.5, 50}},
		{"gpt-6.1-sol", [4]float64{2, 0.1, 2.5, 10}},
		{"gpt-6-sol", [4]float64{2, 0.2, 2.5, 10}},
		{"gpt-6-luna", [4]float64{0.1, 0.01, 0.125, 0.5}},
		{"gpt-5.6-sol", [4]float64{5, 0.5, 6.25, 30}},
		{"gpt-5.6-terra", [4]float64{2, 0.2, 2.5, 12}},
		{"gpt-5.6-luna", [4]float64{0.2, 0.02, 0.25, 1.2}},
		{"gpt-5.6", [4]float64{5, 0.5, 6.25, 30}},
		{"gpt-5.5-pro", [4]float64{30, 0, 0, 180}},
		{"gpt-5.5", [4]float64{5, 0.5, 0, 30}},
		{"gpt-5.4-mini", [4]float64{0.75, 0.075, 0, 4.5}},
		{"gpt-5.4-nano", [4]float64{0.2, 0.02, 0, 1.25}},
		{"gpt-5.4-pro", [4]float64{30, 0, 0, 180}},
		{"gpt-5.4", [4]float64{2.5, 0.25, 0, 15}},
		{"gpt-5.3-codex", [4]float64{1.75, 0.175, 0, 14}},
		{"gpt-5.2-pro", [4]float64{21, 0, 0, 168}},
		{"gpt-5.2", [4]float64{1.75, 0.175, 0, 14}},
		{"gpt-5.1", [4]float64{1.25, 0.125, 0, 10}},
		{"gpt-5-mini", [4]float64{0.25, 0.025, 0, 2}},
		{"gpt-5-nano", [4]float64{0.05, 0.005, 0, 0.4}},
		{"gpt-5-pro", [4]float64{15, 0, 0, 120}},
		{"gpt-5", [4]float64{1.25, 0.125, 0, 10}},
		{"gpt-4.1-mini", [4]float64{0.4, 0.1, 0, 1.6}},
		{"gpt-4.1-nano", [4]float64{0.1, 0.025, 0, 0.4}},
		{"gpt-4.1", [4]float64{2, 0.5, 0, 8}},
		{"gpt-4o-2024-05-13", [4]float64{5, 0, 0, 15}},
		{"gpt-4o-mini", [4]float64{0.15, 0.075, 0, 0.6}},
		{"gpt-4o", [4]float64{2.5, 1.25, 0, 10}},
		{"o1-pro", [4]float64{150, 0, 0, 600}},
		{"o1", [4]float64{15, 7.5, 0, 60}},
		{"o3-pro", [4]float64{20, 0, 0, 80}},
		{"o3-mini", [4]float64{1.1, 0.55, 0, 4.4}},
		{"o3", [4]float64{2, 0.5, 0, 8}},
		{"o4-mini", [4]float64{1.1, 0.275, 0, 4.4}},
		{"gpt-4-turbo", [4]float64{10, 0, 0, 30}},
		{"gpt-4", [4]float64{30, 0, 0, 60}},
		{"gpt-3.5-turbo-instruct", [4]float64{1.5, 0, 0, 2}},
		{"gpt-3.5-turbo-1106", [4]float64{1, 0, 0, 2}},
		{"gpt-3.5-turbo", [4]float64{0.5, 0, 0, 1.5}},
		{"gpt-5.6-cyber", [4]float64{12.5, 1.25, 15.625, 75}},
		{"gpt-5.5-cyber", [4]float64{12.5, 1.25, 0, 75}},
		{"chat-latest", [4]float64{5, 0.5, 0, 30}},
	}
	if len(pricingRules) != len(cases) || len(cases) != 42 {
		t.Fatalf("catalog has %d models, want all 42", len(pricingRules))
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			r, source, ok := Lookup(tc.model, "", nil)
			got := [4]float64{r.Input, r.CachedInput, r.CacheWrite, r.Output}
			if !ok || source != "builtin" || got != tc.want {
				t.Fatalf("Lookup() = (%v, %q, %v), want (%v, builtin, true)", got, source, ok, tc.want)
			}
		})
	}
}

func requireCost(t *testing.T, r Rule, input, cached, cacheWrite, output, want int64) {
	t.Helper()
	got, ok := Cost(r, input, cached, cacheWrite, output)
	if !ok || got != want {
		t.Fatalf("Cost(%d,%d,%d,%d) = (%d,%v), want (%d,true)", input, cached, cacheWrite, output, got, ok, want)
	}
}

func TestLookupOverrideMultiplier(t *testing.T) {
	for _, multiplier := range []float64{0, 0.1, 1, 2, 1.23456} {
		r, source, ok := Lookup("gpt-6-astra", "", map[string]float64{"gpt-6-astra": multiplier})
		if !ok || source != "override" {
			t.Fatalf("Lookup(multiplier=%v) = (%+v, %q, %v)", multiplier, r, source, ok)
		}
		requireCost(t, r, 100, 0, 0, 0, int64(math.Round(1000*multiplier)))
		requireCost(t, r, 300000, 0, 0, 0, int64(math.Round(6000000*multiplier)))
	}
	r, _, _ := Lookup("gpt-6-astra", "", map[string]float64{"gpt-6-astra": 2})
	if r.Input != 20 || r.CachedInput != 2 || r.CacheWrite != 25 || r.Output != 100 || r.LongContext.CacheWrite != 50 {
		t.Fatalf("multiplier did not scale all segments: %+v / %+v", r, r.LongContext)
	}
	r.LongContext.Input = 999
	builtin, source, _ := Lookup("gpt-6-astra", "", nil)
	if source != "builtin" || builtin.LongContext.Input != 20 {
		t.Fatal("override/caller mutation leaked into catalog")
	}
	r, source, ok := Lookup("gpt-5", "flex", map[string]float64{"gpt-5": 2})
	if !ok || source != "override" || r.Input != 1.25 {
		t.Fatalf("tier override = %+v, %q, %v", r, source, ok)
	}
}

func TestLookupTierAndLongContext(t *testing.T) {
	cases := []struct {
		model, tier string
		short, long [4]float64
	}{
		{"gpt-5.4", "flex", [4]float64{1.25, 0.13, 0, 7.5}, [4]float64{2.5, 0.25, 0, 11.25}},
	}
	for _, tc := range cases {
		t.Run(tc.model+"/"+tc.tier, func(t *testing.T) {
			r, _, ok := Lookup(tc.model, tc.tier, nil)
			if !ok || r.LongContextThreshold != 272000 || r.LongContext == nil || r.UnpricedLongContext {
				t.Fatalf("missing prices: %+v, %v", r, ok)
			}
			long := r.LongContext
			if got := [4]float64{r.Input, r.CachedInput, r.CacheWrite, r.Output}; got != tc.short {
				t.Fatalf("short = %v, want %v", got, tc.short)
			}
			if got := [4]float64{long.Input, long.CachedInput, long.CacheWrite, long.Output}; got != tc.long {
				t.Fatalf("long = %v, want %v", got, tc.long)
			}
		})
	}
}

func TestSol61NoLongContextSurcharge(t *testing.T) {
	for _, tc := range []struct {
		tier   string
		factor int64
	}{
		{"", 2}, {"standard", 2}, {"default", 2}, {"flex", 1}, {"fast", 4}, {"priority", 4},
	} {
		t.Run(tc.tier, func(t *testing.T) {
			for _, multiplier := range []float64{1, 2} {
				overrides := map[string]float64{"gpt-6.1-sol": multiplier}
				r, _, ok := Lookup(" /OPENAI/GPT-6.1-SOL ", tc.tier, overrides)
				reportTestFailure(t, !ok || r.LongContextThreshold != 0 || r.LongContext != nil || r.UnpricedLongContext, func() {
					t.Fatalf("long context pricing still enabled: %+v", r)
				})
				for _, input := range []int64{271999, 272000, 272001, 1000000} {
					// uncached + cached/20 + write*1.25 + output*5, scaled by tier and override.
					want := (input - 150000 + 5000 + 62500 + 5000) * tc.factor * int64(multiplier)
					requireCost(t, r, input, 100000, 50000, 1000, want)
					d := Explain("gpt-6.1-sol", tc.tier, overrides, input, 100000, 50000, 1000)
					reportTestFailure(t, d.LongContext || d.ContextThreshold != 0 || d.TotalCostMicros == nil || *d.TotalCostMicros != want || d.UnavailableReason != "", func() {
						t.Fatalf("incorrect bill: %+v, want %d", d, want)
					})
					reportTestFailure(t, *d.ContextMultipliers != (Rates{1, 1, 1, 1}), func() {
						t.Fatalf("context surcharge in bill: %+v", d.ContextMultipliers)
					})
				}
			}
		})
	}
}

func TestLookupAliasesAndNormalization(t *testing.T) {
	for alias, canonical := range modelAliases {
		r, source, ok := Lookup(" /openai/"+alias+" ", "", nil)
		want, _, _ := Lookup(canonical, "", nil)
		if !ok || source != "builtin" || !reflect.DeepEqual(r, want) {
			t.Fatalf("alias %s differs from %s", alias, canonical)
		}
	}
	r, source, ok := Lookup(" /OPENAI/GPT-5 ", " FlEx ", map[string]float64{"gpt-5": 2})
	if !ok || source != "override" || r.Input != 1.25 {
		t.Fatalf("normalized override = %+v, %q, %v", r, source, ok)
	}
}

func TestLookupUnknownModel(t *testing.T) {
	for _, model := range []string{"", "not-a-real-model", "gpt-5-2099-01-01", "gpt-5-made-up", "ft:gpt-5", "gpt-5:flex"} {
		if _, _, ok := Lookup(model, "", map[string]float64{model: 2}); ok {
			t.Errorf("unknown model %q matched even though override is only a multiplier", model)
		}
	}
	for _, tc := range []struct{ model, tier string }{{"gpt-5", "invalid"}, {"o1", "flex"}, {"gpt-5-nano", "fast"}} {
		if _, _, ok := Lookup(tc.model, tc.tier, nil); ok {
			t.Errorf("unknown/unconfigured tier %s/%s matched", tc.model, tc.tier)
		}
	}
}

func TestLookupInvalidMultiplier(t *testing.T) {
	for _, multiplier := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1), math.MaxFloat64} {
		if _, _, ok := Lookup("gpt-6-astra", "", map[string]float64{"gpt-6-astra": multiplier}); ok {
			t.Errorf("invalid/overflowing multiplier %v accepted", multiplier)
		}
	}
}

func TestCostSegmentsSubtractCachedAndCacheWrite(t *testing.T) {
	r, _, _ := Lookup("gpt-6-astra", "", nil)
	// 8×10 + 2×1 + 2×12.5 + 3×50 = 257 micros。
	requireCost(t, r, 12, 2, 2, 3, 257)
	// 缓存段价为零也扣除，不回退 input 单价；uncached 下限为零。
	requireCost(t, Rule{Input: 10, Output: 50}, 12, 2, 2, 3, 230)
	requireCost(t, r, 1, 2, 2, 0, 27)
}

func TestCostLongContextSwitch(t *testing.T) {
	r, _, _ := Lookup("gpt-5.4", "", nil)
	requireCost(t, r, 271999, 0, 0, 0, 679998)
	requireCost(t, r, 272000, 0, 0, 0, 1360000)
	requireCost(t, r, 272001, 0, 0, 0, 1360005)
	// 阈值使用总 input，不是 uncached；所有分段一起使用长价。
	r, _, _ = Lookup("gpt-6-astra", "fast", nil)
	requireCost(t, r, 300000, 100000, 100000, 2, 9400300)
}

func TestCostMissingLongBandRetainsShortPrice(t *testing.T) {
	r, _, _ := Lookup("gpt-5", "", nil)
	if r.LongContextThreshold != 0 || r.LongContext != nil || r.UnpricedLongContext {
		t.Fatalf("model without long band must retain short price: %+v", r)
	}
	requireCost(t, r, 300000, 0, 0, 0, 375000)
	// 手动 Rule 有阈值但长价为 nil，且不是未定价，也沿用短价。
	requireCost(t, Rule{Input: 1, LongContextThreshold: 272000}, 300000, 0, 0, 0, 300000)
}

func TestCostUnpricedLongContext(t *testing.T) {
	cases := []struct{ model, tier string }{
		{"gpt-5.6-cyber", ""}, {"gpt-5.5-cyber", ""},
		{"gpt-5.5-pro", "flex"},
	}
	for _, tc := range cases {
		r, _, ok := Lookup(tc.model, tc.tier, nil)
		if !ok || r.LongContextThreshold != 272000 || r.LongContext != nil || !r.UnpricedLongContext {
			t.Fatalf("%s/%s must preserve unavailable long band: %+v", tc.model, tc.tier, r)
		}
		if _, ok := Cost(r, 271999, 0, 0, 0); !ok {
			t.Errorf("%s must price short context", tc.model)
		}
		if got, ok := Cost(r, 272001, 0, 0, 0); ok || got != 0 {
			t.Errorf("%s long context = (%d,%v), want (0,false)", tc.model, got, ok)
		}
	}
	// 显式未定价优先于提供的长价。
	r := Rule{LongContextThreshold: 1, LongContext: &Rule{Input: 1}, UnpricedLongContext: true}
	if got, ok := Cost(r, 2, 0, 0, 0); got != 0 || ok {
		t.Fatalf("unpriced flag must take precedence: (%d,%v)", got, ok)
	}
}

func TestCostUsesIntegerMicrosWithoutFloatDrift(t *testing.T) {
	cases := []struct {
		model, tier         string
		input, cached, want int64
	}{
		{"gpt-5", "flex", 1_000_000, 0, 625000},
		{"o4-mini", "flex", 1000, 1000, 138},
		{"gpt-5.4", "flex", 1000, 1000, 130},
		{"gpt-6-luna", "", 90, 0, 9},
	}
	for _, tc := range cases {
		r, _, _ := Lookup(tc.model, tc.tier, nil)
		requireCost(t, r, tc.input, tc.cached, 0, 0, tc.want)
	}
	// 超过 float64 的连续整数范围仍逐 token 精确：2^53+1。
	requireCost(t, Rule{Input: 1}, 9007199254740993, 0, 0, 0, 9007199254740993)
}

func TestCostRoundsOnlyAfterSumming(t *testing.T) {
	requireCost(t, Rule{Input: 0.1, CachedInput: 0.2, CacheWrite: 0.3, Output: 0.4}, 3, 1, 1, 1, 1)
	for _, tc := range []struct {
		rate float64
		want int64
	}{{0.49, 0}, {0.5, 1}, {1.49, 1}, {1.5, 2}} {
		requireCost(t, Rule{Input: tc.rate}, 1, 0, 0, 0, tc.want)
	}
}

func TestCostInvalidAndOverflow(t *testing.T) {
	for _, rate := range []float64{-1, math.NaN(), math.Inf(1)} {
		if got, ok := Cost(Rule{Input: rate}, 1, 0, 0, 0); got != 0 || ok {
			t.Errorf("invalid rate %v = (%d,%v), want (0,false)", rate, got, ok)
		}
	}
	for _, tc := range []struct {
		rate  float64
		input int64
	}{{1, -1}, {2, math.MaxInt64}} {
		if got, ok := Cost(Rule{Input: tc.rate}, tc.input, 0, 0, 0); got != 0 || ok {
			t.Errorf("invalid tokens or overflow = (%d,%v), want (0,false)", got, ok)
		}
	}
	requireCost(t, Rule{Input: 1}, math.MaxInt64, math.MaxInt64, math.MaxInt64, 0, 0)
}
