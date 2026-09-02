package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func alphaSearchResponsesSSE(output string) string {
	return "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":` + strconv.Quote(output) + `}` + "\n\n" +
		"event: response.output_text.annotation.added\n" +
		`data: {"type":"response.output_text.annotation.added","annotation":{"type":"url_citation","url":"https://example.com/news","title":"Example News"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":` + strconv.Quote(output) + `}]}]}}` + "\n\n"
}

func TestForwardAlphaSearchOAuthPreservesWire(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{
		"id":"search-session",
		"model":"gpt-5.6-sol",
		"reasoning":{"effort":"max","context":"all_turns"},
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"latest news"}]}],
		"commands":{"search_query":[{"q":"OpenAI news","recency":1}]},
		"settings":{"allowed_callers":["direct"],"external_web_access":true},
		"max_output_tokens":2000,
		"future_field":{"keep":true}
	}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search?feature=standalone", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", codexCLIUserAgent)
	c.Request.Header.Set("Originator", "codex_cli_rs")
	c.Request.Header.Set("Version", "0.144.1")
	c.Request.Header.Set("X-Codex-Turn-Metadata", `{"session_id":"search-session","turn_id":"search-turn"}`)

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"encrypted_output":"ciphertext","output":"search result"}`)),
	}}
	service := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:          42,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-account",
		},
	}

	result, err := service.ForwardAlphaSearch(context.Background(), c, account, body)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"encrypted_output":"ciphertext","output":"search result"}`, recorder.Body.String())
	require.Equal(t, chatgptCodexAlphaSearchURL+"?feature=standalone", upstream.lastReq.URL.String())
	require.Equal(t, "chatgpt.com", upstream.lastReq.Host)
	require.Equal(t, "Bearer oauth-token", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "chatgpt-account", upstream.lastReq.Header.Get("chatgpt-account-id"))
	require.Equal(t, "application/json", upstream.lastReq.Header.Get("Accept"))
	require.Equal(t, codexCLIVersion, upstream.lastReq.Header.Get("Version"))
	require.Equal(t, codexCLIUserAgent, upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, "codex_cli_rs", upstream.lastReq.Header.Get("Originator"))
	require.Empty(t, upstream.lastReq.Header.Get("OpenAI-Beta"))
	require.Equal(t,
		scopeCodexAccountIdentityValue(account, 0, "session", "search-session"),
		gjson.Get(upstream.lastReq.Header.Get("X-Codex-Turn-Metadata"), "session_id").String(),
	)
	require.Equal(t,
		scopeCodexAccountIdentityValue(account, 0, "turn", "search-turn"),
		gjson.Get(upstream.lastReq.Header.Get("X-Codex-Turn-Metadata"), "turn_id").String(),
	)
	require.JSONEq(t, string(body), string(upstream.lastBody))
}

func TestForwardAlphaSearchAPIKeyMapsModelAndPassesThroughError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"id":"search-session","model":"gpt-5.6-sol","commands":{"search_query":[{"q":"news"}]}}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/alpha/search", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := `{"error":{"type":"invalid_request_error","message":"bad search"}}`
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	service := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:       7,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://compat.example/v4",
			"model_mapping": map[string]any{
				"gpt-5.6-sol": "upstream-5.6",
			},
		},
	}

	result, err := service.ForwardAlphaSearch(context.Background(), c, account, body)

	require.NoError(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.JSONEq(t, upstreamBody, recorder.Body.String())
	require.Equal(t, "https://compat.example/v4/alpha/search", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer sk-test", upstream.lastReq.Header.Get("Authorization"))
	require.Empty(t, upstream.lastReq.Header.Get("Originator"), "API-key alpha/search must not inherit OAuth identity")
	require.Equal(t, "upstream-5.6", gjson.GetBytes(upstream.lastBody, "model").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "commands.search_query").IsArray())
}

