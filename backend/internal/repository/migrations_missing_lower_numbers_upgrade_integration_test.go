//go:build integration

package repository

import (
	"context"
	"database/sql"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

var restoredLowerNumberedMigrations = []string{
	"137_redeem_code_expires_at.sql",
	"138_channel_monitor_openai_api_mode.sql",
	"139_seed_openai_monitor_templates.sql",
	"140_extend_user_provider_default_grants_check.sql",
	"141_subscription_expiry_notify_enabled.sql",
	"151_channel_monitor_jitter.sql",
	"156_content_moderation_matched_keyword.sql",
	"173_allow_cyber_blocked_usage_request_type.sql",
	"175_add_ops_system_logs_host.sql",
	"175_default_openai_long_context_billing.sql",
	"175a_add_ops_system_logs_host_index_notx.sql",
	"176_channel_monitor_grok_provider.sql",
	"177_add_subscription_plan_currency.sql",
	"181_group_duplicate_operation_id.sql",
}

// TestMigrationsRunner_BackfillsRestoredLowerNumberedMigrations reproduces an
// upgrade from the first v2.29 candidate: all later migrations were recorded,
// while several lower-numbered upstream files were absent. The runner must
// discover those files by filename, apply them safely, and preserve newer
// eight-provider channel-monitor constraints.
func TestMigrationsRunner_BackfillsRestoredLowerNumberedMigrations(t *testing.T) {
	ctx := context.Background()
	pgContainer, err := tcpostgres.Run(
		ctx,
		selectDockerImage(ctx, postgresImageTag),
		tcpostgres.WithDatabase("sub2api_upgrade_test"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pgContainer.Terminate(ctx)) })

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	require.NoError(t, err)
	db, err := openSQLWithRetry(ctx, dsn, 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	withoutRestored := migrationFSWithout(t, dbmigrations.FS, restoredLowerNumberedMigrations)
	require.NoError(t, applyMigrationsFS(ctx, db, withoutRestored))
	requireMigrationRecorded(t, db, "228_channel_pricing_multipliers.sql")
	requireUpgradeColumnAbsent(t, db, "channel_monitors", "api_mode")

	require.NoError(t, applyMigrationsFS(ctx, db, dbmigrations.FS))
	require.NoError(t, applyMigrationsFS(ctx, db, dbmigrations.FS), "full migration set must remain idempotent")

	for _, name := range restoredLowerNumberedMigrations {
		requireMigrationRecorded(t, db, name)
	}
	for _, column := range [][2]string{
		{"redeem_codes", "expires_at"},
		{"channel_monitors", "api_mode"},
		{"channel_monitor_request_templates", "api_mode"},
		{"channel_monitors", "jitter_seconds"},
		{"content_moderation_logs", "matched_keyword"},
		{"ops_system_logs", "host"},
		{"subscription_plans", "currency"},
		{"groups", "duplicate_operation_id"},
	} {
		requireUpgradeColumn(t, db, column[0], column[1])
	}

	requireConstraintFragments(t, db, "channel_monitors", "channel_monitors_provider_check", "grok", "antigravity", "kimi", "zhipu", "deepseek")
	requireConstraintFragments(t, db, "channel_monitor_request_templates", "channel_monitor_request_templates_provider_check", "grok", "antigravity", "kimi", "zhipu", "deepseek")
	requireConstraintFragments(t, db, "usage_logs", "usage_logs_request_type_check", "4")
	requireConstraintFragments(t, db, "user_provider_default_grants", "user_provider_default_grants_provider_type_check", "github", "google", "dingtalk")
	requireUpgradeSetting(t, db, "subscription_expiry_notify_enabled", "true")
	requireUpgradeIndex(t, db, "idx_ops_system_logs_host_created_at")
	requireUpgradeIndex(t, db, "idx_groups_duplicate_operation_id_active")
	requireUpgradeTrigger(t, db, "accounts_enforce_openai_long_context_billing_extra")

	var seededTemplates int
	require.NoError(t, db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM channel_monitor_request_templates
WHERE provider = 'openai' AND api_mode IN ('chat_completions', 'responses')
`).Scan(&seededTemplates))
	require.GreaterOrEqual(t, seededTemplates, 4)
}

func migrationFSWithout(t *testing.T, source fs.FS, excludedNames []string) fs.FS {
	t.Helper()
	excluded := make(map[string]struct{}, len(excludedNames))
	for _, name := range excludedNames {
		excluded[name] = struct{}{}
	}

	names, err := fs.Glob(source, "*.sql")
	require.NoError(t, err)
	result := make(fstest.MapFS, len(names))
	for _, name := range names {
		if _, skip := excluded[name]; skip {
			continue
		}
		data, readErr := fs.ReadFile(source, name)
		require.NoError(t, readErr)
		result[name] = &fstest.MapFile{Data: data}
	}
	return result
}

func requireMigrationRecorded(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE filename = $1)`, name).Scan(&exists))
	require.True(t, exists, "expected migration %s to be recorded", name)
}

func requireUpgradeColumn(t *testing.T, db *sql.DB, table, column string) {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`
SELECT EXISTS (
  SELECT 1 FROM information_schema.columns
  WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
)`, table, column).Scan(&exists))
	require.True(t, exists, "expected column %s.%s", table, column)
}

func requireUpgradeColumnAbsent(t *testing.T, db *sql.DB, table, column string) {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`
SELECT EXISTS (
  SELECT 1 FROM information_schema.columns
  WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
)`, table, column).Scan(&exists))
	require.False(t, exists, "expected column %s.%s to be absent before backfill", table, column)
}

func requireConstraintFragments(t *testing.T, db *sql.DB, table, constraint string, fragments ...string) {
	t.Helper()
	var definition string
	require.NoError(t, db.QueryRow(`
SELECT pg_get_constraintdef(c.oid)
FROM pg_constraint c
JOIN pg_class t ON t.oid = c.conrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
WHERE n.nspname = 'public' AND t.relname = $1 AND c.conname = $2
`, table, constraint).Scan(&definition))
	for _, fragment := range fragments {
		require.True(t, strings.Contains(definition, fragment), "constraint %s should contain %q: %s", constraint, fragment, definition)
	}
}

func requireUpgradeSetting(t *testing.T, db *sql.DB, key, expected string) {
	t.Helper()
	var value string
	require.NoError(t, db.QueryRow(`SELECT value FROM settings WHERE key = $1`, key).Scan(&value))
	require.Equal(t, expected, value)
}

func requireUpgradeIndex(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass('public.' || $1) IS NOT NULL`, name).Scan(&exists))
	require.True(t, exists, "expected index %s", name)
}

func requireUpgradeTrigger(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`
SELECT EXISTS (
  SELECT 1 FROM pg_trigger WHERE tgname = $1 AND NOT tgisinternal
)`, name).Scan(&exists))
	require.True(t, exists, "expected trigger %s", name)
}
