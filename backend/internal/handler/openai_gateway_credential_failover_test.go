package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGatewayChatCredentialFailureReturnsNeutralMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	(&GatewayHandler{}).handleCCFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:        http.StatusUnauthorized,
		Stage:             service.GatewayFailureStageAccountAuth,
		Scope:             service.GatewayFailureScopeAccount,
		Reason:            service.AntigravityCredentialRejectedReason,
		NextAccountAction: service.NextAccountRetry,
		ClientStatusCode:  http.StatusBadGateway,
		ClientMessage:     service.AntigravityCredentialRejectedClientMessage,
		ResponseBody:      []byte(`{"error":{"message":"Invalid bearer token","refresh_token":"must-not-leak"}}`),
	}, false)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), anthropicTemporaryUnavailableMessage)
	require.NotContains(t, strings.ToLower(recorder.Body.String()), "antigravity")
	require.NotContains(t, strings.ToLower(recorder.Body.String()), "grok")
	require.NotContains(t, strings.ToLower(recorder.Body.String()), "project_id")
	require.NotContains(t, strings.ToLower(recorder.Body.String()), "bearer")
	require.NotContains(t, strings.ToLower(recorder.Body.String()), "refresh_token")
}

func TestGatewayChatCredentialStopDoesNotSelectAnotherAccountAndReturnsSafe503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stopErr := &service.UpstreamFailoverError{
		Stage:             service.GatewayFailureStageAccountAuth,
		Scope:             service.GatewayFailureScopeProvider,
		Reason:            service.GrokCredentialReasonProviderConfig,
		NextAccountAction: service.NextAccountStop,
		ClientStatusCode:  http.StatusTeapot,
		ClientMessage:     "invalid_client client_secret=must-not-leak",
	}
	state := NewFailoverState(3, false)
	action := state.HandleFailoverError(context.Background(), &mockTempUnscheduler{}, 71, service.PlatformGrok, stopErr)

	require.Equal(t, FailoverExhausted, action)
	require.Zero(t, state.SwitchCount)
	require.Empty(t, state.FailedAccountIDs)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	(&GatewayHandler{}).handleCCFailoverExhausted(c, state.LastFailoverErr, false)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), anthropicTemporaryUnavailableMessage)
	require.NotContains(t, strings.ToLower(recorder.Body.String()), "grok")
	require.NotContains(t, recorder.Body.String(), "invalid_client")
	require.NotContains(t, recorder.Body.String(), "client_secret")
}

func TestGatewayChatInferenceExhaustionRestoresRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	(&GatewayHandler{}).handleCCFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:      http.StatusTooManyRequests,
		ResponseHeaders: http.Header{"Retry-After": []string{"45"}},
	}, false)

	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	require.Equal(t, "45", recorder.Header().Get("Retry-After"))
}

func TestCredentialFailoverExhaustionReturnsFixedSafe503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	h := &OpenAIGatewayHandler{}

	h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
		Stage:             service.GatewayFailureStageAccountAuth,
		Scope:             service.GatewayFailureScopeAccount,
		Reason:            service.GrokCredentialReasonRevoked,
		NextAccountAction: service.NextAccountRetry,
		ClientStatusCode:  http.StatusTeapot,
		ClientMessage:     "invalid_grant refresh_token=must-not-leak",
	}, false)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), anthropicTemporaryUnavailableMessage)
	require.NotContains(t, strings.ToLower(recorder.Body.String()), "grok")
	require.NotContains(t, strings.ToLower(recorder.Body.String()), "invalid_grant")
	require.NotContains(t, strings.ToLower(recorder.Body.String()), "refresh_token")
	require.NotContains(t, recorder.Body.String(), "must-not-leak")
}

func TestInferenceFailoverExhaustionRestoresRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	h := &OpenAIGatewayHandler{}

	h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:      http.StatusTooManyRequests,
		ResponseHeaders: http.Header{"Retry-After": []string{"17"}},
	}, false)

	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	require.Equal(t, "17", recorder.Header().Get("Retry-After"))
}

func TestFailoverExhaustionRejectsSecretBearingRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	h := &OpenAIGatewayHandler{}

	h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:      http.StatusTooManyRequests,
		ResponseHeaders: http.Header{"Retry-After": []string{"refresh_token=must-not-leak"}},
	}, false)

	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	require.Empty(t, recorder.Header().Get("Retry-After"))
	require.NotContains(t, recorder.Body.String(), "must-not-leak")
}

