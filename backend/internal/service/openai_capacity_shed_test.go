package service

import (
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
)

// --- mock: 只记录临时不可调度写入，其余方法不应被调用 ---

type capacityShedAccountRepoStub struct {
	AccountRepository // 嵌入接口，未实现的方法会 panic（不应被调用）

	tempUnschedCalls int
}

func (r *capacityShedAccountRepoStub) SetTempUnschedulable(_ context.Context, _ int64, _ time.Time, _ string) error {
	r.tempUnschedCalls++
	return nil
}

// 上游容量降载是请求级信号：故障因素（客户端身份、模型容量）与账号无关，
// 同账号重试用尽后不得把账号临时摘掉——否则一个被降载的请求会顺着 failover
// 把整池账号逐个封禁，而每个账号都会以同一个错误失败。
func TestTempUnscheduleRetryableErrorSkipsRequestScopedTransient(t *testing.T) {
	t.Run("请求级瞬时故障不写账号状态", func(t *testing.T) {
		repo := &capacityShedAccountRepoStub{}
		svc := &GatewayService{accountRepo: repo}

		svc.TempUnscheduleRetryableError(context.Background(), 1, &UpstreamFailoverError{
			StatusCode:             http.StatusBadGateway,
			RetryableOnSameAccount: true,
			RequestScopedTransient: true,
		})

		require.Zero(t, repo.tempUnschedCalls)
	})

	// 对照组：同样的 502 在未标记请求级瞬时故障时仍按原有语义临时摘号，
	// 确认上面的断言来自新增守卫而非其他前置条件。
	t.Run("未标记时保持原有临时摘号语义", func(t *testing.T) {
		repo := &capacityShedAccountRepoStub{}
		svc := &GatewayService{accountRepo: repo}

		svc.TempUnscheduleRetryableError(context.Background(), 1, &UpstreamFailoverError{
			StatusCode:             http.StatusBadGateway,
			RetryableOnSameAccount: true,
		})

		require.Equal(t, 1, repo.tempUnschedCalls)
	})
}

// 非池模式账号同样要先在同账号重试：换号不改变降载因素。
func TestStreamFailedEventCapacityShedRetriesOnSameAccount(t *testing.T) {
	nonPool := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	for _, code := range []string{"server_is_overloaded", "slow_down"} {
		payload := []byte(`{"type":"response.failed","response":{"error":{"code":"` + code + `"}}}`)
		require.True(t, isOpenAIUpstreamCapacityShedEvent(payload), code)
		require.True(t, openAIStreamFailedEventRetryableOnSameAccount(nonPool, payload, "overloaded"), code)
	}

	// 非降载的 failed 事件在非池模式下仍不做同账号重试，避免放大改动面。
	other := []byte(`{"type":"response.failed","response":{"error":{"code":"server_error"}}}`)
	require.False(t, isOpenAIUpstreamCapacityShedEvent(other))
	require.False(t, openAIStreamFailedEventRetryableOnSameAccount(nonPool, other, "boom"))
}

type capacityShedGatedReader struct {
	first     *strings.Reader
	second    *strings.Reader
	release   <-chan struct{}
	firstDone bool
}

func (r *capacityShedGatedReader) Read(p []byte) (int, error) {
	if !r.firstDone {
		n, err := r.first.Read(p)
		if err != io.EOF {
			return n, err
		}
		r.firstDone = true
		if n > 0 {
			return n, nil
		}
	}
	<-r.release
	return r.second.Read(p)
}

func newCapacityShedTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c, recorder
}

func assertCapacityShedFailover(t *testing.T, err error) *UpstreamFailoverError {
	t.Helper()
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.True(t, failoverErr.RequestScopedTransient)
	return failoverErr
}

func TestOpenAIStreamDataStartsClientOutputRetryableErrorBoundary(t *testing.T) {
	tests := []struct {
		payload   string
		eventType string
		want      bool
	}{
		{`{"type":"error","error":{"code":"server_is_overloaded","message":"overloaded"}}`, "error", false},
		{`{"type":"error","error":{"code":"slow_down","message":"slow down"}}`, "error", false},
		{`{"type":"error","error":{"code":"content_policy_violation","message":"blocked"}}`, "error", true},
		{`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}`, "response.failed", false},
		{`{"type":"response.created","response":{"id":"resp_1"}}`, "response.created", false},
		{`{"type":"response.output_text.delta","delta":"hi"}`, "response.output_text.delta", true},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, openAIStreamDataStartsClientOutput(tt.payload, tt.eventType), tt.payload)
	}
}

