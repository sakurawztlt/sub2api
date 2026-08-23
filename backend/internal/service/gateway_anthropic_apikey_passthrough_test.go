package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type anthropicHTTPUpstreamRecorder struct {
	lastReq  *http.Request
	lastBody []byte
	resp     *http.Response
	err      error
}

func newAnthropicAPIKeyAccountForTest() *Account {
	return &Account{
		ID:          201,
		Name:        "anthropic-apikey-pass-test",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "upstream-anthropic-key",
			"base_url": "https://api.anthropic.com",
		},
		Extra: map[string]any{
			"anthropic_passthrough": true,
		},
		Status:      StatusActive,
		Schedulable: true,
	}
}

func (u *anthropicHTTPUpstreamRecorder) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	u.lastReq = req
	if req != nil && req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		u.lastBody = b
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(b))
	}
	if u.err != nil {
		return nil, u.err
	}
	return u.resp, nil
}

func (u *anthropicHTTPUpstreamRecorder) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

type streamReadCloser struct {
	payload []byte
	sent    bool
	err     error
}

func (r *streamReadCloser) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		n := copy(p, r.payload)
		return n, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	return 0, io.EOF
}

func (r *streamReadCloser) Close() error { return nil }

type failWriteResponseWriter struct {
	gin.ResponseWriter
}

func (w *failWriteResponseWriter) Write(data []byte) (int, error) {
	return 0, errors.New("client disconnected")
}

func (w *failWriteResponseWriter) WriteString(_ string) (int, error) {
	return 0, errors.New("client disconnected")
}

func TestGatewayService_AnthropicAPIKeyPassthrough_ForwardStreamPreservesBodyAndAuthReplacement(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "claude-cli/1.0.0")
	c.Request.Header.Set("Authorization", "Bearer inbound-token")
	c.Request.Header.Set("X-Api-Key", "inbound-api-key")
	c.Request.Header.Set("X-Goog-Api-Key", "inbound-goog-key")
	c.Request.Header.Set("Cookie", "secret=1")
	c.Request.Header.Set("Anthropic-Beta", "interleaved-thinking-2025-05-14")

	body := []byte(`{"model":"claude-3-7-sonnet-20250219","stream":true,"system":[{"type":"text","text":"x-anthropic-billing-header keep"}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	parsed := &ParsedRequest{
		Body:   NewRequestBodyRef(body),
		Model:  "claude-3-7-sonnet-20250219",
		Stream: true,
	}

	upstreamSSE := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":9,"cached_tokens":7}}}`,
		"",
		`data: {"type":"message_delta","usage":{"output_tokens":3}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &anthropicHTTPUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
				"x-request-id": []string{"rid-anthropic-pass"},
				"Set-Cookie":   []string{"secret=upstream"},
			},
			Body: io.NopCloser(strings.NewReader(upstreamSSE)),
		},
	}

	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			MaxLineSize: defaultMaxLineSize,
		},
	}
	svc := &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     &RateLimitService{},
		deferredService:      &DeferredService{},
		billingCacheService:  nil,
	}

	account := &Account{
		ID:          101,
		Name:        "anthropic-apikey-pass",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":       "upstream-anthropic-key",
			"base_url":      "https://api.anthropic.com",
			"model_mapping": map[string]any{"claude-3-7-sonnet-20250219": "claude-3-haiku-20240307"},
		},
		Extra: map[string]any{
			"anthropic_passthrough": true,
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	result, err := svc.Forward(context.Background(), c, account, parsed)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)

	require.Equal(t, "claude-3-haiku-20240307", gjson.GetBytes(upstream.lastBody, "model").String(), "透传模式应应用账号级模型映射")

	require.Equal(t, "upstream-anthropic-key", getHeaderRaw(upstream.lastReq.Header, "x-api-key"))
	require.Empty(t, getHeaderRaw(upstream.lastReq.Header, "authorization"))
	require.Empty(t, getHeaderRaw(upstream.lastReq.Header, "x-goog-api-key"))
	require.Empty(t, getHeaderRaw(upstream.lastReq.Header, "cookie"))
	require.Equal(t, "2023-06-01", getHeaderRaw(upstream.lastReq.Header, "anthropic-version"))
	require.Equal(t, "interleaved-thinking-2025-05-14", getHeaderRaw(upstream.lastReq.Header, "anthropic-beta"))
	require.Empty(t, getHeaderRaw(upstream.lastReq.Header, "x-stainless-lang"), "API Key 透传不应注入 OAuth 指纹头")

	require.Contains(t, rec.Body.String(), `"cached_tokens":7`)
	require.NotContains(t, rec.Body.String(), `"cache_read_input_tokens":7`, "透传输出不应被网关改写")
	require.Equal(t, 7, result.Usage.CacheReadInputTokens, "计费 usage 解析应保留 cached_tokens 兼容")
	require.Empty(t, rec.Header().Get("Set-Cookie"), "响应头应经过安全过滤")
	rawBody, ok := c.Get(OpsUpstreamRequestBodyKey)
	require.True(t, ok)
	bodyBytes, ok := rawBody.([]byte)
	require.True(t, ok, "应以 []byte 形式缓存上游请求体，避免重复 string 拷贝")
	require.Equal(t, "claude-3-haiku-20240307", gjson.GetBytes(bodyBytes, "model").String(), "缓存的上游请求体应包含映射后的模型")
}

func TestGatewayService_AnthropicAPIKeyPassthrough_ForwardCountTokensPreservesBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
	c.Request.Header.Set("Authorization", "Bearer inbound-token")
	c.Request.Header.Set("X-Api-Key", "inbound-api-key")
	c.Request.Header.Set("Cookie", "secret=1")

	body := []byte(`{"model":"claude-3-5-sonnet-latest","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"thinking":{"type":"enabled"}}`)
	parsed := &ParsedRequest{
		Body:  NewRequestBodyRef(body),
		Model: "claude-3-5-sonnet-latest",
	}

	upstreamRespBody := `{"input_tokens":42}`
	upstream := &anthropicHTTPUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
				"x-request-id": []string{"rid-count"},
				"Set-Cookie":   []string{"secret=upstream"},
			},
			Body: io.NopCloser(strings.NewReader(upstreamRespBody)),
		},
	}

	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			MaxLineSize: defaultMaxLineSize,
		},
	}
	svc := &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     &RateLimitService{},
	}

	account := &Account{
		ID:          102,
		Name:        "anthropic-apikey-pass-count",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":       "upstream-anthropic-key",
			"base_url":      "https://api.anthropic.com",
			"model_mapping": map[string]any{"claude-3-5-sonnet-latest": "claude-3-opus-20240229"},
		},
		Extra: map[string]any{
			"anthropic_passthrough": true,
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	err := svc.ForwardCountTokens(context.Background(), c, account, parsed)
	require.NoError(t, err)

	require.Equal(t, "claude-3-opus-20240229", gjson.GetBytes(upstream.lastBody, "model").String(), "count_tokens 透传模式应应用账号级模型映射")
	require.Equal(t, "upstream-anthropic-key", getHeaderRaw(upstream.lastReq.Header, "x-api-key"))
	require.Empty(t, getHeaderRaw(upstream.lastReq.Header, "authorization"))
	require.Empty(t, getHeaderRaw(upstream.lastReq.Header, "cookie"))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, upstreamRespBody, rec.Body.String())
	require.Empty(t, rec.Header().Get("Set-Cookie"))
}

