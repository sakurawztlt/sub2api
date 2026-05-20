package service

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// codex round40 fu58 (2026-05-20) / upstream PR #2581 intent:
// hard cap on the upstream request body captured for ops retry replay.
// Bodies above this size are NOT stored verbatim in gin context or
// ops_error_logs — they are replaced with a JSON marker envelope
// containing only size + sha256_short + base64-preview head/tail.
//
// Why a cap is required:
//   typical request:        <  100 KiB
//   large-context request:  ~  800 KiB - 2 MiB
//   pathological example in codex round40: 39 MiB
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
)

func setOpsUpstreamRequestBody(c *gin.Context, body []byte) {
	if c == nil || len(body) == 0 {
		return
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

func SetOpsLatencyMs(c *gin.Context, key string, value int64) {
	if c == nil || strings.TrimSpace(key) == "" || value < 0 {
		return
	}
	c.Set(key, value)
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
	Kind string `json:"kind,omitempty"`

	Message string `json:"message,omitempty"`
	Detail  string `json:"detail,omitempty"`
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

// checkSkipMonitoringForUpstreamEvent checks whether the upstream error event
// matches a passthrough rule with skip_monitoring=true and, if so, sets the
// OpsSkipPassthroughKey on the context.  This ensures intermediate retry /
// failover errors (which never go through the final applyErrorPassthroughRule
// path) can still suppress ops_error_logs recording.
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
		c.Set(OpsSkipPassthroughKey, true)
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
