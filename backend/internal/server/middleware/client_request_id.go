package middleware

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// upstreamRequestIDHeaders lists the headers we honor as a client-supplied
// correlation id, in priority order. NewAPI sets X-Newapi-Request-Id;
// generic upstreams use X-Request-Id / X-Correlation-Id. First non-empty
// value wins; if none present we generate a UUID.
//
// 2026-05-03 codex casjbcfasju 1322455-token incident: pre-fix we always
// generated a fresh UUID per request, so NewAPI's own request_id had no
// way to correlate to sub2api access logs after the fact. Forensics had
// to be by time-window guesswork.
var upstreamRequestIDHeaders = []string{
	"X-Newapi-Request-Id",
	"X-Newapi-Request-ID",
	"X-Request-Id",
	"X-Request-ID",
	"X-Correlation-Id",
	"X-Correlation-ID",
}

// ClientRequestID ensures every request has a unique client_request_id in request.Context().
//
// This is used by the Ops monitoring module for end-to-end request correlation.
//
// Resolution order (first non-empty wins):
//  1. ctx already carries a value (set by an upstream middleware)
//  2. an upstream-supplied header (NewAPI / proxy correlation id)
//  3. fresh UUID
func ClientRequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil {
			c.Next()
			return
		}

		if v := c.Request.Context().Value(ctxkey.ClientRequestID); v != nil {
			c.Next()
			return
		}

		id := ""
		source := "generated"
		for _, h := range upstreamRequestIDHeaders {
			if v := strings.TrimSpace(c.GetHeader(h)); v != "" {
				id = v
				source = h
				break
			}
		}
		if id == "" {
			id = uuid.New().String()
		}

		ctx := context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, id)
		requestLogger := logger.FromContext(ctx).With(
			zap.String("client_request_id", strings.TrimSpace(id)),
			zap.String("client_request_id_source", source),
		)
		ctx = logger.IntoContext(ctx, requestLogger)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
