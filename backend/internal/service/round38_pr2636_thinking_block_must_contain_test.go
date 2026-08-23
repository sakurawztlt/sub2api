package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// codex round39 fu57 / upstream PR #2636 (2026-05-20): cross-model
// session reuse can carry assistant history with empty thinking
// blocks that fail upstream validation:
//   "messages.X.content.Y.thinking: each thinking block must contain thinking"
// FilterThinkingBlocksForRetry already handles dropping such blocks;
// fu57 adds the upstream error pattern to isThinkingBlockSignatureError
// so the retry path actually triggers.

func TestRound38_FilterThinkingBlocksForRetry_DropsThinkingBlockWithEmptyContent(t *testing.T) {
	// Cross-model scenario: history from another model included a
	// thinking block with an empty `thinking` field. Claude with
	// extended thinking enabled then rejects the whole turn. The
	// retry path should drop the empty thinking block and keep the
	// remaining text block intact.
	input := []byte(`{
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[
			{"role":"user","content":[{"type":"text","text":"Hi"}]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"","signature":"sig"},
				{"type":"text","text":"Answer"}
			]}
		]
	}`)

	// Local FilterThinkingBlocksForRetry takes (body, mappedModel) — the
	// upstream PR test passed only body. We must pass an anthropic-strict
	// model id so ShouldApplyRetryFilters returns true; otherwise the
	// function early-returns the body unchanged. Claude is strict.
	out := FilterThinkingBlocksForRetry(input, "claude-sonnet-4-6")

	var req map[string]any
	require.NoError(t, json.Unmarshal(out, &req))
	_, hasThinking := req["thinking"]
	require.False(t, hasThinking, "top-level thinking config should be removed for retry")

	msgs, ok := req["messages"].([]any)
	require.True(t, ok)
	assistant, ok := msgs[1].(map[string]any)
	require.True(t, ok)
	content, ok := assistant["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1, "empty thinking block should be dropped, only text remains")
	textBlock, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "text", textBlock["type"])
	require.Equal(t, "Answer", textBlock["text"])
}

// Direct test on isThinkingBlockSignatureError — the new pattern must
// trigger on the exact upstream phrasing.
func TestRound38_IsThinkingBlockSignatureError_RecognizesEmptyThinkingPattern(t *testing.T) {
	s := &GatewayService{}

	// Real-world upstream error body shape: Anthropic-formatted error
	// envelope with the "thinking block must contain" phrase nested
	// inside the message string.
	bodies := [][]byte{
		[]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.0.thinking: each thinking block must contain thinking"}}`),
		[]byte(`{"error":{"message":"each thinking block must contain thinking"}}`),
		// Tolerate case variation — the matcher lowercases the message.
		[]byte(`{"error":{"message":"each Thinking Block Must Contain Thinking"}}`),
	}
	for i, body := range bodies {
		require.True(t, s.isThinkingBlockSignatureError(body),
			"case %d: must recognise the PR #2636 pattern", i)
	}
}

// Negative: unrelated 400s must not start matching as thinking block errors.
func TestRound38_IsThinkingBlockSignatureError_DoesNotFalseMatch(t *testing.T) {
	s := &GatewayService{}
	negatives := [][]byte{
		[]byte(`{"error":{"message":"rate limit exceeded"}}`),
		[]byte(`{"error":{"message":"context length exceeded"}}`),
		[]byte(`{"error":{"message":"thinking blocks present but field is fine"}}`), // doesn't contain the exact "must contain" phrase
		[]byte(`{}`),
	}
	for i, body := range negatives {
		require.False(t, s.isThinkingBlockSignatureError(body),
			"case %d: must not match unrelated error", i)
	}
}