func TestGatewayService_AnthropicAPIKeyPassthrough_BearerAuthScheme(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("Authorization", "Bearer inbound-token")
	c.Request.Header.Set("X-Api-Key", "inbound-api-key")
	c.Request.Header.Set("Cookie", "secret=1")

	svc := &GatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false},
			},
		},
	}
	account := &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "ollama-key",
			"base_url": "https://ollama.com",
		},
		Extra: map[string]any{
			"anthropic_passthrough":        true,
			"anthropic_apikey_auth_scheme": AnthropicAPIKeyAuthSchemeAuthorizationBearer,
		},
	}

	msgReq, wireBody, err := svc.buildUpstreamRequestAnthropicAPIKeyPassthrough(
		context.Background(), c, account, []byte(`{"model":"gpt-oss:20b","messages":[]}`), "ollama-key",
	)
	require.NoError(t, err)
	require.Equal(t, "https://ollama.com/v1/messages?beta=true", msgReq.URL.String())
	require.JSONEq(t, `{"model":"gpt-oss:20b","messages":[]}`, string(wireBody))
	require.Equal(t, "Bearer ollama-key", getHeaderRaw(msgReq.Header, "authorization"))
	require.Empty(t, getHeaderRaw(msgReq.Header, "x-api-key"))
	require.Empty(t, getHeaderRaw(msgReq.Header, "cookie"))

	countReq, err := svc.buildCountTokensRequestAnthropicAPIKeyPassthrough(
		context.Background(), c, account, []byte(`{"model":"gpt-oss:20b","messages":[]}`), "ollama-key",
	)
	require.NoError(t, err)
	require.Equal(t, "https://ollama.com/v1/messages/count_tokens?beta=true", countReq.URL.String())
	require.Equal(t, "Bearer ollama-key", getHeaderRaw(countReq.Header, "authorization"))
	require.Empty(t, getHeaderRaw(countReq.Header, "x-api-key"))
	require.Empty(t, getHeaderRaw(countReq.Header, "cookie"))
}

