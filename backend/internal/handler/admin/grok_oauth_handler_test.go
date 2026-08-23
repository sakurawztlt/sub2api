//go:build unit

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type grokQuotaHandlerAccountRepo struct {
	service.AccountRepository
	account *service.Account
	updates map[int64]map[string]any
}

type grokOAuthReconcilerStub struct {
	input  service.GrokOAuthReconcileInput
	calls  int
	result *service.GrokOAuthReconcileResult
	err    error
}

type grokSSOImportOAuthClient struct {
	mu      sync.Mutex
	active  int
	maxSeen int
	started chan struct{}
	release chan struct{}
}

func (c *grokSSOImportOAuthClient) ExchangeCode(context.Context, string, string, string, string, string) (*xai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (c *grokSSOImportOAuthClient) RefreshToken(context.Context, string, string, string) (*xai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (c *grokSSOImportOAuthClient) LoginWithPassword(context.Context, string, string, string) (*service.GrokPasswordLoginResult, error) {
	return nil, errors.New("not implemented")
}

func (c *grokSSOImportOAuthClient) ConvertSSOToBuild(ctx context.Context, _, _ string) (*xai.TokenResponse, error) {
	c.mu.Lock()
	c.active++
	if c.active > c.maxSeen {
		c.maxSeen = c.active
	}
	c.mu.Unlock()
	c.started <- struct{}{}
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	return &xai.TokenResponse{
		AccessToken:  "converted-access-token",
		RefreshToken: "converted-refresh-token",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
	}, nil
}

func (c *grokSSOImportOAuthClient) maxConcurrency() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxSeen
}

func (s *grokOAuthReconcilerStub) ReconcileGrokOAuth(_ context.Context, input service.GrokOAuthReconcileInput) (*service.GrokOAuthReconcileResult, error) {
	s.calls++
	s.input = input
	return s.result, s.err
}

func (r *grokQuotaHandlerAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	if r.account != nil && r.account.ID == id {
		return r.account, nil
	}
	return nil, service.ErrAccountNotFound
}

func (r *grokQuotaHandlerAccountRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	if r.updates == nil {
		r.updates = make(map[int64]map[string]any)
	}
	r.updates[id] = updates
	return nil
}

type grokQuotaHandlerUpstream struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
}

func (u *grokQuotaHandlerUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	u.mu.Lock()
	u.requests = append(u.requests, req)
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()
	if req.URL.Path == "/v1/responses" {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"X-Ratelimit-Limit-Requests":     []string{"10"},
				"X-Ratelimit-Remaining-Requests": []string{"8"},
			},
			Body: io.NopCloser(strings.NewReader(`{"id":"resp_probe"}`)),
		}, nil
	}
	payload := `{"config":{"billingPeriodStart":"2026-07-01T00:00:00Z","billingPeriodEnd":"2026-08-01T00:00:00Z"}}`
	if req.URL.RawQuery == "format=credits" {
		payload = `{"config":{"currentPeriod":{"type":"WEEKLY","start":"2026-07-09T03:25:00Z","end":"2026-07-16T03:25:00Z"}}}`
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
}

