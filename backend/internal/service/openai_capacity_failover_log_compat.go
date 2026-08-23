package service

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

func logOpenAICapacityFailoverSuppressed(
	ctx context.Context,
	account *Account,
	path string,
	upstreamRequestID string,
	eventType string,
) {
	fields := []zap.Field{
		zap.String("path", path),
		zap.String("event_type", strings.TrimSpace(eventType)),
		zap.String("upstream_request_id", strings.TrimSpace(upstreamRequestID)),
	}
	if account != nil {
		fields = append(fields,
			zap.Int64("account_id", account.ID),
			zap.String("platform", account.Platform),
		)
	}
	logger.FromContext(ctx).Warn("gateway.failover_suppressed_after_semantic_output", fields...)
}
