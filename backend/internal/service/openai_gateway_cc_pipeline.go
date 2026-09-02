package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func (s *OpenAIGatewayService) newUpstreamSSEScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	return scanner
}

// newStreamHeaderWriter delays committing an SSE response until the first
// converted event. Early upstream failures can therefore still fail over.
func (s *OpenAIGatewayService) newStreamHeaderWriter(c *gin.Context, upstream http.Header) func() {
	headersWritten := false
	return func() {
		if headersWritten {
			return
		}
		headersWritten = true
		if s.responseHeaderFilter != nil {
			responseheaders.WriteFilteredHeaders(c.Writer.Header(), upstream, s.responseHeaderFilter)
		}
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(http.StatusOK)
	}
}

// failoverOpenAIUpstreamHTTPError applies account-aware failover policy before
// any response bytes are committed. In this relay tree the Messages transport
// still lives in openai_gateway_messages.go, so this helper intentionally
// contains only the shared decision and side effects from upstream PR #5164.
func (s *OpenAIGatewayService) failoverOpenAIUpstreamHTTPError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	resp *http.Response,
	respBody []byte,
	upstreamMsg string,
	upstreamModel string,
) *UpstreamFailoverError {
	if resp == nil || account == nil {
		return nil
	}

	shouldFailover := s.shouldFailoverOpenAIUpstreamResponseForAccount(resp.StatusCode, upstreamMsg, respBody, account)
	tempUnscheduled := false
	if c != nil && account.Platform != PlatformGrok && !shouldFailover && !IsResponseCommitted(c) && s.rateLimitService != nil {
		tempUnscheduled = s.rateLimitService.CheckErrorPolicy(ctx, account, resp.StatusCode, respBody, upstreamModel) == ErrorPolicyTempUnscheduled
		shouldFailover = tempUnscheduled
	}
	if account.Platform == PlatformGrok {
		shouldFailover = s.shouldFailoverGrokUpstreamError(resp.StatusCode, respBody)
		s.handleGrokAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
	}
	if !shouldFailover {
		return nil
	}

	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(respBody), maxBytes)
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               "failover",
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})

	shouldDisable := tempUnscheduled
	if account.Platform != PlatformGrok && !tempUnscheduled {
		shouldDisable = s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody, upstreamModel)
	}

	failoverStatus, failoverBody := sanitizeOpenAICompatFailoverError(resp.StatusCode, upstreamMsg, respBody, account)
	failoverMessage := upstreamMsg
	if failoverStatus != resp.StatusCode || !bytes.Equal(failoverBody, respBody) {
		failoverMessage = openAICompatSensitiveBackendErrorMessage
	}
	return s.newOpenAIAccountFailoverError(
		account,
		failoverStatus,
		resp.Header,
		failoverBody,
		failoverMessage,
		shouldDisable,
		!shouldDisable && account.IsPoolMode() && (account.IsPoolModeRetryableStatus(resp.StatusCode) || isOpenAITransientProcessingError(resp.StatusCode, upstreamMsg, respBody)),
	)
}

func (s *OpenAIGatewayService) openAIChatCompletionsTargetURL(account *Account) (string, error) {
	baseURL := account.GetOpenAIBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	validatedURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid base_url: %w", err)
	}
	return buildOpenAIChatCompletionsURL(validatedURL), nil
}

func (s *OpenAIGatewayService) resolveCCFallbackTarget(account *Account) (apiKey string, targetURL string, err error) {
	apiKey = strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if apiKey == "" {
		return "", "", fmt.Errorf("account %d missing api_key", account.ID)
	}
	targetURL, err = s.openAIChatCompletionsTargetURL(account)
	if err != nil {
		return "", "", err
	}
	return apiKey, targetURL, nil
}

