package service

import (
	"context"

	"github.com/tidwall/gjson"
)

type largeContextCtxKey struct{}

// WithLargeContextCtx marks ctx so the OpenAI messages stream processor
// uses the tighter "large request" first-meaningful timeout and the
// handler refuses to retry on first-timeout failover (per-reason cap=0).
//
// 2026-05-15 codex round 11ai: backup 108 看到 NewAPI request_id
// 202605150325...d9d6TQDlPL9o 等了 239s 客户断开. gcr [route-passthrough]
// 显示 msgs=135 lookup_bp=136 deepest_hit=296544 — 是 Claude Code 深上下文.
// sub2api 默认 2 轮 first_meaningful_timeout 让一个大请求烧 240s 才失败,
// 客户体验差且 NewAPI 已经按 message_start.usage 全额扣 cache_creation+
// cache_read 费. fail-fast 让 240s → 45s.
func WithLargeContextCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, largeContextCtxKey{}, true)
}

// IsLargeContextCtx reports whether the current request was classified
// as a large-context request (high msgs count OR large body bytes).
func IsLargeContextCtx(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(largeContextCtxKey{}).(bool)
	return v
}

// IsLargeContextRequest classifies an Anthropic-format /v1/messages
// request as "large" using two cheap signals:
//
//  1. messages array length > msgThreshold (default 100; deep CC sessions)
//  2. body bytes > bodyBytesThreshold (default 800000; rough 200k input
//     tokens at 4 bytes/token estimate)
//
// Either signal alone trips. Both thresholds are 0-disable so the gate
// is opt-in via configmap.
func IsLargeContextRequest(body []byte, msgThreshold, bodyBytesThreshold int) bool {
	if msgThreshold > 0 {
		if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
			if len(msgs.Array()) > msgThreshold {
				return true
			}
		}
	}
	if bodyBytesThreshold > 0 && len(body) > bodyBytesThreshold {
		return true
	}
	return false
}
