package service

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
)

// codex round40 fu58 (2026-05-20) / upstream PR #2581 intent:
// hard cap on the upstream request body captured for ops retry replay.
// Bodies above this size are NOT stored verbatim in gin context or
// ops_error_logs — they are replaced with a JSON marker envelope
// containing only size + sha256_short + base64-preview head/tail.
//
// Why a cap is required:
//
//	typical request:        <  100 KiB
//	large-context request:  ~  800 KiB - 2 MiB
//	pathological example in codex round40: 39 MiB
//
// At 39 MiB, full-body capture inflates gin context retention + DB
// columns + retry replay buffers — measurable memory pressure under
// concurrency, and the DB row is essentially unreadable anyway.
//
// 4 MiB leaves comfortable headroom for legitimate large-context
// requests; anything beyond is treated as "ops can identify by hash;
// retry from the client's original body, not from our captured copy".
const (
	opsUpstreamRequestBodyCapBytes     = 4 * 1024 * 1024
	opsUpstreamRequestBodyPreviewBytes = 8 * 1024
)

// Gin context keys used by Ops error logger for capturing upstream error details.
// These keys are set by gateway services and consumed by handler/ops_error_logger.go.
const (
	OpsUpstreamStatusCodeKey   = "ops_upstream_status_code"
	OpsUpstreamErrorMessageKey = "ops_upstream_error_message"
	OpsUpstreamErrorDetailKey  = "ops_upstream_error_detail"
	OpsUpstreamErrorsKey       = "ops_upstream_errors"
	OpsUpstreamModelKey        = "ops_upstream_model"

	// Best-effort capture of the current upstream request body so ops can
	// retry the specific upstream attempt (not just the client request).
	// This value is sanitized+trimmed before being persisted.
	OpsUpstreamRequestBodyKey = "ops_upstream_request_body"

	// Optional stage latencies (milliseconds) for troubleshooting and alerting.
	OpsAuthLatencyMsKey      = "ops_auth_latency_ms"
	OpsRoutingLatencyMsKey   = "ops_routing_latency_ms"
	OpsUpstreamLatencyMsKey  = "ops_upstream_latency_ms"
	OpsResponseLatencyMsKey  = "ops_response_latency_ms"
	OpsTimeToFirstTokenMsKey = "ops_time_to_first_token_ms"
	// OpenAI WS 关键观测字段
	OpsOpenAIWSQueueWaitMsKey = "ops_openai_ws_queue_wait_ms"
	OpsOpenAIWSConnPickMsKey  = "ops_openai_ws_conn_pick_ms"
	OpsOpenAIWSConnReusedKey  = "ops_openai_ws_conn_reused"
	OpsOpenAIWSConnIDKey      = "ops_openai_ws_conn_id"

	// OpsSkipPassthroughKey 由 applyErrorPassthroughRule 在命中 skip_monitoring=true 的规则时设置。
	// ops_error_logger 中间件检查此 key，为 true 时跳过错误记录。
	OpsSkipPassthroughKey = "ops_skip_passthrough"

	// TrafficCaptureUpstreamRequestBodyKey is independent from
	// OpsUpstreamRequestBodyKey. Ops keeps a 4 MiB cap for retry/debug safety,
	// while backup-only traffic_capture may intentionally retain the full
	// upstream body when SUB2API_TRAFFIC_CAPTURE_ENABLED is on.
	TrafficCaptureUpstreamRequestBodyKey = "traffic_capture_upstream_request_body"

	// OpsStreamErrorKey 保存 handleStreamingAwareError 在「响应已固化为 HTTP 200 的 SSE 流」
	// 上就地(in-band)补发错误帧时记录的 OpsStreamError。因为 wire 状态码停留在 200，
	// ops_error_logger 的 status>=400 采集路径永远不会触发，这类流内失败
	//（例如等待并发槽位超时后回退的限流、Wait 后二次计费校验失败）本会在错误看板里隐形。
	OpsStreamErrorKey  = "ops_stream_error"
	OpsStreamErrorsKey = "ops_stream_errors"
	OpsStreamTurnKey   = "ops_stream_turn"

	// Client-side configuration denials should remain visible in ops_error_logs,
	// but should be excluded from SLA/error-rate calculations.
	// ResponseCommittedKey 由 handleErrorResponse 系列函数在写完 HTTP 错误响应后设置。
	// ensureForwardErrorResponse 检查此 key，为 true 时跳过兜底写入，避免在已完成的 JSON 后追加 SSE。
	ResponseCommittedKey = "response_committed"

	OpsClientBusinessLimitedKey                           = "ops_client_business_limited"
	OpsClientBusinessLimitedReasonKey                     = "ops_client_business_limited_reason"
	OpsClientBusinessLimitedReasonIPRestriction           = "api_key_ip_restriction"
	OpsClientBusinessLimitedReasonAPIKeyGroupUnavailable  = "api_key_group_unavailable"
	OpsClientBusinessLimitedReasonAPIKeyGroupUnassigned   = "api_key_group_unassigned"
	OpsClientBusinessLimitedReasonLocalFeatureGate        = "local_feature_gate"
	OpsClientBusinessLimitedReasonLocalPolicyDenied       = "local_policy_denied"
	OpsClientBusinessLimitedReasonLocalModelConfiguration = "local_model_configuration"
)