func (s *OpenAIGatewayService) sendCCUpstreamRequest(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	targetURL string,
	body []byte,
	stream bool,
	bearerToken string,
	userAgent string,
	grokCacheIdentity string,
) (*http.Response, error) {
	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	upstreamReq, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, targetURL, bytes.NewReader(body))
	releaseUpstreamCtx()
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	// 记录本次实际选择的协议端点，供错误日志和用量日志在没有
	// OpenAIForwardResult（例如 503/传输失败）时使用。每次发送都覆盖，
	// 避免 Gin context 在账号 failover 尝试之间残留旧端点。
	SetActualOpenAIUpstreamEndpoint(c, "/v1/chat/completions")
	upstreamReq = upstreamReq.WithContext(WithHTTPUpstreamProfile(upstreamReq.Context(), HTTPUpstreamProfileOpenAI))
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Authorization", "Bearer "+bearerToken)
	if stream {
		upstreamReq.Header.Set("Accept", "text/event-stream")
	} else {
		upstreamReq.Header.Set("Accept", "application/json")
	}

	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			if !openaiCCRawAllowedHeaders[strings.ToLower(key)] {
				continue
			}
			for _, value := range values {
				upstreamReq.Header.Add(key, value)
			}
		}
	}
	if userAgent != "" {
		upstreamReq.Header.Set("user-agent", userAgent)
	}
	if account.Platform == PlatformGrok {
		if account.IsGrokOAuth() {
			applyGrokCLIHeaders(upstreamReq.Header)
		}
		applyGrokCacheHeaders(upstreamReq.Header, grokCacheIdentity)
	}
	account.ApplyHeaderOverrides(upstreamReq.Header)

	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.doOpenAIUpstream(upstreamReq, proxyURL, account)
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	return resp, nil
}

type ccStreamScanState struct {
	Usage        OpenAIUsage
	FirstTokenMs *int
	SawDone      bool
	Err          error
}

func (s *OpenAIGatewayService) scanCCStream(
	c *gin.Context,
	resp *http.Response,
	logPrefix string,
	requestID string,
	startTime time.Time,
	emit func(*apicompat.ChatCompletionsChunk),
) ccStreamScanState {
	var state ccStreamScanState
	scanner := s.newUpstreamSSEScanner(resp.Body)
	for scanner.Scan() {
		payload, ok := extractOpenAISSEDataLine(scanner.Text())
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			state.SawDone = true
			break
		}
		// Chat Completions chunks have no Responses event type. Treat them as
		// untyped observations so a declared model/service_tier describes the
		// actual upstream result rather than the requested outbound tier.
		if observer := upstreamResponseModelObserverFromContext(c); observer != nil {
			observer.ObserveOpenAI([]byte(payload), "")
		}
		if usage := extractCCStreamUsage(payload); usage != nil {
			state.Usage = *usage
		}
		var chunk apicompat.ChatCompletionsChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			logger.L().Warn(logPrefix+": failed to parse chat stream chunk", zap.Error(err), zap.String("request_id", requestID))
			continue
		}
		if state.FirstTokenMs == nil && !isOpenAIChatUsageOnlyStreamChunk(payload) && chatChunkStartsResponsesOutput(&chunk) {
			ms := int(time.Since(startTime).Milliseconds())
			state.FirstTokenMs = &ms
		}
		emit(&chunk)
	}
	if err := scanner.Err(); err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logger.L().Warn(logPrefix+": stream read error", zap.Error(err), zap.String("request_id", requestID))
		}
		state.Err = err
	}
	return state
}

func logCCStreamMissingDoneSentinel(logPrefix, requestID string) {
	logger.L().Debug(logPrefix+": upstream stream ended without done sentinel", zap.String("request_id", requestID))
}

func (s *OpenAIGatewayService) readCCUpstreamJSONResponse(
	c *gin.Context,
	resp *http.Response,
	writeError compatErrorWriter,
) (*apicompat.ChatCompletionsResponse, OpenAIUsage, error) {
	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		if !errors.Is(err, ErrUpstreamResponseBodyTooLarge) {
			writeError(c, http.StatusBadGateway, "api_error", "Failed to read upstream response")
		}
		return nil, OpenAIUsage{}, fmt.Errorf("read upstream body: %w", err)
	}
	var response apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		writeError(c, http.StatusBadGateway, "api_error", "Failed to parse upstream response")
		return nil, OpenAIUsage{}, fmt.Errorf("parse chat completions response: %w", err)
	}
	if observer := upstreamResponseModelObserverFromContext(c); observer != nil {
		observer.ObserveOpenAI(respBody, "")
	}
	usage := OpenAIUsage{}
	if parsed, ok := extractOpenAIUsageFromJSONBytes(respBody); ok {
		usage = parsed
	}
	return &response, usage, nil
}

func writeOpenAIResponsesFallbackError(c *gin.Context, statusCode int, errType, message string) {
	c.JSON(statusCode, gin.H{"error": gin.H{"type": errType, "message": message}})
}
