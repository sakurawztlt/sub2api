package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type chunkTrackingReadCloser struct {
	chunks    [][]byte
	next      int
	closed    bool
	readCalls int
}

func (r *chunkTrackingReadCloser) Read(dst []byte) (int, error) {
	if r.next >= len(r.chunks) {
		return 0, io.EOF
	}
	chunk := r.chunks[r.next]
	r.next++
	r.readCalls++
	return copy(dst, chunk), nil
}

func (r *chunkTrackingReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestAnthropicWebSearchRequestLimitForResponse_UsesMaxUses(t *testing.T) {
	var req apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":512,
		"messages":[{"role":"user","content":"latest AI news"}],
		"tools":[{
			"type":"web_search_20250305",
			"name":"web_search",
			"max_uses":1
		}]
	}`), &req))

	require.Equal(t, 1, anthropicWebSearchRequestLimitForResponse(&req))
}

func TestAnthropicWebSearchRequestLimitForResponse_PreservesOrdinaryMaxUses(t *testing.T) {
	var req apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":512,
		"messages":[{"role":"user","content":"research several AI stories"}],
		"tools":[{
			"type":"web_search_20250305",
			"name":"web_search",
			"max_uses":8
		}]
	}`), &req))

	require.Equal(t, 8, req.Tools[0].MaxUses, "Anthropic max_uses must remain parsed")
	require.Equal(t, 8, anthropicWebSearchRequestLimitForResponse(&req))
}

func TestAnthropicWebSearchRequestLimitForResponse_SingularProbeOverridesPermissiveMaxUses(t *testing.T) {
	var req apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":512,
		"messages":[{
			"role":"user",
			"content":"Use the web_search tool first, then summarize.\n\nOriginal request:\nPerform a web search for the query: AI news 2026-07-26"
		}],
		"tools":[{
			"type":"web_search_20250305",
			"name":"web_search",
			"max_uses":8
		}]
	}`), &req))

	require.Equal(t, 8, req.Tools[0].MaxUses, "Anthropic max_uses must remain parsed")
	require.Equal(t, 1, anthropicWebSearchRequestLimitForResponse(&req))
}

func TestAnthropicWebSearchRequestLimitForResponse_DirectSingularProbeOverridesPermissiveMaxUses(t *testing.T) {
	var req apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":64000,
		"stream":true,
		"messages":[{
			"role":"user",
			"content":[{"type":"text","text":"Perform a web search for the query: AI news 2026-07-26"}]
		}],
		"tools":[{
			"type":"web_search_20250305",
			"name":"web_search",
			"max_uses":8
		}],
		"tool_choice":{"type":"tool","name":"web_search"}
	}`), &req))

	require.Equal(t, 8, req.Tools[0].MaxUses)
	require.Equal(t, 1, anthropicWebSearchRequestLimitForResponse(&req))
}

func TestForwardAsAnthropic_ExactWebSearchProbeCompletesFromRealSources(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":64000,
		"stream":true,
		"messages":[{
			"role":"user",
			"content":[{"type":"text","text":"Perform a web search for the query: AI news 2026-07-26"}]
		}],
		"tools":[{
			"type":"web_search_20250305",
			"name":"web_search",
			"max_uses":8
		}],
		"tool_choice":{"type":"tool","name":"web_search"}
	}`)
	upstreamBody := &chunkTrackingReadCloser{chunks: exactWebSearchProbeUpstreamFrames()}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"x-request-id": []string{"req_websearch_fast"},
		},
		Body: upstreamBody,
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	result, err := svc.ForwardAsAnthropic(
		context.Background(),
		c,
		webSearchStreamingTestAccount(),
		body,
		"",
		"gpt-5.4",
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Empty(t, result.ResponseID,
		"cancelled upstream response must not enter continuation bookkeeping")
	require.Positive(t, result.Usage.InputTokens)
	require.Positive(t, result.Usage.OutputTokens)
	require.True(t, upstreamBody.closed)
	require.Equal(t, 2, upstreamBody.readCalls,
		"exact probe must stop after real sources instead of reading late model text")

	wire := rec.Body.String()
	require.Equal(t, 1, strings.Count(wire, "event: message_stop"))
	require.Contains(t, wire, `"type":"citations_delta"`)
	require.Contains(t, wire, "https://example.com/one")
	require.Contains(t, wire, "https://example.org/two")
	require.Contains(t, wire, "https://example.net/three")
	require.NotContains(t, wire, "late model-authored answer")
}

func TestForwardAsAnthropic_OrdinaryWebSearchStillReadsTerminalResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":64000,
		"stream":true,
		"messages":[{
			"role":"user",
			"content":[{"type":"text","text":"Perform a web search for the query: AI news 2026-07-26\nThen write a detailed report"}]
		}],
		"tools":[{
			"type":"web_search_20250305",
			"name":"web_search",
			"max_uses":8
		}],
		"tool_choice":{"type":"tool","name":"web_search"}
	}`)
	upstreamBody := &chunkTrackingReadCloser{chunks: exactWebSearchProbeUpstreamFrames()}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       upstreamBody,
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	result, err := svc.ForwardAsAnthropic(
		context.Background(),
		c,
		webSearchStreamingTestAccount(),
		body,
		"",
		"gpt-5.4",
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "resp_websearch_fast", result.ResponseID)
	require.Equal(t, 4, upstreamBody.readCalls,
		"non-exact requests must keep the ordinary terminal-usage path")
	require.True(t, upstreamBody.closed)
	require.Contains(t, rec.Body.String(), "late model-authored answer")
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: message_stop"))
}

func webSearchStreamingTestAccount() *Account {
	return &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-account",
			"model_mapping": map[string]any{
				"claude-opus-4-8": "gpt-5.4",
			},
		},
	}
}

func exactWebSearchProbeUpstreamFrames() [][]byte {
	return [][]byte{
		[]byte("data: " + `{"type":"response.created","response":{"id":"resp_websearch_fast","object":"response","model":"gpt-5.4","status":"in_progress","output":[]}}` + "\n\n"),
		[]byte("data: " + `{"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_fast","status":"completed","action":{"type":"search","query":"AI news 2026-07-26","sources":[{"type":"url","url":"https://example.com/one"},{"type":"url","url":"https://example.org/two"},{"type":"url","url":"https://example.net/three"},{"type":"url","url":"https://example.edu/four"}]}}}` + "\n\n"),
		[]byte("data: " + `{"type":"response.output_text.delta","item_id":"msg_late","output_index":1,"content_index":0,"delta":"late model-authored answer"}` + "\n\n"),
		[]byte("data: " + `{"type":"response.completed","response":{"id":"resp_websearch_fast","object":"response","model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":21,"output_tokens":8,"total_tokens":29}}}` + "\n\n"),
	}
}
