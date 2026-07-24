package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const deferredToolNoticeFixture = `<system-reminder>The following deferred tools are now available via ToolSearch. Their schemas are NOT loaded.</system-reminder>`

func TestAppendOpenAICompatDeferredToolGuard_MatchingRequest(t *testing.T) {
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

	require.True(t, appendOpenAICompatDeferredToolGuard(req))
	assert.False(t, appendOpenAICompatDeferredToolGuard(req), "guard insertion must be idempotent")

	var items []apicompat.ResponsesInputItem
	require.NoError(t, json.Unmarshal(req.Input, &items))
	require.Len(t, items, 3)
	assert.Equal(t, "developer", items[0].Role)
	assert.Equal(t, "developer", items[1].Role)
	assert.True(t, containsPlainOrJSONEscapedText(string(items[1].Content), openAICompatDeferredToolGuardMarker))
	assert.True(t, containsPlainOrJSONEscapedText(string(items[1].Content), "batch compatible operations into the same Bash call when safe"))
	assert.True(t, containsPlainOrJSONEscapedText(string(items[1].Content), "verify the result before replying"))
	assert.Equal(t, "user", items[2].Role)
	assert.Equal(t, 2, strings.Count(string(req.Input), "sub2api-deferred-tool-guard"))
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

func TestAppendOpenAICompatDeferredToolGuardToRequestBody_MatchingRequest(t *testing.T) {
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

	require.True(t, appendOpenAICompatDeferredToolGuardToRequestBody(reqBody))
	assert.False(t, appendOpenAICompatDeferredToolGuardToRequestBody(reqBody), "guard insertion must be idempotent")

	body, err := json.Marshal(reqBody)
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(body), "sub2api-deferred-tool-guard"))

	input := reqBody["input"].([]any)
	require.Len(t, input, 2)
	guard, ok := input[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "developer", guard["role"])
	assert.True(t, inputContainsText(input, "complete the requested deliverable within the available tool turns"))
}

func TestApplyOpenAICompatOAuthMessagesBridgeGuards_PreservesDeferredToolLoadingContract(t *testing.T) {
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
	applyOpenAICompatOAuthMessagesBridgeGuards(reqBody)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	assert.True(t, inputContainsDeferredToolNotice(input))
	assert.True(t, inputContainsText(input, openAICompatDeferredToolGuardMarker))
	assert.True(t, inputContainsText(input, "Do not load task or todo tracking tools only for bookkeeping."))
	assert.True(t, inputContainsText(input, "batch compatible operations into the same Bash call when safe"))
	assert.True(t, inputContainsText(input, "verify the result before replying"))
	assert.True(t, inputContainsText(input, openAICompatClaudeCodeTodoGuardMarker))
	assert.True(t, requestBodyToolsSupportDirectImplementation(reqBody["tools"]))
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