// TestGatewayService_AnthropicAPIKeyPassthrough_ModelMappingEdgeCases 覆盖透传模式下模型映射的各种边界情况
func TestGatewayService_AnthropicAPIKeyPassthrough_ModelMappingEdgeCases(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name          string
		model         string
		modelMapping  map[string]any // nil = 不配置映射
		expectedModel string
		endpoint      string // "messages" or "count_tokens"
	}{
		{
			name:          "Forward: 无映射配置时不改写模型",
			model:         "claude-sonnet-4-20250514",
			modelMapping:  nil,
			expectedModel: "claude-sonnet-4-20250514",
			endpoint:      "messages",
		},
		{
			name:          "Forward: 空映射配置时不改写模型",
			model:         "claude-sonnet-4-20250514",
			modelMapping:  map[string]any{},
			expectedModel: "claude-sonnet-4-20250514",
			endpoint:      "messages",
		},
		{
			name:          "Forward: 模型不在映射表中时不改写",
			model:         "claude-sonnet-4-20250514",
			modelMapping:  map[string]any{"claude-3-haiku-20240307": "claude-3-opus-20240229"},
			expectedModel: "claude-sonnet-4-20250514",
			endpoint:      "messages",
		},
		{
			name:          "Forward: 精确匹配映射应改写模型",
			model:         "claude-sonnet-4-20250514",
			modelMapping:  map[string]any{"claude-sonnet-4-20250514": "claude-sonnet-4-5-20241022"},
			expectedModel: "claude-sonnet-4-5-20241022",
			endpoint:      "messages",
		},
		{
			name:          "Forward: 通配符映射应改写模型",
			model:         "claude-sonnet-4-20250514",
			modelMapping:  map[string]any{"claude-sonnet-4-*": "claude-sonnet-4-5-20241022"},
			expectedModel: "claude-sonnet-4-5-20241022",
			endpoint:      "messages",
		},
		{
			name:          "CountTokens: 无映射配置时不改写模型",
			model:         "claude-sonnet-4-20250514",
			modelMapping:  nil,
			expectedModel: "claude-sonnet-4-20250514",
			endpoint:      "count_tokens",
		},
		{
			name:          "CountTokens: 模型不在映射表中时不改写",
			model:         "claude-sonnet-4-20250514",
			modelMapping:  map[string]any{"claude-3-haiku-20240307": "claude-3-opus-20240229"},
			expectedModel: "claude-sonnet-4-20250514",
			endpoint:      "count_tokens",
		},
		{
			name:          "CountTokens: 精确匹配映射应改写模型",
			model:         "claude-sonnet-4-20250514",
			modelMapping:  map[string]any{"claude-sonnet-4-20250514": "claude-sonnet-4-5-20241022"},
			expectedModel: "claude-sonnet-4-5-20241022",
			endpoint:      "count_tokens",
		},
		{
			name:          "CountTokens: 通配符映射应改写模型",
			model:         "claude-sonnet-4-20250514",
			modelMapping:  map[string]any{"claude-sonnet-4-*": "claude-sonnet-4-5-20241022"},
			expectedModel: "claude-sonnet-4-5-20241022",
			endpoint:      "count_tokens",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)

			body := []byte(`{"model":"` + tt.model + `","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
			parsed := &ParsedRequest{
				Body:  NewRequestBodyRef(body),
				Model: tt.model,
			}

			credentials := map[string]any{
				"api_key":  "upstream-key",
				"base_url": "https://api.anthropic.com",
			}
			if tt.modelMapping != nil {
				credentials["model_mapping"] = tt.modelMapping
			}

			account := &Account{
				ID:          300,
				Name:        "edge-case-test",
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Concurrency: 1,
				Credentials: credentials,
				Extra:       map[string]any{"anthropic_passthrough": true},
				Status:      StatusActive,
				Schedulable: true,
			}

			if tt.endpoint == "messages" {
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				parsed.Stream = false

				upstreamJSON := `{"id":"msg_1","type":"message","usage":{"input_tokens":5,"output_tokens":3}}`
				upstream := &anthropicHTTPUpstreamRecorder{
					resp: &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(upstreamJSON)),
					},
				}
				svc := &GatewayService{
					cfg:              &config.Config{},
					httpUpstream:     upstream,
					rateLimitService: &RateLimitService{},
				}

				result, err := svc.Forward(context.Background(), c, account, parsed)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, tt.expectedModel, gjson.GetBytes(upstream.lastBody, "model").String(),
					"Forward 上游请求体中的模型应为: %s", tt.expectedModel)
			} else {
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)

				upstreamRespBody := `{"input_tokens":42}`
				upstream := &anthropicHTTPUpstreamRecorder{
					resp: &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(upstreamRespBody)),
					},
				}
				svc := &GatewayService{
					cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
					httpUpstream:     upstream,
					rateLimitService: &RateLimitService{},
				}

				err := svc.ForwardCountTokens(context.Background(), c, account, parsed)
				require.NoError(t, err)
				require.Equal(t, tt.expectedModel, gjson.GetBytes(upstream.lastBody, "model").String(),
					"CountTokens 上游请求体中的模型应为: %s", tt.expectedModel)
			}
		})
	}
}

// TestGatewayService_AnthropicAPIKeyPassthrough_ModelMappingPreservesOtherFields
// 确保模型映射只替换 model 字段，不影响请求体中的其他字段
func TestGatewayService_AnthropicAPIKeyPassthrough_ModelMappingPreservesOtherFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)

	// 包含复杂字段的请求体：system、thinking、messages
	body := []byte(`{"model":"claude-sonnet-4-20250514","system":[{"type":"text","text":"You are a helpful assistant."}],"messages":[{"role":"user","content":[{"type":"text","text":"hello world"}]}],"thinking":{"type":"enabled","budget_tokens":5000},"max_tokens":1024}`)
	parsed := &ParsedRequest{
		Body:  NewRequestBodyRef(body),
		Model: "claude-sonnet-4-20250514",
	}

	upstreamRespBody := `{"input_tokens":42}`
	upstream := &anthropicHTTPUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(upstreamRespBody)),
		},
	}

	svc := &GatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		httpUpstream:     upstream,
		rateLimitService: &RateLimitService{},
	}

	account := &Account{
		ID:          301,
		Name:        "preserve-fields-test",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":       "upstream-key",
			"base_url":      "https://api.anthropic.com",
			"model_mapping": map[string]any{"claude-sonnet-4-20250514": "claude-sonnet-4-5-20241022"},
		},
		Extra:       map[string]any{"anthropic_passthrough": true},
		Status:      StatusActive,
		Schedulable: true,
	}

	err := svc.ForwardCountTokens(context.Background(), c, account, parsed)
	require.NoError(t, err)

	sentBody := upstream.lastBody
	require.Equal(t, "claude-sonnet-4-5-20241022", gjson.GetBytes(sentBody, "model").String(), "model 应被映射")
	require.Equal(t, "You are a helpful assistant.", gjson.GetBytes(sentBody, "system.0.text").String(), "system 字段不应被修改")
	require.Equal(t, "hello world", gjson.GetBytes(sentBody, "messages.0.content.0.text").String(), "messages 字段不应被修改")
	require.Equal(t, "enabled", gjson.GetBytes(sentBody, "thinking.type").String(), "thinking 字段不应被修改")
	require.Equal(t, int64(5000), gjson.GetBytes(sentBody, "thinking.budget_tokens").Int(), "thinking.budget_tokens 不应被修改")
	require.False(t, gjson.GetBytes(sentBody, "max_tokens").Exists(),
		"max_tokens 作为生成参数应被 count_tokens 过滤剥离")
}

func TestGatewayService_AnthropicAPIKeyPassthrough_CountTokensFiltersGenerationFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)

	body := []byte(`{"model":"claude-sonnet-4-20250514","system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":"hello"}],"tools":[{"name":"tool","input_schema":{"type":"object"}}],"temperature":0.7,"top_p":0.9,"top_k":40,"stream":true,"stop_sequences":["END"],"max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":5000}}`)
	parsed := &ParsedRequest{
		Body:  NewRequestBodyRef(body),
		Model: "claude-sonnet-4-20250514",
	}

	upstreamRespBody := `{"input_tokens":42}`
	upstream := &anthropicHTTPUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(upstreamRespBody)),
		},
	}

	svc := &GatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		httpUpstream:     upstream,
		rateLimitService: &RateLimitService{},
	}

	account := &Account{
		ID:          302,
		Name:        "count-token-filter-test",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "upstream-key",
			"base_url": "https://api.anthropic.com",
		},
		Extra:       map[string]any{"anthropic_passthrough": true},
		Status:      StatusActive,
		Schedulable: true,
	}

	err := svc.ForwardCountTokens(context.Background(), c, account, parsed)
	require.NoError(t, err)

	sentBody := upstream.lastBody
	require.False(t, gjson.GetBytes(sentBody, "temperature").Exists())
	require.False(t, gjson.GetBytes(sentBody, "top_p").Exists())
	require.False(t, gjson.GetBytes(sentBody, "top_k").Exists())
	require.False(t, gjson.GetBytes(sentBody, "stream").Exists())
	require.False(t, gjson.GetBytes(sentBody, "stop_sequences").Exists())
	require.Equal(t, "claude-sonnet-4-20250514", gjson.GetBytes(sentBody, "model").String())
	require.Equal(t, "sys", gjson.GetBytes(sentBody, "system.0.text").String())
	require.Equal(t, "hello", gjson.GetBytes(sentBody, "messages.0.content").String())
	require.Equal(t, "tool", gjson.GetBytes(sentBody, "tools.0.name").String())
	require.False(t, gjson.GetBytes(sentBody, "max_tokens").Exists(),
		"count_tokens 请求不得携带生成参数 max_tokens")
	require.Equal(t, "enabled", gjson.GetBytes(sentBody, "thinking.type").String())
}

// TestGatewayService_AnthropicAPIKeyPassthrough_EmptyModelSkipsMapping
// 确保空模型名不会触发映射逻辑
func TestGatewayService_AnthropicAPIKeyPassthrough_EmptyModelSkipsMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)

	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	parsed := &ParsedRequest{
		Body:  NewRequestBodyRef(body),
		Model: "", // 空模型
	}

	upstreamRespBody := `{"input_tokens":10}`
	upstream := &anthropicHTTPUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(upstreamRespBody)),
		},
	}

	svc := &GatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		httpUpstream:     upstream,
		rateLimitService: &RateLimitService{},
	}

	account := &Account{
		ID:          302,
		Name:        "empty-model-test",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":       "upstream-key",
			"base_url":      "https://api.anthropic.com",
			"model_mapping": map[string]any{"*": "claude-3-opus-20240229"},
		},
		Extra:       map[string]any{"anthropic_passthrough": true},
		Status:      StatusActive,
		Schedulable: true,
	}

	err := svc.ForwardCountTokens(context.Background(), c, account, parsed)
	require.NoError(t, err)
	// 空模型名时，body 应原样透传，不应触发映射
	require.Equal(t, body, upstream.lastBody, "空模型名时请求体不应被修改")
}

func TestGatewayService_AnthropicAPIKeyPassthrough_CountTokens404PassthroughNotError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name            string
		statusCode      int
		respBody        string
		wantPassthrough bool
	}{
		{
			name:            "404 endpoint not found passes through as 404",
			statusCode:      http.StatusNotFound,
			respBody:        `{"error":{"message":"Not found: /v1/messages/count_tokens","type":"not_found_error"}}`,
			wantPassthrough: true,
		},
		{
			name:            "404 generic not found does not passthrough",
			statusCode:      http.StatusNotFound,
			respBody:        `{"error":{"message":"resource not found","type":"not_found_error"}}`,
			wantPassthrough: false,
		},
		{
			name:            "400 Invalid URL does not passthrough",
			statusCode:      http.StatusBadRequest,
			respBody:        `{"error":{"message":"Invalid URL (POST /v1/messages/count_tokens)","type":"invalid_request_error"}}`,
			wantPassthrough: false,
		},
		{
			name:            "400 model error does not passthrough",
			statusCode:      http.StatusBadRequest,
			respBody:        `{"error":{"message":"model not found: claude-unknown","type":"invalid_request_error"}}`,
			wantPassthrough: false,
		},
		{
			name:            "500 internal error does not passthrough",
			statusCode:      http.StatusInternalServerError,
			respBody:        `{"error":{"message":"internal error","type":"api_error"}}`,
			wantPassthrough: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)

			body := []byte(`{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"hi"}]}`)
			parsed := &ParsedRequest{Body: NewRequestBodyRef(body), Model: "claude-sonnet-4-5-20250929"}

			upstream := &anthropicHTTPUpstreamRecorder{
				resp: &http.Response{
					StatusCode: tt.statusCode,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(tt.respBody)),
				},
			}

			svc := &GatewayService{
				cfg: &config.Config{
					Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize},
				},
				httpUpstream:     upstream,
				rateLimitService: nil,
			}

			account := &Account{
				ID:          200,
				Name:        "proxy-acc",
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Concurrency: 1,
				Credentials: map[string]any{
					"api_key":  "sk-proxy",
					"base_url": "https://proxy.example.com",
				},
				Extra:       map[string]any{"anthropic_passthrough": true},
				Status:      StatusActive,
				Schedulable: true,
			}

			err := svc.ForwardCountTokens(context.Background(), c, account, parsed)

			if tt.wantPassthrough {
				// 返回 nil（不记录为错误），HTTP 状态码 404 + Anthropic 错误体
				require.NoError(t, err)
				require.Equal(t, http.StatusNotFound, rec.Code)
				var errResp map[string]any
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
				require.Equal(t, "error", errResp["type"])
				errObj, ok := errResp["error"].(map[string]any)
				require.True(t, ok)
				require.Equal(t, "not_found_error", errObj["type"])
			} else {
				require.Error(t, err)
				require.Equal(t, tt.statusCode, rec.Code)
			}
		})
	}
}

func TestGatewayService_AnthropicAPIKeyPassthrough_BuildRequestRejectsInvalidBaseURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	svc := &GatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{
					Enabled: false,
				},
			},
		},
	}
	account := &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "k",
			"base_url": "://invalid-url",
		},
	}

	_, _, err := svc.buildUpstreamRequestAnthropicAPIKeyPassthrough(context.Background(), c, account, []byte(`{}`), "k")
	require.Error(t, err)
}

func TestGatewayService_AnthropicAPIKeyPassthrough_StripsDeferredToolCacheControl(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	svc := &GatewayService{cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}}}
	account := &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
	body := []byte(`{"tools":[{"name":"deferred","custom":{"defer_loading":true},"cache_control":{"type":"ephemeral"}},{"name":"top-level-deferred","defer_loading":true,"cache_control":{"type":"ephemeral"}},{"name":"ordinary","defer_loading":false,"cache_control":{"type":"ephemeral"}},{"name":"malformed","defer_loading":"true","cache_control":{"type":"ephemeral"}}]}`)

	_, wireBody, err := svc.buildUpstreamRequestAnthropicAPIKeyPassthrough(context.Background(), c, account, body, "k")
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(wireBody, "tools.0.cache_control").Exists())
	require.False(t, gjson.GetBytes(wireBody, "tools.1.cache_control").Exists())
	require.True(t, gjson.GetBytes(wireBody, "tools.2.cache_control").Exists())
	require.True(t, gjson.GetBytes(wireBody, "tools.3.cache_control").Exists())

	countReq, err := svc.buildCountTokensRequestAnthropicAPIKeyPassthrough(context.Background(), c, account, body, "k")
	require.NoError(t, err)
	countBody, err := io.ReadAll(countReq.Body)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(countBody, "tools.0.cache_control").Exists())
	require.False(t, gjson.GetBytes(countBody, "tools.1.cache_control").Exists())
	require.True(t, gjson.GetBytes(countBody, "tools.2.cache_control").Exists())
	require.True(t, gjson.GetBytes(countBody, "tools.3.cache_control").Exists())
}

func TestGatewayService_AnthropicOAuth_NotAffectedByAPIKeyPassthroughToggle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	svc := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize},
		},
	}
	account := &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"anthropic_passthrough": true,
		},
	}

	require.False(t, account.IsAnthropicAPIKeyPassthroughEnabled())

	req, _, err := svc.buildUpstreamRequest(context.Background(), c, account, []byte(`{"model":"claude-3-7-sonnet-20250219"}`), "oauth-token", "oauth", "claude-3-7-sonnet-20250219", true, false)
	require.NoError(t, err)
	require.Equal(t, "Bearer oauth-token", getHeaderRaw(req.Header, "authorization"))
	require.Contains(t, getHeaderRaw(req.Header, "anthropic-beta"), claude.BetaOAuth, "OAuth 链路仍应按原逻辑补齐 oauth beta")
}

func TestGatewayService_AnthropicOAuthMimic_RewritesSystemWithBillingBlock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	haikuSessionID := "123e4567-e89b-42d3-a456-426614174000"
	haikuMetadataUserID := FormatMetadataUserID(
		"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		"550e8400-e29b-41d4-a716-446655440000",
		haikuSessionID,
		claude.CLICurrentVersion,
	)

	tests := []struct {
		name                       string
		body                       string
		wantModel                  string
		wantOriginalSystem         string
		wantOriginalSystemCacheTTL string
		wantMetadataUserID         string
		wantSessionID              string
	}{
		{
			name:               "system array",
			body:               `{"model":"claude-3-5-sonnet-latest","system":[{"type":"text","text":"x-anthropic-billing-header keep"}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`,
			wantModel:          "claude-3-5-sonnet-latest",
			wantOriginalSystem: "x-anthropic-billing-header keep",
		},
		{
			name:               "system string",
			body:               `{"model":"claude-3-5-sonnet-latest","system":"x-anthropic-billing-header keep","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`,
			wantModel:          "claude-3-5-sonnet-latest",
			wantOriginalSystem: "x-anthropic-billing-header keep",
		},
		{
			name:                       "haiku full mimicry",
			body:                       `{"model":"claude-haiku-4-5","metadata":{"user_id":` + strconvQuote(haikuMetadataUserID) + `},"system":[{"type":"text","text":"Pi project instructions","cache_control":{"type":"ephemeral","ttl":"1h"}}],"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`,
			wantModel:                  "claude-haiku-4-5-20251001",
			wantOriginalSystem:         "Pi project instructions",
			wantOriginalSystemCacheTTL: "1h",
			wantMetadataUserID:         haikuMetadataUserID,
			wantSessionID:              haikuSessionID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request.Header.Set("User-Agent", "pi/0.51.0")
			c.Request.Header.Set("Anthropic-Beta", claude.BetaFineGrainedToolStreaming+",prompt-caching-2024-07-31")

			parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(tt.body)), PlatformAnthropic)
			require.NoError(t, err)

			upstream := &anthropicHTTPUpstreamRecorder{
				resp: &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type": []string{"application/json"},
						"x-request-id": []string{"rid-oauth-preserve"},
					},
					Body: io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":12,"output_tokens":7}}`)),
				},
			}

			cfg := &config.Config{
				Gateway: config.GatewayConfig{
					MaxLineSize: defaultMaxLineSize,
				},
			}
			gatewayForwardingSF.Forget("gateway_forwarding")
			gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{})
			t.Cleanup(func() {
				gatewayForwardingSF.Forget("gateway_forwarding")
				gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{})
			})
			svc := &GatewayService{
				cfg:                  cfg,
				responseHeaderFilter: compileResponseHeaderFilter(cfg),
				httpUpstream:         upstream,
				rateLimitService:     &RateLimitService{},
				deferredService:      &DeferredService{},
				settingService: NewSettingService(&gatewayTTLSettingRepo{data: map[string]string{
					SettingKeyEnableCCHSigning: "true",
				}}, cfg),
			}

			account := &Account{
				ID:          301,
				Name:        "anthropic-oauth-preserve",
				Platform:    PlatformAnthropic,
				Type:        AccountTypeOAuth,
				Concurrency: 1,
				Credentials: map[string]any{
					"access_token": "oauth-token",
				},
				Status:      StatusActive,
				Schedulable: true,
			}

			result, err := svc.Forward(context.Background(), c, account, parsed)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.NotNil(t, upstream.lastReq)
			require.Equal(t, "Bearer oauth-token", getHeaderRaw(upstream.lastReq.Header, "authorization"))
			finalBeta := getHeaderRaw(upstream.lastReq.Header, "anthropic-beta")
			for _, beta := range claude.FullClaudeCodeMimicryBetas() {
				require.Truef(t, anthropicBetaTokensContains(finalBeta, beta), "missing mimic beta %s", beta)
			}
			require.Contains(t, finalBeta, claude.BetaAdvisorTool)
			require.Contains(t, finalBeta, claude.BetaEffort)
			require.NotContains(t, finalBeta, claude.BetaFineGrainedToolStreaming)
			require.NotContains(t, finalBeta, "prompt-caching-2024-07-31")
			for key, value := range claude.DefaultHeaders {
				require.Equal(t, value, getHeaderRaw(upstream.lastReq.Header, key), "mimic fingerprint header %s", key)
			}
			require.NotEmpty(t, getHeaderRaw(upstream.lastReq.Header, "x-client-request-id"))
			if tt.wantSessionID != "" {
				require.Equal(t, tt.wantSessionID, getHeaderRaw(upstream.lastReq.Header, "X-Claude-Code-Session-Id"))
			}

			require.Equal(t, tt.wantModel, gjson.GetBytes(upstream.lastBody, "model").String())
			system := gjson.GetBytes(upstream.lastBody, "system")
			require.True(t, system.Exists())
			require.True(t, system.IsArray(), "system should be an array")
			arr := system.Array()
			require.Len(t, arr, 3, "system array should have billing + identity + expansion blocks")

			require.Contains(t, arr[0].Get("text").String(), "x-anthropic-billing-header:")
			require.Contains(t, arr[0].Get("text").String(), "cc_version=")
			require.Regexp(t, `\bcch=[0-9a-f]{5}\b`, arr[0].Get("text").String())
			require.NotContains(t, arr[0].Get("text").String(), "cch=00000")

			require.Equal(t, claudeCodeSystemPrompt, arr[1].Get("text").String())
			require.False(t, arr[1].Get("cache_control").Exists())
			require.Equal(t, claudeCodeSystemPromptExpansion, arr[2].Get("text").String())
			require.Equal(t, "ephemeral", arr[2].Get("cache_control.type").String())

			// 原始 system prompt 应迁移至 messages 中
			messages := gjson.GetBytes(upstream.lastBody, "messages")
			require.True(t, messages.IsArray())
			firstMsg := messages.Array()[0]
			require.Equal(t, "user", firstMsg.Get("role").String())
			require.Equal(t, "[System Instructions]", firstMsg.Get("content.0.text").String())
			require.Contains(t, firstMsg.Get("content.1.text").String(), tt.wantOriginalSystem)
			if tt.wantOriginalSystemCacheTTL != "" {
				require.Equal(t, "ephemeral", firstMsg.Get("content.1.cache_control.type").String())
				require.Equal(t, tt.wantOriginalSystemCacheTTL, firstMsg.Get("content.1.cache_control.ttl").String())
			} else {
				require.False(t, firstMsg.Get("content.1.cache_control").Exists())
			}
			if tt.wantMetadataUserID != "" {
				require.Equal(t, tt.wantMetadataUserID, gjson.GetBytes(upstream.lastBody, "metadata.user_id").String())
				require.True(t, gjson.GetBytes(upstream.lastBody, "context_management").Exists())
			}
		})
	}
}

