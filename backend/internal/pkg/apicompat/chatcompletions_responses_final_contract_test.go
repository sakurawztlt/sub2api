package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageConversionsPreserveCacheWriteTokens(t *testing.T) {
	var responsesUsage ResponsesUsage
	require.NoError(t, json.Unmarshal([]byte(`{
		"input_tokens":1000,
		"output_tokens":50,
		"input_tokens_details":{"cached_tokens":100,"cache_write_tokens":200}
	}`), &responsesUsage))
	require.NotNil(t, responsesUsage.InputTokensDetails)
	require.Equal(t, 200, responsesUsage.InputTokensDetails.CacheWriteTokens)

	chatUsage := chatUsageFromResponsesUsage(&responsesUsage)
	require.NotNil(t, chatUsage.PromptTokensDetails)
	require.Equal(t, 100, chatUsage.PromptTokensDetails.CachedTokens)
	require.Equal(t, 200, chatUsage.PromptTokensDetails.CacheWriteTokens)

	roundTrip := ChatUsageToResponsesUsage(chatUsage)
	require.NotNil(t, roundTrip.InputTokensDetails)
	require.Equal(t, 200, roundTrip.CacheCreationInputTokens)
	require.Equal(t, 200, roundTrip.InputTokensDetails.CacheWriteTokens)
}

func TestResponsesUsageNestedCacheWritePresenceOverridesTopLevelAlias(t *testing.T) {
	tests := []struct {
		name       string
		nestedJSON string
		want       int
	}{
		{name: "explicit zero", nestedJSON: `{"cache_write_tokens":0}`, want: 0},
		{name: "nonzero", nestedJSON: `{"cache_write_tokens":7}`, want: 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var usage ResponsesUsage
			payload := []byte(`{"input_tokens":20,"output_tokens":2,"cache_creation_input_tokens":19,"input_tokens_details":` + tt.nestedJSON + `}`)
			require.NoError(t, json.Unmarshal(payload, &usage))
			require.Equal(t, tt.want, usage.CacheCreationInputTokens)
		})
	}
}

func TestChatCompletionsToResponses_ToolStrict(t *testing.T) {
	strictTrue := true
	strictFalse := false
	tests := []struct {
		name   string
		strict *bool
		want   bool
	}{
		{name: "defaults omitted strict to false", want: false},
		{name: "preserves explicit true", strict: &strictTrue, want: true},
		{name: "preserves explicit false", strict: &strictFalse, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &ChatCompletionsRequest{
				Model:    "gpt-4o",
				Messages: []ChatMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
				Tools: []ChatTool{{
					Type: "function",
					Function: &ChatFunction{
						Name:   "lookup",
						Strict: tt.strict,
					},
				}},
			}

			resp, err := ChatCompletionsToResponses(req)
			require.NoError(t, err)
			require.Len(t, resp.Tools, 1)
			require.NotNil(t, resp.Tools[0].Strict)
			assert.Equal(t, tt.want, *resp.Tools[0].Strict)

			payload, err := json.Marshal(resp)
			require.NoError(t, err)
			var serialized struct {
				Tools []map[string]json.RawMessage `json:"tools"`
			}
			require.NoError(t, json.Unmarshal(payload, &serialized))
			require.Len(t, serialized.Tools, 1)
			strictJSON, ok := serialized.Tools[0]["strict"]
			require.True(t, ok, "strict must be present in the Responses payload")
			require.JSONEq(t, string(mustMarshalFinalContractJSON(t, tt.want)), string(strictJSON))
		})
	}
}

func TestChatCompletionsToResponses_LegacyFunctionDefaultsStrictFalse(t *testing.T) {
	req := &ChatCompletionsRequest{
		Model:    "gpt-4o",
		Messages: []ChatMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
		Functions: []ChatFunction{{
			Name: "lookup",
		}},
	}

	resp, err := ChatCompletionsToResponses(req)
	require.NoError(t, err)
	require.Len(t, resp.Tools, 1)
	require.NotNil(t, resp.Tools[0].Strict)
	assert.False(t, *resp.Tools[0].Strict)

	payload, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.Contains(t, string(payload), `"strict":false`)
}

func TestChatCompletionsToResponses_AssistantReasoningContentPreserved(t *testing.T) {
	req := &ChatCompletionsRequest{
		Model: "gpt-4o",
		Messages: []ChatMessage{
			{Role: "user", Content: json.RawMessage(`"Hi"`)},
			{
				Role:             "assistant",
				ReasoningContent: "internal plan",
				Content:          json.RawMessage(`"final answer"`),
			},
		},
	}

	resp, err := ChatCompletionsToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	require.Len(t, items, 2)

	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[1].Content, &parts))
	require.Len(t, parts, 1)
	assert.Equal(t, "output_text", parts[0].Type)
	assert.Contains(t, parts[0].Text, "<thinking>internal plan</thinking>")
	assert.Contains(t, parts[0].Text, "final answer")
}

func mustMarshalFinalContractJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}
