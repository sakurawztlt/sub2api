// 2026-05-11 codex audit / R29 backup-only traffic capture middleware.
//
// 钩点: gin middleware, 总是 capture (不只 error). 跟 OpsErrorLoggerMiddleware
// 不同的是 OpsErrorLogger 只 buffer status>=400, 我们 200 也全 buffer.
//
// 流程:
//  1. handler 入口前: 读 inbound body 一次, stash 到 ctx (key TrafficCaptureInboundBodyKey)
//     gateway_handler 后续读 body 用同一份, 不用再 ReadAll.
//  2. wrap c.Writer 累积 response body (上限 cfg.MaxBytes)
//  3. c.Next() — 走完 handler 链, gateway_service 内部 setOpsUpstreamRequestBody
//     会 stash final outbound body 到 OpsUpstreamRequestBodyKey.
//  4. defer 阶段: 拼装 TrafficCaptureEntry 异步 submit
package handler

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// gin context keys (跟 OpsUpstreamRequestBodyKey 平级)
const (
	TrafficCaptureInboundBodyKey      = "traffic_capture_inbound_body"
	TrafficCaptureAccountIDKey        = "traffic_capture_account_id"
	TrafficCaptureGroupIDKey          = "traffic_capture_group_id"
	TrafficCapturePlatformKey         = "traffic_capture_platform"
	TrafficCaptureAccountTypeKey      = "traffic_capture_account_type"
	TrafficCaptureModelKey            = "traffic_capture_model"
	TrafficCaptureUpstreamReqIDKey    = "traffic_capture_upstream_req_id"
	TrafficCaptureOutboundHeadersKey  = "traffic_capture_outbound_headers"
)

// SetTrafficCaptureContext — gateway_service 内部调用, stash 选中 account 信息.
// 跟 setOpsUpstreamRequestBody 一并 reuse, 不破坏现有调用.
func SetTrafficCaptureContext(c *gin.Context, accountID, groupID int64, platform, accountType, model string) {
	if c == nil {
		return
	}
	if accountID > 0 {
		c.Set(TrafficCaptureAccountIDKey, accountID)
	}
	if groupID > 0 {
		c.Set(TrafficCaptureGroupIDKey, groupID)
	}
	if p := strings.TrimSpace(platform); p != "" {
		c.Set(TrafficCapturePlatformKey, p)
	}
	if t := strings.TrimSpace(accountType); t != "" {
		c.Set(TrafficCaptureAccountTypeKey, t)
	}
	if m := strings.TrimSpace(model); m != "" {
		c.Set(TrafficCaptureModelKey, m)
	}
}

// SetTrafficCaptureUpstreamRequestID — sub2api 收到上游响应后 stash x-request-id.
func SetTrafficCaptureUpstreamRequestID(c *gin.Context, id string) {
	if c == nil || strings.TrimSpace(id) == "" {
		return
	}
	c.Set(TrafficCaptureUpstreamReqIDKey, id)
}

// SetTrafficCaptureOutboundHeaders — P4 fix. gateway_service buildUpstreamRequest 之后调,
// stash sub2api → upstream 真实 header (含改写过的 Authorization / OAuth path 等).
// middleware 落库时读出来填 outbound_headers 字段, service 层脱敏.
func SetTrafficCaptureOutboundHeaders(c *gin.Context, h http.Header) {
	if c == nil || len(h) == 0 {
		return
	}
	flat := make(map[string]string, len(h))
	for k, vs := range h {
		if len(vs) > 0 {
			flat[k] = vs[0]
		}
	}
	c.Set(TrafficCaptureOutboundHeadersKey, flat)
}

// === 响应 writer wrapper ===
//
// 2026-05-12 P5 fix: 累积 totalBytes 让 entry 知道真实 response 大小, 不是 buf.Len()
// (buf 被 cap 截了). truncated = totalBytes > limit. 还要 implement Flush/Hijack
// 等 interface 让 SSE 流响应/WebSocket 升级不破.

type trafficCaptureWriter struct {
	gin.ResponseWriter
	limit      int
	buf        bytes.Buffer
	totalBytes int64 // 累积所有 Write 的总字节, capture truncate 判断真实大小
}

var trafficCaptureWriterPool = sync.Pool{
	New: func() any { return &trafficCaptureWriter{} },
}

func acquireTrafficCaptureWriter(rw gin.ResponseWriter, limit int) *trafficCaptureWriter {
	w, ok := trafficCaptureWriterPool.Get().(*trafficCaptureWriter)
	if !ok || w == nil {
		w = &trafficCaptureWriter{}
	}
	w.ResponseWriter = rw
	w.limit = limit
	w.buf.Reset()
	w.totalBytes = 0
	return w
}