func TestGatewayService_AnthropicOAuth_SystemPromptInjectionCanBeDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gatewayForwardingSF.Forget("gateway_forwarding")
	gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{})
	t.Cleanup(func() {
		gatewayForwardingSF.Forget("gateway_forwarding")
		gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{})
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	body := []byte(`{"model":"claude-3-5-sonnet-latest","system":"Original system prompt","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)

	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"x-request-id": []string{"rid-oauth-no-system-injection"},
		},
		Body: io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":12,"output_tokens":7}}`)),
	}}

	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	settingService := NewSettingService(&gatewayTTLSettingRepo{data: map[string]string{
		SettingKeyEnableClaudeOAuthSystemPromptInjection: "false",
	}}, cfg)
	svc := &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     &RateLimitService{},
		deferredService:      &DeferredService{},
		settingService:       settingService,
	}
	account := &Account{
		ID: 302, Name: "anthropic-oauth-no-system-injection", Platform: PlatformAnthropic,
		Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Status:      StatusActive, Schedulable: true,
	}

	result, err := svc.Forward(context.Background(), c, account, parsed)
	require.NoError(t, err)
	require.NotNil(t, result)

	system := gjson.GetBytes(upstream.lastBody, "system")
	require.True(t, system.Exists())
	require.Equal(t, "Original system prompt", system.String())
	require.NotContains(t, string(upstream.lastBody), "x-anthropic-billing-header:")
	require.NotContains(t, string(upstream.lastBody), "[System Instructions]")
}

