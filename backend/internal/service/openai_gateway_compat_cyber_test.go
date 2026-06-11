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

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// compatCyberOAuthAccount 是 compat cyber 测试共用的 OAuth 账号。
func compatCyberOAuthAccount() *Account {
	return &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}
}

// compatCyberUpstreamSSE 构造上游 responses SSE：response.created 后 response.failed(cyber_policy)。
func compatCyberUpstreamSSE() string {
	return strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_cyber","model":"gpt-5.5","status":"in_progress","output":[]}}`,
		"",
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_cyber","object":"response","model":"gpt-5.5","status":"failed","output":[],"error":{"code":"cyber_policy","message":"flagged for cyber policy"}}}`,
		"",
	}, "\n")
}

func compatCyberUpstreamRecorder() *httpUpstreamRecorder {
	return &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_cyber"}},
		Body:       io.NopCloser(strings.NewReader(compatCyberUpstreamSSE())),
	}}
}

// C-1: chat completions 非流式客户端（buffered 路径）cyber 命中——不 failover、标记已设、
// 以 chat 错误格式回写、丢弃普通 result（handler 改用 cyber 专用路径记录真实 token）。
func TestForwardAsChatCompletions_BufferedCyberPolicyNoFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	svc := &OpenAIGatewayService{httpUpstream: compatCyberUpstreamRecorder()}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, compatCyberOAuthAccount(), body, "", "gpt-5.5")
	require.Error(t, err)
	require.Nil(t, result, "cyber must drop normal result so handler uses the dedicated cyber usage path")
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "cyber must NOT trigger failover")
	mark := GetOpsCyberPolicy(c)
	require.NotNil(t, mark, "cyber mark must be set for handler-side recording")
	require.Equal(t, "cyber_policy", mark.Code)
	require.True(t, c.Writer.Written(), "cyber error must be written to client (passthrough)")
}

// I-1: chat completions 流式客户端 cyber 命中——result 必须被丢弃（返回 nil），
// 使 handler forwardErrored 分支走专用 cyber 计费，避免正常 RecordUsage 重复扣费。
func TestForwardAsChatCompletions_StreamCyberPolicyDropsResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	svc := &OpenAIGatewayService{httpUpstream: compatCyberUpstreamRecorder()}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, compatCyberOAuthAccount(), body, "", "gpt-5.5")
	require.Error(t, err)
	require.Nil(t, result, "cyber must drop normal result so billing is recorded exactly once")
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "cyber must NOT trigger failover")
	require.NotNil(t, GetOpsCyberPolicy(c), "cyber mark must be set")
	require.Contains(t, rec.Body.String(), "data: [DONE]", "stream must terminate with [DONE]")
}

// anthropic 非流式客户端（buffered 路径）cyber 命中——不 failover、标记已设、以 anthropic 错误格式回写、丢弃 result。
func TestForwardAsAnthropic_BufferedCyberPolicyNoFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	svc := &OpenAIGatewayService{httpUpstream: compatCyberUpstreamRecorder()}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, compatCyberOAuthAccount(), body, "", "gpt-5.5")
	require.Error(t, err)
	require.Nil(t, result, "cyber must drop normal result so handler uses the dedicated cyber usage path")
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "cyber must NOT trigger failover")
	mark := GetOpsCyberPolicy(c)
	require.NotNil(t, mark, "cyber mark must be set")
	require.Equal(t, "cyber_policy", mark.Code)
	require.True(t, c.Writer.Written(), "anthropic cyber error must be written to client")
	require.Contains(t, rec.Body.String(), `"type":"error"`, "must use anthropic error envelope")
}

// anthropic 流式客户端 cyber 命中——不 failover、标记已设、下发 anthropic SSE error 事件、丢弃 result。
func TestForwardAsAnthropic_StreamCyberPolicyNoFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	svc := &OpenAIGatewayService{httpUpstream: compatCyberUpstreamRecorder()}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, compatCyberOAuthAccount(), body, "", "gpt-5.5")
	require.Error(t, err)
	require.Nil(t, result, "cyber must drop normal result so billing is recorded exactly once")
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "cyber must NOT trigger failover")
	require.NotNil(t, GetOpsCyberPolicy(c), "cyber mark must be set")
	require.Contains(t, rec.Body.String(), "event: error", "must emit anthropic SSE error event")
}

// API-key Responses backends may honor stream=false and return response.failed
// directly as JSON. That newer transport branch must retain the same hard-stop,
// audit marker, and real-token semantics as the original SSE paths.
func TestForwardAsAnthropic_DirectJSONCyberPolicyNoFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := `{"id":"resp_cyber_json","object":"response","model":"gpt-5.5","status":"failed","output":[],"error":{"code":"cyber_policy","message":"flagged for cyber policy"},"usage":{"input_tokens":13,"output_tokens":2,"total_tokens":15}}`
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_cyber_json"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
			Enabled: false,
		}}},
		httpUpstream: upstream,
	}
	account := &Account{
		ID: 2, Name: "openai-api-key", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.example.com/v1"},
		Extra:       map[string]any{"use_responses_api": true},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.5")
	require.Error(t, err)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "cyber must NOT trigger failover")
	mark := GetOpsCyberPolicy(c)
	require.NotNil(t, mark)
	require.Equal(t, "cyber_policy", mark.Code)
	require.Equal(t, 13, mark.UpstreamInTok)
	require.Equal(t, 2, mark.UpstreamOutTok)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `"type":"error"`)
}