func setOpsUpstreamRequestBody(c *gin.Context, body []byte) {
	if c == nil || len(body) == 0 {
		return
	}
	if capBytes := trafficCaptureFullBodyCapBytes(); capBytes > 0 {
		if len(body) <= capBytes {
			c.Set(TrafficCaptureUpstreamRequestBodyKey, body)
		} else {
			c.Set(TrafficCaptureUpstreamRequestBodyKey, buildTrafficCaptureBodyTruncationMarker(body, capBytes))
		}
	}
	if len(body) <= opsUpstreamRequestBodyCapBytes {
		// 热路径避免 string(body) 额外分配，按需在落库前再转换。
		c.Set(OpsUpstreamRequestBodyKey, body)
		return
	}
	// codex round40 fu58: body exceeds the cap. Replace with a marker
	// envelope so we still record enough for ops triage (size + hash +
	// preview) without retaining 4MB+ of bytes in the gin context. The
	// envelope is a valid JSON string; existing readers in
	// appendOpsUpstreamError hit the `string` branch of the type switch.
	c.Set(OpsUpstreamRequestBodyKey, buildOpsUpstreamRequestBodyTruncationMarker(body))
}

func trafficCaptureFullBodyCapBytes() int {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SUB2API_TRAFFIC_CAPTURE_ENABLED")))
	if v != "1" && v != "true" && v != "yes" && v != "on" {
		return 0
	}
	capBytes := 256 * 1024
	if raw := strings.TrimSpace(os.Getenv("SUB2API_TRAFFIC_CAPTURE_MAX_BYTES")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			capBytes = n
		}
	}
	return capBytes
}

func buildTrafficCaptureBodyTruncationMarker(body []byte, capBytes int) string {
	marker := buildOpsUpstreamRequestBodyTruncationMarker(body)
	if capBytes > 0 {
		var envelope map[string]any
		if err := json.Unmarshal([]byte(marker), &envelope); err == nil {
			envelope["_capture_cap_bytes"] = capBytes
			if b, err := json.Marshal(envelope); err == nil {
				return string(b)
			}
		}
	}
	return marker
}

// buildOpsUpstreamRequestBodyTruncationMarker returns a JSON envelope
// describing an over-cap body. Format:
//
//	{
//	  "_truncated": true,
//	  "_size_bytes": <int>,
//	  "_sha256_short": "<16 hex chars>",
//	  "_preview_head_b64": "<base64 of body[:N]>",
//	  "_preview_tail_b64": "<base64 of body[len-N:]>"   (omitted on overlap)
//	}
//
// All field names start with `_` so ops tools that scan upstream
// request bodies for protocol fields (model, messages, etc.) clearly
// see this is metadata, not a real request body. The body content is
// base64-encoded so binary payloads are safely embedded without
// breaking the surrounding JSON.
func buildOpsUpstreamRequestBodyTruncationMarker(body []byte) string {
	hash := sha256.Sum256(body)
	previewEnd := opsUpstreamRequestBodyPreviewBytes
	if previewEnd > len(body) {
		previewEnd = len(body)
	}
	tailStart := len(body) - opsUpstreamRequestBodyPreviewBytes
	if tailStart < 0 {
		tailStart = 0
	}
	envelope := struct {
		Truncated   bool   `json:"_truncated"`
		SizeBytes   int    `json:"_size_bytes"`
		Sha256Short string `json:"_sha256_short"`
		PreviewHead string `json:"_preview_head_b64"`
		PreviewTail string `json:"_preview_tail_b64,omitempty"`
	}{
		Truncated:   true,
		SizeBytes:   len(body),
		Sha256Short: hex.EncodeToString(hash[:])[:16],
		PreviewHead: base64.StdEncoding.EncodeToString(body[:previewEnd]),
	}
	if tailStart > previewEnd {
		envelope.PreviewTail = base64.StdEncoding.EncodeToString(body[tailStart:])
	}
	out, _ := json.Marshal(envelope)
	return string(out)
}

