//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type oauth429RateLimitRepo struct {
	AccountRepository
	setRateLimitedCalls  int
	lastRateLimitedUntil time.Time
}

func (r *oauth429RateLimitRepo) SetRateLimited(_ context.Context, _ int64, until time.Time) error {
	r.setRateLimitedCalls++
	r.lastRateLimitedUntil = until
	return nil
}

func TestOpenAI429FastPath_KeepsOAuthAccountSchedulableDuringRetryWindow(t *testing.T) {
	repo := &oauth429RateLimitRepo{}
	rateLimits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{rateLimitService: rateLimits}
	rateLimits.SetAccountRuntimeBlocker(svc)
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKeyAccount := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	setupTokenAccount := &Account{ID: 44, Platform: PlatformOpenAI, Type: AccountTypeSetupToken}
	grokOAuthAccount := &Account{ID: 45, Platform: PlatformGrok, Type: AccountTypeOAuth}

	shouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusTooManyRequests, http.Header{}, nil)
	apiKeyShouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), apiKeyAccount, http.StatusTooManyRequests, http.Header{}, nil)

	require.False(t, shouldDisable)
	require.False(t, apiKeyShouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(apiKeyAccount), "API-key 429 keeps the existing scheduler cooldown behavior")
	require.Equal(t, 1, repo.setRateLimitedCalls, "only the API-key 429 should persist a scheduler block")
	require.True(t, svc.shouldRetryOpenAIOAuth429OnSameAccount(account, http.StatusTooManyRequests, false))
	require.True(t, svc.shouldRetryOpenAIOAuth429OnSameAccount(setupTokenAccount, http.StatusTooManyRequests, false))
	require.False(t, svc.shouldRetryOpenAIOAuth429OnSameAccount(apiKeyAccount, http.StatusTooManyRequests, false))
	require.False(t, svc.shouldRetryOpenAIOAuth429OnSameAccount(grokOAuthAccount, http.StatusTooManyRequests, false))
	require.WithinDuration(t, time.Now().Add(openAIOAuth429RetryWindow), svc.openAIOAuth429RetryDeadline(account), time.Second)
	require.WithinDuration(t, time.Now().Add(openAIOAuth429RetryWindow), svc.openAIOAuth429RetryDeadline(setupTokenAccount), time.Second)
}

func TestOpenAI429FastPath_BlocksOAuthOnlyAfterRetryWindow(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 420, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc.openaiOAuth429RetryStartedAt.Store(account.ID, time.Now().Add(-openAIOAuth429RetryWindow-time.Second))

	svc.markOpenAIOAuth429RateLimited(context.Background(), account, http.Header{}, nil)

	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.False(t, svc.shouldRetryOpenAIOAuth429OnSameAccount(account, http.StatusTooManyRequests, false))
}

func TestOpenAIStream429IgnoresSuccessfulQuotaSnapshotHeaders(t *testing.T) {
	repo := &oauth429RateLimitRepo{}
	rateLimits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{rateLimitService: rateLimits}
	rateLimits.SetAccountRuntimeBlocker(svc)
	account := &Account{ID: 421, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc.openaiOAuth429RetryStartedAt.Store(account.ID, time.Now().Add(-openAIOAuth429RetryWindow-time.Second))
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "37")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")
	payload := []byte(`{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`)

	status, disabled := svc.handleOpenAIStreamTerminalAccountSideEffects(nil, account, payload, "slow down", headers)

	require.Equal(t, http.StatusTooManyRequests, status)
	require.False(t, disabled)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok)
	blockedUntil, ok := value.(time.Time)
	require.True(t, ok)
	remaining := time.Until(blockedUntil)
	require.Greater(t, remaining, 4*time.Minute)
	require.Less(t, remaining, 6*time.Minute, "stream 429 must use the protected short cooldown, not the normal seven-day quota snapshot")
	if !repo.lastRateLimitedUntil.IsZero() {
		repoRemaining := time.Until(repo.lastRateLimitedUntil)
		require.Greater(t, repoRemaining, 4*time.Minute)
		require.Less(t, repoRemaining, 6*time.Minute)
	}
}

func TestOpenAIHTTP429StillUsesQuotaResetHeaders(t *testing.T) {
	svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{}}
	account := &Account{ID: 422, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc.openaiOAuth429RetryStartedAt.Store(account.ID, time.Now().Add(-openAIOAuth429RetryWindow-time.Second))
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")

	svc.markOpenAIOAuth429RateLimited(context.Background(), account, headers, nil)

	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok)
	blockedUntil, ok := value.(time.Time)
	require.True(t, ok)
	require.Greater(t, time.Until(blockedUntil), 6*24*time.Hour, "real HTTP 429 must retain the upstream quota reset")
}

func TestOpenAIHTTP429BurstUsesProtectedShortCooldown(t *testing.T) {
	svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{}}
	account := &Account{ID: 423, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc.openaiOAuth429RetryStartedAt.Store(account.ID, time.Now().Add(-openAIOAuth429RetryWindow-time.Second))
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "37")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")

	svc.markOpenAIOAuth429RateLimited(context.Background(), account, headers, nil)

	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok)
	blockedUntil, ok := value.(time.Time)
	require.True(t, ok)
	remaining := time.Until(blockedUntil)
	require.Greater(t, remaining, 4*time.Minute)
	require.Less(t, remaining, 6*time.Minute, "a non-exhausted HTTP burst must not inherit the quota reset")
}

func TestOpenAI429RetryDelayHonorsBoundedRetryAfter(t *testing.T) {
	deadline := time.Now().Add(openAIOAuth429RetryWindow)
	require.Equal(t, openAIOAuth429RetryDelay, openAIOAuth429SameAccountRetryDelay(nil, deadline))
	require.Equal(t, openAIOAuth429MaxRetryDelay, openAIOAuth429SameAccountRetryDelay(http.Header{"Retry-After": []string{"90"}}, deadline))
}

