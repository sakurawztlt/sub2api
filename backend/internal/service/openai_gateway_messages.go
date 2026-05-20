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
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
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

	// 5/9 codex audit: defense-in-depth. gcr 入口已经 canonicalize 过
	// cache_control.ttl ("5min" → "5m" 等), 这里再扫一遍防 gcr 旁路 (备用
	// 环境 cctest 直连 k8s NodePort / admin 调试). 已 canonical 时 no-op.
	body = CanonicalizeAnthropicCacheControlTTLInBody(body)

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
		// 5/10 codex P3 #5: namespace derived keys per account+apiKey so two
		// sub2api accounts hitting same content don't collide on upstream
		// OpenAI prefix cache. Skipped for client-supplied keys (already
		// scoped by client) and for cached digest matches that came from
		// our own namespaced storage (re-prefixing would double-prefix).
		if promptCacheKey != "" && anthropicMatchedDigestChain == "" {
			promptCacheKey = applyOpenAICompatPromptCacheKeyNamespace(account, apiKeyID, promptCacheKey)
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
		// 5/10 codex P3 #5 验证用: env on 时打明文 promptCacheKey 看 namespace
		// 前缀 a{accountID}k{apiKeyID}_ 是否生效. backup 108 临时调试, 验完关.
		if os.Getenv("SUB2API_DEBUG_PROMPT_CACHE_KEY") == "1" {
			logFields = append(logFields,
				zap.String("compat_prompt_cache_key_plain", promptCacheKey),
			)
		}
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
	// codex round 11al: 记 time-to-headers (从 Do() 到 resp 返回的耗时).
	// 大上下文请求排查时跟 first_token_ms / first_meaningful_ms 一起看,
	// 区分"上游 HTTP 慢" vs "上游处理慢".
	upstreamReqStart := time.Now()
	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	timeToHeadersMs := int(time.Since(upstreamReqStart).Milliseconds())
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
		// codex 2026-05-16: account-aware variant — 404 on OAuth account
		// triggers cross-account failover (Codex backend 单账号 scoped 不可用).
		if s.shouldFailoverOpenAIUpstreamResponseForAccount(resp.StatusCode, upstreamMsg, respBody, account) {
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
	// codex round 11al: 装入 forensics meta 一次性传给 streaming/buffered
	// 处理函数, 让 first_meaningful_timeout 跟 large_context summary log
	// 能写出 account_id/type / proxy_hash / messages_count / cache key
	// hash / continuation 状态 / time_to_headers_ms.
	streamMeta := computeStreamReqMeta(account, body, promptCacheKey, previousResponseID, compatTurnState, proxyURL, timeToHeadersMs)
	// codex round36 fu55 (2026-05-20): annotate the turn_state cache
	// lookup for the large_context_request summary log. Only enum / bool —
	// never the raw session id (codex privacy constraint). Set unconditionally
	// (not gated on shouldAutoInjectPromptCacheKeyForCompat) so the
	// "lookup never attempted" case is recorded as source=none + hit=false,
	// distinguishing it from "tried + missed".
	streamMeta.TurnStateKeySource = describeTurnStateKeySource(c, promptCacheKey)
	streamMeta.TurnStateCacheHit = strings.TrimSpace(compatTurnState) != ""
	streamMeta.HasSessionHeader = c != nil && strings.TrimSpace(c.GetHeader("X-Claude-Code-Session-Id")) != ""
	streamMeta.HasMetadataSession = hasMetadataUserSessionID(body)
	if clientStream {
		result, handleErr = s.handleAnthropicStreamingResponse(resp, c, originalModel, billingModel, upstreamModel, startTime, len(body), streamMeta)
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
		// codex 2026-05-16 round5 #2457: bill from post-policy responsesBody,
		// not the pre-policy responsesReq.ServiceTier — see same change in
		// openai_gateway_chat_completions.go for the rationale (policy may
		// have stripped service_tier before sending upstream, billing must
		// not still charge the original priority/flex).
		if billingTier := extractOpenAIServiceTierFromBody(responsesBody); billingTier != nil && *billingTier != "" {
			st := *billingTier
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

	finalResponse, usage, acc, err := s.readOpenAICompatBufferedTerminal(c.Request.Context(), resp, "openai messages buffered", requestID)
	if err != nil {
		// 5/10 codex audit: buffered path 网络层 stream 读取错误 (EOF/reset/
		// TLS handshake/timeout) 在 !c.Writer.Written() 时 (buffered 路径
		// 一定没写过 SSE) 返 BreakSticky failover 让 handler 重选账号 retry.
		// 业务错误 (JSON parse / 4xx event 等) 走原 path 客户 502.
		if IsUpstreamNetworkError(err) && !c.Writer.Written() {
			return nil, &UpstreamFailoverError{
				StatusCode:  http.StatusBadGateway,
				BreakSticky: true,
				Reason:      "buffered_stream_read_error",
			}
		}
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
			// 5/10 codex audit: 上游 200 但空 stream 无 [DONE] (clean close
			// 无 EOF 错误) = 单账号上游异常, !c.Writer.Written() 时让 handler
			// 重选账号 retry. 不在 service 层 writeAnthropicError 否则阻断
			// failover (handler 看到客户响应已写就不重试).
			//
			// 已 WriteHeader 的极端兜底维持原行为 (buffered 路径理论走不到
			// 但防御性).
			logger.L().Warn("openai messages buffered: upstream EOF without any delta",
				zap.String("request_id", requestID),
			)
			if !c.Writer.Written() {
				return nil, &UpstreamFailoverError{
					StatusCode:  http.StatusBadGateway,
					BreakSticky: true,
					Reason:      "buffered_empty_stream",
				}
			}
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

// readOpenAICompatBufferedTerminal reads the upstream SSE stream into
// memory, builds a buffered terminal response, and returns when the
// upstream closes the connection or one of the buffered timeouts fires.
//
// 2026-05-15 codex round 11ai: ctx parameter added so the buffered
// first-meaningful timeout can be tightened for large-context requests
// (msgs>100 or body>800KB) per IsLargeContextCtx.
func (s *OpenAIGatewayService) readOpenAICompatBufferedTerminal(
	ctx context.Context,
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

	// 2026-05-13 codex round 11i: buffered 非流总超时 + 首个 meaningful event
	// 超时. 防 stream_data_interval_timeout 心跳能拖到 9 分钟的情况.
	//
	// total: 整请求硬上限. 启用后无论上游发什么 (terminal / 心跳 / 数据), 到
	//        时立刻 close + return error 让 caller failover or 504.
	// first_meaningful: 首个**非心跳事件** (terminal 或业务事件) 未达到时
	//                   超时. 心跳事件不算 (会被 ProcessEvent 忽略).
	var (
		totalTimeoutCh    <-chan time.Time
		totalTimeoutTimer *time.Timer
		firstMeaningTimer *time.Timer
		firstMeaningCh    <-chan time.Time
		firstMeaningSeen  bool
	)
	if s.cfg != nil && s.cfg.Gateway.BufferedTotalTimeout > 0 {
		totalTimeoutTimer = time.NewTimer(time.Duration(s.cfg.Gateway.BufferedTotalTimeout) * time.Second)
		totalTimeoutCh = totalTimeoutTimer.C
		defer totalTimeoutTimer.Stop()
	}
	// 2026-05-15 codex round 11ai: large-context requests use the
	// tighter LargeRequestFirstMeaningfulTimeout to fail fast instead
	// of waiting full 60s. Default 0 = use normal BufferedFirstMeaningfulTimeout.
	firstMeaningfulTimeoutSec := 0
	if s.cfg != nil {
		firstMeaningfulTimeoutSec = s.cfg.Gateway.BufferedFirstMeaningfulTimeout
		if IsLargeContextCtx(ctx) && s.cfg.Gateway.LargeRequestFirstMeaningfulTimeout > 0 {
			firstMeaningfulTimeoutSec = s.cfg.Gateway.LargeRequestFirstMeaningfulTimeout
		}
	}
	if firstMeaningfulTimeoutSec > 0 {
		firstMeaningTimer = time.NewTimer(time.Duration(firstMeaningfulTimeoutSec) * time.Second)
		firstMeaningCh = firstMeaningTimer.C
		defer firstMeaningTimer.Stop()
	}

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

	// codex round23 / upstream cc5328c49: terminal events may arrive as
	// `event: response.completed\ndata: {...}` (no type field in data).
	// openAICompatSSEFrameParser buffers event+data lines per SSE frame so
	// we recognize that form too. See openai_gateway_service.go for the
	// parser/helpers and a long-form comment.
	var parser openAICompatSSEFrameParser
	processFrame := func(frame openAICompatSSEFrame) (terminalReturn bool, retResp *apicompat.ResponsesResponse, retErr error) {
		payload := openAICompatPayloadWithEventType(frame.Data, frame.EventType)
		if strings.TrimSpace(payload) == "" {
			return false, nil, nil
		}
		var event apicompat.ResponsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			logger.L().Warn(logPrefix+": failed to parse event",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
			return false, nil, nil
		}
		acc.ProcessEvent(&event)
		if !firstMeaningSeen && firstMeaningTimer != nil {
			firstMeaningSeen = true
			if !firstMeaningTimer.Stop() {
				select {
				case <-firstMeaningTimer.C:
				default:
				}
			}
		}
		if isOpenAICompatResponsesTerminalEvent(event.Type) && event.Response != nil {
			if event.Response.Usage != nil {
				usage = copyOpenAIUsageFromResponsesUsage(event.Response.Usage)
			}
			return true, event.Response, nil
		}
		return false, nil, nil
	}

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				// Upstream closed. Flush any pending parser state — the
				// final SSE frame may have arrived without a trailing
				// blank line.
				if frame, hasFrame := parser.Finish(); hasFrame {
					if isReturn, resp, err := processFrame(frame); isReturn {
						return resp, usage, acc, err
					}
				}
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
			frame, hasFrame := parser.AddLine(ev.line)
			if !hasFrame {
				continue
			}
			payload := openAICompatPayloadWithEventType(frame.Data, frame.EventType)
			if strings.TrimSpace(payload) == "" {
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

			// 2026-05-13 codex round 11i: any business event (not just terminal)
			// counts as "first meaningful" — stop the first-meaningful timer.
			// Heartbeat events parse as ResponsesStreamEvent but have no
			// terminal Type; an arbitrary processed event still satisfies
			// "upstream is doing real work". Conservative interpretation:
			// the timer is for catching "upstream silent / heartbeat-only"
			// scenarios, so we trip it OFF on any successfully-parsed event.
			if !firstMeaningSeen && firstMeaningTimer != nil {
				firstMeaningSeen = true
				if !firstMeaningTimer.Stop() {
					select {
					case <-firstMeaningTimer.C:
					default:
					}
				}
			}

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

		case <-totalTimeoutCh:
			_ = resp.Body.Close()
			logger.L().Warn(logPrefix+": buffered total timeout (codex round 11i)",
				zap.String("request_id", requestID),
				zap.Int("seconds", s.cfg.Gateway.BufferedTotalTimeout),
			)
			return nil, usage, acc, fmt.Errorf("buffered total timeout")

		case <-firstMeaningCh:
			if firstMeaningSeen {
				continue
			}
			_ = resp.Body.Close()
			logger.L().Warn(logPrefix+": buffered first meaningful event timeout (codex round 11i)",
				zap.String("request_id", requestID),
				zap.Int("seconds", s.cfg.Gateway.BufferedFirstMeaningfulTimeout),
			)
			return nil, usage, acc, fmt.Errorf("buffered first meaningful timeout")
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
	inboundBodyLen int,
	meta streamReqMeta,
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
	// 2026-05-12 cctest profile 项 5 (codex audit): message_start.usage.input_tokens
	// 不能是 0, 用客户请求 body 长度粗估 token (bytes/4). 真 Claude 这里报
	// 5K-11K (cctest system prompt 估算) 不报 0.
	//
	// 2026-05-13 codex round 6 capture diff: 老版本依赖 c.Request.ContentLength,
	// 但 gcr 转发 sub2api 用 chunked transfer encoding → ContentLength=-1 → 整段
	// gate 跳过, message_start.usage.input_tokens=0 暴露. 改成 caller (ForwardAsAnthropic)
	// 传 len(body) 进来, body 已 buffer 完整必有值. ContentLength 仍作 fallback
	// 防 caller 漏传.
	preflightBytes := inboundBodyLen
	if preflightBytes <= 0 && c.Request != nil && c.Request.ContentLength > 0 {
		preflightBytes = int(c.Request.ContentLength)
	}
	if preflightBytes > 0 {
		state.SetPreflightInputEstimate(preflightBytes)
	}
	var usage OpenAIUsage
	responseID := ""
	var firstTokenMs *int
	var firstMeaningfulMs *int // codex round 11ak: time-to-first meaningful event
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
			RequestID:         requestID,
			ResponseID:        responseID,
			Usage:             usage,
			Model:             originalModel,
			BillingModel:      billingModel,
			UpstreamModel:     upstreamModel,
			Stream:            true,
			Duration:          time.Since(startTime),
			FirstTokenMs:      firstTokenMs,
			FirstMeaningfulMs: firstMeaningfulMs,
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
			// codex round 11ak: record time-to-first meaningful for forensics.
			fmMs := int(time.Since(startTime).Milliseconds())
			firstMeaningfulMs = &fmMs
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
		// codex round 11ak (2026-05-15): large-context cache realism summary.
		// 上游 (OpenAI Responses) usage.cached_input_tokens 是真 cache hit;
		// gcr X-GCR-Estimated-Tokens 是 gcr 这边估出来传给 NewAPI 合成 cache_read
		// 的依据. 如果两者差距大 (e.g. gcr_estimate=200000 但 upstream_cached=5000)
		// 说明上游实际没复用上下文 — codex 警告"296k 读缓存只是账面数字". 配合
		// firstMeaningfulMs 一起诊断深上下文的真实性能.
		ctxFinal := c.Request.Context()
		if IsLargeContextCtx(ctxFinal) {
			cliReqIDFinal, _ := ctxFinal.Value(ctxkey.ClientRequestID).(string)
			ftMsFinal := 0
			if firstTokenMs != nil {
				ftMsFinal = *firstTokenMs
			}
			fmMsFinal := 0
			if firstMeaningfulMs != nil {
				fmMsFinal = *firstMeaningfulMs
			}
			accIDFinal := int64(0)
			accNameFinal := ""
			accPlatFinal := ""
			accTypeFinal := ""
			if meta.Account != nil {
				accIDFinal = meta.Account.ID
				accNameFinal = meta.Account.Name
				accPlatFinal = meta.Account.Platform
				accTypeFinal = string(meta.Account.Type)
			}
			logger.L().Info("openai messages stream: large_context_request summary",
				zap.String("request_id", requestID),
				zap.String("client_request_id", cliReqIDFinal),
				zap.String("gcr_request_id", c.Request.Header.Get("X-GCR-Request-Id")),
				zap.String("newapi_request_id", c.Request.Header.Get("X-Newapi-Request-Id")),
				zap.String("oneapi_request_id", c.Request.Header.Get("X-Oneapi-Request-Id")),
				zap.String("model", originalModel),
				zap.Int("inbound_body_len", inboundBodyLen),
				zap.String("gcr_depth_bucket", c.Request.Header.Get("X-GCR-Depth-Bucket")),
				zap.String("gcr_estimated_tokens", c.Request.Header.Get("X-GCR-Estimated-Tokens")),
				zap.Int("first_token_ms", ftMsFinal),
				zap.Int("first_meaningful_ms", fmMsFinal),
				zap.Int("upstream_cached_input_tokens", state.RawCachedInputTokens),
				zap.Int("upstream_total_input_tokens", state.RawTotalInputTokens),
				zap.Duration("total_duration", time.Since(startTime)),
				// 11al: account / proxy / continuation state
				zap.Int64("account_id", accIDFinal),
				zap.String("account_name", accNameFinal),
				zap.String("account_platform", accPlatFinal),
				zap.String("account_type", accTypeFinal),
				zap.String("proxy_hash", meta.ProxyHash),
				zap.Int("messages_count", meta.MessagesCount),
				zap.String("prompt_cache_key_sha", meta.PromptCacheKeySha256),
				zap.Bool("has_previous_response_id", meta.HasPreviousResponseID),
				zap.Bool("has_turn_state", meta.HasTurnState),
				// codex round36 fu55 (2026-05-20): observability for the
				// fu54 turn_state cache. turn_state_hit is the INBOUND cache
				// lookup (sub2api side) — DIFFERENT from has_turn_state above
				// (which is the upstream-side response header). Grep these
				// together with has_turn_state to verify fu54 is effective:
				// source=session_header + turn_state_hit=true → fu54 working.
				zap.String("turn_state_key_source", meta.TurnStateKeySource),
				zap.Bool("turn_state_hit", meta.TurnStateCacheHit),
				zap.Bool("has_session_header", meta.HasSessionHeader),
				zap.Bool("has_metadata_session", meta.HasMetadataSession),
				zap.Int("time_to_headers_ms", meta.TimeToHeadersMs),
			)
			// codex round31 fu50 (2026-05-19): explicit warn when the
			// account-side cache accounting (gcr X-GCR-Estimated-Tokens,
			// which NewAPI translates into a cache_read figure for the
			// client) shows a large request but the upstream OpenAI
			// Responses API reported cached_input_tokens=0. Means the
			// upstream actually reprocessed the entire prompt — the
			// user-visible "X tokens read from cache" is purely book-
			// keeping. Pair with first_meaningful_ms to confirm the
			// perceived slowness is real upstream work, not a sub2api
			// stall.
			//
			// Threshold: any large-context request (IsLargeContextCtx
			// already gates us here) with upstream cached=0 is worth
			// a distinct log line. The Info above stays as the base
			// summary; this Warn surfaces only the mismatch case so
			// ops grep `book_cache_hit_upstream_miss` directly.
			if state.RawCachedInputTokens == 0 {
				logger.L().Warn("openai messages stream: book_cache_hit_upstream_miss (账面缓存命中但上游未命中)",
					zap.String("request_id", requestID),
					zap.String("client_request_id", cliReqIDFinal),
					zap.String("gcr_request_id", c.Request.Header.Get("X-GCR-Request-Id")),
					zap.String("newapi_request_id", c.Request.Header.Get("X-Newapi-Request-Id")),
					zap.String("oneapi_request_id", c.Request.Header.Get("X-Oneapi-Request-Id")),
					zap.String("gcr_estimated_tokens", c.Request.Header.Get("X-GCR-Estimated-Tokens")),
					zap.Int("upstream_total_input_tokens", state.RawTotalInputTokens),
					zap.Int("upstream_cached_input_tokens", state.RawCachedInputTokens),
					zap.Int("first_token_ms", ftMsFinal),
					zap.Int("first_meaningful_ms", fmMsFinal),
					zap.Bool("has_previous_response_id", meta.HasPreviousResponseID),
					zap.Bool("has_turn_state", meta.HasTurnState),
					zap.Int("messages_count", meta.MessagesCount),
					zap.Duration("total_duration", time.Since(startTime)),
				)
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
	// codex round23 / upstream cc5328c49 fu40: processFrame is the new
	// frame-aware entry point — patches `type` into payloads that arrived
	// via `event: <name>` form (no type field in data). Wraps the existing
	// processDataLine so all the local first-meaningful / billing / cache
	// / tool-fix logic still runs unchanged.
	processFrame := func(frame openAICompatSSEFrame) bool {
		payload := openAICompatPayloadWithEventType(frame.Data, frame.EventType)
		return processDataLine(payload)
	}

	// 5/9 codex audit #2: stream 层错误 (unexpected EOF / scanner err / 上游
	// 断流没 [DONE]) 在客户响应**还没写 header** 时, 返 BreakSticky failover
	// 让 handler 重选别的账号 retry. 之前直接 fmt.Errorf 走 handler 路径 2 →
	// 直接客户 502, 没机会换账号. 已经 WriteHeader 后不能 retry (客户已收
	// 200 + 部分 SSE), 维持原行为.
	//
	// 这跟 first_meaningful_event_timeout 不同: timeout 是"上游真没出 token,
	// thinking 长任务可能误判 retry 烧账号", 用户慎重决定不动. EOF / scan err
	// 是上游单账号真挂了 (TCP / TLS 层断), retry 换账号大概率成功.
	streamFailoverIfNoHeader := func(orig error) error {
		if !headerWritten {
			return &UpstreamFailoverError{
				StatusCode:  http.StatusBadGateway,
				BreakSticky: true,
			}
		}
		return orig // 已写 header, 不能 retry, 返原 error 维持原行为
	}

	// ── Determine keepalive interval ──
	keepaliveInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}

	// ── No keepalive: fast synchronous path (no goroutine overhead) ──
	if streamInterval <= 0 && keepaliveInterval <= 0 {
		var parser openAICompatSSEFrameParser
		for scanner.Scan() {
			line := scanner.Text()
			if isOpenAICompatDoneSentinelLine(line) {
				return missingTerminalErr()
			}
			frame, hasFrame := parser.AddLine(line)
			if !hasFrame {
				continue
			}
			if processFrame(frame) {
				return finalizeStream()
			}
		}
		if err := scanner.Err(); err != nil {
			handleScanErr(err)
			origErr := fmt.Errorf("stream usage incomplete: %w", err)
			return resultWithUsage(), streamFailoverIfNoHeader(origErr)
		}
		// Upstream closed cleanly — flush any pending parser state in case
		// the final terminal event arrived without a trailing blank-line
		// boundary (codex round23: this is the form that was previously
		// silently dropped, causing missingTerminalErr).
		if frame, hasFrame := parser.Finish(); hasFrame {
			if strings.TrimSpace(frame.Data) == "[DONE]" {
				return missingTerminalErr()
			}
			if processFrame(frame) {
				return finalizeStream()
			}
		}
		// channel closed without [DONE] sentinel = upstream truncation
		if !headerWritten {
			return resultWithUsage(), &UpstreamFailoverError{
				StatusCode: http.StatusBadGateway, BreakSticky: true,
			}
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
	// codex round23 fu40: parser shared across all events in this loop.
	var parser openAICompatSSEFrameParser

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
				// Upstream closed without [DONE] = truncation. Try failover
				// if the client hasn't seen any bytes yet.
				//
				// codex round23 fu40: flush pending parser state first —
				// the final terminal event may have arrived without a
				// trailing blank-line boundary and would otherwise be
				// silently dropped (the bug PR #2530 fixed).
				if frame, hasFrame := parser.Finish(); hasFrame {
					if strings.TrimSpace(frame.Data) == "[DONE]" {
						// fallthrough to truncation handling below
					} else if processFrame(frame) {
						return finalizeStream()
					}
				}
				if !headerWritten {
					return resultWithUsage(), &UpstreamFailoverError{
						StatusCode: http.StatusBadGateway, BreakSticky: true,
					}
				}
				return missingTerminalErr()
			}
			if ev.err != nil {
				handleScanErr(ev.err)
				origErr := fmt.Errorf("stream usage incomplete: %w", ev.err)
				return resultWithUsage(), streamFailoverIfNoHeader(origErr)
			}
			lastDataAt = time.Now()
			line := ev.line
			if isOpenAICompatDoneSentinelLine(line) {
				return missingTerminalErr()
			}
			frame, hasFrame := parser.AddLine(line)
			if !hasFrame {
				continue
			}
			if processFrame(frame) {
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
				zap.Bool("header_written", headerWritten),
				// codex round 11ak forensics 字段
				zap.String("client_request_id", func() string { v, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string); return v }()),
				zap.String("gcr_request_id", c.Request.Header.Get("X-GCR-Request-Id")),
				zap.String("newapi_request_id", c.Request.Header.Get("X-Newapi-Request-Id")),
				// codex round35 fu54 (2026-05-20): NewAPI/QuantumNous forks
				// send X-Oneapi-Request-Id (not X-Newapi). Summary log at
				// line 1218-1219 already records both — mirror it here so
				// timeout triage doesn't need to chase the request via two
				// different log lines.
				zap.String("oneapi_request_id", c.Request.Header.Get("X-Oneapi-Request-Id")),
				zap.Int("inbound_body_len", inboundBodyLen),
				zap.Bool("large_context_request", IsLargeContextCtx(c.Request.Context())),
				zap.String("gcr_depth_bucket", c.Request.Header.Get("X-GCR-Depth-Bucket")),
				zap.String("gcr_estimated_tokens", c.Request.Header.Get("X-GCR-Estimated-Tokens")),
				zap.Bool("first_meaningful_seen", firstMeaningfulSeen),
			)
			// 5/10 codex audit (R38): !headerWritten 时返 BreakSticky failover.
			// data interval timeout 跟 first_meaningful_timeout 是不同失败模式
			// (前者发生在已有数据但中段断流, 后者从未见到首个 meaningful event)
			// 但 retry 策略相同: 没写 header 就切账号试一次. Reason 区分让
			// handler per-reason cap 各自计数 (一个请求两类 timeout 各占 1 次).
			if !headerWritten {
				return resultWithUsage(), &UpstreamFailoverError{
					StatusCode:  http.StatusBadGateway,
					BreakSticky: true,
					Reason:      "stream_data_interval_timeout",
				}
			}
			return resultWithUsage(), fmt.Errorf("stream data interval timeout")

		case <-firstMeaningfulDeadlineCh:
			// codex 5/8 #2: 首个 meaningful event 超时. 如果还没 WriteHeader,
			// 我们能干净返 error 让 caller 改 status (502). 之前是傻等
			// stream_data_interval_timeout 默认 180s 才发现空流.
			if firstMeaningfulSeen {
				// 已经见过 meaningful, 此 timer 残留 (理论上 Stop 了不会到这).
				continue
			}
			// codex round 11ak/11al (2026-05-15): enrich timeout 日志 + ops_error_logs.
			// 三边 req_id + 账号/上游/继续状态 + 三类失败模式分类一次到位.
			ctx := c.Request.Context()
			clientReqID, _ := ctx.Value(ctxkey.ClientRequestID).(string)
			ftMs := 0
			if firstTokenMs != nil {
				ftMs = *firstTokenMs
			}
			timeoutState := classifyTimeoutState(firstChunk, firstMeaningfulSeen)
			accID := int64(0)
			accName := ""
			accPlat := ""
			accType := ""
			if meta.Account != nil {
				accID = meta.Account.ID
				accName = meta.Account.Name
				accPlat = meta.Account.Platform
				accType = string(meta.Account.Type)
			}
			logger.L().Warn("openai messages stream: first meaningful event timeout",
				zap.String("request_id", requestID),
				zap.String("client_request_id", clientReqID),
				zap.String("gcr_request_id", c.Request.Header.Get("X-GCR-Request-Id")),
				zap.String("newapi_request_id", c.Request.Header.Get("X-Newapi-Request-Id")),
				// codex round35 fu54: mirror oneapi_request_id (see data interval timeout above).
				zap.String("oneapi_request_id", c.Request.Header.Get("X-Oneapi-Request-Id")),
				zap.String("model", originalModel),
				zap.Duration("timeout", firstMeaningfulTimeout),
				zap.Bool("header_written", headerWritten),
				zap.String("timeout_state", timeoutState),
				zap.Int("inbound_body_len", inboundBodyLen),
				zap.Bool("large_context_request", IsLargeContextCtx(ctx)),
				zap.String("gcr_depth_bucket", c.Request.Header.Get("X-GCR-Depth-Bucket")),
				zap.String("gcr_estimated_tokens", c.Request.Header.Get("X-GCR-Estimated-Tokens")),
				zap.Int("first_token_ms", ftMs),
				zap.Int("pending_events", len(pendingEvents)),
				// 11al: account / proxy / continuation state
				zap.Int64("account_id", accID),
				zap.String("account_name", accName),
				zap.String("account_platform", accPlat),
				zap.String("account_type", accType),
				zap.String("proxy_hash", meta.ProxyHash),
				zap.Int("messages_count", meta.MessagesCount),
				zap.String("prompt_cache_key_sha", meta.PromptCacheKeySha256),
				zap.Bool("has_previous_response_id", meta.HasPreviousResponseID),
				zap.Bool("has_turn_state", meta.HasTurnState),
				zap.Int("time_to_headers_ms", meta.TimeToHeadersMs),
			)
			// 11al codex #3 + 11am: 写入 ops_error_logs.upstream_errors. Detail
			// 是 JSON-marshaled 内部排查结构 (codex 建议 schema), 8 字段含
			// kind/attempt/account_id/model/timeout_ms/header_written/body_bytes/
			// messages_count/has_previous_response_id/has_turn_state +
			// timeout_state 扩展. 内部脱敏: 无 prompt/Authorization/真实 URL.
			// **不进 customer response** (那条走 mapUpstreamError 中性文案).
			if meta.Account != nil {
				detailJSON, _ := json.Marshal(struct {
					Kind                  string `json:"kind"`
					Attempt               int    `json:"attempt"`
					AccountID             int64  `json:"account_id"`
					Model                 string `json:"model"`
					TimeoutMs             int    `json:"timeout_ms"`
					HeaderWritten         bool   `json:"header_written"`
					BodyBytes             int    `json:"body_bytes"`
					MessagesCount         int    `json:"messages_count"`
					HasPreviousResponseID bool   `json:"has_previous_response_id"`
					HasTurnState          bool   `json:"has_turn_state"`
					TimeoutState          string `json:"timeout_state"`
					TimeToHeadersMs       int    `json:"time_to_headers_ms"`
					FirstTokenMs          int    `json:"first_token_ms"`
					LargeContext          bool   `json:"large_context"`
				}{
					Kind:                  "first_meaningful_timeout",
					Attempt:               1, // sub2api 这层不知道 handler 端 perReasonSwitchCount, 简化 1
					AccountID:             meta.Account.ID,
					Model:                 originalModel,
					TimeoutMs:             int(firstMeaningfulTimeout.Milliseconds()),
					HeaderWritten:         headerWritten,
					BodyBytes:             inboundBodyLen,
					MessagesCount:         meta.MessagesCount,
					HasPreviousResponseID: meta.HasPreviousResponseID,
					HasTurnState:          meta.HasTurnState,
					TimeoutState:          timeoutState,
					TimeToHeadersMs:       meta.TimeToHeadersMs,
					FirstTokenMs:          ftMs,
					LargeContext:          IsLargeContextCtx(ctx),
				})
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           meta.Account.Platform,
					AccountID:          meta.Account.ID,
					AccountName:        meta.Account.Name,
					UpstreamStatusCode: 0,
					Kind:               "first_meaningful_timeout",
					Message:            fmt.Sprintf("first meaningful event timeout after %s (state=%s)", firstMeaningfulTimeout, timeoutState),
					Detail:             string(detailJSON),
				})
			}
			// 5/10 codex audit: !headerWritten 时返 BreakSticky failover, 让
			// handler 切账号试一次. Reason 让 handler per-reason cap=1 避免
			// 一个请求烧 10 账号 (timeout=120s × 10 = 20min, 客户体验差).
			// thinking 长任务真没出 token 时, retry 一次还 timeout 就放弃.
			if !headerWritten {
				return resultWithUsage(), &UpstreamFailoverError{
					StatusCode:  http.StatusBadGateway,
					BreakSticky: true,
					Reason:      "first_meaningful_timeout",
				}
			}
			return resultWithUsage(), fmt.Errorf("first meaningful event timeout after %s", firstMeaningfulTimeout)

		case <-keepaliveCh:
			// 2026-05-15 codex round 11aj: 大上下文 pre-firstMeaningful 保活.
			// 上游 HTTP 已接通 (有 metadata events 累在 pendingEvents) 但还
			// 没出首个 meaningful event 时, 在 keepalive 周期 flush pending
			// (含 message_start) + 发 ping. 让客户端看到合法 SSE 流头, 不
			// 因 30s+ 无字节而断开. 一旦 WriteHeader 提交 200, 该请求无法
			// 再 retry (per codex "已写 header 不再隐藏重试" 原则).
			//
			// 触发条件:
			//   - 11ai 的 IsLargeContextCtx 标记本请求为大上下文
			//   - !firstMeaningfulSeen (尚未触发 main flush)
			//   - len(pendingEvents) > 0 (上游至少发了 message_start, 流形态合法)
			//   - !clientDisconnected
			if !firstMeaningfulSeen && IsLargeContextCtx(c.Request.Context()) && len(pendingEvents) > 0 && !clientDisconnected {
				writeStreamHeader()
				for _, evt := range pendingEvents {
					sse, err := apicompat.ResponsesAnthropicEventToSSE(evt)
					if err != nil {
						continue
					}
					if _, werr := fmt.Fprint(c.Writer, sse); werr != nil {
						clientDisconnected = true
						disconnectedAt = time.Now()
						break
					}
				}
				pendingEvents = nil
				if !clientDisconnected {
					if _, err := fmt.Fprint(c.Writer, "event: ping\ndata: {\"type\":\"ping\"}\n\n"); err != nil {
						clientDisconnected = true
						disconnectedAt = time.Now()
					} else {
						c.Writer.Flush()
					}
				}
				continue
			}

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
//
//	真实 (返 true): content_block_delta (text/thinking/input_json/signature),
//	               content_block_start tool_use/server_tool_use,
//	               message_delta (含 usage), message_stop, error.
//	元数据 (返 false): message_start, ping, content_block_start text/thinking
//	                 (空块, 还没 token), content_block_stop.
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
