package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// OpenAIGatewayHandler handles OpenAI API gateway requests
type OpenAIGatewayHandler struct {
	gatewayService             *service.OpenAIGatewayService
	billingCacheService        *service.BillingCacheService
	apiKeyService              *service.APIKeyService
	usageRecordWorkerPool      *service.UsageRecordWorkerPool
	errorPassthroughService    *service.ErrorPassthroughService
	contentModerationService   *service.ContentModerationService
	grokMediaEligibilityProber grokMediaEligibilityProber
	opsService                 *service.OpsService
	concurrencyHelper          *ConcurrencyHelper
	imageLimiter               *imageConcurrencyLimiter
	maxAccountSwitches         int
	cfg                        *config.Config
}

type grokMediaEligibilityProber interface {
	ProbeMediaEligibility(ctx context.Context, accountID int64) (bool, string, error)
}

const openAIRemoteCompactRequestBodyBytesKey = "openai_remote_compact_request_body_bytes"

var errOpenAIWSUnsupportedModelSwitch = errors.New("selected account does not support websocket model switch")

func newOpenAIWSUnsupportedModelSwitchError(model string) error {
	cause := fmt.Errorf("%w: model %q", errOpenAIWSUnsupportedModelSwitch, strings.TrimSpace(model))
	return service.NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "model switch requires reconnect", cause)
}

func shouldReportOpenAIWSProxyAccountFailure(err error) bool {
	return err != nil && !errors.Is(err, errOpenAIWSUnsupportedModelSwitch) && !service.IsOpenAIWSSessionPreemptedError(err)
}

// openAIWSIngressEndedByClient reports whether a finished ingress WebSocket turn
// ended the way a healthy client ends one, rather than through an upstream or
// account fault.
//
// Three error shapes describe that same benign outcome and only the first was
// recognised:
//
//   - *service.OpenAIWSClientCloseError carrying 1000 — the gateway closing the
//     socket on its own terms, e.g. the inter-turn idle timeout.
//   - a bare coderws.CloseError{Code: 1000} — what coder/websocket returns when
//     the client closes cleanly. ReadOpenAIWSClientMessage hands conn.Read's
//     error back verbatim, so nothing ever wraps it into the type above and an
//     errors.As against that type cannot see it.
//   - context.Canceled — the client went away mid-turn. That path closes with
//     StatusGoingAway (1001) and carries the cancellation as its cause, so a
//     check for 1000 alone never matched it either.
//
// The last two fell through to shouldReportOpenAIWSProxyAccountFailure, which
// filters only model-switch and session-preemption errors. Everything else
// reaches ObserveOpenAIAPIKeyHealthFailure and scheduler.ReportResult(false), so
// a client that merely disconnected counted against the upstream account's
// health and could trip it out of scheduling.
//
// failoverClientGone already states the rule this restores for the HTTP failover
// path — a cancelled client context "被误报成账号耗尽" is a bug, not a signal —
// and summarizeWSCloseErrorForLog already reads the close code the correct way,
// which is why the resulting WARN printed close_status=1000(StatusNormalClosure)
// for an error that was, in the same breath, being charged to the account.
//
// Deliberately narrow. StatusGoingAway is not matched on its own: the gateway
// emits 1001 when it tears a session down for its own reasons too, and the
// client-cancellation case is already covered by context.Canceled.
// context.DeadlineExceeded is left out as well — the idle-timeout path wraps it
// in a 1000 close error and stays benign through the first check, while any
// other deadline is a genuine stall worth reporting.
func openAIWSIngressEndedByClient(err error) bool {
	if err == nil {
		return true
	}
	var closeErr *service.OpenAIWSClientCloseError
	if errors.As(err, &closeErr) && closeErr.StatusCode() == coderws.StatusNormalClosure {
		return true
	}
	if coderws.CloseStatus(err) == coderws.StatusNormalClosure {
		return true
	}
	return errors.Is(err, context.Canceled)
}

func openAIWSTurnBillingModel(result *service.OpenAIForwardResult, mapping service.ChannelMappingResult, requestedModel, upstreamModel string) string {
	billingModel := ""
	if result != nil {
		billingModel = strings.TrimSpace(result.BillingModel)
	}
	if billingModel == "" {
		billingModel = strings.TrimSpace(upstreamModel)
	}
	if billingModel == "" {
		billingModel = strings.TrimSpace(requestedModel)
	}

	requestedModel = strings.TrimSpace(requestedModel)
	switch mapping.BillingModelSource {
	case service.BillingModelSourceRequested:
		if requestedModel != "" {
			billingModel = requestedModel
		}
	case service.BillingModelSourceChannelMapped:
		mappedModel := strings.TrimSpace(mapping.MappedModel)
		if mappedModel != "" && mappedModel != requestedModel {
			billingModel = mappedModel
		}
	}
	return billingModel
}

func resolveOpenAIMessagesDispatchMappedModel(c *gin.Context, apiKey *service.APIKey, requestedModel string) string {
	if apiKey == nil || apiKey.Group == nil {
		return ""
	}
	// composite 解析到 grok/CN 目标时调度级映射不适用（Group 级映射的 gpt-5.x
	// 默认值是 openai 专属,发给这些上游必错）,模型改写交给账号级 model_mapping。
	if apiKey.Group.Platform == service.PlatformComposite && c != nil && c.Request != nil {
		if platform, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context()); ok &&
			(platform == service.PlatformGrok || service.IsCNProvider(platform)) {
			return ""
		}
	}
	return strings.TrimSpace(apiKey.Group.ResolveMessagesDispatchModel(requestedModel))
}

type openAIModelBodyReplaceFunc func([]byte, string) []byte

func openAIForwardSucceededForScheduling(result *service.OpenAIForwardResult) bool {
	return result.SucceededForScheduling()
}

func openAIModelMappedBody(body []byte, mapped bool, mappedModel string, replace openAIModelBodyReplaceFunc) []byte {
	if !mapped || replace == nil {
		return body
	}
	return replace(body, mappedModel)
}

func openAIAccountScheduleModel(c *gin.Context, account *service.Account, forwardModel string, requireCompact bool, result *service.OpenAIForwardResult) string {
	if result != nil {
		if actual := strings.TrimSpace(result.UpstreamModel); actual != "" {
			return actual
		}
	}
	if c != nil {
		if value, ok := c.Get(service.OpsUpstreamModelKey); ok {
			if actual, ok := value.(string); ok && strings.TrimSpace(actual) != "" {
				return strings.TrimSpace(actual)
			}
		}
	}
	return service.ResolveOpenAIAccountUpstreamModelForRequest(account, forwardModel, requireCompact)
}

func seedOpenAIForwardImageIntentHint(c *gin.Context, channelMapped bool, imageIntent bool) {
	if channelMapped {
		// The mapped model/body becomes canonical inside Forward; leave the hint
		// unknown so the service classifies that payload rather than the inbound one.
		return
	}
	service.SetOpenAIImageIntentHint(c, imageIntent)
}

func newOpenAIModelMappedBodyCache(body []byte, replace openAIModelBodyReplaceFunc) func(bool, string) []byte {
	replacedBodies := make(map[string][]byte)
	return func(mapped bool, mappedModel string) []byte {
		if !mapped {
			return body
		}
		if cachedBody, ok := replacedBodies[mappedModel]; ok {
			return cachedBody
		}
		replacedBody := openAIModelMappedBody(body, true, mappedModel, replace)
		replacedBodies[mappedModel] = replacedBody
		return replacedBody
	}
}

func usageRecordContext(parent context.Context, base context.Context) context.Context {
	if base == nil {
		base = context.Background()
	}
	if parent == nil {
		return base
	}
	if clientRequestID, _ := parent.Value(ctxkey.ClientRequestID).(string); strings.TrimSpace(clientRequestID) != "" {
		base = context.WithValue(base, ctxkey.ClientRequestID, strings.TrimSpace(clientRequestID))
	}
	if requestID, _ := parent.Value(ctxkey.RequestID).(string); strings.TrimSpace(requestID) != "" {
		base = context.WithValue(base, ctxkey.RequestID, strings.TrimSpace(requestID))
	}
	return base
}

func wrapUsageRecordTaskContext(parent context.Context, task service.UsageRecordTask) service.UsageRecordTask {
	if task == nil {
		return nil
	}
	return func(ctx context.Context) {
		task(usageRecordContext(parent, ctx))
	}
}

func openAICompatibleRequestPlatform(ctx context.Context, apiKey *service.APIKey) string {
	if platform, ok := service.ResolvedTargetPlatformFromContext(ctx); ok {
		// 保留 grok 与国产供应商原值，其他归一为 openai（与调度器精确匹配语义一致）。
		return service.NormalizeOpenAICompatiblePlatform(platform)
	}
	if apiKey != nil && apiKey.Group != nil {
		return service.NormalizeOpenAICompatiblePlatform(apiKey.Group.Platform)
	}
	return service.PlatformOpenAI
}

func openAIResponsesRequiredCapability(imageIntent bool, platform string) service.OpenAIEndpointCapability {
	if imageIntent && platform == service.PlatformOpenAI {
		return service.OpenAIEndpointCapabilityResponses
	}
	return service.OpenAIEndpointCapabilityChatCompletions
}

func openAIResponsesRequiredCapabilityForRequest(imageIntent bool, needsResponses bool, platform string) service.OpenAIEndpointCapability {
	if needsResponses && platform == service.PlatformOpenAI {
		return service.OpenAIEndpointCapabilityResponses
	}
	return openAIResponsesRequiredCapability(imageIntent, platform)
}

func allowOpenAICompatibleMessagesDispatch(c *gin.Context, apiKey *service.APIKey) bool {
	if apiKey == nil || apiKey.Group == nil {
		return true
	}
	if apiKey.Group.Platform == service.PlatformGrok {
		return true
	}
	// 国产供应商分组与 grok 同语义:/v1/messages 就是其主要服务形态(anthropic
	// 协议账号原生直通 Claude Code),无需 allow_messages_dispatch 开关授权——
	// 该开关对非 openai/composite 平台恒被 sanitizeGroupMessagesDispatchFields 置 false,
	// 若不豁免,CN 分组将永远 403。
	if service.IsCNProvider(apiKey.Group.Platform) {
		return true
	}
	// composite 分组解析到 grok/CN 目标时与对应独立分组同语义豁免；
	// 解析到 openai 目标则受 composite 分组自身的可配置开关控制。
	if apiKey.Group.Platform == service.PlatformComposite && c != nil && c.Request != nil {
		if platform, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context()); ok &&
			(platform == service.PlatformGrok || service.IsCNProvider(platform)) {
			return true
		}
	}
	return apiKey.Group.AllowMessagesDispatch
}

func openAICompatibleTextTargetAllowed(c *gin.Context, apiKey *service.APIKey, model string) bool {
	return compositeTargetPlatformAllowed(c, apiKey, model,
		service.PlatformOpenAI, service.PlatformGrok,
		service.PlatformKimi, service.PlatformZhipu, service.PlatformDeepseek)
}

// isResponsesWebSocketCompositePlatform 限定 composite 分组在 Responses WebSocket
// 上可服务的目标平台。CN 供应商（kimi/zhipu/deepseek）刻意排除：其账号无法通过
// WSv2 ingress 的 transport 过滤，且 WS HTTP 桥没有面向 CN 的 Responses 转换，
// 放行只会把明确的策略拒绝变成误导性的 "no available account"。
func isResponsesWebSocketCompositePlatform(platform string) bool {
	switch platform {
	case service.PlatformOpenAI, service.PlatformGrok:
		return true
	default:
		return false
	}
}

// NewOpenAIGatewayHandler creates a new OpenAIGatewayHandler
func NewOpenAIGatewayHandler(
	gatewayService *service.OpenAIGatewayService,
	concurrencyService *service.ConcurrencyService,
	billingCacheService *service.BillingCacheService,
	apiKeyService *service.APIKeyService,
	usageRecordWorkerPool *service.UsageRecordWorkerPool,
	errorPassthroughService *service.ErrorPassthroughService,
	contentModerationService *service.ContentModerationService,
	opsService *service.OpsService,
	grokQuotaService *service.GrokQuotaService,
	cfg *config.Config,
) *OpenAIGatewayHandler {
	pingInterval := time.Duration(0)
	maxAccountSwitches := 3
	if cfg != nil {
		pingInterval = time.Duration(cfg.Concurrency.PingInterval) * time.Second
		if cfg.Gateway.MaxAccountSwitches > 0 {
			maxAccountSwitches = cfg.Gateway.MaxAccountSwitches
		}
	}
	return &OpenAIGatewayHandler{
		gatewayService:             gatewayService,
		billingCacheService:        billingCacheService,
		apiKeyService:              apiKeyService,
		usageRecordWorkerPool:      usageRecordWorkerPool,
		errorPassthroughService:    errorPassthroughService,
		contentModerationService:   contentModerationService,
		grokMediaEligibilityProber: grokQuotaService,
		opsService:                 opsService,
		concurrencyHelper:          NewConcurrencyHelper(concurrencyService, SSEPingFormatComment, pingInterval),
		imageLimiter:               &imageConcurrencyLimiter{},
		maxAccountSwitches:         maxAccountSwitches,
		cfg:                        cfg,
	}
}

