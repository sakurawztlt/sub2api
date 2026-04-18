package dto

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountFromServiceShallow_UsesRuntimeSchedulableWhenQuotaExceeded(t *testing.T) {
	account := &service.Account{
		ID:          1,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Extra: map[string]any{
			"quota_daily_limit": 10.0,
			"quota_daily_used":  10.0,
			"quota_daily_start": time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
		},
	}

	got := AccountFromServiceShallow(account)
	require.NotNil(t, got)
	require.False(t, got.Schedulable)
	require.NotNil(t, got.QuotaDailyUsed)
	require.Equal(t, 10.0, *got.QuotaDailyUsed)
}

func TestAccountFromServiceShallow_ExpiredQuotaPeriodRestoresSchedulableAndZeroesUsed(t *testing.T) {
	account := &service.Account{
		ID:          2,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Extra: map[string]any{
			"quota_weekly_limit": 10.0,
			"quota_weekly_used":  10.0,
			"quota_weekly_start": time.Now().Add(-8 * 24 * time.Hour).Format(time.RFC3339),
		},
	}

	got := AccountFromServiceShallow(account)
	require.NotNil(t, got)
	require.True(t, got.Schedulable)
	require.NotNil(t, got.QuotaWeeklyUsed)
	require.Equal(t, 0.0, *got.QuotaWeeklyUsed)
}
