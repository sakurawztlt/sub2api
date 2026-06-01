//go:build unit

package service

import (
	"math"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func newTestBillingService() *BillingService {
	return NewBillingService(&config.Config{}, nil)
}

func TestCalculateCost_BasicComputation(t *testing.T) {
	svc := newTestBillingService()

	// 使用 claude-sonnet-4 的回退价格：Input $3/MTok, Output $15/MTok
	tokens := UsageTokens{
		InputTokens:  1000,
		OutputTokens: 500,
	}
	cost, err := svc.CalculateCost("claude-sonnet-4", tokens, 1.0)
	require.NoError(t, err)

	// 1000 * 3e-6 = 0.003, 500 * 15e-6 = 0.0075
	expectedInput := 1000 * 3e-6
	expectedOutput := 500 * 15e-6
	require.InDelta(t, expectedInput, cost.InputCost, 1e-10)
	require.InDelta(t, expectedOutput, cost.OutputCost, 1e-10)
	require.InDelta(t, expectedInput+expectedOutput, cost.TotalCost, 1e-10)
	require.InDelta(t, expectedInput+expectedOutput, cost.ActualCost, 1e-10)
}

func TestCalculateCost_WithCacheTokens(t *testing.T) {
	svc := newTestBillingService()

	tokens := UsageTokens{
		InputTokens:         1000,
		OutputTokens:        500,
		CacheCreationTokens: 2000,
		CacheReadTokens:     3000,
	}
	cost, err := svc.CalculateCost("claude-sonnet-4", tokens, 1.0)
	require.NoError(t, err)

	expectedCacheCreation := 2000 * 3.75e-6
	expectedCacheRead := 3000 * 0.3e-6
	require.InDelta(t, expectedCacheCreation, cost.CacheCreationCost, 1e-10)
	require.InDelta(t, expectedCacheRead, cost.CacheReadCost, 1e-10)

	expectedTotal := cost.InputCost + cost.OutputCost + expectedCacheCreation + expectedCacheRead
	require.InDelta(t, expectedTotal, cost.TotalCost, 1e-10)
}

func TestCalculateCost_RateMultiplier(t *testing.T) {
	svc := newTestBillingService()

	tokens := UsageTokens{InputTokens: 1000, OutputTokens: 500}

	cost1x, err := svc.CalculateCost("claude-sonnet-4", tokens, 1.0)
	require.NoError(t, err)

	cost2x, err := svc.CalculateCost("claude-sonnet-4", tokens, 2.0)
	require.NoError(t, err)

	// TotalCost 不受倍率影响，ActualCost 翻倍
	require.InDelta(t, cost1x.TotalCost, cost2x.TotalCost, 1e-10)
	require.InDelta(t, cost1x.ActualCost*2, cost2x.ActualCost, 1e-10)
}

func TestGetModelPricing_FallbackMatchesByFamily(t *testing.T) {
	svc := newTestBillingService()

	tests := []struct {
		model         string
		expectedInput float64
	}{
		{"claude-opus-4.5-20250101", 5e-6},
		{"claude-opus-4-8-20260601", 5e-6},
		{"claude-3-opus-20240229", 15e-6},
		{"claude-sonnet-4-20250514", 3e-6},
		{"claude-3-5-sonnet-20241022", 3e-6},
		{"claude-3-5-haiku-20241022", 1e-6},
		{"claude-3-haiku-20240307", 0.25e-6},
	}

	for _, tt := range tests {
		pricing, err := svc.GetModelPricing(tt.model)
		require.NoError(t, err, "模型 %s", tt.model)
		require.InDelta(t, tt.expectedInput, pricing.InputPricePerToken, 1e-12, "模型 %s 输入价格", tt.model)
	}
}

func TestGetModelPricing_CaseInsensitive(t *testing.T) {
	svc := newTestBillingService()

	p1, err := svc.GetModelPricing("Claude-Sonnet-4")
	require.NoError(t, err)

	p2, err := svc.GetModelPricing("claude-sonnet-4")
	require.NoError(t, err)

	require.Equal(t, p1.InputPricePerToken, p2.InputPricePerToken)
}

func TestGetModelPricing_UnknownClaudeModelFallsBackToSonnet(t *testing.T) {
	svc := newTestBillingService()

	// 不包含 opus/sonnet/haiku 关键词的 Claude 模型会走默认 Sonnet 价格
	pricing, err := svc.GetModelPricing("claude-unknown-model")
	require.NoError(t, err)
	require.InDelta(t, 3e-6, pricing.InputPricePerToken, 1e-12)
}

func TestGetModelPricing_UnknownOpenAIModelReturnsError(t *testing.T) {
	svc := newTestBillingService()

	pricing, err := svc.GetModelPricing("gpt-unknown-model")
	require.Error(t, err)
	require.Nil(t, pricing)
	require.Contains(t, err.Error(), "pricing not found")
}

func TestGetModelPricing_OpenAIGPT54Fallback(t *testing.T) {
	svc := newTestBillingService()

	pricing, err := svc.GetModelPricing("gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, pricing)
	require.InDelta(t, 2.5e-6, pricing.InputPricePerToken, 1e-12)
	require.InDelta(t, 15e-6, pricing.OutputPricePerToken, 1e-12)
	require.InDelta(t, 0.25e-6, pricing.CacheReadPricePerToken, 1e-12)
	require.Equal(t, 272000, pricing.LongContextInputThreshold)
	require.InDelta(t, 2.0, pricing.LongContextInputMultiplier, 1e-12)
	require.InDelta(t, 1.5, pricing.LongContextOutputMultiplier, 1e-12)
}

func TestGetModelPricing_OpenAIGPT54MiniFallback(t *testing.T) {
	svc := newTestBillingService()

	pricing, err := svc.GetModelPricing("gpt-5.4-mini")
	require.NoError(t, err)
	require.NotNil(t, pricing)
	require.InDelta(t, 7.5e-7, pricing.InputPricePerToken, 1e-12)
	require.InDelta(t, 4.5e-6, pricing.OutputPricePerToken, 1e-12)
	require.InDelta(t, 7.5e-8, pricing.CacheReadPricePerToken, 1e-12)
	require.Zero(t, pricing.LongContextInputThreshold)
}

func TestCalculateCost_OpenAIGPT54LongContextAppliesWholeSessionMultipliers(t *testing.T) {
	svc := newTestBillingService()

	tokens := UsageTokens{
		InputTokens:  300000,
		OutputTokens: 4000,
	}

	cost, err := svc.CalculateCost("gpt-5.4-2026-03-05", tokens, 1.0)
	require.NoError(t, err)

	expectedInput := float64(tokens.InputTokens) * 2.5e-6 * 2.0
	expectedOutput := float64(tokens.OutputTokens) * 15e-6 * 1.5
	require.InDelta(t, expectedInput, cost.InputCost, 1e-10)
	require.InDelta(t, expectedOutput, cost.OutputCost, 1e-10)
	require.InDelta(t, expectedInput+expectedOutput, cost.TotalCost, 1e-10)
	require.InDelta(t, expectedInput+expectedOutput, cost.ActualCost, 1e-10)
}

func TestGetFallbackPricing_FamilyMatching(t *testing.T) {
	svc := newTestBillingService()

	tests := []struct {
		name             string
		model            string
		expectedInput    float64
		expectNilPricing bool
	}{
		{name: "empty model", model: "   ", expectNilPricing: true},
		{name: "claude opus 4.8", model: "claude-opus-4-8-20260601", expectedInput: 5e-6},
		{name: "claude opus 4.6", model: "claude-opus-4.6-20260201", expectedInput: 5e-6},
		{name: "claude opus 4.5 alt separator", model: "claude-opus-4-5-20260101", expectedInput: 5e-6},
		{name: "claude generic model fallback sonnet", model: "claude-foo-bar", expectedInput: 3e-6},
		{name: "gemini explicit fallback", model: "gemini-3-1-pro", expectedInput: 2e-6},
		{name: "gemini unknown no fallback", model: "gemini-2.0-pro", expectNilPricing: true},
		{name: "openai gpt5.4", model: "gpt-5.4", expectedInput: 2.5e-6},
		{name: "openai gpt5.4 mini", model: "gpt-5.4-mini", expectedInput: 7.5e-7},
		{name: "openai gpt5.3 codex", model: "gpt-5.3-codex", expectedInput: 1.5e-6},
		{name: "openai gpt5.3 codex spark", model: "gpt-5.3-codex-spark", expectedInput: 1.5e-6},
		// codex round54 fu64: gpt-5.5 不再 fallback 到 gpt-5.4 半价, 用专属
		// $5/$0.5/$30 fallback pricing. 防 gcr Opus 升 5.5 后 NewAPI 报表
		// 跟 OpenAI 实际账单偏离 2x.
		{name: "openai gpt5.5 uses gpt5.5 fallback (not gpt5.4)", model: "gpt-5.5", expectedInput: 5e-6},
		{name: "openai gpt5.5 high variant", model: "gpt-5.5-high", expectedInput: 5e-6},
		{name: "openai gpt5.5 xhigh variant", model: "gpt-5.5-xhigh", expectedInput: 5e-6},
		{name: "openai legacy gpt5.1 falls back to gpt5.4", model: "gpt-5.1", expectedInput: 2.5e-6},
		{name: "openai legacy gpt5.1 codex falls back to gpt5.3 codex", model: "gpt-5.1-codex", expectedInput: 1.5e-6},
		{name: "openai legacy codex mini latest falls back to gpt5.3 codex", model: "codex-mini-latest", expectedInput: 1.5e-6},
		{name: "openai unknown no fallback", model: "gpt-unknown-model", expectNilPricing: true},
		{name: "non supported family", model: "qwen-max", expectNilPricing: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pricing := svc.getFallbackPricing(tt.model)
			if tt.expectNilPricing {
				require.Nil(t, pricing)
				return
			}
			require.NotNil(t, pricing)
			require.InDelta(t, tt.expectedInput, pricing.InputPricePerToken, 1e-12)
		})
	}
}
func TestCalculateCostWithLongContext_BelowThreshold(t *testing.T) {
	svc := newTestBillingService()

	tokens := UsageTokens{
		InputTokens:     50000,
		OutputTokens:    1000,
		CacheReadTokens: 100000,
	}
	// 总输入 150k < 200k 阈值，应走正常计费
	cost, err := svc.CalculateCostWithLongContext("claude-sonnet-4", tokens, 1.0, 200000, 2.0)
	require.NoError(t, err)

	normalCost, err := svc.CalculateCost("claude-sonnet-4", tokens, 1.0)
	require.NoError(t, err)

	require.InDelta(t, normalCost.ActualCost, cost.ActualCost, 1e-10)
}

func TestCalculateCostWithLongContext_AboveThreshold_CacheExceedsThreshold(t *testing.T) {
	svc := newTestBillingService()

	// 缓存 210k + 输入 10k = 220k > 200k 阈值
	// 缓存已超阈值：范围内 200k 缓存，范围外 10k 缓存 + 10k 输入
	tokens := UsageTokens{
		InputTokens:     10000,
		OutputTokens:    1000,
		CacheReadTokens: 210000,
	}
	cost, err := svc.CalculateCostWithLongContext("claude-sonnet-4", tokens, 1.0, 200000, 2.0)
	require.NoError(t, err)

	// 范围内：200k cache + 0 input + 1k output
	inRange, _ := svc.CalculateCost("claude-sonnet-4", UsageTokens{
		InputTokens:     0,
		OutputTokens:    1000,
		CacheReadTokens: 200000,
	}, 1.0)

	// 范围外：10k cache + 10k input，倍率 2.0
	outRange, _ := svc.CalculateCost("claude-sonnet-4", UsageTokens{
		InputTokens:     10000,
		CacheReadTokens: 10000,
	}, 2.0)

	require.InDelta(t, inRange.ActualCost+outRange.ActualCost, cost.ActualCost, 1e-10)
}

func TestCalculateCostWithLongContext_AboveThreshold_CacheBelowThreshold(t *testing.T) {
	svc := newTestBillingService()

	// 缓存 100k + 输入 150k = 250k > 200k 阈值
	// 缓存未超阈值：范围内 100k 缓存 + 100k 输入，范围外 50k 输入
	tokens := UsageTokens{
		InputTokens:     150000,
		OutputTokens:    1000,
		CacheReadTokens: 100000,
	}
	cost, err := svc.CalculateCostWithLongContext("claude-sonnet-4", tokens, 1.0, 200000, 2.0)
	require.NoError(t, err)

	require.True(t, cost.ActualCost > 0, "费用应大于 0")

	// 正常费用不含长上下文
	normalCost, _ := svc.CalculateCost("claude-sonnet-4", tokens, 1.0)
	require.True(t, cost.ActualCost > normalCost.ActualCost, "长上下文费用应高于正常费用")
}

func TestCalculateCostWithLongContext_DisabledThreshold(t *testing.T) {
	svc := newTestBillingService()

	tokens := UsageTokens{InputTokens: 300000, CacheReadTokens: 0}

	// threshold <= 0 应禁用长上下文计费
	cost1, err := svc.CalculateCostWithLongContext("claude-sonnet-4", tokens, 1.0, 0, 2.0)
	require.NoError(t, err)

	cost2, err := svc.CalculateCost("claude-sonnet-4", tokens, 1.0)
	require.NoError(t, err)

	require.InDelta(t, cost2.ActualCost, cost1.ActualCost, 1e-10)
}

func TestCalculateCostWithLongContext_ExtraMultiplierLessEqualOne(t *testing.T) {
	svc := newTestBillingService()

	tokens := UsageTokens{InputTokens: 300000}

	// extraMultiplier <= 1 应禁用长上下文计费
	cost, err := svc.CalculateCostWithLongContext("claude-sonnet-4", tokens, 1.0, 200000, 1.0)
	require.NoError(t, err)

	normalCost, err := svc.CalculateCost("claude-sonnet-4", tokens, 1.0)
	require.NoError(t, err)

	require.InDelta(t, normalCost.ActualCost, cost.ActualCost, 1e-10)
}

func TestCalculateImageCost(t *testing.T) {
	svc := newTestBillingService()

	price := 0.134
	cfg := &ImagePriceConfig{Price1K: &price}
	cost := svc.CalculateImageCost("gpt-image-1", "1K", 3, cfg, 1.0)

	require.InDelta(t, 0.134*3, cost.TotalCost, 1e-10)
	require.InDelta(t, 0.134*3, cost.ActualCost, 1e-10)
}

func TestIsModelSupported(t *testing.T) {
	svc := newTestBillingService()

	require.True(t, svc.IsModelSupported("claude-sonnet-4"))
	require.True(t, svc.IsModelSupported("Claude-Opus-4.5"))
	require.True(t, svc.IsModelSupported("claude-3-haiku"))
	require.False(t, svc.IsModelSupported("gpt-4o"))
	require.False(t, svc.IsModelSupported("gemini-pro"))
}

func TestCalculateCost_ZeroTokens(t *testing.T) {
	svc := newTestBillingService()

	cost, err := svc.CalculateCost("claude-sonnet-4", UsageTokens{}, 1.0)
	require.NoError(t, err)
	require.Equal(t, 0.0, cost.TotalCost)
	require.Equal(t, 0.0, cost.ActualCost)
}

func TestCalculateCostWithConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1.5
	svc := NewBillingService(cfg, nil)

	tokens := UsageTokens{InputTokens: 1000, OutputTokens: 500}
	cost, err := svc.CalculateCostWithConfig("claude-sonnet-4", tokens)
	require.NoError(t, err)

	expected, _ := svc.CalculateCost("claude-sonnet-4", tokens, 1.5)
	require.InDelta(t, expected.ActualCost, cost.ActualCost, 1e-10)
}

