package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpsAuthCacheInvalidationHealthHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name   string
		svc    *service.OpsService
		status int
	}{
		{"available", service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil), http.StatusOK},
		{"unavailable", nil, http.StatusServiceUnavailable},
		{"monitoring-disabled", service.NewOpsService(nil, nil, &config.Config{Ops: config.OpsConfig{Enabled: false}}, nil, nil, nil, nil, nil, nil, nil, nil), http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.GET("/health", NewOpsHandler(tc.svc).GetAuthCacheInvalidationHealth)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
			require.Equal(t, tc.status, w.Code)
			if tc.status == http.StatusOK {
				require.Contains(t, w.Body.String(), `"outbox"`)
				require.Contains(t, w.Body.String(), `"subscriber"`)
			}
		})
	}
}
