package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// codex round41 fu59 (2026-05-20): hardening tests for IsReasoningModel.
// The fu57 implementation was `strings.HasPrefix(model, "gpt-5")`, which
// missed two real-world shapes:
//
//   (1) Mixed-case model ids: NewAPI / our own admin UI sometimes round-
//       trip model names with different casing (e.g. "GPT-5.2"). A
//       missed strip there means temperature/top_p still flow to gpt-5.
//
//   (2) Vendor-prefixed forms: some clients pass model ids like
//       "openai/gpt-5.4" (LiteLLM / OpenRouter style). The original
//       HasPrefix("gpt-5") matcher would falsely classify these as
//       non-reasoning and re-emit sampling parameters.
//
// fu59 IsReasoningModel: trim + lowercase + strip single vendor prefix
// segment (before first '/') + HasPrefix("gpt-5") check.

func TestRound41_IsReasoningModel_TrimsWhitespace(t *testing.T) {
	for _, m := range []string{"  gpt-5.2", "gpt-5.2  ", "  gpt-5.2  ", "\tgpt-5.2\n"} {
		require.True(t, IsReasoningModel(m), "must trim whitespace: %q", m)
	}
}

func TestRound41_IsReasoningModel_HandlesMixedCase(t *testing.T) {
	for _, m := range []string{"GPT-5.2", "Gpt-5", "gPt-5.4", "GPT-5.4-MINI", "GPT-5"} {
		require.True(t, IsReasoningModel(m), "must lowercase: %q", m)
	}
}

func TestRound41_IsReasoningModel_HandlesVendorPrefix(t *testing.T) {
	for _, m := range []string{
		"openai/gpt-5.2",
		"openai/gpt-5",
		"openai/gpt-5.4-mini",
		"OpenAI/GPT-5.2",
		"azure/gpt-5",
		"litellm/gpt-5.4",
		"openrouter/gpt-5.3-codex",
	} {
		require.True(t, IsReasoningModel(m), "must strip vendor prefix: %q", m)
	}
}

func TestRound41_IsReasoningModel_NotConfusedByNonGpt5Vendor(t *testing.T) {
	// Vendor-prefixed but model is NOT gpt-5 — must remain non-reasoning.
	for _, m := range []string{
		"openai/gpt-4o",
		"openai/gpt-4.1",
		"anthropic/claude-sonnet-4-6",
		"azure/gpt-3.5-turbo",
		"openrouter/o1-preview",
	} {
		require.False(t, IsReasoningModel(m), "must NOT classify as reasoning: %q", m)
	}
}

func TestRound41_IsReasoningModel_DoesNotFalseMatchOtherPrefixes(t *testing.T) {
	// Negative cases — must NOT match models whose name happens to share
	// some characters with gpt-5.
	for _, m := range []string{
		"gpt-4o", "gpt-4.1", "gpt-3.5-turbo",
		"o1-preview", "o1-mini",
		"claude-sonnet-4-6", "claude-opus-4-6",
		"gemini-1.5-pro",
		"deepseek-v3",
		"",
		"  ",
		"/", // empty input after slash-strip — returns false
	} {
		require.False(t, IsReasoningModel(m), "must NOT match: %q", m)
	}
}

func TestRound41_IsReasoningModel_AndPrivateAliasStayInSync(t *testing.T) {
	// The internal isReasoningModel alias MUST always agree with the
	// exported IsReasoningModel — they are deliberately synonymous so
	// converter sites and gateway sites read the same answer.
	for _, m := range []string{
		"gpt-5.2", "GPT-5", "openai/gpt-5.4-mini",
		"gpt-4o", "claude-sonnet-4-6", "",
		"openai/gpt-5", "OpenRouter/GPT-5.3-codex",
	} {
		require.Equal(t, IsReasoningModel(m), isReasoningModel(m),
			"exported and internal helpers MUST agree on %q", m)
	}
}

func TestRound41_IsReasoningModel_SlashCornerCases(t *testing.T) {
	// Various slash-position edge cases. The matcher must:
	//   - strip a real vendor prefix segment ("openai/gpt-5") → reasoning
	//   - still recognise a leading-slash form ("/gpt-5") as reasoning
	//     (vendor segment is empty, gets stripped, gpt-5 remains)
	//   - tolerate trailing slash on a reasoning model ("gpt-5/") — it
	//     still has the gpt-5 prefix, so it's reasoning. The idx<len-1
	//     guard in the matcher prevents the strip from consuming the
	//     whole string and returning a false negative.
	require.True(t, IsReasoningModel("openai/gpt-5"), "openai/gpt-5 stripped → gpt-5 → reasoning")
	require.True(t, IsReasoningModel("/gpt-5"), "leading-slash treated as empty vendor → gpt-5 → reasoning")
	require.True(t, IsReasoningModel("gpt-5/"), "trailing slash on reasoning model → prefix match still holds")
}