func TestGatewayService_AnthropicOAuth_ProxiedClaudeCodeBodyKeepsCacheAndLocksWireProfile(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sessionID := "123e4567-e89b-42d3-a456-426614174000"
	metadataUserID := FormatMetadataUserID(
		"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		"550e8400-e29b-41d4-a716-446655440000",
		sessionID,
		"2.1.119",
	)
	body, err := json.Marshal(map[string]any{
		"model":  "claude-sonnet-4-6",
		"stream": false,
		"metadata": map[string]any{
			"user_id": metadataUserID,
		},
		"system": []any{
			map[string]any{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=9.9.9.proxy; cc_entrypoint=cli; cch=00000;",
			},
			map[string]any{
				"type":          "text",
				"text":          "ORIGINAL CLAUDE CODE SYSTEM",
				"cache_control": map[string]any{"type": "ephemeral"},
			},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "NewAPI/relay-replaced")
	c.Request.Header.Set("X-Stainless-Package-Version", "9.9.9")
	c.Request.Header.Set("Anthropic-Beta", claude.BetaFineGrainedToolStreaming)

	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"x-request-id": []string{"rid-proxied-cli"},
		},
		Body: io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":12,"output_tokens":7}}`)),
	}}
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	identityCache := &identityCacheStub{fingerprint: &Fingerprint{
		ClientID: "cached-client-id", UserAgent: "NewAPI/9.9.9",
		StainlessLang: "go", StainlessPackageVersion: "9.9.9",
		StainlessOS: "Other", StainlessArch: "other",
		StainlessRuntime: "other", StainlessRuntimeVersion: "v9.9.9",
	}}
	svc := &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     &RateLimitService{},
		deferredService:      &DeferredService{},
		identityService:      NewIdentityService(identityCache),
	}
	account := &Account{
		ID: 302, Name: "anthropic-oauth-proxied-cli", Platform: PlatformAnthropic,
		Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Status:      StatusActive, Schedulable: true,
	}

	result, err := svc.Forward(context.Background(), c, account, parsed)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq)

	// Body-level detection preserves the real CLI cache layout.
	system := gjson.GetBytes(upstream.lastBody, "system").Array()
	require.Len(t, system, 2)
	require.Equal(t, "ORIGINAL CLAUDE CODE SYSTEM", system[1].Get("text").String())
	require.Equal(t, "ephemeral", system[1].Get("cache_control.type").String())
	require.Contains(t, system[0].Get("text").String(), "cc_version=2.1.119")
	require.NotContains(t, string(upstream.lastBody), "[System Instructions]")

	// Header-level disguise is independent of body preservation: the inbound
	// gateway identity and beta list must not leak to Anthropic.
	require.Equal(t, claude.DefaultHeaders["User-Agent"], getHeaderRaw(upstream.lastReq.Header, "User-Agent"))
	require.Equal(t, claude.DefaultHeaders["X-Stainless-Package-Version"], getHeaderRaw(upstream.lastReq.Header, "X-Stainless-Package-Version"))
	require.Equal(t, claude.DefaultHeaders["X-Stainless-Runtime-Version"], getHeaderRaw(upstream.lastReq.Header, "X-Stainless-Runtime-Version"))
	require.Equal(t, sessionID, getHeaderRaw(upstream.lastReq.Header, "X-Claude-Code-Session-Id"))
	beta := getHeaderRaw(upstream.lastReq.Header, "Anthropic-Beta")
	require.Contains(t, beta, claude.BetaAdvisorTool)
	require.Contains(t, beta, claude.BetaEffort)
	require.NotContains(t, beta, claude.BetaFineGrainedToolStreaming)

	// A proxied first request must repair, rather than poison, the account cache.
	require.NotNil(t, identityCache.fingerprint)
	require.Equal(t, "cached-client-id", identityCache.fingerprint.ClientID)
	require.Equal(t, claude.DefaultHeaders["User-Agent"], identityCache.fingerprint.UserAgent)
	require.Equal(t, claude.DefaultHeaders["X-Stainless-Package-Version"], identityCache.fingerprint.StainlessPackageVersion)

	// Reuse the same cache/account with a directly detected CLI request. Even a
	// newer inbound CLI fingerprint must not revive the previously cached proxy
	// identity or drift from the coordinated relay profile.
	directSessionID := "987e6543-e21b-42d3-a456-426614174999"
	directUserID := FormatMetadataUserID(
		"f1e2d3c4b5a6f1e2d3c4b5a6f1e2d3c4b5a6f1e2d3c4b5a6f1e2d3c4b5a6f1e2",
		"550e8400-e29b-41d4-a716-446655440000",
		directSessionID,
		"9.9.9",
	)
	directBody, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"metadata": map[string]any{
			"user_id": directUserID,
		},
		"system": []any{
			map[string]any{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=9.9.9.direct; cc_entrypoint=cli; cch=00000;",
			},
			map[string]any{
				"type":          "text",
				"text":          "DIRECT CLAUDE CODE SYSTEM",
				"cache_control": map[string]any{"type": "ephemeral"},
			},
		},
		"messages": []any{map[string]any{"role": "user", "content": "hello again"}},
	})
	require.NoError(t, err)
	directParsed, err := ParseGatewayRequest(NewRequestBodyRef(directBody), PlatformAnthropic)
	require.NoError(t, err)
	directRec := httptest.NewRecorder()
	directCtx, _ := gin.CreateTestContext(directRec)
	directCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	directCtx.Request.Header.Set("User-Agent", "claude-cli/9.9.9 (external, sdk-cli)")
	directCtx.Request.Header.Set("X-Stainless-Package-Version", "9.9.9")
	upstream.resp = &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"msg_2","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":8,"output_tokens":4}}`)),
	}

	result, err = svc.Forward(context.Background(), directCtx, account, directParsed)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, claude.DefaultHeaders["User-Agent"], getHeaderRaw(upstream.lastReq.Header, "User-Agent"))
	require.Equal(t, claude.DefaultHeaders["X-Stainless-Package-Version"], getHeaderRaw(upstream.lastReq.Header, "X-Stainless-Package-Version"))
	require.Equal(t, directSessionID, getHeaderRaw(upstream.lastReq.Header, "X-Claude-Code-Session-Id"))
	require.Contains(t, gjson.GetBytes(upstream.lastBody, "system.0.text").String(), "cc_version=2.1.119")
	require.Equal(t, "cached-client-id", identityCache.fingerprint.ClientID)
	require.Equal(t, claude.DefaultHeaders["User-Agent"], identityCache.fingerprint.UserAgent)
}

func TestGatewayService_AnthropicOAuth_ProxiedClaudeCodeCountTokensLocksWireProfileAndDropsClientBeta(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sessionID := "123e4567-e89b-42d3-a456-426614174111"
	metadataUserID := FormatMetadataUserID(
		"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		"550e8400-e29b-41d4-a716-446655440000",
		sessionID,
		"2.1.119",
	)
	body, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"metadata": map[string]any{
			"user_id": metadataUserID,
		},
		"system": []any{
			map[string]any{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=9.9.9.proxy; cc_entrypoint=cli; cch=00000;",
			},
			map[string]any{
				"type":          "text",
				"text":          "ORIGINAL COUNT TOKENS SYSTEM",
				"cache_control": map[string]any{"type": "ephemeral"},
			},
		},
		"messages": []any{map[string]any{"role": "user", "content": "count me"}},
	})
	require.NoError(t, err)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
	c.Request.Header.Set("User-Agent", "NewAPI/relay-replaced")
	c.Request.Header.Set("X-Stainless-Package-Version", "9.9.9")
	c.Request.Header.Set("Anthropic-Beta", claude.BetaFineGrainedToolStreaming+",prompt-caching-2024-07-31")

	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"input_tokens":42}`)),
	}}
	identityCache := &identityCacheStub{}
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	svc := &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     &RateLimitService{},
		identityService:      NewIdentityService(identityCache),
	}
	account := &Account{
		ID: 303, Name: "anthropic-oauth-proxied-count", Platform: PlatformAnthropic,
		Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Status:      StatusActive, Schedulable: true,
	}

	err = svc.ForwardCountTokens(context.Background(), c, account, parsed)
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)

	system := gjson.GetBytes(upstream.lastBody, "system").Array()
	require.Len(t, system, 2)
	require.Equal(t, "ORIGINAL COUNT TOKENS SYSTEM", system[1].Get("text").String())
	require.Equal(t, "ephemeral", system[1].Get("cache_control.type").String())
	require.Contains(t, system[0].Get("text").String(), "cc_version=2.1.119")
	require.Equal(t, claude.DefaultHeaders["User-Agent"], getHeaderRaw(upstream.lastReq.Header, "User-Agent"))
	require.Equal(t, claude.DefaultHeaders["X-Stainless-Package-Version"], getHeaderRaw(upstream.lastReq.Header, "X-Stainless-Package-Version"))
	require.Equal(t, claude.DefaultHeaders["X-Stainless-Runtime-Version"], getHeaderRaw(upstream.lastReq.Header, "X-Stainless-Runtime-Version"))
	require.Equal(t, sessionID, getHeaderRaw(upstream.lastReq.Header, "X-Claude-Code-Session-Id"))
	beta := getHeaderRaw(upstream.lastReq.Header, "Anthropic-Beta")
	require.Contains(t, beta, claude.BetaAdvisorTool)
	require.Contains(t, beta, claude.BetaEffort)
	require.Contains(t, beta, claude.BetaTokenCounting)
	require.NotContains(t, beta, claude.BetaFineGrainedToolStreaming)
	require.NotContains(t, beta, "prompt-caching-2024-07-31")
}

