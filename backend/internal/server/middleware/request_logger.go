package middleware

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	// requestIDHeader is the header used on admin/other routes for request
	// tracing. API-pretending routes use different header names so responses
	// match the real upstream's conventions.
	requestIDHeader = "X-Request-ID"
	// anthropicRequestIDHeader is what real Anthropic emits on /v1/messages
	// responses — lowercase, no X- prefix. Go's http.Header canonicalises
	// this to "Request-Id" on the wire, which is still closer to real
	// Anthropic than our legacy X-Request-Id.
	anthropicRequestIDHeader = "request-id"
)

// generateAnthropicRequestID returns `req_01<22-char-base64url>` (28 chars),
// matching real Anthropic's request-id format (e.g. req_01EzvKmvMxV3eT7vKUp9Cz8ab).
// Kept in sync with apicompat.generateAnthropicMessageID (same byte count).
func generateAnthropicRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "req_01" + base64.RawURLEncoding.EncodeToString(b)
}

// isAnthropicFacingPath reports whether a URL path pretends to be Anthropic's
// /v1/messages API. Used to pick the request-id header name + format so
// responses match the real upstream.
func isAnthropicFacingPath(path string) bool {
	return strings.HasPrefix(path, "/v1/messages") ||
		strings.HasPrefix(path, "/v1/complete")
}

// RequestLogger 在请求入口注入 request-scoped logger。
//
// For Anthropic-facing paths (/v1/messages, /v1/complete), the response
// carries a `request-id` header in Anthropic's native format
// (`req_01<base64url>`). For all other paths (admin, OpenAI/chat completions,
// health, etc.) the legacy `X-Request-ID` UUID header is used so existing
// admin/tracing tooling keeps working.
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil {
			c.Next()
			return
		}

		path := ""
		if c.Request.URL != nil {
			path = c.Request.URL.Path
		}
		anthropicFacing := isAnthropicFacingPath(path)

		// Respect client-supplied X-Request-ID on admin paths (tracing
		// continuity). On Anthropic-facing paths, always regenerate so
		// the emitted id matches the req_01 format regardless of what
		// the client sent — real Anthropic ignores client-supplied
		// tracking ids the same way.
		requestID := ""
		if !anthropicFacing {
			requestID = strings.TrimSpace(c.GetHeader(requestIDHeader))
		}
		if requestID == "" {
			if anthropicFacing {
				requestID = generateAnthropicRequestID()
			} else {
				requestID = uuid.NewString()
			}
		}

		if anthropicFacing {
			c.Header(anthropicRequestIDHeader, requestID)
		} else {
			c.Header(requestIDHeader, requestID)
		}

		ctx := context.WithValue(c.Request.Context(), ctxkey.RequestID, requestID)
		clientRequestID, _ := ctx.Value(ctxkey.ClientRequestID).(string)

		requestLogger := logger.With(
			zap.String("component", "http"),
			zap.String("request_id", requestID),
			zap.String("client_request_id", strings.TrimSpace(clientRequestID)),
			zap.String("path", path),
			zap.String("method", c.Request.Method),
		)

		ctx = logger.IntoContext(ctx, requestLogger)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