// Responses handles OpenAI Responses API endpoint
// POST /openai/v1/responses
func (h *OpenAIGatewayHandler) Responses(c *gin.Context) {
	// 局部兜底：确保该 handler 内部任何 panic 都不会击穿到进程级。
	streamStarted := false
	defer h.recoverResponsesPanic(c, &streamStarted)
	compactStartedAt := time.Now()
	defer h.logOpenAIRemoteCompactOutcome(c, compactStartedAt)
	setOpenAIClientTransportHTTP(c)

	requestStart := time.Now()

	// Get apiKey and user from context (set by ApiKeyAuth middleware)
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(
		c,
		"handler.openai_gateway.responses",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	// Read request body
	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		logRequestBodyReadFailure(reqLog, c.Request, err)
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}

	if len(body) == 0 {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}
	if isOpenAIRemoteCompactPath(c) {
		c.Set(openAIRemoteCompactRequestBodyBytesKey, len(body))
	}

	setOpsRequestContext(c, "", false, body)
	sessionHashBody := body
	body, ok = h.normalizeOpenAIResponsesCompactRequest(c, reqLog, body)
	if !ok {
		return
	}
	legacyCompact := service.IsOpenAIResponsesCompactPath(c)
	nativeV2 := isBareOpenAIResponsesPath(c) && isOpenAIRemoteCompactionV2Request(c, body)
	if nativeV2 {
		service.MarkOpenAINativeCompactionV2(c)
	}
	if isOpenAIRemoteCompactPath(c) {
		c.Set(openAIRemoteCompactRequestBodyBytesKey, len(sessionHashBody))
	}
	// body-signal compact：上游 unary 等待期间向下游发 SSE 注释行心跳，防止
	// 反向代理空闲超时掐断长压缩连接（#3887）。首拍延迟一个心跳间隔，快速
	// 失败仍走 JSON+状态码链路；未标记客户端流式或间隔为 0 时是 no-op。
	stopCompactKeepalive := service.StartOpenAICompactSSEKeepalive(c, h.openAICompactKeepaliveInterval())
	defer stopCompactKeepalive()

	// 校验请求体 JSON 合法性
	if !gjson.ValidBytes(body) {
		logRequestBodyParseFailure(reqLog, body, nil)
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}

	// 使用 gjson 只读提取字段做校验，避免完整 Unmarshal
	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.Type != gjson.String || modelResult.String() == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	reqModel := modelResult.String()
	ensureCompositeTargetPlatform(c, apiKey, reqModel)
	if !openAICompatibleTextTargetAllowed(c, apiKey, reqModel) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Model is not supported by this OpenAI-compatible endpoint for composite groups")
		return
	}
	if cappedBody, changed, err := applyOpenAIReasoningEffortPolicyForRequest(c, apiKey, body); err != nil {
		respondOpenAIReasoningEffortPolicyError(c, err, h.errorResponse)
		return
	} else if changed {
		body = cappedBody
	}
	if normalizedBody, changed := normalizeCodexAutomationBootstrap(body); changed {
		body = normalizedBody
		reqLog.Info("openai.codex_automation_bootstrap_normalized",
			zap.String("normalization", "call_output_to_user_message"),
		)
	}
	if normalizedBody, changed := normalizeCodexDelegationBootstrap(body); changed {
		body = normalizedBody
		reqLog.Info("openai.codex_delegation_bootstrap_normalized",
			zap.String("normalization", "call_output_to_user_message"),
		)
	}

	reqStream, ok := parseOpenAICompatibleStream(body)
	if !ok {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", invalidStreamFieldTypeMessage)
		return
	}
	if _, err := service.ValidateOpenAIServiceTierField(body); err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", reqStream))
	previousResponseID := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
	if previousResponseID != "" {
		previousResponseIDKind := service.ClassifyOpenAIPreviousResponseIDKind(previousResponseID)
		reqLog = reqLog.With(
			zap.Bool("has_previous_response_id", true),
			zap.String("previous_response_id_kind", previousResponseIDKind),
			zap.Int("previous_response_id_len", len(previousResponseID)),
		)
		if previousResponseIDKind == service.OpenAIPreviousResponseIDKindMessageID {
			reqLog.Warn("openai.request_validation_failed",
				zap.String("reason", "previous_response_id_looks_like_message_id"),
			)
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "previous_response_id must be a response.id (resp_*), not a message id")
			return
		}
		groupID := int64(0)
		if apiKey.GroupID != nil {
			groupID = *apiKey.GroupID
		}
		owned, ownershipErr := h.gatewayService.ValidateOpenAIHTTPResponseOwner(
			c.Request.Context(),
			groupID,
			previousResponseID,
			subject.UserID,
			apiKey.ID,
		)
		if ownershipErr != nil {
			reqLog.Warn("openai.previous_response_owner_lookup_failed", zap.Error(ownershipErr))
		}
		if !owned {
			reqLog.Warn("openai.request_validation_failed", zap.String("reason", "previous_response_owner_mismatch"))
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "previous_response_id is not available for this user")
			return
		}
	}
	service.SetOpenAIHTTPResponseOwner(c, subject.UserID, apiKey.ID)

	setOpsRequestContext(c, reqModel, reqStream, body)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))

	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIResponses, reqModel, body); decision != nil && decision.Blocked {
		h.errorResponse(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
		return
	}
	imageIntent := service.IsImageGenerationIntent("/v1/responses", reqModel, body)
	if imageIntent && !service.GroupAllowsImageGeneration(apiKey.Group) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", service.ImageGenerationPermissionMessage())
		return
	}
	if imageIntent {
		imageReleaseFunc, imageAcquired := h.acquireImageGenerationSlot(c, streamStarted)
		if !imageAcquired {
			return
		}
		if imageReleaseFunc != nil {
			defer imageReleaseFunc()
		}
	}

	// 解析渠道级模型映射
	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)
	forwardBody := openAIModelMappedBody(body, channelMapping.Mapped, channelMapping.MappedModel, h.gatewayService.ReplaceModelInBody)
	seedOpenAIForwardImageIntentHint(c, channelMapping.Mapped, imageIntent)
	forwardModel := reqModel
	if channelMapping.Mapped {
		forwardModel = channelMapping.MappedModel
	}
	c.Request = c.Request.WithContext(service.WithOpenAIForwardModel(c.Request.Context(), forwardModel, legacyCompact))

	// 提前校验 function_call_output 是否具备可关联上下文，避免上游 400。
	if !h.validateFunctionCallOutputRequest(c, body, reqLog) {
		return
	}

	// 绑定错误透传服务，允许 service 层在非 failover 错误场景复用规则。
	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}

	// Get subscription info (may be nil)
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	requestPlatform := openAICompatibleRequestPlatform(c.Request.Context(), apiKey)

	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())
	routingStart := time.Now()

	userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, reqStream, &streamStarted, reqLog)
	if !acquired {
		return
	}
	// 确保请求取消时也会释放槽位，避免长连接被动中断造成泄漏
	if userReleaseFunc != nil {
		defer userReleaseFunc()
	}

	// 2. Re-check billing eligibility after wait
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.handleStreamingAwareError(c, status, code, message, streamStarted)
		return
	}

	// Generate session hash (header first; fallback to prompt_cache_key)
	sessionHash := h.gatewayService.GenerateSessionHash(c, sessionHashBody)
	if h.rejectIfCyberSessionBlocked(c, apiKey, sessionHashBody, reqModel, cyberBlockFormatResponses) {
		return
	}
	c.Request = c.Request.WithContext(service.WithOpenAIGuardianParentAffinity(
		c.Request.Context(), c, sessionHashBody, reqModel,
	))
	requireCompact := legacyCompact

	maxAccountSwitches := h.maxAccountSwitches
	switchCount := 0
	profitVetoCount := 0
	failedAccountIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	var lastFailoverErr *service.UpstreamFailoverError
	var passthroughFailoverState openAIPassthroughFailoverState
	var oauth429FailoverState service.OpenAIOAuth429FailoverState
	needsResponses := nativeV2 || legacyCompact
	requiredCapability := openAIResponsesRequiredCapabilityForRequest(imageIntent, needsResponses, requestPlatform)
	pricingCtx, pricingAt := h.gatewayService.WithOpenAIRequestPricingContext(c.Request.Context(), apiKey.GroupID)
	c.Request = c.Request.WithContext(pricingCtx)

	for {
		if c.Request.Context().Err() != nil {
			return
		}
		// Select account supporting the requested model
		reqLog.Debug("openai.account_selecting", zap.Int("excluded_account_count", len(failedAccountIDs)))
		selection, scheduleDecision, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
			c.Request.Context(),
			apiKey.GroupID,
			previousResponseID,
			sessionHash,
			reqModel,
			failedAccountIDs,
			service.OpenAIUpstreamTransportAny,
			requiredCapability,
			requireCompact,
			false,
			!imageIntent,
			requestPlatform,
		)
		if err != nil {
			reqLog.Warn("openai.account_select_failed",
				zap.Error(err),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if len(failedAccountIDs) == 0 {
				if legacyCompact && errors.Is(err, service.ErrNoAvailableCompactAccounts) {
					markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
					h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "compact_not_supported", "No available accounts support /responses/compact", streamStarted)
					return
				}
				cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel, requestPlatform)
				cls = classifySelectionFailureError(err, cls)
				if !cls.ModelNotFound {
					markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
				}
				h.handleStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
				return
			}
			if lastFailoverErr != nil {
				h.handleFailoverExhausted(c, lastFailoverErr, streamStarted)
			} else {
				h.handleFailoverExhaustedSimple(c, 502, streamStarted)
			}
			return
		}
		if selection == nil || selection.Account == nil {
			cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel, requestPlatform)
			if !cls.ModelNotFound {
				markOpsRoutingCapacityLimited(c)
			}
			h.handleStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
			return
		}
		if previousResponseID != "" && selection != nil && selection.Account != nil {
			reqLog.Debug("openai.account_selected_with_previous_response_id", zap.Int64("account_id", selection.Account.ID))
		}
		reqLog.Debug("openai.account_schedule_decision",
			zap.String("layer", scheduleDecision.Layer),
			zap.Bool("sticky_previous_hit", scheduleDecision.StickyPreviousHit),
			zap.Bool("sticky_session_hit", scheduleDecision.StickySessionHit),
			zap.Int("candidate_count", scheduleDecision.CandidateCount),
			zap.Int("top_k", scheduleDecision.TopK),
			zap.Int64("latency_ms", scheduleDecision.LatencyMs),
			zap.Float64("load_skew", scheduleDecision.LoadSkew),
		)
		account := selection.Account
		if previousResponseID != "" && requestPlatform == service.PlatformOpenAI && !account.IsOpenAIApiKey() {
			// The public Responses HTTP API supports previous_response_id on API-key
			// accounts. OAuth/SetupToken upstreams do not, so keep searching instead
			// of silently deleting continuation state from a mixed account pool.
			failedAccountIDs[account.ID] = struct{}{}
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
				selection.ReleaseFunc = nil
			}
			lastFailoverErr = &service.UpstreamFailoverError{
				StatusCode:       http.StatusBadRequest,
				Stage:            service.GatewayFailureStageInference,
				Scope:            service.GatewayFailureScopeRequest,
				Reason:           service.OpenAIHTTPContinuationUnsupportedReason,
				ClientStatusCode: http.StatusBadRequest,
				ClientMessage:    "previous_response_id requires an OpenAI API-key account for HTTP requests",
			}
			reqLog.Debug("openai.account_skipped_http_continuation_unsupported",
				zap.Int64("account_id", account.ID),
				zap.String("account_type", account.Type),
			)
			continue
		}
		sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
		reqLog.Debug("openai.account_selected", zap.Int64("account_id", account.ID), zap.String("account_name", account.Name))
		setOpsSelectedAccount(c, account.ID, account.Platform)

		accountReleaseFunc, slotResult := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, reqStream, &streamStarted, reqLog)
		if slotResult == openAISlotAcquireProfitVetoed {
			if !recordOpenAIProfitVeto(failedAccountIDs, account.ID, &profitVetoCount) {
				h.handleOpenAIProfitVetoExhausted(c, streamStarted, reqLog, profitVetoCount)
				return
			}
			continue
		}
		if slotResult != openAISlotAcquireOK {
			return
		}

		// Forward request
		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		forwardStart := time.Now()
		// Compact heartbeat comments are transport keepalive bytes, not a
		// semantic response, so they must not suppress account failover.
		writerSizeBeforeForward := service.OpenAICompactKeepaliveAdjustedWrittenSize(c)
		// 跨 passthrough 边界的 failover：从 Kiro 等透传账号切到 Bedrock 等非透传账号前，
		// 从不可变的 canonical forwardBody 派生本次尝试 body 并整块剔除上游私有的加密
		// reasoning item（含耦合的 id/summary），避免非透传上游 400 拒绝 Kiro reasoning 形态。
		attemptBody := h.deriveOpenAIForwardAttemptBody(reqLog, forwardBody, account, &passthroughFailoverState)
		result, err := func() (*service.OpenAIForwardResult, error) {
			defer func() {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
			}()
			return h.gatewayService.Forward(c.Request.Context(), c, account, attemptBody)
		}()
		var cyberBlockBodyHTTP []byte
		if service.GetOpsCyberPolicy(c) != nil {
			cyberBlockBodyHTTP = sessionHashBody
		}
		h.recordCyberPolicyIfMarked(c, apiKey, account, subscription, reqModel, err != nil, cyberBlockBodyHTTP, clientRequestedUsageFields(c, channelMapping, reqModel, ""), service.HashUsageRequestPayload(body))
		forwardDurationMs := time.Since(forwardStart).Milliseconds()
		upstreamLatencyMs, _ := getContextInt64(c, service.OpsUpstreamLatencyMsKey)
		responseLatencyMs := forwardDurationMs
		if upstreamLatencyMs > 0 && forwardDurationMs > upstreamLatencyMs {
			responseLatencyMs = forwardDurationMs - upstreamLatencyMs
		}
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, responseLatencyMs)
		if err == nil && result != nil && result.FirstTokenMs != nil {
			service.SetOpsLatencyMs(c, service.OpsTimeToFirstTokenMsKey, int64(*result.FirstTokenMs))
		}
		// Errors may carry a partial result with already-observed usage. Submit it
		// exactly once here as well as on success; failover errors return nil.
		submitResponsesUsage := func(res *service.OpenAIForwardResult) {
			if res == nil {
				return
			}
			stampOpenAIRequestedReasoningEffort(res, c)
			userAgent := c.GetHeader("User-Agent")
			clientIP := ip.GetClientIP(c)
			requestPayloadHash := service.HashUsageRequestPayload(body)
			inboundEndpoint := GetInboundEndpoint(c)
			upstreamEndpoint := resolveOpenAIUpstreamEndpoint(c, account, res)
			quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)
			sessionID := service.ExtractClientSessionID(c)
			cyberBlocked := service.GetOpsCyberPolicy(c) != nil
			h.submitOpenAIUsageRecordTask(c.Request.Context(), res, func(ctx context.Context) {
				if recordErr := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
					Result:             res,
					APIKey:             apiKey,
					User:               apiKey.User,
					Account:            account,
					Subscription:       subscription,
					InboundEndpoint:    inboundEndpoint,
					UpstreamEndpoint:   upstreamEndpoint,
					UserAgent:          userAgent,
					IPAddress:          clientIP,
					RequestPayloadHash: requestPayloadHash,
					APIKeyService:      h.apiKeyService,
					QuotaPlatform:      quotaPlatform,
					SessionID:          sessionID,
					ChannelUsageFields: clientRequestedUsageFields(c, channelMapping, reqModel, res.UpstreamModel),
					PricingAt:          pricingAt,
					CyberBlocked:       cyberBlocked,
				}); recordErr != nil {
					reqLog.Error("openai.record_usage_failed", zap.Error(recordErr), zap.Int64("account_id", account.ID))
				}
			})
		}
		// A streamed attempt may have flushed only SSE comments/keepalives.
		// Those bytes commit HTTP 200 and require a terminal SSE error if all
		// retries are exhausted, but they are not semantic output and therefore
		// must not prevent trying another account.
		streamStarted = openAIResponsesTransportCommitted(c, reqStream, streamStarted)
		if err != nil {
			if result != nil && result.ImageCount > 0 {
				reqLog.Warn("openai.forward_partial_error_with_image_result",
					zap.Int64("account_id", account.ID),
					zap.Int("image_count", result.ImageCount),
					zap.Error(err),
				)
			} else {
				var failoverErr *service.UpstreamFailoverError
				if errors.As(err, &failoverErr) {
					if c.Request.Context().Err() != nil {
						return
					}
					semanticStarted := reqStream && service.OpenAIStreamSemanticOutputStarted(c)
					if semanticStarted {
						h.handleFailoverExhausted(c, failoverErr, true)
						return
					}
					if !reqStream && service.OpenAICompactKeepaliveAdjustedWrittenSize(c) != writerSizeBeforeForward {
						h.handleFailoverExhausted(c, failoverErr, true)
						return
					}
					if failoverErr.ShouldReportAccountScheduleFailure() {
						h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, forwardModel, requireCompact, nil), false, nil, err)
					}
					if !failoverErr.ShouldRetryNextAccount() {
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					// 池模式：同账号重试
					if failoverErr.RetryableOnSameAccount {
						retryLimit := effectiveSameAccountRetryLimit(failoverErr, account)
						if sameAccountRetryAllowed(failoverErr, sameAccountRetryCount[account.ID], retryLimit) {
							sameAccountRetryCount[account.ID]++
							retryDelay := sameAccountRetryDelayFor(failoverErr, sameAccountRetryCount[account.ID])
							reqLog.Warn("openai.pool_mode_same_account_retry",
								zap.Int64("account_id", account.ID),
								zap.Int("upstream_status", failoverErr.StatusCode),
								zap.Int("retry_limit", retryLimit),
								zap.Int("retry_count", sameAccountRetryCount[account.ID]),
								zap.Duration("retry_delay", retryDelay),
							)
							select {
							case <-c.Request.Context().Done():
								return
							case <-time.After(retryDelay):
							}
							continue
						}
					}
					h.gatewayService.RecordOpenAIAccountSwitch()
					failedAccountIDs[account.ID] = struct{}{}
					lastFailoverErr = failoverErr
					if switchCount >= maxAccountSwitches {
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					switchCount++
					if h.gatewayService.ShouldStopOpenAIOAuth429Failover(account, failoverErr.StatusCode, switchCount, &oauth429FailoverState) {
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					reqLog.Warn("openai.upstream_failover_switching",
						zap.Int64("account_id", account.ID),
						zap.Int("upstream_status", failoverErr.StatusCode),
						zap.Int("switch_count", switchCount),
						zap.Int("max_switches", maxAccountSwitches),
					)
					continue
				}
				h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, forwardModel, requireCompact, result), false, nil, err)
				upstreamErrorAlreadyCommunicated := openAIForwardErrorAlreadyCommunicated(c, writerSizeBeforeForward, err)
				wroteFallback := false
				if !upstreamErrorAlreadyCommunicated {
					wroteFallback = h.ensureForwardErrorResponse(c, streamStarted)
				}
				fields := []zap.Field{
					zap.Int64("account_id", account.ID),
					zap.Bool("fallback_error_response_written", wroteFallback),
					zap.Bool("upstream_error_response_already_written", upstreamErrorAlreadyCommunicated),
					zap.Error(err),
				}
				submitResponsesUsage(result)
				if shouldLogOpenAIForwardFailureAsWarn(c, wroteFallback) {
					reqLog.Warn("openai.forward_failed", fields...)
					return
				}
				reqLog.Error("openai.forward_failed", fields...)
				return
			}
		}
		if result != nil {
			if account.Type == service.AccountTypeOAuth && !account.IsShadow() {
				h.gatewayService.UpdateCodexUsageSnapshotFromHeaders(c.Request.Context(), account.ID, result.ResponseHeaders)
			}
			h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, forwardModel, requireCompact, result), openAIForwardSucceededForScheduling(result), result.FirstTokenMs)
		} else {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, forwardModel, requireCompact, result), openAIForwardSucceededForScheduling(result), nil)
		}

		submitResponsesUsage(result)
		reqLog.Debug("openai.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", switchCount),
		)
		return
	}
}

// openAIResponsesTransportCommitted tracks the HTTP/SSE transport separately
// from OpenAIStreamSemanticOutputStarted. A comment-only stream is still safe
// to retry, but once its HTTP 200 is on the wire an exhausted retry chain must
// terminate with response.failed SSE instead of appending a JSON response.
func openAIResponsesTransportCommitted(c *gin.Context, reqStream, committed bool) bool {
	if committed || !reqStream {
		return committed
	}
	return service.OpenAIStreamTransportCommitted(c)
}

func isOpenAIRemoteCompactPath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	normalizedPath := strings.TrimRight(strings.TrimSpace(c.Request.URL.Path), "/")
	return strings.HasSuffix(normalizedPath, "/responses/compact")
}

func isOpenAILegacyCompactPath(c *gin.Context) bool {
	return service.IsOpenAIResponsesCompactPath(c)
}

// isBareOpenAIResponsesPath only matches a bare /responses endpoint. Body-
// signal compact promotion must not rewrite /responses/{id}/... routes.
func isBareOpenAIResponsesPath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	normalizedPath := strings.TrimRight(strings.TrimSpace(c.Request.URL.Path), "/")
	switch normalizedPath {
	case EndpointResponses, "/openai/v1/responses", "/responses", "/backend-api/codex/responses":
		return true
	default:
		return false
	}
}

func isOpenAIRemoteCompactionV2Request(c *gin.Context, body []byte) bool {
	stream, valid := parseOpenAICompatibleStream(body)
	if !valid || !stream || !service.HasCompactionTriggerInInput(body) || c == nil || c.Request == nil {
		return false
	}
	for _, header := range c.Request.Header.Values("x-codex-beta-features") {
		for _, feature := range strings.Split(header, ",") {
			if strings.TrimSpace(feature) == "remote_compaction_v2" {
				return true
			}
		}
	}
	return false
}

// normalizeOpenAIResponsesCompactRequest keeps explicit remote_compaction_v2
// traffic on native /responses while preserving the legacy body-signal
// promotion for clients that expect a synthesized Responses SSE stream.
func (h *OpenAIGatewayHandler) normalizeOpenAIResponsesCompactRequest(c *gin.Context, reqLog *zap.Logger, body []byte) ([]byte, bool) {
	isCompactRequest := isOpenAILegacyCompactPath(c)
	if !isCompactRequest && isBareOpenAIResponsesPath(c) && service.HasCompactionTriggerInInput(body) {
		if normalized, changed, err := service.NormalizeCompactionTriggerInputOrder(body); err != nil {
			reqLog.Warn("codex.remote_compact.trigger_order_normalization_failed", zap.Error(err))
		} else if changed {
			body = normalized
		}
		if isOpenAIRemoteCompactionV2Request(c, body) {
			return body, true
		}
		c.Request.URL.Path = strings.TrimRight(c.Request.URL.Path, "/") + "/compact"
		isCompactRequest = true
		clientStream := gjson.GetBytes(body, "stream").Bool()
		if clientStream {
			service.MarkOpenAICompactClientStream(c)
		}
		reqLog.Info("codex.remote_compact.detected_body_signal", zap.Bool("client_stream", clientStream))
	}
	if !isCompactRequest {
		return body, true
	}
	if compactSeed := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); compactSeed != "" {
		c.Set(service.OpenAICompactSessionSeedKeyForTest(), compactSeed)
	}
	normalizedBody, normalized, err := service.NormalizeOpenAICompactRequestBodyForTest(body)
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to normalize compact request body")
		return nil, false
	}
	if normalized {
		body = normalizedBody
	}
	return body, true
}