func TestGatewayService_AnthropicAPIKeyPassthrough_StreamingStillCollectsUsageAfterClientDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Use a canceled context recorder to simulate client disconnect behavior.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	svc := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				MaxLineSize: defaultMaxLineSize,
			},
		},
		rateLimitService: &RateLimitService{},
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}`,
			"",
			`data: {"type":"message_delta","usage":{"output_tokens":5}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n"))),
	}

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, &Account{ID: 1}, time.Now(), "claude-3-7-sonnet-20250219")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.usage)
	require.Equal(t, 11, result.usage.InputTokens)
	require.Equal(t, 5, result.usage.OutputTokens)
}

func TestGatewayService_AnthropicAPIKeyPassthrough_MissingTerminalEventReturnsError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	svc := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				MaxLineSize: defaultMaxLineSize,
			},
		},
		rateLimitService: &RateLimitService{},
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}`,
			"",
			`data: {"type":"message_delta","usage":{"output_tokens":5}}`,
			"",
		}, "\n"))),
	}

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, &Account{ID: 1}, time.Now(), "claude-3-7-sonnet-20250219")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing terminal event")
	require.NotNil(t, result)
}

func TestGatewayService_AnthropicAPIKeyPassthrough_ForwardDirect_NonStreamingSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	body := []byte(`{"model":"claude-3-5-sonnet-latest","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	upstreamJSON := `{"id":"msg_1","type":"message","usage":{"input_tokens":12,"output_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":3},"cached_tokens":4}}`
	upstream := &anthropicHTTPUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
				"x-request-id": []string{"rid-nonstream"},
			},
			Body: io.NopCloser(strings.NewReader(upstreamJSON)),
		},
	}
	svc := &GatewayService{
		cfg:              &config.Config{},
		httpUpstream:     upstream,
		rateLimitService: &RateLimitService{},
	}

	result, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, newAnthropicAPIKeyAccountForTest(), body, "claude-3-5-sonnet-latest", "claude-3-5-sonnet-latest", false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 7, result.Usage.OutputTokens)
	require.Equal(t, 5, result.Usage.CacheCreationInputTokens)
	require.Equal(t, 4, result.Usage.CacheReadInputTokens)
	require.Equal(t, upstreamJSON, rec.Body.String())
}

