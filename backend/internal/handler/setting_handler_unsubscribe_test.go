//go:build unit

package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type unsubscribeHandlerSettingRepo struct {
	service.SettingRepository
	values map[string]string
	writes int
}

func (r *unsubscribeHandlerSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	v, ok := r.values[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return v, nil
}
func (r *unsubscribeHandlerSettingRepo) Set(_ context.Context, key, value string) error {
	r.values[key] = value
	r.writes++
	return nil
}

func signedUnsubscribeHandlerToken(t *testing.T, secret, email, event string, exp int64) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"email": email, "event": event, "exp": exp})
	require.NoError(t, err)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, err = mac.Write([]byte(encoded))
	require.NoError(t, err)
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestSettingHandlerUnsubscribeSignedTokenAndEscapesHTML(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "unit-unsubscribe-secret"
	repo := &unsubscribeHandlerSettingRepo{values: map[string]string{"notification_email_unsubscribe_secret": secret}}
	notifications := service.NewNotificationEmailService(repo, nil)
	h := ProvideSettingHandler(nil, notifications, BuildInfo{Version: "test"})
	router := gin.New()
	router.GET("/api/v1/settings/email-unsubscribe", h.UnsubscribeNotificationEmail)
	email := `<script>alert("x")</script>@example.com`
	token := signedUnsubscribeHandlerToken(t, secret, email, service.NotificationEmailEventBalanceLow, time.Now().Add(time.Hour).Unix())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/settings/email-unsubscribe?token="+url.QueryEscape(token), nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Header().Get("Content-Type"), "text/html")
	require.NotContains(t, w.Body.String(), "<script>")
	require.Contains(t, w.Body.String(), "&lt;script&gt;")
	require.NotContains(t, w.Body.String(), secret)
	require.NotContains(t, w.Body.String(), token)
	unsubscribed, err := notifications.IsUnsubscribed(context.Background(), email, service.NotificationEmailEventBalanceLow)
	require.NoError(t, err)
	require.True(t, unsubscribed)
	require.Equal(t, 1, repo.writes)
}

func TestSettingHandlerUnsubscribeRejectsInvalidTokensWithoutPreferencesWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "unit-unsubscribe-secret"
	for _, tc := range []struct{ name, token string }{
		{"missing", ""},
		{"malformed", "bad"},
		{"wrong-signature", signedUnsubscribeHandlerToken(t, "other", "user@example.com", service.NotificationEmailEventBalanceLow, time.Now().Add(time.Hour).Unix())},
		{"expired", signedUnsubscribeHandlerToken(t, secret, "user@example.com", service.NotificationEmailEventBalanceLow, time.Now().Add(-time.Hour).Unix())},
		{"transactional", signedUnsubscribeHandlerToken(t, secret, "user@example.com", service.NotificationEmailEventBalanceRechargeSuccess, time.Now().Add(time.Hour).Unix())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &unsubscribeHandlerSettingRepo{values: map[string]string{"notification_email_unsubscribe_secret": secret}}
			h := ProvideSettingHandler(nil, service.NewNotificationEmailService(repo, nil), BuildInfo{})
			router := gin.New()
			router.GET("/unsubscribe", h.UnsubscribeNotificationEmail)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/unsubscribe?token="+url.QueryEscape(tc.token), nil))
			require.Equal(t, http.StatusBadRequest, w.Code)
			require.Zero(t, repo.writes)
		})
	}
}
