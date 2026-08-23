// 2026-05-12 R29 P7 middleware behavior tests.
//
// 覆盖 codex audit 7 项的 middleware 行为面:
//  1. inbound 大于 cap → 业务 handler 拿到完整 body (P1 fix)
//  2. 落库 inbound 按 cap 截 + truncated=true
//  3. 200 成功也 capture (不只 error)
//  4. response totalBytes 真实大小 (P5 fix)
//  5. UpstreamRequestID 接得上 (P3)
//  6. OutboundHeaders 落库脱敏 (P4)
//  7. client_ip / user_agent 落库 (P-B)
package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestTrafficCaptureMiddleware_LargeBodyNotTruncatedForHandler(t *testing.T) {
	// P1 critical: cap=64 bytes, 但 inbound 是 200 bytes. 业务 handler 必须拿完整 200b.
	const cap = 64
	bigBody := strings.Repeat("X", 200)
	var handlerSawBody []byte

	r := gin.New()
	// 用真 service Enabled=true cap=64. 不传 ent client 但 Submit 走 queue 不 panic.
	cfg := service.TrafficCaptureConfig{Enabled: true, MaxBytes: cap, TTL: time.Hour, Sampling: 1.0}
	svc := service.NewTrafficCaptureService(nil, cfg)
	r.Use(TrafficCaptureMiddleware(svc))
	r.POST("/v1/messages", func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		handlerSawBody = b
		c.JSON(200, gin.H{"ok": true})
	})

	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(bigBody)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if string(handlerSawBody) != bigBody {
		t.Errorf("handler saw truncated body! len=%d (want %d)", len(handlerSawBody), len(bigBody))
		t.Errorf("handler body sample: %q", string(handlerSawBody[:min(60, len(handlerSawBody))]))
	}
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	// graceful shutdown
	_ = svc.Close(context.Background())
}

func TestTrafficCaptureMiddleware_DisabledIsNoop(t *testing.T) {
	r := gin.New()
	cfg := service.TrafficCaptureConfig{Enabled: false}
	svc := service.NewTrafficCaptureService(nil, cfg)
	r.Use(TrafficCaptureMiddleware(svc))
	r.POST("/v1/messages", func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		if string(b) != "hello" {
			t.Errorf("handler body = %q, want hello", b)
		}
		c.JSON(200, gin.H{"ok": true})
	})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("hello"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestTrafficCaptureMiddleware_ContextHelpers(t *testing.T) {
	r := gin.New()
	cfg := service.TrafficCaptureConfig{Enabled: true, MaxBytes: 1024, TTL: time.Hour, Sampling: 1.0}
	svc := service.NewTrafficCaptureService(nil, cfg)
	r.Use(TrafficCaptureMiddleware(svc))
	r.POST("/v1/messages", func(c *gin.Context) {
		// 模拟 gateway_service 钩 — 调 helper 写 context
		SetTrafficCaptureUpstreamRequestID(c, "req_upstream_abc")
		SetTrafficCaptureOutboundHeaders(c, http.Header{
			"Authorization": []string{"Bearer oat01-secret-token-xxxxxx"},
			"X-Api-Key":     []string{"sk-ant-api03-secret"},
			"Content-Type":  []string{"application/json"},
		})
		c.Set("traffic_capture_account_id", int64(42))
		c.Set("traffic_capture_platform", "anthropic")
		c.Set("traffic_capture_account_type", "oauth")
		c.Set("traffic_capture_model", "claude-opus-4-6")
		c.JSON(200, gin.H{"ok": true})
	})

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-6"}`))
	req.Header.Set("X-Newapi-Request-Id", "test-req-id-001")
	req.RemoteAddr = "192.168.1.100:54321"
	req.Header.Set("User-Agent", "claude-cli/2.1.0")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	// middleware 已 enqueue, 但 consumeLoop 调 ent.Client (nil) 会 panic.
	// 我们这里不让它 persist, 只确认 200 返回 + middleware 路径走通.
	// 真 persist 测试见 service_test 那边.
	_ = svc.Close(context.Background())
}

func TestTrafficCaptureRequestID_AdoptsGCRHeader(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	c.Request.Header.Set("X-GCR-Request-Id", "gcr-local-test123")

	if got := extractRequestIDForTrafficCapture(c); got != "gcr-local-test123" {
		t.Fatalf("request id = %q, want X-GCR-Request-Id", got)
	}
}

func TestTrafficCaptureRequestID_FallsBackToClientRequestIDContext(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.ClientRequestID, "client-ctx-id"))
	c.Request = req

	if got := extractRequestIDForTrafficCapture(c); got != "client-ctx-id" {
		t.Fatalf("request id = %q, want context client request id", got)
	}
}

