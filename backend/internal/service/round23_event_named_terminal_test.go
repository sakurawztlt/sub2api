package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// codex round 23 / upstream cc5328c49 fu40: regression tests for the
// event-named terminal recognition fix.
//
// Bug: OpenAI Responses SSE can deliver terminal events as
//
//	event: response.completed
//	data: {"response":{...usage...}}
//
// (no `type` field inside data) — local sub2api before this fix only
// inspected `data:` lines and used `gjson.Get(data, "type")` which
// returned "", so the terminal was never recognized. Stream stayed open
// until upstream-close → "missing terminal event" / 502 / 504 / usage
// incomplete / very slow non-stream requests.
//
// Test plan per codex:
//   - parser API behaves correctly across event:, data:, blank-line,
//     comment, and end-of-buffer flush
//   - openAICompatPayloadWithEventType patches type when missing and
//     leaves it alone when present (or [DONE]/empty)
//   - body-level helpers (extractOpenAISSETerminalEvent,
//     extractCodexFinalResponse, parseSSEUsageFromBody) recognize the
//     event-named form

func TestRound23_OpenAICompatSSEFrameParser_DataOnlyForm(t *testing.T) {
	var p openAICompatSSEFrameParser
	frame, ok := p.AddLine(`data: {"type":"response.completed","response":{"id":"resp_1"}}`)
	require.False(t, ok, "data without blank-line boundary must not flush yet")
	require.Equal(t, "", frame.Data)
	// blank line flushes
	frame, ok = p.AddLine("")
	require.True(t, ok)
	require.Equal(t, "", frame.EventType)
	require.Equal(t, `{"type":"response.completed","response":{"id":"resp_1"}}`, frame.Data)
}

func TestRound23_OpenAICompatSSEFrameParser_EventNamedForm(t *testing.T) {
	var p openAICompatSSEFrameParser
	frame, ok := p.AddLine("event: response.completed")
	require.False(t, ok)
	require.Equal(t, "", frame.Data)
	frame, ok = p.AddLine(`data: {"response":{"id":"resp_2","usage":{"input_tokens":100,"output_tokens":50}}}`)
	require.False(t, ok)
	frame, ok = p.AddLine("")
	require.True(t, ok, "blank line MUST flush the buffered event+data")
	require.Equal(t, "response.completed", frame.EventType)
	require.Equal(t, `{"response":{"id":"resp_2","usage":{"input_tokens":100,"output_tokens":50}}}`, frame.Data)
}

func TestRound23_OpenAICompatSSEFrameParser_CommentSkipped(t *testing.T) {
	var p openAICompatSSEFrameParser
	frame, ok := p.AddLine(": keepalive")
	require.False(t, ok, "comment line MUST NOT flush")
	require.Equal(t, "", frame.Data)
}

func TestRound23_OpenAICompatSSEFrameParser_FinishFlushesPending(t *testing.T) {
	// Real-world: upstream closes after the final SSE frame without a
	// trailing blank-line boundary. Parser.Finish MUST surface it.
	var p openAICompatSSEFrameParser
	_, _ = p.AddLine("event: response.completed")
	_, _ = p.AddLine(`data: {"response":{"id":"resp_x"}}`)
	frame, ok := p.Finish()
	require.True(t, ok, "Finish must flush pending event+data without blank-line boundary")
	require.Equal(t, "response.completed", frame.EventType)
	require.Equal(t, `{"response":{"id":"resp_x"}}`, frame.Data)
}

func TestRound23_OpenAICompatSSEFrameParser_FinishEmpty(t *testing.T) {
	var p openAICompatSSEFrameParser
	frame, ok := p.Finish()
	require.False(t, ok, "Finish must return ok=false when nothing buffered")
	require.Equal(t, "", frame.Data)
}

