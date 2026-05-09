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
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// ForwardAsAnthropic accepts an Anthropic Messages request body, converts it
// to OpenAI Responses API format, forwards to the OpenAI upstream, and converts
// the response back to Anthropic Messages format. This enables Claude Code
// clients to access OpenAI models through the standard /v1/messages endpoint.
func (s *OpenAIGatewayService) ForwardAsAnthropic(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	promptCacheKey string,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()

	// 1. Parse Anthropic request
	var anthropicReq apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}
	// 058 step 2: snapshot the unmutated request for digest derivation. The
	// digest must reflect what the *client* sent, before normalization or
	// the replay-guard sliding window — otherwise the same conversation
	// produces a different digest each turn and prompt cache is invalidated.
	anthropicDigestReq := cloneAnthropicRequestForDigest(&anthropicReq)
	originalModel := anthropicReq.Model
	applyOpenAICompatModelNormalization(&anthropicReq)
	normalizedModel := anthropicReq.Model
	clientStream := anthropicReq.Stream // client's original stream preference

	// 2. Model mapping (computed early so 058 prompt-cache derivation can
	// gate on the upstream model).
	billingModel := resolveOpenAIForwardModel(account, normalizedModel, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	apiKeyID := getAPIKeyIDFromContext(c)

	// 058 step 2: prompt-cache key derivation, replay guard, continuation
	// chain, and todo guard. All gated on the upstream model + account type.
	promptCacheKey = strings.TrimSpace(promptCacheKey)
	anthropicDigestChain := ""
	anthropicMatchedDigestChain := ""
	compatPromptCacheInjected := false
	if promptCacheKey == "" && shouldAutoInjectPromptCacheKeyForCompat(upstreamModel) {
		// Three-layer fallback: Anthropic metadata.user_id → cache_control
		// breakpoints → full message digest. Each layer downgrades how
		// stable the key is across multi-turn conversations.
		promptCacheKey = promptCacheKeyFromAnthropicMetadataSession(&anthropicReq)
		if promptCacheKey == "" {
			promptCacheKey = deriveAnthropicCacheControlPromptCacheKey(&anthropicReq)
		}
		if promptCacheKey == "" {
			anthropicDigestChain = buildOpenAICompatAnthropicDigestChain(anthropicDigestReq)
			if reusedKey, matchedChain := s.findOpenAICompatAnthropicDigestPromptCacheKey(account, apiKeyID, anthropicDigestChain); reusedKey != "" {
				promptCacheKey = reusedKey
				anthropicMatchedDigestChain = matchedChain
			} else {
				promptCacheKey = promptCacheKeyFromAnthropicDigest(anthropicDigestChain)
			}
		}
		compatPromptCacheInjected = promptCacheKey != ""
	}

	compatReplayGuardEnabled := shouldAutoInjectPromptCacheKeyForCompat(upstreamModel)
	compatContinuationEnabled := openAICompatContinuationEnabled(account, upstreamModel)
	previousResponseID := ""
	if compatContinuationEnabled {
		previousResponseID = s.getOpenAICompatSessionResponseID(ctx, c, account, promptCacheKey)
	}
	compatContinuationDisabled := compatContinuationEnabled &&
		s.isOpenAICompatSessionContinuationDisabled(ctx, c, account, promptCacheKey)
	compatTurnState := ""
	compatReplayTrimmed := false
	// OAuth/Plus relies on session_id + x-codex-turn-state; trimming to a
	// sliding 12-message window makes the cached prefix stall at system/tools.
	// Keep full replay there so upstream prompt caching can grow turn by turn.
	if compatReplayGuardEnabled && account.Type != AccountTypeOAuth && previousResponseID == "" && !compatContinuationDisabled {
		compatReplayTrimmed = applyAnthropicCompatFullReplayGuard(&anthropicReq)
	}

	// 3. Convert Anthropic → Responses (after replay guard mutates messages).
	responsesReq, err := apicompat.AnthropicToResponses(&anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("convert anthropic to responses: %w", err)
	}

	// Upstream always uses streaming (upstream may not support sync mode).
	// The client's original preference determines the response format.
	responsesReq.Stream = true
	isStream := true

	// 3b. Handle BetaFastMode → service_tier: "priority"
	if containsBetaToken(c.GetHeader("anthropic-beta"), claude.BetaFastMode) {
		responsesReq.ServiceTier = "priority"
	}

	responsesReq.Model = upstreamModel
	if previousResponseID != "" {
		responsesReq.PreviousResponseID = previousResponseID
		trimAnthropicCompatResponsesInputToLatestTurn(responsesReq)
	}
	if compatReplayGuardEnabled && account.Type != AccountTypeOAuth {
		appendOpenAICompatClaudeCodeTodoGuard(responsesReq)
	}

	logFields := []zap.Field{
		zap.Int64("account_id", account.ID),
		zap.String("original_model", originalModel),
		zap.String("normalized_model", normalizedModel),
		zap.String("billing_model", billingModel),
		zap.String("upstream_model", upstreamModel),
		zap.Bool("stream", isStream),
	}
	if compatPromptCacheInjected {
		logFields = append(logFields,
			zap.Bool("compat_prompt_cache_key_injected", true),
			zap.String("compat_prompt_cache_key_sha256", hashSensitiveValueForLog(promptCacheKey)),
		)
	}
	if compatReplayTrimmed {
		logFields = append(logFields,
			zap.Bool("compat_full_replay_trimmed", true),
			zap.Int("compat_messages_after_trim", len(anthropicReq.Messages)),
		)
	}
	if previousResponseID != "" {
		logFields = append(logFields,
			zap.Bool("compat_previous_response_id_attached", true),
			zap.String("compat_previous_response_id", truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen)),
		)
	}
	logger.L().Debug("openai messages: model mapping applied", logFields...)

	// 4. Marshal Responses request body, then apply OAuth codex transform
	responsesBody, err := json.Marshal(responsesReq)
	if err != nil {
		return nil, fmt.Errorf("marshal responses request: %w", err)
	}
	if promptCacheKey != "" {
		responsesBody, err = sjson.SetBytes(responsesBody, "prompt_cache_key", promptCacheKey)
		if err != nil {
			return nil, fmt.Errorf("inject prompt_cache_key: %w", err)
		}
	}

	if account.Type == AccountTypeOAuth {
		textFormatRaw := extractResponsesTextFormatRaw(responsesBody)
		var reqBody map[string]any
		if err := json.Unmarshal(responsesBody, &reqBody); err != nil {
			return nil, fmt.Errorf("unmarshal for codex transform: %w", err)
		}
		// 058 step 2: messages bridge skips the default "helpful coding assistant"
		// instructions (the developer-message shape is authoritative) and keeps
		// Anthropic tool ids verbatim through the call_id round trip.
		codexResult := applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{
			SkipDefaultInstructions: true,
			PreserveToolCallIDs:     true,
		})
		forcedTemplateText := ""
		if s.cfg != nil {
			forcedTemplateText = s.cfg.Gateway.ForcedCodexInstructionsTemplate
		}
		templateUpstreamModel := upstreamModel
		if codexResult.NormalizedModel != "" {
			templateUpstreamModel = codexResult.NormalizedModel
		}
		existingInstructions, _ := reqBody["instructions"].(string)
		// 058 step 2: when the codex transform's developer-message extraction
		// did not populate instructions (e.g. because the bridge skipped the
		// default), pull from input directly so the forced-template feature
		// still has client text to prepend onto.
		if strings.TrimSpace(existingInstructions) == "" {
			existingInstructions = extractPromptLikeInstructionsFromInput(reqBody)
		}
		if _, err := applyForcedCodexInstructionsTemplate(reqBody, forcedTemplateText, forcedCodexInstructionsTemplateData{
			ExistingInstructions: strings.TrimSpace(existingInstructions),
			OriginalModel:        originalModel,
			NormalizedModel:      normalizedModel,
			BillingModel:         billingModel,
			UpstreamModel:        templateUpstreamModel,
		}); err != nil {
			return nil, err
		}
		// 058 step 2: ensure instructions is always a string field upstream
		// so Codex SSE stops emitting "instructions: null" rejection paths.
		ensureCodexOAuthInstructionsField(reqBody)
		if shouldAutoInjectPromptCacheKeyForCompat(upstreamModel) {
			appendOpenAICompatClaudeCodeTodoGuardToRequestBody(reqBody)
		}
		if codexResult.NormalizedModel != "" {
			upstreamModel = codexResult.NormalizedModel
		}
		if codexResult.PromptCacheKey != "" {
			promptCacheKey = codexResult.PromptCacheKey
		}
		// 058 step 2: prompt_cache_key is captured into result + sent via the
		// session_id header. Sending it again in the body confuses Codex SSE
		// (rejected as unsupported field on bridge path).
		delete(reqBody, "prompt_cache_key")
		if shouldAutoInjectPromptCacheKeyForCompat(upstreamModel) {
			compatTurnState = s.getOpenAICompatSessionTurnState(ctx, c, account, promptCacheKey)
		}
		if serviceTier := extractOpenAIServiceTier(reqBody); serviceTier != nil {
			responsesReq.ServiceTier = *serviceTier
		} else {
			responsesReq.ServiceTier = ""
		}
		// OAuth codex transform forces stream=true upstream, so always use
		// the streaming response handler regardless of what the client asked.
		isStream = true
		responsesBody, err = json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("remarshal after codex transform: %w", err)
		}
		responsesBody, err = restoreResponsesTextFormatRaw(responsesBody, textFormatRaw)
		if err != nil {
			return nil, fmt.Errorf("restore text.format after codex transform: %w", err)
		}
	}

	// For API key accounts (including OpenAI-compatible upstream gateways),
	// ensure promptCacheKey is also propagated via the request body so that
	// upstreams using the Responses API can derive a stable session identifier
	// from prompt_cache_key. This makes our Anthropic /v1/messages compatibility
	// path behave more like a native Responses client.
	if account.Type == AccountTypeAPIKey {
		if trimmedKey := strings.TrimSpace(promptCacheKey); trimmedKey != "" {
			var reqBody map[string]any
			if err := json.Unmarshal(responsesBody, &reqBody); err != nil {
				return nil, fmt.Errorf("unmarshal for prompt cache key injection: %w", err)
			}
			if existing, ok := reqBody["prompt_cache_key"].(string); !ok || strings.TrimSpace(existing) == "" {
				reqBody["prompt_cache_key"] = trimmedKey
				updated, err := json.Marshal(reqBody)
				if err != nil {
					return nil, fmt.Errorf("remarshal after prompt cache key injection: %w", err)
				}
				responsesBody = updated
			}
		}
	}

	// 4c. Apply OpenAI fast policy (may filter service_tier or block the request).
	// Mirrors the Claude anthropic-beta "fast-mode-2026-02-01" filter, but keyed
	// on the body-level service_tier field (priority/flex).
	updatedBody, policyErr := s.applyOpenAIFastPolicyToBody(ctx, account, upstreamModel, responsesBody)
	if policyErr != nil {
		var blocked *OpenAIFastBlockedError
		if errors.As(policyErr, &blocked) {
			writeAnthropicError(c, http.StatusForbidden, "forbidden_error", blocked.Message)
		}
		return nil, policyErr
	}
	responsesBody = updatedBody

	// 5. Get access token
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	// 6. Build upstream request
	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	upstreamReq, err := s.buildUpstreamRequest(upstreamCtx, c, account, responsesBody, token, isStream, promptCacheKey, false)
	releaseUpstreamCtx()
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}

	// Align /v1/messages OAuth/Codex upstream session headers to a single stable
	// session_id with the isolated prompt cache key to preserve the legacy
	// upstream session behavior for OAuth/Codex accounts.
	if promptCacheKey != "" {
		isolatedSessionID := generateSessionUUID(isolateOpenAISessionID(apiKeyID, promptCacheKey))
		upstreamReq.Header.Set("session_id", isolatedSessionID)
		// 058 step 2: when upstream/builder set conversation_id we re-align it
		// onto the isolated session id so per-key sessions stay separated.
		if upstreamReq.Header.Get("conversation_id") != "" {
			upstreamReq.Header.Set("conversation_id", isolatedSessionID)
		}
	}
	if account.Type == AccountTypeOAuth {
		// 058 step 2: Anthropic Messages → ChatGPT Codex SSE does NOT accept
		// the Responses experimental beta header, and forcing originator can
		// switch ChatGPT to a different internal continuation path. airgate-
		// openai's airgate bridge omits both — match that shape.
		upstreamReq.Header.Del("OpenAI-Beta")
		upstreamReq.Header.Del("originator")
	}
	if account.Type == AccountTypeOAuth && promptCacheKey != "" && strings.TrimSpace(c.GetHeader("conversation_id")) == "" {
		// Without inbound conversation_id, sending one upstream creates a
		// disposable conversation that confuses the cache lookup.
		upstreamReq.Header.Del("conversation_id")
	}
	if compatTurnState != "" && upstreamReq.Header.Get("x-codex-turn-state") == "" {
		// 058 step 2: replay-side Codex turn state. The upstream emits
		// x-codex-turn-state on the response header; we cache it under
		// the prompt_cache_key and replay on the next turn so Codex SSE
		// resumes the same internal continuation slot.
		upstreamReq.Header.Set("x-codex-turn-state", compatTurnState)
	}

	// 7. Send request
	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.ActiveURL()
	}
	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		safeErr := sanitizeUpstreamErrorMessage(err.Error())
		setOpsUpstreamError(c, 0, safeErr, "")
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: 0,
			Kind:               "request_error",
			Message:            safeErr,
		})
		// 5/9 codex audit fix: 网络层错误 (connection refused / DNS / TLS
		// handshake / SOCKS proxy unreachable) 走 handler failover 路径,
		// 由 handler 决定 DeleteStickySession + 重选账号. **不在 service
		// 层写客户响应** — 否则 handler 后续 retry 写新响应会响应拼接.
		// 非网络错误维持原行为 (写 502 + 返普通 error).
		if IsUpstreamNetworkError(err) {
			return nil, &UpstreamFailoverError{
				StatusCode:  http.StatusBadGateway,
				BreakSticky: true,
			}
		}
		// Generic Anthropic-style message — "Upstream request failed" leaks
		// our relay wording to clients. Specific cause is already logged
		// upstream + recorded via appendOpsUpstreamError above.
		writeAnthropicError(c, http.StatusBadGateway, "api_error", "Internal server error")
		return nil, fmt.Errorf("upstream request failed: %s", safeErr)
	}
	defer func() { _ = resp.Body.Close() }()

	// 8. Handle error response with failover
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
		upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
		// 058 step 2: when upstream rejects the cached previous_response_id
		// (account moved, response evicted, server doesn't honor the field),
		// drop the binding and retry once without continuation. "unsupported"
		// is sticky — disables continuation for this prompt key permanently;
		// "not_found" is per-call.
		if previousResponseID != "" && (isOpenAICompatPreviousResponseNotFound(resp.StatusCode, upstreamMsg, respBody) || isOpenAICompatPreviousResponseUnsupported(resp.StatusCode, upstreamMsg, respBody)) {
			if isOpenAICompatPreviousResponseUnsupported(resp.StatusCode, upstreamMsg, respBody) {
				s.disableOpenAICompatSessionContinuation(ctx, c, account, promptCacheKey)
			} else {
				s.deleteOpenAICompatSessionResponseID(ctx, c, account, promptCacheKey)
			}
			logger.L().Info("openai messages: previous_response_id unavailable, retrying without continuation",
				zap.Int64("account_id", account.ID),
				zap.String("previous_response_id", truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen)),
				zap.String("upstream_model", upstreamModel),
			)
			return s.ForwardAsAnthropic(ctx, c, account, body, promptCacheKey, defaultMappedModel)
		}
		if s.shouldFailoverOpenAIUpstreamResponse(resp.StatusCode, upstreamMsg, respBody) {
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
			if s.rateLimitService != nil {
				s.rateLimitService.HandleUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
			}
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && (isPoolModeRetryableStatus(resp.StatusCode) || isOpenAITransientProcessingError(resp.StatusCode, upstreamMsg, respBody)),
			}
		}
		// Non-failover error: return Anthropic-formatted error to client
		return s.handleAnthropicErrorResponse(resp, c, account)
	}

	// 058 step 2: Codex SSE returns x-codex-turn-state on the success header.
	// Cache it under the prompt cache key so the next turn can resume the
	// same internal slot.
	if account.Type == AccountTypeOAuth && promptCacheKey != "" {
		if turnState := strings.TrimSpace(resp.Header.Get("x-codex-turn-state")); turnState != "" {
			s.bindOpenAICompatSessionTurnState(ctx, c, account, promptCacheKey, turnState)
		}
	}

	// 9. Handle normal response
	// Upstream is always streaming; choose response format based on client preference.
	var result *OpenAIForwardResult
	var handleErr error
	if clientStream {
		result, handleErr = s.handleAnthropicStreamingResponse(resp, c, originalModel, billingModel, upstreamModel, startTime)
	} else {
		// Client wants JSON: buffer the streaming response and assemble a JSON reply.
		result, handleErr = s.handleAnthropicBufferedStreamingResponse(resp, c, originalModel, billingModel, upstreamModel, startTime)
	}

	// Propagate ServiceTier and ReasoningEffort to result for billing
	if handleErr == nil && result != nil {
		// 058 step 2: bind upstream response id under the prompt cache key
		// (continuation chain) and bind any new digest chain (cache key
		// reuse) so the next turn picks up where this one left off.
		if compatContinuationEnabled && promptCacheKey != "" && result.ResponseID != "" {
			s.bindOpenAICompatSessionResponseID(ctx, c, account, promptCacheKey, result.ResponseID)
		}
		if promptCacheKey != "" && anthropicDigestChain != "" {
			s.bindOpenAICompatAnthropicDigestPromptCacheKey(account, apiKeyID, anthropicDigestChain, promptCacheKey, anthropicMatchedDigestChain)
		}
		if responsesReq.ServiceTier != "" {
			st := responsesReq.ServiceTier
			result.ServiceTier = &st
		}
		if responsesReq.Reasoning != nil && responsesReq.Reasoning.Effort != "" {
			re := responsesReq.Reasoning.Effort
			result.ReasoningEffort = &re
		}
	}

	// Extract and save Codex usage snapshot from response headers (for OAuth accounts)
	if handleErr == nil && account.Type == AccountTypeOAuth {
		if snapshot := ParseCodexRateLimitHeaders(resp.Header); snapshot != nil {
			s.updateCodexUsageSnapshot(ctx, account.ID, snapshot)
		}
	}

	return result, handleErr
}

