//go:build unit

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthMarksIngressRejectReason(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		requestURL string
		authHeader string
		want       IngressRejectReason
	}{
		{name: "missing credential", requestURL: "/v1/messages", want: IngressRejectAPIKeyRequired},
		{name: "malformed authorization", requestURL: "/v1/messages", authHeader: "Basic malformed", want: IngressRejectInvalidAPIKey},
		{name: "query credential", requestURL: "/v1/messages?key=deprecated", want: IngressRejectQueryAPIKeyDeprecated},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := gin.New()
			var got IngressRejectReason
			engine.Use(func(c *gin.Context) {
				c.Next()
				got, _ = GetIngressRejectReason(c)
			})
			engine.Use(apiKeyAuthWithSubscription(nil, nil, &config.Config{}))
			engine.POST("/v1/messages", func(c *gin.Context) { c.Status(http.StatusOK) })

			req := httptest.NewRequest(http.MethodPost, tt.requestURL, nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, req)

			require.Equal(t, tt.want, got)
			require.NotEqual(t, http.StatusOK, response.Code)
		})
	}
}

func TestGoogleAPIKeyAuthMarksMissingCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	var got IngressRejectReason
	engine.Use(func(c *gin.Context) {
		c.Next()
		got, _ = GetIngressRejectReason(c)
	})
	engine.Use(APIKeyAuthWithSubscriptionGoogle(nil, nil, &config.Config{}))
	engine.POST("/v1beta/models/test:generateContent", func(c *gin.Context) { c.Status(http.StatusOK) })

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/test:generateContent", nil)
	engine.ServeHTTP(response, request)

	require.Equal(t, IngressRejectAPIKeyRequired, got)
	require.Equal(t, http.StatusUnauthorized, response.Code)
}