func TestCalculateCostWithConfig_ZeroMultiplier(t *testing.T) {
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 0
	svc := NewBillingService(cfg, nil)

	tokens := UsageTokens{InputTokens: 1000}
	cost, err := svc.CalculateCostWithConfig("claude-sonnet-4", tokens)
	require.NoError(t, err)

	// 倍率 <=0 时默认 1.0
	expected, _ := svc.CalculateCost("claude-sonnet-4", tokens, 1.0)
	require.InDelta(t, expected.ActualCost, cost.ActualCost, 1e-10)
}

func TestGetEstimatedCost(t *testing.T) {
	svc := newTestBillingService()

	est, err := svc.GetEstimatedCost("claude-sonnet-4", 1000, 500)
	require.NoError(t, err)
	require.True(t, est > 0)
}

func TestListSupportedModels(t *testing.T) {
	svc := newTestBillingService()

	models := svc.ListSupportedModels()
	require.NotEmpty(t, models)
	require.GreaterOrEqual(t, len(models), 6)
}

func TestGetPricingServiceStatus_NilService(t *testing.T) {
	svc := newTestBillingService()

	status := svc.GetPricingServiceStatus()
	require.NotNil(t, status)
	require.Equal(t, "using fallback", status["last_updated"])
}

func TestForceUpdatePricing_NilService(t *testing.T) {
	svc := newTestBillingService()

	err := svc.ForceUpdatePricing()
	require.Error(t, err)
	require.Contains(t, err.Error(), "not initialized")
}

