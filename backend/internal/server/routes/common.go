package routes

import (
	"net/http"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// shuttingDown is set to 1 by main.go after SIGTERM. /readyz reads it
// to flip from 200 to 503 so k8s endpoints controller removes this pod
// from the Service before the http.Server.Shutdown drains in-flight.
//
// 5/9 codex audit: 之前发版 rollout 旧 pod 没"draining" 状态, k8s endpoint
// 表里仍在, kube-proxy 把流量打到正在退出的 pod → connect refused → nginx
// 把整个 NodePort 标 down → no live upstreams → 502 spike (实测 22:19-22:21
// 1.5 min 内 463 条 502, 单分钟 372 条).
var shuttingDown atomic.Bool

// SetShuttingDown — main 收到 SIGTERM 时调. 之后 /readyz 立刻返 503.
func SetShuttingDown() {
	shuttingDown.Store(true)
}

// IsShuttingDown — 仅用于 main.go 决定 grace sleep 时长. 单测可读.
func IsShuttingDown() bool {
	return shuttingDown.Load()
}

// RegisterCommonRoutes 注册通用路由（健康检查、状态等）
func RegisterCommonRoutes(r *gin.Engine) {
	// /health — liveness, 进程活着就返 200. k8s livenessProbe 用这个.
	// (不能跟 /readyz 一样, 否则 SIGTERM 后 livenessProbe fail → k8s 直接 SIGKILL)
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// /readyz — readiness, SIGTERM 后立即 503 让 k8s 从 endpoint 移除.
	// k8s readinessProbe 用这个. 注意 livenessProbe 仍用 /health 不能用这个.
	r.GET("/readyz", func(c *gin.Context) {
		if shuttingDown.Load() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "draining"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	// Claude Code 遥测日志（忽略，直接返回200）
	r.POST("/api/event_logging/batch", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	// Setup status endpoint (always returns needs_setup: false in normal mode)
	// This is used by the frontend to detect when the service has restarted after setup
	r.GET("/setup/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"code": 0,
			"data": gin.H{
				"needs_setup": false,
				"step":        "completed",
			},
		})
	})
}
