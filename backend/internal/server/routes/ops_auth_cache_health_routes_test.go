package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpsAuthCacheHealthRouteRequiresAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &handler.Handlers{Admin: &handler.AdminHandlers{System: adminhandler.NewSystemHandler(&systemRoutesUpdateServiceStub{disabled: true}, nil)}}
	adminAuth := middleware.AdminAuthMiddleware(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatus(http.StatusUnauthorized)
		} else {
			c.AbortWithStatus(http.StatusForbidden)
		}
	})
	RegisterAdminRoutes(r.Group("/api/v1"), h, adminAuth, func(c *gin.Context) { c.Next() }, func(c *gin.Context) { c.Next() }, nil, nil)
	for _, auth := range []string{"", "Bearer ordinary-user"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/ops/auth-cache-invalidation/health", nil)
		req.Header.Set("Authorization", auth)
		r.ServeHTTP(w, req)
		want := http.StatusUnauthorized
		if auth != "" {
			want = http.StatusForbidden
		}
		require.Equal(t, want, w.Code)
	}
}