func TestForwardAlphaSearchReturnsFailoverBeforeWriting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"id":"search-session","model":"gpt-5.6-sol","commands":{}}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", bytes.NewReader(body))

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"rate limited"}}`)),
	}}
	service := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:       8,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "sk-test",
		},
	}

	result, err := service.ForwardAlphaSearch(context.Background(), c, account, body)

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Nil(t, result)
	require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
	require.Empty(t, failoverErr.Stage)
	require.Empty(t, failoverErr.Scope)
	require.Empty(t, failoverErr.Reason)
	require.Zero(t, failoverErr.ClientStatusCode)
	require.Empty(t, failoverErr.ClientMessage)
	require.Nil(t, failoverErr.ResponseHeaders)
	require.Equal(t, openAIPlatformAlphaSearchURL, upstream.lastReq.URL.String())
	require.False(t, c.Writer.Written())
	require.Empty(t, recorder.Body.String())
}

func TestForwardAlphaSearchPATUsesResponsesWebSearchFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"id":"search-session","model":"gpt-5.6-sol","commands":{"search_query":[{"q":"OpenAI news"}]},"prompt_cache_key":"cache","prompt_cache_retention":"24h"}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", bytes.NewReader(body))
	c.Request.Header.Set("User-Agent", codexCLIUserAgent)
	c.Request.Header.Set("Originator", "codex_cli_rs")
	c.Request.Header.Set("Version", "0.144.1")
	c.Request.Header.Set("X-Codex-Turn-Metadata", `{"turn_id":"turn-1"}`)

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"req-search"}},
		Body:       io.NopCloser(strings.NewReader(alphaSearchResponsesSSE("search result"))),
	}}
	service := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{
		"access_token": "at-test-token", "auth_mode": OpenAIAuthModePersonalAccessToken,
		"chatgpt_account_id": "chatgpt-account", "chatgpt_account_is_fedramp": true,
	}}

	result, err := service.ForwardAlphaSearch(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, result.WebSearchCalls)
	require.Equal(t, "/v1/responses", result.UpstreamEndpoint)
	require.JSONEq(t, `{"output":"search result","results":[{"type":"text_result","ref_id":"turn0search0","url":"https://example.com/news","title":"Example News"}]}`, recorder.Body.String())
	require.Equal(t, chatgptCodexURL, upstream.lastReq.URL.String())
	require.Equal(t, "Bearer at-test-token", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "chatgpt-account", upstream.lastReq.Header.Get("ChatGPT-Account-ID"))
	require.Equal(t, "true", upstream.lastReq.Header.Get("X-OpenAI-Fedramp"))
	require.Equal(t, "responses=experimental", upstream.lastReq.Header.Get("OpenAI-Beta"))
	require.Equal(t, resolveCodexOutboundIdentity("").originator, upstream.lastReq.Header.Get("Originator"))
	require.False(t, gjson.GetBytes(upstream.lastBody, "prompt_cache_key").Exists())
	require.False(t, gjson.GetBytes(upstream.lastBody, "prompt_cache_retention").Exists())
	require.Equal(t, "web_search", gjson.GetBytes(upstream.lastBody, "tools.0.type").String())
}

func TestAlphaSearchCompatibilityHelpers(t *testing.T) {
	normalized, err := sanitizeOpenAIAlphaSearchBody([]byte(`{"id":"s","store":false,"prompt_cache_key":"cache","prompt_cache_retention":"24h","commands":{"search_query":[{"q":"news"}]}}`))
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(normalized, "store").Exists())
	require.False(t, gjson.GetBytes(normalized, "prompt_cache_key").Exists())
	require.False(t, gjson.GetBytes(normalized, "prompt_cache_retention").Exists())
	require.Equal(t, "news", gjson.GetBytes(normalized, "commands.search_query.0.q").String())

	apiKey := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	oauth := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.True(t, isOpenAIAlphaSearchEndpointUnsupported(apiKey, http.StatusNotFound))
	require.True(t, isOpenAIAlphaSearchEndpointUnsupported(apiKey, http.StatusMethodNotAllowed))
	require.False(t, isOpenAIAlphaSearchEndpointUnsupported(oauth, http.StatusNotFound))
	require.False(t, shouldApplyOpenAIAlphaSearchAccountErrorSideEffects(http.StatusUnauthorized))
	require.False(t, shouldApplyOpenAIAlphaSearchAccountErrorSideEffects(http.StatusNotFound))
	require.True(t, shouldApplyOpenAIAlphaSearchAccountErrorSideEffects(http.StatusForbidden))

	mapped := &Account{Credentials: map[string]any{"model_mapping": map[string]any{"client-visible": "canonical-upstream"}}}
	require.Equal(t, "canonical-upstream", openAIAlphaSearchSchedulingModel(mapped, "client-visible"))
}

func TestForwardAlphaSearchSetupToken429CarriesSameAccountRetryWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"id":"search-session","model":"gpt-5.6-sol","commands":{}}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", bytes.NewReader(body))

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{"1"},
			"X-Request-Id": []string{"req_alpha_oauth_429"},
		},
		Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"rate limited"}}`)),
	}}
	service := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:          81,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeSetupToken,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-account",
		},
	}
	startedAt := time.Now()

	result, err := service.ForwardAlphaSearch(context.Background(), c, account, body)

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.Equal(t, time.Second, failoverErr.SameAccountRetryDelay)
	require.WithinDuration(t, startedAt.Add(openAIOAuth429RetryWindow), failoverErr.SameAccountRetryDeadline, time.Second)
	require.Equal(t, "req_alpha_oauth_429", failoverErr.ResponseHeaders.Get("x-request-id"))
	require.False(t, c.Writer.Written())
}

