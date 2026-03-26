package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	pkgopenai "github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type openaiOAuthClientCodexBulkStub struct {
	responses map[string]*pkgopenai.TokenResponse
	errs      map[string]error
	calls     []openaiOAuthBulkCall
}

type openaiOAuthBulkCall struct {
	RefreshToken string
	ProxyURL     string
	ClientID     string
}

func (s *openaiOAuthClientCodexBulkStub) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*pkgopenai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientCodexBulkStub) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*pkgopenai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientCodexBulkStub) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*pkgopenai.TokenResponse, error) {
	s.calls = append(s.calls, openaiOAuthBulkCall{
		RefreshToken: refreshToken,
		ProxyURL:     proxyURL,
		ClientID:     clientID,
	})
	if err := s.errs[refreshToken]; err != nil {
		return nil, err
	}
	if resp, ok := s.responses[refreshToken]; ok {
		return resp, nil
	}
	return nil, infraerrors.New(http.StatusUnauthorized, "OPENAI_RT_INVALID", "invalid refresh token")
}

func TestOpenAIOAuthHandler_CodexBulkImportCreatesAccounts(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	adminSvc := newStubAdminService()
	now := time.Now().UTC()
	adminSvc.proxyCounts = []service.ProxyWithAccountCount{
		{
			Proxy: service.Proxy{
				ID:        21,
				Name:      "proxy-main",
				Protocol:  "http",
				Host:      "10.10.0.1",
				Port:      9001,
				Status:    service.StatusActive,
				CreatedAt: now,
				UpdatedAt: now,
			},
			AccountCount:  0,
			LatencyStatus: "success",
			QualityStatus: "healthy",
			QualityScore:  intPtr(95),
		},
	}

	openaiClient := &openaiOAuthClientCodexBulkStub{
		responses: map[string]*pkgopenai.TokenResponse{
			"rt-ok": {AccessToken: "at-ok", RefreshToken: "", ExpiresIn: 3600},
		},
		errs: map[string]error{
			"rt-bad": infraerrors.New(http.StatusUnauthorized, "OPENAI_RT_INVALID", "invalid refresh token"),
		},
	}
	oauthSvc := service.NewOpenAIOAuthService(nil, openaiClient)
	defer oauthSvc.Stop()

	handler := NewOpenAIOAuthHandler(oauthSvc, adminSvc)
	router := gin.New()
	router.POST("/admin/openai/codex/bulk-import", handler.CodexBulkImport)

	rateMultiplier := 1.25
	loadFactor := 5
	body := map[string]any{
		"batch_id":                "batch-2",
		"name_template":           "codex-{batch}",
		"refresh_tokens":          []string{"rt-ok", "   ", "rt-bad"},
		"proxy_pool_ids":          []int64{21},
		"accounts_per_proxy":      4,
		"concurrency":             6,
		"priority":                7,
		"notes":                   "  bulk note  ",
		"rate_multiplier":         rateMultiplier,
		"load_factor":             loadFactor,
		"skip_default_group_bind": true,
	}

	rec := performJSONRequest(t, router, http.MethodPost, "/admin/openai/codex/bulk-import", body)
	require.Equal(t, http.StatusOK, rec.Code)

	var payload struct {
		Data CodexBulkImportResult `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))

	require.Equal(t, "batch-2", payload.Data.BatchID)
	require.Equal(t, 3, payload.Data.Summary.RequestedCount)
	require.Equal(t, 2, payload.Data.Summary.ParsedCount)
	require.Equal(t, 1, payload.Data.Summary.CreatedCount)
	require.Equal(t, 1, payload.Data.Summary.FailedCount)
	require.Len(t, payload.Data.Items, 2)
	require.Equal(t, "created", payload.Data.Items[0].Status)
	require.Equal(t, "failed", payload.Data.Items[1].Status)

	require.Len(t, adminSvc.createdAccounts, 1)
	created := adminSvc.createdAccounts[0]
	require.Equal(t, "codex-batch-2-001", created.Name)
	require.Equal(t, service.PlatformOpenAI, created.Platform)
	require.Equal(t, service.AccountTypeOAuth, created.Type)
	require.NotNil(t, created.ProxyID)
	require.EqualValues(t, 21, *created.ProxyID)
	require.Equal(t, 6, created.Concurrency)
	require.Equal(t, 7, created.Priority)
	require.Equal(t, []int64(nil), created.GroupIDs)
	require.True(t, created.SkipDefaultGroupBind)
	require.NotNil(t, created.RateMultiplier)
	require.InDelta(t, rateMultiplier, *created.RateMultiplier, 0.0001)
	require.NotNil(t, created.LoadFactor)
	require.Equal(t, loadFactor, *created.LoadFactor)
	require.NotNil(t, created.Notes)
	require.Equal(t, "bulk note", *created.Notes)
	require.Equal(t, "rt-ok", created.Credentials["refresh_token"])
	require.Equal(t, true, created.Extra["openai_passthrough"])
	require.Equal(t, true, created.Extra["codex_cli_only"])
	require.Equal(t, codexImportSource, created.Extra["import_source"])
	require.Equal(t, "batch-2", created.Extra["import_batch_id"])
}

func TestOpenAIOAuthHandler_CodexBulkImportPreservesExplicitZeroPriority(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	adminSvc := newStubAdminService()
	now := time.Now().UTC()
	adminSvc.proxyCounts = []service.ProxyWithAccountCount{
		{
			Proxy: service.Proxy{
				ID:        31,
				Name:      "proxy-zero",
				Protocol:  "http",
				Host:      "10.20.0.1",
				Port:      9002,
				Status:    service.StatusActive,
				CreatedAt: now,
				UpdatedAt: now,
			},
			AccountCount:  0,
			LatencyStatus: "success",
			QualityStatus: "healthy",
			QualityScore:  intPtr(90),
		},
	}

	openaiClient := &openaiOAuthClientCodexBulkStub{
		responses: map[string]*pkgopenai.TokenResponse{
			"rt-zero": {AccessToken: "at-zero", RefreshToken: "rt-zero", ExpiresIn: 3600},
		},
	}
	oauthSvc := service.NewOpenAIOAuthService(nil, openaiClient)
	defer oauthSvc.Stop()

	handler := NewOpenAIOAuthHandler(oauthSvc, adminSvc)
	router := gin.New()
	router.POST("/admin/openai/codex/bulk-import", handler.CodexBulkImport)

	body := map[string]any{
		"batch_id":           "batch-zero",
		"refresh_tokens":     []string{"rt-zero"},
		"proxy_pool_ids":     []int64{31},
		"accounts_per_proxy": 2,
		"priority":           0,
	}

	rec := performJSONRequest(t, router, http.MethodPost, "/admin/openai/codex/bulk-import", body)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, adminSvc.createdAccounts, 1)
	require.Equal(t, 0, adminSvc.createdAccounts[0].Priority)
}

func makeOpenAIIDToken(t *testing.T, email, planType string) string {
	t.Helper()

	payload := map[string]any{
		"email": email,
		"exp":   time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type": planType,
		},
	}

	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	return "header." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
}

func performJSONRequest(t *testing.T, router http.Handler, method, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()

	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func intPtr(v int) *int {
	return &v
}
