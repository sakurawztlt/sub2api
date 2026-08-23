package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/stretchr/testify/require"
)

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
			// codex round54 fu64 (2026-05-21): Opus 默认从 gpt-5.4 → gpt-5.5.
			// 客户绕过 gcr 直连 sub2api 用 Anthropic-shape 时, Opus 也走 gpt-5.5
			// 跟 gcr ModelMap 一致. Sonnet/Haiku 默认不动 (Phase 1).
			name:           "no config at all: hard-coded opus family default → gpt-5.5",
			group:          &Group{},
			requestedModel: "claude-opus-4-7",
			want:           "gpt-5.5",
		},
		{
			name:           "opus 5 defaults to gpt-5.6-sol",
			group:          &Group{},
			requestedModel: "claude-opus-5",
			want:           "gpt-5.6-sol",
		},
		{
			name:           "future opus 5.1 keeps generic opus default",
			group:          &Group{},
			requestedModel: "claude-opus-5-1",
			want:           "gpt-5.5",
		},
		{
			name: "opus 5 upgrades the legacy opus family mapping",
			group: &Group{
				MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
					OpusMappedModel: "gpt-5.5",
				},
			},
			requestedModel: "claude-opus-5",
			want:           "gpt-5.6-sol",
		},
		{
			name: "opus 5 upgrades the legacy group default",
			group: &Group{
				DefaultMappedModel: "gpt-5.4",
			},
			requestedModel: "claude-opus-5",
			want:           "gpt-5.6-sol",
		},
		{
			name: "opus 5 preserves a non-legacy family mapping",
			group: &Group{
				MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
					OpusMappedModel: "gpt-5.6-sol-pro",
				},
			},
			requestedModel: "claude-opus-5",
			want:           "gpt-5.6-sol-pro",
		},
		{
			name: "opus 5 exact mapping beats the generation guard",
			group: &Group{
				MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
					OpusMappedModel: "gpt-5.5",
					ExactModelMappings: map[string]string{
						"claude-opus-5": "gpt-5.5-pro",
					},
				},
			},
			requestedModel: "claude-opus-5",
			want:           "gpt-5.5-pro",
		},
		{
			name:           "no config at all: sonnet hard-coded default",
			group:          &Group{},
			requestedModel: "claude-sonnet-4-5",
			want:           "gpt-5.4",
		},
		{
			name:           "no config at all: sonnet 5 hard-coded default",
			group:          &Group{},
			requestedModel: "claude-sonnet-5",
			want:           "gpt-5.5",
		},
		{
			name: "sonnet 5 legacy family mapping is upgraded",
			group: &Group{
				MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
					SonnetMappedModel: "gpt-5.4",
				},
			},
			requestedModel: "claude-sonnet-5",
			want:           "gpt-5.5",
		},
		{
			name: "sonnet 5 legacy group default is upgraded",
			group: &Group{
				DefaultMappedModel: "gpt-5.4",
			},
			requestedModel: "claude-sonnet-5",
			want:           "gpt-5.5",
		},
		{
			name: "sonnet 5 explicit gpt-5.5 family mapping is preserved",
			group: &Group{
				MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
					SonnetMappedModel: "gpt-5.5",
				},
			},
			requestedModel: "claude-sonnet-5",
			want:           "gpt-5.5",
		},
		{
			name: "sonnet 5 exact mapping beats family guard",
			group: &Group{
				MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
					SonnetMappedModel: "gpt-5.4",
					ExactModelMappings: map[string]string{
						"claude-sonnet-5": "gpt-5.5-pro",
					},
				},
			},
			requestedModel: "claude-sonnet-5",
			want:           "gpt-5.5-pro",
		},
		{
			name: "sonnet 4 legacy codex family mapping is guarded",
			group: &Group{
				MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
					SonnetMappedModel: "gpt-5.3-codex",
				},
			},
			requestedModel: "claude-sonnet-4-6",
			want:           "gpt-5.4",
		},
		{
			name: "sonnet 4 legacy codex group default is guarded",
			group: &Group{
				DefaultMappedModel: "gpt-5.3-codex",
			},
			requestedModel: "claude-sonnet-4-6",
			want:           "gpt-5.4",
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

func TestGroupResolveMessagesDispatchModel_GrokRequiresCrossClientMapping(t *testing.T) {
	original := xai.RuntimeModelMappingOptions()
	t.Cleanup(func() { xai.SetRuntimeModelMappingOptions(original) })
	group := &Group{Platform: PlatformGrok}

	xai.SetRuntimeModelMappingOptions(xai.ModelMappingOptions{})
	require.Empty(t, group.ResolveMessagesDispatchModel("claude-sonnet-4-5"))

	xai.SetRuntimeModelMappingOptions(xai.ModelMappingOptions{
		DefaultText:          "grok-build-0.1",
		EnableCrossClientMap: true,
	})
	require.Equal(t, "grok-build-0.1", group.ResolveMessagesDispatchModel("claude-sonnet-4-5"))
	require.Equal(t, "grok-build-0.1", group.ResolveMessagesDispatchModel("claude-opus-4-6"))
	require.Equal(t, "grok-build-0.1", group.ResolveMessagesDispatchModel("claude-haiku-4-5"))
	require.Empty(t, group.ResolveMessagesDispatchModel("grok"))
	require.Empty(t, group.ResolveMessagesDispatchModel("gpt-5.3-codex"))
}

func TestFallbackSonnetFiveMessagesDispatchModel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		requestedModel string
		currentMapped  string
		want           string
	}{
		{
			name:           "sonnet 5 primary gpt-5.5 falls back to gpt-5.4",
			requestedModel: "claude-sonnet-5",
			currentMapped:  "gpt-5.5",
			want:           "gpt-5.4",
		},
		{
			name:           "sonnet 5 empty current mapping falls back",
			requestedModel: "claude-sonnet-5",
			currentMapped:  "",
			want:           "gpt-5.4",
		},
		{
			name:           "sonnet 5 explicit non-default mapping is preserved",
			requestedModel: "claude-sonnet-5",
			currentMapped:  "gpt-5.5-pro",
			want:           "",
		},
		{
			name:           "sonnet 4 does not use sonnet 5 fallback",
			requestedModel: "claude-sonnet-4-6",
			currentMapped:  "gpt-5.5",
			want:           "",
		},
		{
			name:           "opus does not use sonnet 5 fallback",
			requestedModel: "claude-opus-4-8",
			currentMapped:  "gpt-5.5",
			want:           "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FallbackSonnetFiveMessagesDispatchModel(tc.requestedModel, tc.currentMapped)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestSanitizeGroupMessagesDispatchFields_ClearsNonOpenAIPlatform(t *testing.T) {
	t.Parallel()

	group := &Group{
		Platform:              PlatformAnthropic,
		AllowMessagesDispatch: true,
		DefaultMappedModel:    "gpt-5.6-sol",
		MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
			SonnetMappedModel: "gpt-5.3-codex",
			ExactModelMappings: map[string]string{
				"claude-fable-5": "gpt-5.6-sol",
			},
		},
	}

	sanitizeGroupMessagesDispatchFields(group)

	require.False(t, group.AllowMessagesDispatch)
	require.Empty(t, group.DefaultMappedModel)
	require.Equal(t, OpenAIMessagesDispatchModelConfig{}, group.MessagesDispatchModelConfig)
}

func TestSanitizeGroupMessagesDispatchFields_PreservesCompositeDispatchToggle(t *testing.T) {
	t.Parallel()

	group := &Group{
		Platform:              PlatformComposite,
		AllowMessagesDispatch: true,
		DefaultMappedModel:    "gpt-5.6-sol",
		MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
			SonnetMappedModel: "gpt-5.3-codex",
			ExactModelMappings: map[string]string{
				"claude-fable-5": "gpt-5.6-sol",
			},
		},
	}

	sanitizeGroupMessagesDispatchFields(group)

	require.True(t, group.AllowMessagesDispatch)
	require.Empty(t, group.DefaultMappedModel)
	require.Equal(t, OpenAIMessagesDispatchModelConfig{}, group.MessagesDispatchModelConfig)
}
