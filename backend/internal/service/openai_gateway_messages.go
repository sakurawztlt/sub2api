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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

const gcrCCTestWebSearchProbeHeader = "X-GCR-CCTest-WebSearch-Probe"

var (
	errOpenAICompatBufferedTotalTimeout           = errors.New("buffered total timeout")
	errOpenAICompatBufferedFirstMeaningfulTimeout = errors.New("buffered first meaningful timeout")
	errOpenAICompatBufferedPostContentIdleTimeout = errors.New("buffered post-content idle timeout")
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
) (forwardResultOut *OpenAIForwardResult, _ error) {
	beginUpstreamResponseModelObservation(c)
	defer func() {
		forwardResultOut = attachObservedOpenAIUpstreamResponseModel(c, forwardResultOut)
	}()
	setCodexToolNameReverse(c, nil)

	// CN providers configured for their native Anthropic protocol (including
	// adaptive mode) must stay on /v1/messages so thinking, tool_use and cache
	// semantics are preserved end to end. This dispatch precedes the Responses
	// capability probe because native Anthropic accounts intentionally do not
	// advertise OpenAI Responses support.
	if account.IsAnthropicProtocol() || account.IsAdaptiveAPIProtocol() {
		return s.forwardAnthropicViaNativeAnthropicEndpoint(ctx, c, account, body, defaultMappedModel)
	}

	// Explicit Chat Completions accounts, plus generic API-key relays whose
	// compatibility probe rejects Responses, use the direct Messages↔CC bridge.
	if shouldForwardOpenAIResponsesViaRawChatCompletions(account) {
		return s.forwardAnthropicViaRawChatCompletions(ctx, c, account, body, defaultMappedModel)
	}
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
	lowLatencyWebSearchProbe := false
	if strings.TrimSpace(c.GetHeader(gcrCCTestWebSearchProbeHeader)) == "1" {
		// gcr derives this marker from the original pre-injection request and
		// strips any client-supplied copy. Ignore only gcr's injected effort
		// while revalidating the rest of the exact detector envelope locally.
		probeCandidate := anthropicReq
		probeCandidate.OutputConfig = nil
		lowLatencyWebSearchProbe = apicompat.IsLowLatencyWebSearchProbe(&probeCandidate)
		if lowLatencyWebSearchProbe {
			anthropicReq.OutputConfig = nil
		}
	}
	originalModel := anthropicReq.Model
	applyOpenAICompatModelNormalization(&anthropicReq)
	normalizedModel := anthropicReq.Model
	clientStream := anthropicReq.Stream // client's original stream preference
	forceThinkingBlock := anthropicThinkingEnabledForResponse(&anthropicReq)
	webSearchRequestLimit := anthropicWebSearchRequestLimitForResponse(&anthropicReq)

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
	if compatReplayGuardEnabled && !account.UsesOpenAICodexProtocol() && previousResponseID == "" && !compatContinuationDisabled {
		compatReplayTrimmed = applyAnthropicCompatFullReplayGuard(&anthropicReq)
	}

	// 3. Convert Anthropic → Responses (after replay guard mutates messages).
	responsesReq, err := apicompat.AnthropicToResponses(&anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("convert anthropic to responses: %w", err)
	}

	// API-key Responses supports non-streaming JSON. Preserve the client's
	// stream=false preference there so long buffered non-stream requests do not
	// sit behind SSE terminal-event timers. OAuth/Codex internal bridge remains
	// streaming because that path relies on SSE-only continuation metadata.
	responsesReq.Stream = clientStream || account.UsesOpenAICodexProtocol()
	isStream := responsesReq.Stream

	// 3b. Handle BetaFastMode → service_tier: "priority"
	if containsBetaToken(c.GetHeader("anthropic-beta"), claude.BetaFastMode) {
		responsesReq.ServiceTier = "priority"
	}

	responsesReq.Model = upstreamModel
	// codex round41 fu59 (2026-05-20): re-evaluate the reasoning-model
	// strip AFTER the upstream model mapping. The converter at line 119
	// above only sees the client-side model (e.g. claude-opus-4-6); the
	// claude → gpt-5.x mapping happens here at line ~134 via upstreamModel.
	// Without this second strip, gpt-5 reasoning models still receive
	// temperature/top_p and return 400 "Unsupported parameter" — exactly
	// the bug PR #2580 set out to fix.
	if apicompat.IsReasoningModel(upstreamModel) {
		responsesReq.Temperature = nil
		responsesReq.TopP = nil
	}
	if responsesReq.Reasoning != nil {
		responsesReq.Reasoning.Effort = openAICompatAnthropicReasoningEffort(&anthropicReq, upstreamModel, responsesReq.Reasoning.Effort)
	}
	if previousResponseID != "" {
		responsesReq.PreviousResponseID = previousResponseID
		trimAnthropicCompatResponsesInputToLatestTurn(responsesReq)
	}
	if compatReplayGuardEnabled && !account.UsesOpenAICodexProtocol() {
		appendOpenAICompatClaudeCodeTodoGuard(responsesReq)
		appendOpenAICompatDeferredToolGuard(responsesReq)
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

	// 4. Marshal Responses request body, then apply the ChatGPT/Codex transform.
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

	if account.UsesOpenAICodexProtocol() && account.Platform != PlatformGrok {
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
		if codexResult.Error != nil {
			writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", codexResult.Error.Error())
			return nil, codexResult.Error
		}
		setCodexToolNameReverse(c, codexResult.ToolNameReverse)
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
			appendOpenAICompatDeferredToolGuardToRequestBody(reqBody)
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
	if account.Platform == PlatformOpenAI {
		if policyBody, changed := ApplyOpenAIReasoningEffortPolicyFromContext(ctx, responsesBody); changed {
			responsesBody = policyBody
			if responsesReq.Reasoning != nil {
				responsesReq.Reasoning.Effort = gjson.GetBytes(responsesBody, "reasoning.effort").String()
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
			MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
			writeAnthropicError(c, http.StatusForbidden, "forbidden_error", blocked.Message)
		}
		return nil, policyErr
	}
	responsesBody = updatedBody
	grokCacheIdentity := ""
	if account.Platform == PlatformGrok {
		grokIntentSourceBody := responsesBody
		grokCacheIdentity = resolveGrokCacheIdentity(c, grokIntentSourceBody, promptCacheKey, upstreamModel)
		patchedBody, patchErr := patchGrokResponsesBody(grokIntentSourceBody, upstreamModel)
		if patchErr != nil {
			return nil, patchErr
		}
		responsesBody, patchErr = applyGrokResponsesCacheIdentity(patchedBody, grokIntentSourceBody, grokCacheIdentity, account.IsGrokOAuth())
		if patchErr != nil {
			return nil, fmt.Errorf("apply grok prompt cache identity: %w", patchErr)
		}
		responsesBody, patchErr = applyGrokFreeMessagesFunctionToolCacheRoute(responsesBody, grokIntentSourceBody, account, grokCacheIdentity)
		if patchErr != nil {
			return nil, fmt.Errorf("apply grok Free function-tool cache route: %w", patchErr)
		}
	}

	// 5. Get access token
	token, _, err := s.getRequestCredential(ctx, c, account)
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	// 6. Build upstream request
	if account.UsesOpenAICodexProtocol() && account.Platform != PlatformGrok {
		// The Messages bridge needs its existing body/session behavior even when
		// the transformed body has no bridge marker. Identity is restored after
		// the request is built.
		setOpenAICompatMessagesBridgeContext(c, true)
	}
	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	var upstreamReq *http.Request
	if account.Platform == PlatformGrok {
		upstreamReq, err = buildGrokResponsesRequest(upstreamCtx, c, account, responsesBody, token, grokCacheIdentity, s.cfg, s.settingService)
	} else {
		upstreamReq, err = s.buildUpstreamRequest(upstreamCtx, c, account, responsesBody, token, isStream, promptCacheKey, false)
	}
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
	if account.UsesOpenAICodexProtocol() && account.Platform != PlatformGrok {
		// Restore the complete paired Codex identity immediately before sending.
		// ChatGPT's Codex endpoint returns 404 when originator and User-Agent do
		// not identify the same official client.
		ensureCodexIdentityHeaders(upstreamReq.Header)
		enforceCodexIdentityHeadersWithUA(upstreamReq.Header, s.codexIdentityOverrideUA(account))
		logger.L().Debug("openai messages: upstream identity restored",
			zap.Int64("account_id", account.ID),
			zap.String("upstream_model", upstreamModel),
			zap.Bool("compat_identity_restored", true),
		)
	}
	if account.UsesOpenAICodexProtocol() && promptCacheKey != "" && strings.TrimSpace(c.GetHeader("conversation_id")) == "" {
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
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	// 8. Handle error response with failover
	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
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
		if account.Platform == PlatformGrok &&
			isGrokInvalidEncryptedContentResponse(resp.StatusCode, respBody) &&
			!grokEncryptedContentStripRetried(ctx) {
			if strippedBody, ok := stripAnthropicThinkingSignatures(body); ok {
				logger.L().Info("openai messages: stripping thinking signatures for Grok failover retry", zap.Int64("account_id", account.ID))
				return s.ForwardAsAnthropic(markGrokEncryptedContentStripRetried(ctx), c, account, strippedBody, promptCacheKey, defaultMappedModel)
			}
		}
		if failoverErr := s.failoverOpenAIUpstreamHTTPError(ctx, c, account, resp, respBody, upstreamMsg, upstreamModel); failoverErr != nil {
			return nil, failoverErr
		}
		// Non-failover error: return Anthropic-formatted error to client
		return s.handleAnthropicErrorResponse(resp, c, account, billingModel)
	}
	if account.Platform == PlatformGrok && account.Type == AccountTypeOAuth {
		s.updateGrokUsageFromResponse(ctx, account, resp.Header, resp.StatusCode)
	}

	// 058 step 2: Codex SSE returns x-codex-turn-state on the success header.
	// Cache it under the prompt cache key so the next turn can resume the
	// same internal slot.
	//
	// codex round37 fu56 (2026-05-20): capture the outbound signal too so
	// the summary log can distinguish "our cache fed prior state in" from
	// "upstream emitted fresh state we'll cache for next turn".
	upstreamTurnState := strings.TrimSpace(resp.Header.Get("x-codex-turn-state"))
	if account.UsesOpenAICodexProtocol() && promptCacheKey != "" && upstreamTurnState != "" {
		s.bindOpenAICompatSessionTurnState(ctx, c, account, promptCacheKey, upstreamTurnState)
	}

	// 9. Handle normal response.
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
	// codex round37 fu56 (2026-05-20): outbound signal — see field doc.
	streamMeta.UpstreamTurnStateReturned = upstreamTurnState != ""
	if clientStream {
		result, handleErr = s.handleAnthropicStreamingResponse(resp, c, account, originalModel, billingModel, upstreamModel, startTime, len(body), streamMeta, webSearchRequestLimit, lowLatencyWebSearchProbe)
	} else if !isStream && !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		result, handleErr = s.handleAnthropicJSONResponse(resp, c, originalModel, billingModel, upstreamModel, startTime, forceThinkingBlock, webSearchRequestLimit)
	} else {
		// Client wants JSON but upstream is streaming or ignored stream=false:
		// buffer the streaming response and assemble a JSON reply.
		result, handleErr = s.handleAnthropicBufferedStreamingResponse(resp, c, account, originalModel, billingModel, upstreamModel, startTime, len(body), forceThinkingBlock, webSearchRequestLimit)
	}

	// cyber_policy：标记已设、error 已按 Anthropic 格式发给客户端。丢弃普通 result、返回哨兵，
	// 使 handler 改用 RecordCyberPolicyUsageLog 依上游真实 token 计费，不 failover、不重复写响应。
	if GetOpsCyberPolicy(c) != nil {
		if handleErr == nil {
			handleErr = errOpenAICyberPolicyForwarded
		}
		return nil, handleErr
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
	if handleErr == nil && account.Type == AccountTypeOAuth && account.Platform != PlatformGrok && !account.IsShadow() {
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
	requestedModel ...string,
) (*OpenAIForwardResult, error) {
	return s.handleCompatErrorResponse(resp, c, account, writeAnthropicError, requestedModel...)
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
	account *Account,
	originalModel string,
	billingModel string,
	upstreamModel string,
	startTime time.Time,
	inboundBodyLen int,
	forceThinkingBlock bool,
	webSearchRequestLimit int,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")
	usageOpts := apicompat.AnthropicUsageEstimationOptions{
		ExternalInputTokens:  positiveIntHeader(c.Request, "X-GCR-Estimated-Tokens"),
		ForceThinkingBlock:   forceThinkingBlock,
		MaxWebSearchRequests: webSearchRequestLimit,
	}

	finalResponse, usage, acc, err := s.readOpenAICompatBufferedTerminal(c.Request.Context(), resp, c, "openai messages buffered", requestID, inboundBodyLen)
	if err != nil {
		// Messages historically surfaces the underlying transport/read error.
		// The shared reader wraps scanner failures so Chat Completions can
		// classify them, but that wrapper must not change Messages semantics.
		var readErr *openAICompatBufferedReadError
		if errors.As(err, &readErr) && readErr != nil {
			return nil, readErr.cause
		}
		if errors.Is(err, errOpenAICompatBufferedTotalTimeout) || errors.Is(err, errOpenAICompatBufferedPostContentIdleTimeout) {
			if acc != nil && acc.HasContent() {
				finalResponse = &apicompat.ResponsesResponse{
					Status: "incomplete",
					Output: acc.BuildOutput(),
				}
				logger.L().Warn("openai messages buffered: synthesized terminal from accumulator (buffered total timeout)",
					zap.String("request_id", requestID),
					zap.Error(err),
				)
			} else if !c.Writer.Written() {
				return nil, &UpstreamFailoverError{
					StatusCode:  http.StatusBadGateway,
					BreakSticky: true,
					Reason:      "buffered_total_timeout",
				}
			}
		}
		if finalResponse == nil {
			if errors.Is(err, errOpenAICompatBufferedFirstMeaningfulTimeout) && !c.Writer.Written() {
				return nil, &UpstreamFailoverError{
					StatusCode:  http.StatusBadGateway,
					BreakSticky: true,
					Reason:      "first_meaningful_timeout",
				}
			}
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
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	observer.Observe(finalResponse.Model, true)

	if strings.TrimSpace(finalResponse.Status) == "failed" {
		payload, _ := json.Marshal(gin.H{"type": "response.failed", "response": finalResponse})
		message := openAICompatFailedResponseMessage(finalResponse)
		if hit, code, msg := detectOpenAICyberPolicy(payload); hit {
			MarkOpsCyberPolicy(c, CyberPolicyMark{
				Code: code, Message: msg, Body: truncateString(string(payload), 4096),
				UpstreamStatus: http.StatusOK, UpstreamInTok: usage.InputTokens, UpstreamOutTok: usage.OutputTokens,
			})
			clientMsg := cyberPolicyClientMessage(msg, payload)
			writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", clientMsg)
			return nil, fmt.Errorf("openai cyber_policy: %s", msg)
		}
		if openAIStreamFailedEventShouldFailover(payload, message) {
			return nil, s.newOpenAIStreamFailoverError(c, account, false, requestID, payload, message, resp.Header)
		}
		message = s.recordOpenAIStreamUpstreamError(c, account, false, requestID, "http_error", payload, message)
		if status, errType, errMsg, matched := applyErrorPassthroughRule(
			c, account.Platform, 0, payload,
			http.StatusBadGateway, "api_error", message,
		); matched {
			if status == 0 {
				status = http.StatusBadGateway
			}
			if errMsg == "" {
				errMsg = message
			}
			MarkResponseCommitted(c)
			writeAnthropicError(c, status, errType, errMsg)
			return nil, fmt.Errorf("upstream response failed (passthrough): %s", errMsg)
		}
		writeAnthropicError(c, http.StatusBadGateway, "api_error", message)
		return nil, fmt.Errorf("upstream response failed: %s", message)
	}
	if strings.TrimSpace(finalResponse.Status) == "completed" {
		logOpenAISuccessMissingUsage(c.Request.Context(), c, account, resp, &usage, "response.completed", false)
	}

	// When the terminal event has an empty output array, reconstruct from
	// accumulated delta events so the client receives the full content.
	acc.SupplementResponseOutput(finalResponse)
	if finalResponse.Usage == nil {
		finalResponse.Usage = synthesizeResponsesUsageForBufferedFallback(c.Request, inboundBodyLen, acc)
		usage = copyOpenAIUsageFromResponsesUsage(finalResponse.Usage)
		logger.L().Warn("openai messages buffered: synthesized usage from accumulator",
			zap.String("request_id", requestID),
			zap.Int("input_tokens", finalResponse.Usage.InputTokens),
			zap.Int("output_tokens", finalResponse.Usage.OutputTokens),
			zap.Int("gcr_estimated_tokens", usageOpts.ExternalInputTokens),
		)
	}
	if !responsesResponseHasVisibleOutput(finalResponse) {
		logger.L().Warn("openai messages buffered: terminal response without visible output",
			zap.String("request_id", requestID),
		)
		if !c.Writer.Written() {
			return nil, &UpstreamFailoverError{
				StatusCode:  http.StatusBadGateway,
				BreakSticky: true,
				Reason:      "buffered_empty_output",
			}
		}
		writeAnthropicError(c, http.StatusBadGateway, "api_error", "Internal server error")
		return nil, fmt.Errorf("upstream terminal response had no visible output")
	}

	anthropicResp := apicompat.ResponsesToAnthropicWithUsageOptions(finalResponse, originalModel, usageOpts)
	// Grok reports authoritative cache counters even for small synthetic/test
	// turns. Preserve those explicit counters instead of applying Claude's
	// request-side cache eligibility estimate to provider-observed usage.
	if account != nil && account.Platform == PlatformGrok && finalResponse.Usage != nil && finalResponse.Usage.InputTokensDetails != nil {
		total := maxInt(finalResponse.Usage.InputTokens, 0)
		cached := maxInt(finalResponse.Usage.InputTokensDetails.CachedTokens, 0)
		if cached > total {
			cached = total
		}
		creation := maxInt(finalResponse.Usage.CacheCreationInputTokens, 0)
		if creation > total-cached {
			creation = total - cached
		}
		anthropicResp.Usage.InputTokens = total - cached - creation
		anthropicResp.Usage.CacheCreationInputTokens = creation
		anthropicResp.Usage.CacheReadInputTokens = cached
	}

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

func (s *OpenAIGatewayService) handleAnthropicJSONResponse(
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	billingModel string,
	upstreamModel string,
	startTime time.Time,
	forceThinkingBlock bool,
	webSearchRequestLimit int,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")
	usageOpts := apicompat.AnthropicUsageEstimationOptions{
		ExternalInputTokens:  positiveIntHeader(c.Request, "X-GCR-Estimated-Tokens"),
		ForceThinkingBlock:   forceThinkingBlock,
		MaxWebSearchRequests: webSearchRequestLimit,
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		if !c.Writer.Written() && IsUpstreamNetworkError(err) {
			return nil, &UpstreamFailoverError{
				StatusCode:  http.StatusBadGateway,
				BreakSticky: true,
				Reason:      "json_body_read_error",
			}
		}
		return nil, fmt.Errorf("read responses json body: %w", err)
	}
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	observer.ObserveOpenAI(respBody, strings.TrimSpace(gjson.GetBytes(respBody, "type").String()))

	var finalResponse apicompat.ResponsesResponse
	if err := json.Unmarshal(respBody, &finalResponse); err != nil {
		logger.L().Warn("openai messages json: failed to parse response",
			zap.String("request_id", requestID),
			zap.Error(err),
		)
		if !c.Writer.Written() {
			writeAnthropicError(c, http.StatusBadGateway, "api_error", "Internal server error")
		}
		return nil, fmt.Errorf("parse responses json body: %w", err)
	}

	// API-key Responses backends may honor stream=false and return a terminal
	// response.failed object directly as a 200 JSON response. This transport
	// branch was added after the original cyber-policy implementation, which
	// only covered SSE terminals. Detect it before the generic empty-output
	// failover and preserve upstream usage for handler-side billing.
	if hit, code, msg := detectOpenAICyberPolicy(respBody); hit {
		usage := copyOpenAIUsageFromResponsesUsage(finalResponse.Usage)
		MarkOpsCyberPolicy(c, CyberPolicyMark{
			Code: code, Message: msg, Body: truncateString(string(respBody), 4096),
			UpstreamStatus: http.StatusOK, UpstreamInTok: usage.InputTokens, UpstreamOutTok: usage.OutputTokens,
		})
		clientMsg := cyberPolicyClientMessage(msg, respBody)
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", clientMsg)
		return nil, fmt.Errorf("openai cyber_policy: %s", msg)
	}

	if !responsesResponseHasVisibleOutput(&finalResponse) {
		logger.L().Warn("openai messages json: terminal response without visible output",
			zap.String("request_id", requestID),
			zap.String("status", finalResponse.Status),
		)
		if !c.Writer.Written() {
			return nil, &UpstreamFailoverError{
				StatusCode:  http.StatusBadGateway,
				BreakSticky: true,
				Reason:      "json_empty_output",
			}
		}
		writeAnthropicError(c, http.StatusBadGateway, "api_error", "Internal server error")
		return nil, fmt.Errorf("upstream json response had no visible output")
	}

	usage := copyOpenAIUsageFromResponsesUsage(finalResponse.Usage)
	anthropicResp := apicompat.ResponsesToAnthropicWithUsageOptions(&finalResponse, originalModel, usageOpts)

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.JSON(http.StatusOK, anthropicResp)

	return &OpenAIForwardResult{
		RequestID:     requestID,
		ResponseID:    strings.TrimSpace(finalResponse.ID),
		Usage:         usage,
		Model:         originalModel,
		BillingModel:  billingModel,
		UpstreamModel: upstreamModel,
		Stream:        false,
		Duration:      time.Since(startTime),
	}, nil
}

const (
	openAICompatLargeBufferedBodyBytes           = 64 * 1024
	openAICompatLargeBufferedTotalTimeoutSeconds = 360
	openAICompatLargeBufferedPostContentIdleSec  = 45
	openAICompatOrdinaryStreamingBodyBytes       = 256 * 1024
	openAICompatOrdinaryFirstMeaningfulSeconds   = 60
)

func effectiveOpenAICompatBufferedTotalTimeoutSeconds(configuredSeconds int, inboundBodyLen int) int {
	if configuredSeconds <= 0 {
		return configuredSeconds
	}
	if inboundBodyLen >= openAICompatLargeBufferedBodyBytes &&
		configuredSeconds < openAICompatLargeBufferedTotalTimeoutSeconds {
		return openAICompatLargeBufferedTotalTimeoutSeconds
	}
	return configuredSeconds
}

func effectiveOpenAICompatBufferedPostContentIdleSeconds(inboundBodyLen int) int {
	if inboundBodyLen >= openAICompatLargeBufferedBodyBytes {
		return openAICompatLargeBufferedPostContentIdleSec
	}
	return 0
}

func effectiveOpenAICompatStreamingFirstMeaningfulTimeout(configured time.Duration, inboundBodyLen int, meta streamReqMeta) time.Duration {
	if configured <= 0 {
		return configured
	}
	ordinaryTimeout := time.Duration(openAICompatOrdinaryFirstMeaningfulSeconds) * time.Second
	if configured <= ordinaryTimeout {
		return configured
	}
	if inboundBodyLen <= 0 || inboundBodyLen >= openAICompatOrdinaryStreamingBodyBytes {
		return configured
	}
	if meta.MessagesCount > 1 || meta.ToolsCount > 0 || meta.HasPreviousResponseID || meta.HasTurnState {
		return configured
	}
	return ordinaryTimeout
}

func positiveIntHeader(req *http.Request, name string) int {
	if req == nil {
		return 0
	}
	raw := strings.TrimSpace(req.Header.Get(name))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func anthropicThinkingEnabledForResponse(req *apicompat.AnthropicRequest) bool {
	if req == nil || req.Thinking == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(req.Thinking.Type)) {
	case "enabled", "adaptive":
		return true
	default:
		return false
	}
}

func anthropicWebSearchRequestLimitForResponse(req *apicompat.AnthropicRequest) int {
	if req == nil {
		return 0
	}
	if apicompat.IsLowLatencyWebSearchProbe(req) {
		return 1
	}
	hasWebSearch := false
	explicitLimit := 0
	for _, tool := range req.Tools {
		if !strings.HasPrefix(tool.Type, "web_search") && tool.Name != "web_search" {
			continue
		}
		hasWebSearch = true
		if tool.MaxUses > 0 && (explicitLimit == 0 || tool.MaxUses < explicitLimit) {
			explicitLimit = tool.MaxUses
		}
	}
	if !hasWebSearch {
		return 0
	}
	text := strings.ToLower(strings.TrimSpace(latestAnthropicUserTextForGateway(req)))
	if strings.Contains(text, "use the web_search tool first") &&
		strings.Contains(text, "perform a web search for the query:") {
		return 1
	}
	return explicitLimit
}

func latestAnthropicUserTextForGateway(req *apicompat.AnthropicRequest) string {
	if req == nil {
		return ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		return latestAnthropicTextPartForGateway(req.Messages[i].Content)
	}
	return ""
}

func latestAnthropicTextPartForGateway(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return s
		}
		return ""
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return ""
	}
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i].Type != "text" {
			continue
		}
		text := strings.TrimSpace(parts[i].Text)
		if text == "" || strings.HasPrefix(text, "<system-reminder>") {
			continue
		}
		return text
	}
	return ""
}

func synthesizeResponsesUsageForBufferedFallback(req *http.Request, inboundBodyLen int, acc *apicompat.BufferedResponseAccumulator) *apicompat.ResponsesUsage {
	inputTokens := positiveIntHeader(req, "X-GCR-Estimated-Tokens")
	if inputTokens <= 0 && inboundBodyLen > 0 {
		inputTokens = (inboundBodyLen + 3) / 4
	}
	if inputTokens <= 0 {
		inputTokens = 1
	}

	outputTokens := 0
	if acc != nil {
		outputTokens = acc.EstimatedOutputTokens()
	}
	if outputTokens <= 0 && acc != nil && acc.HasContent() {
		outputTokens = 1
	}

	return &apicompat.ResponsesUsage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
	}
}

func isOpenAICompatResponsesTerminalEvent(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.completed", "response.done", "response.incomplete", "response.failed", "response.cancelled", "response.canceled", "error":
		return true
	default:
		return false
	}
}

func isOpenAICompatBufferedProgressEvent(event *apicompat.ResponsesStreamEvent) bool {
	if event == nil {
		return false
	}
	eventType := strings.TrimSpace(event.Type)
	if event.Item != nil && strings.TrimSpace(event.Item.Type) != "" {
		switch eventType {
		case "response.output_item.added", "response.output_item.in_progress", "response.output_item.done":
			return true
		}
	}
	if strings.HasPrefix(eventType, "response.content_part.") ||
		strings.HasPrefix(eventType, "response.reasoning_summary_part.") ||
		strings.HasPrefix(eventType, "response.web_search_call.") {
		return true
	}
	return false
}

func isMeaningfulOpenAICompatBufferedEvent(event *apicompat.ResponsesStreamEvent, acc *apicompat.BufferedResponseAccumulator) bool {
	if event == nil {
		return false
	}
	if acc != nil && acc.HasContent() {
		return true
	}
	if event.Type == "error" {
		return true
	}
	if isOpenAICompatBufferedProgressEvent(event) {
		return true
	}
	if event.Usage != nil {
		return true
	}
	if event.Response != nil {
		if event.Response.Usage != nil {
			return true
		}
		if isOpenAICompatResponsesTerminalEvent(event.Type) {
			return true
		}
	}
	return false
}

func responsesResponseHasVisibleOutput(resp *apicompat.ResponsesResponse) bool {
	if resp == nil {
		return false
	}
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if strings.TrimSpace(part.Text) != "" ||
					strings.TrimSpace(part.ImageURL) != "" ||
					strings.TrimSpace(part.FileData) != "" ||
					strings.TrimSpace(part.FileURL) != "" ||
					strings.TrimSpace(part.FileID) != "" {
					return true
				}
			}
		case "reasoning":
			for _, summary := range item.Summary {
				if strings.TrimSpace(summary.Text) != "" {
					return true
				}
			}
		case "function_call":
			if strings.TrimSpace(item.CallID) != "" ||
				strings.TrimSpace(item.Name) != "" ||
				strings.TrimSpace(item.Arguments) != "" {
				return true
			}
		case "web_search_call":
			if item.Action != nil {
				return true
			}
		default:
			return true
		}
	}
	return false
}

