-- Migration: 229_refresh_claude_code_template_2_1_119
--
-- Refresh only the untouched built-in Claude Code monitor seed. Operators may
-- edit templates and monitor snapshots independently, so both UPDATEs match
-- the complete stale fingerprint before changing anything.

WITH refreshed_template AS (
    UPDATE channel_monitor_request_templates
    SET description = '完整模拟 Claude Code 2.1.119 客户端：UA + anthropic-beta + system + metadata.user_id 全部对齐，绕过 Anthropic 上游 ''Claude Code only'' 限制（如 Max 套餐）。',
        extra_headers = jsonb_set(
            jsonb_set(
                extra_headers,
                '{User-Agent}',
                to_jsonb('claude-cli/2.1.119 (external, sdk-cli)'::text),
                true
            ),
            '{anthropic-beta}',
            to_jsonb('claude-code-20250219,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advisor-tool-2026-03-01,effort-2025-11-24'::text),
            true
        ),
        updated_at = NOW()
    WHERE provider = 'anthropic'
      AND name = 'Claude Code 伪装'
      AND description = '完整模拟 Claude Code 2.1.114 客户端：UA + anthropic-beta + system + metadata.user_id 全部对齐，绕过 Anthropic 上游 ''Claude Code only'' 限制（如 Max 套餐）。'
      AND extra_headers->>'User-Agent' = 'claude-cli/2.1.114 (external, sdk-cli)'
      AND extra_headers->>'anthropic-beta' = 'claude-code-20250219,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advisor-tool-2026-03-01'
    RETURNING id
)
UPDATE channel_monitors AS monitor
SET extra_headers = jsonb_set(
        jsonb_set(
            monitor.extra_headers,
            '{User-Agent}',
            to_jsonb('claude-cli/2.1.119 (external, sdk-cli)'::text),
            true
        ),
        '{anthropic-beta}',
        to_jsonb('claude-code-20250219,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advisor-tool-2026-03-01,effort-2025-11-24'::text),
        true
    ),
    updated_at = NOW()
FROM refreshed_template
WHERE monitor.template_id = refreshed_template.id
  AND monitor.extra_headers->>'User-Agent' = 'claude-cli/2.1.114 (external, sdk-cli)'
  AND monitor.extra_headers->>'anthropic-beta' = 'claude-code-20250219,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advisor-tool-2026-03-01';
