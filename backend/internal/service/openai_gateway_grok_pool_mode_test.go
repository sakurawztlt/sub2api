package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHandleGrokAccountUpstreamErrorDefaultCooldownsRespectPoolMode(t *testing.T) {
	for _, statusCode := range []int{
		http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
	} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			svc := &OpenAIGatewayService{}
			account := &Account{
				ID:       int64(4800 + statusCode),
				Platform: PlatformGrok,
				Type:     AccountTypeAPIKey,
				Credentials: map[string]any{
					"pool_mode": true,
				},
			}

			svc.handleGrokAccountUpstreamError(
				context.Background(), account, statusCode, nil,
				[]byte(`{"error":{"message":"grok access or entitlement denied"}}`),
			)

			require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
			require.Nil(t, account.TempUnschedulableUntil)
			require.Empty(t, account.TempUnschedulableReason)
		})
	}
}
