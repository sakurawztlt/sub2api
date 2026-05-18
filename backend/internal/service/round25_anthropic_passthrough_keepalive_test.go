package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// codex round25 fu42 / upstream PR #2552 commit 164e2f610 (2026-05-18):
// regression tests for the keepalive ping added to
// handleStreamingResponseAnthropicAPIKeyPassthrough.
//
// Three scenarios pin behavior:
//   (1) Keepalive ping fires when upstream is idle longer than the
//       configured interval (ported from upstream test).
//   (2) Keepalive ping does NOT interleave into a partial SSE frame
//       (ported from upstream test).
//   (3) [local] When the client disconnects DURING the keepalive ping
//       write, the loop continues draining the upstream for usage
//       accounting — it does not short-circuit and lose billing data.

func TestRound25_AnthropicPassthrough_SendsKeepaliveDuringIdle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	svc := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				StreamKeepaliveInterval: 1,
				MaxLineSize:             defaultMaxLineSize,
			},
		},
		rateLimitService: &RateLimitService{},
	}

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Hold upstream silent for > keepalive interval so the loop
		// fires at least one ping before any data arrives.
		time.Sleep(1200 * time.Millisecond)
		_, _ = pw.Write([]byte(strings.Join([]string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			"",
			`data: {"type":"message_delta","usage":{"output_tokens":2}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n")))
		_ = pw.Close()
	}()

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, &Account{ID: 8}, time.Now(), "claude-3-7-sonnet-20250219")
	_ = pr.Close()
	<-done

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "event: ping\ndata: {\"type\": \"ping\"}\n\n",
		"keepalive ping MUST appear in the forwarded stream when upstream is idle longer than the interval")
	require.Contains(t, rec.Body.String(), "data: [DONE]",
		"real upstream data MUST still pass through after the ping")
}

func TestRound25_AnthropicPassthrough_KeepaliveDoesNotInterleavePartialEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	svc := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				StreamKeepaliveInterval: 1,
				MaxLineSize:             defaultMaxLineSize,
			},
		},
		rateLimitService: &RateLimitService{},
	}

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Write a data: line WITHOUT its frame-closing blank line, then
		// stall past the keepalive interval. The ping must NOT be
		// injected between the data: line and its blank-line boundary —
		// the inPartialEvent guard exists precisely for this case.
		_, _ = pw.Write([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":4}}}` + "\n"))
		time.Sleep(1200 * time.Millisecond)
		_, _ = pw.Write([]byte("\n"))
		_, _ = pw.Write([]byte("data: [DONE]\n\n"))
		_ = pw.Close()
	}()

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, &Account{ID: 9}, time.Now(), "claude-3-7-sonnet-20250219")
	_ = pr.Close()
	<-done

	require.NoError(t, err)
	require.NotNil(t, result)
	body := rec.Body.String()
	require.NotContains(t, body, `data: {"type":"message_start","message":{"usage":{"input_tokens":4}}}`+"\n"+"event: ping",
		"ping MUST NOT land between a data: line and its blank-line frame boundary")
	require.NotContains(t, body, "event: ping",
		"no ping should fire while upstream is mid-frame (inPartialEvent guard)")
	require.Contains(t, body, "data: [DONE]")
}

// TestRound25_AnthropicPassthrough_KeepalivePingWriteFailureContinuesDrain —
// codex round25 fu42 local additional test. When fmt.Fprint on the
// keepalive ping fails (client disconnected mid-stream), the loop must
// flip clientDisconnected=true AND continue iterating the events
// channel so upstream usage is still accumulated. Previously the
// passthrough had NO keepalive at all so this case didn't exist; now
// that we have a ping write path, we need to make sure its failure
// path doesn't accidentally short-circuit the billing-protection
// logic the rest of the function relies on.
func TestRound25_AnthropicPassthrough_KeepalivePingWriteFailureContinuesDrain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	// Wrap the recorder so writes after the first one fail (simulates
	// client disconnect during the keepalive ping). The first write is
	// the initial header. After that, writes fail. The keepalive ping
	// write should hit this and set clientDisconnected=true.
	failing := &keepaliveFailingWriter{ResponseWriter: c.Writer, failAfter: 0}
	c.Writer = failing

	svc := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				StreamKeepaliveInterval: 1,
				MaxLineSize:             defaultMaxLineSize,
			},
		},
		rateLimitService: &RateLimitService{},
	}

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Stay idle long enough to trigger the keepalive ping (which
		// will fail to write to the failing recorder). Then send
		// usage events so we can verify they still get accumulated
		// after the disconnect.
		time.Sleep(1200 * time.Millisecond)
		_, _ = pw.Write([]byte(strings.Join([]string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":17}}}`,
			"",
			`data: {"type":"message_delta","usage":{"output_tokens":29}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n")))
		_ = pw.Close()
	}()

	result, _ := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, &Account{ID: 42}, time.Now(), "claude-3-7-sonnet-20250219")
	_ = pr.Close()
	<-done

	require.NotNil(t, result, "result must not be nil — usage must be returned even after disconnect")
	require.True(t, result.clientDisconnect, "clientDisconnect MUST be set when ping write fails")
	require.NotNil(t, result.usage, "usage must still be parsed from upstream after the disconnect")
	require.Equal(t, 17, result.usage.InputTokens, "input_tokens MUST be drained from upstream even after ping-write failure")
	require.Equal(t, 29, result.usage.OutputTokens, "output_tokens MUST be drained from upstream even after ping-write failure")
}

// keepaliveFailingWriter wraps a ResponseWriter so Write returns an
// error after `failAfter` successful writes. Used to simulate client
// disconnect during the keepalive ping.
type keepaliveFailingWriter struct {
	gin.ResponseWriter
	failAfter int
	writes    int32
}

func (w *keepaliveFailingWriter) Write(p []byte) (int, error) {
	n := atomic.AddInt32(&w.writes, 1)
	if int(n) > w.failAfter {
		return 0, errors.New("write failed: client disconnected")
	}
	return w.ResponseWriter.Write(p)
}

func (w *keepaliveFailingWriter) WriteString(s string) (int, error) {
	n := atomic.AddInt32(&w.writes, 1)
	if int(n) > w.failAfter {
		return 0, errors.New("write failed: client disconnected")
	}
	return io.WriteString(w.ResponseWriter, s)
}
