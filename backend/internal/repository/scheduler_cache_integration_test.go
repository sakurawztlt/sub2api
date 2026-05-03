//go:build integration

package repository

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSchedulerCacheSnapshotUsesSlimMetadataButKeepsFullAccount(t *testing.T) {
	ctx := context.Background()
	rdb := testRedis(t)
	cache := NewSchedulerCache(rdb)

	bucket := service.SchedulerBucket{GroupID: 2, Platform: service.PlatformGemini, Mode: service.SchedulerModeSingle}
	now := time.Now().UTC().Truncate(time.Second)
	limitReset := now.Add(10 * time.Minute)
	overloadUntil := now.Add(2 * time.Minute)
	tempUnschedUntil := now.Add(3 * time.Minute)
	windowEnd := now.Add(5 * time.Hour)

	account := service.Account{
		ID:          101,
		Name:        "gemini-heavy",
		Platform:    service.PlatformGemini,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 3,
		Priority:    7,
		LastUsedAt:  &now,
		Credentials: map[string]any{
			"api_key":       "gemini-api-key",
			"access_token":  "secret-access-token",
			"project_id":    "proj-1",
			"oauth_type":    "ai_studio",
			"model_mapping": map[string]any{"gemini-2.5-pro": "gemini-2.5-pro"},
			"huge_blob":     strings.Repeat("x", 4096),
		},
		Extra: map[string]any{
			"mixed_scheduling":             true,
			"window_cost_limit":            12.5,
			"window_cost_sticky_reserve":   8.0,
			"max_sessions":                 4,
			"session_idle_timeout_minutes": 11,
			"unused_large_field":           strings.Repeat("y", 4096),
		},
		RateLimitResetAt:       &limitReset,
		OverloadUntil:          &overloadUntil,
		TempUnschedulableUntil: &tempUnschedUntil,
		SessionWindowStart:     &now,
		SessionWindowEnd:       &windowEnd,
		SessionWindowStatus:    "active",
		GroupIDs:               []int64{bucket.GroupID},
		AccountGroups: []service.AccountGroup{
			{
				AccountID: 101,
				GroupID:   bucket.GroupID,
				Priority:  5,
				Group:     &service.Group{ID: bucket.GroupID, Name: "gemini-group"},
			},
		},
	}

	require.NoError(t, cache.SetSnapshot(ctx, bucket, []service.Account{account}))

	snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Len(t, snapshot, 1)

	got := snapshot[0]
	require.NotNil(t, got)
	require.Equal(t, "gemini-api-key", got.GetCredential("api_key"))
	require.Equal(t, "proj-1", got.GetCredential("project_id"))
	require.Equal(t, "ai_studio", got.GetCredential("oauth_type"))
	require.NotEmpty(t, got.GetModelMapping())
	require.Empty(t, got.GetCredential("access_token"))
	require.Empty(t, got.GetCredential("huge_blob"))
	require.Equal(t, true, got.Extra["mixed_scheduling"])
	require.Equal(t, 12.5, got.GetWindowCostLimit())
	require.Equal(t, 8.0, got.GetWindowCostStickyReserve())
	require.Equal(t, 4, got.GetMaxSessions())
	require.Equal(t, 11, got.GetSessionIdleTimeoutMinutes())
	require.Nil(t, got.Extra["unused_large_field"])
	require.Equal(t, []int64{bucket.GroupID}, got.GroupIDs)
	require.Len(t, got.AccountGroups, 1)
	require.Equal(t, account.ID, got.AccountGroups[0].AccountID)
	require.Equal(t, bucket.GroupID, got.AccountGroups[0].GroupID)
	require.Nil(t, got.AccountGroups[0].Group)

	full, err := cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.NotNil(t, full)
	require.Equal(t, "secret-access-token", full.GetCredential("access_token"))
	require.Equal(t, strings.Repeat("x", 4096), full.GetCredential("huge_blob"))
	require.Len(t, full.AccountGroups, 1)
	require.NotNil(t, full.AccountGroups[0].Group)
}

func TestSchedulerCacheUpdateLastUsedUsesSideKeyAndKeepsAccountJSON(t *testing.T) {
	ctx := context.Background()
	rdb := testRedis(t)
	cache := NewSchedulerCache(rdb)

	initialUsedAt := time.Now().UTC().Truncate(time.Second)
	account := &service.Account{
		ID:          202,
		Name:        "hot-field-account",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 2,
		LastUsedAt:  &initialUsedAt,
		Credentials: map[string]any{
			"api_key":   "k-1",
			"huge_blob": strings.Repeat("a", 2048),
		},
		Extra: map[string]any{
			"quota_limit": 100,
			"notes_blob":  strings.Repeat("b", 2048),
		},
	}
	require.NoError(t, cache.SetAccount(ctx, account))

	id := strconv.FormatInt(account.ID, 10)
	accountBefore, err := rdb.Get(ctx, schedulerAccountKey(id)).Result()
	require.NoError(t, err)
	metaBefore, err := rdb.Get(ctx, schedulerAccountMetaKey(id)).Result()
	require.NoError(t, err)

	latestUsedAt := initialUsedAt.Add(37 * time.Second)
	require.NoError(t, cache.UpdateLastUsed(ctx, map[int64]time.Time{
		account.ID: latestUsedAt,
	}))

	accountAfter, err := rdb.Get(ctx, schedulerAccountKey(id)).Result()
	require.NoError(t, err)
	metaAfter, err := rdb.Get(ctx, schedulerAccountMetaKey(id)).Result()
	require.NoError(t, err)
	require.Equal(t, accountBefore, accountAfter, "更新 LastUsedAt 不应重写完整账号 JSON")
	require.Equal(t, metaBefore, metaAfter, "更新 LastUsedAt 不应重写快照元数据 JSON")

	lastUsedRaw, err := rdb.Get(ctx, schedulerLastUsedKey(id)).Result()
	require.NoError(t, err)
	require.Equal(t, strconv.FormatInt(latestUsedAt.UTC().UnixNano(), 10), lastUsedRaw)

	cached, err := cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.NotNil(t, cached)
	require.NotNil(t, cached.LastUsedAt)
	require.WithinDuration(t, latestUsedAt.UTC(), *cached.LastUsedAt, time.Nanosecond)

	bucket := service.SchedulerBucket{GroupID: 9, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	require.NoError(t, cache.SetSnapshot(ctx, bucket, []service.Account{*account}))

	latestForSnapshot := latestUsedAt.Add(13 * time.Second)
	require.NoError(t, cache.UpdateLastUsed(ctx, map[int64]time.Time{
		account.ID: latestForSnapshot,
	}))

	snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Len(t, snapshot, 1)
	require.NotNil(t, snapshot[0].LastUsedAt)
	require.WithinDuration(t, latestForSnapshot.UTC(), *snapshot[0].LastUsedAt, time.Nanosecond)
}
