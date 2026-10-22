// Package pricing 提供内置 token 价目与整数 micro-USD 计费，仅依赖标准库。
package pricing

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Rule 为每 1M token 的美元单价。
// 达到 LongContextThreshold 时，UnpricedLongContext 表示未定价；
// 否则使用 LongContext，若其为 nil 则沿用短档价格。阈值为 0 时不切换。
type Rule struct {
	Input, CachedInput, CacheWrite, Output float64
	LongContextThreshold                   int64
	LongContext                            *Rule
	// UnpricedLongContext 对应参考实现的 unpriced_long_context。
	// 达到阈值时 Cost 返回 ok=false，而不是把未确认价格当作零。
	UnpricedLongContext bool
}

// Lookup 返回模型价格；tier 为空、standard、default 时使用 standard，
// fast/priority 使用 fast，flex 使用 flex。映射来源 usage.rs:886-893。
// overrides 的 key 是模型名（不含 tier），乘数同时作用于短档和长档。
func Lookup(model, tier string, overrides map[string]float64) (Rule, string, bool) {
	normalized := normalizeModel(model)
	canonical := normalized
	if alias, ok := modelAliases[normalized]; ok {
		canonical = alias
	}
	pricing, ok := pricingRules[canonical]
	if !ok {
		return Rule{}, "", false
	}
	selectedTier, ok := normalizeTier(tier)
	if !ok {
		return Rule{}, "", false
	}
	rates, longRates := pricing.standard, pricing.longStandard
	switch selectedTier {
	case "flex":
		rates, longRates = pricing.flex, pricing.longFlex
	case "fast":
		rates, longRates = pricing.fast, pricing.longFast
	}
	if rates == (tokenRates{}) {
		return Rule{}, "", false
	}
	r := rates.rule(pricing.cacheWritePercent)
	applyLongContextPricing(&r, pricing, longRates)
	for _, key := range []string{model, normalized, canonical} {
		if multiplier, found := overrides[key]; found {
			if !scaleRule(&r, multiplier) {
				return Rule{}, "", false
			}
			return r, "override", true
		}
	}
	return r, "builtin", true
}

// 来源 usage.rs:965-973：去除外围空白及命名空间，并做 ASCII 小写转换。
func normalizeModel(model string) string {
	model = strings.TrimSpace(model)
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, model)
}

func normalizeTier(tier string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "", "standard", "default":
		return "standard", true
	case "flex":
		return "flex", true
	case "fast", "priority":
		return "fast", true
	default:
		return "", false
	}
}

func (rates tokenRates) rule(cacheWritePercent int64) Rule {
	// 来源 usage.rs:944-952、1027-1032；百分比乘法在 ticks 上四舍五入。
	writeTicks := (rates[0]*cacheWritePercent + 50) / 100
	return Rule{
		Input:       float64(rates[0]) / 10_000,
		Output:      float64(rates[1]) / 10_000,
		CachedInput: float64(rates[2]) / 10_000,
		CacheWrite:  float64(writeTicks) / 10_000,
	}
}

// decimal 将公开 float64 的最短十进制表示转为整数分子/分母，
// 不将 IEEE 二进制尾差引入 token 乘法，也不预先把单 token 价格取整为 micros。
func decimal(value float64) (*big.Rat, bool) {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, false
	}
	return new(big.Rat).SetString(strconv.FormatFloat(value, 'f', -1, 64))
}

func scaleRule(r *Rule, multiplier float64) bool {
	factor, ok := decimal(multiplier)
	if !ok {
		return false
	}
	for _, rate := range []*float64{&r.Input, &r.CachedInput, &r.CacheWrite, &r.Output} {
		value, ok := decimal(*rate)
		if !ok {
			return false
		}
		*rate, _ = value.Mul(value, factor).Float64()
		if math.IsInf(*rate, 0) {
			return false
		}
	}
	return r.LongContext == nil || scaleRule(r.LongContext, multiplier)
}

// Cost 返回 micro-USD（USD×1e6），不使用浮点乘法或浮点累加。
// input 包含 cached 与 cacheWrite；uncached=max(input-cached-cacheWrite, 0)。
// 整次请求按原始 input 是否 >= LongContextThreshold 选择长上下文价格，
// 不是只对超过阈值的增量 token 加价（来源 usage.rs:499-510）。
//
// 分段计费来源 usage.rs:902-941，但按本任务约定始终扣除 cached/cacheWrite，
// 即使对应价格为零也扣除；参考代码对零缓存价会改按普通 input 计费，且
// 在 cached+cacheWrite>input 时拒绝计算，本接口改为 uncached 下限为零。
//
// 未确认：参考实现保留 USD ticks，没有定义 micro-USD 的最终舍入规则。
// 本包明确采用各段精确求和后四舍五入（半值向上），不逐段舍入。
// 未定价的长上下文、负 token、非法价格、int64 溢出返回 (0,false)，不用哨兵值。
func Cost(r Rule, input, cached, cacheWrite, output int64) (int64, bool) {
	if input < 0 || cached < 0 || cacheWrite < 0 || output < 0 {
		return 0, false
	}
	if r.LongContextThreshold > 0 && input >= r.LongContextThreshold {
		if r.UnpricedLongContext {
			return 0, false
		}
		if r.LongContext != nil {
			r = *r.LongContext
		}
	}
	// 连续饱和减法避免 cached+cacheWrite 溢出。
	uncached := input - min(input, cached)
	uncached -= min(uncached, cacheWrite)
	total := new(big.Rat)
	for _, segment := range []struct {
		tokens int64
		rate   float64
	}{
		{uncached, r.Input}, {cached, r.CachedInput},
		{cacheWrite, r.CacheWrite}, {output, r.Output},
	} {
		rate, ok := decimal(segment.rate)
		if !ok {
			return 0, false
		}
		// USD/1M token × tokens × 1M micro-USD/USD，两个 1M 恰好抵消。
		total.Add(total, rate.Mul(rate, new(big.Rat).SetInt64(segment.tokens)))
	}
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(total.Num(), total.Denom(), remainder)
	if remainder.Lsh(remainder, 1).Cmp(total.Denom()) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, false
	}
	return quotient.Int64(), true
}

func applyLongContextPricing(r *Rule, pricing modelPricing, longRates tokenRates) {
	// 来源 usage.rs:196-213：模型没有 long_standard 时沿用短价；
	// 当前 tier 没有对应长档时参考实现返回 None，故标记为未定价。
	if pricing.unpricedLongContext {
		r.LongContextThreshold = longContextThreshold
		r.UnpricedLongContext = true
	} else if pricing.longStandard != (tokenRates{}) {
		r.LongContextThreshold = longContextThreshold
		if longRates != (tokenRates{}) {
			long := longRates.rule(pricing.cacheWritePercent)
			r.LongContext = &long
		} else {
			r.UnpricedLongContext = true
		}
	}
}
