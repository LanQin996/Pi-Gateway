package pricing

// 初始价目移植自 _research/codex-proxy-rs/backend/crates/providers/openai/src/transport/usage.rs:234-486。
// Long fast/pro 及 >= 阈值边界参照 github.com/james-6-23/codex2api/database/billing.go 补齐。
// 以下每项的行号均指该来源文件，不宣称重新核验了上游公开价格。
// TokenRates 参数顺序和单位照抄来源 :105-125：input, output, cache_read，
// 数值为 USD / 1M token 的万分之一，亦为单 token 的 USD ticks。
type tokenRates [3]int64

// 来源 :136-221（ModelPricing / PricingTier）。未配置的档位保持零值，不能继承 standard。
type modelPricing struct {
	standard, flex, fast             tokenRates
	longStandard, longFlex, longFast tokenRates
	cacheWritePercent                int64
	unpricedLongContext              bool
}

// 来源 :16（LONG_CONTEXT_THRESHOLD）；input >= 阈值才切换整次请求的价格。
const longContextThreshold int64 = 272_000

var pricingRules = map[string]modelPricing{
	// 来源 :237-246。
	"gpt-6-astra": {
		standard: tokenRates{100_000, 500_000, 10_000}, flex: tokenRates{50_000, 250_000, 5_000}, fast: tokenRates{200_000, 1_000_000, 20_000},
		longStandard: tokenRates{200_000, 750_000, 20_000}, longFlex: tokenRates{100_000, 375_000, 10_000}, longFast: tokenRates{400_000, 1_500_000, 40_000}, cacheWritePercent: 125,
	},
	// 基础价格来源 :247-256；GPT-6.1 Sol 不启用长上下文加价。
	"gpt-6.1-sol": {
		standard: tokenRates{20_000, 100_000, 1_000}, flex: tokenRates{10_000, 50_000, 500}, fast: tokenRates{40_000, 200_000, 2_000},
		cacheWritePercent: 125,
	},
	// 来源 :258-267。
	"gpt-6-sol": {
		standard: tokenRates{20_000, 100_000, 2_000}, flex: tokenRates{10_000, 50_000, 1_000}, fast: tokenRates{40_000, 200_000, 4_000},
		longStandard: tokenRates{40_000, 150_000, 4_000}, longFlex: tokenRates{20_000, 75_000, 2_000}, longFast: tokenRates{80_000, 300_000, 8_000}, cacheWritePercent: 125,
	},
	// 来源 :269-278。
	"gpt-6-luna": {
		standard: tokenRates{1_000, 5_000, 100}, flex: tokenRates{500, 2_500, 50}, fast: tokenRates{2_000, 10_000, 200},
		longStandard: tokenRates{2_000, 7_500, 200}, longFlex: tokenRates{1_000, 3_750, 100}, longFast: tokenRates{4_000, 15_000, 400}, cacheWritePercent: 125,
	},
	// 来源 :279-288。
	"gpt-5.6-sol": {
		standard: tokenRates{50_000, 300_000, 5_000}, flex: tokenRates{25_000, 150_000, 2_500}, fast: tokenRates{100_000, 600_000, 10_000},
		longStandard: tokenRates{100_000, 450_000, 10_000}, longFlex: tokenRates{50_000, 225_000, 5_000}, longFast: tokenRates{200_000, 900_000, 20_000}, cacheWritePercent: 125,
	},
	// 来源 :289-298。
	"gpt-5.6-terra": {
		standard: tokenRates{20_000, 120_000, 2_000}, flex: tokenRates{10_000, 60_000, 1_000}, fast: tokenRates{40_000, 240_000, 4_000},
		longStandard: tokenRates{40_000, 180_000, 4_000}, longFlex: tokenRates{20_000, 90_000, 2_000}, longFast: tokenRates{80_000, 360_000, 8_000}, cacheWritePercent: 125,
	},
	// 来源 :299-308。
	"gpt-5.6-luna": {
		standard: tokenRates{2_000, 12_000, 200}, flex: tokenRates{1_000, 6_000, 100}, fast: tokenRates{4_000, 24_000, 400},
		longStandard: tokenRates{4_000, 18_000, 400}, longFlex: tokenRates{2_000, 9_000, 200}, longFast: tokenRates{8_000, 36_000, 800}, cacheWritePercent: 125,
	},
	// 来源 :309-318。
	"gpt-5.6": {
		standard: tokenRates{50_000, 300_000, 5_000}, flex: tokenRates{25_000, 150_000, 2_500}, fast: tokenRates{100_000, 600_000, 10_000},
		longStandard: tokenRates{100_000, 450_000, 10_000}, longFlex: tokenRates{50_000, 225_000, 5_000}, longFast: tokenRates{200_000, 900_000, 20_000}, cacheWritePercent: 125,
	},
	// 来源 :319-324。
	"gpt-5.5-pro": {standard: tokenRates{300_000, 1_800_000, 0}, flex: tokenRates{150_000, 900_000, 0}, fast: tokenRates{750_000, 4_500_000, 0}, longStandard: tokenRates{600_000, 2_700_000, 0}, longFast: tokenRates{1_500_000, 6_750_000, 0}},
	// 来源 :325-332。
	"gpt-5.5": {standard: tokenRates{50_000, 300_000, 5_000}, flex: tokenRates{25_000, 150_000, 2_500}, fast: tokenRates{125_000, 750_000, 12_500}, longStandard: tokenRates{100_000, 450_000, 10_000}, longFlex: tokenRates{50_000, 225_000, 5_000}, longFast: tokenRates{250_000, 1_125_000, 25_000}},
	// 来源 :333-338。
	"gpt-5.4-mini": {standard: tokenRates{7_500, 45_000, 750}, flex: tokenRates{3_750, 22_500, 375}, fast: tokenRates{15_000, 90_000, 1_500}},
	// 来源 :339-342。
	"gpt-5.4-nano": {standard: tokenRates{2_000, 12_500, 200}, flex: tokenRates{1_000, 6_250, 100}},
	// 来源 :343-349。
	"gpt-5.4-pro": {standard: tokenRates{300_000, 1_800_000, 0}, flex: tokenRates{150_000, 900_000, 0}, fast: tokenRates{750_000, 4_500_000, 0}, longStandard: tokenRates{600_000, 2_700_000, 0}, longFlex: tokenRates{300_000, 1_350_000, 0}, longFast: tokenRates{1_500_000, 6_750_000, 0}},
	// 来源 :350-357；flex cached=1300 按原值移植，不用 standard/2 推算。
	"gpt-5.4": {standard: tokenRates{25_000, 150_000, 2_500}, flex: tokenRates{12_500, 75_000, 1_300}, fast: tokenRates{50_000, 300_000, 5_000}, longStandard: tokenRates{50_000, 225_000, 5_000}, longFlex: tokenRates{25_000, 112_500, 2_500}, longFast: tokenRates{100_000, 450_000, 10_000}},
	// 来源 :358-361。
	"gpt-5.3-codex": {standard: tokenRates{17_500, 140_000, 1_750}, fast: tokenRates{35_000, 280_000, 3_500}},
	// 来源 :362-365。
	"gpt-5.2-pro": {standard: tokenRates{210_000, 1_680_000, 0}},
	// 来源 :366-371。
	"gpt-5.2": {standard: tokenRates{17_500, 140_000, 1_750}, flex: tokenRates{8_750, 70_000, 875}, fast: tokenRates{35_000, 280_000, 3_500}},
	// 来源 :372-377。
	"gpt-5.1": {standard: tokenRates{12_500, 100_000, 1_250}, flex: tokenRates{6_250, 50_000, 625}, fast: tokenRates{25_000, 200_000, 2_500}},
	// 来源 :378-383。
	"gpt-5-mini": {standard: tokenRates{2_500, 20_000, 250}, flex: tokenRates{1_250, 10_000, 125}, fast: tokenRates{4_500, 36_000, 450}},
	// 来源 :384-387。
	"gpt-5-nano": {standard: tokenRates{500, 4_000, 50}, flex: tokenRates{250, 2_000, 25}},
	// 来源 :388-391。
	"gpt-5-pro": {standard: tokenRates{150_000, 1_200_000, 0}},
	// 来源 :392-397。
	"gpt-5": {standard: tokenRates{12_500, 100_000, 1_250}, flex: tokenRates{6_250, 50_000, 625}, fast: tokenRates{25_000, 200_000, 2_500}},
	// 来源 :398-401。
	"gpt-4.1-mini": {standard: tokenRates{4_000, 16_000, 1_000}, fast: tokenRates{7_000, 28_000, 1_750}},
	// 来源 :402-405。
	"gpt-4.1-nano": {standard: tokenRates{1_000, 4_000, 250}, fast: tokenRates{2_000, 8_000, 500}},
	// 来源 :406-409。
	"gpt-4.1": {standard: tokenRates{20_000, 80_000, 5_000}, fast: tokenRates{35_000, 140_000, 8_750}},
	// 来源 :410-413。
	"gpt-4o-2024-05-13": {standard: tokenRates{50_000, 150_000, 0}, fast: tokenRates{87_500, 262_500, 0}},
	// 来源 :414-417。
	"gpt-4o-mini": {standard: tokenRates{1_500, 6_000, 750}, fast: tokenRates{2_500, 10_000, 1_250}},
	// 来源 :418-421。
	gpt4oModel: {standard: tokenRates{25_000, 100_000, 12_500}, fast: tokenRates{42_500, 170_000, 21_250}},
	// 来源 :422-425。
	"o1-pro": {standard: tokenRates{1_500_000, 6_000_000, 0}},
	// 来源 :426-429。
	"o1": {standard: tokenRates{150_000, 600_000, 75_000}},
	// 来源 :430-433。
	"o3-pro": {standard: tokenRates{200_000, 800_000, 0}},
	// 来源 :434-437。
	"o3-mini": {standard: tokenRates{11_000, 44_000, 5_500}},
	// 来源 :438-443。
	"o3": {standard: tokenRates{20_000, 80_000, 5_000}, flex: tokenRates{10_000, 40_000, 2_500}, fast: tokenRates{35_000, 140_000, 8_750}},
	// 来源 :444-449；flex cached=1380 按原值移植。
	"o4-mini": {standard: tokenRates{11_000, 44_000, 2_750}, flex: tokenRates{5_500, 22_000, 1_380}, fast: tokenRates{20_000, 80_000, 5_000}},
	// 来源 :450-453。
	"gpt-4-turbo": {standard: tokenRates{100_000, 300_000, 0}},
	// 来源 :454-457。
	"gpt-4": {standard: tokenRates{300_000, 600_000, 0}},
	// 来源 :458-461。
	"gpt-3.5-turbo-instruct": {standard: tokenRates{15_000, 20_000, 0}},
	// 来源 :462-465。
	"gpt-3.5-turbo-1106": {standard: tokenRates{10_000, 20_000, 0}},
	// 来源 :466-469。
	"gpt-3.5-turbo": {standard: tokenRates{5_000, 15_000, 0}},
	// 来源 :470-477。未确认：Cyber 长上下文价格来源存在差异，原实现不估价。
	"gpt-5.6-cyber": {standard: tokenRates{125_000, 750_000, 12_500}, cacheWritePercent: 125, unpricedLongContext: true},
	// 来源 :478-481。未确认：同上；不能继承父模型的缓存写入价和档位。
	"gpt-5.5-cyber": {standard: tokenRates{125_000, 750_000, 12_500}, unpricedLongContext: true},
	// 来源 :482-485。
	"chat-latest": {standard: tokenRates{50_000, 300_000, 5_000}},
}