func TestOpenAISensitiveBackend400_UsesShortRuntimeCooldown(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 48, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	body := []byte(`{"error":{"message":"The 'gpt-5.3-codex' model is not supported when using Codex with a ChatGPT account."}}`)

	shouldDisable := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusBadRequest, http.Header{}, body)

	require.True(t, shouldDisable)
	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok)
	actualUntil, ok := value.(time.Time)
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(openAISensitiveBackendFallbackCooldown), actualUntil, time.Second)
	require.True(t, actualUntil.Before(time.Now().Add(openAIStopSchedulingBridgeCooldown/2)))
}

func TestOpenAIRuntimeBlock_AppliesToOpenAIAPIKeyWhenRateLimitServiceStopsScheduling(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 44, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	svc.BlockAccountScheduling(account, time.Time{}, "custom_error_code")

	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIRuntimeBlock_DoesNotApplyToOtherPlatforms(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 45, Platform: PlatformGemini, Type: AccountTypeOAuth}

	svc.BlockAccountScheduling(account, time.Time{}, "custom_error_code")

	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIRuntimeBlocker_IgnoresNonOpenAIFromRateLimitService(t *testing.T) {
	gateway := &OpenAIGatewayService{}
	repo := &rateLimitAccountRepoStub{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	rateLimitService.SetAccountRuntimeBlocker(gateway)
	account := &Account{ID: 45, Platform: PlatformGemini, Type: AccountTypeOAuth}

	shouldDisable := rateLimitService.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{}, []byte("forbidden"))

	require.True(t, shouldDisable)
	require.False(t, gateway.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIModelNotFound_DoesNotRuntimeBlockWholeAccount(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := openAIModelNotFoundTempAccount()

	shouldDisable := svc.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"code":"model_not_found","message":"model not found"}}`),
		"gpt-5.4",
	)

	require.True(t, shouldDisable)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Zero(t, repo.tempCalls)
	require.Len(t, repo.modelRateLimitCalls, 1)
}

func TestOpenAIRuntimeBlock_DoesNotShortenExistingBlock(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 46, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	longUntil := time.Now().Add(10 * time.Minute)

	svc.BlockAccountScheduling(account, longUntil, "oauth_401")
	svc.BlockAccountScheduling(account, time.Time{}, "upstream_disable")

	value, ok := svc.openaiAccountRuntimeBlockUntil.Load(account.ID)
	require.True(t, ok)
	actualUntil, ok := value.(time.Time)
	require.True(t, ok)
	require.WithinDuration(t, longUntil, actualUntil, time.Second)
}

func TestOpenAIRuntimeBlock_ClearAccountSchedulingBlock(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 47, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	svc.BlockAccountScheduling(account, time.Now().Add(time.Minute), "429")
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))

	svc.ClearAccountSchedulingBlock(account.ID)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestShouldStopOpenAIOAuth429Failover_AfterBoundedFullWindows(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKeyAccount := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	var state OpenAIOAuth429FailoverState

	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 1, &state))

	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 1, &state))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 2, &state))
	require.True(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 3, &state))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(apiKeyAccount, http.StatusTooManyRequests, 1, &state))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusInternalServerError, 1, &state))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 0, &state))
}

func TestShouldStopOpenAIOAuth429Failover_TracksOneGrokFollowupAttempt(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 44, Platform: PlatformGrok, Type: AccountTypeOAuth}
	apiKeyAccount := &Account{ID: 45, Platform: PlatformGrok, Type: AccountTypeAPIKey}

	t.Run("429 then 500 stops after one followup", func(t *testing.T) {
		var state OpenAIOAuth429FailoverState
		require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 1, &state))
		require.True(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusInternalServerError, 2, &state))
	})

	t.Run("500 then 429 still allows one followup", func(t *testing.T) {
		var state OpenAIOAuth429FailoverState
		require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusInternalServerError, 1, &state))
		require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 2, &state))
		require.True(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusBadGateway, 3, &state))
	})

	t.Run("OAuth 429 then API-key failure consumes the same followup", func(t *testing.T) {
		var state OpenAIOAuth429FailoverState
		require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 1, &state))
		require.True(t, svc.ShouldStopOpenAIOAuth429Failover(apiKeyAccount, http.StatusInternalServerError, 2, &state))
	})

	var state OpenAIOAuth429FailoverState
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(account, http.StatusTooManyRequests, 0, &state))
	require.False(t, svc.ShouldStopOpenAIOAuth429Failover(apiKeyAccount, http.StatusTooManyRequests, 2, &state))
}

// TestOpenAI429FastPath_SkipsSparkShadow 外审第8轮 P1:spark 影子被选中后若 /responses 返回 429,
// 不得按 global x-codex-* 信号写内存运行时熔断(否则 spark 被冷却到 global reset、单影子场景无可用账号)。
func TestOpenAI429FastPath_SkipsSparkShadow(t *testing.T) {
	svc := &OpenAIGatewayService{}
	parentID := int64(800)
	shadow := &Account{
		ID:              801,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
	}
	normal := &Account{ID: 802, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "18000")
	headers.Set("x-codex-primary-window-minutes", "300")

	svc.markOpenAIOAuth429RateLimited(context.Background(), shadow, headers, nil)
	svc.markOpenAIOAuth429RateLimited(context.Background(), normal, headers, nil)

	require.False(t, svc.isOpenAIAccountRuntimeBlocked(shadow), "spark shadow must not be runtime-blocked by /responses global 429")
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(normal), "normal OpenAI OAuth account stays schedulable during its retry window")
}
