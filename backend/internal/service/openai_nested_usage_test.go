package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractOpenAIUsage_ReadsClineDataEnvelope(t *testing.T) {
	body := []byte(`{"data":{"choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":8,"completion_tokens":27,"total_tokens":35,"prompt_tokens_details":{"cached_tokens":4}}},"success":true}`)

	usage, ok := extractOpenAIUsageFromJSONBytes(body)

	require.True(t, ok)
	require.Equal(t, 8, usage.InputTokens)
	require.Equal(t, 27, usage.OutputTokens)
	require.Equal(t, 4, usage.CacheReadInputTokens)
}

func TestExtractOpenAIUsage_ReadsWrappedResponsesDataEnvelope(t *testing.T) {
	body := []byte(`{"data":{"response":{"usage":{"input_tokens":11,"output_tokens":5,"total_tokens":16,"input_tokens_details":{"cached_tokens":2}}}}}`)

	usage, ok := extractOpenAIUsageFromJSONBytes(body)

	require.True(t, ok)
	require.Equal(t, 11, usage.InputTokens)
	require.Equal(t, 5, usage.OutputTokens)
	require.Equal(t, 2, usage.CacheReadInputTokens)
}

func TestExtractOpenAIUsage_PreservesResponseUsagePriority(t *testing.T) {
	body := []byte(`{"data":{"usage":{"prompt_tokens":100,"completion_tokens":50}},"response":{"usage":{"input_tokens":11,"output_tokens":5}}}`)

	usage, ok := extractOpenAIUsageFromJSONBytes(body)

	require.True(t, ok)
	require.Equal(t, 11, usage.InputTokens)
	require.Equal(t, 5, usage.OutputTokens)
}
