package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type staticVersionServiceStub struct {
	checkCalls int
}

func (*staticVersionServiceStub) CurrentVersion() string {
	return "2.24.0-relay"
}

func (*staticVersionServiceStub) UpdatesDisabled() bool {
	return true
}

func (s *staticVersionServiceStub) CheckUpdate(context.Context, bool) (*service.UpdateInfo, error) {
	s.checkCalls++
	return nil, nil
}

func (*staticVersionServiceStub) PerformUpdate(context.Context) error {
	return nil
}

func (*staticVersionServiceStub) Rollback() error {
	return nil
}

func TestSystemVersionHandlerUsesEmbeddedVersionOnly(t *testing.T) {
	updateSvc := &staticVersionServiceStub{}
	systemHandler := NewSystemHandler(updateSvc, nil)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/admin/system/version", systemHandler.GetVersion)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/system/version", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Zero(t, updateSvc.checkCalls)
	require.JSONEq(t, `{"code":0,"message":"success","data":{"version":"2.24.0-relay"}}`, rec.Body.String())
}
