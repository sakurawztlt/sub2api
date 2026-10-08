package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRegisterBackupRoutesIncludesImageStorageAndStepUpBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{Backup: &adminhandler.BackupHandler{}}}
	stepUpCalls := 0
	registerBackupRoutes(router.Group("/api/v1/admin"), handlers, func(c *gin.Context) {
		stepUpCalls++
		c.AbortWithStatus(http.StatusForbidden)
	})

	registered := make(map[string]bool)
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	for _, route := range []string{
		"GET /api/v1/admin/backups/image-storage",
		"PUT /api/v1/admin/backups/image-storage",
		"POST /api/v1/admin/backups/image-storage/test",
	} {
		require.True(t, registered[route], "missing frontend API route %s", route)
	}

	for _, request := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/admin/backups/s3-config"},
		{http.MethodPut, "/api/v1/admin/backups/image-storage"},
		{http.MethodPost, "/api/v1/admin/backups"},
		{http.MethodGet, "/api/v1/admin/backups/1/download-url"},
		{http.MethodPost, "/api/v1/admin/backups/1/restore"},
	} {
		t.Run(request.method+" "+request.path, func(t *testing.T) {
			before := stepUpCalls
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(request.method, request.path, nil))
			require.Equal(t, http.StatusForbidden, recorder.Code)
			require.Equal(t, before+1, stepUpCalls, "handler must stay behind the step-up boundary")
		})
	}
}
