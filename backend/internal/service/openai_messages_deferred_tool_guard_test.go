package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const deferredToolNoticeFixture = `<system-reminder>The following deferred tools are now available via ToolSearch. Their schemas are NOT loaded.</system-reminder>`

func TestOpenAICompatDeferredToolContract_RemainsUnmodified(t *testing.T) {
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

	require.True(t, appendOpenAICompatClaudeCodeTodoGuard(req))

	var items []apicompat.ResponsesInputItem
	require.NoError(t, json.Unmarshal(req.Input, &items))
	require.Len(t, items, 3)
	assert.Equal(t, "developer", items[0].Role)
	assert.Equal(t, "developer", items[1].Role)
	assert.True(t, containsPlainOrJSONEscapedText(string(items[1].Content), openAICompatClaudeCodeTodoGuardMarker))
	assert.Equal(t, "user", items[2].Role)
	assert.Contains(t, string(items[2].Content), "deferred tools")
	assert.NotContains(t, string(req.Input), "sub2api-deferred-tool-guard")
}

func TestApplyOpenAICompatOAuthMessagesBridgeGuards_DoesNotOverrideDeferredToolContract(t *testing.T) {
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
	body, err = json.Marshal(reqBody)
	require.NoError(t, err)
	assert.Contains(t, string(body), "deferred tools")
	assert.NotContains(t, string(body), "sub2api-deferred-tool-guard")
	assert.True(t, inputContainsText(input, openAICompatClaudeCodeTodoGuardMarker))
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
