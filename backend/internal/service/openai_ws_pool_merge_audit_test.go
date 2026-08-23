package service

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func activeCodexFingerprintPoolAccountForMergeAudit(id int64) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			codexFingerprintModeExtraKey: "session",
			codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
		},
	}
}

func stableOpenAIWSIdentityHeadersForMergeAudit() http.Header {
	headers := make(http.Header)
	headers.Set("X-Codex-Beta-Features", "remote_compaction_v2,responses_websockets_v2")
	headers.Set("X-Codex-Installation-ID", "install-a")
	headers.Set("session-id", "session-hyphen-a")
	headers.Set("session_id", "session-underscore-a")
	headers.Set("thread-id", "thread-a")
	headers.Set("x-client-request-id", "client-request-a")
	headers.Set("x-codex-window-id", "window-a")
	return headers
}

func TestNormalizeOpenAIWSHandshakeCompatibility_MergeAudit(t *testing.T) {
	account := activeCodexFingerprintPoolAccountForMergeAudit(132)
	base := stableOpenAIWSIdentityHeadersForMergeAudit()
	baseKey := normalizeOpenAIWSHandshakeCompatibility(account, base)

	turnChanged := stableOpenAIWSIdentityHeadersForMergeAudit()
	turnChanged.Set("Authorization", "Bearer token-b")
	turnChanged.Set("x-codex-turn-metadata", `{"turn_id":"turn-b"}`)
	turnChanged.Set(openAICodexRoutingHintHeader, "model=gpt-5.6-codex;tier=priority")
	require.Equal(t, baseKey, normalizeOpenAIWSHandshakeCompatibility(account, turnChanged),
		"auth, turn metadata, and soft routing hints must not split a stable identity")

	reorderedBeta := stableOpenAIWSIdentityHeadersForMergeAudit()
	reorderedBeta.Del("X-Codex-Beta-Features")
	reorderedBeta["X-Codex-Beta-Features"] = []string{" responses_websockets_v2 ", " remote_compaction_v2 "}
	require.Equal(t, baseKey, normalizeOpenAIWSHandshakeCompatibility(account, reorderedBeta))

	for _, tc := range []struct {
		name   string
		header string
		value  string
	}{
		{name: "installation", header: "x-codex-installation-id", value: "install-b"},
		{name: "session hyphen", header: "session-id", value: "session-hyphen-b"},
		{name: "session underscore", header: "session_id", value: "session-underscore-b"},
		{name: "thread", header: "thread-id", value: "thread-b"},
		{name: "client request", header: "x-client-request-id", value: "client-request-b"},
		{name: "window", header: "x-codex-window-id", value: "window-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := stableOpenAIWSIdentityHeadersForMergeAudit()
			changed.Set(tc.header, tc.value)
			require.NotEqual(t, baseKey, normalizeOpenAIWSHandshakeCompatibility(account, changed))
		})
	}

	deviceAccount := activeCodexFingerprintPoolAccountForMergeAudit(133)
	deviceAccount.Extra[codexFingerprintModeExtraKey] = "device"
	deviceBase := normalizeOpenAIWSHandshakeCompatibility(deviceAccount, base)
	deviceSessionChanged := stableOpenAIWSIdentityHeadersForMergeAudit()
	deviceSessionChanged.Set("session-id", "session-hyphen-b")
	deviceSessionChanged.Set("session_id", "session-underscore-b")
	deviceSessionChanged.Set("thread-id", "thread-b")
	deviceSessionChanged.Set("x-client-request-id", "client-request-b")
	deviceSessionChanged.Set("x-codex-window-id", "window-b")
	require.Equal(t, deviceBase, normalizeOpenAIWSHandshakeCompatibility(deviceAccount, deviceSessionChanged),
		"device mode keys only the installation identity")
	deviceSessionChanged.Set("x-codex-installation-id", "install-b")
	require.NotEqual(t, deviceBase, normalizeOpenAIWSHandshakeCompatibility(deviceAccount, deviceSessionChanged))
}