func TestOpenAIStreamCapacityShedAfterKeepaliveStillFailsOver(t *testing.T) {
	c, recorder := newCapacityShedTestContext()
	release := make(chan struct{})
	reader := &capacityShedGatedReader{
		first: strings.NewReader(strings.Join([]string{
			`event: response.created`,
			`data: {"type":"response.created","response":{"id":"resp_keepalive"}}`,
			``,
			`event: response.in_progress`,
			`data: {"type":"response.in_progress","response":{"id":"resp_keepalive"}}`,
			``,
			`event: error`,
			`data: {"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"overloaded"}}`,
			``,
		}, "\n")),
		second: strings.NewReader(strings.Join([]string{
			`event: response.failed`,
			`data: {"type":"response.failed","response":{"id":"resp_keepalive","status":"failed","error":{"code":"server_is_overloaded","message":"overloaded"},"usage":{"input_tokens":9,"output_tokens":2}}}`,
			``,
		}, "\n")),
		release: release,
	}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			MaxLineSize:             defaultMaxLineSize,
			StreamKeepaliveInterval: 1,
		}},
		toolCorrector: NewCodexToolCorrector(),
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"x-request-id": {"rid-keepalive-shed"}}, Body: io.NopCloser(reader)}

	type streamResult struct {
		result *openaiStreamingResult
		err    error
	}
	resultCh := make(chan streamResult, 1)
	go func() {
		result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, time.Now(), "model", "model")
		resultCh <- streamResult{result: result, err: err}
	}()
	time.Sleep(1200 * time.Millisecond)
	close(release)
	got := <-resultCh

	_ = assertCapacityShedFailover(t, got.err)
	require.NotNil(t, got.result)
	require.Equal(t, 9, got.result.usage.InputTokens)
	require.Equal(t, 2, got.result.usage.OutputTokens)
	require.Contains(t, recorder.Body.String(), ":\n\n")
	require.NotContains(t, recorder.Body.String(), "data:", "transport keepalive must not flush pre-output business frames")
	require.False(t, OpenAIStreamSemanticOutputStarted(c))
}

func TestOpenAIPassthroughCapacityShedAfterCommentStillFailsOver(t *testing.T) {
	c, recorder := newCapacityShedTestContext()
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	body := strings.Join([]string{
		`: upstream keepalive`,
		``,
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_comment"}}`,
		``,
		`event: error`,
		`data: {"type":"error","error":{"code":"slow_down","message":"slow down"}}`,
		``,
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_comment","error":{"code":"slow_down","message":"slow down"},"usage":{"input_tokens":7,"output_tokens":1}}}`,
		``,
	}, "\n")
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"x-request-id": {"rid-comment-shed"}}, Body: io.NopCloser(strings.NewReader(body))}

	result, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, time.Now(), "model", "model")

	_ = assertCapacityShedFailover(t, err)
	require.NotNil(t, result)
	require.Equal(t, 7, result.usage.InputTokens)
	require.Equal(t, 1, result.usage.OutputTokens)
	require.Contains(t, recorder.Body.String(), ": upstream keepalive")
	require.NotContains(t, recorder.Body.String(), "data:")
	require.False(t, OpenAIStreamSemanticOutputStarted(c))
}

func TestSanitizeOpenAICapacityShedErrorCodeForClient(t *testing.T) {
	for _, payload := range []string{
		`{"type":"error","error":{"code":"server_is_overloaded","message":"overloaded"}}`,
		`{"type":"response.failed","response":{"error":{"code":"slow_down","message":"slow down"}}}`,
	} {
		out, changed := sanitizeOpenAICapacityShedErrorCodeForClient([]byte(payload))
		require.True(t, changed)
		require.Contains(t, string(out), `"code":"server_error"`)
		require.NotContains(t, string(out), "server_is_overloaded")
		require.NotContains(t, string(out), "slow_down")
	}

	unchanged := []byte(`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}`)
	out, changed := sanitizeOpenAICapacityShedErrorCodeForClient(unchanged)
	require.False(t, changed)
	require.Equal(t, unchanged, out)
}

