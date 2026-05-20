package apicompat

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// codex round44 fu62 (2026-05-21): canonical Claude SSE wire shape.
//
// Real Claude streaming responses always emit:
//   - message_start.message.stop_reason   = null
//   - message_start.message.stop_sequence = null
//   - message_delta.delta.stop_reason     = "<reason>" (set by caller)
//   - message_delta.delta.stop_sequence   = null
//
// Our default json.Marshal output drifted from canonical because:
//   - AnthropicResponse.StopReason is `string` (no omitempty), so the
//     zero value emits "stop_reason":"" rather than null.
//   - AnthropicResponse.StopSequence + AnthropicDelta.StopSequence are
//     `*string` with omitempty, so nil pointers are dropped entirely.
//
// fu62 patches the wire shape at the SSE boundary (in
// ResponsesAnthropicEventToSSE) using sjson so we don't have to
// remove omitempty on AnthropicDelta — that would pollute every
// content_block_delta event (text_delta / thinking_delta /
// signature_delta / input_json_delta) which real Claude never decorates
// with stop_* fields. These tests pin both the canonical output AND
// the non-pollution invariant.

// dataJSON returns the JSON payload from the "data: ..." line of an SSE
// event pair produced by ResponsesAnthropicEventToSSE. The wire shape
// is "event: <type>\ndata: <json>\n\n".
func dataJSON(t *testing.T, sse string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(sse, "\n"), "\n")
	require.GreaterOrEqual(t, len(lines), 2, "SSE pair must have at least event/data lines")
	for _, ln := range lines {
		if strings.HasPrefix(ln, "data: ") {
			return strings.TrimPrefix(ln, "data: ")
		}
	}
	t.Fatalf("no data line in SSE pair: %q", sse)
	return ""
}

func TestRound44_MessageStart_EmitsExplicitNullStopReasonAndStopSequence(t *testing.T) {
	evt := AnthropicStreamEvent{
		Type: "message_start",
		Message: &AnthropicResponse{
			ID:      "msg_round44_a",
			Type:    "message",
			Role:    "assistant",
			Content: []AnthropicContentBlock{},
			Model:   "claude-sonnet-4-6",
			Usage:   AnthropicUsage{InputTokens: 5},
			// StopReason and StopSequence left zero — must canonicalise to null.
		},
	}
	sse, err := ResponsesAnthropicEventToSSE(evt)
	require.NoError(t, err)

	data := dataJSON(t, sse)

	// stop_reason MUST be JSON null, not "".
	sr := gjson.Get(data, "message.stop_reason")
	require.True(t, sr.Exists(), "message_start MUST emit message.stop_reason field")
	require.Equal(t, gjson.Null, sr.Type,
		"message_start.message.stop_reason must be JSON null, got %s (raw=%q)", sr.Type, sr.Raw)

	// stop_sequence MUST be JSON null and present.
	ss := gjson.Get(data, "message.stop_sequence")
	require.True(t, ss.Exists(), "message_start MUST emit message.stop_sequence field")
	require.Equal(t, gjson.Null, ss.Type,
		"message_start.message.stop_sequence must be JSON null, got %s (raw=%q)", ss.Type, ss.Raw)

	// Sanity: empty-string variant must NOT appear anywhere in the data.
	require.NotContains(t, data, `"stop_reason":""`,
		"the empty-string form must never reach the wire (canonical drift regression)")
}

func TestRound44_MessageDelta_EmitsStopReasonAndExplicitNullStopSequence(t *testing.T) {
	evt := AnthropicStreamEvent{
		Type: "message_delta",
		Delta: &AnthropicDelta{
			StopReason: "end_turn",
			// StopSequence intentionally nil → must canonicalise to null.
		},
		Usage: &AnthropicUsage{OutputTokens: 7},
	}
	sse, err := ResponsesAnthropicEventToSSE(evt)
	require.NoError(t, err)

	data := dataJSON(t, sse)

	require.Equal(t, "end_turn", gjson.Get(data, "delta.stop_reason").String(),
		"message_delta.delta.stop_reason must carry the caller-supplied value")

	ss := gjson.Get(data, "delta.stop_sequence")
	require.True(t, ss.Exists(), "message_delta MUST emit delta.stop_sequence field")
	require.Equal(t, gjson.Null, ss.Type,
		"message_delta.delta.stop_sequence must be JSON null (real Claude wire shape)")
}

func TestRound44_MessageDelta_ToolUseStopReasonAlsoCarriesNullStopSequence(t *testing.T) {
	// Tool-use termination must canonicalise the same way.
	evt := AnthropicStreamEvent{
		Type: "message_delta",
		Delta: &AnthropicDelta{
			StopReason: "tool_use",
		},
		Usage: &AnthropicUsage{OutputTokens: 3},
	}
	sse, err := ResponsesAnthropicEventToSSE(evt)
	require.NoError(t, err)

	data := dataJSON(t, sse)
	require.Equal(t, "tool_use", gjson.Get(data, "delta.stop_reason").String())
	require.Equal(t, gjson.Null, gjson.Get(data, "delta.stop_sequence").Type)
}

