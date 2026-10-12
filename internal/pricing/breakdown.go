package pricing

// Version is stored with the settled bill, so future price changes do not
// rewrite historical explanations.
const Version = "model-pricing-v3"

type Rates struct {
	Input       float64 `json:"input"`
	CachedInput float64 `json:"cached_input"`
	CacheWrite  float64 `json:"cache_write"`
	Output      float64 `json:"output"`
}

type Breakdown struct {
	Version            string `json:"version"`
	Tier               string `json:"tier"`
	TierSource         string `json:"tier_source"`
	PriceSource        string `json:"price_source"`
	UnavailableReason  string `json:"unavailable_reason,omitempty"`
	LongContext        bool   `json:"long_context"`
	ContextThreshold   int64  `json:"context_threshold"`
	BaseRates          *Rates `json:"base_rates"`
	ContextRates       *Rates `json:"context_rates"`
	EffectiveRates     *Rates `json:"effective_rates"`
	ContextMultipliers *Rates `json:"context_multipliers"`
	TierMultipliers    *Rates `json:"tier_multipliers"`
	BaseCostMicros     *int64 `json:"base_cost_micros"`
	ContextCostMicros  *int64 `json:"context_cost_micros"`
	TotalCostMicros    *int64 `json:"total_cost_micros"`
}

func rates(r Rule) *Rates { return &Rates{r.Input, r.CachedInput, r.CacheWrite, r.Output} }
func ratios(a, b Rule) *Rates {
	ratio := func(x, y float64) float64 {
		if y == 0 {
			return 1
		}
		return x / y
	}
	return &Rates{ratio(a.Input, b.Input), ratio(a.CachedInput, b.CachedInput), ratio(a.CacheWrite, b.CacheWrite), ratio(a.Output, b.Output)}
}

func contextRule(r Rule, input int64) (Rule, bool) {
	if r.LongContextThreshold > 0 && input >= r.LongContextThreshold {
		if r.UnpricedLongContext {
			return Rule{}, false
		}
		if r.LongContext != nil {
			return *r.LongContext, true
		}
	}
	return r, true
}

// Explain uses the same integer settlement as Cost. Each stage selects all
// segment prices; a long input changes output prices too, and a service tier
// may have its own prices instead of one common multiplier.
func Explain(model, tier string, overrides map[string]float64, input, cached, write, output int64) Breakdown {
	d := Breakdown{Version: Version, Tier: tier}
	base, source, ok := Lookup(model, "standard", overrides)
	if !ok {
		d.UnavailableReason = "unknown_model"
		return d
	}
	d.PriceSource = source
	d.BaseRates = rates(base)
	d.ContextThreshold = base.LongContextThreshold
	d.LongContext = base.LongContextThreshold > 0 && input >= base.LongContextThreshold
	baseShort := base
	baseShort.LongContextThreshold = 0
	if cost, ok := Cost(baseShort, input, cached, write, output); ok {
		d.BaseCostMicros = &cost
	}
	context, ok := contextRule(base, input)
	if !ok {
		d.UnavailableReason = "unpriced_context"
		return d
	}
	d.ContextRates = rates(context)
	d.ContextMultipliers = ratios(context, baseShort)
	if cost, ok := Cost(context, input, cached, write, output); ok {
		d.ContextCostMicros = &cost
	}
	selected, _, ok := Lookup(model, tier, overrides)
	if !ok {
		d.UnavailableReason = "unpriced_tier"
		return d
	}
	d.Tier, _ = normalizeTier(tier)
	effective, ok := contextRule(selected, input)
	if !ok {
		d.UnavailableReason = "unpriced_context"
		return d
	}
	d.EffectiveRates = rates(effective)
	d.TierMultipliers = ratios(effective, context)
	if cost, ok := Cost(selected, input, cached, write, output); ok {
		d.TotalCostMicros = &cost
	} else {
		d.UnavailableReason = "invalid_usage"
	}
	return d
}