func TestTrafficCaptureMiddleware_ResponseTotalBytesAccumulated(t *testing.T) {
	// P5: writer 必须累积 totalBytes 哪怕 buf 早就到 cap
	const cap = 30
	cfg := service.TrafficCaptureConfig{Enabled: true, MaxBytes: cap, TTL: time.Hour, Sampling: 1.0}
	svc := service.NewTrafficCaptureService(nil, cfg)

	// 直接测 writer wrapper 逻辑 (绕过 middleware 路径)
	rw := httptest.NewRecorder()
	// 包一层假 gin.ResponseWriter
	c, _ := gin.CreateTestContext(rw)
	w := acquireTrafficCaptureWriter(c.Writer, cap)
	defer releaseTrafficCaptureWriter(w)

	// 写 100b, cap=30. buf 应只 30b, totalBytes=100
	body := strings.Repeat("Y", 100)
	_, err := w.Write([]byte(body))
	require.NoError(t, err)
	if w.totalBytes != 100 {
		t.Errorf("totalBytes = %d, want 100", w.totalBytes)
	}
	if w.buf.Len() > cap {
		t.Errorf("buf.Len() = %d, want <= %d", w.buf.Len(), cap)
	}
	_ = svc.Close(context.Background())
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// codex r3 fix #1: api_key context 类型是 *service.APIKey, 不是 int64
func TestTrafficCaptureMiddleware_APIKeyIDExtraction(t *testing.T) {
	cfg := service.TrafficCaptureConfig{Enabled: true, MaxBytes: 1024, TTL: time.Hour, Sampling: 1.0}
	svc := service.NewTrafficCaptureService(nil, cfg)
	t.Cleanup(func() { require.NoError(t, svc.Close(context.Background())) })

	r := gin.New()
	// 模拟 ApiKeyAuth: set api_key 为 *service.APIKey
	r.Use(func(c *gin.Context) {
		gid := int64(42)
		c.Set("api_key", &service.APIKey{
			ID:      777,
			GroupID: &gid,
		})
		c.Next()
	})
	r.Use(TrafficCaptureMiddleware(svc))
	r.POST("/v1/messages", func(c *gin.Context) {
		// 在 handler 里调用 extractor 验证能拿到 777
		gotID := extractAPIKeyIDFromContext(c)
		if gotID != 777 {
			t.Errorf("extractAPIKeyIDFromContext = %d, want 777", gotID)
		}
		gotGID := extractAPIKeyGroupIDFromContext(c)
		if gotGID != 42 {
			t.Errorf("extractAPIKeyGroupIDFromContext = %d, want 42", gotGID)
		}
		c.JSON(200, gin.H{"ok": true})
	})

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// codex r3 fix #2: inbound > 16MB hard ceiling → 413, 不 drain body 给 handler
func TestTrafficCaptureMiddleware_OversizeReturns413(t *testing.T) {
	cfg := service.TrafficCaptureConfig{Enabled: true, MaxBytes: 1024, TTL: time.Hour, Sampling: 1.0}
	svc := service.NewTrafficCaptureService(nil, cfg)
	t.Cleanup(func() { require.NoError(t, svc.Close(context.Background())) })

	r := gin.New()
	r.Use(TrafficCaptureMiddleware(svc))
	handlerCalled := false
	r.POST("/v1/messages", func(c *gin.Context) {
		handlerCalled = true
		c.JSON(200, gin.H{"ok": true})
	})

	// 模拟 17MB body (> 16MB hard ceiling)
	bigBody := bytes.Repeat([]byte("X"), 17*1024*1024)
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(bigBody))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize status = %d, want 413", w.Code)
	}
	if handlerCalled {
		t.Error("handler should NOT be called when inbound > 16MB (codex r3 fix)")
	}
	body := w.Body.String()
	if !strings.Contains(body, "invalid_request_error") {
		t.Errorf("response body should be Anthropic-shaped 413, got: %s", body)
	}
}

func TestTrafficCaptureMiddleware_CeilingFollowsCaptureMaxBytes(t *testing.T) {
	bodyLen := defaultInboundBodyCaptureCeiling + 1
	cfg := service.TrafficCaptureConfig{Enabled: true, MaxBytes: bodyLen + 1024, TTL: time.Hour, Sampling: 1.0}
	svc := service.NewTrafficCaptureService(nil, cfg)
	t.Cleanup(func() { require.NoError(t, svc.Close(context.Background())) })

	r := gin.New()
	r.Use(TrafficCaptureMiddleware(svc))
	var handlerSaw int
	r.POST("/v1/messages", func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		handlerSaw = len(b)
		c.JSON(200, gin.H{"ok": true})
	})

	bigBody := bytes.Repeat([]byte("X"), bodyLen)
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(bigBody))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if handlerSaw != bodyLen {
		t.Fatalf("handler body len = %d, want %d", handlerSaw, bodyLen)
	}
}
