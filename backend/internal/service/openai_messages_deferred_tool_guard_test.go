package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const deferredToolNoticeFixture = `<system-reminder>The following deferred tools are now available via ToolSearch. Their schemas are NOT loaded.</system-reminder>`

func TestAppendOpenAICompatDeferredToolGuard_PreservesClientProtocol(t *testing.T) {
	input, err := json.Marshal([]apicompat.ResponsesInputItem{
		{
			Type: "message",
			Role: "developer",
			Content: json.RawMessage(`[
				{"type":"input_text","text":"existing developer instructions"}
			]`),
		},
		{
			Type: "message",
			Role: "user",
			Content: json.RawMessage(`[
				{"type":"input_text","text":"` + deferredToolNoticeFixture + `"},
				{"type":"input_text","text":"帮我创建一个 Go 项目"}
			]`),
		},
	})
	require.NoError(t, err)

	req := &apicompat.ResponsesRequest{
		Input: input,
		Tools: []apicompat.ResponsesTool{
			{Type: "function", Name: "ToolSearch"},
			{Type: "function", Name: "Bash"},
			{Type: "function", Name: "Write"},
		},
	}

	before := append([]byte(nil), req.Input...)
	assert.False(t, appendOpenAICompatDeferredToolGuard(req))
	assert.JSONEq(t, string(before), string(req.Input))
	assert.NotContains(t, string(req.Input), "sub2api-deferred-tool-guard")
}

func TestAppendOpenAICompatDeferredToolGuard_RequiresNoticeAndLoadedDirectTool(t *testing.T) {
	makeRequest := func(text string, tools []apicompat.ResponsesTool) *apicompat.ResponsesRequest {
		input, err := json.Marshal([]apicompat.ResponsesInputItem{{
			Type:    "message",
			Role:    "user",
			Content: json.RawMessage(`[{"type":"input_text","text":` + mustJSONTestString(t, text) + `}]`),
		}})
		require.NoError(t, err)
		return &apicompat.ResponsesRequest{Input: input, Tools: tools}
	}

	tests := []struct {
		name  string
		text  string
		tools []apicompat.ResponsesTool
	}{
		{
			name: "no deferred notice",
			text: "帮我创建一个 Go 项目",
			tools: []apicompat.ResponsesTool{
				{Type: "function", Name: "ToolSearch"},
				{Type: "function", Name: "Bash"},
			},
		},
		{
			name: "no direct implementation tool",
			text: deferredToolNoticeFixture,
			tools: []apicompat.ResponsesTool{
				{Type: "function", Name: "ToolSearch"},
				{Type: "function", Name: "Read"},
			},
		},
		{
			name: "no tool search",
			text: deferredToolNoticeFixture,
			tools: []apicompat.ResponsesTool{
				{Type: "function", Name: "Bash"},
				{Type: "function", Name: "Write"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := makeRequest(tt.text, tt.tools)
			assert.False(t, appendOpenAICompatDeferredToolGuard(req))
			assert.NotContains(t, string(req.Input), openAICompatDeferredToolGuardMarker)
		})
	}
}

func TestAppendOpenAICompatDeferredToolGuardToRequestBody_PreservesClientProtocol(t *testing.T) {
	reqBody := map[string]any{
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": deferredToolNoticeFixture},
					map[string]any{"type": "input_text", "text": "帮我创建一个 Go 项目"},
				},
			},
		},
		"tools": []any{
			map[string]any{"type": "function", "name": "ToolSearch"},
			map[string]any{"type": "function", "function": map[string]any{"name": "Edit"}},
		},
	}

	before, err := json.Marshal(reqBody)
	require.NoError(t, err)
	assert.False(t, appendOpenAICompatDeferredToolGuardToRequestBody(reqBody))

	after, err := json.Marshal(reqBody)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after))
	assert.NotContains(t, string(after), "sub2api-deferred-tool-guard")
}

func TestAppendOpenAICompatDeferredToolGuard_OAuthMessagesBridgeShape(t *testing.T) {
	anthropicReq := &apicompat.AnthropicRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 64000,
		Messages: []apicompat.AnthropicMessage{{
			Role: "user",
			Content: json.RawMessage(`[
				{"type":"text","text":"` + deferredToolNoticeFixture + `"},
				{"type":"text","text":"帮我创建一个 test-golang 项目写几个算法"}
			]`),
		}},
		Tools: []apicompat.AnthropicTool{
			{Name: "ToolSearch", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "Bash", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "Write", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}

	responsesReq, err := apicompat.AnthropicToResponses(anthropicReq)
	require.NoError(t, err)
	responsesReq.Model = "gpt-5.5"
	responsesReq.PromptCacheKey = "anthropic-metadata-session-test"

	body, err := json.Marshal(responsesReq)
	require.NoError(t, err)
	var reqBody map[string]any
	require.NoError(t, json.Unmarshal(body, &reqBody))

	applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{
		SkipDefaultInstructions: true,
		PreserveToolCallIDs:     true,
	})
	require.False(t, appendOpenAICompatDeferredToolGuardToRequestBody(reqBody))

	transformed, err := json.Marshal(reqBody)
	require.NoError(t, err)
	assert.NotContains(t, string(transformed), "sub2api-deferred-tool-guard")
	assert.Contains(t, string(transformed), `"name":"ToolSearch"`)
	assert.Contains(t, string(transformed), `"name":"Bash"`)
	assert.Contains(t, string(transformed), `"name":"Write"`)
}

func TestAppendOpenAICompatClaudeCodeTodoGuard_IdempotentWithJSONEscaping(t *testing.T) {
	input, err := json.Marshal([]apicompat.ResponsesInputItem{{
		Type:    "message",
		Role:    "user",
		Content: json.RawMessage(`[{"type":"input_text","text":"hello"}]`),
	}})
	require.NoError(t, err)
	req := &apicompat.ResponsesRequest{Input: input}

	require.True(t, appendOpenAICompatClaudeCodeTodoGuard(req))
	assert.False(t, appendOpenAICompatClaudeCodeTodoGuard(req))

	var items []apicompat.ResponsesInputItem
	require.NoError(t, json.Unmarshal(req.Input, &items))
	assert.Len(t, items, 2)
}

func mustJSONTestString(t *testing.T, value string) string {
	t.Helper()
	b, err := json.Marshal(value)
	require.NoError(t, err)
	return string(b)
}