// === Non-pollution invariants ===
//
// AnthropicDelta is shared between message_delta and the four
// content_block_delta variants. Real Claude does NOT decorate the
// content_block_delta forms with stop_* fields, so fu62's canonical
// patch must NEVER touch those events.

func TestRound44_TextDelta_DoesNotEmitStopFields(t *testing.T) {
	evt := AnthropicStreamEvent{
		Type:  "content_block_delta",
		Index: intPtr(0),
		Delta: &AnthropicDelta{
			Type: "text_delta",
			Text: "hello",
		},
	}
	sse, err := ResponsesAnthropicEventToSSE(evt)
	require.NoError(t, err)

	data := dataJSON(t, sse)
	require.False(t, gjson.Get(data, "delta.stop_reason").Exists(),
		"text_delta MUST NOT acquire a stop_reason field")
	require.False(t, gjson.Get(data, "delta.stop_sequence").Exists(),
		"text_delta MUST NOT acquire a stop_sequence field — would break byte-level Claude mimic")
	require.Equal(t, "hello", gjson.Get(data, "delta.text").String())
}

func TestRound44_ThinkingDelta_DoesNotEmitStopFields(t *testing.T) {
	evt := AnthropicStreamEvent{
		Type:  "content_block_delta",
		Index: intPtr(0),
		Delta: &AnthropicDelta{
			Type:     "thinking_delta",
			Thinking: "let me consider",
		},
	}
	sse, err := ResponsesAnthropicEventToSSE(evt)
	require.NoError(t, err)

	data := dataJSON(t, sse)
	require.False(t, gjson.Get(data, "delta.stop_reason").Exists())
	require.False(t, gjson.Get(data, "delta.stop_sequence").Exists())
	require.Equal(t, "let me consider", gjson.Get(data, "delta.thinking").String())
}

func TestRound44_SignatureDelta_DoesNotEmitStopFields(t *testing.T) {
	evt := AnthropicStreamEvent{
		Type:  "content_block_delta",
		Index: intPtr(0),
		Delta: &AnthropicDelta{
			Type:      "signature_delta",
			Signature: "sig-abc",
		},
	}
	sse, err := ResponsesAnthropicEventToSSE(evt)
	require.NoError(t, err)

	data := dataJSON(t, sse)
	require.False(t, gjson.Get(data, "delta.stop_reason").Exists())
	require.False(t, gjson.Get(data, "delta.stop_sequence").Exists())
}

func TestRound44_InputJSONDelta_DoesNotEmitStopFields(t *testing.T) {
	evt := AnthropicStreamEvent{
		Type:  "content_block_delta",
		Index: intPtr(0),
		Delta: &AnthropicDelta{
			Type:        "input_json_delta",
			PartialJSON: `{"q":"hi"}`,
		},
	}
	sse, err := ResponsesAnthropicEventToSSE(evt)
	require.NoError(t, err)

	data := dataJSON(t, sse)
	require.False(t, gjson.Get(data, "delta.stop_reason").Exists())
	require.False(t, gjson.Get(data, "delta.stop_sequence").Exists())
}

// content_block_start / content_block_stop / ping / message_stop all use
// shapes where the canonical patch should be a no-op (no Message, no
// matching event type). Pin those to catch a future refactor widening
// the switch by accident.

func TestRound44_ContentBlockStart_NotPatched(t *testing.T) {
	evt := AnthropicStreamEvent{
		Type:  "content_block_start",
		Index: intPtr(0),
		ContentBlock: &AnthropicContentBlock{
			Type: "text",
			Text: "",
		},
	}
	sse, err := ResponsesAnthropicEventToSSE(evt)
	require.NoError(t, err)
	data := dataJSON(t, sse)
	require.False(t, gjson.Get(data, "content_block.stop_reason").Exists())
	require.False(t, gjson.Get(data, "content_block.stop_sequence").Exists())
	require.False(t, gjson.Get(data, "delta.stop_sequence").Exists())
}

func TestRound44_MessageStop_NotPatched(t *testing.T) {
	evt := AnthropicStreamEvent{Type: "message_stop"}
	sse, err := ResponsesAnthropicEventToSSE(evt)
	require.NoError(t, err)
	data := dataJSON(t, sse)
	// message_stop is just `{"type":"message_stop"}` — no nested objects
	// to patch. Verify no spurious fields appeared.
	require.False(t, gjson.Get(data, "message").Exists())
	require.False(t, gjson.Get(data, "delta").Exists())
}

// Sanity: SSE pair structure (event line + data line + blank) untouched
// by the canonical patch.

func TestRound44_SSEPairStructureIntact(t *testing.T) {
	evt := AnthropicStreamEvent{
		Type: "message_start",
		Message: &AnthropicResponse{
			ID:      "msg_pair",
			Type:    "message",
			Role:    "assistant",
			Content: []AnthropicContentBlock{},
			Model:   "claude-sonnet-4-6",
			Usage:   AnthropicUsage{InputTokens: 1},
		},
	}
	sse, err := ResponsesAnthropicEventToSSE(evt)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(sse, "event: message_start\n"))
	require.True(t, strings.HasSuffix(sse, "\n\n"))
	require.Contains(t, sse, "\ndata: {")
}

func intPtr(i int) *int { return &i }