// ensureCodexOAuthInstructionsField guarantees reqBody["instructions"] is a
// string (possibly empty). Codex SSE upstream emits an unrecoverable
// "instructions: null" rejection if the field is absent or non-string.
func ensureCodexOAuthInstructionsField(reqBody map[string]any) {
	if reqBody == nil {
		return
	}
	if value, ok := reqBody["instructions"]; !ok || value == nil {
		reqBody["instructions"] = ""
		return
	}
	if _, ok := reqBody["instructions"].(string); !ok {
		reqBody["instructions"] = ""
	}
}

// handleAnthropicErrorResponse reads an upstream error and returns it in
// Anthropic error format.
func (s *OpenAIGatewayService) handleAnthropicErrorResponse(
	resp *http.Response,
	c *gin.Context,
	account *Account,
) (*OpenAIForwardResult, error) {
	return s.handleCompatErrorResponse(resp, c, account, writeAnthropicError)
}

// handleAnthropicBufferedStreamingResponse reads all Responses SSE events from
// the upstream streaming response, finds the terminal event (response.completed
// / response.incomplete / response.failed), converts the complete response to
// Anthropic Messages JSON format, and writes it to the client.
// This is used when the client requested stream=false but the upstream is always
// streaming.
func (s *OpenAIGatewayService) handleAnthropicBufferedStreamingResponse(
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	billingModel string,
	upstreamModel string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")

	finalResponse, usage, acc, err := s.readOpenAICompatBufferedTerminal(resp, "openai messages buffered", requestID)
	if err != nil {
		return nil, err
	}

	// If the upstream closed the stream without emitting a terminal event
	// (e.g. Codex EOFs mid-response for web_search or near an output cap),
	// synthesize a finalResponse from the accumulated delta content. Only
	// bail out if nothing was streamed at all — otherwise we'd drop a
	// partially-delivered answer that the client could still use.
	if finalResponse == nil {
		if acc.HasContent() {
			synthesized := &apicompat.ResponsesResponse{
				Status: "incomplete",
				Output: acc.BuildOutput(),
			}
			finalResponse = synthesized
			logger.L().Warn("openai messages buffered: synthesized terminal from accumulator (upstream EOF without terminal event)",
				zap.String("request_id", requestID),
			)
		} else {
			// Empty accumulator = upstream dropped before any delta. Use a
			// generic Anthropic-style message so the client body doesn't
			// leak our internal wording ("Upstream stream ended without a
			// terminal response event" is not something real Anthropic
			// would ever return and would fingerprint us as not-Anthropic).
			logger.L().Warn("openai messages buffered: upstream EOF without any delta",
				zap.String("request_id", requestID),
			)
			writeAnthropicError(c, http.StatusBadGateway, "api_error", "Internal server error")
			return nil, fmt.Errorf("upstream stream ended without terminal event")
		}
	}

	// When the terminal event has an empty output array, reconstruct from
	// accumulated delta events so the client receives the full content.
	acc.SupplementResponseOutput(finalResponse)

	anthropicResp := apicompat.ResponsesToAnthropic(finalResponse, originalModel)

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.JSON(http.StatusOK, anthropicResp)

	responseID := ""
	if finalResponse != nil {
		responseID = strings.TrimSpace(finalResponse.ID)
	}
	return &OpenAIForwardResult{
		RequestID:     requestID,
		ResponseID:    responseID,
		Usage:         usage,
		Model:         originalModel,
		BillingModel:  billingModel,
		UpstreamModel: upstreamModel,
		Stream:        false,
		Duration:      time.Since(startTime),
	}, nil
}

