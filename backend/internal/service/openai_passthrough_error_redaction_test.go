package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIGatewayService_APIKeyPassthrough_RebuildsUpstreamErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name           string
		statusCode     int
		contentType    string
		responseBody   string
		retryAfter     string
		wantStatus     int
		wantMessage    string
		wantRetryAfter string
	}{
		{
			name: "upstream forbidden is reported as gateway failure", statusCode: http.StatusForbidden,
			contentType: "text/html; charset=UTF-8", responseBody: `<!DOCTYPE html><title>secret-upstream.example denied the request</title>`,
			retryAfter: "17", wantStatus: http.StatusBadGateway, wantMessage: "Upstream access denied", wantRetryAfter: "17",
		},
		{
			name: "upstream unauthorized is reported as gateway failure", statusCode: http.StatusUnauthorized,
			contentType: "application/json", responseBody: `{"error":{"message":"invalid secret-upstream.example token","type":"authentication_error","code":"invalid_api_key","param":"api_key"},"rate_limit":{"remaining":0}}`,
			wantStatus: http.StatusBadGateway, wantMessage: "Upstream authentication failed",
		},
		{
			name: "html 5xx", statusCode: 530, contentType: "text/html; charset=UTF-8",
			responseBody: `<!DOCTYPE html><title>secret-upstream.example | 530: Origin DNS error</title>`,
			wantStatus:   530, wantMessage: "Upstream service temporarily unavailable",
		},
		{
			name: "structured 5xx", statusCode: http.StatusNotImplemented, contentType: "application/json",
			responseBody: `{"error":{"message":"secret-upstream.example internal failure"}}`,
			wantStatus:   http.StatusNotImplemented, wantMessage: "Upstream service temporarily unavailable",
		},
		{
			name: "unstructured 4xx", statusCode: http.StatusBadRequest, contentType: "text/plain",
			responseBody: `proxy secret-upstream.example rejected the request`,
			wantStatus:   http.StatusBadRequest, wantMessage: "Upstream request failed",
		},
		{
			name: "malicious valid json 4xx", statusCode: http.StatusBadRequest, contentType: "application/json",
			responseBody: `{"error":{"message":"secret-upstream.example invalid parameter","type":"invalid_request_error","code":"upstream_secret_code","param":"private_field","internal_token":"sk-upstream-secret"},"rate_limit":{"remaining":0,"reset":"internal-window"},"debug":{"admin":"root"},"redirect":"https://secret-upstream.example/admin"}`,
			retryAfter:   "not-a-valid-delay", wantStatus: http.StatusBadRequest, wantMessage: "Upstream request failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
			c.Request.Header.Set("User-Agent", "codex_cli_rs/0.1.0")

			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: tt.statusCode,
				Header: http.Header{
					"Content-Type": []string{tt.contentType}, "Location": []string{"https://secret-upstream.example/admin"},
					"Retry-After": []string{tt.retryAfter}, "Server": []string{"secret-upstream-proxy"},
					"Set-Cookie": []string{"admin_token=secret"}, "WWW-Authenticate": []string{`Bearer realm="secret-upstream.example"`},
					"X-Admin-Debug": []string{"internal-route=secret-upstream.example"}, "X-Codex-Primary-Used-Percent": []string{"99"},
					"x-request-id": []string{"rid-sensitive-upstream"},
				},
				Body: io.NopCloser(strings.NewReader(tt.responseBody)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}}, httpUpstream: upstream}
			account := &Account{
				ID: 124, Name: "sensitive-upstream", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://secret-upstream.example"},
				Extra:       map[string]any{"openai_passthrough": true}, Status: StatusActive, Schedulable: true,
			}

			_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.2","stream":false,"input":"hello"}`))

			require.Error(t, err)
			require.Equal(t, tt.wantStatus, rec.Code)
			require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
			require.Equal(t, tt.wantRetryAfter, rec.Header().Get("Retry-After"))
			for _, key := range []string{"Location", "Server", "Set-Cookie", "WWW-Authenticate", "X-Admin-Debug", "X-Codex-Primary-Used-Percent", "X-Request-Id"} {
				require.Empty(t, rec.Header().Values(key), "sensitive upstream header %s must be dropped", key)
			}
			require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
			require.Equal(t, tt.wantMessage, gjson.Get(rec.Body.String(), "error.message").String())
			require.False(t, gjson.Get(rec.Body.String(), "error.code").Exists())
			require.False(t, gjson.Get(rec.Body.String(), "error.param").Exists())
			require.False(t, gjson.Get(rec.Body.String(), "rate_limit").Exists())
			require.NotContains(t, rec.Body.String(), "secret-upstream.example")
			require.NotContains(t, rec.Body.String(), "sk-upstream-secret")
			require.NotContains(t, err.Error(), "secret-upstream.example")
		})
	}
}