func TestCalculateCostWithLongContext_PropagatesError(t *testing.T) {
	// 使用空的 fallback prices 让 GetModelPricing 失败
	svc := &BillingService{
		cfg:            &config.Config{},
		fallbackPrices: make(map[string]*ModelPricing),
	}

	tokens := UsageTokens{InputTokens: 300000, CacheReadTokens: 0}
	_, err := svc.CalculateCostWithLongContext("unknown-model", tokens, 1.0, 200000, 2.0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pricing not found")
}

func TestCalculateCost_SupportsCacheBreakdown(t *testing.T) {
	svc := &BillingService{
		cfg: &config.Config{},
		fallbackPrices: map[string]*ModelPricing{
			"claude-sonnet-4": {
				InputPricePerToken:     3e-6,
				OutputPricePerToken:    15e-6,
				SupportsCacheBreakdown: true,
				CacheCreation5mPrice:   4e-6, // per token
				CacheCreation1hPrice:   5e-6, // per token
			},
		},
	}

	tokens := UsageTokens{
		InputTokens:           1000,
		OutputTokens:          500,
		CacheCreation5mTokens: 100000,
		CacheCreation1hTokens: 50000,
	}
	cost, err := svc.CalculateCost("claude-sonnet-4", tokens, 1.0)
	require.NoError(t, err)

	expected5m := float64(tokens.CacheCreation5mTokens) * 4e-6
	expected1h := float64(tokens.CacheCreation1hTokens) * 5e-6
	require.InDelta(t, expected5m+expected1h, cost.CacheCreationCost, 1e-10)
}

func TestCalculateCost_LargeTokenCount(t *testing.T) {
	svc := newTestBillingService()

	tokens := UsageTokens{
		InputTokens:  1_000_000,
		OutputTokens: 1_000_000,
	}
	cost, err := svc.CalculateCost("claude-sonnet-4", tokens, 1.0)
	require.NoError(t, err)

	// Input: 1M * 3e-6 = $3, Output: 1M * 15e-6 = $15
	require.InDelta(t, 3.0, cost.InputCost, 1e-6)
	require.InDelta(t, 15.0, cost.OutputCost, 1e-6)
	require.False(t, math.IsNaN(cost.TotalCost))
	require.False(t, math.IsInf(cost.TotalCost, 0))
}

func TestServiceTierCostMultiplier(t *testing.T) {
	require.InDelta(t, 2.0, serviceTierCostMultiplier("priority"), 1e-12)
	require.InDelta(t, 2.0, serviceTierCostMultiplier(" Priority "), 1e-12)
	require.InDelta(t, 0.5, serviceTierCostMultiplier("flex"), 1e-12)
	require.InDelta(t, 1.0, serviceTierCostMultiplier(""), 1e-12)
	require.InDelta(t, 1.0, serviceTierCostMultiplier("default"), 1e-12)
}

func TestCalculateCostWithServiceTier_OpenAIPriorityUsesPriorityPricing(t *testing.T) {
	svc := newTestBillingService()
	tokens := UsageTokens{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 20}

	baseCost, err := svc.CalculateCost("gpt-5.1-codex", tokens, 1.0)
	require.NoError(t, err)

	priorityCost, err := svc.CalculateCostWithServiceTier("gpt-5.1-codex", tokens, 1.0, "priority")
	require.NoError(t, err)

	require.InDelta(t, baseCost.InputCost*2, priorityCost.InputCost, 1e-10)
	require.InDelta(t, baseCost.OutputCost*2, priorityCost.OutputCost, 1e-10)
	require.InDelta(t, baseCost.CacheReadCost*2, priorityCost.CacheReadCost, 1e-10)
	require.InDelta(t, baseCost.TotalCost*2, priorityCost.TotalCost, 1e-10)
}

// codex round55 fu65 (2026-05-21): gpt-5.5 priority 真按 2.5x 走 (官方
// $12.5/$1.25/$75), 不是 fu64 误设的 2x. service_tier=priority 默认被
// scrub_service_tier.go 剥离, 但管理员/channel-level priority 配置仍可触发,
// fu64 underbill 是真 bug.
func TestCalculateCostWithServiceTier_GPT55PriorityUses_2_5x(t *testing.T) {
	svc := newTestBillingService()
	tokens := UsageTokens{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 20}

	baseCost, err := svc.CalculateCost("gpt-5.5", tokens, 1.0)
	require.NoError(t, err)

	priorityCost, err := svc.CalculateCostWithServiceTier("gpt-5.5", tokens, 1.0, "priority")
	require.NoError(t, err)

	// Official ratio per developers.openai.com docs: priority = 2.5x standard.
	require.InDelta(t, baseCost.InputCost*2.5, priorityCost.InputCost, 1e-10,
		"gpt-5.5 priority input must be exactly 2.5x base (codex round55)")
	require.InDelta(t, baseCost.OutputCost*2.5, priorityCost.OutputCost, 1e-10,
		"gpt-5.5 priority output must be exactly 2.5x base (codex round55)")
	require.InDelta(t, baseCost.CacheReadCost*2.5, priorityCost.CacheReadCost, 1e-10,
		"gpt-5.5 priority cached read must be exactly 2.5x base (codex round55)")
}

// codex round55 fu65: 明确防 future regression to 2x (codex round54 fu64 bug).
func TestCalculateCostWithServiceTier_GPT55PriorityIsNot_2x(t *testing.T) {
	svc := newTestBillingService()
	tokens := UsageTokens{InputTokens: 100, OutputTokens: 50}

	baseCost, err := svc.CalculateCost("gpt-5.5", tokens, 1.0)
	require.NoError(t, err)
	priorityCost, err := svc.CalculateCostWithServiceTier("gpt-5.5", tokens, 1.0, "priority")
	require.NoError(t, err)

	// 2x 是 fu64 的 underbill bug; 不允许 future regression.
	require.Greater(t, math.Abs(priorityCost.InputCost-baseCost.InputCost*2), 1e-9,
		"gpt-5.5 priority input must NOT be 2x (fu64 regression guard); got %v expected ≠ %v",
		priorityCost.InputCost, baseCost.InputCost*2)
	require.Greater(t, math.Abs(priorityCost.OutputCost-baseCost.OutputCost*2), 1e-9,
		"gpt-5.5 priority output must NOT be 2x (fu64 regression guard); got %v expected ≠ %v",
		priorityCost.OutputCost, baseCost.OutputCost*2)
}

// codex round56 fu66 (2026-05-21): priority cache_creation 也按 2.5x 走.
// fu65 漏了, computeCacheCreationCost 仍按 standard CacheCreationPricePerToken
// 算; 管理员 pass priority + cache write 时少计费. fu66 加
// CacheCreationPricePerTokenPriority 字段 + priority swap branch.
func TestRound56_GPT55PriorityCacheCreationUses_2_5x(t *testing.T) {
	svc := newTestBillingService()
	tokens := UsageTokens{InputTokens: 100, OutputTokens: 50, CacheCreationTokens: 40}

	baseCost, err := svc.CalculateCost("gpt-5.5", tokens, 1.0)
	require.NoError(t, err)
	priorityCost, err := svc.CalculateCostWithServiceTier("gpt-5.5", tokens, 1.0, "priority")
	require.NoError(t, err)

	require.InDelta(t, baseCost.CacheCreationCost*2.5, priorityCost.CacheCreationCost, 1e-10,
		"gpt-5.5 priority cache_creation must be 2.5x base (codex round56 #1)")
	// 全 4 个 cost dim 一致 2.5x
	require.InDelta(t, baseCost.InputCost*2.5, priorityCost.InputCost, 1e-10)
	require.InDelta(t, baseCost.OutputCost*2.5, priorityCost.OutputCost, 1e-10)
}

// codex round56 fu66 (2026-05-21): long-context multiplier 之前只乘 input/
// output. cached input 在长上下文也上浮 (官方 GPT-5.5 doc), cache write 跟
// input 同价格族也上浮. 用 InputMultiplier (input-side 流量).
func TestRound56_LongContext_CacheReadAlsoUsesInputMultiplier(t *testing.T) {
	svc := newTestBillingService()

	// 普通 (input + cache_read 合计 < 272K)
	smallTokens := UsageTokens{InputTokens: 1000, OutputTokens: 100, CacheReadTokens: 500}
	smallCost, err := svc.CalculateCost("gpt-5.5", smallTokens, 1.0)
	require.NoError(t, err)

	// 触发长上下文 (input + cache_read 合计 > 272K)
	largeTokens := UsageTokens{InputTokens: 200000, OutputTokens: 100, CacheReadTokens: 100000}
	largeCost, err := svc.CalculateCost("gpt-5.5", largeTokens, 1.0)
	require.NoError(t, err)

	// gpt-5.5 long-context input multiplier = 2x; CacheReadCost 每 token 应上浮 2x.
	smallPerToken := smallCost.CacheReadCost / float64(smallTokens.CacheReadTokens)
	largePerToken := largeCost.CacheReadCost / float64(largeTokens.CacheReadTokens)
	require.InDelta(t, smallPerToken*2.0, largePerToken, 1e-12,
		"gpt-5.5 long-context cache_read 单价必须 2x 上浮 (codex round56 #2)")
}

func TestRound56_LongContext_CacheCreationAlsoUsesInputMultiplier(t *testing.T) {
	svc := newTestBillingService()

	smallTokens := UsageTokens{InputTokens: 1000, OutputTokens: 100, CacheCreationTokens: 500}
	smallCost, err := svc.CalculateCost("gpt-5.5", smallTokens, 1.0)
	require.NoError(t, err)

	// 此 fixture 用 InputTokens > 272K 触发阈值 (round56 时 cache_creation 不计入
	// 阈值; round57 fu67 已修 — cache_creation 也算入阈值, 见
	// TestRound57_LongContextTriggerByCacheCreationAlone 覆盖 cache write 单独
	// 触发 case). 这条仍按 InputTokens 主导触发, 保留作为 round56 主路径不变验证.
	largeTokens := UsageTokens{InputTokens: 280000, OutputTokens: 100, CacheCreationTokens: 100000}
	largeCost, err := svc.CalculateCost("gpt-5.5", largeTokens, 1.0)
	require.NoError(t, err)

	smallPerToken := smallCost.CacheCreationCost / float64(smallTokens.CacheCreationTokens)
	largePerToken := largeCost.CacheCreationCost / float64(largeTokens.CacheCreationTokens)
	require.InDelta(t, smallPerToken*2.0, largePerToken, 1e-12,
		"gpt-5.5 long-context cache_creation 单价必须 2x 上浮 (codex round56 #2)")
}

// codex round56 fu66 #3: codex 担心 JSON 缺 gpt-5.5 entry → UI/枚举接口
// 可能看不到. 真路径 verify: GetModelPricing("gpt-5.5") via fallback 必须
// 返完整 ModelPricing (priority + long-context + cache_creation 全字段).
// 不依赖 model_prices_and_context_window.json.
func TestRound56_GPT55FallbackProvidesCompletePriorityPricing(t *testing.T) {
	svc := newTestBillingService()
	pricing, err := svc.GetModelPricing("gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, pricing)

	// 标准价格
	require.InDelta(t, 5e-6, pricing.InputPricePerToken, 1e-12)
	require.InDelta(t, 30e-6, pricing.OutputPricePerToken, 1e-12)
	require.InDelta(t, 0.5e-6, pricing.CacheReadPricePerToken, 1e-12)
	require.InDelta(t, 5e-6, pricing.CacheCreationPricePerToken, 1e-12)

	// Priority 2.5x 全字段
	require.InDelta(t, 12.5e-6, pricing.InputPricePerTokenPriority, 1e-12,
		"priority input 字段必须从 fallback 走通 (codex round56 #3)")
	require.InDelta(t, 75e-6, pricing.OutputPricePerTokenPriority, 1e-12)
	require.InDelta(t, 1.25e-6, pricing.CacheReadPricePerTokenPriority, 1e-12)
	require.InDelta(t, 12.5e-6, pricing.CacheCreationPricePerTokenPriority, 1e-12,
		"priority cache_creation 字段必须从 fallback 走通 (codex round56 #3)")

	// Long-context multiplier 字段
	require.Equal(t, 272000, pricing.LongContextInputThreshold)
	require.InDelta(t, 2.0, pricing.LongContextInputMultiplier, 1e-12)
	require.InDelta(t, 1.5, pricing.LongContextOutputMultiplier, 1e-12)
}

// codex round57 fu67 (2026-05-21): shouldApplySessionLongContextPricing
// 之前 totalInputTokens 只算 InputTokens + CacheReadTokens, 没算
// CacheCreationTokens. 边缘场景: 冷启动大量 cache write — input + cache_read
// 不过 272K, 但 cache_creation 巨大. cache_creation 也是 input-side 流量,
// 应该参与长上下文阈值判断. 否则 cache_creation 单价该 2x 但实际仍标准,
// underbill.
func TestRound57_LongContextTriggerByCacheCreationAlone(t *testing.T) {
	svc := newTestBillingService()

	// InputTokens 很小, CacheCreationTokens > 272K — 现在应触发长上下文
	smallTokens := UsageTokens{InputTokens: 1000, OutputTokens: 100, CacheCreationTokens: 500}
	smallCost, err := svc.CalculateCost("gpt-5.5", smallTokens, 1.0)
	require.NoError(t, err)
	smallPerToken := smallCost.CacheCreationCost / float64(smallTokens.CacheCreationTokens)

	// 关键 fixture: InputTokens 远低于 272K, CacheCreationTokens 单独触发阈值
	largeTokens := UsageTokens{InputTokens: 1000, OutputTokens: 100, CacheCreationTokens: 300000}
	largeCost, err := svc.CalculateCost("gpt-5.5", largeTokens, 1.0)
	require.NoError(t, err)
	largePerToken := largeCost.CacheCreationCost / float64(largeTokens.CacheCreationTokens)

	require.InDelta(t, smallPerToken*2.0, largePerToken, 1e-12,
		"gpt-5.5 长上下文阈值必须算入 cache_creation: 仅大量 cache write 应触发 2x 上浮 (codex round57)")
}

// 边界: cache_creation 单独不够阈值, 但加上 input + cache_read 后过阈值,
// 行为应跟之前一致 (cache_creation 上浮 2x). codex round56 已覆盖此 case,
// 此 test 防 round57 修改让该场景退化.
func TestRound57_LongContextStillTriggeredByCombinedInputAndCacheCreation(t *testing.T) {
	svc := newTestBillingService()
	// input=100K, cache_read=100K, cache_creation=100K — combined 300K > 272K
	tokens := UsageTokens{InputTokens: 100000, OutputTokens: 100,
		CacheReadTokens: 100000, CacheCreationTokens: 100000}
	cost, err := svc.CalculateCost("gpt-5.5", tokens, 1.0)
	require.NoError(t, err)

	// cache_creation per-token cost = standard 5e-6 × LongContextInputMultiplier 2 = 10e-6
	require.InDelta(t, 100000*10e-6, cost.CacheCreationCost, 1e-9,
		"combined > 272K cache_creation 单价 2x 上浮 (round56 行为不变)")
	require.InDelta(t, 100000*1e-6, cost.CacheReadCost, 1e-9,
		"combined > 272K cache_read 单价 2x 上浮 (round56 行为不变)")
}

func TestCalculateCostWithServiceTier_FlexAppliesHalfMultiplier(t *testing.T) {
	svc := newTestBillingService()
	tokens := UsageTokens{InputTokens: 100, OutputTokens: 50, CacheCreationTokens: 40, CacheReadTokens: 20}

	baseCost, err := svc.CalculateCost("gpt-5.4", tokens, 1.0)
	require.NoError(t, err)

	flexCost, err := svc.CalculateCostWithServiceTier("gpt-5.4", tokens, 1.0, "flex")
	require.NoError(t, err)

	require.InDelta(t, baseCost.InputCost*0.5, flexCost.InputCost, 1e-10)
	require.InDelta(t, baseCost.OutputCost*0.5, flexCost.OutputCost, 1e-10)
	require.InDelta(t, baseCost.CacheCreationCost*0.5, flexCost.CacheCreationCost, 1e-10)
	require.InDelta(t, baseCost.CacheReadCost*0.5, flexCost.CacheReadCost, 1e-10)
	require.InDelta(t, baseCost.TotalCost*0.5, flexCost.TotalCost, 1e-10)
}

func TestCalculateCostWithServiceTier_Gpt54MiniPriorityFallsBackToTierMultiplier(t *testing.T) {
	svc := newTestBillingService()
	tokens := UsageTokens{InputTokens: 120, OutputTokens: 30, CacheCreationTokens: 12, CacheReadTokens: 8}

	baseCost, err := svc.CalculateCost("gpt-5.4-mini", tokens, 1.0)
	require.NoError(t, err)

	priorityCost, err := svc.CalculateCostWithServiceTier("gpt-5.4-mini", tokens, 1.0, "priority")
	require.NoError(t, err)

	require.InDelta(t, baseCost.InputCost*2, priorityCost.InputCost, 1e-10)
	require.InDelta(t, baseCost.OutputCost*2, priorityCost.OutputCost, 1e-10)
	require.InDelta(t, baseCost.CacheCreationCost*2, priorityCost.CacheCreationCost, 1e-10)
	require.InDelta(t, baseCost.CacheReadCost*2, priorityCost.CacheReadCost, 1e-10)
	require.InDelta(t, baseCost.TotalCost*2, priorityCost.TotalCost, 1e-10)
}

func TestCalculateCostWithServiceTier_Gpt54NanoFlexAppliesHalfMultiplier(t *testing.T) {
	svc := newTestBillingService()
	tokens := UsageTokens{InputTokens: 100, OutputTokens: 50, CacheCreationTokens: 40, CacheReadTokens: 20}

	baseCost, err := svc.CalculateCost("gpt-5.4-nano", tokens, 1.0)
	require.NoError(t, err)

	flexCost, err := svc.CalculateCostWithServiceTier("gpt-5.4-nano", tokens, 1.0, "flex")
	require.NoError(t, err)

	require.InDelta(t, baseCost.InputCost*0.5, flexCost.InputCost, 1e-10)
	require.InDelta(t, baseCost.OutputCost*0.5, flexCost.OutputCost, 1e-10)
	require.InDelta(t, baseCost.CacheCreationCost*0.5, flexCost.CacheCreationCost, 1e-10)
	require.InDelta(t, baseCost.CacheReadCost*0.5, flexCost.CacheReadCost, 1e-10)
	require.InDelta(t, baseCost.TotalCost*0.5, flexCost.TotalCost, 1e-10)
}

func TestCalculateCostWithServiceTier_PriorityFallsBackToTierMultiplierWithoutExplicitPriorityPrice(t *testing.T) {
	svc := newTestBillingService()
	tokens := UsageTokens{InputTokens: 120, OutputTokens: 30, CacheCreationTokens: 12, CacheReadTokens: 8}

	baseCost, err := svc.CalculateCost("claude-sonnet-4", tokens, 1.0)
	require.NoError(t, err)

	priorityCost, err := svc.CalculateCostWithServiceTier("claude-sonnet-4", tokens, 1.0, "priority")
	require.NoError(t, err)

	require.InDelta(t, baseCost.InputCost*2, priorityCost.InputCost, 1e-10)
	require.InDelta(t, baseCost.OutputCost*2, priorityCost.OutputCost, 1e-10)
	require.InDelta(t, baseCost.CacheCreationCost*2, priorityCost.CacheCreationCost, 1e-10)
	require.InDelta(t, baseCost.CacheReadCost*2, priorityCost.CacheReadCost, 1e-10)
	require.InDelta(t, baseCost.TotalCost*2, priorityCost.TotalCost, 1e-10)
}

func TestBillingServiceGetModelPricing_UsesDynamicPriorityFields(t *testing.T) {
	pricingSvc := &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"gpt-5.4": {
				InputCostPerToken:               2.5e-6,
				InputCostPerTokenPriority:       5e-6,
				OutputCostPerToken:              15e-6,
				OutputCostPerTokenPriority:      30e-6,
				CacheCreationInputTokenCost:     2.5e-6,
				CacheReadInputTokenCost:         0.25e-6,
				CacheReadInputTokenCostPriority: 0.5e-6,
				LongContextInputTokenThreshold:  272000,
				LongContextInputCostMultiplier:  2.0,
				LongContextOutputCostMultiplier: 1.5,
			},
		},
	}
	svc := NewBillingService(&config.Config{}, pricingSvc)

	pricing, err := svc.GetModelPricing("gpt-5.4")
	require.NoError(t, err)
	require.InDelta(t, 2.5e-6, pricing.InputPricePerToken, 1e-12)
	require.InDelta(t, 5e-6, pricing.InputPricePerTokenPriority, 1e-12)
	require.InDelta(t, 15e-6, pricing.OutputPricePerToken, 1e-12)
	require.InDelta(t, 30e-6, pricing.OutputPricePerTokenPriority, 1e-12)
	require.InDelta(t, 0.25e-6, pricing.CacheReadPricePerToken, 1e-12)
	require.InDelta(t, 0.5e-6, pricing.CacheReadPricePerTokenPriority, 1e-12)
	require.Equal(t, 272000, pricing.LongContextInputThreshold)
	require.InDelta(t, 2.0, pricing.LongContextInputMultiplier, 1e-12)
	require.InDelta(t, 1.5, pricing.LongContextOutputMultiplier, 1e-12)
}

