package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// codex round 24 fu41 (2026-05-18): non-blocking follow-ups to fu40's
// PR #2530 port:
//
//   (1) CRLF compatibility in openAICompatSSEFrameParser — bufio.Scanner
//       strips CR/LF on the streaming paths, but body-level callers use
//       strings.Split(body, "\n") which leaves "\r" trailing each line.
//       Without trimming, "\r" alone fails the blank-line frame-boundary
//       check and Form A terminals get silently dropped on CRLF bodies.
//
//   (2) Native OpenAI Responses passthrough (handleStreamingResponsePassthrough
//       and handleStreamingResponse) still consulted only data.type for
//       terminal/failed/usage detection. Add openAICompatLineEventTracker
//       so the same Form A terminal recognition works on these passthrough
//       paths without changing what bytes get forwarded to the client.

// ============================ CRLF parser tests ============================

func TestRound24_OpenAICompatSSEFrameParser_CRLFBlankLineFlushesFrame(t *testing.T) {
	var p openAICompatSSEFrameParser
	_, _ = p.AddLine("event: response.completed\r")
	_, _ = p.AddLine(`data: {"response":{"id":"resp_crlf"}}` + "\r")
	frame, ok := p.AddLine("\r") // blank line with CR — must still flush
	require.True(t, ok, "CRLF blank line MUST trigger frame dispatch")
	require.Equal(t, "response.completed", frame.EventType)
	require.Equal(t, `{"response":{"id":"resp_crlf"}}`, frame.Data,
		"CR must be stripped from data payload before joining")
}

func TestRound24_OpenAICompatSSEFrameParser_CRLFEventLineStripped(t *testing.T) {
	var p openAICompatSSEFrameParser
	_, _ = p.AddLine("event: response.in_progress\r")
	// blank line flushes — but we want to verify the event name got
	// captured without trailing CR even though extractOpenAISSEEventLine
	// already TrimSpaces; this is a defense-in-depth check.
	_, _ = p.AddLine(`data: {"response":{}}`)
	frame, ok := p.AddLine("")
	require.True(t, ok)
	require.Equal(t, "response.in_progress", frame.EventType)
}

// TestRound24_ExtractOpenAISSETerminalEvent_CRLFBody — body-level
// body uses strings.Split on "\n" and feeds lines (with trailing "\r")
// through the parser. CRLF trim happens inside the parser; the helper
// must recognize Form A terminals end-to-end on CRLF bodies.
func TestRound24_ExtractOpenAISSETerminalEvent_CRLFBody(t *testing.T) {
	// Use "\r\n" line endings — the real-world wire format for many
	// HTTP servers' SSE output.
	body := strings.Join([]string{
		`event: response.in_progress`,
		`data: {"response":{"id":"resp_crlf"}}`,
		``,
		`event: response.completed`,
		`data: {"response":{"id":"resp_crlf","usage":{"input_tokens":12,"output_tokens":34}}}`,
		``,
	}, "\r\n")
	gotType, gotPayload, ok := extractOpenAISSETerminalEvent(body)
	require.True(t, ok, "Form A terminal MUST be recognized on CRLF body")
	require.Equal(t, "response.completed", gotType)
	require.Contains(t, string(gotPayload), `"input_tokens":12`)
}

func TestRound24_ParseSSEUsageFromBody_CRLFBody(t *testing.T) {
	body := strings.Join([]string{
		`event: response.completed`,
		`data: {"response":{"id":"resp","usage":{"input_tokens":11,"output_tokens":22}}}`,
		``,
	}, "\r\n")
	svc := &OpenAIGatewayService{}
	usage := svc.parseSSEUsageFromBody(body)
	require.NotNil(t, usage)
	require.Equal(t, 11, usage.InputTokens, "usage from Form A terminal MUST parse on CRLF body")
	require.Equal(t, 22, usage.OutputTokens)
}

// ============================ Line tracker tests ============================

func TestRound24_LineEventTracker_FormAPatchesTypeOnDataLine(t *testing.T) {
	var t1 openAICompatLineEventTracker

	// event: opens scope
	patched, isData := t1.Update("event: response.completed")
	require.False(t, isData)
	require.Equal(t, "", patched)
	require.Equal(t, "response.completed", t1.eventName)

	// data: line — patcher should inject `type` from the open event name
	patched, isData = t1.Update(`data: {"response":{"id":"r"}}`)
	require.True(t, isData, "data: line must report isData=true")
	require.Equal(t, "response.completed", gjson.Get(patched, "type").String(),
		"type MUST be patched from open event name; got %s", patched)
	require.Equal(t, "r", gjson.Get(patched, "response.id").String())

	// blank line closes scope
	_, _ = t1.Update("")
	require.Equal(t, "", t1.eventName)
}

func TestRound24_LineEventTracker_FormBPassesThroughUnchanged(t *testing.T) {
	var t1 openAICompatLineEventTracker
	// No event: line — data already has type
	patched, isData := t1.Update(`data: {"type":"response.completed","response":{}}`)
	require.True(t, isData)
	require.Equal(t, "response.completed", gjson.Get(patched, "type").String())
	// Tracker has no event scope open
	require.Equal(t, "", t1.eventName)
}

func TestRound24_LineEventTracker_DoneSentinelLeftAlone(t *testing.T) {
	var t1 openAICompatLineEventTracker
	_, _ = t1.Update("event: response.completed")
	patched, isData := t1.Update(`data: [DONE]`)
	require.True(t, isData)
	require.Equal(t, "[DONE]", patched, "[DONE] sentinel MUST NOT be type-patched")
}

func TestRound24_LineEventTracker_CRLFCompat(t *testing.T) {
	var t1 openAICompatLineEventTracker
	_, _ = t1.Update("event: response.completed\r")
	patched, isData := t1.Update(`data: {"response":{}}` + "\r")
	require.True(t, isData)
	require.Equal(t, "response.completed", gjson.Get(patched, "type").String())
	// blank CR line closes scope
	_, _ = t1.Update("\r")
	require.Equal(t, "", t1.eventName)
}

func TestRound24_LineEventTracker_CommentLineNoOp(t *testing.T) {
	var t1 openAICompatLineEventTracker
	_, _ = t1.Update("event: response.completed")
	_, isData := t1.Update(": keepalive comment")
	require.False(t, isData)
	require.Equal(t, "response.completed", t1.eventName,
		"comment line MUST NOT close the event scope")
}

func TestRound24_LineEventTracker_BlankLineResetsEventName(t *testing.T) {
	var t1 openAICompatLineEventTracker
	_, _ = t1.Update("event: response.in_progress")
	_, _ = t1.Update(`data: {"response":{}}`)
	_, _ = t1.Update("")
	require.Equal(t, "", t1.eventName, "blank line MUST close event scope")
	// New event begins fresh
	_, _ = t1.Update("event: response.completed")
	require.Equal(t, "response.completed", t1.eventName)
}
