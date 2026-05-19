package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// codex round 33 / fu52 (2026-05-19): regression tests for adapted
// upstream PR #2523 — OpenAI OAuth path drops item_reference + ids
// under store=false, with a per-account opt-in (opts.StoreEnabled +
// explicit `store:true` in body) to keep references for accounts that
// have ChatGPT internal store=true negotiated.

// TestRound33_RequestStoreExplicitTrue — the helper used as the second
// condition for preserveReferences. Conservative: only honor explicit
// boolean true.
func TestRound33_RequestStoreExplicitTrue(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want bool
	}{
		{"explicit true", map[string]any{"store": true}, true},
		{"explicit false", map[string]any{"store": false}, false},
		{"missing field", map[string]any{"model": "gpt-5.2"}, false},
		{"nil body", nil, false},
		{"non-boolean string 'true'", map[string]any{"store": "true"}, false},
		{"non-boolean number 1", map[string]any{"store": 1}, false},
		{"non-boolean null", map[string]any{"store": nil}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, requestStoreExplicitTrue(tc.body),
				"requestStoreExplicitTrue must only honor explicit literal true")
		})
	}
}

// TestRound33_OAuthDefaultDropsItemReference — opts.StoreEnabled=false
// (default OAuth path). item_reference must be dropped from input
// even when the body would have otherwise triggered the old
// NeedsToolContinuation=true gate.
func TestRound33_OAuthDefaultDropsItemReference(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.2",
		"input": []any{
			map[string]any{"type": "item_reference", "id": "fc_prior_call"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok", "id": "out_1"},
		},
	}
	res := applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{
		StoreEnabled: false,
	})
	require.True(t, res.Modified)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 1, "item_reference must be dropped on OAuth default path")

	survivor, ok := input[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "function_call_output", survivor["type"])
	require.Equal(t, "fc_1", survivor["call_id"], "call_*→fc_* normalization still applies")
	_, hasID := survivor["id"]
	require.False(t, hasID, "function_call_output id MUST be stripped under store=false")
}

// TestRound33_OAuthDefaultStripsMessageID — even non-reference items
// lose their `id` field under store=false. Mirrors PR #2523's
// requirement.
func TestRound33_OAuthDefaultStripsMessageID(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.2",
		"input": []any{
			map[string]any{"type": "message", "id": "msg_keep_me", "role": "user", "content": "hi"},
		},
	}
	applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{StoreEnabled: false})

	input, _ := reqBody["input"].([]any)
	require.Len(t, input, 1)
	survivor, _ := input[0].(map[string]any)
	require.Equal(t, "message", survivor["type"])
	_, hasID := survivor["id"]
	require.False(t, hasID, "message id MUST be stripped under store=false")
}

// TestRound33_StoreEnabledAccountWithExplicitStoreTruePreservesRefs —
// the carve-out: when opts.StoreEnabled (per-account toggle) AND the
// body explicitly carries store:true, item_reference and ids ARE
// preserved (assuming NeedsToolContinuation also returns true).
func TestRound33_StoreEnabledAccountWithExplicitStoreTruePreservesRefs(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.2",
		"store": true,
		"input": []any{
			// function_call + matching function_call_output → satisfies
			// NeedsToolContinuation, which requires a tool call context.
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "echo", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok", "id": "out_1"},
			map[string]any{"type": "item_reference", "id": "fc_prior"},
		},
	}
	applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{StoreEnabled: true})

	input, _ := reqBody["input"].([]any)
	// All 3 items survive — item_reference NOT dropped on this path.
	require.Len(t, input, 3, "item_reference MUST be preserved when opts.StoreEnabled+explicit store:true")
	// The item_reference's id is still present.
	var foundRef bool
	for _, item := range input {
		m, _ := item.(map[string]any)
		if m["type"] == "item_reference" {
			foundRef = true
			require.NotEmpty(t, m["id"])
		}
	}
	require.True(t, foundRef, "item_reference must survive on preserved path")
}

// TestRound33_StoreEnabledAccountWithoutExplicitStoreDropsRefs —
// negative complement: even on a StoreEnabled account, if the request
// body does NOT explicitly set store:true, we still drop refs. This is
// the conservative behavior codex required ("不要因为字段缺失就默认保留
// 引用, 避免又踩 store=false 404").
func TestRound33_StoreEnabledAccountWithoutExplicitStoreDropsRefs(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.2",
		// no `store` field at all
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "echo", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
			map[string]any{"type": "item_reference", "id": "fc_prior"},
		},
	}
	applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{StoreEnabled: true})

	input, _ := reqBody["input"].([]any)
	// item_reference dropped because body didn't explicitly carry store:true.
	require.Len(t, input, 2, "missing store field on a StoreEnabled account must still drop refs (codex round33 conservative gate)")
}

// TestRound33_PostTransformOrphanOutputFlagSet — the orphan-output
// detector for codex item 5: after dropping item_reference, if input
// has function_call_output but no inline function_call/tool_call and
// no previous_response_id, set PostTransformRequiresLocalReject.
func TestRound33_PostTransformOrphanOutputFlagSet(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.2",
		"input": []any{
			// only item_reference (→ dropped) + function_call_output (→ orphan)
			map[string]any{"type": "item_reference", "id": "fc_prior"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
		},
	}
	res := applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{StoreEnabled: false})
	require.True(t, res.PostTransformRequiresLocalReject,
		"orphan function_call_output after ref drop MUST set the local-reject flag")
}

// TestRound33_PostTransformWithPreviousResponseIDDoesNotFlag — when
// previous_response_id is set, the continuation context is provided
// out-of-band; the orphan-output state is still valid upstream.
func TestRound33_PostTransformWithPreviousResponseIDDoesNotFlag(t *testing.T) {
	reqBody := map[string]any{
		"model":                "gpt-5.2",
		"previous_response_id": "resp_prior_123",
		"input": []any{
			map[string]any{"type": "item_reference", "id": "fc_prior"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
		},
	}
	res := applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{StoreEnabled: false})
	require.False(t, res.PostTransformRequiresLocalReject,
		"previous_response_id provides continuation context — must not flag")
}

// TestRound33_PostTransformWithInlineToolCallDoesNotFlag — the
// canonical Responses-API continuation shape: function_call +
// matching function_call_output both inlined. No flag.
func TestRound33_PostTransformWithInlineToolCallDoesNotFlag(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.2",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "echo", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
		},
	}
	res := applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{StoreEnabled: false})
	require.False(t, res.PostTransformRequiresLocalReject,
		"inline function_call provides continuation context — must not flag")
}

// TestRound33_PostTransformWithoutOutputDoesNotFlag — no
// function_call_output at all → no flag (most regular requests).
func TestRound33_PostTransformWithoutOutputDoesNotFlag(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.2",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hi"},
		},
	}
	res := applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{StoreEnabled: false})
	require.False(t, res.PostTransformRequiresLocalReject,
		"no function_call_output → no flag")
}
