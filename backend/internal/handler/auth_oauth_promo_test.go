//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOAuthPromoCookieCaptureAndClear(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/linuxdo/start?promo_code=%20WELCOME%20", nil)
	captureOAuthPromoCode(c, true)
	cookie := findCookie(w.Result().Cookies(), oauthPromoCodeCookieName)
	require.NotNil(t, cookie)
	require.True(t, cookie.HttpOnly)
	require.True(t, cookie.Secure)
	require.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	require.Equal(t, oauthPendingBrowserCookiePath, cookie.Path)
	require.Equal(t, oauthPendingCookieMaxAgeSec, cookie.MaxAge)
	c.Request.AddCookie(cookie)
	require.Equal(t, "WELCOME", readOAuthPromoCode(c))

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/linuxdo/start", nil)
	captureOAuthPromoCode(c, false)
	cookie = findCookie(w.Result().Cookies(), oauthPromoCodeCookieName)
	require.NotNil(t, cookie)
	require.Less(t, cookie.MaxAge, 0)
	require.Empty(t, cookie.Value)
	require.Empty(t, readOAuthPromoCode(c))
}

func TestOAuthPromoPendingStateSurvivesCallbackCookieClear(t *testing.T) {
	h, client := newOAuthPendingFlowTestHandler(t, false)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/oauth/linuxdo/callback", nil)
	c.Request.AddCookie(&http.Cookie{Name: oauthPromoCodeCookieName, Value: encodeCookieValue(" WELCOME ")})
	require.NoError(t, h.createOAuthPendingSession(c, oauthPendingSessionPayload{
		Intent: "login",
		Identity: service.PendingAuthIdentityKey{
			ProviderType:    "linuxdo",
			ProviderKey:     "linuxdo",
			ProviderSubject: "promo-pending-subject",
		},
		ResolvedEmail:     "pending-promo@example.com",
		BrowserSessionKey: "promo-browser-key",
		CompletionResponse: map[string]any{
			"registration_required": true,
		},
	}))
	clearOAuthPromoCodeCookie(c, false)
	session, err := client.PendingAuthSession.Query().Only(context.Background())
	require.NoError(t, err)
	require.Equal(t, "WELCOME", pendingOAuthPromoCode(session))
	completion, ok := readCompletionResponse(session.LocalFlowState)
	require.True(t, ok)
	require.Equal(t, true, completion["registration_required"])
	require.NotContains(t, w.Body.String(), "WELCOME")
	require.Less(t, findCookie(w.Result().Cookies(), oauthPromoCodeCookieName).MaxAge, 0)
}

func TestOAuthPromoMalformedCookieDoesNotCreateCode(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/callback", nil)
	c.Request.AddCookie(&http.Cookie{Name: oauthPromoCodeCookieName, Value: "%%%"})
	require.Empty(t, readOAuthPromoCode(c))
	require.Empty(t, pendingOAuthPromoCode(nil))
}