func isOpenAICompatResponsesTerminalEvent(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.completed", "response.done", "response.incomplete", "response.failed":
		return true
	default:
		return false
	}
}

func isOpenAICompatDoneSentinelLine(line string) bool {
	payload, ok := extractOpenAISSEDataLine(line)
	return ok && strings.TrimSpace(payload) == "[DONE]"
}

func (s *OpenAIGatewayService) readOpenAICompatBufferedTerminal(
	resp *http.Response,
	logPrefix string,
	requestID string,
) (*apicompat.ResponsesResponse, OpenAIUsage, *apicompat.BufferedResponseAccumulator, error) {
	acc := apicompat.NewBufferedResponseAccumulator()
	var usage OpenAIUsage
	if resp == nil || resp.Body == nil {
		return nil, usage, acc, errors.New("upstream response body is nil")
	}

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	streamInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	var timeoutCh <-chan time.Time
	var timeoutTimer *time.Timer
	resetTimeout := func() {
		if streamInterval <= 0 {
			return
		}
		if timeoutTimer == nil {
			timeoutTimer = time.NewTimer(streamInterval)
			timeoutCh = timeoutTimer.C
			return
		}
		if !timeoutTimer.Stop() {
			select {
			case <-timeoutTimer.C:
			default:
			}
		}
		timeoutTimer.Reset(streamInterval)
	}
	stopTimeout := func() {
		if timeoutTimer == nil {
			return
		}
		if !timeoutTimer.Stop() {
			select {
			case <-timeoutTimer.C:
			default:
			}
		}
	}
	resetTimeout()
	defer stopTimeout()

	type scanEvent struct {
		line string
		err  error
	}
	events := make(chan scanEvent, 16)
	done := make(chan struct{})
	go func() {
		defer close(events)
		for scanner.Scan() {
			select {
			case events <- scanEvent{line: scanner.Text()}:
			case <-done:
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case events <- scanEvent{err: err}:
			case <-done:
			}
		}
	}()
	defer close(done)

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return nil, usage, acc, nil
			}
			resetTimeout()
			if ev.err != nil {
				if !errors.Is(ev.err, context.Canceled) && !errors.Is(ev.err, context.DeadlineExceeded) {
					logger.L().Warn(logPrefix+": read error",
						zap.Error(ev.err),
						zap.String("request_id", requestID),
					)
				}
				return nil, usage, acc, ev.err
			}

			if isOpenAICompatDoneSentinelLine(ev.line) {
				return nil, usage, acc, nil
			}
			payload, ok := extractOpenAISSEDataLine(ev.line)
			if !ok || payload == "" {
				continue
			}

			var event apicompat.ResponsesStreamEvent
			if err := json.Unmarshal([]byte(payload), &event); err != nil {
				logger.L().Warn(logPrefix+": failed to parse event",
					zap.Error(err),
					zap.String("request_id", requestID),
				)
				continue
			}

			acc.ProcessEvent(&event)

			if isOpenAICompatResponsesTerminalEvent(event.Type) && event.Response != nil {
				if event.Response.Usage != nil {
					usage = copyOpenAIUsageFromResponsesUsage(event.Response.Usage)
				}
				return event.Response, usage, acc, nil
			}

		case <-timeoutCh:
			_ = resp.Body.Close()
			logger.L().Warn(logPrefix+": data interval timeout",
				zap.String("request_id", requestID),
				zap.Duration("interval", streamInterval),
			)
			return nil, usage, acc, fmt.Errorf("stream data interval timeout")
		}
	}
}

