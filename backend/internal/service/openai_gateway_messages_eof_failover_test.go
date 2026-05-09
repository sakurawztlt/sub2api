package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 5/9 codex audit #2 最小版验证: 上游 stream EOF 在 !headerWritten 时返
// UpstreamFailoverError{BreakSticky:true} 让 handler retry. headerWritten
// 后维持原 fmt.Errorf 行为 (客户已收 SSE, 不能 retry).

// scenario 1: 上游立即 close (stream 没任何 data, 直接 EOF) →
// !firstMeaningfulSeen → !headerWritten → 返 UpstreamFailoverError BreakSticky
func TestForwardAsAnthropic_StreamEOFWithoutHeader_ReturnsBreakStickyFailover(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	// 上游返空 stream — 没 [DONE], 没 data, 立即 EOF
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("")),
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID: 1, Name: "openai-oauth", Platform: PlatformOpenAI,
		Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
			"model_mapping":      map[string]any{"gpt-5.4": "gpt-5.4"},
		},
	}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.4")

	require.Error(t, err, "EOF stream should fail")
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr),
		"empty stream EOF before headerWritten should return UpstreamFailoverError, got %T: %v", err, err)
	require.True(t, failoverErr.BreakSticky,
		"BreakSticky must be true so handler DeleteStickySession + retry next account")
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)

	// 客户响应不能被写 (handler 还能 retry 写新响应)
	require.Equal(t, 200, rec.Code, "rec.Code default 200 means we never WriteHeader (only handler at end writes)")
	require.Empty(t, rec.Body.String(), "rec body should be empty — service didn't write SSE")
}

// scenario 2: 上游已发 meaningful content (text_delta) 之后 EOF →
// headerWritten=true → 维持原 fmt.Errorf 不返 BreakSticky (客户已收部分 SSE)
func TestForwardAsAnthropic_StreamEOFAfterHeader_NoBreakSticky(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	// 上游发 created + 部分 text_delta 后立即 EOF (没 [DONE], 没 response.completed)
	// 这模拟: 真上游写到一半挂了
	upstreamBody := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		"",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[]}}`,
		"",
		`data: {"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		"",
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}`,
		"",
		// EOF here — no [DONE], no completed event
	}, "\n")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID: 1, Name: "openai-oauth", Platform: PlatformOpenAI,
		Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
			"model_mapping":      map[string]any{"gpt-5.4": "gpt-5.4"},
		},
	}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.4")

	// 上游断流仍是错误
	require.Error(t, err, "truncated stream still error")
	// 但 NOT BreakSticky — 客户已经收到 message_start + text_delta SSE,
	// retry 会让客户收到 2 套 message_start, 破坏 SSE 状态
	var failoverErr *UpstreamFailoverError
	if errors.As(err, &failoverErr) {
		require.False(t, failoverErr.BreakSticky,
			"after WriteHeader (client got SSE bytes), MUST NOT BreakSticky — retry breaks SSE state")
	}
	// 客户已经收过 message_start (写过 header)
	require.NotEqual(t, 0, len(rec.Body.Bytes()), "client should have received some SSE bytes")
}
