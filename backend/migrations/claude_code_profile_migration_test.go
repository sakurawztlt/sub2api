package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration229RefreshesOnlyUntouchedClaudeCode2119Seeds(t *testing.T) {
	content, err := FS.ReadFile("229_refresh_claude_code_template_2_1_119.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "claude-cli/2.1.119 (external, sdk-cli)")
	require.Contains(t, sql, "advisor-tool-2026-03-01,effort-2025-11-24")
	require.NotContains(t, sql, "fine-grained-tool-streaming-2025-05-14")
	require.NotContains(t, sql, "prompt-caching-2024-07-31")
	require.Contains(t, sql, "description = '完整模拟 Claude Code 2.1.114")
	require.Contains(t, sql, "extra_headers->>'User-Agent' = 'claude-cli/2.1.114")
	require.Contains(t, sql, "extra_headers->>'anthropic-beta' = 'claude-code-20250219")
	require.Contains(t, sql, "UPDATE channel_monitors AS monitor")
	require.Contains(t, sql, "monitor.template_id = refreshed_template.id")
}
