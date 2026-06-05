package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// assertAnthropicPairing enforces the Anthropic Messages tool-pairing invariants
// that, when violated, surface as upstream 400s.
func assertAnthropicPairing(t *testing.T, messages []AnthropicMessage) {
	t.Helper()
	for i, m := range messages {
		blocks := parseContentBlocks(m.Content)

		// No two consecutive same-role messages.
		if i > 0 {
			require.NotEqualf(t, messages[i-1].Role, m.Role, "consecutive %s messages at %d", m.Role, i)
		}

		for _, b := range blocks {
			switch b.Type {
			case "tool_result":
				// Must have a matching tool_use in the immediately previous message.
				require.Positivef(t, i, "tool_result %s has no previous message", b.ToolUseID)
				prev := parseContentBlocks(messages[i-1].Content)
				require.Truef(t, hasToolUse(prev, b.ToolUseID),
					"tool_result %s has no corresponding tool_use in previous message", b.ToolUseID)
			case "tool_use":
				// Must be answered by a tool_result in the immediately next message.
				require.Lessf(t, i+1, len(messages), "tool_use %s has no following message", b.ID)
				next := parseContentBlocks(messages[i+1].Content)
				require.Truef(t, hasToolResult(next, b.ID),
					"tool_use %s is not answered in the next message", b.ID)
			}
		}
	}
}

func hasToolUse(blocks []AnthropicContentBlock, id string) bool {
	for _, b := range blocks {
		if b.Type == "tool_use" && b.ID == id {
			return true
		}
	}
	return false
}

func hasToolResult(blocks []AnthropicContentBlock, toolUseID string) bool {
	for _, b := range blocks {
		if b.Type == "tool_result" && b.ToolUseID == toolUseID {
			return true
		}
	}
	return false
}

func convertAnthropic(t *testing.T, input string) []AnthropicMessage {
	t.Helper()
	_, messages, err := convertResponsesInputToAnthropic(json.RawMessage(input))
	require.NoError(t, err)
	assertAnthropicPairing(t, messages)
	return messages
}

func expectedAnthropicToolID(id string) string {
	return fromResponsesCallIDToAnthropic(id)
}

// Tests use call_-prefixed ids because real OpenAI/Codex histories use them.
// This fork intentionally rewrites them to Anthropic-style toolu_* IDs before
// forwarding, so assertions compare against expectedAnthropicToolID(...).

// A developer/approval message injected between a function_call and its output
// must be moved out of the tool_use→tool_result adjacency. This is the shape
// that produced the production 400 "tool_result ... must have a corresponding
// tool_use block in the previous message".
func TestAnthropicPairing_DeveloperMessageBetween(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"do it"}]},
		{"type":"function_call","call_id":"call_A","name":"exec","arguments":"{}"},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"Approved command prefix saved"}]},
		{"type":"function_call_output","call_id":"call_A","output":"ok"}
	]`)
	// The assistant tool_use message is immediately followed by its tool_result.
	expectedID := expectedAnthropicToolID("call_A")
	for i, m := range msgs {
		if hasToolUse(parseContentBlocks(m.Content), expectedID) {
			require.Equal(t, "user", msgs[i+1].Role)
			require.True(t, hasToolResult(parseContentBlocks(msgs[i+1].Content), expectedID))
		}
	}
}

// Parallel tool calls where both outputs arrive stay grouped: one assistant
// message with both tool_use blocks, the next user message with both results.
func TestAnthropicPairing_ParallelBothAnswered(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"features?"}]},
		{"type":"function_call","call_id":"call_c0","name":"exec","arguments":"{}"},
		{"type":"function_call","call_id":"call_c1","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_c0","output":"log"},
		{"type":"function_call_output","call_id":"call_c1","output":"tags"}
	]`)
	expectedC0 := expectedAnthropicToolID("call_c0")
	expectedC1 := expectedAnthropicToolID("call_c1")
	var sawGrouped bool
	for _, m := range msgs {
		blocks := parseContentBlocks(m.Content)
		if hasToolUse(blocks, expectedC0) && hasToolUse(blocks, expectedC1) {
			sawGrouped = true
		}
	}
	require.True(t, sawGrouped, "parallel tool_use blocks should share one assistant message")
}

// A parallel call whose sibling output never arrived must be dropped so every
// remaining tool_use is answered.
func TestAnthropicPairing_ParallelOneUnanswered(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"function_call","call_id":"call_A","name":"exec","arguments":"{}"},
		{"type":"function_call","call_id":"call_B","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_A","output":"oa"}
	]`)
	expectedB := expectedAnthropicToolID("call_B")
	for _, m := range msgs {
		require.Falsef(t, hasToolUse(parseContentBlocks(m.Content), expectedB),
			"unanswered tool_use call_B should have been dropped")
	}
}

// An orphan tool_result whose tool_use was never announced must be dropped.
func TestAnthropicPairing_OrphanToolResultDropped(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"function_call_output","call_id":"call_ghost","output":"orphan"}
	]`)
	expectedGhost := expectedAnthropicToolID("call_ghost")
	for _, m := range msgs {
		require.Falsef(t, hasToolResult(parseContentBlocks(m.Content), expectedGhost),
			"orphan tool_result should have been dropped")
	}
}

// A dangling tool_call at the end of the history (no output yet) drops the
// assistant message holding only that call, leaving no tool_use behind.
func TestAnthropicPairing_DanglingCallDropped(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"function_call","call_id":"call_A","name":"exec","arguments":"{}"}
	]`)
	expectedA := expectedAnthropicToolID("call_A")
	for _, m := range msgs {
		require.Falsef(t, hasToolUse(parseContentBlocks(m.Content), expectedA),
			"dangling tool_use call_A should have been dropped")
	}
}

// Baseline: a single answered call pairs correctly and preserves the surrounding
// turns.
func TestAnthropicPairing_SingleCall(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"latest sha?"}]},
		{"type":"function_call","call_id":"call_A","name":"exec","arguments":"{\"cmd\":\"git rev-parse HEAD\"}"},
		{"type":"function_call_output","call_id":"call_A","output":"deadbeef"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"It is deadbeef."}]}
	]`)
	// user, assistant(tool_use), user(tool_result), assistant(text)
	expectedA := expectedAnthropicToolID("call_A")
	require.GreaterOrEqual(t, len(msgs), 4)
	require.Equal(t, "user", msgs[0].Role)
	require.True(t, hasToolUse(parseContentBlocks(msgs[1].Content), expectedA))
	require.True(t, hasToolResult(parseContentBlocks(msgs[2].Content), expectedA))
}
