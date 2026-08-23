package repository

import (
	"context"
	"database/sql/driver"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountRepository_SetGrokOAuthErrorIfCredentialsUnchanged_RequiresActiveExactCredentialMatch(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	applied, err := repo.SetGrokOAuthErrorIfCredentialsUnchanged(
		context.Background(),
		42,
		map[string]any{"access_token": "observed", "_token_version": int64(7)},
		"missing refresh token",
	)

	require.NoError(t, err)
	require.False(t, applied)
	require.Len(t, exec.execQueries, 1, "the account mutation and outbox insert must be one statement")
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "WITH updated AS")
	require.Contains(t, normalized, "INSERT INTO scheduler_outbox")
	require.Contains(t, normalized, "FROM updated")
	require.Contains(t, normalized, "platform = $4")
	require.Contains(t, normalized, "type = $5")
	require.Contains(t, normalized, "status = $6")
	require.Contains(t, normalized, "credentials = $7::jsonb")
	require.Contains(t, normalized, "NULLIF(BTRIM(a.credentials->>'refresh_token'), '') IS NULL")
	require.Len(t, exec.execArgs, 1)
	require.Equal(t, service.StatusActive, exec.execArgs[0][5])
	require.Contains(t, exec.execArgs[0][6], `"_token_version":7`)
}

func TestAccountRepository_SetGrokOAuthErrorIfCredentialsUnchanged_AppliedWritesOutbox(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	applied, err := repo.SetGrokOAuthErrorIfCredentialsUnchanged(
		context.Background(),
		42,
		map[string]any{"access_token": "observed"},
		"missing refresh token",
	)

	require.NoError(t, err)
	require.True(t, applied)
	require.Len(t, exec.execQueries, 1)
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "WITH updated AS")
	require.Contains(t, normalized, "INSERT INTO scheduler_outbox")
	require.Contains(t, normalized, "SELECT $8, updated.id, NULL, NULL FROM updated")
}

func TestAccountRepository_ListOAuthRefreshCandidatePage_SQLFilter(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var capturedSQL string
	var capturedArgs []any
	mock.ExpectQuery("SELECT id").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	repo := newAccountRepositoryWithSQL(nil, captureQuerySQL{db: db, captured: &capturedSQL, args: &capturedArgs}, nil)

	page, err := repo.ListOAuthRefreshCandidatePage(context.Background(), service.OAuthRefreshPageOptions{
		Platforms:            []string{service.PlatformAnthropic, service.PlatformOpenAI, service.PlatformGemini, service.PlatformAntigravity, service.PlatformGrok},
		AfterID:              100,
		Limit:                200,
		ActiveOnly:           true,
		IncludeSetupToken:    true,
		RequireRefreshToken:  true,
		ExcludeRetryCooldown: true,
	})

	require.NoError(t, err)
	require.Empty(t, page.Accounts)
	normalized := normalizeSQLWhitespace(capturedSQL)
	require.Contains(t, normalized, "deleted_at IS NULL")
	require.Contains(t, normalized, "schedulable = TRUE")
	require.Contains(t, normalized, "status = 'active'")
	require.Contains(t, normalized, "type IN ('oauth', 'setup-token')")
	require.Contains(t, normalized, "platform = ANY($1)")
	require.Contains(t, normalized, "credentials ? 'refresh_token'")
	require.Contains(t, normalized, "btrim(credentials->>'refresh_token') <> ''")
	require.Contains(t, normalized, "temp_unschedulable_until > NOW()")
	require.Contains(t, normalized, ") IS NOT TRUE")
	require.Contains(t, normalized, "id > $2")
	require.Contains(t, normalized, "ORDER BY id ASC")
	require.Contains(t, normalized, "LIMIT $3")
	require.NotContains(t, normalized, "credentials->>'expires_at'")
	require.Len(t, capturedArgs, 3)
	require.Equal(t, int64(100), capturedArgs[1])
	require.Equal(t, 200, capturedArgs[2])
	valuer, ok := capturedArgs[0].(interface{ Value() (driver.Value, error) })
	require.True(t, ok)
	platforms, err := valuer.Value()
	require.NoError(t, err)
	require.Contains(t, platforms, service.PlatformGrok)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRepository_ListOAuthRefreshCandidatePage_ReconciliationExcludesAPIKeysAndIncludesMissingRefresh(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var capturedSQL string
	mock.ExpectQuery("SELECT id").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	repo := newAccountRepositoryWithSQL(nil, captureQuerySQL{db: db, captured: &capturedSQL}, nil)

	page, err := repo.ListOAuthRefreshCandidatePage(context.Background(), service.OAuthRefreshPageOptions{
		Platforms:  []string{service.PlatformGrok},
		AfterID:    0,
		Limit:      50,
		ActiveOnly: true,
	})

	require.NoError(t, err)
	require.Empty(t, page.Accounts)
	normalized := normalizeSQLWhitespace(capturedSQL)
	require.Contains(t, normalized, "type = 'oauth'")
	require.Contains(t, normalized, "schedulable = TRUE")
	require.NotContains(t, normalized, "type IN ('oauth', 'setup-token')")
	require.NotContains(t, normalized, "type = 'api-key'")
	require.NotContains(t, normalized, "credentials ? 'refresh_token'",
		"reconciliation must discover structurally invalid OAuth rows")
	require.Contains(t, normalized, "ORDER BY id ASC")
	require.NoError(t, mock.ExpectationsWereMet())
}