func (u *grokQuotaHandlerUpstream) DoWithTLS(
	req *http.Request,
	proxyURL string,
	accountID int64,
	accountConcurrency int,
	_ *tlsfingerprint.Profile,
) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func TestGrokOAuthHandlerQueryQuotaProbesUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	repo := &grokQuotaHandlerAccountRepo{account: &service.Account{
		ID:          42,
		Platform:    service.PlatformGrok,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"expires_at":    time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		},
	}}
	upstream := &grokQuotaHandlerUpstream{}
	quotaService := service.NewGrokQuotaService(repo, nil, service.NewGrokTokenProvider(repo, nil), upstream, nil)
	handler := NewGrokOAuthHandler(nil, nil, quotaService, nil)

	router := gin.New()
	router.GET("/api/v1/admin/grok/accounts/:id/quota", handler.QueryQuota)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/grok/accounts/42/quota", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"source":"hybrid_probe"`)
	require.Contains(t, rec.Body.String(), `"billing":`)
	require.Contains(t, rec.Body.String(), `"snapshot":`)
	require.Contains(t, rec.Body.String(), `"headers_observed":true`)
	require.NotContains(t, rec.Body.String(), "access-token")
	require.Eventually(t, func() bool {
		upstream.mu.Lock()
		defer upstream.mu.Unlock()
		return len(upstream.requests) == 4
	}, time.Second, 10*time.Millisecond)
	upstream.mu.Lock()
	requests := append([]*http.Request(nil), upstream.requests...)
	bodies := append([][]byte(nil), upstream.bodies...)
	upstream.mu.Unlock()
	require.Len(t, requests, 4)
	responsesProbeSeen := false
	modelsSyncSeen := false
	for i, upstreamReq := range requests {
		require.Equal(t, "Bearer access-token", upstreamReq.Header.Get("Authorization"))
		if upstreamReq.URL.String() == xai.DefaultCLIBaseURL+"/responses" {
			responsesProbeSeen = true
			require.Equal(t, "application/json, text/event-stream", upstreamReq.Header.Get("Accept"))
			require.Contains(t, string(bodies[i]), `"model":"grok-4.5"`)
			require.Contains(t, string(bodies[i]), `"input":"hi"`)
			require.Contains(t, string(bodies[i]), `"stream":true`)
			require.NotContains(t, string(bodies[i]), `"max_output_tokens"`)
			require.NotContains(t, string(bodies[i]), `"store"`)
		}
		if upstreamReq.URL.String() == xai.DefaultCLIBaseURL+"/models" {
			modelsSyncSeen = true
		}
	}
	require.True(t, responsesProbeSeen)
	require.True(t, modelsSyncSeen)
	require.NotNil(t, repo.updates[42])
}

func TestGrokOAuthHandlerResetQuotaReturnsUnsupported(t *testing.T) {
	gin.SetMode(gin.TestMode)

	repo := &grokQuotaHandlerAccountRepo{account: &service.Account{
		ID:       43,
		Platform: service.PlatformGrok,
		Type:     service.AccountTypeOAuth,
	}}
	quotaService := service.NewGrokQuotaService(repo, nil, nil, nil, nil)
	handler := NewGrokOAuthHandler(nil, nil, quotaService, nil)

	router := gin.New()
	router.POST("/api/v1/admin/grok/accounts/:id/reset-quota", handler.ResetQuota)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/grok/accounts/43/reset-quota", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotImplemented, rec.Code)
	require.Contains(t, rec.Body.String(), `"reason":"GROK_QUOTA_RESET_UNSUPPORTED"`)
	require.NotContains(t, rec.Body.String(), "access-token")
}

func TestGrokOAuthHandlerRuntimeSanityDoesNotExposeSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(xai.EnvBaseURL, "http://127.0.0.1:8080/v1?access_token=secret")
	t.Setenv(xai.EnvClientID, "client-secret-like-value")

	handler := NewGrokOAuthHandler(nil, nil, nil, nil)
	router := gin.New()
	router.GET("/api/v1/admin/grok/runtime-sanity", handler.RuntimeSanity)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/grok/runtime-sanity", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"public_gateway_scope":"responses_only"`)
	require.Contains(t, rec.Body.String(), `"valid":false`)
	require.NotContains(t, rec.Body.String(), "access_token")
	require.NotContains(t, rec.Body.String(), "secret")
	require.NotContains(t, rec.Body.String(), "client-secret-like-value")
}

func TestGrokSSOImportNormalizesAndDeduplicatesTokens(t *testing.T) {
	tokens := normalizeSSOImportTokens(
		[]string{" sso=token-a; other=value\ntoken-b ", "cookie: sso-rw=token-a"},
		"token-c, token-b",
	)

	require.Equal(t, []string{"token-c", "token-b", "token-a"}, tokens)
}

