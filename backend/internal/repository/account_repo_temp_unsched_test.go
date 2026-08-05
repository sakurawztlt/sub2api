package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountRepository_GrokCredentialConditionalMutationsAreEligibleAndAtomicallyPropagated(t *testing.T) {
	proxyID := int64(77)
	snapshot := service.GrokCredentialMutationSnapshot{
		CredentialsJSON: `{"access_token":"access","refresh_token":"refresh","_token_version":123}`,
		ProxyID:         &proxyID,
	}

	t.Run("permanent", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		updated, err := repo.SetGrokCredentialErrorIfMatch(context.Background(), 42, snapshot, "revoked")

		require.NoError(t, err)
		require.False(t, updated)
		require.Len(t, exec.execQueries, 1)
		normalized := normalizeSQLWhitespace(exec.execQueries[0])
		require.Contains(t, normalized, "WITH updated AS ( UPDATE accounts AS a")
		require.Contains(t, normalized, "a.schedulable IS TRUE")
		require.Contains(t, normalized, "a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until <= NOW()")
		require.Contains(t, normalized, "a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at <= NOW()")
		require.Contains(t, normalized, "a.overload_until IS NULL OR a.overload_until <= NOW()")
		require.Contains(t, normalized, "a.credentials = $7::jsonb")
		require.Contains(t, normalized, "a.proxy_id IS NOT DISTINCT FROM $8")
		require.Contains(t, normalized, "NOT EXISTS ( SELECT 1 FROM proxies p")
		require.Contains(t, normalized, "INSERT INTO scheduler_outbox")
		require.Len(t, exec.execArgs[0], 10)
		require.Equal(t, snapshot.CredentialsJSON, exec.execArgs[0][6])
		require.Equal(t, &proxyID, exec.execArgs[0][7])
		require.Equal(t, string(service.GrokCredentialReasonProxyInvalid), exec.execArgs[0][8])
		require.Equal(t, service.SchedulerOutboxEventAccountChanged, exec.execArgs[0][9])
	})

	t.Run("transient", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		updated, err := repo.SetGrokCredentialTempUnschedulableIfMatch(
			context.Background(), 42, snapshot, time.Now().Add(time.Minute), "temporary",
		)

		require.NoError(t, err)
		require.False(t, updated)
		require.Len(t, exec.execQueries, 1)
		normalized := normalizeSQLWhitespace(exec.execQueries[0])
		require.Contains(t, normalized, "WITH updated AS ( UPDATE accounts AS a")
		require.Contains(t, normalized, "a.schedulable IS TRUE")
		require.Contains(t, normalized, "a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until <= NOW()")
		require.Contains(t, normalized, "a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at <= NOW()")
		require.Contains(t, normalized, "a.overload_until IS NULL OR a.overload_until <= NOW()")
		require.Contains(t, normalized, "a.credentials = $7::jsonb")
		require.Contains(t, normalized, "a.proxy_id IS NOT DISTINCT FROM $8")
		require.Contains(t, normalized, "INSERT INTO scheduler_outbox")
		require.Len(t, exec.execArgs[0], 9)
		require.Equal(t, snapshot.CredentialsJSON, exec.execArgs[0][6])
		require.Equal(t, &proxyID, exec.execArgs[0][7])
		require.Equal(t, service.SchedulerOutboxEventAccountChanged, exec.execArgs[0][8])
	})
}

func TestAccountRepository_GrokCredentialCommitCarriesOutboxAcrossCallerCancellation(t *testing.T) {
	snapshot := service.GrokCredentialMutationSnapshot{CredentialsJSON: `{"access_token":"access","refresh_token":"refresh"}`}
	tests := []struct {
		name   string
		mutate func(context.Context, *accountRepository) (bool, error)
	}{
		{
			name: "permanent",
			mutate: func(ctx context.Context, repo *accountRepository) (bool, error) {
				return repo.SetGrokCredentialErrorIfMatch(ctx, 42, snapshot, string(service.GrokCredentialReasonRevoked))
			},
		},
		{
			name: "transient",
			mutate: func(ctx context.Context, repo *accountRepository) (bool, error) {
				return repo.SetGrokCredentialTempUnschedulableIfMatch(ctx, 42, snapshot, time.Now().Add(time.Minute), string(service.GrokCredentialReasonRefreshTransient))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			exec := &recordingSQLExecutor{result: rowsAffectedResult(1), afterExec: cancel}
			repo := newAccountRepositoryWithSQL(nil, exec, nil)

			updated, err := tt.mutate(ctx, repo)

			require.NoError(t, err)
			require.True(t, updated)
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.Len(t, exec.execQueries, 1, "state update and scheduler outbox must share one atomic SQL statement")
			require.Contains(t, normalizeSQLWhitespace(exec.execQueries[0]), "INSERT INTO scheduler_outbox")
		})
	}
}