func TestGatewayService_AnthropicAPIKeyPassthrough_ForceCacheBillingResponse(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "converts input tokens for downstream billing",
			body: `{"id":"msg_1","type":"message","content":[{"type":"text","text":"unchanged"}],"usage":{"input_tokens":5,"output_tokens":3}}`,
			want: `{"id":"msg_1","type":"message","content":[{"type":"text","text":"unchanged"}],"usage":{"input_tokens":0,"output_tokens":3,"cache_read_input_tokens":5}}`,
		},
		{
			name: "adds to genuine cache reads",
			body: `{"id":"msg_2","type":"message","usage":{"input_tokens":5,"output_tokens":3,"cache_read_input_tokens":7,"cache_creation_input_tokens":11}}`,
			want: `{"id":"msg_2","type":"message","usage":{"input_tokens":0,"output_tokens":3,"cache_read_input_tokens":12,"cache_creation_input_tokens":11}}`,
		},
		{
			name: "zero input leaves response unchanged",
			body: `{"id":"msg_3","type":"message","usage":{"input_tokens":0,"output_tokens":3,"cache_read_input_tokens":7}}`,
			want: `{"id":"msg_3","type":"message","usage":{"input_tokens":0,"output_tokens":3,"cache_read_input_tokens":7}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(tt.body)),
			}
			svc := &GatewayService{cfg: &config.Config{}}

			usage, err := svc.handleNonStreamingResponseAnthropicAPIKeyPassthrough(
				WithForceCacheBilling(context.Background()),
				resp,
				c,
				&Account{ID: 2},
			)

			require.NoError(t, err)
			require.Equal(t, int(gjson.Get(tt.body, "usage.input_tokens").Int()), usage.InputTokens)
			require.Equal(t, int(gjson.Get(tt.body, "usage.cache_read_input_tokens").Int()), usage.CacheReadInputTokens)
			require.JSONEq(t, tt.want, rec.Body.String())
		})
	}
}

func TestGatewayService_AnthropicAPIKeyPassthrough_ForwardDirect_InvalidTokenType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	account := &Account{
		ID:       202,
		Name:     "anthropic-oauth",
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "oauth-token",
		},
	}
	svc := &GatewayService{}

	result, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, account, []byte(`{}`), "claude-3-5-sonnet-latest", "claude-3-5-sonnet-latest", false, time.Now())
	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires apikey token")
}

func TestGatewayService_AnthropicAPIKeyPassthrough_ForwardDirect_UpstreamRequestError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	upstream := &anthropicHTTPUpstreamRecorder{
		err: errors.New("dial tcp timeout"),
	}
	svc := &GatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false},
			},
		},
		httpUpstream: upstream,
	}
	account := newAnthropicAPIKeyAccountForTest()

	result, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, account, []byte(`{"model":"x"}`), "x", "x", false, time.Now())
	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream request failed")
	require.Equal(t, http.StatusBadGateway, rec.Code)
	rawBody, ok := c.Get(OpsUpstreamRequestBodyKey)
	require.True(t, ok)
	_, ok = rawBody.([]byte)
	require.True(t, ok)
}

func TestGatewayService_AnthropicAPIKeyPassthrough_ForwardDirect_EmptyResponseBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	upstream := &anthropicHTTPUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"x-request-id": []string{"rid-empty-body"}},
			Body:       nil,
		},
	}
	svc := &GatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false},
			},
		},
		httpUpstream: upstream,
	}

	result, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, newAnthropicAPIKeyAccountForTest(), []byte(`{"model":"x"}`), "x", "x", false, time.Now())
	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty response")
}

func TestExtractAnthropicSSEDataLine(t *testing.T) {
	t.Run("valid data line with spaces", func(t *testing.T) {
		data, ok := extractAnthropicSSEDataLine("data:   {\"type\":\"message_start\"}")
		require.True(t, ok)
		require.Equal(t, `{"type":"message_start"}`, data)
	})

	t.Run("non data line", func(t *testing.T) {
		data, ok := extractAnthropicSSEDataLine("event: message_start")
		require.False(t, ok)
		require.Empty(t, data)
	})
}

func TestGatewayService_ParseSSEUsagePassthrough_MessageStartFallbacks(t *testing.T) {
	usage := &ClaudeUsage{}
	data := `{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cached_tokens":9,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":4}}}}`

	parseSSEUsagePassthrough(data, usage)

	require.Equal(t, 12, usage.InputTokens)
	require.Equal(t, 9, usage.CacheReadInputTokens, "应兼容 cached_tokens 字段")
	require.Equal(t, 7, usage.CacheCreationInputTokens, "聚合字段为空时应从 5m/1h 明细回填")
	require.Equal(t, 3, usage.CacheCreation5mTokens)
	require.Equal(t, 4, usage.CacheCreation1hTokens)
}

func TestGatewayService_ParseSSEUsagePassthrough_MessageDeltaSelectiveOverwrite(t *testing.T) {
	usage := &ClaudeUsage{
		InputTokens:           10,
		CacheCreation5mTokens: 2,
		CacheCreation1hTokens: 6,
	}
	data := `{"type":"message_delta","usage":{"input_tokens":0,"output_tokens":5,"cache_creation_input_tokens":8,"cache_read_input_tokens":0,"cached_tokens":11,"cache_creation":{"ephemeral_5m_input_tokens":1,"ephemeral_1h_input_tokens":0}}}`

	parseSSEUsagePassthrough(data, usage)

	require.Equal(t, 10, usage.InputTokens, "message_delta 中 0 值不应覆盖已有 input_tokens")
	require.Equal(t, 5, usage.OutputTokens)
	require.Equal(t, 8, usage.CacheCreationInputTokens)
	require.Equal(t, 11, usage.CacheReadInputTokens, "cache_read_input_tokens 为空时应回退到 cached_tokens")
	require.Equal(t, 1, usage.CacheCreation5mTokens)
	require.Equal(t, 6, usage.CacheCreation1hTokens, "message_delta 中 0 值不应覆盖已有 1h 明细")
}

func TestGatewayService_ParseSSEUsagePassthrough_NoopCases(t *testing.T) {

	usage := &ClaudeUsage{InputTokens: 3}
	parseSSEUsagePassthrough("", usage)
	require.Equal(t, 3, usage.InputTokens)

	parseSSEUsagePassthrough("[DONE]", usage)
	require.Equal(t, 3, usage.InputTokens)

	parseSSEUsagePassthrough("not-json", usage)
	require.Equal(t, 3, usage.InputTokens)

	// nil usage 不应 panic
	parseSSEUsagePassthrough(`{"type":"message_start"}`, nil)
}

func TestGatewayService_ParseSSEUsagePassthrough_FallbackFromUsageNode(t *testing.T) {
	usage := &ClaudeUsage{}
	data := `{"type":"content_block_delta","usage":{"cached_tokens":6,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":1}}}`

	parseSSEUsagePassthrough(data, usage)

	require.Equal(t, 6, usage.CacheReadInputTokens)
	require.Equal(t, 3, usage.CacheCreationInputTokens)
}

func TestParseClaudeUsageFromResponseBody(t *testing.T) {
	t.Run("empty or missing usage", func(t *testing.T) {
		got := parseClaudeUsageFromResponseBody(nil)
		require.NotNil(t, got)
		require.Equal(t, 0, got.InputTokens)

		got = parseClaudeUsageFromResponseBody([]byte(`{"id":"x"}`))
		require.NotNil(t, got)
		require.Equal(t, 0, got.OutputTokens)
	})

	t.Run("parse all usage fields and fallback", func(t *testing.T) {
		body := []byte(`{"usage":{"input_tokens":21,"output_tokens":34,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cached_tokens":13,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":8}}}`)
		got := parseClaudeUsageFromResponseBody(body)
		require.Equal(t, 21, got.InputTokens)
		require.Equal(t, 34, got.OutputTokens)
		require.Equal(t, 13, got.CacheReadInputTokens, "cache_read_input_tokens 为空时应回退 cached_tokens")
		require.Equal(t, 13, got.CacheCreationInputTokens, "聚合字段为空时应由 5m/1h 回填")
		require.Equal(t, 5, got.CacheCreation5mTokens)
		require.Equal(t, 8, got.CacheCreation1hTokens)
	})

	t.Run("keep explicit aggregate values", func(t *testing.T) {
		body := []byte(`{"usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":9,"cache_read_input_tokens":7,"cached_tokens":99,"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":5}}}`)
		got := parseClaudeUsageFromResponseBody(body)
		require.Equal(t, 9, got.CacheCreationInputTokens, "已显式提供聚合字段时不应被明细覆盖")
		require.Equal(t, 7, got.CacheReadInputTokens, "已显式提供 cache_read_input_tokens 时不应回退 cached_tokens")
	})
}

func TestGatewayService_AnthropicAPIKeyPassthrough_StreamingErrTooLong(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	svc := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				MaxLineSize: 32,
			},
		},
	}

	// Scanner 初始缓冲为 64KB，构造更长单行触发 bufio.ErrTooLong。
	longLine := "data: " + strings.Repeat("x", 80*1024)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(longLine)),
	}

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, &Account{ID: 2}, time.Now(), "claude-3-7-sonnet-20250219")
	require.Error(t, err)
	require.ErrorIs(t, err, bufio.ErrTooLong)
	require.NotNil(t, result)
}