// 2026-05-12 R29 traffic_capture 桥 — service 包不能 import handler 包 (circular),
// 复用 gin context key 间接传递. middleware 拿到这些 key 时按 handler 包定义的
// key 名读取. 这里硬编码 string 跟 handler/traffic_capture_middleware.go 保持一致.
const (
	trafficCaptureAccountIDKey       = "traffic_capture_account_id"
	trafficCaptureGroupIDKey         = "traffic_capture_group_id"
	trafficCapturePlatformKey        = "traffic_capture_platform"
	trafficCaptureAccountTypeKey     = "traffic_capture_account_type"
	trafficCaptureModelKey           = "traffic_capture_model"
	trafficCaptureUpstreamReqIDKey   = "traffic_capture_upstream_req_id"
	trafficCaptureOutboundHeadersKey = "traffic_capture_outbound_headers"
)

// setTrafficCaptureAccountContext — P2 fix. 选完 account 后 stash 元数据.
// account 类型 *Account (sub2api 内部 ent-wrapped). reqModel 是映射后的 model.
func setTrafficCaptureAccountContext(c *gin.Context, account *Account, reqModel string) {
	if c == nil || account == nil {
		return
	}
	if account.ID > 0 {
		c.Set(trafficCaptureAccountIDKey, account.ID)
	}
	// Account 是多 group 绑定的, 取第一个非零 group_id (cctest 一般一个 group)
	if len(account.GroupIDs) > 0 {
		for _, gid := range account.GroupIDs {
			if gid > 0 {
				c.Set(trafficCaptureGroupIDKey, gid)
				break
			}
		}
	}
	if account.Platform != "" {
		c.Set(trafficCapturePlatformKey, account.Platform)
	}
	if account.Type != "" {
		c.Set(trafficCaptureAccountTypeKey, account.Type)
	}
	if strings.TrimSpace(reqModel) != "" {
		c.Set(trafficCaptureModelKey, reqModel)
	}
}

// setTrafficCaptureUpstreamRequestID — P3 fix. 上游响应回来后调.
func setTrafficCaptureUpstreamRequestID(c *gin.Context, id string) {
	if c == nil || strings.TrimSpace(id) == "" {
		return
	}
	c.Set(trafficCaptureUpstreamReqIDKey, id)
}

// setTrafficCaptureOutboundHeaders — P4 fix. buildUpstreamRequest 后调,
// stash sub2api → upstream 真实 header (含 Authorization / OAuth path 等改写后值).
// 落库时由 service 层 redactSensitiveHeaders 脱敏.
func setTrafficCaptureOutboundHeaders(c *gin.Context, h http.Header) {
	if c == nil || len(h) == 0 {
		return
	}
	flat := make(map[string]string, len(h))
	for k, vs := range h {
		if len(vs) > 0 {
			flat[k] = vs[0]
		}
	}
	c.Set(trafficCaptureOutboundHeadersKey, flat)
}

func MarkResponseCommitted(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(ResponseCommittedKey, true)
}

