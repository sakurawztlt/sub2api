package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestMessagesForwardRouteKind(t *testing.T) {
	t.Run("anthropic group routed to openai oauth uses openai compat forwarder", func(t *testing.T) {
		kind := messagesForwardRouteKind(
			&service.Group{Platform: service.PlatformAnthropic},
			&service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth},
		)
		require.Equal(t, messagesForwardRouteOpenAICompat, kind)
	})

	t.Run("anthropic native accounts keep anthropic forwarder", func(t *testing.T) {
		kind := messagesForwardRouteKind(
			&service.Group{Platform: service.PlatformAnthropic},
			&service.Account{Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth},
		)
		require.Equal(t, messagesForwardRouteAnthropicNative, kind)
	})
}