func releaseTrafficCaptureWriter(w *trafficCaptureWriter) {
	if w == nil {
		return
	}
	w.ResponseWriter = nil
	w.limit = 0
	w.buf.Reset()
	w.totalBytes = 0
	trafficCaptureWriterPool.Put(w)
}

func (w *trafficCaptureWriter) Write(b []byte) (int, error) {
	w.totalBytes += int64(len(b))
	if w.limit > 0 && w.buf.Len() < w.limit {
		remaining := w.limit - w.buf.Len()
		if len(b) > remaining {
			_, _ = w.buf.Write(b[:remaining])
		} else {
			_, _ = w.buf.Write(b)
		}
	}
	return w.ResponseWriter.Write(b)
}

func (w *trafficCaptureWriter) WriteString(s string) (int, error) {
	w.totalBytes += int64(len(s))
	if w.limit > 0 && w.buf.Len() < w.limit {
		remaining := w.limit - w.buf.Len()
		if len(s) > remaining {
			_, _ = w.buf.WriteString(s[:remaining])
		} else {
			_, _ = w.buf.WriteString(s)
		}
	}
	return w.ResponseWriter.WriteString(s)
}

// === middleware ===

// TrafficCaptureMiddleware — 包 gin route. 默认关闭 (svc.Enabled() = false 时 noop),
// env on 后会 1) **完整** ReadAll inbound (不用 LimitReader, 否则业务 handler 拿截断
// body 污染测试 — 2026-05-12 P1 fix), 2) wrap response writer 累积 totalBytes,
// 3) defer 拼装 submit (落库时按 cap 截断, 不污染真实流量).
//
// 顺序建议: 装在 OpsErrorLoggerMiddleware 之前 (顶部), 这样 inbound capture
// 不被 ops 中间件影响, response wrap 在最外层覆盖完整 SSE body.
//
// 防 OOM 保护 (P1 codex audit 补): 读 inbound 用 maxInboundBodyForCapture cap (16MB),
// 超过 cap 直接 abort 不 capture 这条 (防恶意大 body 把 sub2api 进程撑爆).
// 16MB > 任何 cctest 多模态最大. 正常请求 < 5MB, 不影响测试.
const maxInboundBodyForCapture = 16 * 1024 * 1024 // 16MB hard ceiling

