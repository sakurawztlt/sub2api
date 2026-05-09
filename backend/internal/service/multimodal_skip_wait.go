package service

import (
	"bytes"
	"context"
)

type multimodalSkipWaitCtxKey struct{}

// WithMultimodalSkipWaitCtx marks ctx so the OpenAI account scheduler will
// collapse sticky/fallback wait timeouts to zero for multimodal requests.
//
// 5/9 backup 108 cctest multimodal 5/10 root cause:
//
//	cctest 同 user_id/api_key/group_id 下连发 10 个 multimodal 请求, sticky
//	hash 集中到 account_id=172, 后面的请求等 sticky_session_wait_timeout
//	(120s default). 客户端的 cctest timeout < 120s → 5 项判 fail.
//
//	GPT vision path 本来 6-13s 能完成 (前面拿到 sticky 的请求是这速度).
//	multimodal 不需要 sticky 一致性 (单轮请求, 没多轮 prompt cache 复用价值),
//	让它 sticky 满立即去重选另一账号比死等更划算.
//
// Tools / thinking 走 cc-api 真 Claude 路径, 不经过这个 sub2api 调度,
// 无副作用. 普通文本仍维持 120s sticky wait (复用 prompt cache 提速).
func WithMultimodalSkipWaitCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, multimodalSkipWaitCtxKey{}, true)
}

// IsMultimodalSkipWaitCtx reports whether the scheduler should skip waiting
// for a busy sticky/fallback slot on this request.
func IsMultimodalSkipWaitCtx(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(multimodalSkipWaitCtxKey{}).(bool)
	return v
}

// HasMultimodalContent reports whether the Anthropic-format request body
// carries an image or document content block. Detection is intentionally
// substring-based (matches both compact and pretty-printed JSON) — gjson
// scanning the nested messages.#.content.#.type structure is more
// expensive than a single bytes.Contains pass on a body that's mostly
// base64 data anyway.
func HasMultimodalContent(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	return bytes.Contains(body, []byte(`"type":"image"`)) ||
		bytes.Contains(body, []byte(`"type": "image"`)) ||
		bytes.Contains(body, []byte(`"type":"document"`)) ||
		bytes.Contains(body, []byte(`"type": "document"`))
}