func TestOpenAIWSConnPool_ReusesOnlyCompatibleHandshakeIdentity_MergeAudit(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 2
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 2

	t.Run("beta features", func(t *testing.T) {
		pool := newOpenAIWSConnPool(cfg)
		dialer := &openAIWSCountingDialer{}
		pool.setClientDialerForTest(dialer)
		account := &Account{ID: 128, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
		baseReq := openAIWSAcquireRequest{Account: account, WSURL: "wss://example.com/v1/responses"}

		plainLease, err := pool.Acquire(context.Background(), baseReq)
		require.NoError(t, err)
		plainConnID := plainLease.ConnID()
		plainLease.Release()

		betaReq := baseReq
		betaReq.Headers = http.Header{"X-Codex-Beta-Features": {" remote_compaction_v2 ", " responses_websockets_v2 "}}
		betaLease, err := pool.Acquire(context.Background(), betaReq)
		require.NoError(t, err)
		require.False(t, betaLease.Reused())
		require.NotEqual(t, plainConnID, betaLease.ConnID())
		betaConnID := betaLease.ConnID()
		betaLease.Release()

		reorderedReq := baseReq
		reorderedReq.Headers = http.Header{"X-Codex-Beta-Features": {"responses_websockets_v2,remote_compaction_v2"}}
		reorderedLease, err := pool.Acquire(context.Background(), reorderedReq)
		require.NoError(t, err)
		require.True(t, reorderedLease.Reused())
		require.Equal(t, betaConnID, reorderedLease.ConnID())
		reorderedLease.Release()

		_, err = pool.Acquire(context.Background(), openAIWSAcquireRequest{
			Account:            account,
			WSURL:              baseReq.WSURL,
			Headers:            betaReq.Headers,
			PreferredConnID:    plainConnID,
			ForcePreferredConn: true,
		})
		require.ErrorIs(t, err, errOpenAIWSPreferredConnUnavailable)
		require.Equal(t, 2, dialer.DialCount())
	})

	t.Run("stable fingerprint identity", func(t *testing.T) {
		identityCfg := &config.Config{}
		identityCfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
		identityCfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
		identityCfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
		pool := newOpenAIWSConnPool(identityCfg)
		dialer := &openAIWSCountingDialer{}
		pool.setClientDialerForTest(dialer)
		account := activeCodexFingerprintPoolAccountForMergeAudit(134)

		firstHeaders := stableOpenAIWSIdentityHeadersForMergeAudit()
		firstHeaders.Set("Authorization", "Bearer token-a")
		firstHeaders.Set("x-codex-turn-metadata", `{"turn_id":"turn-a"}`)
		first, err := pool.Acquire(context.Background(), openAIWSAcquireRequest{
			Account: account, WSURL: "wss://example.com/v1/responses", Headers: firstHeaders,
		})
		require.NoError(t, err)
		firstConnID := first.ConnID()
		first.Release()

		nextHeaders := stableOpenAIWSIdentityHeadersForMergeAudit()
		nextHeaders.Set("Authorization", "Bearer token-b")
		nextHeaders.Set("x-codex-turn-metadata", `{"turn_id":"turn-b"}`)
		nextHeaders.Set(openAICodexRoutingHintHeader, "model=gpt-5.6-codex;tier=priority")
		second, err := pool.Acquire(context.Background(), openAIWSAcquireRequest{
			Account: account, WSURL: "wss://example.com/v1/responses", Headers: nextHeaders,
		})
		require.NoError(t, err)
		require.True(t, second.Reused())
		require.Equal(t, firstConnID, second.ConnID())
		second.Release()

		differentIdentity := stableOpenAIWSIdentityHeadersForMergeAudit()
		differentIdentity.Set("session-id", "session-hyphen-b")
		third, err := pool.Acquire(context.Background(), openAIWSAcquireRequest{
			Account: account, WSURL: "wss://example.com/v1/responses", Headers: differentIdentity,
		})
		require.NoError(t, err)
		require.False(t, third.Reused())
		require.NotEqual(t, firstConnID, third.ConnID())
		third.Release()
		require.Equal(t, 2, dialer.DialCount())
	})
}

func TestOpenAIWSConnPool_ReplacesOrWaitsForIncompatibleHandshake_MergeAudit(t *testing.T) {
	t.Run("replace idle at capacity", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
		cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
		cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
		pool := newOpenAIWSConnPool(cfg)
		dialer := &openAIWSCountingDialer{}
		pool.setClientDialerForTest(dialer)
		account := &Account{ID: 129, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

		plain, err := pool.Acquire(context.Background(), openAIWSAcquireRequest{
			Account: account, WSURL: "wss://example.com/v1/responses",
		})
		require.NoError(t, err)
		plainConnID := plain.ConnID()
		plain.Release()

		beta, err := pool.Acquire(context.Background(), openAIWSAcquireRequest{
			Account: account,
			WSURL:   "wss://example.com/v1/responses",
			Headers: http.Header{"X-Codex-Beta-Features": {"remote_compaction_v2"}},
		})
		require.NoError(t, err)
		require.False(t, beta.Reused())
		require.NotEqual(t, plainConnID, beta.ConnID())
		beta.Release()
		require.Equal(t, 2, dialer.DialCount())
	})

	t.Run("wait for busy incompatible connection", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
		cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
		cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
		pool := newOpenAIWSConnPool(cfg)
		dialer := &openAIWSCountingDialer{}
		pool.setClientDialerForTest(dialer)
		account := &Account{ID: 130, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
		baseReq := openAIWSAcquireRequest{Account: account, WSURL: "wss://example.com/v1/responses"}

		plain, err := pool.Acquire(context.Background(), baseReq)
		require.NoError(t, err)
		plainConnID := plain.ConnID()

		type acquireResult struct {
			lease *openAIWSConnLease
			err   error
		}
		resultCh := make(chan acquireResult, 1)
		var done atomic.Bool
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		go func() {
			betaReq := baseReq
			betaReq.Headers = http.Header{"X-Codex-Beta-Features": {"remote_compaction_v2"}}
			lease, acquireErr := pool.Acquire(ctx, betaReq)
			resultCh <- acquireResult{lease: lease, err: acquireErr}
			done.Store(true)
		}()

		require.Never(t, done.Load, 50*time.Millisecond, 5*time.Millisecond)
		plain.Release()

		result := <-resultCh
		require.NoError(t, result.err)
		require.NotNil(t, result.lease)
		require.False(t, result.lease.Reused())
		require.NotEqual(t, plainConnID, result.lease.ConnID())
		result.lease.Release()
		require.Equal(t, 2, dialer.DialCount())
	})
}