func TestGatewayService_AnthropicAPIKeyPassthrough_StreamingDataIntervalTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	svc := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				StreamDataIntervalTimeout: 1,
				MaxLineSize:               defaultMaxLineSize,
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

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, &Account{ID: 5}, time.Now(), "claude-3-7-sonnet-20250219")
	_ = pw.Close()
	_ = pr.Close()

	require.Error(t, err)
	require.Contains(t, err.Error(), "stream data interval timeout")
	require.NotNil(t, result)
	require.False(t, result.clientDisconnect)
}

func TestGatewayService_AnthropicAPIKeyPassthrough_StreamingReadError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	svc := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				MaxLineSize: defaultMaxLineSize,
			},
		},
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &streamReadCloser{
			err: io.ErrUnexpectedEOF,
		},
	}

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, &Account{ID: 6}, time.Now(), "claude-3-7-sonnet-20250219")
	require.Error(t, err)
	require.Contains(t, err.Error(), "stream read error")
	require.NotNil(t, result)
	require.False(t, result.clientDisconnect)
}

func TestGatewayService_AnthropicAPIKeyPassthrough_StreamingTimeoutAfterClientDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer}

	svc := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				StreamDataIntervalTimeout: 1,
				MaxLineSize:               defaultMaxLineSize,
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
		_, _ = pw.Write([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":9}}}` + "\n"))
		// 保持上游连接静默，触发数据间隔超时分支。
		time.Sleep(1500 * time.Millisecond)
		_ = pw.Close()
	}()

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, &Account{ID: 7}, time.Now(), "claude-3-7-sonnet-20250219")
	_ = pr.Close()
	<-done

	require.Error(t, err)
	require.Contains(t, err.Error(), "stream usage incomplete after timeout")
	require.NotNil(t, result)
	require.True(t, result.clientDisconnect)
	require.Equal(t, 9, result.usage.InputTokens)
}

func TestGatewayService_AnthropicAPIKeyPassthrough_StreamingContextCanceled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	svc := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				MaxLineSize: defaultMaxLineSize,
			},
		},
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &streamReadCloser{
			err: context.Canceled,
		},
	}

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, &Account{ID: 3}, time.Now(), "claude-3-7-sonnet-20250219")
	require.Error(t, err)
	require.Contains(t, err.Error(), "stream usage incomplete")
	require.NotNil(t, result)
	require.True(t, result.clientDisconnect)
}

func TestGatewayService_AnthropicAPIKeyPassthrough_StreamingUpstreamReadErrorAfterClientDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer}

	svc := &GatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				MaxLineSize: defaultMaxLineSize,
			},
		},
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &streamReadCloser{
			payload: []byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":8}}}` + "\n\n"),
			err:     io.ErrUnexpectedEOF,
		},
	}

	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, &Account{ID: 4}, time.Now(), "claude-3-7-sonnet-20250219")
	require.Error(t, err)
	require.Contains(t, err.Error(), "stream usage incomplete after disconnect")
	require.NotNil(t, result)
	require.True(t, result.clientDisconnect)
	require.Equal(t, 8, result.usage.InputTokens)
}

func TestGatewayService_AnthropicAPIKeyPassthrough_TransportErrorRecordsOllamaActivity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deferred := NewDeferredService(nil, nil, time.Second)
	upstream := &anthropicHTTPUpstreamRecorder{err: errors.New("dial tcp timeout")}
	svc := &GatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false},
			},
		},
		httpUpstream:    upstream,
		deferredService: deferred,
	}

	ollama := &Account{
		ID: 601, Name: "ollama-anthropic", Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "k-ollama", "base_url": "https://ollama.com"},
		Extra:       map[string]any{"anthropic_passthrough": true},
		Status:      StatusActive, Schedulable: true,
	}
	other := newAnthropicAPIKeyAccountForTest()
	other.ID = 602

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	_, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, ollama, []byte(`{"model":"x"}`), "x", "x", false, time.Now())
	require.Error(t, err)

	rec2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(rec2)
	c2.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	_, err = svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c2, other, []byte(`{"model":"x"}`), "x", "x", false, time.Now())
	require.Error(t, err)

	_, ok := deferred.lastUsedUpdates.Load(int64(601))
	require.True(t, ok, "Anthropic passthrough transport error on Ollama account must record activity")
	_, ok = deferred.lastUsedUpdates.Load(int64(602))
	require.False(t, ok, "non-Ollama Anthropic passthrough transport error must not record Ollama activity")
}

func TestGatewayService_AnthropicAPIKeyPassthrough_ContextCanceledSkipsOllamaActivity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deferred := NewDeferredService(nil, nil, time.Second)
	upstream := &anthropicHTTPUpstreamRecorder{err: context.Canceled}
	svc := &GatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false},
			},
		},
		httpUpstream:    upstream,
		deferredService: deferred,
	}
	ollama := &Account{
		ID: 603, Name: "ollama-canceled", Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "k-ollama", "base_url": "https://ollama.com"},
		Extra:       map[string]any{"anthropic_passthrough": true},
		Status:      StatusActive, Schedulable: true,
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err := svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, ollama, []byte(`{"model":"x"}`), "x", "x", false, time.Now())

	require.Error(t, err)
	_, ok := deferred.lastUsedUpdates.Load(int64(603))
	require.False(t, ok, "context.Canceled on Anthropic passthrough must not count as Ollama activity")
}

func TestGatewayService_AnthropicAPIKeyPassthrough_Non2xxRecordsOllamaActivity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deferred := NewDeferredService(nil, nil, time.Second)
	// 400 is non-retryable / non-failover for default API-key accounts, so it reaches handleErrorResponse.
	upstream := &anthropicHTTPUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)),
		},
	}
	svc := &GatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false},
			},
		},
		httpUpstream:     upstream,
		deferredService:  deferred,
		rateLimitService: &RateLimitService{},
	}
	ollama := &Account{
		ID: 604, Name: "ollama-400", Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "k-ollama", "base_url": "https://ollama.com"},
		Extra:       map[string]any{"anthropic_passthrough": true},
		Status:      StatusActive, Schedulable: true,
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, _ = svc.forwardAnthropicAPIKeyPassthrough(context.Background(), c, ollama, []byte(`{"model":"x"}`), "x", "x", false, time.Now())

	_, ok := deferred.lastUsedUpdates.Load(int64(604))
	require.True(t, ok, "Anthropic passthrough non-2xx on Ollama account must record activity via handleErrorResponse")
}