func TestBillingServiceGetModelPricing_OpenAIFallbackGpt52Variants(t *testing.T) {
	svc := newTestBillingService()

	gpt52, err := svc.GetModelPricing("gpt-5.2")
	require.NoError(t, err)
	require.NotNil(t, gpt52)
	require.InDelta(t, 1.75e-6, gpt52.InputPricePerToken, 1e-12)
	require.InDelta(t, 3.5e-6, gpt52.InputPricePerTokenPriority, 1e-12)

	gpt52Codex, err := svc.GetModelPricing("gpt-5.2-codex")
	require.NoError(t, err)
	require.NotNil(t, gpt52Codex)
	require.InDelta(t, 1.75e-6, gpt52Codex.InputPricePerToken, 1e-12)
	require.InDelta(t, 3.5e-6, gpt52Codex.InputPricePerTokenPriority, 1e-12)
	require.InDelta(t, 28e-6, gpt52Codex.OutputPricePerTokenPriority, 1e-12)
}

func TestCalculateCostWithServiceTier_PriorityFallsBackToTierMultiplierWhenExplicitPriceMissing(t *testing.T) {
	svc := NewBillingService(&config.Config{}, &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"custom-no-priority": {
				InputCostPerToken:           1e-6,
				OutputCostPerToken:          2e-6,
				CacheCreationInputTokenCost: 0.5e-6,
				CacheReadInputTokenCost:     0.25e-6,
			},
		},
	})
	tokens := UsageTokens{InputTokens: 100, OutputTokens: 50, CacheCreationTokens: 40, CacheReadTokens: 20}

	baseCost, err := svc.CalculateCost("custom-no-priority", tokens, 1.0)
	require.NoError(t, err)

	priorityCost, err := svc.CalculateCostWithServiceTier("custom-no-priority", tokens, 1.0, "priority")
	require.NoError(t, err)

	require.InDelta(t, baseCost.InputCost*2, priorityCost.InputCost, 1e-10)
	require.InDelta(t, baseCost.OutputCost*2, priorityCost.OutputCost, 1e-10)
	require.InDelta(t, baseCost.CacheCreationCost*2, priorityCost.CacheCreationCost, 1e-10)
	require.InDelta(t, baseCost.CacheReadCost*2, priorityCost.CacheReadCost, 1e-10)
	require.InDelta(t, baseCost.TotalCost*2, priorityCost.TotalCost, 1e-10)
}

