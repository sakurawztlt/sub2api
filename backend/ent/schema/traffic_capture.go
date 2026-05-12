// Package schema 定义 Ent ORM 的数据库 schema。
package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// TrafficCapture 2026-05-11 codex audit / R29 backup-only traffic capture.
//
// 备用测试环境 cctest 调试用 — 完整留存 sub2api 内部 inbound (gcr → sub2api 收到的) +
// final outbound (sub2api 改完 OAuth path / metadata.user_id / signature / headers
// 后发给真上游 Anthropic/OpenAI 的) + response body. 默认关闭, env-gated, TTL 清理.
//
// 跟 ops_error_logger 不同:
//   - ops_error_logger: 200 跳过, 只记 error 路径
//   - TrafficCapture: 200 也全量留, capture 整条链路三份 body
//
// 含 PII / api_key / signature, 默认关闭. backup 108 临时开启, 验证完 env 关.
type TrafficCapture struct {
	ent.Schema
}

func (TrafficCapture) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "traffic_captures"},
	}
}

func (TrafficCapture) Fields() []ent.Field {
	return []ent.Field{
		field.Time("ts").
			Immutable().
			Comment("capture 时间, RFC3339Nano"),
		field.String("request_id").
			MaxLen(128).
			Optional().
			Comment("NewAPI / 客户传的 X-Newapi-Request-Id 或 X-Oneapi-Request-Id"),
		field.String("upstream_request_id").
			MaxLen(128).
			Optional().
			Comment("上游 Anthropic/OpenAI 返回的 Request-Id 或 x-request-id"),
		field.Int64("api_key_id").
			Optional().
			Comment("sub2api 内部 API key ID, 0 = 未知"),
		field.Int64("account_id").
			Optional().
			Comment("绑定的 OAuth/apikey account ID, 0 = 未知"),
		field.Int64("group_id").
			Optional().
			Comment("绑定的 group ID, 0 = 未知"),
		field.String("platform").
			MaxLen(40).
			Optional().
			Comment("anthropic / openai / gemini"),
		field.String("account_type").
			MaxLen(40).
			Optional().
			Comment("oauth / setup-token / apikey"),
		field.String("model").
			MaxLen(120).
			Optional(),
		field.Int("upstream_status").
			Default(0),
		field.Bool("stream").
			Default(false),
		field.Int64("use_time_ms").
			Default(0).
			Comment("整条链路耗时 (ms)"),
		// === inbound body (sub2api 从 gcr 收到的原始 body, gcr forwarded 一致) ===
		field.Text("inbound_body").
			Optional().
			Comment("截断到 max_bytes, 见 SUB2API_TRAFFIC_CAPTURE_MAX_BYTES"),
		field.Int("inbound_body_bytes").
			Default(0).
			Comment("原始 inbound body 字节数 (截断前)"),
		field.Bool("inbound_body_truncated").
			Default(false),
		// === final outbound body (sub2api 改完, 发给真上游 Anthropic/OpenAI 的) ===
		field.Text("outbound_body").
			Optional().
			Comment("sub2api buildUpstreamRequest 之后, 含改写后 OAuth path/metadata/signature"),
		field.Int("outbound_body_bytes").
			Default(0),
		field.Bool("outbound_body_truncated").
			Default(false),
		// === response body (sub2api 收到上游响应的 raw body, stream 累积 SSE 全量) ===
		field.Text("response_body").
			Optional(),
		field.Int("response_body_bytes").
			Default(0),
		field.Bool("response_body_truncated").
			Default(false),
		// === headers snapshot ===
		field.JSON("outbound_headers", map[string]string{}).
			Optional().
			Comment("发给上游的 request header (Authorization / x-api-key 脱敏)"),
		field.JSON("response_headers", map[string]string{}).
			Optional().
			Comment("上游响应 header snapshot"),
		// === bookkeeping ===
		field.String("error_kind").
			MaxLen(80).
			Optional().
			Comment("upstream_5xx / upstream_4xx / network / timeout / stream_eof / ''"),
		field.String("error_msg").
			MaxLen(400).
			Optional(),
		// 2026-05-12 R29 P-B 加强: capture 客户端识别 (cctest 是哪个 IP/agent 打的)
		field.String("client_ip").
			MaxLen(64).
			Optional(),
		field.String("user_agent").
			MaxLen(400).
			Optional(),
		// 默认 TTL — 写入时 set, janitor 周期清理 (24h backup-only)
		field.Time("expires_at").
			Optional().
			Comment("TTL 到期时间, janitor 清理"),
	}
}

func (TrafficCapture) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("ts"),
		index.Fields("request_id"),
		index.Fields("api_key_id", "ts"),
		index.Fields("account_id", "ts"),
		index.Fields("expires_at"),
		index.Fields("upstream_status", "ts"),
	}
}
