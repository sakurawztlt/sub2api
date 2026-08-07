//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

type dailyMidnightResetRepo struct {
	userSubRepoNoop

	resetCalled    bool
	newWindowStart time.Time
}

func (r *dailyMidnightResetRepo) ResetDailyUsage(_ context.Context, _ int64, newWindowStart time.Time) error {
	r.resetCalled = true
	r.newWindowStart = newWindowStart
	return nil
}

func midnightTestBase() time.Time {
	return timezone.StartOfDay(time.Date(2026, 8, 6, 12, 0, 0, 0, timezone.Location()))
}

func newMidnightTestSub(dailyWindowStart, base time.Time) *UserSubscription {
	start := dailyWindowStart
	return &UserSubscription{
		ID:               1,
		UserID:           10,
		GroupID:          20,
		StartsAt:         base.AddDate(0, 0, -3),
		ExpiresAt:        base.AddDate(0, 0, 30),
		DailyUsageUSD:    43.34,
		DailyWindowStart: &start,
	}
}

func TestCheckAndResetWindows_DailyResetsAtMidnightNotRollingAnchor(t *testing.T) {
	base := midnightTestBase()
	manualResetAt := base.Add(16*time.Hour + 49*time.Minute)
	now := base.AddDate(0, 0, 1).Add(5 * time.Minute)

	repo := &dailyMidnightResetRepo{}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	svc.now = func() time.Time { return now }
	sub := newMidnightTestSub(manualResetAt, base)

	require.NoError(t, svc.CheckAndResetWindows(context.Background(), sub))
	require.True(t, repo.resetCalled)
	require.Equal(t, base.AddDate(0, 0, 1), repo.newWindowStart)
	require.Zero(t, sub.DailyUsageUSD)
	require.Equal(t, base.AddDate(0, 0, 1), *sub.DailyWindowStart)
}

func TestCheckAndResetWindows_DailyNoResetWithinSameCalendarDay(t *testing.T) {
	base := midnightTestBase()
	manualResetAt := base.Add(16*time.Hour + 49*time.Minute)
	now := base.Add(23*time.Hour + 59*time.Minute)

	repo := &dailyMidnightResetRepo{}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	svc.now = func() time.Time { return now }
	sub := newMidnightTestSub(manualResetAt, base)

	require.NoError(t, svc.CheckAndResetWindows(context.Background(), sub))
	require.False(t, repo.resetCalled)
	require.Equal(t, 43.34, sub.DailyUsageUSD)
}

func TestCheckAndResetWindows_LegacyRollingAnchorHealsToMidnight(t *testing.T) {
	base := midnightTestBase()
	staleAnchor := base.AddDate(0, 0, -3).Add(17*time.Hour + 18*time.Minute)
	now := base.Add(10 * time.Hour)

	repo := &dailyMidnightResetRepo{}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	svc.now = func() time.Time { return now }
	sub := newMidnightTestSub(staleAnchor, base)

	require.NoError(t, svc.CheckAndResetWindows(context.Background(), sub))
	require.True(t, repo.resetCalled)
	require.Equal(t, base, repo.newWindowStart)
}

func TestNeedsDailyReset_MidnightScheduleSurvivesManualReset(t *testing.T) {
	base := midnightTestBase()
	sub := newMidnightTestSub(base, base)

	require.False(t, sub.NeedsDailyResetAt(base.Add(23*time.Hour+54*time.Minute)))
	require.True(t, sub.NeedsDailyResetAt(base.AddDate(0, 0, 1).Add(time.Minute)))
}

func TestDailyResetTime_NextMidnightForMultiDaySubscription(t *testing.T) {
	base := midnightTestBase()

	sub := newMidnightTestSub(base, base)
	require.Equal(t, base.AddDate(0, 0, 1), *sub.DailyResetTime())

	rolling := newMidnightTestSub(base.Add(16*time.Hour+49*time.Minute), base)
	require.Equal(t, base.AddDate(0, 0, 1), *rolling.DailyResetTime())
}

func TestNormalizeExpiredWindows_DailyUsageClearsAfterMidnight(t *testing.T) {
	base := midnightTestBase()
	manualResetAt := base.Add(16*time.Hour + 49*time.Minute)
	now := base.AddDate(0, 0, 1).Add(time.Minute)

	subs := []UserSubscription{*newMidnightTestSub(manualResetAt, base)}
	normalizeExpiredWindowsAt(subs, now)

	require.Zero(t, subs[0].DailyUsageUSD)
	require.Nil(t, subs[0].DailyWindowStart)
}

func TestCheckAndResetWindows_OneTimeDailyCardStillExemptFromMidnightReset(t *testing.T) {
	base := midnightTestBase()
	startsAt := base.Add(17 * time.Hour)
	anchor := base
	now := base.AddDate(0, 0, 1).Add(2 * time.Hour)

	repo := &dailyMidnightResetRepo{}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	svc.now = func() time.Time { return now }
	sub := &UserSubscription{
		ID:               1,
		UserID:           10,
		GroupID:          20,
		StartsAt:         startsAt,
		ExpiresAt:        startsAt.AddDate(0, 0, 1),
		DailyUsageUSD:    10,
		DailyWindowStart: &anchor,
	}

	require.NoError(t, svc.CheckAndResetWindows(context.Background(), sub))
	require.False(t, repo.resetCalled)
	require.Equal(t, 10.0, sub.DailyUsageUSD)
	require.Equal(t, sub.ExpiresAt, *sub.DailyResetTime())
}