// handleAnthropicStreamingResponse reads Responses SSE events from upstream,
// converts each to Anthropic SSE events, and writes them to the client.
// When StreamKeepaliveInterval is configured, it uses a goroutine + channel
// pattern to send Anthropic ping events during periods of upstream silence,
// preventing proxy/client timeout disconnections.
func (s *OpenAIGatewayService) handleAnthropicStreamingResponse(
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	billingModel string,
	upstreamModel string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")

	// 5/8 codex audit #1+#2: WriteHeader(200) 延后到首个 meaningful event.
	// 原本立即写 200 → 上游空流时客户傻等 stream_data_interval_timeout
	// (默认 180s) 才知道, NewAPI 记成"成功 0 输出". 现在: header 没写时
	// 出错可走 502, 客户立刻知道没数据. 写 helper 防多次 WriteHeader.
	headerWritten := false
	writeStreamHeader := func() {
		if headerWritten {
			return
		}
		if s.responseHeaderFilter != nil {
			responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
		}
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(http.StatusOK)
		headerWritten = true
	}

	state := apicompat.NewResponsesEventToAnthropicState()
	state.Model = originalModel
	var usage OpenAIUsage
	responseID := ""
	var firstTokenMs *int
	firstChunk := true
	firstMeaningfulSeen := false // codex 5/8 #1: WriteHeader 直到见到 meaningful event
	clientDisconnected := false
	var disconnectedAt time.Time // codex 5/8 #3: drain after disconnect 上限

	// R29 v25 cctest 签名校验失败教训: 没 forward 累积的 metadata events
	// (message_start / content_block_start text/thinking / ping) 给客户,
	// 客户拿到的 SSE 流缺 message_start, cctest 校验"流必须以 message_start
	// 开头" → fail. 修法: 累积 metadata events 等 meaningful 一次性 flush.
	// maxPending 防上游 metadata flood 内存爆炸.
	var pendingEvents []apicompat.AnthropicStreamEvent
	const maxPendingEvents = 100

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	streamInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	var intervalTicker *time.Ticker
	if streamInterval > 0 {
		intervalTicker = time.NewTicker(streamInterval)
		defer intervalTicker.Stop()
	}
	var intervalCh <-chan time.Time
	if intervalTicker != nil {
		intervalCh = intervalTicker.C
	}

	// codex 5/8 #1+#2: first meaningful event timeout. 0 关.
	firstMeaningfulTimeout := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.FirstMeaningfulEventTimeoutSeconds > 0 {
		firstMeaningfulTimeout = time.Duration(s.cfg.Gateway.FirstMeaningfulEventTimeoutSeconds) * time.Second
	}
	var firstMeaningfulDeadlineCh <-chan time.Time
	var firstMeaningfulTimer *time.Timer
	if firstMeaningfulTimeout > 0 {
		firstMeaningfulTimer = time.NewTimer(firstMeaningfulTimeout)
		defer firstMeaningfulTimer.Stop()
		firstMeaningfulDeadlineCh = firstMeaningfulTimer.C
	}

	// codex 5/8 #3: drain after client disconnect 上限. 0 关.
	drainMax := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.DrainAfterClientDisconnectMaxSeconds > 0 {
		drainMax = time.Duration(s.cfg.Gateway.DrainAfterClientDisconnectMaxSeconds) * time.Second
	}

	// resultWithUsage builds the final result snapshot.
	resultWithUsage := func() *OpenAIForwardResult {
		return &OpenAIForwardResult{
			RequestID:     requestID,
			ResponseID:    responseID,
			Usage:         usage,
			Model:         originalModel,
			BillingModel:  billingModel,
			UpstreamModel: upstreamModel,
			Stream:        true,
			Duration:      time.Since(startTime),
			FirstTokenMs:  firstTokenMs,
		}
	}

	// processDataLine handles a single "data: ..." SSE line from upstream.
	processDataLine := func(payload string) bool {
		if firstChunk {
			firstChunk = false
			ms := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &ms
		}

		var event apicompat.ResponsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			logger.L().Warn("openai messages stream: failed to parse event",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
			return false
		}

		// 仅按兼容转换器支持的终止事件提取 usage，避免无意扩大事件语义。
		isTerminalEvent := isOpenAICompatResponsesTerminalEvent(event.Type)
		if isTerminalEvent && event.Response != nil {
			// 058 step 2: capture upstream response id (resp_xxx) so the
			// continuation chain can attach it as previous_response_id on
			// the next turn. Anthropic-facing id stays synthesised — only
			// internal binding sees the upstream value.
			if id := strings.TrimSpace(event.Response.ID); id != "" {
				responseID = id
			}
			if event.Response.Usage != nil {
				usage = copyOpenAIUsageFromResponsesUsage(event.Response.Usage)
			}
		}

		// Convert to Anthropic events
		events := apicompat.ResponsesEventToAnthropicEvents(&event, state)

		// codex 5/8 #1 + R29 修: 见到首个 meaningful event 才 WriteHeader(200),
		// 但**累积** metadata events (message_start / content_block_start /
		// ping), 触发 meaningful 时一次性 flush. 之前 v25 的 bug 是直接
		// drop metadata events 导致 cctest 签名校验失败 (流缺 message_start).
		if !firstMeaningfulSeen {
			// 累积所有 events (含 metadata + 当次的 meaningful)
			pendingEvents = append(pendingEvents, events...)

			// maxPendingEvents 防上游 metadata flood 内存爆炸. 触发后当成
			// 空流处理 — 让 first_meaningful_event_timeout 自然 fire 走 502.
			if len(pendingEvents) > maxPendingEvents {
				logger.L().Warn("openai messages stream: pending metadata events overflow, dropping (treat as empty stream)",
					zap.String("request_id", requestID),
					zap.Int("max_pending", maxPendingEvents),
				)
				pendingEvents = nil
				return isTerminalEvent
			}

			// 检查 events 里是否有 meaningful
			seenMeaningful := false
			for _, evt := range events {
				if isMeaningfulAnthropicEvent(evt) {
					seenMeaningful = true
					break
				}
			}
			if !seenMeaningful {
				// 还没真实数据, 累积继续等. caller 看 isTerminalEvent 决定终止.
				return isTerminalEvent
			}

			// 触发! WriteHeader + 一次性 flush 所有累积的 events 给客户.
			firstMeaningfulSeen = true
			writeStreamHeader()
			if firstMeaningfulTimer != nil {
				firstMeaningfulTimer.Stop()
			}

			if !clientDisconnected {
				for _, evt := range pendingEvents {
					sse, err := apicompat.ResponsesAnthropicEventToSSE(evt)
					if err != nil {
						logger.L().Warn("openai messages stream: failed to marshal pending event",
							zap.Error(err),
							zap.String("request_id", requestID),
						)
						continue
					}
					if _, err := fmt.Fprint(c.Writer, sse); err != nil {
						clientDisconnected = true
						disconnectedAt = time.Now()
						logger.L().Info("openai messages stream: client disconnected during initial flush",
							zap.String("request_id", requestID),
						)
						break
					}
				}
				if !clientDisconnected {
					c.Writer.Flush()
				}
			}
			pendingEvents = nil // 释放, 后续走 normal 路径
			return isTerminalEvent
		}

		// 此时 header 已写, 正常 forward
		if !clientDisconnected {
			for _, evt := range events {
				sse, err := apicompat.ResponsesAnthropicEventToSSE(evt)
				if err != nil {
					logger.L().Warn("openai messages stream: failed to marshal event",
						zap.Error(err),
						zap.String("request_id", requestID),
					)
					continue
				}
				if _, err := fmt.Fprint(c.Writer, sse); err != nil {
					clientDisconnected = true
					disconnectedAt = time.Now()
					logger.L().Info("openai messages stream: client disconnected, continuing to drain upstream for billing",
						zap.String("request_id", requestID),
					)
					break
				}
			}
		}
		if len(events) > 0 && !clientDisconnected {
			c.Writer.Flush()
		}
		return isTerminalEvent
	}

	// finalizeStream sends any remaining Anthropic events and returns the result.
	finalizeStream := func() (*OpenAIForwardResult, error) {
		if finalEvents := apicompat.FinalizeResponsesAnthropicStream(state); len(finalEvents) > 0 && !clientDisconnected {
			for _, evt := range finalEvents {
				sse, err := apicompat.ResponsesAnthropicEventToSSE(evt)
				if err != nil {
					continue
				}
				if _, err := fmt.Fprint(c.Writer, sse); err != nil {
					clientDisconnected = true
					logger.L().Info("openai messages stream: client disconnected during final flush",
						zap.String("request_id", requestID),
					)
					break
				}
			}
			if !clientDisconnected {
				c.Writer.Flush()
			}
		}
		return resultWithUsage(), nil
	}

	// handleScanErr logs scanner errors if meaningful.
	handleScanErr := func(err error) {
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logger.L().Warn("openai messages stream: read error",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
		}
	}
	missingTerminalErr := func() (*OpenAIForwardResult, error) {
		return resultWithUsage(), fmt.Errorf("stream usage incomplete: missing terminal event")
	}

	// ── Determine keepalive interval ──
	keepaliveInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}

	// ── No keepalive: fast synchronous path (no goroutine overhead) ──
	if streamInterval <= 0 && keepaliveInterval <= 0 {
		for scanner.Scan() {
			line := scanner.Text()
			if isOpenAICompatDoneSentinelLine(line) {
				return missingTerminalErr()
			}
			payload, ok := extractOpenAISSEDataLine(line)
			if !ok {
				continue
			}
			if processDataLine(payload) {
				return finalizeStream()
			}
		}
		if err := scanner.Err(); err != nil {
			handleScanErr(err)
			return resultWithUsage(), fmt.Errorf("stream usage incomplete: %w", err)
		}
		return missingTerminalErr()
	}

	// ── With keepalive: goroutine + channel + select ──
	type scanEvent struct {
		line string
		err  error
	}
	events := make(chan scanEvent, 16)
	done := make(chan struct{})
	var lastReadAt int64
	atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
	sendEvent := func(ev scanEvent) bool {
		select {
		case events <- ev:
			return true
		case <-done:
			return false
		}
	}
	go func() {
		defer close(events)
		for scanner.Scan() {
			atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
			if !sendEvent(scanEvent{line: scanner.Text()}) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = sendEvent(scanEvent{err: err})
		}
	}()
	defer close(done)

	var keepaliveTicker *time.Ticker
	if keepaliveInterval > 0 {
		keepaliveTicker = time.NewTicker(keepaliveInterval)
		defer keepaliveTicker.Stop()
	}
	var keepaliveCh <-chan time.Time
	if keepaliveTicker != nil {
		keepaliveCh = keepaliveTicker.C
	}
	lastDataAt := time.Now()

	for {
		// codex 5/8 #3: drain max — client 断开后超过 drainMax 强制 abort.
		// 防"客户走了上游还跑 180s 占账号并发"的浪费.
		if clientDisconnected && drainMax > 0 && !disconnectedAt.IsZero() &&
			time.Since(disconnectedAt) > drainMax {
			logger.L().Info("openai messages stream: drain after disconnect exceeded max, aborting",
				zap.String("request_id", requestID),
				zap.Duration("drain_max", drainMax),
				zap.Duration("disconnect_age", time.Since(disconnectedAt)),
			)
			return resultWithUsage(), fmt.Errorf("drain after disconnect exceeded max %s", drainMax)
		}
		select {
		case ev, ok := <-events:
			if !ok {
				// Upstream closed
				return missingTerminalErr()
			}
			if ev.err != nil {
				handleScanErr(ev.err)
				return resultWithUsage(), fmt.Errorf("stream usage incomplete: %w", ev.err)
			}
			lastDataAt = time.Now()
			line := ev.line
			if isOpenAICompatDoneSentinelLine(line) {
				return missingTerminalErr()
			}
			payload, ok := extractOpenAISSEDataLine(line)
			if !ok {
				continue
			}
			if processDataLine(payload) {
				return finalizeStream()
			}

		case <-intervalCh:
			lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
			if time.Since(lastRead) < streamInterval {
				continue
			}
			if clientDisconnected {
				return resultWithUsage(), fmt.Errorf("stream usage incomplete after timeout")
			}
			logger.L().Warn("openai messages stream: data interval timeout",
				zap.String("request_id", requestID),
				zap.String("model", originalModel),
				zap.Duration("interval", streamInterval),
			)
			return resultWithUsage(), fmt.Errorf("stream data interval timeout")

		case <-firstMeaningfulDeadlineCh:
			// codex 5/8 #2: 首个 meaningful event 超时. 如果还没 WriteHeader,
			// 我们能干净返 error 让 caller 改 status (502). 之前是傻等
			// stream_data_interval_timeout 默认 180s 才发现空流.
			if firstMeaningfulSeen {
				// 已经见过 meaningful, 此 timer 残留 (理论上 Stop 了不会到这).
				continue
			}
			logger.L().Warn("openai messages stream: first meaningful event timeout",
				zap.String("request_id", requestID),
				zap.String("model", originalModel),
				zap.Duration("timeout", firstMeaningfulTimeout),
				zap.Bool("header_written", headerWritten),
			)
			return resultWithUsage(), fmt.Errorf("first meaningful event timeout after %s", firstMeaningfulTimeout)

		case <-keepaliveCh:
			if !shouldEmitKeepalivePing(clientDisconnected, firstMeaningfulSeen, time.Since(lastDataAt), keepaliveInterval) {
				continue
			}
			// Send Anthropic-format ping event
			if _, err := fmt.Fprint(c.Writer, "event: ping\ndata: {\"type\":\"ping\"}\n\n"); err != nil {
				// Client disconnected
				logger.L().Info("openai messages stream: client disconnected during keepalive",
					zap.String("request_id", requestID),
				)
				clientDisconnected = true
				continue
			}
			c.Writer.Flush()
		}
	}
}

