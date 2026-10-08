package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type legacyRecoveryAccountRepo struct {
	stubOpenAIAccountRepo
	clearCalls        int
	recoveryListCalls int
}

func (r *legacyRecoveryAccountRepo) ListByPlatform(ctx context.Context, platform string) ([]Account, error) {
	r.recoveryListCalls++
	return r.stubOpenAIAccountRepo.ListByPlatform(ctx, platform)
}

func (r *legacyRecoveryAccountRepo) ClearRateLimit(_ context.Context, accountID int64) error {
	r.clearCalls++
	for i := range r.accounts {
		if r.accounts[i].ID == accountID {
			r.accounts[i].RateLimitedAt = nil
			r.accounts[i].RateLimitResetAt = nil
		}
	}
	return nil
}

func legacyRecoverableAccount(now time.Time) Account {
	limitedAt := now.Add(-time.Minute)
	resetAt := now.Add(10 * time.Minute)
	return Account{
		ID: 91233, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		RateLimitedAt: &limitedAt, RateLimitResetAt: &resetAt,
		Extra: map[string]any{
			"codex_5h_used_percent":  20.0,
			"codex_7d_used_percent":  30.0,
			"codex_usage_updated_at": limitedAt.Format(time.RFC3339Nano),
		},
	}
}

func TestLegacyOpenAISelectionRateLimitRecoveryGate(t *testing.T) {
	for _, mode := range []string{"direct", "no_concurrency", "batch_disabled"} {
		for _, tc := range []struct {
			name   string
			mutate func(*Account, time.Time)
			wantOK bool
		}{
			{name: "current_non_exhausted_snapshot", wantOK: true},
			{name: "missing_snapshot", mutate: func(a *Account, _ time.Time) { delete(a.Extra, "codex_usage_updated_at") }},
			{name: "missing_window", mutate: func(a *Account, _ time.Time) { delete(a.Extra, "codex_7d_used_percent") }},
			{name: "new_429_with_stale_prior_snapshot", mutate: func(a *Account, now time.Time) {
				a.RateLimitedAt = &now
				a.Extra["codex_usage_updated_at"] = now.Add(-openAICodexRecoverySnapshotMaxSkew - time.Second).Format(time.RFC3339Nano)
			}},
			{name: "exhausted_5h", mutate: func(a *Account, _ time.Time) { a.Extra["codex_5h_used_percent"] = 100.0 }},
			{name: "exhausted_7d", mutate: func(a *Account, _ time.Time) { a.Extra["codex_7d_used_percent"] = 100.0 }},
			{name: "active_temporary_cooldown", mutate: func(a *Account, now time.Time) {
				until := now.Add(time.Minute)
				a.TempUnschedulableUntil = &until
			}},
			{name: "active_overload_cooldown", mutate: func(a *Account, now time.Time) {
				until := now.Add(time.Minute)
				a.OverloadUntil = &until
			}},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				now := time.Now()
				account := legacyRecoverableAccount(now)
				if tc.mutate != nil {
					tc.mutate(&account, now)
				}
				repo := &legacyRecoveryAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}}
				cache := &stubGatewayCache{}
				svc := &OpenAIGatewayService{accountRepo: repo, cache: cache}
				if mode == "batch_disabled" {
					svc.cfg = &config.Config{}
					svc.concurrencyService = NewConcurrencyService(stubConcurrencyCache{})
				}
				var selected *Account
				var err error
				if mode == "direct" {
					selected, err = svc.SelectAccountForModel(context.Background(), nil, "recovery-session", "gpt-5.5")
				} else {
					var result *AccountSelectionResult
					result, err = svc.SelectAccountWithLoadAwareness(context.Background(), nil, "recovery-session", "gpt-5.5", nil)
					if result != nil {
						selected = result.Account
						if result.ReleaseFunc != nil {
							result.ReleaseFunc()
						}
					}
				}
				if !tc.wantOK {
					require.ErrorIs(t, err, ErrNoAvailableAccounts)
					require.Nil(t, selected)
					require.Zero(t, repo.clearCalls, "cooldown or untrusted/exhausted usage must not trigger an early retry")
					require.Empty(t, cache.sessionBindings)
					return
				}
				require.NoError(t, err)
				require.NotNil(t, selected)
				require.Equal(t, account.ID, selected.ID)
				require.Nil(t, selected.RateLimitResetAt)
				require.Equal(t, 1, repo.clearCalls)
				require.Equal(t, account.ID, cache.sessionBindings[svc.openAISessionCacheKey("recovery-session")])
			})
		}
	}
}

