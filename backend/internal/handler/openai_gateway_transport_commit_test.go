package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIResponsesKeepaliveThenCapacityFailedExhaustionUsesTerminalSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Header("Content-Type", "text/event-stream")

	// Attempt 1 commits HTTP 200 with a transport-only comment. It has not
	// emitted any client-visible Responses event, so capacity failover remains
	// safe even though a later exhausted response can no longer be JSON.
	_, err := c.Writer.WriteString(": keepalive\n\n")
	require.NoError(t, err)
	c.Writer.Flush()
	require.False(t, service.OpenAIStreamSemanticOutputStarted(c))

	transportCommitted := openAIResponsesTransportCommitted(c, true, false)
	require.True(t, transportCommitted)

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode: http.StatusServiceUnavailable,
		ResponseBody: []byte(`{"type":"response.failed","response":{"status":"failed","error":` +
			`{"code":"server_is_overloaded","message":"capacity shed"}}}`),
	}, transportCommitted)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
	body := recorder.Body.String()
	require.True(t, strings.HasPrefix(body, ": keepalive\n\n"), body)
	tail := strings.TrimPrefix(body, ": keepalive\n\n")
	require.True(t, strings.HasPrefix(tail, "event: response.failed\ndata: "), tail)
	require.Contains(t, tail, `"type":"response.failed"`)
	require.Contains(t, tail, `"status":"failed"`)
	require.Contains(t, tail, `"code":"server_error"`)
	require.NotContains(t, tail, "\n{\"error\"")
	require.Equal(t, 1, strings.Count(tail, "event: response.failed"))
}

func TestOpenAIResponsesPanicAfterKeepaliveUsesTerminalSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Header("Content-Type", "text/event-stream")

	_, err := c.Writer.WriteString(": keepalive\n\n")
	require.NoError(t, err)
	c.Writer.Flush()
	transportCommitted := false // Simulate a panic before the loop updates it.

	h := &OpenAIGatewayHandler{}
	require.NotPanics(t, func() {
		func() {
			defer h.recoverResponsesPanic(c, &transportCommitted)
			panic("panic after transport-only comment")
		}()
	})

	require.Equal(t, http.StatusOK, recorder.Code)
	body := recorder.Body.String()
	require.True(t, strings.HasPrefix(body, ": keepalive\n\n"), body)
	tail := strings.TrimPrefix(body, ": keepalive\n\n")
	require.True(t, strings.HasPrefix(tail, "event: response.failed\ndata: "), tail)
	require.Contains(t, tail, `"code":"server_error"`)
	require.NotContains(t, tail, "\n{\"error\"")
	require.Equal(t, 1, strings.Count(tail, "event: response.failed"))
}