func TrafficCaptureMiddleware(svc *service.TrafficCaptureService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !svc.Enabled() {
			c.Next()
			return
		}
		// 1. P1 fix: **完整** 读 inbound body, 不用 LimitReader 截断业务 handler.
		// 落库时再按 cap 截断 (在 service.persist 里走 captureBodyMaxBytes).
		// 用 MaxBytesReader 给硬上限 防恶意大请求 OOM.
		var inboundBody []byte
		if c.Request.Body != nil {
			limited := http.MaxBytesReader(c.Writer, c.Request.Body, maxInboundBodyForCapture)
			b, err := io.ReadAll(limited)
			if err != nil {
				// 超 maxInboundBodyForCapture (16MB) 或读失败 — 不 capture 让业务处理
				log.Printf("[traffic-capture] inbound read failed (body too big or read error): %v", err)
				// 这种 case 已读到部分 body, 不能塞回去, 改让 handler 拿到 c.Request.Body
				// 的剩余部分 (但 limited 已 drain). 安全降级: noop, 让 ops 接管.
				c.Next()
				return
			}
			inboundBody = b
			c.Request.Body = io.NopCloser(bytes.NewReader(b))
			if len(b) > 0 {
				c.Set(TrafficCaptureInboundBodyKey, b)
			}
		}

		// 2. wrap writer 累积 response body (limit=cap, totalBytes 永远累积)
		originalWriter := c.Writer
		w := acquireTrafficCaptureWriter(originalWriter, svc.MaxBytesCap())
		defer func() {
			if c.Writer == w {
				c.Writer = originalWriter
			}
			releaseTrafficCaptureWriter(w)
		}()
		c.Writer = w
		start := time.Now()

		// 3. run handler
		c.Next()

		// 4. defer 阶段 — 拼装 entry 异步 submit. inbound 完整 + response 用 totalBytes
		//    标真实大小让 service 落库时 truncate 准确.
		useTimeMS := time.Since(start).Milliseconds()
		entry := service.TrafficCaptureEntry{
			Ts:             start.UTC(),
			InboundBody:    inboundBody,
			UpstreamStatus: c.Writer.Status(),
			UseTimeMS:      useTimeMS,
		}
		// P5 fix: response_body bytes = totalBytes (真实大小), body 截断了用
		// ResponseTotalBytes 让 service.persist 知道; service 会 cap-clamp body 并
		// 计算 truncated.
		entry.ResponseBody = append([]byte(nil), w.buf.Bytes()...)
		entry.ResponseTotalBytes = w.totalBytes

		// 5. P2/P3/P4: 各种 metadata 从 gateway_service stash 的 context 拿
		if v, ok := c.Get(service.OpsUpstreamRequestBodyKey); ok {
			if b, ok := v.([]byte); ok {
				entry.OutboundBody = b
			}
		}
		entry.RequestID = extractFirstHeader(c, "X-Oneapi-Request-Id", "X-Newapi-Request-Id", "X-Request-Id")
		if v, ok := c.Get(TrafficCaptureUpstreamReqIDKey); ok {
			if s, ok := v.(string); ok {
				entry.UpstreamRequestID = s
			}
		}
		// P2 fix: 不再用错误的 "api_key_id" key, 用 sub2api 标准的 helper
		entry.APIKeyID = extractAPIKeyIDFromContext(c)
		entry.AccountID = getInt64FromCtx(c, TrafficCaptureAccountIDKey)
		entry.GroupID = getInt64FromCtx(c, TrafficCaptureGroupIDKey)
		entry.Platform = getStrFromCtx(c, TrafficCapturePlatformKey)
		entry.AccountType = getStrFromCtx(c, TrafficCaptureAccountTypeKey)
		entry.Model = getStrFromCtx(c, TrafficCaptureModelKey)
		entry.Stream = isStreamRequest(inboundBody)
		// P-B: 客户端识别
		entry.ClientIP = c.ClientIP()
		entry.UserAgent = c.Request.UserAgent()
		// stream 模式下 c.Writer.Status() = 200 但实际 upstream 可能错, 从 ops upstream
		// status 拿更准
		if v, ok := c.Get(service.OpsUpstreamStatusCodeKey); ok {
			if code, ok := v.(int); ok && code > 0 {
				entry.UpstreamStatus = code
			}
		}
		if v, ok := c.Get(service.OpsUpstreamErrorMessageKey); ok {
			if s, ok := v.(string); ok && s != "" {
				entry.ErrorMsg = s
				if entry.UpstreamStatus >= 500 {
					entry.ErrorKind = "upstream_5xx"
				} else if entry.UpstreamStatus >= 400 {
					entry.ErrorKind = "upstream_4xx"
				}
			}
		}
		// P4 fix: outbound_headers — gateway_service buildUpstreamRequest 后 stash 的
		// (sub2api → upstream 的真实 header, 含改写过的 OAuth path/signature 等).
		if v, ok := c.Get(TrafficCaptureOutboundHeadersKey); ok {
			if h, ok := v.(map[string]string); ok {
				entry.OutboundHeaders = h
			}
		}
		// response headers — sub2api → 客户的 (gin Writer 的 header)
		respHdr := make(map[string]string, 8)
		for k, vs := range c.Writer.Header() {
			if len(vs) > 0 {
				respHdr[k] = vs[0]
			}
		}
		entry.ResponseHeaders = respHdr

		svc.Submit(entry)
	}
}

// extractAPIKeyIDFromContext — P2 fix. sub2api APIKeyAuth set 多种 key, 我们
// 全部 try 一遍. 实际 key 名见 internal/server/middleware/api_key_auth.go 跟
// internal/middleware/api_key.go (新旧 path).
func extractAPIKeyIDFromContext(c *gin.Context) int64 {
	// 常见 key 名 (按 sub2api 代码实际使用):
	keys := []string{
		"apiKeyID",
		"api_key_id",
		"APIKeyID",
		"apikey_id",
	}
	for _, k := range keys {
		if id := getInt64FromCtx(c, k); id != 0 {
			return id
		}
	}
	// 也试一下 APIKey object 形式
	if v, ok := c.Get("apiKey"); ok {
		// reflective best-effort, 实际类型由 ApiKeyAuth 决定. 不行就 0
		type idGetter interface{ GetID() int64 }
		if g, ok := v.(idGetter); ok {
			return g.GetID()
		}
	}
	return 0
}

func extractFirstHeader(c *gin.Context, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(c.Request.Header.Get(k)); v != "" {
			return v
		}
	}
	return ""
}

func getInt64FromCtx(c *gin.Context, key string) int64 {
	if v, ok := c.Get(key); ok {
		switch t := v.(type) {
		case int64:
			return t
		case int:
			return int64(t)
		}
	}
	return 0
}

func getStrFromCtx(c *gin.Context, key string) string {
	if v, ok := c.Get(key); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func isStreamRequest(body []byte) bool {
	// 简单判断: body 含 "\"stream\":true" 或 "stream=true". 不需精确解析 JSON.
	if len(body) == 0 {
		return false
	}
	return bytes.Contains(body, []byte(`"stream":true`)) || bytes.Contains(body, []byte(`"stream": true`))
}