// 来源 _research/codex-proxy-rs/backend/crates/providers/openai/src/transport/usage.rs:975-1011。
// 只承认逐项列出的别名/快照，不匹配任意后缀或未来日期。
var modelAliases = map[string]string{
	"gpt-3.5-turbo-0125":       "gpt-3.5-turbo",
	"gpt-4-0613":               "gpt-4",
	"gpt-4-turbo-2024-04-09":   "gpt-4-turbo",
	"gpt-4.1-2025-04-14":       "gpt-4.1",
	"gpt-4.1-mini-2025-04-14":  "gpt-4.1-mini",
	"gpt-4.1-nano-2025-04-14":  "gpt-4.1-nano",
	"gpt-4o-2024-08-06":        gpt4oModel,
	"gpt-4o-2024-11-20":        gpt4oModel,
	"gpt-4o-mini-2024-07-18":   "gpt-4o-mini",
	"gpt-5-2025-08-07":         "gpt-5",
	"gpt-5-mini-2025-08-07":    "gpt-5-mini",
	"gpt-5-nano-2025-08-07":    "gpt-5-nano",
	"gpt-5-pro-2025-10-06":     "gpt-5-pro",
	"gpt-5.1-2025-11-13":       "gpt-5.1",
	"gpt-5.2-2025-12-11":       "gpt-5.2",
	"gpt-5.2-pro-2025-12-11":   "gpt-5.2-pro",
	"gpt-5.4-2026-03-05":       "gpt-5.4",
	"gpt-5.4-mini-2026-03-17":  "gpt-5.4-mini",
	"gpt-5.4-nano-2026-03-17":  "gpt-5.4-nano",
	"gpt-5.4-pro-2026-03-05":   "gpt-5.4-pro",
	"gpt-5.5-2026-04-23":       "gpt-5.5",
	"gpt-5.5-pro-2026-04-23":   "gpt-5.5-pro",
	"gpt-daybreak-blue-latest": "gpt-5.6-sol",
	"gpt-daybreak-red-latest":  "gpt-5.6-cyber",
	"o1-2024-12-17":            "o1",
	"o1-pro-2025-03-19":        "o1-pro",
	"o3-2025-04-16":            "o3",
	"o3-mini-2025-01-31":       "o3-mini",
	"o3-pro-2025-06-10":        "o3-pro",
	"o4-mini-2025-04-16":       "o4-mini",
}

const gpt4oModel = "gpt-4o"
