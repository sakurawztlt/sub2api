package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestBuildKeyBillingInfoAppliesPeakMultiplierWithoutSensitiveFields(t *testing.T) {
	groupID := int64(7)
	apiKey := &service.APIKey{
		Key:     "sk-sensitive-value",
		GroupID: &groupID,
		Group: &service.Group{
			ID:                 groupID,
			Name:               "private-group-name",
			RateMultiplier:     1.2,
			SubscriptionType:   service.SubscriptionTypeSubscription,
			PeakRateEnabled:    true,
			PeakStart:          "09:00",
			PeakEnd:            "18:00",
			PeakRateMultiplier: 1.5,
		},
	}
	now := time.Date(2026, time.July, 12, 10, 0, 0, 0, timezone.Location())

	got := buildKeyBillingInfo(apiKey, 0.8, now)

	require.Equal(t, "sub2api.key_billing", got.Object)
	require.Equal(t, 1, got.SchemaVersion)
	require.InDelta(t, 1.2, got.EffectiveRateMultiplier, 1e-12)
	require.Equal(t, now.UTC(), got.ObservedAt)
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), apiKey.Key)
	require.NotContains(t, string(encoded), apiKey.Group.Name)
}
