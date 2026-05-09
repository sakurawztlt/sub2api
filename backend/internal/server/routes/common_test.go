package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func setupRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterCommonRoutes(r)
	return r
}

func TestReadyz_DefaultReturnsOK(t *testing.T) {
	// 测试隔离: 单测可能并行, 用本地变量重置
	shuttingDown.Store(false)
	t.Cleanup(func() { shuttingDown.Store(false) })

	r := setupRouter()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("readyz before SIGTERM should be 200, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestReadyz_AfterShutdownReturns503(t *testing.T) {
	shuttingDown.Store(false)
	t.Cleanup(func() { shuttingDown.Store(false) })

	SetShuttingDown()
	if !IsShuttingDown() {
		t.Fatalf("IsShuttingDown should be true after SetShuttingDown")
	}

	r := setupRouter()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz during drain should be 503, got %d body=%s", w.Code, w.Body.String())
	}
}

// /health (liveness) 即使 shutting down 也必须返 200.
// 否则 k8s livenessProbe fail → 直接 SIGKILL, 跳过 graceful drain.
func TestHealth_StaysOKDuringShutdown(t *testing.T) {
	shuttingDown.Store(false)
	t.Cleanup(func() { shuttingDown.Store(false) })

	SetShuttingDown()

	r := setupRouter()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("liveness /health must STAY 200 during drain (else k8s SIGKILL): got %d", w.Code)
	}
}