func (h *OpenAIGatewayHandler) logOpenAIRemoteCompactOutcome(c *gin.Context, startedAt time.Time) {
	if !isOpenAILegacyCompactPath(c) {
		return
	}

	var (
		ctx    = context.Background()
		path   string
		status int
	)
	if c != nil {
		if c.Request != nil {
			ctx = c.Request.Context()
			if c.Request.URL != nil {
				path = strings.TrimSpace(c.Request.URL.Path)
			}
		}
		if c.Writer != nil {
			status = c.Writer.Status()
		}
	}

	outcome := "failed"
	if status >= 200 && status < 300 {
		outcome = "succeeded"
	}
	// A committed heartbeat fixes the wire status at 200; the stream error is
	// the authoritative outcome in that case.
	if outcome == "succeeded" && c != nil {
		if _, hasStreamErr := service.GetOpsStreamError(c); hasStreamErr {
			outcome = "failed"
		}
	}
	clientContextErr := ""
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			clientContextErr = err.Error()
			if outcome == "succeeded" {
				outcome = "client_disconnected_after_success"
			}
		}
	}
	latencyMs := time.Since(startedAt).Milliseconds()
	if latencyMs < 0 {
		latencyMs = 0
	}

	fields := []zap.Field{
		zap.String("component", "handler.openai_gateway.responses"),
		zap.Bool("remote_compact", true),
		zap.String("compact_outcome", outcome),
		zap.Int("status_code", status),
		zap.Int64("latency_ms", latencyMs),
		zap.String("path", path),
		zap.Bool("force_codex_cli", h != nil && h.cfg != nil && h.cfg.Gateway.ForceCodexCLI),
	}
	if clientContextErr != "" {
		fields = append(fields, zap.String("client_context_error", clientContextErr))
	}

	if c != nil {
		if v, ok := c.Get(openAIRemoteCompactRequestBodyBytesKey); ok {
			if bodyBytes, ok := v.(int); ok && bodyBytes >= 0 {
				fields = append(fields, zap.Int("body_bytes", bodyBytes))
			}
		}
		if userAgent := strings.TrimSpace(c.GetHeader("User-Agent")); userAgent != "" {
			fields = append(fields, zap.String("request_user_agent", userAgent))
		}
		if v, ok := c.Get(opsModelKey); ok {
			if model, ok := v.(string); ok && strings.TrimSpace(model) != "" {
				fields = append(fields, zap.String("request_model", strings.TrimSpace(model)))
			}
		}
		if v, ok := c.Get(opsAccountIDKey); ok {
			if accountID, ok := v.(int64); ok && accountID > 0 {
				fields = append(fields, zap.Int64("account_id", accountID))
			}
		}
		if c.Writer != nil {
			if upstreamRequestID := strings.TrimSpace(c.Writer.Header().Get("x-request-id")); upstreamRequestID != "" {
				fields = append(fields, zap.String("upstream_request_id", upstreamRequestID))
			} else if upstreamRequestID := strings.TrimSpace(c.Writer.Header().Get("X-Request-Id")); upstreamRequestID != "" {
				fields = append(fields, zap.String("upstream_request_id", upstreamRequestID))
			}
		}
	}

	log := logger.FromContext(ctx).With(fields...)
	if outcome == "succeeded" {
		log.Info("codex.remote_compact.succeeded")
		return
	}
	if outcome == "client_disconnected_after_success" {
		log.Warn("codex.remote_compact.client_disconnected_after_success")
		return
	}
	log.Warn("codex.remote_compact.failed")
}

// Messages handles Anthropic Messages API requests routed to OpenAI platform.
// POST /v1/messages (when group platform is OpenAI)
func (h *OpenAIGatewayHandler) Messages(c *gin.Context) {
	if h == nil {
		if c != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"type": "error",
				"error": gin.H{
					"type":    "api_error",
					"message": "Internal server error",
				},
			})
		}
		return
	}
	streamStarted := false
	defer h.recoverAnthropicMessagesPanic(c, &streamStarted)

	requestStart := time.Now()

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.anthropicErrorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.anthropicErrorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(
		c,
		"handler.openai_gateway.messages",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)

	// 检查分组是否允许 /v1/messages 调度
	if !allowOpenAICompatibleMessagesDispatch(c, apiKey) {
		h.anthropicErrorResponse(c, http.StatusForbidden, "permission_error",
			"This group does not allow /v1/messages dispatch")
		return
	}

	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.anthropicErrorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	if len(body) == 0 {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}

	if !gjson.ValidBytes(body) {
		logRequestBodyParseFailure(reqLog, body, nil)
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}

	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.Type != gjson.String || modelResult.String() == "" {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	reqModel := modelResult.String()
	ensureCompositeTargetPlatform(c, apiKey, reqModel)
	if !openAICompatibleTextTargetAllowed(c, apiKey, reqModel) {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Model is not supported by this OpenAI-compatible endpoint for composite groups")
		return
	}
	bindOpenAIReasoningEffortPolicyForMessagesRequest(c, apiKey, body)
	routingModel := service.NormalizeOpenAICompatRequestedModel(reqModel)
	preferredMappedModel := resolveOpenAIMessagesDispatchMappedModel(c, apiKey, reqModel)
	reqStream := gjson.GetBytes(body, "stream").Bool()

	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", reqStream))

	setOpsRequestContext(c, reqModel, reqStream, body)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))

	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolAnthropicMessages, reqModel, body); decision != nil && decision.Blocked {
		h.anthropicErrorResponse(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
		return
	}

	// 解析渠道级模型映射
	channelMappingMsg, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)
	mappedBodyForMessages := newOpenAIModelMappedBodyCache(body, h.gatewayService.ReplaceModelInBody)

	// 绑定错误透传服务，允许 service 层在非 failover 错误场景复用规则。
	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	requestPlatform := openAICompatibleRequestPlatform(c.Request.Context(), apiKey)

	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())
	routingStart := time.Now()

	userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, reqStream, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if userReleaseFunc != nil {
		defer userReleaseFunc()
	}

	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai_messages.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.anthropicStreamingAwareError(c, status, code, message, streamStarted)
		return
	}

	sessionCtx := h.gatewayService.ResolveAnthropicMessageSessionContext(c, reqModel, body)
	sessionHash := sessionCtx.SessionHash
	promptCacheKey := sessionCtx.PromptCacheKey
	if h.rejectIfCyberSessionBlocked(c, apiKey, body, reqModel, cyberBlockFormatAnthropic) {
		return
	}

	// 5/9 multimodal short-circuit: 含 image/document content block 的请求
	// 在 scheduler 里跳 sticky/fallback 等待 (timeout=0 立即重选另一账号).
	// 修 cctest 多模态 5/10 — 同账号 sticky 排队 120s 让一半请求 timeout.
	// tools/thinking 走 cc-api 真 Claude, 不经过这里, 不受影响.
	if service.HasMultimodalContent(body) {
		c.Request = c.Request.WithContext(service.WithMultimodalSkipWaitCtx(c.Request.Context()))
		reqLog = reqLog.With(zap.Bool("multimodal_skip_wait", true))
	}

	maxAccountSwitches := h.maxAccountSwitches
	switchCount := 0
	profitVetoCount := 0
	failedAccountIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	// 5/10 codex audit: per-reason switch count cap. 默认空 reason 走全局
	// maxAccountSwitches=10.
	// 6/2 production triage: metadata-only first_meaningful_timeout on an
	// ordinary single-turn request waited 120s, switched once, then waited
	// another 120s. This reason means the selected upstream is already stuck
	// before any visible Anthropic content, so cross-account retry usually
	// just doubles client latency and burns another account. cap=0 means the
	// first occurrence is exhausted immediately in this handler's pre-increment
	// check below.
	perReasonSwitchCount := make(map[string]int)
	perReasonSwitchCap := map[string]int{
		"first_meaningful_timeout":     0,
		"stream_data_interval_timeout": 1, // 5/10 R38: data interval 同样限 1 次防烧账号
		"buffered_total_timeout":       0, // 5/27 v0.5: 100s buffered 总超时不再换号重试, 避免 202/405s 放大
		"buffered_empty_output":        1, // terminal 200 但无可见输出, 换号一次后放弃
	}
	// 2026-05-15 codex round 11ai: large-context fail-fast. 大请求
	// (msgs>100 或 body>800KB) 不让 first_meaningful_timeout retry,
	// 服务也用更窄 timeout (45s vs 60s) — 配合 service.WithLargeContextCtx
	// + LargeRequestFirstMeaningfulTimeout configmap 字段. 防止单条
	// 大上下文请求等 2×120s=240s 才失败的问题 (forensics 来自 backup 108
	// req_id 202605150325...d9d6TQDlPL9o use_time=239s end_reason=client_gone).
	if h.cfg != nil {
		msgT := h.cfg.Gateway.LargeRequestMsgThreshold
		byteT := h.cfg.Gateway.LargeRequestBodyBytesThreshold
		if service.IsLargeContextRequest(body, msgT, byteT) {
			c.Request = c.Request.WithContext(service.WithLargeContextCtx(c.Request.Context()))
			perReasonSwitchCap["first_meaningful_timeout"] = 0
			perReasonSwitchCap["stream_data_interval_timeout"] = 0
			perReasonSwitchCap["buffered_total_timeout"] = 0
			reqLog = reqLog.With(zap.Bool("large_context_request", true))
		}
	}
	var lastFailoverErr *service.UpstreamFailoverError
	var oauth429FailoverState service.OpenAIOAuth429FailoverState
	effectiveMappedModel := preferredMappedModel
	sonnetFiveDispatchFallbackTried := false
	trySonnetFiveDispatchFallback := func(reason string, err error) bool {
		if sonnetFiveDispatchFallbackTried {
			return false
		}
		fallbackModel := service.FallbackSonnetFiveMessagesDispatchModel(reqModel, effectiveMappedModel)
		if fallbackModel == "" {
			return false
		}
		previousModel := strings.TrimSpace(effectiveMappedModel)
		sonnetFiveDispatchFallbackTried = true
		effectiveMappedModel = fallbackModel
		failedAccountIDs = make(map[int64]struct{})
		sameAccountRetryCount = make(map[int64]int)
		perReasonSwitchCount = make(map[string]int)
		lastFailoverErr = nil
		switchCount = 0
		reqLog.Warn("openai_messages.sonnet5_dispatch_fallback",
			zap.String("from_model", previousModel),
			zap.String("to_model", fallbackModel),
			zap.String("reason", reason),
			zap.Error(err),
		)
		return true
	}
	msgPricingCtx, pricingAt := h.gatewayService.WithOpenAIRequestPricingContext(c.Request.Context(), apiKey.GroupID)
	c.Request = c.Request.WithContext(msgPricingCtx)

	for {
		if c.Request.Context().Err() != nil {
			return
		}
		currentRoutingModel := routingModel
		if effectiveMappedModel != "" {
			currentRoutingModel = effectiveMappedModel
		}
		reqLog.Debug("openai_messages.account_selecting", zap.Int("excluded_account_count", len(failedAccountIDs)))
		selection, scheduleDecision, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
			c.Request.Context(),
			apiKey.GroupID,
			"", // no previous_response_id
			sessionHash,
			currentRoutingModel,
			failedAccountIDs,
			service.OpenAIUpstreamTransportAny,
			service.OpenAIEndpointCapabilityChatCompletions,
			false,
			false,
			true,
			requestPlatform,
		)
		if err != nil {
			reqLog.Warn("openai_messages.account_select_failed",
				zap.Error(err),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if trySonnetFiveDispatchFallback("account_select_failed", err) {
				continue
			}
			if len(failedAccountIDs) == 0 {
				if err != nil {
					cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, currentRoutingModel, reqModel, service.PlatformOpenAI)
					if !cls.ModelNotFound {
						markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
					}
					h.anthropicStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
					return
				}
			} else {
				if lastFailoverErr != nil {
					h.handleAnthropicFailoverExhausted(c, lastFailoverErr, streamStarted)
				} else {
					h.anthropicStreamingAwareError(c, http.StatusBadGateway, "api_error", "Internal server error", streamStarted)
				}
				return
			}
		}
		if selection == nil || selection.Account == nil {
			if trySonnetFiveDispatchFallback("no_available_accounts", nil) {
				continue
			}
			cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, currentRoutingModel, reqModel, service.PlatformOpenAI)
			if !cls.ModelNotFound {
				markOpsRoutingCapacityLimited(c)
			}
			h.anthropicStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
			return
		}
		account := selection.Account
		sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
		reqLog.Debug("openai_messages.account_selected", zap.Int64("account_id", account.ID), zap.String("account_name", account.Name))
		_ = scheduleDecision
		setOpsSelectedAccount(c, account.ID, account.Platform)

		accountReleaseFunc, slotResult := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, reqStream, &streamStarted, reqLog)
		if slotResult == openAISlotAcquireProfitVetoed {
			if !recordOpenAIProfitVeto(failedAccountIDs, account.ID, &profitVetoCount) {
				h.handleOpenAIProfitVetoExhausted(c, streamStarted, reqLog, profitVetoCount)
				return
			}
			continue
		}
		if slotResult != openAISlotAcquireOK {
			return
		}

		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		forwardStart := time.Now()

		defaultMappedModel := strings.TrimSpace(effectiveMappedModel)
		// 应用渠道模型映射到请求体
		forwardBody := mappedBodyForMessages(channelMappingMsg.Mapped, channelMappingMsg.MappedModel)
		writerSizeBeforeForward := c.Writer.Size()
		result, err := func() (*service.OpenAIForwardResult, error) {
			defer func() {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
			}()
			return h.gatewayService.ForwardAsAnthropic(c.Request.Context(), c, account, forwardBody, promptCacheKey, defaultMappedModel)
		}()
		var cyberBlockBodyMsg []byte
		if service.GetOpsCyberPolicy(c) != nil {
			cyberBlockBodyMsg = body
		}
		h.recordCyberPolicyIfMarked(c, apiKey, account, subscription, reqModel, err != nil, cyberBlockBodyMsg, clientRequestedUsageFields(c, channelMappingMsg, reqModel, ""), service.HashUsageRequestPayload(body))
		forwardDurationMs := time.Since(forwardStart).Milliseconds()
		upstreamLatencyMs, _ := getContextInt64(c, service.OpsUpstreamLatencyMsKey)
		responseLatencyMs := forwardDurationMs
		if upstreamLatencyMs > 0 && forwardDurationMs > upstreamLatencyMs {
			responseLatencyMs = forwardDurationMs - upstreamLatencyMs
		}
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, responseLatencyMs)
		if err == nil && result != nil && result.FirstTokenMs != nil {
			service.SetOpsLatencyMs(c, service.OpsTimeToFirstTokenMsKey, int64(*result.FirstTokenMs))
		}
		// Forward 与错误一起返回的部分结果：流中断/客户端断开排水前上游已计量的
		// usage 照常入账，避免上游已产生消耗的请求完全漏记（#5148，对齐 anthropic
		// 网关同名修复）。failover 错误恒定 result=nil，不会重复计费。
		submitMessagesUsage := func(res *service.OpenAIForwardResult) {
			if res == nil {
				return
			}
			stampOpenAIRequestedReasoningEffort(res, c)
			userAgent := c.GetHeader("User-Agent")
			clientIP := ip.GetClientIP(c)
			requestPayloadHash := service.HashUsageRequestPayload(body)
			inboundEndpoint := GetInboundEndpoint(c)
			upstreamEndpoint := resolveOpenAIUpstreamEndpoint(c, account, res)
			quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)
			sessionID := service.ExtractClientSessionID(c)
			cyberBlocked := service.GetOpsCyberPolicy(c) != nil
			h.submitOpenAIUsageRecordTask(c.Request.Context(), res, func(ctx context.Context) {
				if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
					Result:             res,
					APIKey:             apiKey,
					User:               apiKey.User,
					Account:            account,
					Subscription:       subscription,
					InboundEndpoint:    inboundEndpoint,
					UpstreamEndpoint:   upstreamEndpoint,
					UserAgent:          userAgent,
					IPAddress:          clientIP,
					RequestPayloadHash: requestPayloadHash,
					APIKeyService:      h.apiKeyService,
					QuotaPlatform:      quotaPlatform,
					SessionID:          sessionID,
					ChannelUsageFields: clientRequestedUsageFields(c, channelMappingMsg, reqModel, res.UpstreamModel),
					PricingAt:          pricingAt,
					CyberBlocked:       cyberBlocked,
				}); err != nil {
					logger.L().With(
						zap.String("component", "handler.openai_gateway.messages"),
						zap.Int64("user_id", subject.UserID),
						zap.Int64("api_key_id", apiKey.ID),
						zap.Any("group_id", apiKey.GroupID),
						zap.String("model", reqModel),
						zap.Int64("account_id", account.ID),
					).Error("openai_messages.record_usage_failed", zap.Error(err))
				}
			})
		}
		if err != nil {
			if result != nil && result.ImageCount > 0 {
				reqLog.Warn("openai_messages.forward_partial_error_with_image_result",
					zap.Int64("account_id", account.ID),
					zap.Int("image_count", result.ImageCount),
					zap.Error(err),
				)
			} else {
				var failoverErr *service.UpstreamFailoverError
				if errors.As(err, &failoverErr) {
					if c.Request.Context().Err() != nil {
						return
					}
					if c.Writer.Size() != writerSizeBeforeForward {
						h.gatewayService.ObserveOpenAIAccountHealthFailure(c.Request.Context(), account, err)
						h.handleAnthropicFailoverExhausted(c, failoverErr, true)
						return
					}
					if failoverErr.ShouldReportAccountScheduleFailure() {
						h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, currentRoutingModel, false, nil), false, nil, err)
					}
					// 5/9 codex audit: 网络层错误 (BreakSticky=true) 解绑 sticky
					// 防止同 sessionHash 后续请求继续被 sticky 命中已坏账号.
					if failoverErr.BreakSticky && sessionHash != "" {
						if delErr := h.gatewayService.DeleteStickySession(c.Request.Context(), apiKey.GroupID, sessionHash); delErr != nil {
							reqLog.Warn("openai_messages.delete_sticky_after_network_error_failed",
								zap.Int64("account_id", account.ID),
								zap.Error(delErr),
							)
						} else {
							reqLog.Info("openai_messages.sticky_deleted_after_network_error",
								zap.Int64("account_id", account.ID),
							)
						}
					}
					if !failoverErr.ShouldRetryNextAccount() {
						h.handleAnthropicFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					// 池模式：同账号重试
					if failoverErr.RetryableOnSameAccount {
						retryLimit := effectiveSameAccountRetryLimit(failoverErr, account)
						if sameAccountRetryAllowed(failoverErr, sameAccountRetryCount[account.ID], retryLimit) {
							sameAccountRetryCount[account.ID]++
							retryDelay := sameAccountRetryDelayFor(failoverErr, sameAccountRetryCount[account.ID])
							reqLog.Warn("openai_messages.pool_mode_same_account_retry",
								zap.Int64("account_id", account.ID),
								zap.Int("upstream_status", failoverErr.StatusCode),
								zap.Int("retry_limit", retryLimit),
								zap.Int("retry_count", sameAccountRetryCount[account.ID]),
								zap.Duration("retry_delay", retryDelay),
							)
							select {
							case <-c.Request.Context().Done():
								return
							case <-time.After(retryDelay):
							}
							continue
						}
					}
					h.gatewayService.RecordOpenAIAccountSwitch()
					failedAccountIDs[account.ID] = struct{}{}
					lastFailoverErr = failoverErr
					if failoverErr.StatusCode == http.StatusBadGateway || failoverErr.StatusCode == http.StatusServiceUnavailable {
						if trySonnetFiveDispatchFallback("upstream_"+strconv.Itoa(failoverErr.StatusCode), failoverErr) {
							continue
						}
					}
					// 5/10 codex audit: per-reason cap (e.g. first_meaningful_timeout
					// 限 1 次 switch 防烧账号). 共享全局 switchCount 也起作用.
					if reason := failoverErr.Reason; reason != "" {
						if cap, hasCap := perReasonSwitchCap[reason]; hasCap {
							if perReasonSwitchCount[reason] >= cap {
								reqLog.Warn("openai_messages.per_reason_switch_cap_reached",
									zap.String("reason", reason),
									zap.Int("cap", cap),
									zap.Int("count", perReasonSwitchCount[reason]),
								)
								h.handleAnthropicFailoverExhausted(c, failoverErr, streamStarted)
								return
							}
							perReasonSwitchCount[reason]++
						}
					}
					if switchCount >= maxAccountSwitches {
						h.handleAnthropicFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					switchCount++
					if h.gatewayService.ShouldStopOpenAIOAuth429Failover(account, failoverErr.StatusCode, switchCount, &oauth429FailoverState) {
						h.handleAnthropicFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					reqLog.Warn("openai_messages.upstream_failover_switching",
						zap.Int64("account_id", account.ID),
						zap.Int("upstream_status", failoverErr.StatusCode),
						zap.String("reason", failoverErr.Reason),
						zap.Bool("network_error_break_sticky", failoverErr.BreakSticky),
						zap.Int("switch_count", switchCount),
						zap.Int("max_switches", maxAccountSwitches),
					)
					continue
				}
				if result != nil && result.ClientDisconnect {
					reqLog.Info("openai_messages.client_disconnected",
						zap.Int64("account_id", account.ID),
						zap.Error(err),
					)
					// 断开排水期间上游已计量的 usage 必须入账（此前直接 return 丢弃，
					// payg 上游照常计费而平台漏记）。
					submitMessagesUsage(result)
					return
				}
				h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, currentRoutingModel, false, result), false, nil, err)
				wroteFallback := h.ensureAnthropicErrorResponse(c, streamStarted)
				reqLog.Warn("openai_messages.forward_failed",
					zap.Int64("account_id", account.ID),
					zap.Bool("fallback_error_response_written", wroteFallback),
					zap.Error(err),
				)
				submitMessagesUsage(result)
				return
			}
		}
		if result != nil {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, currentRoutingModel, false, result), true, result.FirstTokenMs)
		} else {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, currentRoutingModel, false, result), true, nil)
		}

		userAgent := c.GetHeader("User-Agent")
		clientIP := ip.GetClientIP(c)
		requestPayloadHash := service.HashUsageRequestPayload(body)
		inboundEndpoint := GetInboundEndpoint(c)
		upstreamEndpoint := resolveOpenAIUpstreamEndpoint(c, account, result)
		quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)
		sessionID := service.ExtractClientSessionID(c)

		cyberBlocked := service.GetOpsCyberPolicy(c) != nil
		h.submitOpenAIUsageRecordTask(c.Request.Context(), result, func(ctx context.Context) {
			if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
				Result:             result,
				APIKey:             apiKey,
				User:               apiKey.User,
				Account:            account,
				Subscription:       subscription,
				InboundEndpoint:    inboundEndpoint,
				UpstreamEndpoint:   upstreamEndpoint,
				UserAgent:          userAgent,
				IPAddress:          clientIP,
				RequestPayloadHash: requestPayloadHash,
				APIKeyService:      h.apiKeyService,
				QuotaPlatform:      quotaPlatform,
				SessionID:          sessionID,
				ChannelUsageFields: clientRequestedUsageFields(c, channelMappingMsg, reqModel, result.UpstreamModel),
				CyberBlocked:       cyberBlocked,
			}); err != nil {
				logger.L().With(
					zap.String("component", "handler.openai_gateway.messages"),
					zap.Int64("user_id", subject.UserID),
					zap.Int64("api_key_id", apiKey.ID),
					zap.Any("group_id", apiKey.GroupID),
					zap.String("model", reqModel),
					zap.Int64("account_id", account.ID),
				).Error("openai_messages.record_usage_failed", zap.Error(err))
			}
		})
		reqLog.Debug("openai_messages.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", switchCount),
		)
		return
	}
}