func TestGetModelPricing_OpenAIGpt52FallbacksExposePriorityPrices(t *testing.T) {
	svc := newTestBillingService()

	gpt52, err := svc.GetModelPricing("gpt-5.2")
	require.NoError(t, err)
	require.InDelta(t, 1.75e-6, gpt52.InputPricePerToken, 1e-12)
	require.InDelta(t, 3.5e-6, gpt52.InputPricePerTokenPriority, 1e-12)
	require.InDelta(t, 14e-6, gpt52.OutputPricePerToken, 1e-12)
	require.InDelta(t, 28e-6, gpt52.OutputPricePerTokenPriority, 1e-12)

	gpt52Codex, err := svc.GetModelPricing("gpt-5.2-codex")
	require.NoError(t, err)
	require.InDelta(t, 1.75e-6, gpt52Codex.InputPricePerToken, 1e-12)
	require.InDelta(t, 3.5e-6, gpt52Codex.InputPricePerTokenPriority, 1e-12)
	require.InDelta(t, 14e-6, gpt52Codex.OutputPricePerToken, 1e-12)
	require.InDelta(t, 28e-6, gpt52Codex.OutputPricePerTokenPriority, 1e-12)
}

func TestGetModelPricing_MapsDynamicPriorityFieldsIntoBillingPricing(t *testing.T) {
	svc := NewBillingService(&config.Config{}, &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"dynamic-tier-model": {
				InputCostPerToken:                   1e-6,
				InputCostPerTokenPriority:           2e-6,
				OutputCostPerToken:                  3e-6,
				OutputCostPerTokenPriority:          6e-6,
				CacheCreationInputTokenCost:         4e-6,
				CacheCreationInputTokenCostAbove1hr: 5e-6,
				CacheReadInputTokenCost:             7e-7,
				CacheReadInputTokenCostPriority:     8e-7,
				LongContextInputTokenThreshold:      999,
				LongContextInputCostMultiplier:      1.5,
				LongContextOutputCostMultiplier:     1.25,
			},
		},
	})

	pricing, err := svc.GetModelPricing("dynamic-tier-model")
	require.NoError(t, err)
	require.InDelta(t, 1e-6, pricing.InputPricePerToken, 1e-12)
	require.InDelta(t, 2e-6, pricing.InputPricePerTokenPriority, 1e-12)
	require.InDelta(t, 3e-6, pricing.OutputPricePerToken, 1e-12)
	require.InDelta(t, 6e-6, pricing.OutputPricePerTokenPriority, 1e-12)
	require.InDelta(t, 4e-6, pricing.CacheCreation5mPrice, 1e-12)
	require.InDelta(t, 5e-6, pricing.CacheCreation1hPrice, 1e-12)
	require.True(t, pricing.SupportsCacheBreakdown)
	require.InDelta(t, 7e-7, pricing.CacheReadPricePerToken, 1e-12)
	require.InDelta(t, 8e-7, pricing.CacheReadPricePerTokenPriority, 1e-12)
	require.Equal(t, 999, pricing.LongContextInputThreshold)
	require.InDelta(t, 1.5, pricing.LongContextInputMultiplier, 1e-12)
	require.InDelta(t, 1.25, pricing.LongContextOutputMultiplier, 1e-12)
}

