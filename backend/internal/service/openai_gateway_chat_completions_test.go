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
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeResponsesRequestServiceTier(t *testing.T) {
	t.Parallel()

	req := &apicompat.ResponsesRequest{ServiceTier: " fast "}
	normalizeResponsesRequestServiceTier(req)
	require.Equal(t, "priority", req.ServiceTier)

	req.ServiceTier = "flex"
	normalizeResponsesRequestServiceTier(req)
	require.Equal(t, "flex", req.ServiceTier)

	req.ServiceTier = "default"
	normalizeResponsesRequestServiceTier(req)
	require.Empty(t, req.ServiceTier)
}

func TestNormalizeResponsesBodyServiceTier(t *testing.T) {
	t.Parallel()

	body, tier, err := normalizeResponsesBodyServiceTier([]byte(`{"model":"gpt-5.1","service_tier":"fast"}`))
	require.NoError(t, err)
	require.Equal(t, "priority", tier)
	require.Equal(t, "priority", gjson.GetBytes(body, "service_tier").String())

	body, tier, err = normalizeResponsesBodyServiceTier([]byte(`{"model":"gpt-5.1","service_tier":"flex"}`))
	require.NoError(t, err)
	require.Equal(t, "flex", tier)
	require.Equal(t, "flex", gjson.GetBytes(body, "service_tier").String())

	body, tier, err = normalizeResponsesBodyServiceTier([]byte(`{"model":"gpt-5.1","service_tier":"default"}`))
	require.NoError(t, err)
	require.Empty(t, tier)
	require.False(t, gjson.GetBytes(body, "service_tier").Exists())
}

func TestOpenAIGatewayService_ForwardAsChatCompletions_APIKeyPreservesStructuredOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(nil))
	c.Request.Header.Set("Content-Type", "application/json")

	reqBody := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"Return weather as JSON"}],
		"response_format":{
			"type":"json_schema",
			"json_schema":{
				"name":"weather",
				"strict":true,
				"schema":{
					"type":"object",
					"properties":{"city":{"type":"string"}},
					"required":["city"],
					"additionalProperties":false
				}
			}
		}
	}`)

	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid"}},
			Body: io.NopCloser(bytes.NewBufferString(
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"model\":\"gpt-5.4\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"{\\\"city\\\":\\\"Paris\\\"}\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
					"data: [DONE]\n\n",
			)),
		},
	}

	svc := &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
	}

	account := &Account{
		ID:          1,
		Name:        "apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key": "sk-test",
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, reqBody, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "https://api.openai.com/v1/responses", upstream.lastReq.URL.String())
	require.Equal(t, "json_schema", gjson.GetBytes(upstream.lastBody, "text.format.type").String())
	require.Equal(t, "weather", gjson.GetBytes(upstream.lastBody, "text.format.name").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "text.format.strict").Bool())
	require.Equal(t, "object", gjson.GetBytes(upstream.lastBody, "text.format.schema.type").String())
}

func TestOpenAIGatewayService_ForwardAsChatCompletions_OAuthPreservesStructuredOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(nil))
	c.Request.Header.Set("Content-Type", "application/json")

	reqBody := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"Return weather as JSON"}],
		"response_format":{
			"type":"json_schema",
			"json_schema":{
				"name":"weather",
				"strict":true,
				"schema":{
					"type":"object",
					"properties":{"city":{"type":"string"}},
					"required":["city"],
					"additionalProperties":false
				}
			}
		}
	}`)

	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid"}},
			Body: io.NopCloser(bytes.NewBufferString(
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"model\":\"gpt-5.4\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"{\\\"city\\\":\\\"Paris\\\"}\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
					"data: [DONE]\n\n",
			)),
		},
	}

	svc := &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
	}

	account := &Account{
		ID:          2,
		Name:        "oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, reqBody, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, chatgptCodexURL, upstream.lastReq.URL.String())
	require.Equal(t, "json_schema", gjson.GetBytes(upstream.lastBody, "text.format.type").String())
	require.Equal(t, "weather", gjson.GetBytes(upstream.lastBody, "text.format.name").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "text.format.strict").Bool())
	require.Equal(t, "object", gjson.GetBytes(upstream.lastBody, "text.format.schema.type").String())
}

func TestOpenAIGatewayService_ForwardAsChatCompletions_OAuthPreservesStructuredOutputSchemaOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(nil))
	c.Request.Header.Set("Content-Type", "application/json")

	reqBody := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"Return weather as JSON"}],
		"response_format":{
			"type":"json_schema",
			"json_schema":{
				"name":"weather",
				"strict":true,
				"schema":{
					"zeta":{"type":"string"},
					"alpha":{"type":"string"},
					"mid":{"type":"object","properties":{"k2":{"type":"string"},"k1":{"type":"string"}}},
					"arr":["b","a"]
				}
			}
		}
	}`)

	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid"}},
			Body: io.NopCloser(bytes.NewBufferString(
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"model\":\"gpt-5.4\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"{}\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
					"data: [DONE]\n\n",
			)),
		},
	}

	svc := &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
	}

	account := &Account{
		ID:          2,
		Name:        "oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
		Status:      StatusActive,
		Schedulable: true,
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, reqBody, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	schemaRaw := gjson.GetBytes(upstream.lastBody, "text.format.schema").Raw
	require.NotEqual(t, -1, strings.Index(schemaRaw, `"zeta"`))
	require.NotEqual(t, -1, strings.Index(schemaRaw, `"alpha"`))
	require.NotEqual(t, -1, strings.Index(schemaRaw, `"mid"`))
	require.NotEqual(t, -1, strings.Index(schemaRaw, `"arr"`))
	require.Less(t, strings.Index(schemaRaw, `"zeta"`), strings.Index(schemaRaw, `"alpha"`))
	require.Less(t, strings.Index(schemaRaw, `"alpha"`), strings.Index(schemaRaw, `"mid"`))
	require.Less(t, strings.Index(schemaRaw, `"mid"`), strings.Index(schemaRaw, `"arr"`))
	require.Less(t, strings.Index(schemaRaw, `"k2"`), strings.Index(schemaRaw, `"k1"`))
}