// anthropicErrorResponse writes an error in Anthropic Messages API format.
func (h *OpenAIGatewayHandler) anthropicErrorResponse(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

// anthropicStreamingAwareError handles errors that may occur during streaming,
// using Anthropic SSE error format.
func (h *OpenAIGatewayHandler) anthropicStreamingAwareError(c *gin.Context, status int, errType, message string, streamStarted bool) {
	if streamStarted {
		flusher, ok := c.Writer.(http.Flusher)
		if ok {
			errPayload, _ := json.Marshal(gin.H{
				"type": "error",
				"error": gin.H{
					"type":    errType,
					"message": message,
				},
			})
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", errPayload) //nolint:errcheck
			flusher.Flush()
		}
		return
	}
	h.anthropicErrorResponse(c, status, errType, message)
}

// handleAnthropicFailoverExhausted maps upstream failover errors to Anthropic format.
func (h *OpenAIGatewayHandler) handleAnthropicFailoverExhausted(c *gin.Context, failoverErr *service.UpstreamFailoverError, streamStarted bool) {
	if failoverErr != nil {
		copyFailoverRetryAfter(c, failoverErr.ResponseHeaders)
	}
	if failoverErr != nil && failoverErr.IsCredentialFailure() {
		status, message := credentialFailoverClientResponse(failoverErr)
		h.anthropicStreamingAwareError(c, status, "api_error", message, streamStarted)
		return
	}
	if failoverErr != nil && failoverErr.IsOpenAICapacityShed() && strings.TrimSpace(failoverErr.ClientMessage) != "" {
		status := failoverErr.ClientStatusCode
		if status <= 0 {
			status = http.StatusServiceUnavailable
		}
		h.anthropicStreamingAwareError(c, status, "api_error", failoverErr.ClientMessage, streamStarted)
		return
	}
	status, errType, errMsg := h.mapUpstreamError(failoverErr.StatusCode)
	h.anthropicStreamingAwareError(c, status, errType, errMsg, streamStarted)
}

// ensureAnthropicErrorResponse writes a fallback Anthropic error if no response was written.
func (h *OpenAIGatewayHandler) ensureAnthropicErrorResponse(c *gin.Context, streamStarted bool) bool {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return false
	}
	h.anthropicStreamingAwareError(c, http.StatusBadGateway, "api_error", anthropicTemporaryUnavailableMessage, streamStarted)
	return true
}

func (h *OpenAIGatewayHandler) validateFunctionCallOutputRequest(c *gin.Context, body []byte, reqLog *zap.Logger) bool {
	if !gjson.GetBytes(body, `input.#(type=="function_call_output")`).Exists() {
		return true
	}

	validation := service.ValidateFunctionCallOutputContextBytes(body)
	if !validation.HasFunctionCallOutput {
		return true
	}

	previousResponseID := gjson.GetBytes(body, "previous_response_id").String()
	if strings.TrimSpace(previousResponseID) != "" || validation.HasToolCallContext {
		return true
	}

	if validation.HasFunctionCallOutputMissingCallID {
		reqLog.Warn("openai.request_validation_failed",
			zap.String("reason", "function_call_output_missing_call_id"),
		)
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "function_call_output requires call_id on HTTP requests; continuation via previous_response_id is only supported on Responses WebSocket v2")
		return false
	}
	if validation.HasItemReferenceForAllCallIDs {
		return true
	}

	reqLog.Warn("openai.request_validation_failed",
		zap.String("reason", "function_call_output_missing_item_reference"),
	)
	h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "function_call_output requires item_reference ids matching each call_id on HTTP requests; continuation via previous_response_id is only supported on Responses WebSocket v2")
	return false
}

func normalizeCodexDelegationBootstrap(body []byte) ([]byte, bool) {
	return normalizeCodexCallOutputBootstrap(body, isCodexDelegationCandidate)
}

func normalizeCodexAutomationBootstrap(body []byte) ([]byte, bool) {
	return normalizeCodexCallOutputBootstrap(body, isCodexAutomationCandidate)
}

func normalizeCodexCallOutputBootstrap(body []byte, isCandidate func(map[string]any) bool) ([]byte, bool) {
	if !hasUniqueJSONMembers(body) {
		return body, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var request map[string]any
	if err := decoder.Decode(&request); err != nil {
		return body, false
	}
	if previousResponseID, exists := request["previous_response_id"]; exists {
		value, ok := previousResponseID.(string)
		if !ok || strings.TrimSpace(value) != "" {
			return body, false
		}
	}
	input, ok := request["input"].([]any)
	if !ok {
		return body, false
	}

	// Any call/reference anchor makes a call-less output ambiguous. Responses
	// built-ins follow the *_call / *_call_output naming convention, so classify
	// by the wire type shape instead of maintaining an incomplete allowlist.
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := stringField(item, "type")
		if typ == "item_reference" || strings.HasSuffix(typ, "_call") {
			return body, false
		}
		if isResponsesCallOutputType(typ) {
			callIDValue, exists := item["call_id"]
			callID, isString := callIDValue.(string)
			if exists && (!isString || strings.TrimSpace(callID) != "") {
				return body, false
			}
			if !isCandidate(item) {
				return body, false
			}
		}
	}

	changed := false
	for i, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || !isCandidate(item) {
			continue
		}
		output, ok := item["output"].(string)
		if !ok {
			continue
		}
		input[i] = map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": output,
			}},
		}
		changed = true
	}
	if !changed {
		return body, false
	}
	normalized, err := json.Marshal(request)
	if err != nil {
		return body, false
	}
	return normalized, true
}

func hasUniqueJSONMembers(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if !consumeUniqueJSONValue(decoder) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func consumeUniqueJSONValue(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return true
	}

	switch delim {
	case '{':
		members := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return false
			}
			key, ok := keyToken.(string)
			if !ok {
				return false
			}
			if _, duplicate := members[key]; duplicate {
				return false
			}
			members[key] = struct{}{}
			if !consumeUniqueJSONValue(decoder) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim('}')
	case '[':
		for decoder.More() {
			if !consumeUniqueJSONValue(decoder) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim(']')
	default:
		return false
	}
}

func isResponsesCallOutputType(typ string) bool {
	return strings.HasSuffix(typ, "_call_output") || typ == "tool_search_output"
}

func isCodexDelegationCandidate(item map[string]any) bool {
	if stringField(item, "type") != "function_call_output" ||
		!isCodexDelegationTool(stringField(item, "namespace"), stringField(item, "name")) {
		return false
	}
	output, ok := item["output"].(string)
	return ok && validCodexDelegationEnvelope(output)
}

func isCodexAutomationCandidate(item map[string]any) bool {
	if stringField(item, "type") != "function_call_output" ||
		stringField(item, "namespace") != "codex_app" ||
		stringField(item, "name") != "automation_update" {
		return false
	}
	output, ok := item["output"].(string)
	return ok && validCodexAutomationBootstrap(output)
}

func stringField(item map[string]any, key string) string {
	value, _ := item[key].(string)
	return value
}

func isCodexDelegationTool(namespace, name string) bool {
	return (namespace == "codex_app" || namespace == "codex_tui") &&
		(name == "create_thread" || name == "send_message_to_thread")
}

func validCodexAutomationBootstrap(value string) bool {
	normalized := strings.ReplaceAll(value, "\r\n", "\n")
	if strings.ContainsRune(normalized, '\r') {
		return false
	}
	lines := strings.Split(normalized, "\n")
	if len(lines) < 6 {
		return false
	}
	if _, ok := codexAutomationHeaderValue(lines[0], "Automation: "); !ok {
		return false
	}
	automationID, ok := codexAutomationHeaderValue(lines[1], "Automation ID: ")
	if !ok || !validCodexAutomationID(automationID) {
		return false
	}
	expectedMemory := "Automation memory: $CODEX_HOME/automations/" + automationID + "/memory.md"
	if lines[2] != expectedMemory {
		return false
	}
	lastRun, ok := codexAutomationHeaderValue(lines[3], "Last run: ")
	if !ok || !validCodexAutomationLastRun(lastRun) || lines[4] != "" {
		return false
	}
	return strings.TrimSpace(strings.Join(lines[5:], "\n")) != ""
}

func codexAutomationHeaderValue(line, prefix string) (string, bool) {
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	value := strings.TrimPrefix(line, prefix)
	return value, value != "" && strings.TrimSpace(value) == value
}

func validCodexAutomationID(value string) bool {
	if len(value) == 0 || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			continue
		}
		return false
	}
	return true
}

func validCodexAutomationLastRun(value string) bool {
	if value == "never" {
		return true
	}
	separator := strings.LastIndex(value, " (")
	if separator <= 0 || !strings.HasSuffix(value, ")") {
		return false
	}
	runAt, err := time.Parse(time.RFC3339Nano, value[:separator])
	if err != nil {
		return false
	}
	epochMillis, err := strconv.ParseInt(value[separator+2:len(value)-1], 10, 64)
	return err == nil && runAt.UnixMilli() == epochMillis
}

func validCodexDelegationEnvelope(value string) bool {
	decoder := xml.NewDecoder(strings.NewReader(value))
	var rootSeen, sourceSeen, inputSeen bool
	var childName string
	var childText bytes.Buffer
	depth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return rootSeen && depth == 0 && sourceSeen && inputSeen
		}
		if err != nil {
			return false
		}
		switch current := token.(type) {
		case xml.StartElement:
			depth++
			if current.Name.Space != "" || len(current.Attr) != 0 || (depth == 1 && current.Name.Local != "codex_delegation") || depth > 2 {
				return false
			}
			if depth == 1 {
				if rootSeen {
					return false
				}
				rootSeen = true
				continue
			}
			if current.Name.Local != "source_thread_id" && current.Name.Local != "input" {
				return false
			}
			childName = current.Name.Local
			childText.Reset()
		case xml.EndElement:
			if current.Name.Space != "" {
				return false
			}
			if depth == 2 {
				if current.Name.Local != childName || strings.TrimSpace(childText.String()) == "" {
					return false
				}
				if childName == "source_thread_id" {
					if sourceSeen {
						return false
					}
					sourceSeen = true
				} else {
					if inputSeen {
						return false
					}
					inputSeen = true
				}
				childName = ""
			}
			depth--
			if depth < 0 {
				return false
			}
		case xml.CharData:
			if depth == 2 {
				_, _ = childText.Write(current)
			} else if len(bytes.TrimSpace(current)) != 0 {
				return false
			}
		case xml.Comment:
			return false
		case xml.ProcInst, xml.Directive:
			return false
		}
	}
}

func (h *OpenAIGatewayHandler) acquireResponsesUserSlot(
	c *gin.Context,
	userID int64,
	userConcurrency int,
	reqStream bool,
	streamStarted *bool,
	reqLog *zap.Logger,
) (func(), bool) {
	ctx := c.Request.Context()
	userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlot(ctx, userID, userConcurrency)
	if err != nil {
		reqLog.Warn("openai.user_slot_acquire_failed", zap.Error(err))
		h.handleConcurrencyError(c, err, "user", *streamStarted)
		return nil, false
	}
	if userAcquired {
		return wrapReleaseOnDone(ctx, userReleaseFunc), true
	}

	maxWait := service.CalculateMaxWait(userConcurrency)
	canWait, waitErr := h.concurrencyHelper.IncrementWaitCount(ctx, userID, maxWait)
	if waitErr != nil {
		reqLog.Warn("openai.user_wait_counter_increment_failed", zap.Error(waitErr))
		// 按现有降级语义：等待计数异常时放行后续抢槽流程
	} else if !canWait {
		reqLog.Info("openai.user_wait_queue_full", zap.Int("max_wait", maxWait))
		h.errorResponse(c, http.StatusTooManyRequests, "rate_limit_error", "Too many pending requests, please retry later")
		return nil, false
	}

	waitCounted := waitErr == nil && canWait
	defer func() {
		if waitCounted {
			h.concurrencyHelper.DecrementWaitCount(ctx, userID)
		}
	}()

	userReleaseFunc, err = h.concurrencyHelper.AcquireUserSlotWithWait(c, userID, userConcurrency, reqStream, streamStarted)
	if err != nil {
		reqLog.Warn("openai.user_slot_acquire_failed_after_wait", zap.Error(err))
		h.handleConcurrencyError(c, err, "user", *streamStarted)
		return nil, false
	}

	// 槽位获取成功后，立刻退出等待计数。
	if waitCounted {
		h.concurrencyHelper.DecrementWaitCount(ctx, userID)
		waitCounted = false
	}
	return wrapReleaseOnDone(ctx, userReleaseFunc), true
}

// openAISlotAcquireResult distinguishes a written error from a post-slot
// profit veto, which must be retried with the account excluded.
type openAISlotAcquireResult int

const (
	openAISlotAcquireOK openAISlotAcquireResult = iota
	openAISlotAcquireFailed
	openAISlotAcquireProfitVetoed
)

type openAIWSTurnPricing struct {
	mu sync.Mutex
	at time.Time
}

func (p *openAIWSTurnPricing) freeze(at time.Time) {
	p.mu.Lock()
	p.at = at
	p.mu.Unlock()
}

func (p *openAIWSTurnPricing) currentOr(fallback time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.at.IsZero() {
		return p.at
	}
	return fallback
}

func recordOpenAIProfitVeto(failedAccountIDs map[int64]struct{}, accountID int64, vetoCount *int) bool {
	failedAccountIDs[accountID] = struct{}{}
	*vetoCount++
	return *vetoCount < maxProfitVetoAttempts
}

func (h *OpenAIGatewayHandler) handleOpenAIProfitVetoExhausted(c *gin.Context, streamStarted bool, reqLog *zap.Logger, vetoCount int) {
	reqLog.Warn("openai.profit_veto_attempts_exhausted", zap.Int("profit_veto_count", vetoCount))
	markOpsRoutingCapacityLimited(c)
	h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", profitVetoExhaustedMessage, streamStarted)
}