func TestFailoverExhaustionRejectsFarFutureRetryAfterDate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	h := &OpenAIGatewayHandler{}

	h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode: http.StatusTooManyRequests,
		ResponseHeaders: http.Header{
			"Retry-After": []string{time.Now().Add(30 * 24 * time.Hour).UTC().Format(http.TimeFormat)},
		},
	}, false)

	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	require.Empty(t, recorder.Header().Get("Retry-After"))
}

func TestFailoverExhaustionAllowsBoundedRetryAfterDate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	h := &OpenAIGatewayHandler{}
	retryAfter := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)

	h.handleFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode:      http.StatusTooManyRequests,
		ResponseHeaders: http.Header{"Retry-After": []string{retryAfter}},
	}, false)

	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	require.Equal(t, retryAfter, recorder.Header().Get("Retry-After"))
}

func TestOpenAICapacityFailoverExhaustionPreservesSafeMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	message := "Our servers are currently overloaded. Please try again later."
	failoverErr := &service.UpstreamFailoverError{
		StatusCode:             http.StatusBadRequest,
		ResponseBody:           []byte(`{"error":{"code":"server_is_overloaded","message":"` + message + `"}}`),
		RetryableOnSameAccount: true,
		RequestScopedTransient: true,
		ClientStatusCode:       http.StatusServiceUnavailable,
		ClientMessage:          message,
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	(&GatewayHandler{}).handleResponsesFailoverExhausted(c, failoverErr, false)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Equal(t, "server_error", gjson.Get(recorder.Body.String(), "error.code").String())
	require.Equal(t, message, gjson.Get(recorder.Body.String(), "error.message").String())
	require.NotContains(t, recorder.Body.String(), "server_is_overloaded")
}

func TestResponsesFailoverExhaustedAfterTerminalDoesNotDuplicateFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	official := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"official failure\"}}}\n\n"
	_, err := c.Writer.Write([]byte(official))
	require.NoError(t, err)
	service.MarkOpsStreamError(c, "server_error", "official failure", http.StatusBadGateway)

	(&GatewayHandler{}).handleResponsesFailoverExhausted(c, &service.UpstreamFailoverError{StatusCode: http.StatusBadGateway}, true)
	require.Equal(t, official, recorder.Body.String())
	streamErr, ok := service.GetOpsStreamError(c)
	require.True(t, ok)
	require.Equal(t, "official failure", streamErr.Message)

	heartbeatRecorder := httptest.NewRecorder()
	heartbeatContext, _ := gin.CreateTestContext(heartbeatRecorder)
	heartbeat := ": keepalive\n\n"
	written, err := heartbeatRecorder.Write([]byte(heartbeat))
	require.NoError(t, err)
	recordGatewayStreamHeartbeat(heartbeatContext, written)
	(&GatewayHandler{}).handleResponsesFailoverExhausted(heartbeatContext, &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests}, true)
	require.Equal(t, 1, strings.Count(heartbeatRecorder.Body.String(), "event: response.failed"))
}

func TestOpsWebSocketCredentialFailoverExhaustedIsRecorded(t *testing.T) {
	setupOpsErrorLogTestQueue(t, 2)
	gin.SetMode(gin.TestMode)
	ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.Use(OpsErrorLoggerMiddleware(ops))
	router.GET("/openai/v1/responses", func(c *gin.Context) {
		c.Set(service.OpsUpstreamErrorsKey, []*service.OpsUpstreamErrorEvent{{
			Stage: string(service.GatewayFailureStageAccountAuth), Scope: string(service.GatewayFailureScopeAccount),
			Reason: string(service.GrokCredentialReasonRevoked), Message: "Grok OAuth credentials require account action",
		}})
		closeOpenAIWSFailoverExhausted(c, nil, &service.UpstreamFailoverError{
			Stage:             service.GatewayFailureStageAccountAuth,
			Scope:             service.GatewayFailureScopeAccount,
			Reason:            service.GrokCredentialReasonRevoked,
			NextAccountAction: service.NextAccountStop,
		})
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/openai/v1/responses", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, int64(1), OpsErrorLogQueueLength())
	job := <-opsErrorLogQueue
	require.Equal(t, "account_auth", job.entry.ErrorPhase)
	require.Equal(t, http.StatusServiceUnavailable, job.entry.StatusCode)
	require.Equal(t, service.GrokCredentialUnavailableClientMessage, job.entry.ErrorMessage)
}