func (s *OpenAIGatewayService) recordOpenAIMessagesStreamUpstreamError(c *gin.Context, account *Account, upstreamRequestID, kind, message string) {
	if c == nil {
		return
	}
	message = sanitizeUpstreamErrorMessage(message)
	setOpsUpstreamError(c, http.StatusBadGateway, message, "")
	event := OpsUpstreamErrorEvent{
		Platform:           PlatformOpenAI,
		UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID:  strings.TrimSpace(upstreamRequestID),
		Kind:               kind,
		Message:            message,
	}
	if account != nil {
		event.Platform = account.Platform
		event.AccountID = account.ID
		event.AccountName = account.Name
	}
	appendOpsUpstreamError(c, event)
}

func isOpenAICompatDoneSentinelLine(line string) bool {
	payload, ok := extractOpenAISSEDataLine(line)
	return ok && strings.TrimSpace(payload) == "[DONE]"
}

func openAICompatTerminalResponse(event *apicompat.ResponsesStreamEvent, payload []byte) *apicompat.ResponsesResponse {
	if event == nil {
		return nil
	}
	if event.Response != nil {
		return event.Response
	}
	switch strings.TrimSpace(event.Type) {
	case "response.failed", "error":
		message := extractOpenAISSEErrorMessage(payload)
		if message == "" {
			message = "Upstream response failed"
		}
		return &apicompat.ResponsesResponse{
			Status: "failed",
			Error:  &apicompat.ResponsesError{Code: event.Code, Message: message},
		}
	default:
		return nil
	}
}

