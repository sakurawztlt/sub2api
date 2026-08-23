package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiForwardAsChatCompletions_OAuthRoutesToGeminiAndReturnsChatFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamBody := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"hello from gemini"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3}}}` + "\n\n" +
		"data: [DONE]\n\n"
	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(upstreamBody)),
		},
	}
	svc := &GeminiMessagesCompatService{
		tokenProvider: &GeminiTokenProvider{},
		httpUpstream:  httpStub,
		cfg:           &config.Config{},
	}
	account := &Account{
		ID:       101,
		Platform: PlatformGemini,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "ya29.test-token",
			"project_id":   "project-1",
		},
		Concurrency: 1,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "gemini-2.5-flash", result.Model)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)

	require.NotNil(t, httpStub.lastReq)
	require.Contains(t, httpStub.lastReq.URL.String(), "/v1internal:streamGenerateContent?alt=sse")
	require.Equal(t, "Bearer ya29.test-token", httpStub.lastReq.Header.Get("Authorization"))
	require.Empty(t, httpStub.lastReq.Header.Get("x-api-key"))
	require.Empty(t, httpStub.lastReq.Header.Get("anthropic-version"))

	var sent map[string]any
	sentBody, err := io.ReadAll(httpStub.lastReq.Body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(sentBody, &sent))
	require.Equal(t, "gemini-2.5-flash", sent["model"])
	require.Equal(t, "project-1", sent["project"])
	require.Contains(t, fmt.Sprint(sent["request"]), "hi")

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "chat.completion", got["object"])
	require.Equal(t, "gemini-2.5-flash", got["model"])
	choices, ok := got["choices"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, choices)
	choice, ok := choices[0].(map[string]any)
	require.True(t, ok)
	message, ok := choice["message"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "assistant", message["role"])
	require.Equal(t, "hello from gemini", message["content"])
	usage, ok := got["usage"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(7), usage["prompt_tokens"])
	require.Equal(t, float64(3), usage["completion_tokens"])
	require.Equal(t, float64(10), usage["total_tokens"])
}

func TestGeminiForwardAsChatCompletions_StreamsOpenAIChunksFromGeminiSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamBody := `data: {"candidates":[{"content":{"parts":[{"text":"hel"}]}}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1}}` + "\n\n" +
		`data: {"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":2}}` + "\n\n" +
		"data: [DONE]\n\n"
	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(upstreamBody)),
		},
	}
	svc := &GeminiMessagesCompatService{
		httpUpstream: httpStub,
		cfg:          &config.Config{},
	}
	account := &Account{
		ID:       102,
		Platform: PlatformGemini,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "gemini-api-key",
		},
		Concurrency: 1,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gemini-2.5-flash","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, result.Stream)
	require.Equal(t, 2, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)

	require.NotNil(t, httpStub.lastReq)
	require.Contains(t, httpStub.lastReq.URL.String(), "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse")
	require.Equal(t, "gemini-api-key", httpStub.lastReq.Header.Get("x-goog-api-key"))

	out := rec.Body.String()
	require.Contains(t, out, `"object":"chat.completion.chunk"`)
	require.Contains(t, out, `"role":"assistant"`)
	require.Contains(t, out, `"content":"hel"`)
	require.Contains(t, out, `"content":"lo"`)
	require.Contains(t, out, `"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}`)
	require.Contains(t, out, "data: [DONE]")
}

func TestGeminiForwardAsChatCompletions_FunctionNamedWebSearchStaysClientSide(t *testing.T) {
	gin.SetMode(gin.TestMode)

	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1}}`,
			)),
		},
	}
	svc := &GeminiMessagesCompatService{
		httpUpstream: httpStub,
		cfg:          &config.Config{},
	}
	account := &Account{
		ID:       103,
		Platform: PlatformGemini,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "gemini-api-key",
		},
		Concurrency: 1,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"user","content":"search and read"}],
		"tools":[
			{"type":"function","function":{"name":"web_search","description":"Search through the Hermes client","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}},
			{"type":"function","function":{"name":"read_file","description":"Read a local file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}
		]
	}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, httpStub.lastReq)

	postedBody, err := io.ReadAll(httpStub.lastReq.Body)
	require.NoError(t, err)

	var posted map[string]any
	require.NoError(t, json.Unmarshal(postedBody, &posted))
	tools, ok := posted["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1, "Chat Completions function tools must not be promoted to Gemini built-ins by name")

	functionTool, ok := tools[0].(map[string]any)
	require.True(t, ok)
	functionDecls, ok := functionTool["functionDeclarations"].([]any)
	require.True(t, ok)
	require.Len(t, functionDecls, 2)
	webSearchDecl, ok := functionDecls[0].(map[string]any)
	require.True(t, ok)
	readFileDecl, ok := functionDecls[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "web_search", webSearchDecl["name"])
	require.Equal(t, "read_file", readFileDecl["name"])
	require.NotContains(t, functionTool, "googleSearch")
	require.NotContains(t, functionTool, "google_search")
}

// TestConvertClaudeToolsToGeminiTools_CustomType 测试custom类型工具转换
