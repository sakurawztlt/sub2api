-- 2026-05-11 codex audit / R29 backup-only traffic capture.
-- 完整留存 sub2api 三份 body (inbound/outbound/response) 给 cctest 调试用.
-- env-gated 默认关闭, TTL 24h janitor 自动清.

CREATE TABLE IF NOT EXISTS traffic_captures (
    id BIGSERIAL PRIMARY KEY,
    ts TIMESTAMP WITH TIME ZONE NOT NULL,
    request_id VARCHAR(128) DEFAULT '' NOT NULL,
    upstream_request_id VARCHAR(128) DEFAULT '' NOT NULL,
    api_key_id BIGINT DEFAULT 0 NOT NULL,
    account_id BIGINT DEFAULT 0 NOT NULL,
    group_id BIGINT DEFAULT 0 NOT NULL,
    platform VARCHAR(40) DEFAULT '' NOT NULL,
    account_type VARCHAR(40) DEFAULT '' NOT NULL,
    model VARCHAR(120) DEFAULT '' NOT NULL,
    upstream_status INTEGER DEFAULT 0 NOT NULL,
    stream BOOLEAN DEFAULT false NOT NULL,
    use_time_ms BIGINT DEFAULT 0 NOT NULL,
    inbound_body TEXT DEFAULT '' NOT NULL,
    inbound_body_bytes INTEGER DEFAULT 0 NOT NULL,
    inbound_body_truncated BOOLEAN DEFAULT false NOT NULL,
    outbound_body TEXT DEFAULT '' NOT NULL,
    outbound_body_bytes INTEGER DEFAULT 0 NOT NULL,
    outbound_body_truncated BOOLEAN DEFAULT false NOT NULL,
    response_body TEXT DEFAULT '' NOT NULL,
    response_body_bytes INTEGER DEFAULT 0 NOT NULL,
    response_body_truncated BOOLEAN DEFAULT false NOT NULL,
    outbound_headers JSONB,
    response_headers JSONB,
    error_kind VARCHAR(80) DEFAULT '' NOT NULL,
    error_msg VARCHAR(400) DEFAULT '' NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_traffic_captures_ts ON traffic_captures (ts DESC);
CREATE INDEX IF NOT EXISTS idx_traffic_captures_request_id ON traffic_captures (request_id);
CREATE INDEX IF NOT EXISTS idx_traffic_captures_api_key_id_ts ON traffic_captures (api_key_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_traffic_captures_account_id_ts ON traffic_captures (account_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_traffic_captures_expires_at ON traffic_captures (expires_at);
CREATE INDEX IF NOT EXISTS idx_traffic_captures_upstream_status_ts ON traffic_captures (upstream_status, ts DESC);