// ---------------------------------------------------------------------------
// GetModelPricingWithChannel
// ---------------------------------------------------------------------------

func TestGetModelPricingWithChannel_NilChannelPricing_ReturnsOriginal(t *testing.T) {
	svc := newTestBillingService()

	pricing, err := svc.GetModelPricingWithChannel("claude-sonnet-4", nil)
	require.NoError(t, err)
	require.NotNil(t, pricing)

	// Should be identical to GetModelPricing
	original, err := svc.GetModelPricing("claude-sonnet-4")
	require.NoError(t, err)
	require.InDelta(t, original.InputPricePerToken, pricing.InputPricePerToken, 1e-12)
	require.InDelta(t, original.OutputPricePerToken, pricing.OutputPricePerToken, 1e-12)
	require.InDelta(t, original.CacheCreationPricePerToken, pricing.CacheCreationPricePerToken, 1e-12)
	require.InDelta(t, original.CacheReadPricePerToken, pricing.CacheReadPricePerToken, 1e-12)
}

func TestGetModelPricingWithChannel_OverrideInputPriceOnly(t *testing.T) {
	svc := newTestBillingService()

	chPricing := &ChannelModelPricing{
		InputPrice: testPtrFloat64(99e-6),
	}
	pricing, err := svc.GetModelPricingWithChannel("claude-sonnet-4", chPricing)
	require.NoError(t, err)

	// InputPrice overridden (both normal and priority)
	require.InDelta(t, 99e-6, pricing.InputPricePerToken, 1e-12)
	require.InDelta(t, 99e-6, pricing.InputPricePerTokenPriority, 1e-12)

	// OutputPrice unchanged (claude-sonnet-4 fallback = 15e-6)
	require.InDelta(t, 15e-6, pricing.OutputPricePerToken, 1e-12)
}

