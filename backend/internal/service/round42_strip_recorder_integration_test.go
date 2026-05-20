package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// round42Cfg is a minimal cfg shared by the recorder integration tests.
// validateUpstreamBaseURL needs svc.cfg to be non-nil; the GatewayConfig
// defaults are fine for these tests (we're not exercising the policy
// paths).
var round42Cfg = config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}}

// codex round42 fu60 (2026-05-20): recorder-level integration test.
//
// codex round42 #3 critique of fu59: the round41 helper-level tests
// (round41_strip_after_model_mapping_test.go) verify IsReasoningModel
// alone — they would have passed even if someone reverted the
// strip-after-mapping line in openai_gateway_messages.go. We need a
// test that drives Forward through httpUpstreamRecorder and asserts
// against the bytes actually sent to upstream.
//
// This file: claude-opus-4-6 → upstream gpt-5.x mapping with a body
// that carries temperature/top_p. After Forward, the recorder must
// hold an upstream body that has NEITHER field — proving the strip
// happens on the OUTBOUND path, regardless of which gateway entry
// point is taken.

// round42UpstreamGpt5Response is a minimal valid Responses-API SSE
// response that lets Forward complete successfully. The test only
// cares about the OUTBOUND body, but Forward needs a parseable
// upstream response to return without error.
func round42Gpt5UpstreamResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid-round42"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_round42","status":"completed","model":"gpt-5.2","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
	}
}

// TestRound42_Forward_NativeResponses_StripsSamplingParamsForGpt5Upstream verifies
// the native /v1/responses entry point: client sends a request body
// targeting gpt-5.x with temperature/top_p set; sub2api maps and
// forwards. The OUTBOUND body must NOT carry temperature or top_p.
//
// This is the case codex round42 flagged as "顺手审 native /v1/responses
// APIKey path" — fu60 added the strip at the bottom of the
// model-mapping block in openai_gateway_service.go.
func TestRound42_Forward_NativeResponses_StripsSamplingParamsForGpt5Upstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{
		"model":"gpt-5.2",
		"stream":false,
		"instructions":"round42",
		"input":"hello",
		"temperature":0.7,
		"top_p":0.9
	}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: round42Gpt5UpstreamResponse()}
	svc := &OpenAIGatewayService{cfg: &round42Cfg, httpUpstream: upstream}

	account := &Account{
		ID:          1001,
		Name:        "round42-apikey-account",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey, // ← APIKey path (not OAuth) — the codex round42 case
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-fake-round42",
			"base_url": "https://api.openai.com/v1",
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotEmpty(t, upstream.lastBody, "recorder must have captured an upstream call")

	// The key assertion: the bytes sent UPSTREAM must not carry the two
	// reasoning-rejected sampling params. The model itself stays gpt-5.2.
	require.Equal(t, "gpt-5.2", gjson.GetBytes(upstream.lastBody, "model").String(),
		"upstream model must be gpt-5.2 (mapping unchanged)")
	require.False(t, gjson.GetBytes(upstream.lastBody, "temperature").Exists(),
		"temperature MUST be stripped from outbound body for gpt-5 reasoning model (codex round42 #3 — native /v1/responses APIKey path)")
	require.False(t, gjson.GetBytes(upstream.lastBody, "top_p").Exists(),
		"top_p MUST be stripped from outbound body for gpt-5 reasoning model")
}

// TestRound42_Forward_NativeResponses_PreservesSamplingForNonReasoning verifies
// the regression guard: when the upstream model is NOT a reasoning
// model (e.g. gpt-4o), Forward must preserve temperature/top_p.
// Without this guard, fu60's strip would over-fire and degrade
// gpt-4o quality.
func TestRound42_Forward_NativeResponses_PreservesSamplingForNonReasoning(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{
		"model":"gpt-4o",
		"stream":false,
		"input":"hello",
		"temperature":0.7,
		"top_p":0.9
	}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid-round42-4o"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_round42_4o","status":"completed","model":"gpt-4o","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &round42Cfg, httpUpstream: upstream}

	account := &Account{
		ID:          1002,
		Name:        "round42-gpt4o",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-fake-round42",
			"base_url": "https://api.openai.com/v1",
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotEmpty(t, upstream.lastBody)

	// gpt-4o accepts sampling params — must NOT be stripped.
	tempResult := gjson.GetBytes(upstream.lastBody, "temperature")
	require.True(t, tempResult.Exists(),
		"gpt-4o must keep temperature in outbound body (regression guard against over-strip)")
	require.InDelta(t, 0.7, tempResult.Float(), 0.0001)

	topResult := gjson.GetBytes(upstream.lastBody, "top_p")
	require.True(t, topResult.Exists(), "gpt-4o must keep top_p")
	require.InDelta(t, 0.9, topResult.Float(), 0.0001)
}

// TestRound42_Forward_NativeResponses_StripsForVendorPrefixedUpstream verifies
// IsReasoningModel's vendor-prefix handling (fu59 hardening) drives
// the real-path strip — not just the helper. Upstream models like
// "openai/gpt-5.4" come from vendor-routed accounts; the strip must
// still fire on those.
func TestRound42_Forward_NativeResponses_StripsForVendorPrefixedUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{
		"model":"openai/gpt-5.4",
		"stream":false,
		"input":"hello",
		"temperature":0.5,
		"top_p":0.8
	}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid-round42-vendor"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_round42_vendor","status":"completed","model":"openai/gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &round42Cfg, httpUpstream: upstream}

	account := &Account{
		ID:          1003,
		Name:        "round42-vendor",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-fake-round42-vendor",
			"base_url": "https://litellm.example.com/v1",
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotEmpty(t, upstream.lastBody)

	require.False(t, gjson.GetBytes(upstream.lastBody, "temperature").Exists(),
		"vendor-prefixed gpt-5.x upstream must trigger strip (fu59 IsReasoningModel handles openai/ prefix)")
	require.False(t, gjson.GetBytes(upstream.lastBody, "top_p").Exists())
}
