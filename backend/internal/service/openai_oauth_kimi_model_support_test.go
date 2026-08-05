package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsModelSupported_OpenAIOAuthEmptyMappingRejectsBareKimiK3(t *testing.T) {
	account := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{},
	}

	for _, model := range []string{"k3", "k3-256k", "provider/k3"} {
		t.Run(model, func(t *testing.T) {
			require.False(t, account.IsModelSupported(model))
		})
	}

	for _, model := range []string{"my-k3-alias", "k3-custom", "gpt-5.4"} {
		t.Run(model, func(t *testing.T) {
			require.True(t, account.IsModelSupported(model))
		})
	}
}

func TestIsModelSupported_OpenAIOAuthExplicitKimiMappingUnchanged(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"k3": "gpt-5.4"},
		},
	}

	require.True(t, account.IsModelSupported("k3"))
	require.False(t, account.IsModelSupported("k3-256k"))
}

func TestIsModelSupported_OpenAIPassthroughStillAllowsBareKimiK3(t *testing.T) {
	account := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{},
		Extra:       map[string]any{"openai_passthrough": true},
	}

	require.True(t, account.IsModelSupported("k3"))
}

func TestIsBareKimiK3Model(t *testing.T) {
	require.True(t, isBareKimiK3Model("k3"))
	require.True(t, isBareKimiK3Model("k3-256k"))
	require.True(t, isBareKimiK3Model("provider/k3"))
	require.False(t, isBareKimiK3Model("my-k3-alias"))
}