func TestWriteOpenAIPassthroughErrorHeaders_StrictRetryAfter(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "positive delay seconds", raw: "17", want: true},
		{name: "fractional delay", raw: "1.5"},
		{name: "scientific notation", raw: "1e3"},
		{name: "explicit plus sign", raw: "+17"},
		{name: "zero", raw: "0"},
		{name: "negative delay", raw: "-1"},
		{name: "uint64 overflow", raw: "18446744073709551616"},
		{name: "future http date", raw: now.Add(time.Hour).Format(http.TimeFormat), want: true},
		{name: "past http date", raw: now.Add(-time.Hour).Format(http.TimeFormat)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := http.Header{"Retry-After": []string{"stale"}}
			writeOpenAIPassthroughErrorHeaders(dst, http.Header{"Retry-After": []string{tt.raw}})
			if tt.want {
				require.Equal(t, tt.raw, dst.Get("Retry-After"))
			} else {
				require.Empty(t, dst.Get("Retry-After"))
			}
		})
	}
}

func TestOpenAIGatewayService_APIKeyPassthrough_CompactErrorBeforeKeepaliveIsSingleJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(nil))
	MarkOpenAICompactClientStream(c)
	stop := StartOpenAICompactSSEKeepalive(c, time.Hour)
	defer stop()

	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}},
		httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"secret-upstream.example invalid request"}}`)),
		}},
	}
	account := &Account{
		ID: 125, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://secret-upstream.example"},
		Extra:       map[string]any{"openai_passthrough": true}, Status: StatusActive, Schedulable: true,
	}

	_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.2","input":"hello"}`))

	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.True(t, gjson.Valid(rec.Body.String()))
	require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.NotContains(t, rec.Body.String(), "event:")
	require.NotContains(t, rec.Body.String(), ": keepalive")
	require.NotContains(t, rec.Body.String(), "secret-upstream.example")
}

func TestOpenAIGatewayService_APIKeyPassthrough_CompactErrorAfterKeepaliveIsFailedSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(nil))
	MarkOpenAICompactClientStream(c)
	stop := StartOpenAICompactSSEKeepalive(c, keepaliveTestInterval)
	defer stop()
	waitForKeepaliveBeats()

	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}},
		httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"secret-upstream.example invalid request"}}`)),
		}},
	}
	account := &Account{
		ID: 126, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://secret-upstream.example"},
		Extra:       map[string]any{"openai_passthrough": true}, Status: StatusActive, Schedulable: true,
	}

	_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.2","input":"hello"}`))

	require.Error(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Result().Header.Get("Content-Type"), "text/event-stream")
	events := parseCompactBridgeSSE(t, stripKeepaliveComments(rec.Body.String()))
	require.Len(t, events, 1)
	require.Equal(t, "response.failed", events[0][0])
	require.Equal(t, "failed", gjson.Get(events[0][1], "response.status").String())
	require.Equal(t, "upstream_error", gjson.Get(events[0][1], "response.error.code").String())
	require.Equal(t, "Upstream request failed", gjson.Get(events[0][1], "response.error.message").String())
	require.NotContains(t, rec.Body.String(), "secret-upstream.example")
}