func (h *OpenAIGatewayHandler) acquireResponsesAccountSlot(
	c *gin.Context,
	groupID *int64,
	sessionHash string,
	selection *service.AccountSelectionResult,
	reqStream bool,
	streamStarted *bool,
	reqLog *zap.Logger,
) (func(), openAISlotAcquireResult) {
	return h.acquireOpenAIAccountSlot(c, groupID, sessionHash, selection, reqStream, streamStarted, reqLog, nil)
}

type openAISlotErrorWriter func(status int, errType, message string)

func (h *OpenAIGatewayHandler) acquireOpenAIAccountSlot(
	c *gin.Context,
	groupID *int64,
	sessionHash string,
	selection *service.AccountSelectionResult,
	reqStream bool,
	streamStarted *bool,
	reqLog *zap.Logger,
	writeError openAISlotErrorWriter,
) (func(), openAISlotAcquireResult) {
	if writeError == nil {
		writeError = func(status int, errType, message string) {
			h.handleStreamingAwareError(c, status, errType, message, *streamStarted)
		}
	}
	if selection == nil || selection.Account == nil {
		markOpsRoutingCapacityLimited(c)
		writeError(http.StatusServiceUnavailable, "api_error", "No available accounts")
		return nil, openAISlotAcquireFailed
	}

	ctx := service.ContextWithSelectionProfitGate(c.Request.Context(), selection)
	account := selection.Account
	if selection.Acquired {
		latest, vetoed, reason := h.gatewayService.ProfitControlVetoLatest(ctx, account)
		if vetoed {
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			reqLog.Debug("openai.account_slot_profit_vetoed", zap.Int64("account_id", account.ID), zap.String("reason", reason))
			return nil, openAISlotAcquireProfitVetoed
		}
		account = latest
		selection.Account = latest
		if selection.ProfitGateActive() {
			if err := h.gatewayService.BindStickySessionAfterProfitAdmission(ctx, groupID, sessionHash, account.ID); err != nil {
				reqLog.Warn("openai.bind_sticky_session_after_profit_admission_failed", zap.Int64("account_id", account.ID), zap.Error(err))
			}
		}
		return wrapReleaseOnDone(ctx, selection.ReleaseFunc), openAISlotAcquireOK
	}
	if selection.WaitPlan == nil {
		markOpsRoutingCapacityLimited(c)
		writeError(http.StatusServiceUnavailable, "api_error", "No available accounts")
		return nil, openAISlotAcquireFailed
	}

	fastReleaseFunc, fastAcquired, err := h.concurrencyHelper.TryAcquireAccountSlot(ctx, account.ID, selection.WaitPlan.MaxConcurrency)
	if err != nil {
		reqLog.Warn("openai.account_slot_quick_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		status, errType, message := concurrencyErrorResponse(err, "account")
		writeError(status, errType, message)
		return nil, openAISlotAcquireFailed
	}
	if fastAcquired {
		latest, vetoed, reason := h.gatewayService.ProfitControlVetoLatest(ctx, account)
		if vetoed {
			if fastReleaseFunc != nil {
				fastReleaseFunc()
			}
			reqLog.Debug("openai.account_slot_profit_vetoed", zap.Int64("account_id", account.ID), zap.String("reason", reason))
			return nil, openAISlotAcquireProfitVetoed
		}
		account = latest
		selection.Account = latest
		if err := h.gatewayService.BindStickySessionAfterProfitAdmission(ctx, groupID, sessionHash, account.ID); err != nil {
			reqLog.Warn("openai.bind_sticky_session_after_profit_admission_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}
		return wrapReleaseOnDone(ctx, fastReleaseFunc), openAISlotAcquireOK
	}

	canWait, waitErr := h.concurrencyHelper.IncrementAccountWaitCount(ctx, account.ID, selection.WaitPlan.MaxWaiting)
	if waitErr != nil {
		reqLog.Warn("openai.account_wait_counter_increment_failed", zap.Int64("account_id", account.ID), zap.Error(waitErr))
	} else if !canWait {
		reqLog.Info("openai.account_wait_queue_full", zap.Int64("account_id", account.ID), zap.Int("max_waiting", selection.WaitPlan.MaxWaiting))
		writeError(http.StatusTooManyRequests, "rate_limit_error", "Too many pending requests, please retry later")
		return nil, openAISlotAcquireFailed
	}

	accountWaitCounted := waitErr == nil && canWait
	releaseWait := func() {
		if accountWaitCounted {
			h.concurrencyHelper.DecrementAccountWaitCount(ctx, account.ID)
			accountWaitCounted = false
		}
	}
	defer releaseWait()

	accountReleaseFunc, err := h.concurrencyHelper.AcquireAccountSlotWithWaitTimeout(
		c, account.ID, selection.WaitPlan.MaxConcurrency, selection.WaitPlan.Timeout, reqStream, streamStarted,
	)
	if err != nil {
		reqLog.Warn("openai.account_slot_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		status, errType, message := concurrencyErrorResponse(err, "account")
		writeError(status, errType, message)
		return nil, openAISlotAcquireFailed
	}
	releaseWait()
	latest, vetoed, reason := h.gatewayService.ProfitControlVetoLatest(ctx, account)
	if vetoed {
		if accountReleaseFunc != nil {
			accountReleaseFunc()
		}
		reqLog.Debug("openai.account_slot_profit_vetoed", zap.Int64("account_id", account.ID), zap.String("reason", reason))
		return nil, openAISlotAcquireProfitVetoed
	}
	account = latest
	selection.Account = latest
	if err := h.gatewayService.BindStickySessionAfterProfitAdmission(ctx, groupID, sessionHash, account.ID); err != nil {
		reqLog.Warn("openai.bind_sticky_session_after_profit_admission_failed", zap.Int64("account_id", account.ID), zap.Error(err))
	}
	return wrapReleaseOnDone(ctx, accountReleaseFunc), openAISlotAcquireOK
}

// ResponsesWebSocket handles OpenAI Responses API WebSocket ingress endpoint
// GET /openai/v1/responses (Upgrade: websocket)
func (h *OpenAIGatewayHandler) ResponsesWebSocket(c *gin.Context) {
	if !isOpenAIWSUpgradeRequest(c.Request) {
		h.errorResponse(c, http.StatusUpgradeRequired, "invalid_request_error", "WebSocket upgrade required (Upgrade: websocket)")
		return
	}
	setOpenAIClientTransportWS(c)

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}

	reqLog := requestLogger(
		c,
		"handler.openai_gateway.responses_ws",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
		zap.Bool("openai_ws_mode", true),
	)
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}
	reqLog.Info("openai.websocket_ingress_started")
	clientIP := ip.GetClientIP(c)
	userAgent := strings.TrimSpace(c.GetHeader("User-Agent"))

	wsConn, err := coderws.Accept(c.Writer, c.Request, &coderws.AcceptOptions{
		CompressionMode: coderws.CompressionContextTakeover,
	})
	if err != nil {
		reqLog.Warn("openai.websocket_accept_failed",
			zap.Error(err),
			zap.String("client_ip", clientIP),
			zap.String("request_user_agent", userAgent),
			zap.String("upgrade_header", strings.TrimSpace(c.GetHeader("Upgrade"))),
			zap.String("connection_header", strings.TrimSpace(c.GetHeader("Connection"))),
			zap.String("sec_websocket_version", strings.TrimSpace(c.GetHeader("Sec-WebSocket-Version"))),
			zap.Bool("has_sec_websocket_key", strings.TrimSpace(c.GetHeader("Sec-WebSocket-Key")) != ""),
		)
		return
	}
	defer func() {
		_ = wsConn.CloseNow()
	}()
	wsConn.SetReadLimit(service.ResolveOpenAIWSClientReadLimitBytes(h.cfg))

	clientLifecycleCtx := c.Request.Context()
	ctx := clientLifecycleCtx
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	msgType, firstMessage, err := wsConn.Read(readCtx)
	cancel()
	if err != nil {
		closeStatus, closeReason := summarizeWSCloseErrorForLog(err)
		reqLog.Warn("openai.websocket_read_first_message_failed",
			zap.Error(err),
			zap.String("client_ip", clientIP),
			zap.String("close_status", closeStatus),
			zap.String("close_reason", closeReason),
			zap.Duration("read_timeout", 30*time.Second),
		)
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "missing first response.create message")
		return
	}
	firstTurnStartedAt := time.Now()
	if msgType != coderws.MessageText && msgType != coderws.MessageBinary {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "unsupported websocket message type")
		return
	}
	if !gjson.ValidBytes(firstMessage) {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "invalid JSON payload")
		return
	}
	reqModel := strings.TrimSpace(gjson.GetBytes(firstMessage, "model").String())
	if reqModel == "" {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "model is required in first response.create payload")
		return
	}
	ensureCompositeTargetPlatform(c, apiKey, reqModel)
	ctx = c.Request.Context()
	if apiKey.Group != nil && apiKey.Group.Platform == service.PlatformComposite {
		platform, ok := service.ResolvedTargetPlatformFromContext(ctx)
		if !ok || !isResponsesWebSocketCompositePlatform(platform) {
			closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "Responses WebSocket API only supports OpenAI-compatible models for composite groups")
			return
		}
	}
	previousResponseID := strings.TrimSpace(gjson.GetBytes(firstMessage, "previous_response_id").String())
	previousResponseIDKind := service.ClassifyOpenAIPreviousResponseIDKind(previousResponseID)
	if previousResponseID != "" && previousResponseIDKind == service.OpenAIPreviousResponseIDKindMessageID {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "previous_response_id must be a response.id (resp_*), not a message id")
		return
	}
	firstMessageToolCoverage := service.AnalyzeToolCallOutputContextCoverageBytes(firstMessage)
	previousResponseCanMove := !firstMessageToolCoverage.HasFunctionCallOutput || firstMessageToolCoverage.ContextCoversAllCallIDs
	imageIntent := service.IsImageGenerationIntent("/v1/responses", reqModel, firstMessage)
	reqLog = reqLog.With(
		zap.Bool("ws_ingress", true),
		zap.String("model", reqModel),
		zap.Bool("has_previous_response_id", previousResponseID != ""),
		zap.String("previous_response_id_kind", previousResponseIDKind),
	)
	setOpsRequestContext(c, reqModel, true, firstMessage)
	setOpsEndpointContext(c, "", int16(service.RequestTypeWSV2))

	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIResponses, reqModel, firstMessage); decision != nil && decision.Blocked {
		writeContentModerationWSError(ctx, wsConn, decision)
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, decision.Message)
		return
	}

	// The handshake has no request body session identifier. Use only explicit
	// session headers here; an empty key deliberately fails open.
	if cyberBlockKey := findBlockedCyberSessionKey(c.Request.Context(), h.gatewayService, apiKey.ID, c, firstMessage); cyberBlockKey != "" {
		writeCyberSessionBlockedWSError(c.Request.Context(), wsConn)
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "session blocked by cyber-security policy")
		h.enqueueCyberSessionBlockedOpsEntry(c, apiKey, reqModel, cyberBlockKey)
		return
	}
	cyberBlockedThisConn := false
	var cyberTurnBodiesMu sync.Mutex
	cyberTurnBodies := map[int][]byte{1: append([]byte(nil), firstMessage...)}
	setCyberTurnBody := func(turn int, payload []byte) {
		cyberTurnBodiesMu.Lock()
		cyberTurnBodies[turn] = append([]byte(nil), payload...)
		cyberTurnBodiesMu.Unlock()
	}
	takeCyberTurnBody := func(turn int) []byte {
		cyberTurnBodiesMu.Lock()
		body := cyberTurnBodies[turn]
		delete(cyberTurnBodies, turn)
		cyberTurnBodiesMu.Unlock()
		return body
	}

	// 解析渠道级模型映射
	channelMappingWS, _ := h.gatewayService.ResolveChannelMappingAndRestrict(ctx, apiKey.GroupID, reqModel)
	wsForwardModel := reqModel
	if channelMappingWS.Mapped && strings.TrimSpace(channelMappingWS.MappedModel) != "" {
		wsForwardModel = strings.TrimSpace(channelMappingWS.MappedModel)
	}
	reqLog = reqLog.With(zap.String("forward_model", wsForwardModel))

	var currentUserRelease func()
	var currentAccountRelease func()
	releaseAccountSlot := func() {
		if currentAccountRelease != nil {
			currentAccountRelease()
			currentAccountRelease = nil
		}
	}
	releaseTurnSlots := func() {
		releaseAccountSlot()
		if currentUserRelease != nil {
			currentUserRelease()
			currentUserRelease = nil
		}
	}
	// 必须尽早注册，确保任何 early return 都能释放已获取的并发槽位。
	defer releaseTurnSlots()

	userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlot(ctx, subject.UserID, subject.Concurrency)
	if err != nil {
		reqLog.Warn("openai.websocket_user_slot_acquire_failed", zap.Error(err))
		closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "failed to acquire user concurrency slot")
		return
	}
	if !userAcquired {
		closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "too many concurrent requests, please retry later")
		return
	}
	currentUserRelease = wrapReleaseOnDone(ctx, userReleaseFunc)
	ensureUserSlotHeld := func() bool {
		if currentUserRelease != nil {
			return true
		}
		userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlot(ctx, subject.UserID, subject.Concurrency)
		if err != nil {
			reqLog.Warn("openai.websocket_user_slot_reacquire_failed", zap.Error(err))
			closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "failed to acquire user concurrency slot")
			return false
		}
		if !userAcquired {
			closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "too many concurrent requests, please retry later")
			return false
		}
		currentUserRelease = wrapReleaseOnDone(ctx, userReleaseFunc)
		return true
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	requestPlatform := openAICompatibleRequestPlatform(ctx, apiKey)
	requiredTransport := service.OpenAIUpstreamTransportResponsesWebsocketV2
	if requestPlatform == service.PlatformGrok {
		requiredTransport = service.OpenAIUpstreamTransportHTTPSSE
	}
	if err := h.billingCacheService.CheckBillingEligibility(ctx, apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai.websocket_billing_eligibility_check_failed", zap.Error(err))
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "billing check failed")
		return
	}

	sessionHash := h.gatewayService.GenerateSessionHashWithFallback(
		c,
		firstMessage,
		openAIWSIngressFallbackSessionSeed(subject.UserID, apiKey.ID, apiKey.GroupID),
	)
	ctx = service.WithOpenAIGuardianParentAffinity(ctx, c, firstMessage, reqModel)
	maxAccountSwitches := h.maxAccountSwitches
	switchCount := 0
	failedAccountIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	var lastFailoverErr *service.UpstreamFailoverError
	var oauth429FailoverState service.OpenAIOAuth429FailoverState
	wsAttemptMessage := append([]byte(nil), firstMessage...)
	waitForWSSameAccountRetry := func(account *service.Account, failoverErr *service.UpstreamFailoverError) bool {
		if account == nil || failoverErr == nil || failoverErr.StatusCode != http.StatusTooManyRequests || failoverErr.SameAccountRetryDeadline.IsZero() {
			return false
		}
		retryLimit := effectiveSameAccountRetryLimit(failoverErr, account)
		if !sameAccountRetryAllowed(failoverErr, sameAccountRetryCount[account.ID], retryLimit) {
			return false
		}
		sameAccountRetryCount[account.ID]++
		retryDelay := sameAccountRetryDelayFor(failoverErr, sameAccountRetryCount[account.ID])
		reqLog.Warn("openai.websocket.same_account_retry",
			zap.Int64("account_id", account.ID),
			zap.Int("upstream_status", failoverErr.StatusCode),
			zap.Int("retry_count", sameAccountRetryCount[account.ID]),
			zap.Duration("retry_delay", retryDelay),
		)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(retryDelay):
			return true
		}
	}
	handleWSFailover := func(account *service.Account, failoverErr *service.UpstreamFailoverError) bool {
		if ctx.Err() != nil {
			return false
		}
		if failoverErr.ShouldReportAccountScheduleFailure() {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, wsForwardModel, false, nil), false, nil, failoverErr)
		}
		releaseAccountSlot()
		if !failoverErr.ShouldRetryNextAccount() {
			closeOpenAIWSFailoverExhausted(c, wsConn, failoverErr)
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		failedAccountIDs[account.ID] = struct{}{}
		lastFailoverErr = failoverErr
		if switchCount >= maxAccountSwitches {
			closeOpenAIWSFailoverExhausted(c, wsConn, failoverErr)
			return false
		}
		switchCount++
		if h.gatewayService.ShouldStopOpenAIOAuth429Failover(account, failoverErr.StatusCode, switchCount, &oauth429FailoverState) {
			closeOpenAIWSFailoverExhausted(c, wsConn, failoverErr)
			return false
		}
		h.gatewayService.RecordOpenAIAccountSwitch()
		reqLog.Warn("openai.websocket_upstream_failover_switching",
			zap.Int64("account_id", account.ID),
			zap.Int("upstream_status", failoverErr.StatusCode),
			zap.Int("switch_count", switchCount),
			zap.Int("max_switches", maxAccountSwitches),
		)
		if ctx.Err() != nil {
			return false
		}
		return ensureUserSlotHeld()
	}

	for {
		if ctx.Err() != nil {
			return
		}
		reqLog.Debug("openai.websocket_account_selecting", zap.Int("excluded_account_count", len(failedAccountIDs)))
		selection, scheduleDecision, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
			ctx,
			apiKey.GroupID,
			previousResponseID,
			sessionHash,
			reqModel,
			failedAccountIDs,
			requiredTransport,
			service.OpenAIEndpointCapabilityChatCompletions,
			false,
			previousResponseCanMove,
			!imageIntent,
			requestPlatform,
		)
		if err != nil {
			reqLog.Warn("openai.websocket_account_select_failed",
				zap.Error(err),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if lastFailoverErr != nil {
				closeOpenAIWSFailoverExhausted(c, wsConn, lastFailoverErr)
			} else {
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "no available account")
			}
			return
		}
		if selection == nil || selection.Account == nil {
			if lastFailoverErr != nil {
				closeOpenAIWSFailoverExhausted(c, wsConn, lastFailoverErr)
			} else {
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "no available account")
			}
			return
		}

		account := selection.Account
		accountMaxConcurrency := account.Concurrency
		if selection.WaitPlan != nil && selection.WaitPlan.MaxConcurrency > 0 {
			accountMaxConcurrency = selection.WaitPlan.MaxConcurrency
		}
		accountReleaseFunc := selection.ReleaseFunc
		if !selection.Acquired {
			if selection.WaitPlan == nil {
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "account is busy, please retry later")
				return
			}
			fastReleaseFunc, fastAcquired, err := h.concurrencyHelper.TryAcquireAccountSlot(
				ctx,
				account.ID,
				selection.WaitPlan.MaxConcurrency,
			)
			if err != nil {
				reqLog.Warn("openai.websocket_account_slot_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
				closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "failed to acquire account concurrency slot")
				return
			}
			if !fastAcquired {
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "account is busy, please retry later")
				return
			}
			accountReleaseFunc = fastReleaseFunc
		}
		currentAccountRelease = wrapReleaseOnDone(ctx, accountReleaseFunc)
		if err := h.gatewayService.BindStickySession(ctx, apiKey.GroupID, sessionHash, account.ID); err != nil {
			reqLog.Warn("openai.websocket_bind_sticky_session_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}

		token, _, err := h.gatewayService.GetRequestCredential(ctx, c, account)
		if err != nil {
			reqLog.Warn("openai.websocket_get_access_token_failed", zap.Int64("account_id", account.ID), zap.Error(err))
			if ctx.Err() != nil {
				return
			}
			var failoverErr *service.UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				if handleWSFailover(account, failoverErr) {
					continue
				}
				return
			}
			closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "failed to get access token")
			return
		}

		reqLog.Debug("openai.websocket_account_selected",
			zap.Int64("account_id", account.ID),
			zap.String("account_name", account.Name),
			zap.String("schedule_layer", scheduleDecision.Layer),
			zap.Int("candidate_count", scheduleDecision.CandidateCount),
		)

		maxReasoningEffort, reasoningEffortMappings, maxReasoningEffortOverLimit, _ := openAIReasoningEffortPolicyForRequest(c, apiKey)
		var requestPayloadHash string
		var turnStartsMu sync.Mutex
		turnStarts := make(map[int]time.Time, 4)
		recordTurnStart := func(turn int, startedAt time.Time) {
			if turn <= 0 || startedAt.IsZero() {
				return
			}
			turnStartsMu.Lock()
			turnStarts[turn] = startedAt
			turnStartsMu.Unlock()
		}
		getTurnStart := func(turn int) time.Time {
			turnStartsMu.Lock()
			startedAt := turnStarts[turn]
			delete(turnStarts, turn)
			turnStartsMu.Unlock()
			return startedAt
		}
		// Keep an immutable mapping snapshot per turn. The local WS relay can
		// read the next client frame concurrently with the previous terminal
		// callback, so a one-slot "latest mapping" value is not sufficient.
		var turnChannelMappingMu sync.RWMutex
		turnChannelMappings := map[int]service.ChannelMappingResult{1: channelMappingWS}
		var turnPricing openAIWSTurnPricing
		hooks := &service.OpenAIWSIngressHooks{
			ClientLifecycleContext:      clientLifecycleCtx,
			InitialRequestModel:         reqModel,
			InitialTurnStartedAt:        firstTurnStartedAt,
			MaxReasoningEffort:          maxReasoningEffort,
			MaxReasoningEffortOverLimit: maxReasoningEffortOverLimit,
			ReasoningEffortMappings:     reasoningEffortMappings,
			TurnStarted:                 recordTurnStart,
			BeforeRequest: func(turn int, payload []byte, originalModel string) error {
				service.BeginOpsStreamTurn(c, turn)
				setCyberTurnBody(turn, payload)
				// Passthrough ingress intentionally skips BeforeTurn, so enforce only
				// the connection-level cyber session gate here as well. Native ingress
				// visits this hook first and gets the same side-effect-free close error;
				// its original BeforeTurn guard remains as defense in depth.
				if cyberBlockedThisConn {
					return service.NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, cyberSessionBlockedClientMsg, nil)
				}
				if turn == 1 {
					return nil
				}
				if !gjson.ValidBytes(payload) {
					return service.NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "invalid websocket request payload", errors.New("invalid json"))
				}
				model := strings.TrimSpace(originalModel)
				if model == "" {
					model = strings.TrimSpace(gjson.GetBytes(payload, "model").String())
				}
				if model == "" {
					model = reqModel
				}
				if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIResponses, model, payload); decision != nil && decision.Blocked {
					writeContentModerationWSError(ctx, wsConn, decision)
					return service.NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, decision.Message, nil)
				}
				return nil
			},
			MapRequestModel: func(turn int, originalModel string) (string, error) {
				model := strings.TrimSpace(originalModel)
				if model == "" {
					model = reqModel
				}
				setOpsRequestContext(c, model, true)
				mapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(ctx, apiKey.GroupID, model)
				turnChannelMappingMu.Lock()
				mappedModelUnchanged := false
				if previous, ok := turnChannelMappings[turn-1]; ok {
					mappedModelUnchanged = strings.TrimSpace(previous.MappedModel) == strings.TrimSpace(mapping.MappedModel)
				}
				if turn > 1 && !mappedModelUnchanged && !account.IsModelSupported(model) && !account.IsModelSupported(mapping.MappedModel) {
					turnChannelMappingMu.Unlock()
					return "", newOpenAIWSUnsupportedModelSwitchError(mapping.MappedModel)
				}
				turnChannelMappings[turn] = mapping
				turnChannelMappingMu.Unlock()
				return mapping.MappedModel, nil
			},
			BeforeTurn: func(turn int) error {
				// turn==1 的会话屏蔽已由握手层检查覆盖；连接内 flag 只拦截后续 turn。
				if cyberBlockedThisConn {
					return service.NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, cyberSessionBlockedClientMsg, nil)
				}
				turnCtx, turnAt := h.gatewayService.WithOpenAITurnPricingContext(ctx, apiKey.GroupID)
				if _, vetoed, reason := h.gatewayService.ProfitControlVetoLatest(turnCtx, account); vetoed {
					reqLog.Info("openai.websocket_turn_profit_vetoed",
						zap.Int("turn", turn),
						zap.Int64("account_id", account.ID),
						zap.String("reason", reason))
					return service.NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "account is no longer eligible for this connection, please reconnect", nil)
				}
				turnPricing.freeze(turnAt)
				if turn == 1 {
					return nil
				}
				// 防御式清理：避免异常路径下旧槽位覆盖导致泄漏。
				releaseTurnSlots()
				// 非首轮 turn 需要重新抢占并发槽位，避免长连接空闲占槽。
				userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlot(ctx, subject.UserID, subject.Concurrency)
				if err != nil {
					return service.NewOpenAIWSClientCloseError(coderws.StatusInternalError, "failed to acquire user concurrency slot", err)
				}
				if !userAcquired {
					return service.NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "too many concurrent requests, please retry later", nil)
				}
				accountReleaseFunc, accountAcquired, err := h.concurrencyHelper.TryAcquireAccountSlot(ctx, account.ID, accountMaxConcurrency)
				if err != nil {
					if userReleaseFunc != nil {
						userReleaseFunc()
					}
					return service.NewOpenAIWSClientCloseError(coderws.StatusInternalError, "failed to acquire account concurrency slot", err)
				}
				if !accountAcquired {
					if userReleaseFunc != nil {
						userReleaseFunc()
					}
					return service.NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "account is busy, please retry later", nil)
				}
				currentUserRelease = wrapReleaseOnDone(ctx, userReleaseFunc)
				currentAccountRelease = wrapReleaseOnDone(ctx, accountReleaseFunc)
				return nil
			},
			AfterTurn: func(turn int, result *service.OpenAIForwardResult, turnErr error) {
				turnStart := getTurnStart(turn)
				cyberBlockBody := takeCyberTurnBody(turn)
				// F1: cyber 标记按 turn 生命周期清理——defer 保证任意早返回路径都执行；
				// CyberBlocked 必须在 submit 前同步预捕获（task 闭包由 worker 池异步执行，
				// 届时 defer 已清除标记）。
				defer clearCyberPolicyTurnState(c)
				releaseTurnSlots()
				turnRequestedModel := reqModel
				turnUpstreamModel := ""
				if result != nil && turn > 1 {
					if model := strings.TrimSpace(result.Model); model != "" {
						turnRequestedModel = model
					}
				}
				if result != nil {
					turnUpstreamModel = strings.TrimSpace(result.UpstreamModel)
				}
				var turnMapping service.ChannelMappingResult
				turnChannelMappingMu.Lock()
				turnMapping, mappingFound := turnChannelMappings[turn]
				delete(turnChannelMappings, turn-1)
				turnChannelMappingMu.Unlock()
				if !mappingFound {
					turnMapping, _ = h.gatewayService.ResolveChannelMappingAndRestrict(ctx, apiKey.GroupID, turnRequestedModel)
				}
				if turnUpstreamModel == "" {
					turnUpstreamModel = turnRequestedModel
				}
				turnUsageFields := turnMapping.ToUsageFields(turnRequestedModel, turnUpstreamModel)
				h.recordCyberPolicyIfMarked(c, apiKey, account, subscription, turnRequestedModel, turnErr != nil, cyberBlockBody, turnUsageFields, requestPayloadHash)
				if service.GetOpsCyberPolicy(c) != nil {
					cyberBlockedThisConn = true
				}
				if turnErr != nil {
					if result == nil || result.ImageCount <= 0 {
						return
					}
					// cyber 命中时该 turn 的用量已由 recordCyberPolicyIfMarked(forwardErrored=true)
					// 按真实 token 记录，这里不再走下方 RecordUsage，避免对同一 turn 双写/双扣费。
					if service.GetOpsCyberPolicy(c) != nil {
						return
					}
					reqLog.Warn("openai.websocket_partial_error_with_image_result",
						zap.Int64("account_id", account.ID),
						zap.Int("image_count", result.ImageCount),
						zap.Error(turnErr),
					)
				}
				if result == nil {
					return
				}
				result.BillingModel = openAIWSTurnBillingModel(result, turnMapping, turnRequestedModel, turnUpstreamModel)
				reqLog.Debug("openai.websocket_turn_billing",
					zap.Int("turn", turn),
					zap.String("turn_requested_model", turnRequestedModel),
					zap.String("turn_upstream_model", turnUpstreamModel),
					zap.String("billing_model", result.BillingModel),
				)
				if account.Type == service.AccountTypeOAuth && !account.IsShadow() {
					h.gatewayService.UpdateCodexUsageSnapshotFromHeaders(ctx, account.ID, result.ResponseHeaders)
				}
				scheduleModel := turnUpstreamModel
				if scheduleModel == "" {
					scheduleModel = turnRequestedModel
				}
				h.gatewayService.ReportOpenAIAccountScheduleResult(account, scheduleModel, openAIForwardSucceededForScheduling(result), result.FirstTokenMs)
				inboundEndpoint := GetInboundEndpoint(c)
				upstreamEndpoint := resolveOpenAIUpstreamEndpoint(c, account, result)
				quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)
				sessionID := service.ExtractClientSessionID(c)
				turnRecordPricingAt := turnPricing.currentOr(turnStart)
				cyberBlocked := service.GetOpsCyberPolicy(c) != nil
				h.submitOpenAIUsageRecordTask(ctx, result, func(taskCtx context.Context) {
					if err := h.gatewayService.RecordUsage(taskCtx, &service.OpenAIRecordUsageInput{
						Result:             result,
						APIKey:             apiKey,
						User:               apiKey.User,
						Account:            account,
						Subscription:       subscription,
						InboundEndpoint:    inboundEndpoint,
						UpstreamEndpoint:   upstreamEndpoint,
						UserAgent:          userAgent,
						IPAddress:          clientIP,
						RequestPayloadHash: requestPayloadHash,
						APIKeyService:      h.apiKeyService,
						QuotaPlatform:      quotaPlatform,
						SessionID:          sessionID,
						ChannelUsageFields: turnUsageFields,
						PricingAt:          turnRecordPricingAt,
						CyberBlocked:       cyberBlocked,
					}); err != nil {
						reqLog.Error("openai.websocket_record_usage_failed",
							zap.Int64("account_id", account.ID),
							zap.String("request_id", result.RequestID),
							zap.Error(err),
						)
					}
				})
			},
		}

		wsFirstMessage := wsAttemptMessage
		// 切组/会话失配防护：previous_response_id 未在当前分组命中粘连账号（StickyPreviousHit=false），
		// 说明该会话链不属于本次调度到的账号，原样转发会触发上游会话链鉴权失败（“鉴权失败，请检查 API Key”）。
		// 故剥离首包里的 previous_response_id，改用首包内 input 重建上下文；带 function_call_output 的
		// 工具续链无法重建，保持原样。仅作用于首轮首包，后续 turn 的续链由 WS 转发层既有逻辑处理。
		if previousResponseID != "" && !scheduleDecision.StickyPreviousHit && previousResponseCanMove {
			wsFirstMessage = service.RemovePreviousResponseIDFromBody(wsFirstMessage)
			reqLog.Debug("openai.websocket_previous_response_id_stripped_cross_group",
				zap.Int64("account_id", account.ID),
				zap.String("schedule_layer", scheduleDecision.Layer),
			)
		}

		// WebSocket 首包可能很大，hash 必须在 hooks 外算成字符串，避免 AfterTurn 闭包保活请求体。
		requestPayloadHash = service.HashUsageRequestPayload(wsFirstMessage)
		if preemptCtx, cleanupPreempt, armed := h.gatewayService.BeginOpenAIWSIngressSessionPreemption(ctx, c, account, wsFirstMessage); armed {
			ctx = preemptCtx
			defer cleanupPreempt()
		}

		for {
			err := h.gatewayService.ProxyResponsesWebSocketFromClient(ctx, c, wsConn, account, token, wsFirstMessage, hooks)
			if err == nil {
				reqLog.Info("openai.websocket_ingress_closed", zap.Int64("account_id", account.ID))
				return
			}
			if service.IsOpenAIWSSessionPreemptedError(err) {
				return
			}
			var failoverErr *service.UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				retryPayload, retryCurrentTurn := service.OpenAIWSCurrentTurnRetryPayload(err)
				nextAttemptMessage, retrySafe := openAIWSNextAttemptMessage(wsAttemptMessage, retryPayload, retryCurrentTurn)
				if !retrySafe {
					closeOpenAIWSFailoverExhausted(c, wsConn, failoverErr)
					return
				}
				wsAttemptMessage = nextAttemptMessage
				if retryCurrentTurn {
					previousResponseID = ""
					reqLog.Warn("openai.websocket_current_turn_failover_retry",
						zap.Int64("account_id", account.ID),
						zap.Int("upstream_status", failoverErr.StatusCode),
						zap.Int("retry_payload_bytes", len(retryPayload)),
					)
				}
				if waitForWSSameAccountRetry(account, failoverErr) {
					if failoverErr.ShouldReportAccountScheduleFailure() {
						h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, wsForwardModel, false, nil), false, nil, err)
					}
					if !ensureUserSlotHeld() {
						return
					}
					if currentAccountRelease == nil {
						accountRelease, acquired, acquireErr := h.concurrencyHelper.TryAcquireAccountSlot(ctx, account.ID, accountMaxConcurrency)
						if acquireErr != nil || !acquired {
							reqLog.Warn("openai.websocket_same_account_retry_slot_unavailable",
								zap.Int64("account_id", account.ID),
								zap.Error(acquireErr),
							)
							closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "account is busy, please retry later")
							return
						}
						currentAccountRelease = wrapReleaseOnDone(ctx, accountRelease)
					}
					wsFirstMessage = wsAttemptMessage
					continue
				}
				if handleWSFailover(account, failoverErr) {
					break
				}
				return
			}

			if shouldReportOpenAIWSProxyAccountFailure(err) {
				h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, wsForwardModel, false, nil), false, nil, err)
			}
			closeStatus, closeReason := summarizeWSCloseErrorForLog(err)
			reqLog.Warn("openai.websocket_proxy_failed",
				zap.Int64("account_id", account.ID),
				zap.Error(err),
				zap.String("close_status", closeStatus),
				zap.String("close_reason", closeReason),
			)
			var closeErr *service.OpenAIWSClientCloseError
			if errors.As(err, &closeErr) {
				closeOpenAIClientWS(wsConn, closeErr.StatusCode(), closeErr.Reason())
				return
			}
			closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "upstream websocket proxy failed")
			return
		}
	}

}