func TestGetModelPricingWithChannel_OverrideOutputPriceOnly(t *testing.T) {
	svc := newTestBillingService()

	chPricing := &ChannelModelPricing{
		OutputPrice: testPtrFloat64(88e-6),
	}
	pricing, err := svc.GetModelPricingWithChannel("claude-sonnet-4", chPricing)
	require.NoError(t, err)

	// OutputPrice overridden
	require.InDelta(t, 88e-6, pricing.OutputPricePerToken, 1e-12)
	require.InDelta(t, 88e-6, pricing.OutputPricePerTokenPriority, 1e-12)

	// InputPrice unchanged (claude-sonnet-4 fallback = 3e-6)
	require.InDelta(t, 3e-6, pricing.InputPricePerToken, 1e-12)
}

func TestGetModelPricingWithChannel_OverrideAllFields(t *testing.T) {
	svc := newTestBillingService()

	chPricing := &ChannelModelPricing{
		InputPrice:       testPtrFloat64(10e-6),
		OutputPrice:      testPtrFloat64(20e-6),
		CacheWritePrice:  testPtrFloat64(5e-6),
		CacheReadPrice:   testPtrFloat64(1e-6),
		ImageOutputPrice: testPtrFloat64(50e-6),
	}
	pricing, err := svc.GetModelPricingWithChannel("claude-sonnet-4", chPricing)
	require.NoError(t, err)

	require.InDelta(t, 10e-6, pricing.InputPricePerToken, 1e-12)
	require.InDelta(t, 10e-6, pricing.InputPricePerTokenPriority, 1e-12)
	require.InDelta(t, 20e-6, pricing.OutputPricePerToken, 1e-12)
	require.InDelta(t, 20e-6, pricing.OutputPricePerTokenPriority, 1e-12)
	require.InDelta(t, 5e-6, pricing.CacheCreationPricePerToken, 1e-12)
	require.InDelta(t, 5e-6, pricing.CacheCreation5mPrice, 1e-12)
	require.InDelta(t, 5e-6, pricing.CacheCreation1hPrice, 1e-12)
	require.InDelta(t, 1e-6, pricing.CacheReadPricePerToken, 1e-12)
	require.InDelta(t, 1e-6, pricing.CacheReadPricePerTokenPriority, 1e-12)
	require.InDelta(t, 50e-6, pricing.ImageOutputPricePerToken, 1e-12)
}

