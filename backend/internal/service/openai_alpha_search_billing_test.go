//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCalculateOpenAIRecordUsageCostAlphaSearchPerCall(t *testing.T) {
	t.Parallel()
	svc := &OpenAIGatewayService{billingService: &BillingService{}}
	groupID := int64(11)
	apiKey := &APIKey{ID: 1, GroupID: &groupID, Group: &Group{ID: groupID, Platform: PlatformOpenAI}}
	result := &OpenAIForwardResult{Model: "gpt-5.6-sol", UpstreamModel: "gpt-5.6-sol", WebSearchCalls: 1}

	// Official default is $10/1k calls = $0.01 per search. Per-call search
	// pricing uses the base multiplier (the webSearchMultiplier argument), not
	// the peak-adjusted token multiplier.
	cost, err := svc.calculateOpenAIRecordUsageCost(
		context.Background(), result, apiKey, []string{"gpt-5.6-sol"},
		3.0, 1.0, 1.0, 2.0, UsageTokens{}, "", boolPtr(false), time.Time{},
	)
	require.NoError(t, err)
	require.Equal(t, string(BillingModePerRequest), cost.BillingMode)
	require.InDelta(t, 0.01, cost.TotalCost, 1e-12)
	require.InDelta(t, 0.02, cost.ActualCost, 1e-12)

	pricePer1k := 5.0
	apiKey.Group.SearchPricePer1k = &pricePer1k
	cost, err = svc.calculateOpenAIRecordUsageCost(
		context.Background(), result, apiKey, []string{"gpt-5.6-sol"},
		1.0, 1.0, 1.0, 1.0, UsageTokens{}, "", boolPtr(false), time.Time{},
	)
	require.NoError(t, err)
	require.InDelta(t, 0.005, cost.TotalCost, 1e-12)
	require.InDelta(t, 0.005, cost.ActualCost, 1e-12)
}

func TestCalculateOpenAIRecordUsageCostAlphaSearchConfiguredPrice(t *testing.T) {
	t.Parallel()
	perCall, free, legacy := 0.025, 0.0, 5.0
	for _, tc := range []struct {
		name string
		key  *APIKey
		want float64
	}{
		{"nil key", nil, 0.02},
		{"nil group", &APIKey{}, 0.02},
		{"per-call setting", &APIKey{Group: &Group{WebSearchPricePerCall: &perCall}}, 0.05},
		{"per-call wins over legacy", &APIKey{Group: &Group{WebSearchPricePerCall: &perCall, SearchPricePer1k: &legacy}}, 0.05},
		{"explicit free wins over legacy", &APIKey{Group: &Group{WebSearchPricePerCall: &free, SearchPricePer1k: &legacy}}, 0},
		{"legacy fallback", &APIKey{Group: &Group{SearchPricePer1k: &legacy}}, 0.01},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &OpenAIGatewayService{billingService: &BillingService{}}
			cost, err := svc.calculateOpenAIRecordUsageCost(context.Background(), &OpenAIForwardResult{WebSearchCalls: 2}, tc.key, nil,
				3, 1, 1, 2, UsageTokens{}, "", nil, time.Time{})
			require.NoError(t, err)
			require.InDelta(t, tc.want, cost.TotalCost, 1e-12)
			require.InDelta(t, tc.want*2, cost.ActualCost, 1e-12)
		})
	}
}