func TestOpenAIStreamCapacityShedAfterSemanticOutputRewritesClientCopyAndKeepsUsage(t *testing.T) {
	c, recorder := newCapacityShedTestContext()
	svc := &OpenAIGatewayService{
		cfg:           &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		toolCorrector: NewCodexToolCorrector(),
	}
	body := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		``,
		`event: error`,
		`data: {"type":"error","error":{"code":"server_is_overloaded","message":"overloaded"}}`,
		``,
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_after_output","error":{"code":"server_is_overloaded","message":"overloaded"},"usage":{"input_tokens":11,"output_tokens":4}}}`,
		``,
	}, "\n")
	resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}

	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, time.Now(), "model", "model")

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.NotErrorAs(t, err, &failoverErr)
	require.NotNil(t, result)
	require.Equal(t, 11, result.usage.InputTokens)
	require.Equal(t, 4, result.usage.OutputTokens)
	require.Contains(t, recorder.Body.String(), "partial")
	require.Contains(t, recorder.Body.String(), `"code":"server_error"`)
	require.NotContains(t, recorder.Body.String(), "server_is_overloaded")
	require.True(t, OpenAIStreamSemanticOutputStarted(c))
}

func TestOpenAIHTTPCapacityShedIsRequestScopedForOAuthAccounts(t *testing.T) {
	payload := []byte(`{"error":{"type":"server_error","message":"Our servers are currently overloaded. Please try again later."}}`)
	failoverErr := newOpenAIUpstreamFailoverError(
		http.StatusBadRequest,
		http.Header{"X-Request-Id": []string{"rid-http-capacity"}},
		payload,
		"Our servers are currently overloaded. Please try again later.",
		false,
	)

	require.True(t, failoverErr.RetryableOnSameAccount)
	require.True(t, failoverErr.RequestScopedTransient)

	repo := &capacityShedAccountRepoStub{}
	(&GatewayService{accountRepo: repo}).TempUnscheduleRetryableError(context.Background(), 1, failoverErr)
	require.Zero(t, repo.tempUnschedCalls)

	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	gateway := &OpenAIGatewayService{rateLimitService: rateLimitService}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.False(t, gateway.handleOpenAIAccountUpstreamError(
		context.Background(),
		account,
		http.StatusBadRequest,
		nil,
		payload,
		"gpt-5",
	))
	require.Zero(t, repo.tempUnschedCalls)
}

func TestOpenAIStreamMetadataPreambleAndMessageOnlyOverloadFailOver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	largeMetadata := strings.Repeat("x", 16*1024)
	stream := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1","metadata":{"padding":"` + largeMetadata + `"}}}`,
		"",
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","item":{"type":"reasoning","summary":[]}}`,
		"",
		"event: response.reasoning_summary_part.added",
		`data: {"type":"response.reasoning_summary_part.added","part":{"type":"summary_text","text":""}}`,
		"",
		"event: error",
		`data: {"type":"error","error":{"type":"service_unavailable_error","message":"Our servers are currently overloaded. Please try again later."}}`,
		"",
	}, "\n")

	tests := []struct {
		name string
		run  func(*OpenAIGatewayService, *gin.Context, *http.Response, *Account) error
	}{
		{
			name: "native",
			run: func(svc *OpenAIGatewayService, c *gin.Context, resp *http.Response, account *Account) error {
				_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
				return err
			},
		},
		{
			name: "passthrough",
			run: func(svc *OpenAIGatewayService, c *gin.Context, resp *http.Response, account *Account) error {
				_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(stream)),
				Header:     http.Header{"X-Request-Id": []string{"rid-message-only-overload"}},
			}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Name: "acc"}

			err := tt.run(svc, c, resp, account)
			require.Error(t, err)
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.True(t, failoverErr.RetryableOnSameAccount)
			require.True(t, failoverErr.RequestScopedTransient)
			require.Equal(t, http.StatusServiceUnavailable, failoverErr.ClientStatusCode)
			require.Contains(t, failoverErr.ClientMessage, "servers are currently overloaded")
			require.False(t, c.Writer.Written())
			require.Empty(t, rec.Body.String())
		})
	}
}

