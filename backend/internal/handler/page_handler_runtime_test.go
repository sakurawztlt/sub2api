package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pageRuntimeSettingRepo struct {
	values map[string]string
}

func (r *pageRuntimeSettingRepo) Get(_ context.Context, key string) (*service.Setting, error) {
	value, err := r.GetValue(context.Background(), key)
	if err != nil {
		return nil, err
	}
	return &service.Setting{Key: key, Value: value}, nil
}

func (r *pageRuntimeSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", service.ErrSettingNotFound
}

func (r *pageRuntimeSettingRepo) Set(_ context.Context, key, value string) error {
	r.values[key] = value
	return nil
}

func (r *pageRuntimeSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			values[key] = value
		}
	}
	return values, nil
}

func (r *pageRuntimeSettingRepo) SetMultiple(_ context.Context, settings map[string]string) error {
	for key, value := range settings {
		r.values[key] = value
	}
	return nil
}

func (r *pageRuntimeSettingRepo) GetAll(context.Context) (map[string]string, error) {
	values := make(map[string]string, len(r.values))
	for key, value := range r.values {
		values[key] = value
	}
	return values, nil
}

func (r *pageRuntimeSettingRepo) Delete(_ context.Context, key string) error {
	delete(r.values, key)
	return nil
}

func TestRegisterPageRoutesEnforcesVisibilityAndServesRuntimeFiles(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dataDir := t.TempDir()
	pagesDir := filepath.Join(dataDir, "pages")
	require.NoError(t, os.MkdirAll(filepath.Join(pagesDir, "guide", "images"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(pagesDir, "staff"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pagesDir, "guide.md"), []byte("# Guide\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pagesDir, "staff.md"), []byte("# Staff\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pagesDir, "guide", "images", "logo.png"), []byte("png"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pagesDir, "staff", "logo.png"), []byte("secret"), 0o644))

	menuJSON := `[
		{"url":"md:guide","visibility":"user"},
		{"page_slug":"staff","url":"md:legacy-staff","visibility":"admin"}
	]`
	settingService := service.NewSettingService(&pageRuntimeSettingRepo{values: map[string]string{
		service.SettingKeyCustomMenuItems: menuJSON,
	}}, &config.Config{})

	jwtAuth := func(c *gin.Context) {
		switch c.GetHeader("Authorization") {
		case "Bearer user":
			c.Set(string(servermiddleware.ContextKeyUserRole), "user")
		case "Bearer admin":
			c.Set(string(servermiddleware.ContextKeyUserRole), "admin")
		default:
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	}
	adminAuth := func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer admin" {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		c.Set(string(servermiddleware.ContextKeyUserRole), "admin")
		c.Next()
	}

	router := gin.New()
	RegisterPageRoutes(router.Group("/api/v1"), dataDir, jwtAuth, adminAuth, settingService)

	doRequest := func(method, path, authorization string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, nil)
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		router.ServeHTTP(recorder, request)
		return recorder
	}

	t.Run("content requires authentication", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, doRequest(http.MethodGet, "/api/v1/pages/guide", "").Code)
	})

	t.Run("user page returns markdown", func(t *testing.T) {
		response := doRequest(http.MethodGet, "/api/v1/pages/guide", "Bearer user")
		assert.Equal(t, http.StatusOK, response.Code)
		assert.Equal(t, "text/markdown; charset=utf-8", response.Header().Get("Content-Type"))
		assert.Equal(t, "# Guide\n", response.Body.String())
	})

	t.Run("admin page is hidden from users", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, doRequest(http.MethodGet, "/api/v1/pages/staff", "Bearer user").Code)
		assert.Equal(t, http.StatusOK, doRequest(http.MethodGet, "/api/v1/pages/staff", "Bearer admin").Code)
	})

	t.Run("unconfigured slug is hidden", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, doRequest(http.MethodGet, "/api/v1/pages/orphan", "Bearer admin").Code)
	})

	t.Run("nested user image is public but admin image is hidden", func(t *testing.T) {
		image := doRequest(http.MethodGet, "/api/v1/pages/guide/images/images/logo.png", "")
		assert.Equal(t, http.StatusOK, image.Code)
		assert.Equal(t, "png", image.Body.String())
		assert.Equal(t, http.StatusNotFound, doRequest(http.MethodGet, "/api/v1/pages/staff/images/logo.png", "").Code)
	})

	t.Run("page list is admin only", func(t *testing.T) {
		assert.Equal(t, http.StatusForbidden, doRequest(http.MethodGet, "/api/v1/pages", "Bearer user").Code)
		response := doRequest(http.MethodGet, "/api/v1/pages", "Bearer admin")
		assert.Equal(t, http.StatusOK, response.Code)
		assert.Contains(t, response.Body.String(), `"guide"`)
		assert.Contains(t, response.Body.String(), `"staff"`)
	})
}