func (h *OpenAIGatewayHandler) recoverResponsesPanic(c *gin.Context, streamStarted *bool) {
	recovered := recover()
	if recovered == nil {
		return
	}

	started := false
	if streamStarted != nil {
		started = *streamStarted
	}
	// Forward may panic after flushing a transport-only SSE comment but before
	// the main loop can persist that state. Re-read the writer here so panic
	// recovery never appends a standalone JSON response to committed HTTP 200.
	started = started || service.OpenAIStreamTransportCommitted(c)
	wroteFallback := h.ensureForwardErrorResponse(c, started)
	requestLogger(c, "handler.openai_gateway.responses").Error(
		"openai.responses_panic_recovered",
		zap.Bool("fallback_error_response_written", wroteFallback),
		zap.Any("panic", recovered),
		zap.ByteString("stack", debug.Stack()),
	)
}

// recoverAnthropicMessagesPanic recovers from panics in the Anthropic Messages
// handler and returns an Anthropic-formatted error response.
func (h *OpenAIGatewayHandler) recoverAnthropicMessagesPanic(c *gin.Context, streamStarted *bool) {
	recovered := recover()
	if recovered == nil {
		return
	}

	started := streamStarted != nil && *streamStarted
	requestLogger(c, "handler.openai_gateway.messages").Error(
		"openai.messages_panic_recovered",
		zap.Bool("stream_started", started),
		zap.Any("panic", recovered),
		zap.ByteString("stack", debug.Stack()),
	)
	if !started {
		h.anthropicErrorResponse(c, http.StatusInternalServerError, "api_error", "Internal server error")
	}
}

func (h *OpenAIGatewayHandler) ensureResponsesDependencies(c *gin.Context, reqLog *zap.Logger) bool {
	missing := h.missingResponsesDependencies()
	if len(missing) == 0 {
		return true
	}

	if reqLog == nil {
		reqLog = requestLogger(c, "handler.openai_gateway.responses")
	}
	reqLog.Error("openai.handler_dependencies_missing", zap.Strings("missing_dependencies", missing))

	if c != nil && c.Writer != nil && !c.Writer.Written() {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"type":    "api_error",
				"message": "Service temporarily unavailable",
			},
		})
	}
	return false
}

func (h *OpenAIGatewayHandler) missingResponsesDependencies() []string {
	missing := make([]string, 0, 5)
	if h == nil {
		return append(missing, "handler")
	}
	if h.gatewayService == nil {
		missing = append(missing, "gatewayService")
	}
	if h.billingCacheService == nil {
		missing = append(missing, "billingCacheService")
	}
	if h.apiKeyService == nil {
		missing = append(missing, "apiKeyService")
	}
	if h.concurrencyHelper == nil || h.concurrencyHelper.concurrencyService == nil {
		missing = append(missing, "concurrencyHelper")
	}
	return missing
}

