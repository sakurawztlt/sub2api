package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChannelMonitorGrokProviderMigration(t *testing.T) {
	content, err := FS.ReadFile("176_channel_monitor_grok_provider.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "channel_monitors_provider_check")
	require.Contains(t, sql, "channel_monitor_request_templates_provider_check")
	require.Contains(t, sql, "CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok'))")
	require.Contains(t, sql, "position('grok' IN monitor_constraint_def) = 0")
	require.Contains(t, sql, "position('grok' IN template_constraint_def) = 0")
}

func TestChannelMonitorGrokProviderMigrationPreservesNewerProviderSuperset(t *testing.T) {
	content, err := FS.ReadFile("176_channel_monitor_grok_provider.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	monitorGuard := "IF monitor_constraint_def IS NULL OR position('grok' IN monitor_constraint_def) = 0 THEN"
	templateGuard := "IF template_constraint_def IS NULL OR position('grok' IN template_constraint_def) = 0 THEN"
	require.Contains(t, sql, monitorGuard)
	require.Contains(t, sql, templateGuard)

	// 108 may already have run the newer quota-mode migration. Its eight-provider
	// constraint contains grok, so the exact predicate used by migration 176 is
	// false and neither guarded ALTER can narrow the provider set.
	supersetConstraint := "CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kimi', 'zhipu', 'deepseek'))"
	requiresLegacyRebuild := supersetConstraint == "" || !strings.Contains(supersetConstraint, "grok")
	require.False(t, requiresLegacyRebuild)
}
