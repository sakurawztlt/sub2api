//go:build unit

package repository

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestApiKeyRateLimitKey(t *testing.T) {
	tests := []struct {
		name     string
		userID   int64
		expected string
	}{
		{
			name:     "normal_user_id",
			userID:   123,
			expected: "apikey:ratelimit:123",
		},
		{
			name:     "zero_user_id",
			userID:   0,
			expected: "apikey:ratelimit:0",
		},
		{
			name:     "negative_user_id",
			userID:   -1,
			expected: "apikey:ratelimit:-1",
		},
		{
			name:     "max_int64",
			userID:   math.MaxInt64,
			expected: "apikey:ratelimit:9223372036854775807",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := apiKeyRateLimitKey(tc.userID)
			require.Equal(t, tc.expected, got)
		})
	}
}

func TestAPIKeyCacheSubscribeInvalidationBlocksUntilContextCanceled(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	cache := &apiKeyCache{rdb: client}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	received := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- cache.SubscribeAuthCacheInvalidation(ctx, func(cacheKey string) {
			received <- cacheKey
		})
	}()

	const cacheKey = "cache-key"
	require.Eventually(t, func() bool {
		if err := client.Publish(ctx, authCacheInvalidateChannel, cacheKey).Err(); err != nil {
			return false
		}
		select {
		case got := <-received:
			return got == cacheKey
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	select {
	case err := <-done:
		t.Fatalf("subscriber returned before cancellation: %v", err)
	default:
	}

	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("subscriber did not stop after cancellation")
	}
}
