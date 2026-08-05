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