func TestGrokSSOImportExpiryUsesTokenExpiryWithoutRefreshToken(t *testing.T) {
	tokenExpiry := time.Now().Add(6 * time.Hour).Unix()
	expiresAt, autoPause := grokSSOImportExpiry(nil, nil, &service.GrokTokenInfo{ExpiresAt: tokenExpiry})

	require.NotNil(t, expiresAt)
	require.Equal(t, tokenExpiry, *expiresAt)
	require.NotNil(t, autoPause)
	require.True(t, *autoPause)
}

func TestGrokSSOImportExpiryUsesEarlierRequestedExpiryWithoutRefreshToken(t *testing.T) {
	requestedExpiry := time.Now().Add(2 * time.Hour).Unix()
	tokenExpiry := time.Now().Add(6 * time.Hour).Unix()
	requestedAutoPause := false
	expiresAt, autoPause := grokSSOImportExpiry(&requestedExpiry, &requestedAutoPause, &service.GrokTokenInfo{ExpiresAt: tokenExpiry})

	require.NotNil(t, expiresAt)
	require.Equal(t, requestedExpiry, *expiresAt)
	require.NotNil(t, autoPause)
	require.True(t, *autoPause)
}

func TestGrokSSOImportExpiryPreservesRequestSettingsWithRefreshToken(t *testing.T) {
	requestedExpiry := time.Now().Add(2 * time.Hour).Unix()
	requestedAutoPause := false
	expiresAt, autoPause := grokSSOImportExpiry(&requestedExpiry, &requestedAutoPause, &service.GrokTokenInfo{
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(6 * time.Hour).Unix(),
	})

	require.Same(t, &requestedExpiry, expiresAt)
	require.Same(t, &requestedAutoPause, autoPause)
}

func TestGrokSSOImportCredentialsKeepsOperatorConfigAndRejectsRawSecrets(t *testing.T) {
	built := map[string]any{
		"access_token":  "built-access",
		"refresh_token": "built-refresh",
		"base_url":      xai.DefaultCLIBaseURL,
		"password":      "unexpected-built-password",
	}
	reqCredentials := map[string]any{
		"access_token":            "request-access",
		"refresh_token":           "request-refresh",
		"password":                "raw-password",
		"sso_token":               "raw-sso",
		"cookie":                  "raw-cookie",
		"base_url":                " https://relay.example.com/v1 ",
		"header_override_enabled": true,
		"header_overrides":        map[string]any{"x-relay-key": "k"},
	}

	credentials := grokSSOImportCredentials(built, reqCredentials)

	require.Equal(t, "built-access", credentials["access_token"])
	require.Equal(t, "built-refresh", credentials["refresh_token"])
	require.Equal(t, "https://relay.example.com/v1", credentials["base_url"])
	require.Equal(t, true, credentials["header_override_enabled"])
	require.Equal(t, map[string]any{"x-relay-key": "k"}, credentials["header_overrides"])
	for _, key := range []string{"password", "sso_token", "sso", "sso-rw", "cookie"} {
		require.NotContains(t, credentials, key)
	}
	// Shared worker input remains immutable.
	require.Equal(t, "request-access", reqCredentials["access_token"])
	require.Equal(t, " https://relay.example.com/v1 ", reqCredentials["base_url"])
}

func TestGrokSSOImportErrorMessageDoesNotEchoRawErrors(t *testing.T) {
	message := grokSSOImportErrorMessage(errors.New("upstream rejected sso-secret-value"))
	require.Equal(t, "internal error", message)
	require.NotContains(t, message, "sso-secret-value")

	message = grokSSOImportErrorMessage(infraerrors.New(
		http.StatusBadGateway,
		"GROK_OAUTH_INVALID_TOKEN_RESPONSE",
		"upstream body and URL contain sso-secret-value",
	))
	require.Equal(t, "GROK_OAUTH_INVALID_TOKEN_RESPONSE: Grok SSO conversion returned an invalid token response", message)
	require.NotContains(t, message, "sso-secret-value")
}

