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

// codex round43 fu61 (2026-05-20): close the two residuals codex round43
// found in fu60.
//
//   (1) Single-field patch leaked through fast-patch path. fu60 used
//       markPatchSet("temperature", nil) / markPatchSet("top_p", nil)
//       which works for two-field requests (mismatched patchPath
//       disables fast-patch, marshal path runs and the deleted map
//       keys are gone) but NOT for single-field requests: only one
//       markPatchSet call, fast-patch path runs, sjson.Set writes
//       "temperature": null instead of removing the key. Upstream
//       still rejects.
//
//   (2) forwardOpenAIPassthrough was completely uncovered. Forward()
//       returns early when account.IsOpenAIPassthroughEnabled() so
//       none of fu59/fu60's strip logic ran on that path.
//
// fu61 routes all four entry points through
// stripSamplingParamsForReasoningModelBody (a sjson.DeleteBytes-based
// helper) and the native path switches from markPatchSet to
// markPatchDelete so the fast-patch path also calls DeleteBytes.

var round43Cfg = config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}}

func round43Gpt5OkResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid-round43"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_round43","status":"completed","model":"gpt-5.2","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
	}
}

func round43NewCtx(method, url string, body []byte) *gin.Context {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, url, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

// === codex round43 #1: single-field patch ===
//
// The fu60 implementation worked on the two-field test case
// (TestRound42_Forward_NativeResponses_StripsSamplingParamsForGpt5Upstream)
// because two markPatchSet calls with different paths trip the
// "mismatched patchPath" branch and disable fast-patch entirely,
// forcing the marshal path. These two tests cover what happens when
// the client only sends one of the two.

func TestRound43_Forward_NativeResponses_StripsTemperatureOnlyForGpt5(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.2",
		"stream":false,
		"instructions":"round43-temp-only",
		"input":"hello",
		"temperature":0.7
	}`)
	c := round43NewCtx(http.MethodPost, "/v1/responses", body)

	upstream := &httpUpstreamRecorder{resp: round43Gpt5OkResponse()}
	svc := &OpenAIGatewayService{cfg: &round43Cfg, httpUpstream: upstream}
	account := &Account{
		ID:          2001,
		Name:        "round43-temp-only",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-fake-r43", "base_url": "https://api.openai.com/v1"},
		Status:      StatusActive,
		Schedulable: true,
	}

	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotEmpty(t, upstream.lastBody)

	// The critical assertion: the key MUST be absent. fu60 would have
	// emitted "temperature":null here via the fast-patch path because
	// only one markPatchSet call ran. fu61's markPatchDelete fixes that.
	temp := gjson.GetBytes(upstream.lastBody, "temperature")
	require.False(t, temp.Exists(),
		"single-field temperature MUST be removed (codex round43 #1), got upstream body: %s", string(upstream.lastBody))
	require.False(t, gjson.GetBytes(upstream.lastBody, "top_p").Exists(),
		"top_p was not in client body — must still be absent")
}

func TestRound43_Forward_NativeResponses_StripsTopPOnlyForGpt5(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.2",
		"stream":false,
		"instructions":"round43-topp-only",
		"input":"hello",
		"top_p":0.9
	}`)
	c := round43NewCtx(http.MethodPost, "/v1/responses", body)

	upstream := &httpUpstreamRecorder{resp: round43Gpt5OkResponse()}
	svc := &OpenAIGatewayService{cfg: &round43Cfg, httpUpstream: upstream}
	account := &Account{
		ID:          2002,
		Name:        "round43-topp-only",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-fake-r43", "base_url": "https://api.openai.com/v1"},
		Status:      StatusActive,
		Schedulable: true,
	}

	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotEmpty(t, upstream.lastBody)

	require.False(t, gjson.GetBytes(upstream.lastBody, "top_p").Exists(),
		"single-field top_p MUST be removed (codex round43 #1)")
	require.False(t, gjson.GetBytes(upstream.lastBody, "temperature").Exists())
}

// === codex round43 #2: passthrough was uncovered ===
//
// APIKey-backed accounts with Extra.openai_passthrough = true bypass
// the entire transform pipeline and forward the raw body. fu59/fu60
// strips never ran here. fu61 added a strip call in
// forwardOpenAIPassthrough right after the policyModel resolution.

