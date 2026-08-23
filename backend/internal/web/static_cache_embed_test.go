//go:build embed

package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddedFrontendAppliesImmutableCacheOnlyToFingerprintedAssets(t *testing.T) {
	server, err := NewFrontendServer(&mockSettingsProvider{settings: map[string]string{"test": "value"}})
	require.NoError(t, err)

	entries, err := fs.ReadDir(server.distFS, "assets")
	require.NoError(t, err)
	fingerprintedPath := ""
	for _, entry := range entries {
		candidate := "assets/" + entry.Name()
		if !entry.IsDir() && isFingerprintedEmbeddedAssetPath(candidate) {
			fingerprintedPath = candidate
			break
		}
	}
	require.NotEmpty(t, fingerprintedPath)

	assertCachePolicy := func(t *testing.T, middleware gin.HandlerFunc) {
		t.Helper()
		router := gin.New()
		router.Use(middleware)

		assetResponse := httptest.NewRecorder()
		router.ServeHTTP(assetResponse, httptest.NewRequest(http.MethodGet, "/"+fingerprintedPath, nil))
		assert.Equal(t, http.StatusOK, assetResponse.Code)
		assert.Equal(t, staticAssetsCacheControl, assetResponse.Header().Get("Cache-Control"))

		logoResponse := httptest.NewRecorder()
		router.ServeHTTP(logoResponse, httptest.NewRequest(http.MethodGet, "/logo.png", nil))
		assert.Equal(t, http.StatusOK, logoResponse.Code)
		assert.Empty(t, logoResponse.Header().Get("Cache-Control"))
	}

	t.Run("settings injection middleware", func(t *testing.T) {
		assertCachePolicy(t, server.Middleware())
	})
	t.Run("legacy middleware", func(t *testing.T) {
		assertCachePolicy(t, ServeEmbeddedFrontend())
	})
}
