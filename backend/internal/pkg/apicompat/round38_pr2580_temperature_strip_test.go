package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codex round39 fu57 / upstream PR #2580 (2026-05-20): gpt-5.x reasoning
// models served via the Responses API reject temperature / top_p.
// AnthropicToResponses and ChatCompletionsToResponses must strip these
// for any model whose name starts with "gpt-5".

func TestRound38_AnthropicToResponses_TemperatureStrippedForReasoningModel(t *testing.T) {
	temp := 0.7
	req := &AnthropicRequest{
		Model:       "gpt-5.2",
		MaxTokens:   1024,
		Messages:    []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		Temperature: &temp,
		TopP:        &temp,
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	assert.Nil(t, resp.Temperature, "reasoning model: temperature must be stripped")
	assert.Nil(t, resp.TopP, "reasoning model: top_p must be stripped")

	// Verify the fields are absent from the serialised JSON — *float64 with
	// omitempty MUST omit nil, otherwise upstream still gets the 400.
	b, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.NotContains(t, string(b), `"temperature"`)
	assert.NotContains(t, string(b), `"top_p"`)
}

func TestRound38_AnthropicToResponses_TemperatureStrippedForAllGpt5Variants(t *testing.T) {
	temp := 1.0
	// Cover the full known variant surface to catch a future hyphen typo
	// or refactor that narrows the prefix match too aggressively.
	models := []string{"gpt-5", "gpt-5.2", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex", "gpt-5.5", "gpt-5-turbo"}
	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			req := &AnthropicRequest{
				Model:       model,
				MaxTokens:   1024,
				Messages:    []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
				Temperature: &temp,
				TopP:        &temp,
			}
			resp, err := AnthropicToResponses(req)
			require.NoError(t, err)
			assert.Nil(t, resp.Temperature, "model %s: temperature must be stripped", model)
			assert.Nil(t, resp.TopP, "model %s: top_p must be stripped", model)
		})
	}
}

func TestRound38_AnthropicToResponses_TemperaturePreservedForNonReasoningModel(t *testing.T) {
	// Sanity: don't over-strip — gpt-4o etc. still need sampling
	// parameters. Regression guard if isReasoningModel matcher ever
	// widens too far.
	temp := 0.5
	models := []string{"gpt-4o", "gpt-4o-mini", "gpt-4.1", "claude-sonnet-4-6"}
	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			req := &AnthropicRequest{
				Model:       model,
				MaxTokens:   1024,
				Messages:    []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
				Temperature: &temp,
				TopP:        &temp,
			}
			resp, err := AnthropicToResponses(req)
			require.NoError(t, err)
			require.NotNil(t, resp.Temperature, "non-reasoning model %s: temperature must be preserved", model)
			require.NotNil(t, resp.TopP, "non-reasoning model %s: top_p must be preserved", model)
			assert.Equal(t, temp, *resp.Temperature)
			assert.Equal(t, temp, *resp.TopP)
		})
	}
}

func TestRound38_ChatCompletionsToResponses_TemperatureStrippedForReasoningModel(t *testing.T) {
	temp := 0.7
	req := &ChatCompletionsRequest{
		Model: "gpt-5.2",
		Messages: []ChatMessage{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
		},
		Temperature: &temp,
		TopP:        &temp,
	}

	resp, err := ChatCompletionsToResponses(req)
	require.NoError(t, err)
	assert.Nil(t, resp.Temperature, "reasoning model via Chat Completions bridge: temperature must be stripped")
	assert.Nil(t, resp.TopP, "reasoning model via Chat Completions bridge: top_p must be stripped")

	b, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.NotContains(t, string(b), `"temperature"`)
	assert.NotContains(t, string(b), `"top_p"`)
}

func TestRound38_ChatCompletionsToResponses_TemperaturePreservedForNonReasoningModel(t *testing.T) {
	temp := 0.5
	req := &ChatCompletionsRequest{
		Model: "gpt-4o",
		Messages: []ChatMessage{
			{Role: "user", Content: json.RawMessage(`"Hi"`)},
		},
		Temperature: &temp,
		TopP:        &temp,
	}
	resp, err := ChatCompletionsToResponses(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Temperature)
	require.NotNil(t, resp.TopP)
	assert.Equal(t, temp, *resp.Temperature)
	assert.Equal(t, temp, *resp.TopP)
}

// Pin the matcher behavior — if a future refactor changes isReasoningModel
// to look at a model registry instead of a prefix, the tests above still
// catch behavior, but pinning the predicate itself helps surface intent.
func TestRound38_IsReasoningModel_PrefixMatches(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-5", true},
		{"gpt-5.2", true},
		{"gpt-5.4-mini", true},
		{"gpt-5.3-codex", true},
		{"gpt-4o", false},
		{"gpt-4o-mini", false},
		{"gpt-4.1", false},
		{"claude-sonnet-4-6", false},
		{"o1-preview", false}, // future-proof: o1 is reasoning but NOT a gpt-5 prefix, and upstream PR #2580 specifically targets gpt-5
		{"", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, isReasoningModel(tc.model), "model %q", tc.model)
	}
}