func TestLegacyOpenAIRecoveryDoesNotBypassModelTransientBlock(t *testing.T) {
	now := time.Now()
	account := legacyRecoverableAccount(now)
	repo := &legacyRecoveryAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}}
	cache := &stubGatewayCache{}
	svc := &OpenAIGatewayService{accountRepo: repo, cache: cache}
	transient := svc.getOpenAIAccountModelTransientState()
	transient.recordFailure(account.ID, "gpt-5.5", now)
	transient.recordFailure(account.ID, "gpt-5.5", now)

	selected, err := svc.SelectAccountForModel(context.Background(), nil, "blocked-session", "gpt-5.5")

	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Nil(t, selected)
	require.Empty(t, cache.sessionBindings, "a recovered account must still pass model admission before sticky binding")
}

func TestLegacyOpenAIRecoveryDefersStickyBindingUnderProfitGate(t *testing.T) {
	now := time.Now()
	account := legacyRecoverableAccount(now)
	groupID := int64(91233)
	account.GroupIDs = []int64{groupID}
	profitControlTestAccountWithRate(&account, 0.4)
	repo := &legacyRecoveryAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}}
	cache := &stubGatewayCache{}
	svc := &OpenAIGatewayService{accountRepo: repo, cache: cache}
	ctx := profitControlTestCtx(profitControlTestGroup(groupID, 0.5, 0))

	selected, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "profit-session", "gpt-5.5", nil)

	require.NoError(t, err)
	require.NotNil(t, selected)
	if selected.ReleaseFunc != nil {
		selected.ReleaseFunc()
	}
	require.Equal(t, 1, repo.clearCalls)
	require.Empty(t, cache.sessionBindings, "profit-gated binding waits for the terminal admission check")
}

func TestLegacyOpenAIRecoveryRespectsCompatibleImageBoundary(t *testing.T) {
	const model = "gemini-3.1-flash-image"
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken, AccountTypeAPIKey} {
		t.Run(accountType, func(t *testing.T) {
			account := legacyRecoverableAccount(time.Now())
			account.Type = accountType
			mappedModel := model
			if accountType == AccountTypeAPIKey {
				mappedModel = "gpt-image-2"
			}
			account.Credentials = map[string]any{"model_mapping": map[string]any{mappedModel: mappedModel}}
			repo := &legacyRecoveryAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}}
			svc := &OpenAIGatewayService{accountRepo: repo}
			ctx := WithOpenAIImagesEndpoint(WithOpenAIImageGenerationIntent(context.Background()))

			selection, _, err := svc.SelectAccountWithSchedulerForImages(ctx, nil, "", model, nil, OpenAIImagesCapabilityAPIKey)

			require.ErrorIs(t, err, ErrNoAvailableAccounts)
			require.Nil(t, selection)
			require.Zero(t, repo.recoveryListCalls, "Codex usage cannot infer compatible-provider image availability")
			require.Zero(t, repo.clearCalls)
			require.NotNil(t, repo.accounts[0].RateLimitResetAt, "an incompatible image request must preserve the account cooldown")
		})
	}
}

func TestLegacyOpenAIRecoveryStillSupportsNativeImages(t *testing.T) {
	const model = "gpt-image-2"
	account := legacyRecoverableAccount(time.Now())
	account.Credentials = map[string]any{"model_mapping": map[string]any{model: model}}
	repo := &legacyRecoveryAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}}
	svc := &OpenAIGatewayService{accountRepo: repo}
	ctx := WithOpenAIImagesEndpoint(WithOpenAIImageGenerationIntent(context.Background()))

	selection, _, err := svc.SelectAccountWithSchedulerForImages(ctx, nil, "", model, nil, OpenAIImagesCapabilityBasic)

	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, account.ID, selection.Account.ID)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	require.Equal(t, 1, repo.recoveryListCalls)
	require.Equal(t, 1, repo.clearCalls)
}