func TestForwardAlphaSearchAccessStateUsesTypedFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"id":"search-session","model":"gpt-5.6-sol","commands":{}}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", bytes.NewReader(body))

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusForbidden,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Request-Id": []string{"req_alpha_access_state"},
		},
		Body: io.NopCloser(strings.NewReader(`{"error":{"code":"deactivated_workspace","message":"Workspace is deactivated"}}`)),
	}}
	service := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:          11,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-account",
		},
	}

	result, err := service.ForwardAlphaSearch(context.Background(), c, account, body)

	require.Nil(t, result)
	assertOpenAIAlphaSearchAccessStateFailover(t, err, "req_alpha_access_state")
	require.Equal(t, chatgptCodexAlphaSearchURL, upstream.lastReq.URL.String())
	require.False(t, c.Writer.Written())
}

func TestForwardAlphaSearchPATFallbackAccessStateUsesTypedFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"id":"search-session","model":"gpt-5.6-sol","commands":{"search_query":[{"q":"news"}]}}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", bytes.NewReader(body))

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusForbidden,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Request-Id": []string{"req_alpha_pat_access_state"},
		},
		Body: io.NopCloser(strings.NewReader(`{"error":{"code":"account_disabled","message":"Account is disabled"}}`)),
	}}
	service := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:          12,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "at-test-token",
			"auth_mode":          OpenAIAuthModePersonalAccessToken,
			"chatgpt_account_id": "chatgpt-account",
		},
	}

	result, err := service.ForwardAlphaSearch(context.Background(), c, account, body)

	require.Nil(t, result)
	assertOpenAIAlphaSearchAccessStateFailover(t, err, "req_alpha_pat_access_state")
	require.Equal(t, chatgptCodexURL, upstream.lastReq.URL.String())
	require.False(t, c.Writer.Written())
}

func assertOpenAIAlphaSearchAccessStateFailover(t *testing.T, err error, requestID string) {
	t.Helper()
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusForbidden, failoverErr.StatusCode)
	require.Equal(t, GatewayFailureStageAccountAuth, failoverErr.Stage)
	require.Equal(t, GatewayFailureScopeAccount, failoverErr.Scope)
	require.Equal(t, OpenAIUpstreamAccessStateReason, failoverErr.Reason)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.Equal(t, http.StatusBadGateway, failoverErr.ClientStatusCode)
	require.Equal(t, openAIUpstreamAccessUnavailableClientMessage, failoverErr.ClientMessage)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.Equal(t, requestID, failoverErr.ResponseHeaders.Get("x-request-id"))
}

func TestOpenAIAlphaSearchSchedulingModelUsesCanonicalAccountMapping(t *testing.T) {
	account := &Account{Credentials: map[string]any{
		"model_mapping": map[string]any{"client-visible": "canonical-upstream"},
	}}
	require.Equal(t, "canonical-upstream", openAIAlphaSearchSchedulingModel(account, "client-visible"))
	require.Equal(t, "unmapped", openAIAlphaSearchSchedulingModel(account, "unmapped"))
}

func TestSanitizeOpenAIAlphaSearchBody_RemovesResponsesOnlyFields(t *testing.T) {
	body := []byte(`{"id":"search-session","store":false,"prompt_cache_key":"cache","commands":{"search_query":[{"q":"news"}]}}`)

	normalized, err := sanitizeOpenAIAlphaSearchBody(body)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(normalized, "store").Exists())
	require.False(t, gjson.GetBytes(normalized, "prompt_cache_key").Exists())
	require.Equal(t, "news", gjson.GetBytes(normalized, "commands.search_query.0.q").String())
}
