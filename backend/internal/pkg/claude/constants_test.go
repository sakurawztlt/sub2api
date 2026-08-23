package claude

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClaudeCLI2119DefaultBetaProfiles(t *testing.T) {
	require.Equal(t, strings.Join([]string{
		BetaClaudeCode,
		BetaInterleavedThinking,
		BetaContextManagement,
		BetaPromptCachingScope,
		BetaAdvisorTool,
		BetaEffort,
	}, ","), ClaudeCLI2119BetaHeader)

	require.Equal(t, strings.Join([]string{
		BetaClaudeCode,
		BetaOAuth,
		BetaInterleavedThinking,
		BetaContextManagement,
		BetaPromptCachingScope,
		BetaAdvisorTool,
		BetaEffort,
	}, ","), DefaultBetaHeader)
	require.Equal(t, ClaudeCLI2119BetaHeader, APIKeyBetaHeader)

	for _, header := range []string{ClaudeCLI2119BetaHeader, DefaultBetaHeader, APIKeyBetaHeader, CountTokensBetaHeader} {
		require.NotContains(t, header, BetaFineGrainedToolStreaming)
		require.NotContains(t, header, "prompt-caching-2024-07-31")
	}
}