func TestRound23_OpenAICompatPayloadWithEventType(t *testing.T) {
	t.Run("patches type when missing", func(t *testing.T) {
		got := openAICompatPayloadWithEventType(`{"response":{"id":"r"}}`, "response.completed")
		require.Equal(t, "response.completed", gjson.Get(got, "type").String(),
			"type MUST be patched from event name; got=%s", got)
		require.Equal(t, "r", gjson.Get(got, "response.id").String(),
			"original fields MUST be preserved")
	})
	t.Run("leaves type alone when present", func(t *testing.T) {
		got := openAICompatPayloadWithEventType(`{"type":"response.in_progress","response":{}}`, "response.completed")
		require.Equal(t, "response.in_progress", gjson.Get(got, "type").String(),
			"existing type MUST NOT be overwritten")
	})
	t.Run("no-op on empty event name", func(t *testing.T) {
		got := openAICompatPayloadWithEventType(`{"response":{}}`, "")
		require.Equal(t, `{"response":{}}`, got)
	})
	t.Run("no-op on whitespace payload", func(t *testing.T) {
		got := openAICompatPayloadWithEventType("", "response.completed")
		require.Equal(t, "", got)
	})
	t.Run("no-op on [DONE]", func(t *testing.T) {
		got := openAICompatPayloadWithEventType("[DONE]", "response.completed")
		require.Equal(t, "[DONE]", got)
	})
}

// TestRound23_ExtractOpenAISSETerminalEvent_EventNamedForm — the body-level
// terminal extractor previously checked gjson.Get(data, "type") directly,
// missing event-named terminals. fu40 walks via the parser.
func TestRound23_ExtractOpenAISSETerminalEvent_EventNamedForm(t *testing.T) {
	body := strings.Join([]string{
		"event: response.created",
		`data: {"response":{"id":"resp_1","status":"in_progress"}}`,
		"",
		"event: response.completed",
		`data: {"response":{"id":"resp_1","status":"completed","usage":{"input_tokens":100,"output_tokens":50}}}`,
		"",
	}, "\n")
	gotType, gotPayload, ok := extractOpenAISSETerminalEvent(body)
	require.True(t, ok, "must recognize event-named terminal")
	require.Equal(t, "response.completed", gotType)
	require.Contains(t, string(gotPayload), `"status":"completed"`)
	require.Contains(t, string(gotPayload), `"input_tokens":100`)
}

// TestRound23_ExtractOpenAISSETerminalEvent_DataOnlyFormStillWorks — the
// original data-only form must keep working unchanged.
func TestRound23_ExtractOpenAISSETerminalEvent_DataOnlyFormStillWorks(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
		"",
	}, "\n")
	gotType, _, ok := extractOpenAISSETerminalEvent(body)
	require.True(t, ok)
	require.Equal(t, "response.completed", gotType)
}

// TestRound23_ExtractCodexFinalResponse_EventNamedForm — the codex final
// response extractor used by the SSE-to-JSON path.
func TestRound23_ExtractCodexFinalResponse_EventNamedForm(t *testing.T) {
	body := strings.Join([]string{
		"event: response.completed",
		`data: {"response":{"id":"resp_xyz","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}}`,
		"",
	}, "\n")
	got, ok := extractCodexFinalResponse(body)
	require.True(t, ok)
	require.Contains(t, string(got), `"id":"resp_xyz"`)
	require.Contains(t, string(got), `"text":"hi"`)
}

// TestRound23_ParseSSEUsageFromBody_EventNamedForm — usage on the terminal
// frame must be picked up even when type lives on the event: line.
func TestRound23_ParseSSEUsageFromBody_EventNamedForm(t *testing.T) {
	body := strings.Join([]string{
		"event: response.completed",
		`data: {"response":{"id":"resp_u","usage":{"input_tokens":42,"output_tokens":7,"total_tokens":49}}}`,
		"",
	}, "\n")
	svc := &OpenAIGatewayService{}
	usage := svc.parseSSEUsageFromBody(body)
	require.NotNil(t, usage)
	require.Equal(t, 42, usage.InputTokens, "must extract input_tokens from event-named terminal")
	require.Equal(t, 7, usage.OutputTokens, "must extract output_tokens")
}

// TestRound23_NoTerminalReturnsFalse — neither data-only nor event-named
// terminal present → helper returns false. Don't false-positive on
// in-progress events.
func TestRound23_NoTerminalReturnsFalse(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.in_progress","response":{"id":"r"}}`,
		"",
	}, "\n")
	_, _, ok := extractOpenAISSETerminalEvent(body)
	require.False(t, ok, "in_progress events MUST NOT register as terminal")
}

// TestRound23_DoneSentinelDoesNotPatch — the [DONE] sentinel is not a
// JSON object and the patch helper must leave it alone.
func TestRound23_DoneSentinelDoesNotPatch(t *testing.T) {
	got := openAICompatPayloadWithEventType("[DONE]", "response.completed")
	require.Equal(t, "[DONE]", got)
}