func TestClassifyOpenAIWSCapacityShedAsPreOutputFallback(t *testing.T) {
	for _, code := range []string{"server_is_overloaded", "slow_down"} {
		reason, canFallback := classifyOpenAIWSErrorEventFromRaw(code, "service_unavailable_error", "overloaded")
		require.Equal(t, "upstream_capacity_shed", reason)
		require.True(t, canFallback)
	}
}

func TestOpenAIWSOnlyOuterPathPreservesPreOutputCapacityFailover(t *testing.T) {
	c, recorder := newCapacityShedTestContext()
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	svc.cfg.Gateway.LogUpstreamErrorBody = true
	account := &Account{ID: 31, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	headers := http.Header{
		"X-Request-Id": []string{"req_ws_capacity"},
		"Cf-Ray":       []string{"ray_ws_capacity"},
	}
	raw := []byte(`{"type":"error","error":{"type":"service_unavailable_error",` +
		`"code":"server_is_overloaded","message":"capacity shed"}}`)

	// This is the exact structured error produced at the forwardOpenAIWSV2
	// pre-output boundary. The WS-only outer path must return it to Responses
	// handler for same-account/account failover instead of rendering generic
	// JSON or silently treating it as a non-retryable reconnect reason.
	wsErr, handled := svc.openAIWSV2PreOutputFailure(
		c,
		account,
		"upstream_capacity_shed",
		true,
		false,
		raw,
		"capacity shed",
		headers,
	)
	require.True(t, handled)
	// Mirror the WS-only outer boundary: structured errors are not renderable
	// fallback errors and therefore remain untouched for the handler.
	require.False(t, svc.writeOpenAIWSFallbackErrorResponse(c, account, wsErr))
	got := error(wsErr)

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, got, &failoverErr)
	require.Same(t, wsErr, failoverErr)
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.True(t, failoverErr.RequestScopedTransient)
	require.Equal(t, "req_ws_capacity", failoverErr.ResponseHeaders.Get("x-request-id"))
	require.Equal(t, "ray_ws_capacity", failoverErr.ResponseHeaders.Get("cf-ray"))
	require.NotContains(t, string(failoverErr.ResponseBody), "server_is_overloaded")
	opsRaw, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	opsEvents, ok := opsRaw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, opsEvents, 1)
	require.Equal(t, "req_ws_capacity", opsEvents[0].UpstreamRequestID)
	require.Contains(t, opsEvents[0].Detail, "server_is_overloaded", "raw payload remains available to ops only")
	require.Empty(t, recorder.Body.String())
	require.False(t, c.Writer.Written(), "outer WS-only path must not consume handler failover")
}

func TestOpenAIStreamPendingBufferIsBounded(t *testing.T) {
	var lines []string
	total := 0
	require.True(t, appendOpenAIStreamPendingLine(&lines, &total, strings.Repeat("x", openAIStreamMaxPendingPreOutputBytes-1)))
	require.False(t, appendOpenAIStreamPendingLine(&lines, &total, "overflow"))
}

// 出站身份的版本声明只能有一个来源：UA 的版本段、version 头、探针版本三处必须同源，
// 各自硬编码会漂移成互相矛盾的身份，而自相矛盾或陈旧的身份会被上游优先降载。
func TestCodexOutboundVersionHasSingleSource(t *testing.T) {
	require.True(t,
		strings.HasPrefix(codexCLIUserAgent, "codex_cli_rs/"+codexCLIVersion+" "),
		"codexCLIUserAgent=%q 必须以 codexCLIVersion=%q 作为版本段", codexCLIUserAgent, codexCLIVersion,
	)
	require.GreaterOrEqual(t, CompareVersions(codexCLIVersion, codexUpstreamMinVersion), 0,
		"codexCLIVersion=%q 不得低于上游最低门槛 %q", codexCLIVersion, codexUpstreamMinVersion,
	)
}