func TestGetModelPricingWithChannel_CacheWritePriceAffects5mAnd1h(t *testing.T) {
	svc := newTestBillingService()

	chPricing := &ChannelModelPricing{
		CacheWritePrice: testPtrFloat64(7e-6),
	}
	pricing, err := svc.GetModelPricingWithChannel("claude-sonnet-4", chPricing)
	require.NoError(t, err)

	// CacheWritePrice should set all three: CacheCreationPricePerToken, 5m, and 1h
	require.InDelta(t, 7e-6, pricing.CacheCreationPricePerToken, 1e-12)
	require.InDelta(t, 7e-6, pricing.CacheCreation5mPrice, 1e-12)
	require.InDelta(t, 7e-6, pricing.CacheCreation1hPrice, 1e-12)
}

func TestGetModelPricingWithChannel_CacheReadPriceAffectsPriority(t *testing.T) {
	svc := newTestBillingService()

	chPricing := &ChannelModelPricing{
		CacheReadPrice: testPtrFloat64(2e-6),
	}
	pricing, err := svc.GetModelPricingWithChannel("claude-sonnet-4", chPricing)
	require.NoError(t, err)

	// CacheReadPrice should set both normal and priority
	require.InDelta(t, 2e-6, pricing.CacheReadPricePerToken, 1e-12)
	require.InDelta(t, 2e-6, pricing.CacheReadPricePerTokenPriority, 1e-12)
}

func TestGetModelPricingWithChannel_UnknownModelReturnsError(t *testing.T) {
	svc := newTestBillingService()

	chPricing := &ChannelModelPricing{
		InputPrice: testPtrFloat64(1e-6),
	}
	pricing, err := svc.GetModelPricingWithChannel("totally-unknown-model", chPricing)
	require.Error(t, err)
	require.Nil(t, pricing)
	require.Contains(t, err.Error(), "pricing not found")
}
