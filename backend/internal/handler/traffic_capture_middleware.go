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
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// gin context keys (跟 OpsUpstreamRequestBodyKey 平级)
const (
	TrafficCaptureInboundBodyKey = "traffic_capture_inbound_body"
	TrafficCaptureAccountIDKey   = "traffic_capture_account_id"
	TrafficCaptureGroupIDKey     = "traffic_capture_group_id"
	TrafficCapturePlatformKey    = "traffic_capture_platform"
	TrafficCaptureAccountTypeKey = "traffic_capture_account_type"
	TrafficCaptureModelKey       = "traffic_capture_model"
	TrafficCaptureUpstreamReqIDKey = "traffic_capture_upstream_req_id"
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

// === 响应 writer wrapper ===

type trafficCaptureWriter struct {
	gin.ResponseWriter
	limit int
	buf   bytes.Buffer
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
	return w
}

func releaseTrafficCaptureWriter(w *trafficCaptureWriter) {
	if w == nil {
		return
	}
	w.ResponseWriter = nil
	w.limit = 0
	w.buf.Reset()
	trafficCaptureWriterPool.Put(w)
}

func (w *trafficCaptureWriter) Write(b []byte) (int, error) {
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
// env on 后会 1) ReadAll inbound + stash 2) wrap response writer 3) defer 拼装 submit.
//
// 顺序建议: 装在 OpsErrorLoggerMiddleware 之前 (顶部), 这样 inbound capture
// 不被 ops 中间件影响, response wrap 在最外层覆盖完整 SSE body.
func TrafficCaptureMiddleware(svc *service.TrafficCaptureService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !svc.Enabled() {
			c.Next()
			return
		}
		// 1. 读 inbound body 一次 + 还原 c.Request.Body 让 handler 继续读
		var inboundBody []byte
		if c.Request.Body != nil {
			b, err := io.ReadAll(io.LimitReader(c.Request.Body, int64(svc.MaxBytesCap())))
			if err == nil && len(b) > 0 {
				inboundBody = b
				c.Request.Body = io.NopCloser(bytes.NewReader(b))
				c.Set(TrafficCaptureInboundBodyKey, b)
			}
		}

		// 2. wrap writer 累积 response body
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

		// 4. defer 阶段 — 拼装 entry 异步 submit
		useTimeMS := time.Since(start).Milliseconds()
		entry := service.TrafficCaptureEntry{
			Ts:             start.UTC(),
			InboundBody:    inboundBody,
			ResponseBody:   append([]byte(nil), w.buf.Bytes()...),
			UpstreamStatus: c.Writer.Status(),
			UseTimeMS:      useTimeMS,
		}
		// 从 gin context 收上 gateway service stash 的字段
		if v, ok := c.Get(service.OpsUpstreamRequestBodyKey); ok {
			if b, ok := v.([]byte); ok {
				entry.OutboundBody = b
			}
		}
		// 5. 各种 metadata
		entry.RequestID = extractFirstHeader(c, "X-Oneapi-Request-Id", "X-Newapi-Request-Id", "X-Request-Id")
		if v, ok := c.Get(TrafficCaptureUpstreamReqIDKey); ok {
			if s, ok := v.(string); ok {
				entry.UpstreamRequestID = s
			}
		}
		entry.APIKeyID = getInt64FromCtx(c, "api_key_id")
		entry.AccountID = getInt64FromCtx(c, TrafficCaptureAccountIDKey)
		entry.GroupID = getInt64FromCtx(c, TrafficCaptureGroupIDKey)
		entry.Platform = getStrFromCtx(c, TrafficCapturePlatformKey)
		entry.AccountType = getStrFromCtx(c, TrafficCaptureAccountTypeKey)
		entry.Model = getStrFromCtx(c, TrafficCaptureModelKey)
		entry.Stream = isStreamRequest(inboundBody)
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
		// headers snapshot — response (上游回的)
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
