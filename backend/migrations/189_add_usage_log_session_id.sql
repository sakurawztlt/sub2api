-- Persist only an explicit client-provided request session/conversation header
-- so usage rows can be correlated without retaining synthetic sticky signals.
--
-- Nullable with no default: on PostgreSQL 11+ this is a metadata-only change
-- and does not rewrite the usage_logs table. Absent or invalid identifiers stay
-- NULL. Body fields such as prompt_cache_key and metadata.user_id, as well as
-- relay-generated identifiers, are never stored in this column.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS session_id VARCHAR(255);
