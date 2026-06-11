package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChannel_IsBedrockCCCompatEnabled_Bool(t *testing.T) {
	ch := &Channel{
		FeaturesConfig: map[string]any{
			featureKeyBedrockCCCompat: true,
		},
	}

	require.True(t, ch.IsBedrockCCCompatEnabled("anthropic"))
	require.True(t, ch.IsBedrockCCCompatEnabled("openai"))
}

func TestChannel_IsBedrockCCCompatEnabled_PlatformMap(t *testing.T) {
	ch := &Channel{
		FeaturesConfig: map[string]any{
			featureKeyBedrockCCCompat: map[string]any{
				"anthropic": true,
				"openai":    false,
			},
		},
	}

	require.True(t, ch.IsBedrockCCCompatEnabled("anthropic"))
	require.False(t, ch.IsBedrockCCCompatEnabled("openai"))
}

func TestChannel_IsBedrockCCCompatEnabled_TypedPlatformMap(t *testing.T) {
	ch := &Channel{
		FeaturesConfig: map[string]any{
			featureKeyBedrockCCCompat: map[string]bool{
				"anthropic": true,
				"openai":    false,
			},
		},
	}

	require.True(t, ch.IsBedrockCCCompatEnabled("anthropic"))
	require.False(t, ch.IsBedrockCCCompatEnabled("openai"))
}