func TestRound43_Forward_APIKeyPassthrough_StripsForGpt5(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.2",
		"stream":false,
		"input":"hello",
		"temperature":0.7,
		"top_p":0.9
	}`)
	c := round43NewCtx(http.MethodPost, "/v1/responses", body)

	upstream := &httpUpstreamRecorder{resp: round43Gpt5OkResponse()}
	svc := &OpenAIGatewayService{cfg: &round43Cfg, httpUpstream: upstream}
	account := &Account{
		ID:          2003,
		Name:        "round43-apikey-passthrough",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-fake-r43-pt", "base_url": "https://api.openai.com/v1"},
		// This is the gate that sends the request into forwardOpenAIPassthrough.
		Extra:       map[string]any{"openai_passthrough": true},
		Status:      StatusActive,
		Schedulable: true,
	}

	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotEmpty(t, upstream.lastBody)

	require.False(t, gjson.GetBytes(upstream.lastBody, "temperature").Exists(),
		"APIKey passthrough MUST strip temperature for gpt-5.x (codex round43 #2)")
	require.False(t, gjson.GetBytes(upstream.lastBody, "top_p").Exists(),
		"APIKey passthrough MUST strip top_p for gpt-5.x")
	require.Equal(t, "gpt-5.2", gjson.GetBytes(upstream.lastBody, "model").String())
}

func TestRound43_Forward_OAuthPassthrough_StripsForGpt5(t *testing.T) {
	// OAuth passthrough's normalize step forces stream=true (Codex SSE
	// requirement) regardless of what the client asked, so the recorder
	// response must be SSE-shaped or the stream parser fails. We're
	// asserting on the OUTBOUND body here, not the response semantics,
	// so a minimal completion sequence is enough.
	body := []byte(`{
		"model":"gpt-5.2",
		"stream":true,
		"input":"hello",
		"temperature":0.7,
		"top_p":0.9
	}`)
	c := round43NewCtx(http.MethodPost, "/v1/responses", body)

	sse := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_r43\",\"model\":\"gpt-5.2\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_r43\",\"model\":\"gpt-5.2\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
		"data: [DONE]\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid-r43-oauth"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}}

	svc := &OpenAIGatewayService{cfg: &round43Cfg, httpUpstream: upstream}
	account := &Account{
		ID:          2004,
		Name:        "round43-oauth-passthrough",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-r43",
			"chatgpt_account_id": "chatgpt-r43",
		},
		Extra:       map[string]any{"openai_passthrough": true},
		Status:      StatusActive,
		Schedulable: true,
	}

	// Tolerate a non-nil err here: the core assertion is what the
	// upstream RECEIVED. httpUpstreamRecorder captures lastBody on Do
	// regardless of how the stream is consumed afterwards, so even a
	// partial parse downstream still gives us a definitive answer on
	// the OUTBOUND-side strip behaviour.
	_, _ = svc.Forward(context.Background(), c, account, body)
	require.NotEmpty(t, upstream.lastBody,
		"recorder must have captured the upstream call even if stream parsing later fails")

	require.False(t, gjson.GetBytes(upstream.lastBody, "temperature").Exists(),
		"OAuth passthrough MUST strip temperature for gpt-5.x (codex round43 #2 — belt-and-braces because passthrough bypasses applyCodexOAuthTransform)")
	require.False(t, gjson.GetBytes(upstream.lastBody, "top_p").Exists())
}

// === Regression guard: non-reasoning model must keep sampling ===

func TestRound43_Forward_Gpt4oPassthrough_PreservesSamplingParams(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o",
		"stream":false,
		"input":"hello",
		"temperature":0.7,
		"top_p":0.9
	}`)
	c := round43NewCtx(http.MethodPost, "/v1/responses", body)

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid-round43-4o"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_round43_4o","status":"completed","model":"gpt-4o","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &round43Cfg, httpUpstream: upstream}
	account := &Account{
		ID:          2005,
		Name:        "round43-gpt4o-passthrough",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-fake-r43-4o", "base_url": "https://api.openai.com/v1"},
		Extra:       map[string]any{"openai_passthrough": true},
		Status:      StatusActive,
		Schedulable: true,
	}

	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotEmpty(t, upstream.lastBody)

	// gpt-4o accepts sampling params. The strip must NOT fire for
	// non-reasoning models — without this guard, fu61's helper plus
	// any future tightening could over-strip and silently degrade
	// gpt-4o output quality.
	temp := gjson.GetBytes(upstream.lastBody, "temperature")
	require.True(t, temp.Exists(),
		"gpt-4o passthrough MUST preserve temperature (regression guard)")
	require.InDelta(t, 0.7, temp.Float(), 0.0001)
	topP := gjson.GetBytes(upstream.lastBody, "top_p")
	require.True(t, topP.Exists())
	require.InDelta(t, 0.9, topP.Float(), 0.0001)
}

// === Helper-level smoke ===
// Direct unit check for the new shared helper. The byte-level
// invariant matters: callers expect (body, modified, err) and a
// non-modified return when the model is not reasoning.

func TestRound43_StripSamplingParamsHelper_NonReasoningNoop(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","temperature":0.7,"top_p":0.9}`)
	out, modified, err := stripSamplingParamsForReasoningModelBody("gpt-4o", body)
	require.NoError(t, err)
	require.False(t, modified)
	require.JSONEq(t, string(body), string(out))
}

func TestRound43_StripSamplingParamsHelper_ReasoningSingleField(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","temperature":0.7}`)
	out, modified, err := stripSamplingParamsForReasoningModelBody("gpt-5.2", body)
	require.NoError(t, err)
	require.True(t, modified)
	require.False(t, gjson.GetBytes(out, "temperature").Exists())
}

func TestRound43_StripSamplingParamsHelper_ReasoningBothFields(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","temperature":0.7,"top_p":0.9}`)
	out, modified, err := stripSamplingParamsForReasoningModelBody("gpt-5.2", body)
	require.NoError(t, err)
	require.True(t, modified)
	require.False(t, gjson.GetBytes(out, "temperature").Exists())
	require.False(t, gjson.GetBytes(out, "top_p").Exists())
}

func TestRound43_StripSamplingParamsHelper_ReasoningNeitherField(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","input":"hi"}`)
	out, modified, err := stripSamplingParamsForReasoningModelBody("gpt-5.2", body)
	require.NoError(t, err)
	require.False(t, modified)
	require.JSONEq(t, string(body), string(out))
}
