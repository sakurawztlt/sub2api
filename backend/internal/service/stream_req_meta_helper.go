package service

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/tidwall/gjson"
)

// 2026-05-15 codex round 11al: helper to populate streamReqMeta from
// per-request inputs. Centralizes the message-count / cache-key-hash /
// proxy-hash derivation so caller (ForwardAsAnthropic) stays clean.
//
// All fields are best-effort observability — empty/zero values are fine
// when source signals are absent (e.g. no proxy → ProxyHash="").

// computeStreamReqMeta builds the forensics struct passed into
// handleAnthropicStreamingResponse. Inputs are taken from the calling
// site's local vars (account, body, promptCacheKey, etc.).
func computeStreamReqMeta(
	account *Account,
	body []byte,
	promptCacheKey string,
	previousResponseID string,
	turnState string,
	proxyURL string,
	timeToHeadersMs int,
) streamReqMeta {
	return streamReqMeta{
		Account:                   account,
		TimeToHeadersMs:           timeToHeadersMs,
		MessagesCount:             countMessagesInBody(body),
		ToolsCount:                countToolsInBody(body),
		CodeExecutionFallbackArgs: apicompat.CodeExecutionFallbackArgsFromAnthropicRequest(body),
		PromptCacheKeySha256:      shortSHA256(promptCacheKey),
		HasPreviousResponseID:     strings.TrimSpace(previousResponseID) != "",
		HasTurnState:              strings.TrimSpace(turnState) != "",
		ProxyHash:                 proxyURLHash(proxyURL),
	}
}

func countMessagesInBody(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return 0
	}
	return len(msgs.Array())
}

func countToolsInBody(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return 0
	}
	return len(tools.Array())
}

// shortSHA256 returns hex(sha256(s))[:16]. Empty in → empty out.
// 16 hex chars = 64 bits, enough collision resistance for log grouping.
func shortSHA256(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// proxyURLHash returns a short hash of the proxy URL for log grouping
// without leaking credentials. Empty URL → empty.
func proxyURLHash(proxyURL string) string {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return ""
	}
	// Strip credentials before hashing — userinfo segment may contain
	// password tokens we don't want hashed into log buckets.
	if idx := strings.Index(proxyURL, "@"); idx > 0 {
		if scheme := strings.Index(proxyURL, "://"); scheme > 0 && scheme < idx {
			proxyURL = proxyURL[:scheme+3] + proxyURL[idx+1:]
		}
	}
	sum := sha256.Sum256([]byte(proxyURL))
	return hex.EncodeToString(sum[:])[:12]
}

// classifyTimeoutState — codex round 11al item 4: 区分 3 类 first
// meaningful timeout 失败模式, 让运维一眼能判断是哪一层卡:
//   - "no_sse_line"   — 上游 HTTP headers 已返 (能到 stream loop) 但
//     一直没发任何 SSE line. 通常代表 OpenAI/Codex 在 prefill 排队或
//     冷启动慢, 还没开始 emit 任何 chunk.
//   - "metadata_only" — 上游发了 message_start / content_block_start /
//     ping 等 metadata, 但没出首个业务事件 (content_block_delta 等).
//     通常代表上游 prefill 已经过去, 但 generation 卡住 / 风控.
//   - "unknown"       — 状态信息不全 (兜底, 一般不应该出现).
//
// firstChunk=true 时说明 SSE 一行都没有. firstChunk=false +
// firstMeaningfulSeen=false 说明拿到 metadata 但没拿到 meaningful.
func classifyTimeoutState(firstChunk bool, firstMeaningfulSeen bool) string {
	if firstMeaningfulSeen {
		return "unknown" // 不应该到这里, timer 应该被 Stop
	}
	if firstChunk {
		return "no_sse_line"
	}
	return "metadata_only"
}