func getContextInt64(c *gin.Context, key string) (int64, bool) {
	if c == nil || key == "" {
		return 0, false
	}
	v, ok := c.Get(key)
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	case float64:
		return int64(t), true
	default:
		return 0, false
	}
}

func (h *OpenAIGatewayHandler) submitUsageRecordTask(parent context.Context, task service.UsageRecordTask) {
	if task == nil {
		return
	}
	task = wrapUsageRecordTaskContext(parent, task)
	if h.usageRecordWorkerPool != nil {
		if mode := h.usageRecordWorkerPool.Submit(task); mode != service.UsageRecordSubmitModeDroppedStopped {
			return
		}
		// 池已停止（进程关停窗口）：计费任务不能静默丢失，降级为内联同步执行。
		// 显式配置的 drop/sample 溢出丢弃仍按配置语义保留。
		logger.L().With(
			zap.String("component", "handler.openai_gateway.responses"),
		).Warn("openai.usage_record_task_stopped_sync_fallback")
	}
	// 回退路径：worker 池未注入或已停止时同步执行，避免退回到无界 goroutine 模式。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.L().With(
				zap.String("component", "handler.openai_gateway.responses"),
				zap.Any("panic", recovered),
			).Error("openai.usage_record_task_panic_recovered")
		}
	}()
	task(ctx)
}

func (h *OpenAIGatewayHandler) submitOpenAIUsageRecordTask(parent context.Context, result *service.OpenAIForwardResult, task service.UsageRecordTask) {
	// Money-critical bills never drop on pool overflow: media, search surcharge, voice.
	if result != nil && (result.ImageCount > 0 || result.VideoCount > 0 ||
		result.SearchCount > 0 || result.WebSearchCalls > 0 || result.AudioUsage != nil) {
		h.submitMandatoryUsageRecordTask(parent, task)
		return
	}
	h.submitUsageRecordTask(parent, task)
}

func (h *OpenAIGatewayHandler) submitMandatoryUsageRecordTask(parent context.Context, task service.UsageRecordTask) {
	if task == nil {
		return
	}
	task = wrapUsageRecordTaskContext(parent, task)
	if h.usageRecordWorkerPool != nil {
		if mode := h.usageRecordWorkerPool.Submit(task); !mode.Dropped() {
			return
		}
		logger.L().With(
			zap.String("component", "handler.openai_gateway.usage"),
		).Warn("openai.usage_record_task_mandatory_sync_fallback")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.L().With(
				zap.String("component", "handler.openai_gateway.usage"),
				zap.Any("panic", recovered),
			).Error("openai.usage_record_task_panic_recovered")
		}
	}()
	task(ctx)
}

func (h *OpenAIGatewayHandler) acquireImageGenerationSlot(c *gin.Context, streamStarted bool) (func(), bool) {
	if h == nil || h.cfg == nil || h.imageLimiter == nil {
		return nil, true
	}
	imageConcurrency := h.cfg.Gateway.ImageConcurrency
	wait := strings.TrimSpace(imageConcurrency.OverflowMode) == config.ImageConcurrencyOverflowModeWait
	release, acquired := h.imageLimiter.Acquire(
		c.Request.Context(),
		imageConcurrency.Enabled,
		imageConcurrency.MaxConcurrentRequests,
		wait,
		time.Duration(imageConcurrency.WaitTimeoutSeconds)*time.Second,
		imageConcurrency.MaxWaitingRequests,
	)
	if acquired {
		return release, true
	}
	h.handleStreamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error", "Image generation concurrency limit exceeded, please retry later", streamStarted)
	return nil, false
}

// handleConcurrencyError handles concurrency-related acquire errors.
func (h *OpenAIGatewayHandler) handleConcurrencyError(c *gin.Context, err error, slotType string, streamStarted bool) {
	status, errType, message := concurrencyErrorResponse(err, slotType)
	h.handleStreamingAwareError(c, status, errType, message, streamStarted)
}

func (h *OpenAIGatewayHandler) handleFailoverExhausted(c *gin.Context, failoverErr *service.UpstreamFailoverError, streamStarted bool) {
	if failoverErr == nil {
		h.handleFailoverExhaustedSimple(c, http.StatusBadGateway, streamStarted)
		return
	}
	copyFailoverRetryAfter(c, failoverErr.ResponseHeaders)
	if failoverErr.IsCredentialFailure() {
		status, message := credentialFailoverClientResponse(failoverErr)
		h.handleStreamingAwareError(c, status, "api_error", message, streamStarted)
		return
	}
	if failoverErr.IsOpenAICapacityShed() && strings.TrimSpace(failoverErr.ClientMessage) != "" {
		status := failoverErr.ClientStatusCode
		if status <= 0 {
			status = http.StatusServiceUnavailable
		}
		h.handleStreamingAwareError(c, status, "server_error", failoverErr.ClientMessage, streamStarted)
		return
	}
	statusCode := failoverErr.StatusCode
	responseBody := failoverErr.ResponseBody

	// 先检查透传规则
	if h.errorPassthroughService != nil && len(responseBody) > 0 {
		if rule := h.errorPassthroughService.MatchRule("openai", statusCode, responseBody); rule != nil {
			// 确定响应状态码
			respCode := statusCode
			if !rule.PassthroughCode && rule.ResponseCode != nil {
				respCode = *rule.ResponseCode
			}

			// 确定响应消息
			msg := service.ExtractUpstreamErrorMessage(responseBody)
			if !rule.PassthroughBody && rule.CustomMessage != nil {
				msg = *rule.CustomMessage
			}

			if rule.SkipMonitoring {
				c.Set(service.OpsSkipPassthroughKey, true)
			}

			// codex round 11am follow-up (2026-05-15): align with
			// GatewayHandler.handlePassthroughError (gateway_handler.go:1435) —
			// errType "upstream_error" was a fork tell (not an Anthropic
			// legal error type). Customers passthrough-rule path now also
			// sees neutral "api_error". upstream msg still passes through
			// when rule.PassthroughBody=true.
			h.handleStreamingAwareError(c, respCode, "api_error", msg, streamStarted)
			return
		}
	}

	// 记录原始上游状态码，以便 ops 错误日志捕获真实的上游错误
	upstreamMsg := service.ExtractUpstreamErrorMessage(responseBody)
	service.SetOpsUpstreamError(c, statusCode, upstreamMsg, "")

	// 使用默认的错误映射
	status, errType, errMsg := h.mapUpstreamError(statusCode)
	h.handleStreamingAwareError(c, status, errType, errMsg, streamStarted)
}

func credentialFailoverClientResponse(failoverErr *service.UpstreamFailoverError) (int, string) {
	_ = failoverErr
	return http.StatusServiceUnavailable, anthropicTemporaryUnavailableMessage
}

func copyFailoverRetryAfter(c *gin.Context, headers http.Header) {
	if c == nil || headers == nil {
		return
	}
	retryAfter := strings.TrimSpace(headers.Get("Retry-After"))
	if retryAfter == "" || len(retryAfter) > 128 || strings.ContainsAny(retryAfter, "\r\n") || !isSafeRetryAfter(retryAfter) {
		return
	}
	c.Header("Retry-After", retryAfter)
}

func isSafeRetryAfter(value string) bool {
	digitsOnly := true
	for _, char := range value {
		if char < '0' || char > '9' {
			digitsOnly = false
			break
		}
	}
	if digitsOnly {
		seconds, err := strconv.ParseUint(value, 10, 32)
		return err == nil && seconds <= uint64((7*24*time.Hour)/time.Second)
	}
	retryAt, err := http.ParseTime(value)
	if err != nil {
		return false
	}
	return !retryAt.After(time.Now().Add(7 * 24 * time.Hour))
}

// handleFailoverExhaustedSimple 简化版本，用于没有响应体的情况
func (h *OpenAIGatewayHandler) handleFailoverExhaustedSimple(c *gin.Context, statusCode int, streamStarted bool) {
	status, errType, errMsg := h.mapUpstreamError(statusCode)
	service.SetOpsUpstreamError(c, statusCode, errMsg, "")
	h.handleStreamingAwareError(c, status, errType, errMsg, streamStarted)
}

const anthropicTemporaryUnavailableMessage = "The service is temporarily unavailable. Please retry."

// mapUpstreamError — codex round 11am (2026-05-15): 客户响应中性化.
// 之前 errType="upstream_error" 不是 Anthropic 协议合法值 (合法只有
// invalid_request_error / authentication_error / permission_error /
// not_found_error / rate_limit_error / api_error / overloaded_error),
// message 含 "Upstream" 暴露 fork 内部架构. 改:
//
//	errType: upstream_error → api_error (Anthropic 通用 5xx)
//	message: 去 "Upstream"/"please contact administrator" 中性化
func (h *OpenAIGatewayHandler) mapUpstreamError(statusCode int) (int, string, string) {
	switch statusCode {
	case 401:
		return http.StatusBadGateway, "api_error", "Authentication failed. Please try again later."
	case 403:
		return http.StatusBadGateway, "api_error", "Access denied. Please try again later."
	case 429:
		return http.StatusTooManyRequests, "rate_limit_error", "Rate limit exceeded. Please retry later."
	case 529:
		return http.StatusServiceUnavailable, "overloaded_error", "Service overloaded. Please retry later."
	case 500, 502, 503, 504:
		return http.StatusBadGateway, "api_error", anthropicTemporaryUnavailableMessage
	default:
		return http.StatusBadGateway, "api_error", "Internal server error"
	}
}

// handleStreamingAwareError handles errors that may occur after streaming has started
func (h *OpenAIGatewayHandler) handleStreamingAwareError(c *gin.Context, status int, errType, message string, streamStarted bool) {
	// body-signal compact 心跳可能已把响应头提交为 200：先停心跳（建立
	// happens-before，接管 ResponseWriter），并升级为流内错误处理。
	if service.StopOpenAICompactSSEKeepaliveCommitted(c) {
		streamStarted = true
	}
	if streamStarted {
		// /v1/responses 的严格 SDK（Codex CLI）要求终止事件必须属于
		// response.completed/failed/incomplete/cancelled 集合。
		// 通用 `event: error` 帧不被识别为终止事件，会导致
		// "stream closed before response.completed"。
		if inboundIsResponses(c) {
			if writeResponsesFailedSSE(c, errType, message) {
				return
			}
		}
		// Stream already started, send error as SSE event then close
		flusher, ok := c.Writer.(http.Flusher)
		if ok {
			// SSE 错误事件固定 schema，使用 Quote 直拼可避免额外 Marshal 分配。
			errorEvent := "event: error\ndata: " + `{"error":{"type":` + strconv.Quote(errType) + `,"message":` + strconv.Quote(message) + `}}` + "\n\n"
			if _, err := fmt.Fprint(c.Writer, errorEvent); err != nil {
				_ = c.Error(err)
			}
			flusher.Flush()
		}
		return
	}

	// Normal case: return JSON response with proper status code
	h.errorResponse(c, status, errType, message)
}

// ensureForwardErrorResponse 在 Forward 返回错误但尚未写响应时补写统一错误响应。
func (h *OpenAIGatewayHandler) ensureForwardErrorResponse(c *gin.Context, streamStarted bool) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	// 先停 compact 心跳再读 Writer 状态，避免与心跳 goroutine 竞争。
	if service.StopOpenAICompactSSEKeepaliveCommitted(c) {
		streamStarted = true
	}
	if service.IsResponseCommitted(c) {
		return false
	}
	if c.Writer.Written() {
		streamStarted = true
	}
	h.handleStreamingAwareError(c, http.StatusBadGateway, "api_error", anthropicTemporaryUnavailableMessage, streamStarted)
	return true
}

func shouldLogOpenAIForwardFailureAsWarn(c *gin.Context, wroteFallback bool) bool {
	if wroteFallback {
		return false
	}
	if c == nil || c.Writer == nil {
		return false
	}
	return c.Writer.Written()
}

