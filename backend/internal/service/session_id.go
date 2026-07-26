package service

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// maxPersistedSessionIDLength matches usage_logs.session_id VARCHAR(255).
// Values over the bound are rejected rather than truncated, so two distinct
// client identifiers can never alias in persisted usage data.
const maxPersistedSessionIDLength = 255

// persistedSessionIDHeaders deliberately contains only explicit inbound client
// headers. In particular, X-Claude-Code-Session-Id is excluded because the
// relay may synthesize it from metadata.user_id. Body prompt_cache_key and
// metadata.user_id values are likewise outside this persistence seam.
//
// The order mirrors the existing OpenAI sticky-header priority. This extractor
// is read-only and is used solely for usage-log correlation; it must never feed
// scheduling, account selection, prompt caching, or an upstream request.
var persistedSessionIDHeaders = [...]string{
	"conversation_id",
	"session_id",
	openCodeSessionIDHeader,
	openCodeNativeSessionHeader,
	codeBuddyConversationHeader,
	"X-Session-Affinity",
}

// ExtractClientSessionID returns the first valid, explicit client session
// header for usage-log persistence. Invalid candidates are skipped so a lower
// priority valid explicit header can still be recorded.
func ExtractClientSessionID(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	for _, header := range persistedSessionIDHeaders {
		if sessionID := sanitizeSessionID(c.GetHeader(header)); sessionID != "" {
			return sessionID
		}
	}
	return ""
}

// sanitizeSessionID enforces the storage contract without changing the value:
// valid UTF-8, no Unicode control characters, and at most 255 Unicode code
// points after trimming surrounding non-control whitespace.
func sanitizeSessionID(raw string) string {
	if !utf8.ValidString(raw) {
		return ""
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return ""
		}
	}
	value := strings.TrimSpace(raw)
	if value == "" || utf8.RuneCountInString(value) > maxPersistedSessionIDLength {
		return ""
	}
	return value
}