func TestGrokSSOImportExtraRecursivelyRemovesSecrets(t *testing.T) {
	extra := map[string]any{
		"safe": "value",
		"nested": map[string]any{
			"password": "raw-password",
			"child":    []any{map[string]any{"SSO_TOKEN": "raw-sso", "keep": true}},
		},
		"access_token": "raw-access",
	}

	cloned := cloneGrokSSOMap(extra)

	require.Equal(t, "value", cloned["safe"])
	require.NotContains(t, cloned, "access_token")
	nested := cloned["nested"].(map[string]any)
	require.NotContains(t, nested, "password")
	child := nested["child"].([]any)[0].(map[string]any)
	require.NotContains(t, child, "SSO_TOKEN")
	require.Equal(t, true, child["keep"])
	// The shared request map is never mutated by workers.
	require.Equal(t, "raw-access", extra["access_token"])
}

func TestGrokSSOImportWorkerHandlesMissingOAuthService(t *testing.T) {
	handler := &GrokOAuthHandler{}
	result := handler.safeCreateAccountFromSSOToken(context.Background(), GrokSSOToOAuthRequest{}, "raw-sso-secret", 2, 3)

	require.False(t, result.created)
	require.Equal(t, 2, result.item.Index)
	require.Contains(t, result.item.Error, "GROK_OAUTH_CLIENT_NOT_CONFIGURED")
	require.NotContains(t, result.item.Error, "raw-sso-secret")
}

func TestGrokOAuthHandlerSSOImportRejectsInvalidJSONWithoutEchoingInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewGrokOAuthHandler(nil, nil, nil, nil)
	router := gin.New()
	router.POST("/api/v1/admin/grok/sso-to-oauth", handler.CreateAccountsFromSSO)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/grok/sso-to-oauth", strings.NewReader(`{"sso_tokens":["raw-sso-secret"]`))
	req.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.NotContains(t, rec.Body.String(), "raw-sso-secret")
}

func TestGrokOAuthHandlerSSOImportRejectsOversizedBatchBeforeConversion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewGrokOAuthHandler(nil, nil, nil, nil)
	router := gin.New()
	router.POST("/api/v1/admin/grok/sso-to-oauth", handler.CreateAccountsFromSSO)
	rec := httptest.NewRecorder()
	tokens := make([]string, grokSSOImportMaxItems+1)
	for i := range tokens {
		tokens[i] = "token-" + strconv.Itoa(i)
	}
	body, err := json.Marshal(GrokSSOToOAuthRequest{SSOTokens: tokens})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/grok/sso-to-oauth", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "maximum batch size")
}