// shouldEmitKeepalivePing decides whether the openai-messages stream loop
// should emit an Anthropic ping event on this keepalive tick.
//
// codex 5/8 audit caught this: gin's ResponseWriter.Write implicitly
// commits HTTP 200 on the first byte. If we send a ping before
// firstMeaningfulSeen=true, the client sees a "200 stream" carrying only
// ping events and no real content — and the firstMeaningfulEventTimeout
// fallback (which would otherwise return 502) is bypassed because by then
// the status is already locked. The client then sits there until upstream
// closes the connection, looking like a successful empty stream.
//
// All four conditions must hold: client still connected, first meaningful
// event already seen (status committed legitimately), enough quiet time
// since last data event.
func shouldEmitKeepalivePing(clientDisconnected, firstMeaningfulSeen bool, sinceLastData, interval time.Duration) bool {
	if clientDisconnected {
		return false
	}
	if !firstMeaningfulSeen {
		return false
	}
	if sinceLastData < interval {
		return false
	}
	return true
}

// writeAnthropicError writes an error response in Anthropic Messages API format.
func writeAnthropicError(c *gin.Context, statusCode int, errType, message string) {
	c.JSON(statusCode, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

func copyOpenAIUsageFromResponsesUsage(usage *apicompat.ResponsesUsage) OpenAIUsage {
	if usage == nil {
		return OpenAIUsage{}
	}
	result := OpenAIUsage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
	}
	if usage.InputTokensDetails != nil {
		result.CacheReadInputTokens = usage.InputTokensDetails.CachedTokens
	}
	return result
}

// isMeaningfulAnthropicEvent codex 5/8 audit #1: 区分真实数据 vs 元数据.
//   真实 (返 true): content_block_delta (text/thinking/input_json/signature),
//                  content_block_start tool_use/server_tool_use,
//                  message_delta (含 usage), message_stop, error.
//   元数据 (返 false): message_start, ping, content_block_start text/thinking
//                    (空块, 还没 token), content_block_stop.
//
// 用 WriteHeader(200) gating: 没收到 meaningful 之前不写 200, 让上游空流
// timeout 走 502 错误返回, 而不是空 200 等 180s.
func isMeaningfulAnthropicEvent(e apicompat.AnthropicStreamEvent) bool {
	switch e.Type {
	case "content_block_delta":
		return true
	case "content_block_start":
		if e.ContentBlock != nil {
			t := e.ContentBlock.Type
			if t == "tool_use" || t == "server_tool_use" {
				return true
			}
		}
		return false
	case "message_delta":
		// 含 usage / stop_reason — 算 meaningful (上游有真实 terminal)
		return true
	case "message_stop":
		return true
	case "error":
		// 上游主动 error event, 算 meaningful (有真实信号要 forward)
		return true
	default:
		return false
	}
}
