package routes

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type systemRoutesUpdateServiceStub struct {
	version  string
	disabled bool
}

func (s *systemRoutesUpdateServiceStub) CurrentVersion() string {
	return s.version
}

func (s *systemRoutesUpdateServiceStub) UpdatesDisabled() bool {
	return s.disabled
}

func (*systemRoutesUpdateServiceStub) CheckUpdate(context.Context, bool) (*service.UpdateInfo, error) {
	return nil, nil
}

func (*systemRoutesUpdateServiceStub) PerformUpdate(context.Context) error {
	return nil
}

func (*systemRoutesUpdateServiceStub) Rollback() error {
	return nil
}

func TestRegisterSystemRoutesRelayOmitsUpstreamUpdater(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	systemHandler := adminhandler.NewSystemHandler(
		&systemRoutesUpdateServiceStub{version: "2.24.0-relay", disabled: true},
		nil,
	)
	handlers := &handler.Handlers{
		Admin: &handler.AdminHandlers{System: systemHandler},
	}

	registerSystemRoutes(router.Group("/api/v1/admin"), handlers)

	registered := make(map[string]bool)
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}

	require.True(t, registered["GET /api/v1/admin/system/version"])
	require.False(t, registered["GET /api/v1/admin/system/check-updates"])
	require.False(t, registered["POST /api/v1/admin/system/update"])
	require.False(t, registered["POST /api/v1/admin/system/rollback"])
}

func TestRegisterSystemRoutesUpstreamBuildKeepsUpdater(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	systemHandler := adminhandler.NewSystemHandler(
		&systemRoutesUpdateServiceStub{version: "0.1.165", disabled: false},
		nil,
	)
	handlers := &handler.Handlers{
		Admin: &handler.AdminHandlers{System: systemHandler},
	}

	registerSystemRoutes(router.Group("/api/v1/admin"), handlers)

	registered := make(map[string]bool)
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}

	require.True(t, registered["GET /api/v1/admin/system/check-updates"])
	require.True(t, registered["POST /api/v1/admin/system/update"])
	require.True(t, registered["POST /api/v1/admin/system/rollback"])
}