func IsResponseCommitted(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, ok := c.Get(ResponseCommittedKey)
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

func SetOpsLatencyMs(c *gin.Context, key string, value int64) {
	if c == nil || strings.TrimSpace(key) == "" || value < 0 {
		return
	}
	c.Set(key, value)
}

// SetOpsUpstreamModel stores only the effective model slug for final Ops
// attribution. Call it immediately before an upstream attempt is dispatched.
func SetOpsUpstreamModel(c *gin.Context, model string) {
	if c == nil {
		return
	}
	if model = strings.TrimSpace(model); model != "" {
		c.Set(OpsUpstreamModelKey, model)
	}
}

// ClearOpsUpstreamModel invalidates attempt-scoped model attribution before a
// newly selected account starts credential resolution or upstream dispatch.
func ClearOpsUpstreamModel(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(OpsUpstreamModelKey, "")
}

func MarkOpsClientBusinessLimited(c *gin.Context, reason string) {
	if c == nil {
		return
	}
	c.Set(OpsClientBusinessLimitedKey, true)
	if reason = strings.TrimSpace(reason); reason != "" {
		c.Set(OpsClientBusinessLimitedReasonKey, reason)
	}
}

func HasOpsClientBusinessLimited(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, ok := c.Get(OpsClientBusinessLimitedKey)
	if !ok {
		return false
	}
	marked, _ := v.(bool)
	return marked
}

func OpsClientBusinessLimitedReason(c *gin.Context) string {
	if c == nil {
		return ""
	}
	v, ok := c.Get(OpsClientBusinessLimitedReasonKey)
	if !ok {
		return ""
	}
	reason, _ := v.(string)
	return strings.TrimSpace(reason)
}

// OpsStreamError 描述网关在「响应状态已固化为 200」之后（keepalive ping 或部分数据
// 已 flush）就地以 SSE error 帧形式返回的错误。由于 HTTP 状态码停留在 200，
// 而 ops_error_logger 以 status>=400 为采集触发条件，这类流内失败
// （并发限流回退、Wait 后二次计费校验失败、流开始后才无可用账号等）本会在错误看板里
// 完全隐形。handler.handleStreamingAwareError 负责标记，ops_error_logger 中间件在
// status<400 分支消费它并补记一条错误日志。
type OpsStreamError struct {
	// ErrType 是写入 SSE 帧的对客错误类型（如 rate_limit_error / upstream_error / api_error）。
	ErrType string
	// Code 是可选的稳定错误分类；用于既保留通用 OpenAI error.type，又向客户端和 Ops
	// 暴露可编程判断的细分类（如 upstream_http2_stream_error）。
	Code string
	// Message 是写入 SSE 帧的对客错误消息。
	Message string
	// IntendedStatus 是流若未固化本应返回的 HTTP 状态码（如并发限流的 429）。
	// 默认仅用于错误分级；CountTowardsSLA=true 时也作为 Ops 的逻辑状态码。
	IntendedStatus int
	// CountTowardsSLA 表示虽然 wire 状态已固化为 200，请求在应用语义上仍然失败，
	// Ops 应使用 IntendedStatus 计入错误率/SLA。
	CountTowardsSLA bool
	// Turn identifies a WebSocket turn. HTTP/SSE requests leave it at zero.
	Turn            int
	SkipMonitoring  bool
	AccountID       int64
	UpstreamModel   string
	UpstreamStatus  int
	UpstreamMessage string
	UpstreamDetail  string
	UpstreamErrors  []*OpsUpstreamErrorEvent
}

const maxOpsStreamErrorsPerRequest = 64

// BeginOpsStreamTurn scopes first-wins deduplication to one WebSocket turn.
func BeginOpsStreamTurn(c *gin.Context, turn int) {
	if c == nil || turn <= 0 {
		return
	}
	c.Set(OpsStreamTurnKey, turn)
	// Rule and attempt state is turn-scoped on a long-lived WS connection.
	c.Set(OpsSkipPassthroughKey, false)
	c.Set(OpsUpstreamErrorsKey, []*OpsUpstreamErrorEvent{})
	c.Set(OpsUpstreamStatusCodeKey, 0)
	c.Set(OpsUpstreamErrorMessageKey, "")
	c.Set(OpsUpstreamErrorDetailKey, "")
}

// MarkOpsStreamError 记录一次就地 SSE 错误，供 ops 日志采集。
// 采用「首个标记生效」策略：同一请求若先后补发多帧（如上游透传错误后又追加通用兜底帧），
// 保留最先记录的根因错误，而不是被后续的 "Upstream request failed" 覆盖。
func MarkOpsStreamError(c *gin.Context, errType, message string, intendedStatus int) {
	markOpsStreamError(c, OpsStreamError{
		ErrType:        strings.TrimSpace(errType),
		Message:        strings.TrimSpace(message),
		IntendedStatus: intendedStatus,
	})
}

// MarkOpsStreamFailure records an in-band stream error that represents a
// failed request and therefore counts toward Ops error rate/SLA even when the
// HTTP wire status was already committed as 200.
func MarkOpsStreamFailure(c *gin.Context, errType, code, message string, intendedStatus int) {
	markOpsStreamError(c, OpsStreamError{
		ErrType:         errType,
		Code:            code,
		Message:         message,
		IntendedStatus:  intendedStatus,
		CountTowardsSLA: true,
	})
}

func markOpsStreamError(c *gin.Context, streamErr OpsStreamError) {
	if c == nil {
		return
	}
	streamErr.ErrType = strings.TrimSpace(streamErr.ErrType)
	streamErr.Code = strings.TrimSpace(streamErr.Code)
	streamErr.Message = strings.TrimSpace(streamErr.Message)
	streamErr.SkipMonitoring = currentOpsFailureSkipMonitoring(c)
	snapshotOpsStreamErrorContext(c, &streamErr)
	if GetOpenAIClientTransport(c) == OpenAIClientTransportWS {
		if value, ok := c.Get(OpsStreamTurnKey); ok {
			streamErr.Turn, _ = value.(int)
		}
		var errorsForRequest []OpsStreamError
		if value, ok := c.Get(OpsStreamErrorsKey); ok {
			errorsForRequest, _ = value.([]OpsStreamError)
		}
		if len(errorsForRequest) > 0 && errorsForRequest[len(errorsForRequest)-1].Turn == streamErr.Turn {
			return
		}
		errorsForRequest = append(errorsForRequest, streamErr)
		if len(errorsForRequest) > maxOpsStreamErrorsPerRequest {
			errorsForRequest = append([]OpsStreamError(nil), errorsForRequest[len(errorsForRequest)-maxOpsStreamErrorsPerRequest:]...)
		}
		c.Set(OpsStreamErrorsKey, errorsForRequest)
		c.Set(OpsStreamErrorKey, streamErr)
		return
	}
	if _, exists := c.Get(OpsStreamErrorKey); exists {
		return
	}
	c.Set(OpsStreamErrorKey, streamErr)
}

func snapshotOpsStreamErrorContext(c *gin.Context, streamErr *OpsStreamError) {
	if c == nil || streamErr == nil {
		return
	}
	if c.Request != nil {
		if accountID, ok := c.Request.Context().Value(ctxkey.AccountID).(int64); ok && accountID > 0 {
			streamErr.AccountID = accountID
		}
	}
	if value, ok := c.Get(OpsUpstreamModelKey); ok {
		streamErr.UpstreamModel, _ = value.(string)
		streamErr.UpstreamModel = strings.TrimSpace(streamErr.UpstreamModel)
	}
	if value, ok := c.Get(OpsUpstreamStatusCodeKey); ok {
		switch status := value.(type) {
		case int:
			streamErr.UpstreamStatus = status
		case int64:
			streamErr.UpstreamStatus = int(status)
		}
	}
	if value, ok := c.Get(OpsUpstreamErrorMessageKey); ok {
		streamErr.UpstreamMessage, _ = value.(string)
		streamErr.UpstreamMessage = strings.TrimSpace(streamErr.UpstreamMessage)
	}
	if value, ok := c.Get(OpsUpstreamErrorDetailKey); ok {
		streamErr.UpstreamDetail, _ = value.(string)
		streamErr.UpstreamDetail = strings.TrimSpace(streamErr.UpstreamDetail)
	}
	if value, ok := c.Get(OpsUpstreamErrorsKey); ok {
		if events, ok := value.([]*OpsUpstreamErrorEvent); ok {
			streamErr.UpstreamErrors = make([]*OpsUpstreamErrorEvent, 0, len(events))
			for _, event := range events {
				if event == nil {
					streamErr.UpstreamErrors = append(streamErr.UpstreamErrors, nil)
					continue
				}
				copyOfEvent := *event
				streamErr.UpstreamErrors = append(streamErr.UpstreamErrors, &copyOfEvent)
			}
		}
	}
}

func currentOpsFailureSkipMonitoring(c *gin.Context) bool {
	if c == nil {
		return false
	}
	if value, ok := c.Get(OpsSkipPassthroughKey); ok {
		if skip, _ := value.(bool); skip {
			return true
		}
	}
	if value, ok := c.Get(OpsUpstreamErrorsKey); ok {
		if events, ok := value.([]*OpsUpstreamErrorEvent); ok {
			for i := len(events) - 1; i >= 0; i-- {
				if events[i] != nil {
					return events[i].SkipMonitoring
				}
			}
		}
	}
	return false
}

// GetOpsStreamError 返回本请求记录的就地 SSE 错误（若有）。
func GetOpsStreamError(c *gin.Context) (OpsStreamError, bool) {
	if c == nil {
		return OpsStreamError{}, false
	}
	v, ok := c.Get(OpsStreamErrorKey)
	if !ok {
		return OpsStreamError{}, false
	}
	se, ok := v.(OpsStreamError)
	return se, ok
}

func GetOpsStreamErrors(c *gin.Context) []OpsStreamError {
	if c == nil {
		return nil
	}
	if value, ok := c.Get(OpsStreamErrorsKey); ok {
		if errorsForRequest, ok := value.([]OpsStreamError); ok && len(errorsForRequest) > 0 {
			return append([]OpsStreamError(nil), errorsForRequest...)
		}
	}
	if streamErr, ok := GetOpsStreamError(c); ok {
		return []OpsStreamError{streamErr}
	}
	return nil
}

// SetOpsUpstreamError is the exported wrapper for setOpsUpstreamError, used by
// handler-layer code (e.g. failover-exhausted paths) that needs to record the
// original upstream status code before mapping it to a client-facing code.
func SetOpsUpstreamError(c *gin.Context, upstreamStatusCode int, upstreamMessage, upstreamDetail string) {
	setOpsUpstreamError(c, upstreamStatusCode, upstreamMessage, upstreamDetail)
}

func setOpsUpstreamError(c *gin.Context, upstreamStatusCode int, upstreamMessage, upstreamDetail string) {
	if c == nil {
		return
	}
	if upstreamStatusCode > 0 {
		c.Set(OpsUpstreamStatusCodeKey, upstreamStatusCode)
	}
	if msg := strings.TrimSpace(upstreamMessage); msg != "" {
		c.Set(OpsUpstreamErrorMessageKey, msg)
	}
	if detail := strings.TrimSpace(upstreamDetail); detail != "" {
		c.Set(OpsUpstreamErrorDetailKey, detail)
	}
}

// OpsUpstreamErrorEvent describes one upstream error attempt during a single gateway request.
// It is stored in ops_error_logs.upstream_errors as a JSON array.
type OpsUpstreamErrorEvent struct {
	AtUnixMs int64 `json:"at_unix_ms,omitempty"`

	// Passthrough 表示本次请求是否命中“原样透传（仅替换认证）”分支。
	// 该字段用于排障与灰度评估；存入 JSON，不涉及 DB schema 变更。
	Passthrough bool `json:"passthrough,omitempty"`

	// Context
	Platform    string `json:"platform,omitempty"`
	AccountID   int64  `json:"account_id,omitempty"`
	AccountName string `json:"account_name,omitempty"`

	// Outcome
	UpstreamStatusCode int    `json:"upstream_status_code,omitempty"`
	UpstreamRequestID  string `json:"upstream_request_id,omitempty"`

	// UpstreamURL is the actual upstream URL that was called (host + path, query/fragment stripped).
	// Helps debug 404/routing errors by showing which endpoint was targeted.
	UpstreamURL string `json:"upstream_url,omitempty"`

	// Best-effort upstream request capture (sanitized+trimmed).
	// Required for retrying a specific upstream attempt.
	UpstreamRequestBody string `json:"upstream_request_body,omitempty"`

	// Best-effort upstream response capture (sanitized+trimmed).
	UpstreamResponseBody string `json:"upstream_response_body,omitempty"`

	// Kind: http_error | request_error | retry_exhausted | failover
	Kind   string `json:"kind,omitempty"`
	Stage  string `json:"stage,omitempty"`
	Scope  string `json:"scope,omitempty"`
	Reason string `json:"reason,omitempty"`

	Message string `json:"message,omitempty"`
	Detail  string `json:"detail,omitempty"`

	// SkipMonitoring is request-local rule state. It is intentionally excluded
	// from persisted attempt JSON. The logger consults it only when this event is
	// the final client-visible failure; recovered attempts remain provider-health
	// telemetry and do not count as failed requests.
	SkipMonitoring bool `json:"-"`
}

func appendOpsUpstreamError(c *gin.Context, ev OpsUpstreamErrorEvent) {
	if c == nil {
		return
	}
	if ev.AtUnixMs <= 0 {
		ev.AtUnixMs = time.Now().UnixMilli()
	}
	ev.Platform = strings.TrimSpace(ev.Platform)
	ev.UpstreamRequestID = strings.TrimSpace(ev.UpstreamRequestID)
	ev.UpstreamRequestBody = strings.TrimSpace(ev.UpstreamRequestBody)
	ev.UpstreamResponseBody = strings.TrimSpace(ev.UpstreamResponseBody)
	ev.Kind = strings.TrimSpace(ev.Kind)
	ev.UpstreamURL = strings.TrimSpace(ev.UpstreamURL)
	ev.Message = strings.TrimSpace(ev.Message)
	ev.Detail = strings.TrimSpace(ev.Detail)
	if ev.Message != "" {
		ev.Message = sanitizeUpstreamErrorMessage(ev.Message)
	}

	// If the caller didn't explicitly pass upstream request body but the gateway
	// stored it on the context, attach it so ops can retry this specific attempt.
	if ev.UpstreamRequestBody == "" {
		if v, ok := c.Get(OpsUpstreamRequestBodyKey); ok {
			switch raw := v.(type) {
			case string:
				ev.UpstreamRequestBody = strings.TrimSpace(raw)
			case []byte:
				ev.UpstreamRequestBody = strings.TrimSpace(string(raw))
			}
		}
	}

	var existing []*OpsUpstreamErrorEvent
	if v, ok := c.Get(OpsUpstreamErrorsKey); ok {
		if arr, ok := v.([]*OpsUpstreamErrorEvent); ok {
			existing = arr
		}
	}

	evCopy := ev
	existing = append(existing, &evCopy)
	c.Set(OpsUpstreamErrorsKey, existing)

	checkSkipMonitoringForUpstreamEvent(c, &evCopy)
}

// checkSkipMonitoringForUpstreamEvent snapshots whether this attempt matches a
// skip_monitoring passthrough rule. The final failure decides whether the
// request error is hidden; an intermediate recovered attempt cannot suppress a
// later client-visible failure.
func checkSkipMonitoringForUpstreamEvent(c *gin.Context, ev *OpsUpstreamErrorEvent) {
	if ev.UpstreamStatusCode == 0 {
		return
	}

	svc := getBoundErrorPassthroughService(c)
	if svc == nil {
		return
	}

	// Use the best available body representation for keyword matching.
	// Even when body is empty, MatchRule can still match rules that only
	// specify ErrorCodes (no Keywords), so we always call it.
	body := ev.Detail
	if body == "" {
		body = ev.Message
	}

	rule := svc.MatchRule(ev.Platform, ev.UpstreamStatusCode, []byte(body))
	if rule != nil && rule.SkipMonitoring {
		ev.SkipMonitoring = true
	}
}

func marshalOpsUpstreamErrors(events []*OpsUpstreamErrorEvent) *string {
	if len(events) == 0 {
		return nil
	}
	// Ensure we always store a valid JSON value.
	raw, err := json.Marshal(events)
	if err != nil || len(raw) == 0 {
		return nil
	}
	s := string(raw)
	return &s
}

func ParseOpsUpstreamErrors(raw string) ([]*OpsUpstreamErrorEvent, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []*OpsUpstreamErrorEvent{}, nil
	}
	var out []*OpsUpstreamErrorEvent
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// safeUpstreamURL returns scheme + host + path from a URL, stripping query/fragment
// to avoid leaking sensitive query parameters (e.g. OAuth tokens).
func safeUpstreamURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	if idx := strings.IndexByte(rawURL, '?'); idx >= 0 {
		rawURL = rawURL[:idx]
	}
	if idx := strings.IndexByte(rawURL, '#'); idx >= 0 {
		rawURL = rawURL[:idx]
	}
	return rawURL
}