// openAIForwardErrorAlreadyCommunicated reports whether Forward returned an
// error after it had already written the upstream terminal error response to
// the client.
//
// This matters for Responses streams: upstream may return HTTP 200 with a
// non-retryable `response.failed` event (for example a policy/safety rejection).
// The service layer forwards that terminal event verbatim, then returns an
// error so the caller can log/account for the failed upstream response. The
// handler must not append its generic fallback `response.failed`, otherwise
// strict clients may see the useful upstream message replaced by "Upstream
// request failed" or receive duplicate terminal events.
func openAIForwardErrorAlreadyCommunicated(c *gin.Context, writerSizeBeforeForward int, err error) bool {
	if err == nil || c == nil || c.Writer == nil {
		return false
	}
	// 与快照同口径：排除 compact 心跳字节，避免"仅心跳写出"被误判为
	// 响应已写出（#3887）。
	if service.OpenAICompactKeepaliveAdjustedWrittenSize(c) == writerSizeBeforeForward {
		return false
	}

	// cyber_policy 命中时上游原始错误体已透传给客户端（非流式 c.Data 写出 400 body，
	// 流式写出 response.failed 事件），不能再让 ensureForwardErrorResponse 追加
	// fallback —— 否则在已写出的完整响应尾部追加 SSE（responses 端点尾随
	// response.failed、chat 端点尾随 event:error），污染响应体。Size 已变化证明响应确已写出。
	if service.GetOpsCyberPolicy(c) != nil {
		return true
	}

	msg := strings.TrimSpace(err.Error())
	for _, prefix := range []string{
		"upstream response failed:",
		"non-streaming openai protocol error:",
	} {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}

// errorResponse returns OpenAI API format error response
func (h *OpenAIGatewayHandler) errorResponse(c *gin.Context, status int, errType, message string) {
	// body-signal compact 心跳可能已把响应头提交为 200：JSON 错误体会与已
	// 提交的 SSE 流交错，必须降级为 response.failed 终止事件（#3887）。
	if service.StopOpenAICompactSSEKeepaliveCommitted(c) {
		service.MarkOpsStreamError(c, errType, message, status)
		if writeResponsesFailedSSE(c, errType, message) {
			return
		}
	}
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

// openAICompactKeepaliveInterval 复用流式 keepalive 配置作为 compact 下游
// 心跳间隔；0 表示禁用（与流式路径语义一致）。
func (h *OpenAIGatewayHandler) openAICompactKeepaliveInterval() time.Duration {
	if h.cfg == nil || h.cfg.Gateway.StreamKeepaliveInterval <= 0 {
		return 0
	}
	return time.Duration(h.cfg.Gateway.StreamKeepaliveInterval) * time.Second
}

func setOpenAIClientTransportHTTP(c *gin.Context) {
	service.SetOpenAIClientTransport(c, service.OpenAIClientTransportHTTP)
}

func setOpenAIClientTransportWS(c *gin.Context) {
	service.SetOpenAIClientTransport(c, service.OpenAIClientTransportWS)
}

func ensureOpenAIPoolModeSessionHash(sessionHash string, account *service.Account) string {
	if sessionHash != "" || account == nil || !account.IsPoolMode() {
		return sessionHash
	}
	// 为当前请求生成一次性粘性会话键，确保同账号重试不会重新负载均衡到其他账号。
	return "openai-pool-retry-" + uuid.NewString()
}

func openAIWSIngressFallbackSessionSeed(userID, apiKeyID int64, groupID *int64) string {
	gid := int64(0)
	if groupID != nil {
		gid = *groupID
	}
	return fmt.Sprintf("openai_ws_ingress:%d:%d:%d", gid, userID, apiKeyID)
}

func isOpenAIWSUpgradeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	return strings.Contains(strings.ToLower(strings.TrimSpace(r.Header.Get("Connection"))), "upgrade")
}

func closeOpenAIClientWS(conn *coderws.Conn, status coderws.StatusCode, reason string) {
	if conn == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 120 {
		reason = reason[:120]
	}
	_ = conn.Close(status, reason)
	_ = conn.CloseNow()
}

func openAIWSNextAttemptMessage(current, retryPayload []byte, retryCurrentTurn bool) ([]byte, bool) {
	if !retryCurrentTurn {
		return append([]byte(nil), current...), true
	}
	if len(retryPayload) == 0 {
		return nil, false
	}
	return append([]byte(nil), retryPayload...), true
}

func closeOpenAIWSFailoverExhausted(c *gin.Context, conn *coderws.Conn, failoverErr *service.UpstreamFailoverError) {
	intendedStatus := http.StatusBadGateway
	errorType := "upstream_error"
	errorCode := "upstream_ws_failover_exhausted"
	message := "upstream websocket proxy failed"
	closeStatus := coderws.StatusInternalError

	if failoverErr != nil {
		if reason := strings.TrimSpace(string(failoverErr.Reason)); reason != "" {
			errorCode = reason
		}
		if failoverErr.Stage == service.GatewayFailureStageAccountAuth {
			intendedStatus = http.StatusServiceUnavailable
			errorType = "api_error"
			// Provider-scoped failures must not reveal which upstream account
			// pool or credential implementation is unavailable. Account-scoped
			// failures retain the Grok-specific operational signal.
			if failoverErr.Scope == service.GatewayFailureScopeProvider {
				message = anthropicTemporaryUnavailableMessage
			} else {
				message = service.GrokCredentialUnavailableClientMessage
			}
			closeStatus = coderws.StatusTryAgainLater
		} else {
			switch failoverErr.StatusCode {
			case http.StatusTooManyRequests:
				intendedStatus = http.StatusTooManyRequests
				errorType = "rate_limit_error"
				message = "upstream rate limit exceeded, please retry later"
				closeStatus = coderws.StatusTryAgainLater
			case 529, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				intendedStatus = failoverErr.StatusCode
				message = "upstream service temporarily unavailable"
				closeStatus = coderws.StatusTryAgainLater
			case http.StatusUnauthorized, http.StatusForbidden:
				intendedStatus = failoverErr.StatusCode
				errorType = "authentication_error"
				message = "upstream websocket authentication failed"
				closeStatus = coderws.StatusPolicyViolation
			}
		}
	}

	service.MarkOpsStreamFailure(c, errorType, errorCode, message, intendedStatus)
	closeOpenAIClientWS(conn, closeStatus, message)
}

func writeContentModerationWSError(ctx context.Context, conn *coderws.Conn, decision *service.ContentModerationDecision) {
	if conn == nil || decision == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	message := strings.TrimSpace(decision.Message)
	if message == "" {
		message = "content moderation blocked this request"
	}
	payload, err := json.Marshal(gin.H{
		"event_id": "evt_content_moderation_blocked",
		"type":     "error",
		"error": gin.H{
			"type":    "invalid_request_error",
			"code":    contentModerationErrorCode(decision),
			"message": message,
		},
	})
	if err != nil {
		payload = []byte(`{"event_id":"evt_content_moderation_blocked","type":"error","error":{"type":"invalid_request_error","code":"content_policy_violation","message":"content moderation blocked this request"}}`)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = conn.Write(writeCtx, coderws.MessageText, payload)
}

// writeCyberSessionBlockedWSError sends an error frame telling the client this
// session is blocked by the cyber session block (F5a) before closing.
func writeCyberSessionBlockedWSError(ctx context.Context, conn *coderws.Conn) {
	if conn == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := json.Marshal(gin.H{
		"event_id": "evt_cyber_session_blocked",
		"type":     "error",
		"error": gin.H{
			"type":    "permission_error",
			"code":    "session_blocked_by_cyber_policy",
			"message": cyberSessionBlockedClientMsg,
		},
	})
	if err != nil {
		payload = []byte(`{"event_id":"evt_cyber_session_blocked","type":"error","error":{"type":"permission_error","code":"session_blocked_by_cyber_policy","message":"This session is blocked by cyber-security policy, please start a new session"}}`)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = conn.Write(writeCtx, coderws.MessageText, payload)
}

// cyberPolicyRecordedKey guards against double-firing recordCyberPolicyIfMarked
// within one request (e.g. in a retry/failover loop).
const cyberPolicyRecordedKey = "ops_cyber_recorded"

// cyberPolicyOpsErrorMeta carries request-scoped fields captured outside the
// async goroutine for building the cyber ops_error_logs entry.
type cyberPolicyOpsErrorMeta struct {
	RequestID       string
	ClientRequestID string
	Platform        string
	Model           string
	RequestPath     string
	Stream          bool
	InboundEndpoint string
	UserAgent       string
	APIKeyPrefix    string
	UserID          int64
	APIKeyID        int64
	AccountID       int64
	GroupID         *int64
	ClientIP        string
	CreatedAt       time.Time
	SessionBlockKey string
}

// buildCyberPolicyOpsErrorEntry builds the ops_error_logs entry for an upstream
// cyber_policy hit. StatusCode mirrors what the codex client actually received
// (400 non-stream / 200 stream), per F6.
func buildCyberPolicyOpsErrorEntry(meta cyberPolicyOpsErrorMeta, mark *service.CyberPolicyMark) *service.OpsInsertErrorLogInput {
	rt := int16(service.RequestTypeCyberBlocked)
	entry := &service.OpsInsertErrorLogInput{
		RequestID:         meta.RequestID,
		ClientRequestID:   meta.ClientRequestID,
		Platform:          meta.Platform,
		Model:             meta.Model,
		RequestPath:       meta.RequestPath,
		Stream:            meta.Stream,
		InboundEndpoint:   meta.InboundEndpoint,
		RequestType:       &rt,
		UserAgent:         meta.UserAgent,
		APIKeyPrefix:      meta.APIKeyPrefix,
		ErrorPhase:        "request",
		ErrorType:         "cyber_policy",
		Severity:          "P3",
		StatusCode:        mark.UpstreamStatus,
		IsBusinessLimited: true,
		ErrorMessage:      "cyber_policy: " + mark.Message,
		// 原始 body 直接入队；ops service 落库前统一走 sanitizeErrorBodyForStorage 脱敏与截断。
		ErrorBody:   mark.Body,
		ErrorSource: "upstream_http",
		ErrorOwner:  "provider",
		CreatedAt:   meta.CreatedAt,
	}
	if meta.UserID > 0 {
		entry.UserID = &meta.UserID
	}
	if meta.APIKeyID > 0 {
		entry.APIKeyID = &meta.APIKeyID
	}
	if meta.AccountID > 0 {
		entry.AccountID = &meta.AccountID
	}
	entry.GroupID = meta.GroupID
	if meta.ClientIP != "" {
		entry.ClientIP = &meta.ClientIP
	}
	return entry
}

// 双语单串：网关客户端面向中英用户，且本错误无 i18n 协商通道。
const cyberSessionBlockedClientMsg = "该会话已被网络安全策略屏蔽，请开启新会话 / This session is blocked by cyber-security policy, please start a new session"

// buildCyberSessionBlockedOpsEntry builds the ops_error_logs entry for a request
// rejected locally by the cyber session block (F5a). Distinct error_type from
// upstream `cyber_policy`; never feeds moderation logs / violation counting
// (the request never reached upstream — see spec).
func buildCyberSessionBlockedOpsEntry(meta cyberPolicyOpsErrorMeta) *service.OpsInsertErrorLogInput {
	rt := int16(service.RequestTypeCyberBlocked)
	entry := &service.OpsInsertErrorLogInput{
		RequestID:         meta.RequestID,
		ClientRequestID:   meta.ClientRequestID,
		Platform:          meta.Platform,
		Model:             meta.Model,
		RequestPath:       meta.RequestPath,
		Stream:            meta.Stream,
		InboundEndpoint:   meta.InboundEndpoint,
		RequestType:       &rt,
		UserAgent:         meta.UserAgent,
		APIKeyPrefix:      meta.APIKeyPrefix,
		ErrorPhase:        "request",
		ErrorType:         "cyber_policy_session_blocked",
		Severity:          "P3",
		StatusCode:        http.StatusForbidden,
		IsBusinessLimited: true,
		ErrorMessage:      "cyber_policy_session_blocked: request rejected locally by session block",
		ErrorSource:       "gateway_local",
		ErrorOwner:        "platform",
		CreatedAt:         meta.CreatedAt,
		// AccountID 有意不设：请求在账号选择前即被拒绝。
	}
	if meta.SessionBlockKey != "" {
		entry.ErrorBody = "session_block_key=" + meta.SessionBlockKey
	}
	if meta.UserID > 0 {
		entry.UserID = &meta.UserID
	}
	if meta.APIKeyID > 0 {
		entry.APIKeyID = &meta.APIKeyID
	}
	entry.GroupID = meta.GroupID
	if meta.ClientIP != "" {
		entry.ClientIP = &meta.ClientIP
	}
	return entry
}

// cyberSessionBlockFormat selects the per-endpoint error envelope for a locally
// blocked session (用户决策：兼容路径各自格式).
type cyberSessionBlockFormat int

const (
	cyberBlockFormatResponses cyberSessionBlockFormat = iota
	cyberBlockFormatChat
	cyberBlockFormatAnthropic
)

// rejectIfCyberSessionBlocked checks the session-block table BEFORE account
// selection. Returns true when the request was rejected (response already
// written + ops entry enqueued). Fail-open: disabled switch / empty key /
// store error → false.
func (h *OpenAIGatewayHandler) rejectIfCyberSessionBlocked(c *gin.Context, apiKey *service.APIKey, body []byte, model string, format cyberSessionBlockFormat) bool {
	if h == nil || h.gatewayService == nil || apiKey == nil {
		return false
	}
	// 开关默认关：先走 ~ns 级缓存开关检查，再付出 key 派生(gjson+sha256)成本。
	if enabled, _ := h.gatewayService.CyberSessionBlockRuntime(c.Request.Context()); !enabled {
		return false
	}
	key := findBlockedCyberSessionKey(c.Request.Context(), h.gatewayService, apiKey.ID, c, body)
	if key == "" {
		return false
	}
	// body-signal compact 心跳可能已把响应头提交为 200（cyber 检查在用户槽位
	// 长等待之后执行）：以 response.failed 终止事件回传；未提交时停拍后照常
	// 写 JSON（#3887）。
	if service.StopOpenAICompactSSEKeepaliveCommitted(c) {
		service.MarkOpsStreamError(c, "permission_error", cyberSessionBlockedClientMsg, http.StatusForbidden)
		if writeResponsesFailedSSE(c, "permission_error", cyberSessionBlockedClientMsg) {
			h.enqueueCyberSessionBlockedOpsEntry(c, apiKey, model, key)
			return true
		}
	}
	switch format {
	case cyberBlockFormatAnthropic:
		c.JSON(http.StatusForbidden, gin.H{"type": "error", "error": gin.H{
			"type":    "permission_error",
			"message": cyberSessionBlockedClientMsg,
		}})
	default: // cyberBlockFormatResponses 与 cyberBlockFormatChat：同构的 OpenAI error envelope
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{
			"type":    "permission_error",
			"code":    "session_blocked_by_cyber_policy",
			"message": cyberSessionBlockedClientMsg,
		}})
	}
	h.enqueueCyberSessionBlockedOpsEntry(c, apiKey, model, key)
	return true
}

type cyberSessionBlockWritePlan struct {
	scopeKey string
	keys     []string
}

func buildCyberSessionBlockWritePlan(apiKeyID int64, c *gin.Context, body []byte) cyberSessionBlockWritePlan {
	plan := cyberSessionBlockWritePlan{}
	if key := service.CyberSessionExplicitBlockKey(apiKeyID, c, body); key != "" {
		plan.keys = append(plan.keys, key)
	}
	transcriptKeys := service.CyberSessionTranscriptBlockKeys(apiKeyID, body)
	for _, key := range transcriptKeys {
		if len(plan.keys) == 0 || key != plan.keys[0] {
			plan.keys = append(plan.keys, key)
		}
	}
	if len(transcriptKeys) > 0 {
		plan.scopeKey = cyberSessionScopeKey(apiKeyID, c)
	}
	return plan
}

func findBlockedCyberSessionKey(ctx context.Context, gatewayService *service.OpenAIGatewayService, apiKeyID int64, c *gin.Context, body []byte) string {
	if gatewayService == nil {
		return ""
	}
	clientIP, userAgent := "", ""
	if c != nil {
		clientIP = strings.TrimSpace(ip.GetClientIP(c))
		userAgent = c.GetHeader("User-Agent")
	}
	return gatewayService.FindCyberSessionBlockedForRequest(ctx, apiKeyID, c, body, clientIP, userAgent)
}

func cyberSessionScopeKey(apiKeyID int64, c *gin.Context) string {
	if c == nil {
		return ""
	}
	return service.CyberSessionScopeKey(apiKeyID, strings.TrimSpace(ip.GetClientIP(c)), c.GetHeader("User-Agent"))
}

// enqueueCyberSessionBlockedOpsEntry captures request meta and enqueues the
// ops_error_logs entry for a locally blocked request.
func (h *OpenAIGatewayHandler) enqueueCyberSessionBlockedOpsEntry(c *gin.Context, apiKey *service.APIKey, model string, sessionBlockKey string) {
	if h.opsService == nil {
		return
	}
	// The dedicated cyber_session_blocked entry owns Ops semantics for this
	// request; suppress the generic middleware record of the same 403 response.
	c.Set(opsDedicatedErrorRecordedKey, true)
	meta := cyberPolicyOpsErrorMeta{Model: model, InboundEndpoint: GetInboundEndpoint(c), CreatedAt: time.Now(), SessionBlockKey: sessionBlockKey}
	meta.RequestID = c.Writer.Header().Get("X-Request-Id")
	if c.Request != nil && c.Request.URL != nil {
		meta.RequestPath = c.Request.URL.Path
	}
	if v, ok := c.Get(opsStreamKey); ok {
		if b, ok := v.(bool); ok {
			meta.Stream = b
		}
	}
	meta.Platform = resolveOpsPlatform(cyberPolicyRequestContext(c), apiKey, guessPlatformFromPath(meta.RequestPath))
	if c.Request != nil {
		meta.ClientRequestID, _ = c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		meta.UserAgent = c.GetHeader("User-Agent")
		meta.ClientIP = strings.TrimSpace(ip.GetClientIP(c))
	}
	meta.APIKeyID = apiKey.ID
	meta.GroupID = apiKey.GroupID
	meta.APIKeyPrefix = keyPrefix(apiKey.Key, 8)
	if apiKey.User != nil {
		meta.UserID = apiKey.User.ID
	}
	enqueueOpsErrorLog(h.opsService, buildCyberSessionBlockedOpsEntry(meta))
}

// recordCyberPolicyIfMarked 在 gateway forward 返回后检查 cyber 标记，异步写风控日志/邮件，
// 并在 forward 返回错误时依上游报告的真实 token 写入 cyber 用量与计费。标记由 gateway
// 服务层在透传 cyber 后设置；
// 当前请求已发给用户，本方法只做事后记录，不影响响应。forwardErrored 为 true 时才写用量行，
// 避免与正常 RecordUsage(forward 成功路径)重复。每请求至多记录一次。
func (h *OpenAIGatewayHandler) recordCyberPolicyIfMarked(c *gin.Context, apiKey *service.APIKey, account *service.Account, subscription *service.UserSubscription, model string, forwardErrored bool, cyberBlockBody []byte, channelFields service.ChannelUsageFields, requestPayloadHash string) {
	mark := service.GetOpsCyberPolicy(c)
	if mark == nil {
		return
	}
	if c.GetBool(cyberPolicyRecordedKey) {
		return
	}
	c.Set(cyberPolicyRecordedKey, true)

	requestID := c.Writer.Header().Get("X-Request-Id")
	var userID, apiKeyID int64
	var userEmail, apiKeyName, groupName string
	var groupID *int64
	if apiKey != nil {
		apiKeyID = apiKey.ID
		apiKeyName = apiKey.Name
		groupID = apiKey.GroupID
		if apiKey.User != nil {
			userID = apiKey.User.ID
			userEmail = apiKey.User.Email
		}
		if apiKey.Group != nil {
			groupName = apiKey.Group.Name
		}
	}
	inboundEndpoint := GetInboundEndpoint(c)
	upstreamEndpoint := ""
	var accountID int64
	if account != nil {
		accountID = account.ID
		upstreamEndpoint = resolveOpenAIUpstreamEndpoint(c, account, nil)
	}
	stream := false
	if v, ok := c.Get(opsStreamKey); ok {
		if b, ok := v.(bool); ok {
			stream = b
		}
	}
	cmSvc := h.contentModerationService
	gwSvc := h.gatewayService
	opsSvc := h.opsService
	apiKeySvc := h.apiKeyService
	requestPath := ""
	if c.Request != nil && c.Request.URL != nil {
		requestPath = c.Request.URL.Path
	}
	platform := resolveOpsPlatform(cyberPolicyRequestContext(c), apiKey, guessPlatformFromPath(requestPath))
	var clientRequestID, userAgent, clientIPStr string
	if c.Request != nil {
		clientRequestID, _ = c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		userAgent = c.GetHeader("User-Agent")
		clientIPStr = strings.TrimSpace(ip.GetClientIP(c))
	}
	// Snapshot request-scoped values before the asynchronous recorder starts.
	sessionID := service.ExtractClientSessionID(c)
	nativeCompactionV2 := service.IsOpenAINativeCompactionV2(c)
	apiKeyPrefix := ""
	if apiKey != nil {
		apiKeyPrefix = keyPrefix(apiKey.Key, 8)
	}
	opsMeta := cyberPolicyOpsErrorMeta{
		RequestID:       requestID,
		ClientRequestID: clientRequestID,
		Platform:        platform,
		Model:           model,
		RequestPath:     requestPath,
		Stream:          stream,
		InboundEndpoint: inboundEndpoint,
		UserAgent:       userAgent,
		APIKeyPrefix:    apiKeyPrefix,
		UserID:          userID,
		APIKeyID:        apiKeyID,
		AccountID:       accountID,
		GroupID:         groupID,
		ClientIP:        clientIPStr,
		CreatedAt:       time.Now(),
	}
	if gwSvc != nil && apiKey != nil {
		plan := buildCyberSessionBlockWritePlan(apiKey.ID, c, cyberBlockBody)
		if len(plan.keys) > 0 {
			blockCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			gwSvc.MarkCyberSessionBlocked(blockCtx, plan.scopeKey, plan.keys)
			cancel()
		}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cmSvc != nil {
			cmSvc.RecordCyberPolicyEvent(ctx, service.CyberPolicyRecordInput{
				RequestID:       requestID,
				UserID:          userID,
				UserEmail:       userEmail,
				APIKeyID:        apiKeyID,
				APIKeyName:      apiKeyName,
				GroupID:         groupID,
				GroupName:       groupName,
				Endpoint:        inboundEndpoint,
				Model:           model,
				UpstreamMessage: mark.Message,
				UpstreamBody:    mark.Body,
				UpstreamStatus:  mark.UpstreamStatus,
				UpstreamInTok:   mark.UpstreamInTok,
				UpstreamOutTok:  mark.UpstreamOutTok,
			})
		}
		if forwardErrored && gwSvc != nil {
			gwSvc.RecordCyberPolicyUsageLog(ctx, service.CyberPolicyUsageInput{
				APIKey:             apiKey,
				Account:            account,
				Subscription:       subscription,
				RequestID:          requestID,
				Model:              model,
				Stream:             stream,
				InputTokens:        mark.UpstreamInTok,
				OutputTokens:       mark.UpstreamOutTok,
				InboundEndpoint:    inboundEndpoint,
				UpstreamEndpoint:   upstreamEndpoint,
				UserAgent:          userAgent,
				IPAddress:          clientIPStr,
				SessionID:          sessionID,
				RequestPayloadHash: requestPayloadHash,
				APIKeyService:      apiKeySvc,
				NativeCompactionV2: nativeCompactionV2,
				ChannelUsageFields: channelFields,
			})
		}
		if opsSvc != nil {
			enqueueOpsErrorLog(opsSvc, buildCyberPolicyOpsErrorEntry(opsMeta, mark))
		}
	}()
}

// clearCyberPolicyTurnState resets the cyber mark and the per-request recorded
// guard. WS-only: called at the END of AfterTurn, after recordCyberPolicyIfMarked
// and RecordUsage (which reads CyberBlocked) have both consumed the mark.
func clearCyberPolicyTurnState(c *gin.Context) {
	if c == nil {
		return
	}
	service.ClearOpsCyberPolicy(c)
	c.Set(cyberPolicyRecordedKey, false)
}

func cyberPolicyRequestContext(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		return c.Request.Context()
	}
	return context.Background()
}
func summarizeWSCloseErrorForLog(err error) (string, string) {
	if err == nil {
		return "-", "-"
	}
	statusCode := coderws.CloseStatus(err)
	if statusCode == -1 {
		return "-", "-"
	}
	closeStatus := fmt.Sprintf("%d(%s)", int(statusCode), statusCode.String())
	closeReason := "-"
	var closeErr coderws.CloseError
	if errors.As(err, &closeErr) {
		reason := strings.TrimSpace(closeErr.Reason)
		if reason != "" {
			closeReason = reason
		}
	}
	return closeStatus, closeReason
}
