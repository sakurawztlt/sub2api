package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type openAIImagesReadErrorBody struct {
	err error
}

func (b *openAIImagesReadErrorBody) Read([]byte) (int, error) { return 0, b.err }
func (b *openAIImagesReadErrorBody) Close() error             { return nil }

func TestImagesOAuthNonStreaming_TransportReadErrorIsTypedForFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"X-Request-Id": []string{"req_read_error"}},
		Body:       &openAIImagesReadErrorBody{err: errors.New("stream error: stream ID 11; INTERNAL_ERROR; received from peer")},
	}

	before := OpenAIImagesJSONKeepaliveAdjustedWrittenSize(c)
	_, _, _, readErr := (&OpenAIGatewayService{}).handleOpenAIImagesOAuthNonStreamingResponse(resp, c, "b64_json", "gpt-image-2")
	code, message, ok := OpenAIUpstreamStreamReadErrorDetails(readErr)
	require.True(t, ok)
	require.Equal(t, OpenAIUpstreamHTTP2StreamErrorCode, code)
	require.Equal(t, "Upstream HTTP/2 stream failed", message)

	err := (&OpenAIGatewayService{}).handleOpenAIImagesOAuthResponseError(
		context.Background(), c,
		&Account{ID: 5400, Name: "openai-oauth", Platform: PlatformOpenAI, Type: AccountTypeOAuth},
		"gpt-image-2", "https://api.openai.com/v1/responses", resp, before, readErr,
	)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.Equal(t, "req_read_error", failoverErr.ResponseHeaders.Get("x-request-id"))
	require.Empty(t, recorder.Body.String())
}

func TestImagesOAuthStreaming_TransportReadErrorRemainsUnflushedForFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       &openAIImagesReadErrorBody{err: errors.New("unexpected EOF")},
	}

	_, _, _, _, err := (&OpenAIGatewayService{}).handleOpenAIImagesOAuthStreamingResponse(
		resp, c, time.Now(), "b64_json", "image_generation", "gpt-image-2",
	)
	code, _, ok := OpenAIUpstreamStreamReadErrorDetails(err)
	require.True(t, ok)
	require.Equal(t, OpenAIUpstreamStreamReadErrorCode, code)
	require.Empty(t, recorder.Body.String(), "transport failure must not commit a raw SSE error before failover")
}

func TestImagesOAuthIncompleteClassification(t *testing.T) {
	retryable := openAIImagesUpstreamErrorFromSSEPayload([]byte(`{
		"type":"response.incomplete",
		"response":{"id":"resp_incomplete","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}
	}`))
	require.NotNil(t, retryable)
	require.Equal(t, http.StatusBadGateway, retryable.StatusCode)
	require.Equal(t, "response_incomplete", retryable.Code)
	require.True(t, IsOpenAIImagesRetryableUpstreamError(retryable))

	contentFilter := openAIImagesUpstreamErrorFromSSEPayload([]byte(`{
		"type":"response.incomplete",
		"response":{"id":"resp_filtered","status":"incomplete","incomplete_details":{"reason":"content_filter"}}
	}`))
	require.NotNil(t, contentFilter)
	require.Equal(t, http.StatusBadRequest, contentFilter.StatusCode)
	require.False(t, IsOpenAIImagesRetryableUpstreamError(contentFilter))
}

func TestImagesOAuthNoOutputSummaryIncludesTerminalDiagnostics(t *testing.T) {
	summary := summarizeOpenAIImagesNoOutputBody([]byte("data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n"))
	require.Contains(t, summary, "last_event=response.incomplete")
	require.Contains(t, summary, "status=incomplete")
	require.Contains(t, summary, "incomplete_reason=max_output_tokens")
}

func TestImagesOAuthNonStreaming_CompletedNoImageRetriesSameAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	upstreamBody := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_empty\",\"status\":\"completed\",\"output\":[]}}\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(upstreamBody))}

	_, _, _, err := (&OpenAIGatewayService{}).handleOpenAIImagesOAuthNonStreamingResponse(resp, c, "b64_json", "gpt-image-2")
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.Empty(t, recorder.Body.String())
}

func TestImagesOAuthStreaming_CompletedNoImageRetriesSameAccountUnflushed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	upstreamBody := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_empty\",\"status\":\"completed\",\"output\":[]}}\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(upstreamBody))}

	_, _, _, _, err := (&OpenAIGatewayService{}).handleOpenAIImagesOAuthStreamingResponse(resp, c, time.Now(), "b64_json", "image_generation", "gpt-image-2")
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.Empty(t, recorder.Body.String())
}

func TestImagesOAuthNonStreaming_TextFallbackReturnsCapabilityError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamSSE := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"status\":\"in_progress\",\"model\":\"gpt-5.4-mini\",\"output\":[]}}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Here's a polished image prompt for your request.\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"model\":\"gpt-5.4-mini\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"Here's a polished image prompt for your request.\"}]}]}}\n\n"

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(upstreamSSE))}

	_, _, _, err := (&OpenAIGatewayService{}).handleOpenAIImagesOAuthNonStreamingResponse(resp, c, "b64_json", "gpt-image-2")
	var imageErr *OpenAIImagesUpstreamError
	require.ErrorAs(t, err, &imageErr)
	require.Equal(t, http.StatusBadGateway, imageErr.StatusCode)
	require.Equal(t, "image_generation_unavailable", imageErr.Code)
}

func TestImagesOAuthStreaming_TextFallbackRemainsUnflushedForFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamSSE := "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Here's a polished image prompt for your request.\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n"

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(upstreamSSE))}

	_, _, _, _, err := (&OpenAIGatewayService{}).handleOpenAIImagesOAuthStreamingResponse(resp, c, time.Now(), "b64_json", "image_generation", "gpt-image-2")
	var imageErr *OpenAIImagesUpstreamError
	require.ErrorAs(t, err, &imageErr)
	require.Equal(t, http.StatusBadGateway, imageErr.StatusCode)
	require.Equal(t, "image_generation_unavailable", imageErr.Code)
	require.NotContains(t, recorder.Body.String(), "event: error")
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
}

func TestImagesOAuthStreaming_SplitSafetyRefusalReturns400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamSSE := "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"安全系\"}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"统拒绝生成\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n"

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(upstreamSSE))}

	_, _, _, _, err := (&OpenAIGatewayService{}).handleOpenAIImagesOAuthStreamingResponse(resp, c, time.Now(), "b64_json", "image_generation", "gpt-image-2")
	var imageErr *OpenAIImagesUpstreamError
	require.ErrorAs(t, err, &imageErr)
	require.Equal(t, http.StatusBadRequest, imageErr.StatusCode)
	require.Equal(t, "content_policy_violation", imageErr.Code)
	require.Contains(t, recorder.Body.String(), "event: error")
}
