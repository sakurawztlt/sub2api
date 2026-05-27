package service

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// sub2api fu70 codex round-two-stage-header (2026-05-24): helpers for the
// "two-stage header" strategy in openai_gateway_messages.go.
//
// Background: 5/8 commit 8f0ed16bc added `firstMeaningfulSeen` gating —
// sub2api waits to WriteHeader until it sees a meaningful event (text_delta
// / thinking_delta / tool_use / terminal usage). This was correct on its
// own (avoids empty-stream 200 to client + preserves clean failover).
//
// But OpenAI's Responses API sometimes sends 20-60s of metadata only
// (message_start / ping / reasoning items WITHOUT reasoning_summary)
// before the first text/thinking delta. During that window the client
// sees zero bytes — gcr capture showed 51s first-token. cctest /
// real-customer UX both suffer.
//
// Fix (Codex 2026-05-24 design):
//   - Stage 1 (0 .. EarlyMetaFlushAfterMs): unchanged — preserve clean
//     failover opportunity (any error here returns 502, no header).
//   - Stage 2 (≥ EarlyMetaFlushAfterMs, AND pendingEvents has
//     message_start): force flush header + pendingEvents proactively.
//     Client sees Claude SSE start immediately; subsequent events flow
//     normally.
//   - Stage 3 (≥ FirstMeaningfulEventTimeoutSeconds, no meaningful
//     event AND header still not written): unchanged terminal timeout
//     → 502 with failover.
//
// This MUST NOT touch lazy-open thinking (7aafcdc3c). That's a separate
// protection against exposing empty thinking blocks and stays intact.
//
// Narrow gate (per Codex #2): only apply early flush to requests that
// have shown the slow pattern in production:
//   - X-GCR-Early-Flush: 1 header (gcr-side opt-in marker)
//   - tools_count >= 20 (cctest / large agent shapes)
//   - body > 64KB (large prompts / multimodal)
//   - X-GCR-Estimated-Tokens >= 8K (medium academic/code-writing prompts that
//     can spend 60s+ in hidden reasoning even when the raw JSON body is <64KB)
//
// Stream=true is enforced by the caller (only stream paths reach this
// timer). Default disabled if EarlyMetaFlushAfterMs==0.

const (
	earlyFlushBodyThreshold            = 64 * 1024
	earlyFlushEstimatedTokensThreshold = 8000
)

// isEarlyFlushEligible reports whether this request's shape qualifies
// for the two-stage header optimization. Caller still checks
// cfg.Gateway.EarlyMetaFlushAfterMs > 0.
//
// Gate (any of):
//   - X-GCR-Early-Flush: 1 header (gcr-side opt-in marker, set by gcr for
//     probe-overlay matches + tools_count>=20 requests so sub2api doesn't
//     need to re-parse the body)
//   - inbound body > 64KB (catches large-prompt / multimodal that gcr didn't mark)
//   - X-GCR-Estimated-Tokens >= 8K (captures token-dense prompts below 64KB)
func isEarlyFlushEligible(httpReq *http.Request, bodyLen int) bool {
	if httpReq != nil {
		if httpReq.Header.Get("X-GCR-Early-Flush") == "1" {
			return true
		}
		if estimated := strings.TrimSpace(httpReq.Header.Get("X-GCR-Estimated-Tokens")); estimated != "" {
			if tokens, err := strconv.Atoi(estimated); err == nil && tokens >= earlyFlushEstimatedTokensThreshold {
				return true
			}
		}
	}
	if bodyLen > earlyFlushBodyThreshold {
		return true
	}
	return false
}

// hasMessageStartInPending reports whether any accumulated event is a
// message_start. We only flush header proactively if we have at least a
// message_start to send — without that, the client would see a stream
// that doesn't start with message_start (cctest structural failure).
func hasMessageStartInPending(events []apicompat.AnthropicStreamEvent) bool {
	for _, e := range events {
		if e.Type == "message_start" {
			return true
		}
	}
	return false
}