// openAICompatBufferedReadError marks failures while consuming an upstream
// response body so endpoint-specific callers can decide whether replay is safe.
type openAICompatBufferedReadError struct {
	cause error
}

func (e *openAICompatBufferedReadError) Error() string { return e.cause.Error() }
func (e *openAICompatBufferedReadError) Unwrap() error { return e.cause }

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
	c *gin.Context,
	logPrefix string,
	requestID string,
	inboundBodyLen int,
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
	// first_meaningful: 首个可见内容 / 工具调用 / terminal / usage 未达到时
	//                   超时. response.created/in_progress 这类元数据不算,
	//                   否则高 reasoning 请求会一直拖到 total timeout.
	var (
		totalTimeoutCh    <-chan time.Time
		totalTimeoutTimer *time.Timer
		firstMeaningTimer *time.Timer
		firstMeaningCh    <-chan time.Time
		firstMeaningSeen  bool
		postContentTimer  *time.Timer
		postContentCh     <-chan time.Time
	)
	bufferedTotalTimeoutSec := 0
	if s.cfg != nil {
		bufferedTotalTimeoutSec = effectiveOpenAICompatBufferedTotalTimeoutSeconds(s.cfg.Gateway.BufferedTotalTimeout, inboundBodyLen)
	}
	if bufferedTotalTimeoutSec > 0 {
		totalTimeoutTimer = time.NewTimer(time.Duration(bufferedTotalTimeoutSec) * time.Second)
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
	postContentIdleSec := effectiveOpenAICompatBufferedPostContentIdleSeconds(inboundBodyLen)
	resetPostContentIdle := func() {
		if postContentIdleSec <= 0 || acc == nil || !acc.HasContent() {
			return
		}
		d := time.Duration(postContentIdleSec) * time.Second
		if postContentTimer == nil {
			postContentTimer = time.NewTimer(d)
			postContentCh = postContentTimer.C
			return
		}
		if !postContentTimer.Stop() {
			select {
			case <-postContentTimer.C:
			default:
			}
		}
		postContentTimer.Reset(d)
	}
	defer func() {
		if postContentTimer != nil {
			postContentTimer.Stop()
		}
	}()

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
		if !firstMeaningSeen && firstMeaningTimer != nil && isMeaningfulOpenAICompatBufferedEvent(&event, acc) {
			firstMeaningSeen = true
			if !firstMeaningTimer.Stop() {
				select {
				case <-firstMeaningTimer.C:
				default:
				}
			}
		}
		if isMeaningfulOpenAICompatBufferedEvent(&event, acc) {
			resetPostContentIdle()
		}
		if response := openAICompatTerminalResponse(&event, []byte(payload)); isOpenAICompatResponsesTerminalEvent(event.Type) && response != nil {
			if event.Usage != nil {
				usage = copyOpenAIUsageFromResponsesUsage(event.Usage)
				if response.Usage == nil {
					response.Usage = event.Usage
				}
			}
			if response.Usage != nil {
				usage = copyOpenAIUsageFromResponsesUsage(response.Usage)
			}
			return true, response, nil
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
				return nil, usage, acc, &openAICompatBufferedReadError{cause: ev.err}
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
			s.parseSSEUsageBytesWithType([]byte(payload), event.Type, &usage)

			acc.ProcessEvent(&event)

			// 2026-05-27: only visible output/tool/terminal/usage counts
			// as meaningful for buffered non-stream. response.created and
			// response.in_progress prove the socket is alive but still give
			// clients no usable response; letting them stop this timer caused
			// 100s buffered_total_timeout and repeated NewAPI 502 retries.
			if !firstMeaningSeen && firstMeaningTimer != nil && isMeaningfulOpenAICompatBufferedEvent(&event, acc) {
				firstMeaningSeen = true
				if !firstMeaningTimer.Stop() {
					select {
					case <-firstMeaningTimer.C:
					default:
					}
				}
			}
			if isMeaningfulOpenAICompatBufferedEvent(&event, acc) {
				resetPostContentIdle()
			}

			if response := openAICompatTerminalResponse(&event, []byte(payload)); isOpenAICompatResponsesTerminalEvent(event.Type) && response != nil {
				if event.Usage != nil {
					usage = copyOpenAIUsageFromResponsesUsage(event.Usage)
					if response.Usage == nil {
						response.Usage = event.Usage
					}
				}
				if response.Usage != nil {
					usage = copyOpenAIUsageFromResponsesUsage(response.Usage)
				}
				return response, usage, acc, nil
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
				zap.Int("seconds", bufferedTotalTimeoutSec),
				zap.Int("inbound_body_len", inboundBodyLen),
			)
			return nil, usage, acc, errOpenAICompatBufferedTotalTimeout

		case <-postContentCh:
			_ = resp.Body.Close()
			logger.L().Warn(logPrefix+": buffered post-content idle timeout",
				zap.String("request_id", requestID),
				zap.Int("seconds", postContentIdleSec),
				zap.Int("inbound_body_len", inboundBodyLen),
			)
			return nil, usage, acc, errOpenAICompatBufferedPostContentIdleTimeout

		case <-firstMeaningCh:
			if firstMeaningSeen {
				continue
			}
			_ = resp.Body.Close()
			logger.L().Warn(logPrefix+": buffered first meaningful event timeout (codex round 11i)",
				zap.String("request_id", requestID),
				zap.Int("seconds", firstMeaningfulTimeoutSec),
			)
			return nil, usage, acc, errOpenAICompatBufferedFirstMeaningfulTimeout
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
	account *Account,
	originalModel string,
	billingModel string,
	upstreamModel string,
	startTime time.Time,
	inboundBodyLen int,
	meta streamReqMeta,
	webSearchRequestLimit int,
	lowLatencyWebSearchProbe bool,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}

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
	state.SetWebSearchRequestLimit(webSearchRequestLimit)
	state.SetIncrementalLiteralCitationsEnabled(lowLatencyWebSearchProbe)
	state.SetLowLatencyWebSearchFastPathEnabled(lowLatencyWebSearchProbe)
	state.SetCodeExecutionFallbackArgs(meta.CodeExecutionFallbackArgs)
	state.SetExternalInputTokenEstimate(positiveIntHeader(c.Request, "X-GCR-Estimated-Tokens"))
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
	clientOutputStarted := false
	var streamFailoverErr error
	var streamNonFailoverErr error
	terminalEventType := ""

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
	firstMeaningfulTimeout = effectiveOpenAICompatStreamingFirstMeaningfulTimeout(firstMeaningfulTimeout, inboundBodyLen, meta)
	var firstMeaningfulDeadlineCh <-chan time.Time
	var firstMeaningfulTimer *time.Timer
	if firstMeaningfulTimeout > 0 {
		firstMeaningfulTimer = time.NewTimer(firstMeaningfulTimeout)
		defer firstMeaningfulTimer.Stop()
		firstMeaningfulDeadlineCh = firstMeaningfulTimer.C
	}

	// sub2api fu70 codex round-two-stage-header (2026-05-24): early meta
	// flush timer. When request matches narrow gate (X-GCR-Early-Flush
	// header OR body > 64KB) AND cfg.Gateway.EarlyMetaFlushAfterMs > 0,
	// arm a short timer; on fire, if message_start is already in pending
	// events but no meaningful event yet, flush header proactively. Solves
	// the "OpenAI sends 20-60s of metadata before first text/thinking"
	// slow path. Does NOT change empty-stream behavior (still goes via
	// firstMeaningfulTimeout → 502).
	earlyFlushDelay := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.EarlyMetaFlushAfterMs > 0 && isEarlyFlushEligible(c.Request, inboundBodyLen) {
		earlyFlushDelay = time.Duration(s.cfg.Gateway.EarlyMetaFlushAfterMs) * time.Millisecond
	}
	var earlyFlushDeadlineCh <-chan time.Time
	var earlyFlushTimer *time.Timer
	if earlyFlushDelay > 0 {
		earlyFlushTimer = time.NewTimer(earlyFlushDelay)
		defer earlyFlushTimer.Stop()
		earlyFlushDeadlineCh = earlyFlushTimer.C
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
			ClientDisconnect:  clientDisconnected,
		}
	}

	clientDisconnectLogFields := func(stage string) []zap.Field {
		clientReqID, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		ftMs := 0
		if firstTokenMs != nil {
			ftMs = *firstTokenMs
		}
		fmMs := 0
		if firstMeaningfulMs != nil {
			fmMs = *firstMeaningfulMs
		}
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
		return []zap.Field{
			zap.String("request_id", requestID),
			zap.String("client_request_id", clientReqID),
			zap.String("gcr_request_id", c.Request.Header.Get("X-GCR-Request-Id")),
			zap.String("newapi_request_id", c.Request.Header.Get("X-Newapi-Request-Id")),
			zap.String("oneapi_request_id", c.Request.Header.Get("X-Oneapi-Request-Id")),
			zap.String("disconnect_stage", stage),
			zap.String("model", originalModel),
			zap.Int64("elapsed_ms", time.Since(startTime).Milliseconds()),
			zap.Int("first_token_ms", ftMs),
			zap.Int("first_meaningful_ms", fmMs),
			zap.Bool("header_written", headerWritten),
			zap.Int("inbound_body_len", inboundBodyLen),
			zap.Bool("large_context_request", IsLargeContextCtx(c.Request.Context())),
			zap.String("gcr_depth_bucket", c.Request.Header.Get("X-GCR-Depth-Bucket")),
			zap.String("gcr_estimated_tokens", c.Request.Header.Get("X-GCR-Estimated-Tokens")),
			zap.Int64("account_id", accID),
			zap.String("account_name", accName),
			zap.String("account_platform", accPlat),
			zap.String("account_type", accType),
		}
	}

	// processDataLine handles a single "data: ..." SSE line from upstream.
	processDataLine := func(payload string) bool {
		payload = string(restoreCodexToolNamesFromContext(c, []byte(payload)))
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
		observer.ObserveOpenAI([]byte(payload), event.Type)
		s.parseSSEUsageBytesWithType([]byte(payload), event.Type, &usage)

		eventType := strings.TrimSpace(event.Type)
		isBareErrorEvent := eventType == "error"
		isTerminalEvent := isOpenAICompatResponsesTerminalEvent(eventType) || isBareErrorEvent
		if isTerminalEvent {
			terminalEventType = eventType
			if event.Response != nil {
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
			if event.Usage != nil {
				usage = copyOpenAIUsageFromResponsesUsage(event.Usage)
			}

			if eventType == "response.failed" || isBareErrorEvent {
				payloadBytes := []byte(payload)
				message := extractOpenAISSEErrorMessage(payloadBytes)
				if hit, code, msg := detectOpenAICyberPolicy(payloadBytes); hit {
					MarkOpsCyberPolicy(c, CyberPolicyMark{
						Code: code, Message: msg, Body: truncateString(payload, 4096),
						UpstreamStatus: http.StatusOK, UpstreamInTok: usage.InputTokens, UpstreamOutTok: usage.OutputTokens,
					})
					if !clientDisconnected {
						writeStreamHeader()
						clientMsg := cyberPolicyClientMessage(msg, payloadBytes)
						if _, err := fmt.Fprint(c.Writer, buildAnthropicStreamErrorSSE("invalid_request_error", clientMsg)); err == nil {
							c.Writer.Flush()
						}
						clientDisconnected = true
					}
					return true
				}
				// Once Anthropic output has started, switching accounts would splice
				// two model streams together. Surface a proper Anthropic error event
				// instead of returning a failover error that the handler cannot retry.
				shouldFailover := openAIStreamFailedEventShouldFailover(payloadBytes, message)
				if isBareErrorEvent {
					shouldFailover = openAIStreamErrorEventShouldFailover(payloadBytes, message)
				}
				if !clientOutputStarted && shouldFailover {
					streamFailoverErr = s.newOpenAIStreamFailoverError(c, account, false, requestID, payloadBytes, message, resp.Header)
					return true
				}

				message = s.recordOpenAIStreamUpstreamError(c, account, false, requestID, "http_error", payloadBytes, message)
				errStatus, errType, errMsg := http.StatusBadGateway, "api_error", message
				if status, et, em, matched := applyErrorPassthroughRule(
					c, account.Platform, 0, payloadBytes,
					errStatus, errType, errMsg,
				); matched {
					if status == 0 {
						status = errStatus
					}
					if em == "" {
						em = errMsg
					}
					errStatus, errType, errMsg = status, et, em
					MarkResponseCommitted(c)
				}
				if !clientDisconnected {
					if !clientOutputStarted {
						writeAnthropicError(c, errStatus, errType, errMsg)
						clientOutputStarted = true
					} else {
						writeStreamHeader()
						if _, err := fmt.Fprint(c.Writer, buildAnthropicStreamErrorSSE(errType, errMsg)); err == nil {
							c.Writer.Flush()
						}
					}
				}
				streamNonFailoverErr = fmt.Errorf("upstream response failed: %s", errMsg)
				return true
			}
		}

		// Convert to Anthropic events
		events := apicompat.ResponsesEventToAnthropicEvents(&event, state)
		// The exact compatibility probe intentionally terminates once the
		// upstream has returned real search sources. This cancels the remaining
		// model-authored answer to avoid the observed 15-25s tail. Since the
		// upstream terminal usage will not arrive, expose the converter's
		// non-zero estimate for accounting; ordinary WebSearch never enters
		// this branch and continues to use terminal upstream usage. The result
		// intentionally carries no upstream ResponseID because the cancelled
		// response must never be reused as previous_response_id.
		lowLatencyWebSearchComplete := lowLatencyWebSearchProbe &&
			state.MessageStopSent && !isTerminalEvent
		if lowLatencyWebSearchComplete {
			if usage.InputTokens <= 0 {
				usage.InputTokens = max(1, state.RawTotalInputTokens)
			}
			if usage.OutputTokens <= 0 {
				usage.OutputTokens = max(1, state.RawOutputTokens)
			}
		}

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
				return isTerminalEvent || lowLatencyWebSearchComplete
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
							clientDisconnectLogFields("initial_flush")...,
						)
						break
					}
				}
				if !clientDisconnected {
					c.Writer.Flush()
					clientOutputStarted = true
				}
			}
			pendingEvents = nil // 释放, 后续走 normal 路径
			return isTerminalEvent || lowLatencyWebSearchComplete
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
				writeStreamHeader()
				if _, err := fmt.Fprint(c.Writer, sse); err != nil {
					clientDisconnected = true
					disconnectedAt = time.Now()
					logger.L().Info("openai messages stream: client disconnected, continuing to drain upstream for billing",
						clientDisconnectLogFields("forward")...,
					)
					break
				}
				clientOutputStarted = true
			}
		}
		if len(events) > 0 && !clientDisconnected {
			c.Writer.Flush()
		}
		return isTerminalEvent || lowLatencyWebSearchComplete
	}

	// finalizeStream sends any remaining Anthropic events and returns the result.
	finalizeStream := func() (*OpenAIForwardResult, error) {
		if streamFailoverErr != nil {
			return resultWithUsage(), streamFailoverErr
		}
		if streamNonFailoverErr != nil {
			return resultWithUsage(), streamNonFailoverErr
		}
		if finalEvents := apicompat.FinalizeResponsesAnthropicStream(state); len(finalEvents) > 0 && !clientDisconnected {
			for _, evt := range finalEvents {
				sse, err := apicompat.ResponsesAnthropicEventToSSE(evt)
				if err != nil {
					continue
				}
				writeStreamHeader()
				if _, err := fmt.Fprint(c.Writer, sse); err != nil {
					clientDisconnected = true
					disconnectedAt = time.Now()
					logger.L().Info("openai messages stream: client disconnected during final flush",
						clientDisconnectLogFields("final_flush")...,
					)
					break
				}
				clientOutputStarted = true
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
		durationFinal := time.Since(startTime)
		isLargeContextFinal := IsLargeContextCtx(ctxFinal)
		summaryMsg := "openai messages stream: large_context_request summary"
		if !isLargeContextFinal {
			summaryMsg = "openai messages stream: slow_stream_request summary"
		}
		if isLargeContextFinal || durationFinal >= 10*time.Second || fmMsFinal >= 5000 {
			logger.L().Info(summaryMsg,
				zap.String("request_id", requestID),
				zap.String("client_request_id", cliReqIDFinal),
				zap.String("gcr_request_id", c.Request.Header.Get("X-GCR-Request-Id")),
				zap.String("newapi_request_id", c.Request.Header.Get("X-Newapi-Request-Id")),
				zap.String("oneapi_request_id", c.Request.Header.Get("X-Oneapi-Request-Id")),
				zap.String("model", originalModel),
				zap.Int("inbound_body_len", inboundBodyLen),
				zap.Bool("large_context_request", isLargeContextFinal),
				zap.String("gcr_depth_bucket", c.Request.Header.Get("X-GCR-Depth-Bucket")),
				zap.String("gcr_estimated_tokens", c.Request.Header.Get("X-GCR-Estimated-Tokens")),
				zap.Int("first_token_ms", ftMsFinal),
				zap.Int("first_meaningful_ms", fmMsFinal),
				zap.Int("upstream_cached_input_tokens", state.RawCachedInputTokens),
				zap.Int("upstream_total_input_tokens", state.RawTotalInputTokens),
				zap.Duration("total_duration", durationFinal),
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
				// codex round36 fu55 / round37 fu56 (2026-05-20):
				// observability for the fu54 turn_state cache.
				//
				// has_turn_state and turn_state_hit are SYNONYMS — both
				// reflect the INBOUND sub2api cache lookup at request time.
				// fu55 originally claimed has_turn_state was outbound;
				// codex round37 corrected that — it has always been
				// inbound (codex round 11al introduced it as such). The
				// duplicate field name is kept for grep compatibility
				// across the older and newer ops log query templates.
				//
				// upstream_turn_state_returned is the TRUE outbound signal
				// (resp.Header["x-codex-turn-state"] != "" for this turn).
				// Together they form the fu54 effectiveness story:
				//   turn_state_hit=true                 → our cache fed prior state IN
				//   upstream_turn_state_returned=true   → upstream emitted fresh state OUT
				zap.String("turn_state_key_source", meta.TurnStateKeySource),
				zap.Bool("turn_state_hit", meta.TurnStateCacheHit),
				zap.Bool("upstream_turn_state_returned", meta.UpstreamTurnStateReturned),
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
			if isLargeContextFinal && state.RawCachedInputTokens == 0 {
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
					zap.Duration("total_duration", durationFinal),
				)
			}
		}
		logOpenAISuccessMissingUsage(c.Request.Context(), c, account, resp, &usage, terminalEventType, clientDisconnected)
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
		result := resultWithUsage()
		if clientDisconnected {
			return result, fmt.Errorf("stream usage incomplete: missing terminal event")
		}
		message := "OpenAI messages stream ended before a terminal event"
		if !clientOutputStarted {
			return result, s.newOpenAIStreamFailoverError(c, account, false, requestID, nil, message)
		}
		s.recordOpenAIMessagesStreamUpstreamError(c, account, requestID, "stream_missing_terminal", message)
		return result, fmt.Errorf("stream usage incomplete: missing terminal event")
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

		case <-earlyFlushDeadlineCh:
			// sub2api fu70 codex round-two-stage-header (2026-05-24): Stage 2.
			// EarlyMetaFlushAfterMs has elapsed without firstMeaningfulSeen.
			// If pendingEvents has message_start, flush proactively so the
			// client sees Claude SSE start. If no message_start yet (upstream
			// hasn't sent anything beyond raw bytes), leave the gate closed —
			// firstMeaningfulTimeout still handles true empty-stream → 502.
			if firstMeaningfulSeen {
				// late timer, ignore
				continue
			}
			if !hasMessageStartInPending(pendingEvents) {
				// no message_start yet — preserve clean-failover window;
				// firstMeaningfulTimeout will still fire if upstream stays silent
				continue
			}
			firstMeaningfulSeen = true
			fmMs := int(time.Since(startTime).Milliseconds())
			firstMeaningfulMs = &fmMs
			writeStreamHeader()
			if firstMeaningfulTimer != nil {
				firstMeaningfulTimer.Stop()
			}
			logger.L().Info("openai messages stream: early meta flush triggered",
				zap.String("request_id", requestID),
				zap.String("model", originalModel),
				zap.Int("pending_events", len(pendingEvents)),
				zap.Int("early_flush_after_ms", int(earlyFlushDelay.Milliseconds())),
				zap.String("gcr_request_id", c.Request.Header.Get("X-GCR-Request-Id")),
				zap.String("newapi_request_id", c.Request.Header.Get("X-Newapi-Request-Id")),
				zap.Int("inbound_body_len", inboundBodyLen),
			)
			if !clientDisconnected {
				for _, evt := range pendingEvents {
					sse, err := apicompat.ResponsesAnthropicEventToSSE(evt)
					if err != nil {
						logger.L().Warn("openai messages stream: early flush failed to marshal pending event",
							zap.Error(err),
							zap.String("request_id", requestID),
						)
						continue
					}
					if _, err := fmt.Fprint(c.Writer, sse); err != nil {
						logger.L().Warn("openai messages stream: early flush write failed",
							zap.Error(err),
							zap.String("request_id", requestID),
						)
						clientDisconnected = true
						break
					}
				}
				if !clientDisconnected {
					c.Writer.Flush()
					clientOutputStarted = true
				}
			}
			pendingEvents = nil

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
				zap.Int("tools_count", meta.ToolsCount),
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
						clientOutputStarted = true
					}
				}
				continue
			}

			if !shouldEmitKeepalivePing(clientDisconnected, firstMeaningfulSeen, time.Since(lastDataAt), keepaliveInterval) {
				continue
			}
			// Send Anthropic-format ping event
			writeStreamHeader()
			if _, err := fmt.Fprint(c.Writer, "event: ping\ndata: {\"type\":\"ping\"}\n\n"); err != nil {
				// Client disconnected
				logger.L().Info("openai messages stream: client disconnected during keepalive",
					clientDisconnectLogFields("keepalive")...,
				)
				clientDisconnected = true
				disconnectedAt = time.Now()
				continue
			}
			clientOutputStarted = true
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

// buildAnthropicStreamErrorSSE builds an Anthropic error event for failures
// discovered after a streaming response has already started.
func buildAnthropicStreamErrorSSE(errType, message string) string {
	payload, err := json.Marshal(gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
	if err != nil {
		return "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream error\"}}\n\n"
	}
	return "event: error\ndata: " + string(payload) + "\n\n"
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
