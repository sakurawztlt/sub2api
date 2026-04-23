package service

import "testing"

import "github.com/stretchr/testify/require"

func TestNormalizeOpenAIMessagesDispatchModelConfig(t *testing.T) {
	t.Parallel()

	cfg := normalizeOpenAIMessagesDispatchModelConfig(OpenAIMessagesDispatchModelConfig{
		OpusMappedModel:   " gpt-5.4-high ",
		SonnetMappedModel: "gpt-5.3-codex",
		HaikuMappedModel:  " gpt-5.4-mini-medium ",
		ExactModelMappings: map[string]string{
			" claude-sonnet-4-5-20250929 ": " gpt-5.2-high ",
			"":                             "gpt-5.4",
			"claude-opus-4-6":              " ",
		},
	})

	require.Equal(t, "gpt-5.4", cfg.OpusMappedModel)
	require.Equal(t, "gpt-5.3-codex", cfg.SonnetMappedModel)
	require.Equal(t, "gpt-5.4-mini", cfg.HaikuMappedModel)
	require.Equal(t, map[string]string{
		"claude-sonnet-4-5-20250929": "gpt-5.2",
	}, cfg.ExactModelMappings)
}

// TestResolveMessagesDispatchModel_GroupDefaultFallback locks in the
// fork-local catch-all: when a Group has DefaultMappedModel set but no
// family-specific OpusMappedModel/SonnetMappedModel/HaikuMappedModel,
// Anthropic requests must resolve to the group default, not the
// hard-coded family constants. Regression test for cctest opus-4-7
// 85% → 60% crash after 2026-04-23 upstream merge accidentally removed
// this fork patch.
func TestResolveMessagesDispatchModel_GroupDefaultFallback(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		group          *Group
		requestedModel string
		want           string
	}{
		{
			name:           "opus: group default when no family config",
			group:          &Group{DefaultMappedModel: "gpt-5.2-high"},
			requestedModel: "claude-opus-4-7",
			want:           "gpt-5.2-high",
		},
		{
			name:           "sonnet: group default when no family config",
			group:          &Group{DefaultMappedModel: "gpt-5.2-high"},
			requestedModel: "claude-sonnet-4-6",
			want:           "gpt-5.2-high",
		},
		{
			name:           "haiku: group default when no family config",
			group:          &Group{DefaultMappedModel: "gpt-5.2-mini"},
			requestedModel: "claude-haiku-4-5",
			want:           "gpt-5.2-mini",
		},
		{
			name: "family-specific config beats group default",
			group: &Group{
				DefaultMappedModel: "gpt-5.2-high",
				MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
					OpusMappedModel: "gpt-5.4-high",
				},
			},
			requestedModel: "claude-opus-4-7",
			want:           "gpt-5.4",
		},
		{
			name: "exact mapping beats everything",
			group: &Group{
				DefaultMappedModel: "gpt-5.2",
				MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
					OpusMappedModel: "gpt-5.4",
					ExactModelMappings: map[string]string{
						"claude-opus-4-7": "gpt-5.5",
					},
				},
			},
			requestedModel: "claude-opus-4-7",
			want:           "gpt-5.5",
		},
		{
			name:           "no config at all: hard-coded family default kicks in",
			group:          &Group{},
			requestedModel: "claude-opus-4-7",
			want:           "gpt-5.4",
		},
		{
			name:           "no config at all: sonnet hard-coded default",
			group:          &Group{},
			requestedModel: "claude-sonnet-4-5",
			want:           "gpt-5.3-codex",
		},
		{
			name:           "non-claude model returns empty",
			group:          &Group{DefaultMappedModel: "gpt-5.2"},
			requestedModel: "gpt-5.4",
			want:           "",
		},
		{
			name:           "nil group returns empty",
			group:          nil,
			requestedModel: "claude-opus-4-7",
			want:           "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.group.ResolveMessagesDispatchModel(tc.requestedModel)
			require.Equal(t, tc.want, got)
		})
	}
}
