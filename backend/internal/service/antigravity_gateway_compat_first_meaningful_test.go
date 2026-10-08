//go:build unit

package service

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAntigravityCompatFirstMeaningfulTimeoutAfterPreContentPing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name    string
		adapter func() antigravityCompatStreamAdapter
		want    string
	}{
		{"chat", func() antigravityCompatStreamAdapter { return newAntigravityChatStreamAdapter("gemini-3.1-pro", false) }, `data: {"error":`},
		{"responses", func() antigravityCompatStreamAdapter { return newAntigravityResponsesStreamAdapter("gemini-3.1-pro") }, "event: response.failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newAntigravityCompatService(config.GatewayConfig{
				MaxLineSize: defaultMaxLineSize, FirstMeaningfulEventTimeoutSeconds: 1,
			}, nil)
			c, recorder := newAntigravityCompatContext(http.MethodPost, "/", nil)
			reader, pipeWriter := io.Pipe()
			defer func() { _ = reader.Close() }()
			defer func() { _ = pipeWriter.Close() }()
			resp := &http.Response{StatusCode: http.StatusOK, Body: reader}
			type outcome struct {
				result *antigravityStreamResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := svc.handleAntigravityCompatStreamWithKeepaliveInterval(
					c, resp, time.Now(), "gemini-3.1-pro", tc.adapter(), "test",
					20*time.Millisecond, 3*time.Second,
				)
				done <- outcome{result, err}
			}()
			select {
			case got := <-done:
				require.ErrorContains(t, got.err, "first meaningful event timeout")
				var failoverErr *UpstreamFailoverError
				require.NotErrorAs(t, got.err, &failoverErr)
				require.NotNil(t, got.result)
				require.Nil(t, got.result.firstTokenMs)
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Contains(t, recorder.Body.String(), ": ping\n\n")
				require.Contains(t, recorder.Body.String(), tc.want)
				require.Contains(t, recorder.Body.String(), `"server_error"`)
				require.Contains(t, recorder.Body.String(), antigravityCompatTemporaryUnavailableMessage)
				require.True(t, IsResponseCommitted(c))
			case <-time.After(2 * time.Second):
				t.Fatal("first meaningful timeout did not end the committed stream")
			}
		})
	}
}
