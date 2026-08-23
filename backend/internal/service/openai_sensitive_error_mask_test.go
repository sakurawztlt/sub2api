package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestContainsOpenAICompatSensitiveBackendTerm(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
		body    []byte
		want    bool
	}{
		{
			name:    "codex chatgpt unsupported model",
			message: "The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account.",
			want:    true,
		},
		{
			name: "body contains gpt 5.5",
			body: []byte(`{"error":{"message":"gpt-5.5 is unavailable"}}`),
			want: true,
		},
		{
			name:    "generic user bad request",
			message: "max_tokens: 128000 > 64000",
			want:    false,
		},
		{
			name:    "generic network failure",
			message: "upstream connection reset",
			want:    false,
		},
		{
			name:    "generic openai provider text without backend model tell",
			message: "OpenAI upstream returned a transient error",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, containsOpenAICompatSensitiveBackendTerm(tt.message, tt.body))
		})
	}
}

func TestForwardAsAnthropic_SensitiveBackendErrorTriggersMaskedFailover(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := `{"error":{"message":"The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account.","type":"invalid_request_error"},"type":"error"}`
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID: 1, Name: "openai-oauth", Platform: PlatformOpenAI,
		Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.4")

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	resp := strings.ToLower(string(failoverErr.ResponseBody))
	require.Contains(t, resp, strings.ToLower(openAICompatSensitiveBackendErrorMessage))
	require.NotContains(t, resp, "gpt")
	require.NotContains(t, resp, "codex")
	require.NotContains(t, resp, "chatgpt")
	require.NotContains(t, resp, "5.4")
	require.NotContains(t, strings.ToLower(err.Error()), "gpt")
	require.NotContains(t, strings.ToLower(err.Error()), "codex")
}

func TestShouldFailoverOpenAIUpstreamResponseForAccount_SensitiveOAuthOnly(t *testing.T) {
	t.Parallel()

	svc := &OpenAIGatewayService{}
	body := []byte(`{"error":{"message":"gpt-5.4 is unavailable for Codex"}}`)
	oauth := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKey := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	require.True(t, svc.shouldFailoverOpenAIUpstreamResponseForAccount(http.StatusBadRequest, "", body, oauth))
	require.False(t, svc.shouldFailoverOpenAIUpstreamResponseForAccount(http.StatusBadRequest, "", body, apiKey))
	require.False(t, svc.shouldFailoverOpenAIUpstreamResponseForAccount(http.StatusBadRequest, "max_tokens: 128000 > 64000", nil, oauth))
}

func TestOpenAIStreamFailoverErrorMasksSensitiveBackendTerms(t *testing.T) {
	t.Parallel()

	svc := &OpenAIGatewayService{}
	account := &Account{
		ID: 1, Name: "openai-oauth", Platform: PlatformOpenAI,
		Type: AccountTypeOAuth, Concurrency: 1,
	}
	payload := []byte(`{"type":"response.failed","response":{"error":{"message":"gpt-5.5 is unavailable for Codex"}}}`)

	failoverErr := svc.newOpenAIStreamFailoverError(nil, account, false, "req_sensitive", payload, "gpt-5.5 is unavailable for Codex")

	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	resp := strings.ToLower(string(failoverErr.ResponseBody))
	require.Contains(t, resp, strings.ToLower(openAICompatSensitiveBackendErrorMessage))
	require.NotContains(t, resp, "gpt")
	require.NotContains(t, resp, "codex")
	require.NotContains(t, resp, "chatgpt")
	require.NotContains(t, resp, "5.5")
}

func TestForwardAsAnthropic_GenericBadRequestStillPassesClientActionable400(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := `{"error":{"message":"max_tokens: 128000 > 64000","type":"invalid_request_error"},"type":"error"}`
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID: 1, Name: "openai-oauth", Platform: PlatformOpenAI,
		Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.4")

	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "max_tokens: 128000")
}

func TestHandleSSEToJSON_SensitiveResponseFailedMasksBody(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	svc := &OpenAIGatewayService{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	body := []byte(strings.Join([]string{
		`data: {"type":"response.failed","error":{"message":"gpt-5.4 is unavailable for Codex"}}`,
		"",
	}, "\n"))

	usage, err := svc.handleSSEToJSON(resp, c, nil, body, "gpt-5.4", "gpt-5.4")

	require.Nil(t, usage)
	require.Error(t, err)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	respBody := strings.ToLower(rec.Body.String())
	require.Contains(t, respBody, strings.ToLower(openAICompatSensitiveBackendErrorMessage))
	require.NotContains(t, respBody, "gpt")
	require.NotContains(t, respBody, "codex")
	require.NotContains(t, strings.ToLower(err.Error()), "gpt")
	require.NotContains(t, strings.ToLower(err.Error()), "codex")
}