func TestGrokOAuthHandlerSSOImportBoundsConcurrencyAndNeverPersistsOrEchoesRawSSO(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client := &grokSSOImportOAuthClient{
		started: make(chan struct{}, 4),
		release: make(chan struct{}),
	}
	oauthService := service.NewGrokOAuthService(nil, client)
	defer oauthService.Stop()
	adminService := newStubAdminService()
	handler := NewGrokOAuthHandler(oauthService, adminService, nil, nil)
	router := gin.New()
	router.POST("/api/v1/admin/grok/sso-to-oauth", handler.CreateAccountsFromSSO)
	rec := httptest.NewRecorder()
	body := `{
		"sso_tokens":["raw-sso-1","raw-sso-2","raw-sso-3","raw-sso-4"],
		"credentials":{"sso_token":"raw-credential-sso","password":"raw-password","base_url":"https://relay.example.com/v1"},
		"extra":{"nested":{"sso-rw":"raw-extra-sso"},"safe":"value"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/grok/sso-to-oauth", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		router.ServeHTTP(rec, req)
	}()
	for i := 0; i < grokSSOImportConcurrency; i++ {
		select {
		case <-client.started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for bounded SSO workers")
		}
	}
	require.Equal(t, grokSSOImportConcurrency, client.maxConcurrency())
	close(client.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SSO import response")
	}

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"created"`)
	for _, secret := range []string{"raw-sso-1", "raw-sso-2", "raw-sso-3", "raw-sso-4", "raw-credential-sso", "raw-password", "raw-extra-sso", "converted-access-token", "converted-refresh-token"} {
		require.NotContains(t, rec.Body.String(), secret)
	}
	adminService.mu.Lock()
	created := append([]*service.CreateAccountInput(nil), adminService.createdAccounts...)
	adminService.mu.Unlock()
	require.Len(t, created, 4)
	for _, input := range created {
		require.Equal(t, "https://relay.example.com/v1", input.Credentials["base_url"])
		require.Equal(t, "converted-access-token", input.Credentials["access_token"])
		require.Equal(t, "converted-refresh-token", input.Credentials["refresh_token"])
		require.NotContains(t, input.Credentials, "sso_token")
		require.NotContains(t, input.Credentials, "password")
		require.Equal(t, "value", input.Extra["safe"])
		require.Empty(t, input.Extra["nested"].(map[string]any))
	}
}

func TestGrokOAuthHandlerReconcileDefaultsToDryRun(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reconciler := &grokOAuthReconcilerStub{result: &service.GrokOAuthReconcileResult{
		DryRun:     true,
		Scanned:    2,
		Actionable: 1,
		WouldBlock: 1,
		Items: []service.GrokOAuthReconcileItem{{
			AccountID: 42,
			Reason:    service.GrokOAuthReconcileReasonMissingRefreshToken,
			Action:    service.GrokOAuthReconcileActionBlock,
			Outcome:   service.GrokOAuthReconcileOutcomePlanned,
		}},
	}}
	handler := NewGrokOAuthHandler(nil, nil, nil, reconciler)
	router := gin.New()
	router.POST("/api/v1/admin/grok/oauth/reconcile", handler.ReconcileOAuthAccounts)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/grok/oauth/reconcile", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, reconciler.calls)
	require.True(t, reconciler.input.DryRun)
	require.False(t, reconciler.input.Apply)
	require.Contains(t, rec.Body.String(), `"reason":"missing_refresh_token"`)
	require.NotContains(t, rec.Body.String(), `"refresh_token":`)
	require.NotContains(t, rec.Body.String(), `"access_token":`)
}

func TestGrokOAuthHandlerReconcileRequiresExplicitApply(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reconciler := &grokOAuthReconcilerStub{}
	handler := NewGrokOAuthHandler(nil, nil, nil, reconciler)
	router := gin.New()
	router.POST("/api/v1/admin/grok/oauth/reconcile", handler.ReconcileOAuthAccounts)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/grok/oauth/reconcile", strings.NewReader(`{"dry_run":false}`))
	req.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Zero(t, reconciler.calls)
	require.NotContains(t, rec.Body.String(), "credentials")
}

func TestGrokOAuthHandlerReconcileExplicitApply(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reconciler := &grokOAuthReconcilerStub{result: &service.GrokOAuthReconcileResult{DryRun: false, Refreshed: 1}}
	handler := NewGrokOAuthHandler(nil, nil, nil, reconciler)
	router := gin.New()
	router.POST("/api/v1/admin/grok/oauth/reconcile", handler.ReconcileOAuthAccounts)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/grok/oauth/reconcile", strings.NewReader(`{"apply":true,"dry_run":false,"after_id":10,"limit":25,"refresh_window_seconds":3600}`))
	req.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, reconciler.calls)
	require.True(t, reconciler.input.Apply)
	require.False(t, reconciler.input.DryRun)
	require.Equal(t, int64(10), reconciler.input.AfterID)
	require.Equal(t, 25, reconciler.input.Limit)
	require.Equal(t, time.Hour, reconciler.input.RefreshWindow)
}
