package service

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 5/9 codex audit: 客户传 cache_control.ttl="5min" 等别名时, gcr 已经在入口
// 把它归一化成 "5m". 但 sub2api 这层是防御性兜底 — 有些请求路径不经 gcr
// (e.g. 备用环境 cctest 直连 k8s NodePort, 或者运营/admin 调试). 这一层
// 复制 gcr canonicalizeCacheControlTTLInBody 的等价实现, 保证 sub2api 内部
// 看到的 body 永远是 canonical "5m"/"1h".

// NormalizeAnthropicCacheTTLString — 跟 gcr cacheable_prefix.go 同函数.
// 重复实现而非依赖 gcr 包: 两个仓库独立部署, 不相互引用.
func NormalizeAnthropicCacheTTLString(s string) string {
	norm := strings.ToLower(strings.TrimSpace(s))
	switch norm {
	case "5m", "5min", "5mins", "5minute", "5minutes", "300s":
		return "5m"
	case "1h", "1hr", "1hour", "1hours", "60m", "60min", "60mins", "60minute", "60minutes", "3600s":
		return "1h"
	}
	return ""
}

// CanonicalizeAnthropicCacheControlTTLInBody 处理 inbound Anthropic-format
// /v1/messages body, 把 system / tools / messages.content 上的 cache_control.ttl
// 归一化到 "5m" / "1h". 未知值删除字段 (上游用自己的 default 不会 400).
//
// gcr 已在入口做过一次, 这里是 defense-in-depth (绕过 gcr 的边缘路径).
// 字节稳定: 用 sjson 路径写, 不动其他字段 (特别是 thinking signature).
func CanonicalizeAnthropicCacheControlTTLInBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	// Fast skip 没 cache_control.
	if !strings.Contains(string(body), "cache_control") {
		return body
	}
	body = canonicalizeCacheTTLOnArray(body, "system")
	body = canonicalizeCacheTTLOnArray(body, "tools")
	if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		for i := range msgs.Array() {
			contentPath := "messages." + intItoa(i) + ".content"
			body = canonicalizeCacheTTLOnArray(body, contentPath)
		}
	}
	return body
}

func canonicalizeCacheTTLOnArray(body []byte, arrayPath string) []byte {
	arr := gjson.GetBytes(body, arrayPath)
	if !arr.IsArray() {
		return body
	}
	for i := range arr.Array() {
		ttlPath := arrayPath + "." + intItoa(i) + ".cache_control.ttl"
		ttl := gjson.GetBytes(body, ttlPath)
		if !ttl.Exists() {
			continue
		}
		canonical := NormalizeAnthropicCacheTTLString(ttl.String())
		if canonical == "" {
			// 5/9 codex audit: 未知/空 ttl 默认 "1h" (不删除). 跟 gcr 同步,
			// 跟用户口径"传得不明确还是 1h"一致.
			canonical = "1h"
		}
		if canonical == ttl.String() {
			continue
		}
		if next, err := sjson.SetBytes(body, ttlPath, canonical); err == nil {
			body = next
		}
	}
	return body
}

func intItoa(i int) string {
	const small = "0123456789"
	if i >= 0 && i < 10 {
		return small[i : i+1]
	}
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
