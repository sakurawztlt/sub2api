-- 2026-05-12 R29 P-B: traffic_captures 加 client_ip + user_agent 字段.
-- cctest 调试时识别请求来源 (哪个 IP / agent 打的).

ALTER TABLE traffic_captures ADD COLUMN IF NOT EXISTS client_ip VARCHAR(64) DEFAULT '' NOT NULL;
ALTER TABLE traffic_captures ADD COLUMN IF NOT EXISTS user_agent VARCHAR(400) DEFAULT '' NOT NULL;
