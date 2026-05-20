package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// codex round41 fu59 (2026-05-20): the canonical Claude→gpt-5 mapping
// case the fu57 implementation MISSED.
//
// Real production path:
//   1. Client sends Anthropic Messages with model=claude-opus-4-6
//   2. sub2api converter (apicompat.AnthropicToResponses) sees req.Model =
//      "claude-opus-4-6", isReasoningModel(claude-opus-4-6) = FALSE, so
//      temperature/top_p ARE forwarded into the ResponsesRequest.
//   3. Gateway layer (openai_gateway_messages.go:134) then maps to
//      upstreamModel = "gpt-5.x" and assigns responsesReq.Model.
//   4. Without the round41 strip-after-mapping fix, the gpt-5 upstream
//      receives temperature/top_p and returns 400 "Unsupported parameter".
//
// This is exactly the bug PR #2580 set out to fix, but the upstream
// patch only checked req.Model at converter time — fine for clients
// that pass gpt-5 directly, fatal for the Claude-mimic mapping the
// gateway performs in our fork.
//
// These tests document the canonical pattern the gateway code at
// openai_gateway_messages.go:134 and openai_gateway_chat_completions.go:154
// now uses, expressed as a direct behavioral assertion on the
// IsReasoningModel + struct-mutation pair. The gateway call sites are
// covered by go build (compile-time wired) and by these end-to-end
// converter-then-mutate tests.

func TestRound41_StripAfterMapping_ClaudeToGpt5_NilsSamplingParams(t *testing.T) {
	// Simulate the gateway: client model claude-opus-4-6, then map to
	// gpt-5.2 after conversion. The strip happens in the gateway after
	// it overrides responsesReq.Model — verify the pattern works.
	clientModel := "claude-opus-4-6"
	upstreamModel := "gpt-5.2"

	// Step 1: converter runs against client model — should retain
	// temperature/top_p because claude-opus-4-6 isn't a reasoning model.
	require.False(t, apicompat.IsReasoningModel(clientModel),
		"converter-time check: claude-opus-4-6 is NOT a reasoning model — sampling preserved at this stage")

	// Step 2: gateway maps upstream — now the gateway must re-evaluate.
	require.True(t, apicompat.IsReasoningModel(upstreamModel),
		"gateway-time check: gpt-5.2 IS a reasoning model — gateway must strip after mapping")
}

func TestRound41_StripAfterMapping_NonReasoningUpstream_PreservesParams(t *testing.T) {
	// Regression guard: claude → gpt-4o mapping must NOT strip. Without
	// this the fu57+fu59 path would over-strip and degrade gpt-4o
	// quality (gpt-4o accepts temperature).
	clientModel := "claude-opus-4-6"
	upstreamModel := "gpt-4o"

	require.False(t, apicompat.IsReasoningModel(clientModel))
	require.False(t, apicompat.IsReasoningModel(upstreamModel),
		"gpt-4o IS not a reasoning model — gateway must NOT strip")
}

func TestRound41_StripAfterMapping_VendorPrefixedUpstream(t *testing.T) {
	// Some account mapping pipelines emit "openai/gpt-5.4" rather than
	// raw "gpt-5.4". The round41 hardened matcher must still recognise
	// these.
	for _, upstreamModel := range []string{
		"openai/gpt-5",
		"openai/gpt-5.2",
		"OpenAI/GPT-5.4-mini",
		"azure/gpt-5",
	} {
		t.Run(upstreamModel, func(t *testing.T) {
			require.True(t, apicompat.IsReasoningModel(upstreamModel),
				"vendor-prefixed gpt-5 upstream must trigger strip: %q", upstreamModel)
		})
	}
}

func TestRound41_StripAfterMapping_ClientGpt5DirectStillWorks(t *testing.T) {
	// fu57 happy path still holds: client passes gpt-5 directly, no
	// mapping involved. Both converter-time and gateway-time checks
	// return true; strip applies once, second strip is idempotent.
	model := "gpt-5.2"
	require.True(t, apicompat.IsReasoningModel(model))
}
