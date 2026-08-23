package service

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func openAIUsageHasTokens(usage *OpenAIUsage) bool {
	return usage != nil && (usage.InputTokens > 0 || usage.ImageInputTokens > 0 ||
		usage.OutputTokens > 0 || usage.CacheCreationInputTokens > 0 ||
		usage.CacheReadInputTokens > 0 || usage.ImageOutputTokens > 0)
}

const openAIMissingUsageLogInterval = time.Minute

type openAIMissingUsageLogSampler struct {
	total      atomic.Uint64
	suppressed atomic.Uint64
	lastLog    atomic.Int64
}

var openAIMissingUsageSampler openAIMissingUsageLogSampler

func (s *openAIMissingUsageLogSampler) sample(now time.Time) (logNow bool, total uint64, suppressed uint64) {
	total = s.total.Add(1)
	nowNanos := now.UnixNano()
	for {
		last := s.lastLog.Load()
		if last != 0 && nowNanos-last < int64(openAIMissingUsageLogInterval) {
			s.suppressed.Add(1)
			return false, total, 0
		}
		if s.lastLog.CompareAndSwap(last, nowNanos) {
			return true, total, s.suppressed.Swap(0)
		}
	}
}

func logOpenAISuccessMissingUsage(ctx context.Context, c *gin.Context, account *Account, resp *http.Response, usage *OpenAIUsage, terminalEvent string, clientDisconnected bool) {
	if resp == nil || resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices || openAIUsageHasTokens(usage) {
		return
	}
	terminalEvent = strings.TrimSpace(terminalEvent)
	if terminalEvent != "response.completed" && terminalEvent != "response.done" && terminalEvent != "[DONE]" && terminalEvent != "json" {
		return
	}
	logNow, total, suppressed := openAIMissingUsageSampler.sample(time.Now())
	if !logNow {
		return
	}
	accountID := int64(0)
	accountType := ""
	if account != nil {
		accountID = account.ID
		accountType = string(account.Type)
	}
	inboundEndpoint := ""
	if c != nil && c.Request != nil && c.Request.URL != nil {
		inboundEndpoint = c.Request.URL.Path
	}
	logger.FromContext(ctx).With(
		zap.String("component", "service.openai_gateway"),
		zap.Int64("account_id", accountID),
		zap.String("account_type", accountType),
		zap.String("inbound_endpoint", inboundEndpoint),
		zap.String("upstream_request_id", strings.TrimSpace(resp.Header.Get("x-request-id"))),
		zap.Int("upstream_status_code", resp.StatusCode),
		zap.String("terminal_event", terminalEvent),
		zap.Bool("client_disconnected", clientDisconnected),
		zap.Uint64("missing_usage_total", total),
		zap.Uint64("suppressed_since_last", suppressed),
	).Warn("openai_usage.success_missing_usage")
}
