package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeKnownOpenAICodexModel(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  string
	}{
		{name: "vendor prefix gpt55", model: "openai/gpt5.5", want: "gpt-5.5"},
		{name: "gpt56 sol", model: "gpt-5.6-sol-high", want: "gpt-5.6-sol"},
		{name: "gpt56 terra spaced", model: "gpt 5.6 terra", want: "gpt-5.6-terra"},
		{name: "gpt56 luna compact suffix", model: "gpt-5.6-luna-openai-compact", want: "gpt-5.6-luna"},
		{name: "codex spark alias", model: "gpt-5.3codexspark", want: "gpt-5.3-codex-spark"},
		{name: "claude is not openai", model: "claude-opus-4-8", want: ""},
		{name: "gemini is not openai", model: "gemini-3.1-pro", want: ""},
		{name: "unknown openai is not known", model: "gpt-unknown-model", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, normalizeKnownOpenAICodexModel(tt.model))
		})
	}
}

func TestIsOpenAIGPT54ModelUsesKnownAliasesOnly(t *testing.T) {
	require.True(t, isOpenAIGPT54Model("openai/gpt5.5"))
	require.True(t, isOpenAIGPT54Model("gpt-5.6-sol-high"))
	require.False(t, isOpenAIGPT54Model("claude-opus-4-8"))
	require.False(t, isOpenAIGPT54Model("gemini-3.1-pro"))
	require.False(t, isOpenAIGPT54Model("gpt-unknown-model"))
}
