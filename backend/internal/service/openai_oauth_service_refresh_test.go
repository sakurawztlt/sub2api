package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

type openaiOAuthClientRefreshStub struct {
	refreshCalls int32
}

func (s *openaiOAuthClientRefreshStub) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientRefreshStub) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	atomic.AddInt32(&s.refreshCalls, 1)
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientRefreshStub) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	atomic.AddInt32(&s.refreshCalls, 1)
	return nil, errors.New("not implemented")
}

func TestOpenAIOAuthService_RefreshAccountToken_NoRefreshTokenUsesExistingAccessToken(t *testing.T) {
	client := &openaiOAuthClientRefreshStub{}
	svc := NewOpenAIOAuthService(nil, client)
	var privacyClientCalls int32
	svc.SetPrivacyClientFactory(func(proxyURL string) (*req.Client, error) {
		atomic.AddInt32(&privacyClientCalls, 1)
		return nil, errors.New("stop before request")
	})

	expiresAt := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	account := &Account{
		ID:       77,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "existing-access-token",
			"expires_at":   expiresAt,
			"client_id":    "client-id-1",
		},
	}

	info, err := svc.RefreshAccountToken(context.Background(), account)
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, "existing-access-token", info.AccessToken)
	require.Equal(t, "client-id-1", info.ClientID)
	require.Zero(t, atomic.LoadInt32(&client.refreshCalls), "existing access token should be reused without calling refresh")
	require.Positive(t, atomic.LoadInt32(&privacyClientCalls), "existing access token should still run enrichment")
}

func TestOpenAIOAuthServiceRefreshAccountTokenPATIgnoresStaleRefreshToken(t *testing.T) {
	client := &openaiOAuthClientRefreshStub{}
	var whoamiCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&whoamiCalls, 1)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"email":"user@example.com","chatgpt_user_id":"user-123",
			"chatgpt_account_id":"acct-123","chatgpt_plan_type":"plus",
			"chatgpt_account_is_fedramp":false
		}`))
	}))
	defer server.Close()
	originalURL := openAICodexPATWhoamiURL
	openAICodexPATWhoamiURL = server.URL
	defer func() { openAICodexPATWhoamiURL = originalURL }()

	svc := NewOpenAIOAuthService(nil, client)
	defer svc.Stop()
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		"access_token": "at-test-token", "refresh_token": "stale-refresh-token",
		"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "auth_mode": "personal_access_token",
	}}
	info, err := svc.RefreshAccountToken(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, OpenAIAuthModePersonalAccessToken, info.AuthMode)
	require.Equal(t, "at-test-token", info.AccessToken)
	require.Empty(t, info.RefreshToken)
	require.Equal(t, int32(1), atomic.LoadInt32(&whoamiCalls))
	require.Zero(t, atomic.LoadInt32(&client.refreshCalls))
}

func TestOpenAITokenRefresherNeedsRefreshSkipsPATWithStaleOAuthFields(t *testing.T) {
	refresher := NewOpenAITokenRefresher(nil, nil)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		"access_token": "at-test-token", "refresh_token": "stale-refresh-token",
		"expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
		"auth_mode":  OpenAIAuthModePersonalAccessToken,
	}}
	require.False(t, refresher.NeedsRefresh(account, 5*time.Minute))
}

func TestOpenAITokenRefresherRefreshPATRemovesStaleOAuthFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"email":"user@example.com","chatgpt_user_id":"user-123",
			"chatgpt_account_id":"acct-123","chatgpt_plan_type":"plus",
			"chatgpt_account_is_fedramp":true
		}`))
	}))
	defer server.Close()
	originalURL := openAICodexPATWhoamiURL
	openAICodexPATWhoamiURL = server.URL
	defer func() { openAICodexPATWhoamiURL = originalURL }()

	svc := NewOpenAIOAuthService(nil, nil)
	defer svc.Stop()
	refresher := NewOpenAITokenRefresher(svc, nil)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		"access_token": "at-test-token", "refresh_token": "stale-refresh-token", "id_token": "stale-id-token",
		"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "expires_in": 3600,
		"client_id": "stale-client", "auth_mode": OpenAIAuthModePersonalAccessToken,
		"model_mapping": map[string]any{"gpt-5": "gpt-5-codex"},
	}}
	credentials, err := refresher.Refresh(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "at-test-token", credentials["access_token"])
	require.Equal(t, OpenAIAuthModePersonalAccessToken, credentials["auth_mode"])
	require.NotContains(t, credentials, "refresh_token")
	require.NotContains(t, credentials, "id_token")
	require.NotContains(t, credentials, "expires_at")
	require.NotContains(t, credentials, "expires_in")
	require.NotContains(t, credentials, "client_id")
	require.Equal(t, map[string]any{"gpt-5": "gpt-5-codex"}, credentials["model_mapping"])
}
