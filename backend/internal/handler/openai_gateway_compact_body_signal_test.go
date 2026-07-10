package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

func newCompactBodySignalTestContext(t *testing.T, path string, body []byte) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

func TestNormalizeOpenAIResponsesCompactRequestRemoteV2StaysNative(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	body := []byte(`{"model":"gpt-5.6-sol","stream":true,"store":true,"prompt_cache_key":"pck-1","reasoning":{"effort":"max"},"input":[{"type":"compaction_trigger"}]}`)
	c := newCompactBodySignalTestContext(t, "/v1/responses", body)
	c.Request.Header.Set("x-codex-beta-features", "responses_websockets_v2, remote_compaction_v2")

	normalized, ok := h.normalizeOpenAIResponsesCompactRequest(c, zap.NewNop(), body)
	require.True(t, ok)
	require.Equal(t, "/v1/responses", c.Request.URL.Path)
	require.Equal(t, body, normalized)
	require.True(t, gjson.GetBytes(normalized, "stream").Bool())
	_, hasSeed := c.Get(service.OpenAICompactSessionSeedKeyForTest())
	require.False(t, hasSeed)
	_, hasStreamMarker := c.Get(service.OpenAICompactClientStreamKeyForTest())
	require.False(t, hasStreamMarker)
}

func TestNormalizeOpenAIResponsesCompactRequestPromotesLegacyBodySignal(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	tests := []struct {
		name       string
		path       string
		stream     string
		beta       string
		wantMarked bool
	}{
		{name: "streaming", path: "/v1/responses", stream: `,"stream":true`, wantMarked: true},
		{name: "trailing slash", path: "/v1/responses/", stream: `,"stream":true`, wantMarked: true},
		{name: "codex alias", path: "/backend-api/codex/responses", stream: `,"stream":true`, wantMarked: true},
		{name: "wrong case feature", path: "/v1/responses", stream: `,"stream":true`, beta: "REMOTE_COMPACTION_V2", wantMarked: true},
		{name: "non streaming", path: "/v1/responses", stream: `,"stream":false`, beta: "remote_compaction_v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.5"` + tt.stream + `,"prompt_cache_key":"seed","input":[{"type":"compaction_trigger"}]}`)
			c := newCompactBodySignalTestContext(t, tt.path, body)
			if tt.beta != "" {
				c.Request.Header.Set("x-codex-beta-features", tt.beta)
			}

			normalized, ok := h.normalizeOpenAIResponsesCompactRequest(c, zap.NewNop(), body)
			require.True(t, ok)
			require.Equal(t, "/compact", c.Request.URL.Path[len(c.Request.URL.Path)-len("/compact"):])
			require.False(t, gjson.GetBytes(normalized, "stream").Exists())
			_, marked := c.Get(service.OpenAICompactClientStreamKeyForTest())
			require.Equal(t, tt.wantMarked, marked)
			seed, hasSeed := c.Get(service.OpenAICompactSessionSeedKeyForTest())
			require.True(t, hasSeed)
			require.Equal(t, "seed", seed)
		})
	}
}

func TestNormalizeOpenAIResponsesCompactRequestLeavesUnrelatedRoutesAlone(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	tests := []struct {
		name string
		path string
		body []byte
	}{
		{name: "no trigger", path: "/v1/responses", body: []byte(`{"model":"gpt-5.5","stream":true,"input":[{"type":"message"}]}`)},
		{name: "responses subpath", path: "/v1/responses/resp_123/cancel", body: []byte(`{"model":"gpt-5.5","input":[{"type":"compaction_trigger"}]}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCompactBodySignalTestContext(t, tt.path, tt.body)
			normalized, ok := h.normalizeOpenAIResponsesCompactRequest(c, zap.NewNop(), tt.body)
			require.True(t, ok)
			require.Equal(t, tt.path, c.Request.URL.Path)
			require.Equal(t, tt.body, normalized)
		})
	}
}

func TestNormalizeOpenAIResponsesCompactRequestPathBasedStaysJSON(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	body := []byte(`{"model":"gpt-5.5","stream":true,"store":true,"input":[{"type":"message"}]}`)
	c := newCompactBodySignalTestContext(t, "/v1/responses/compact", body)

	normalized, ok := h.normalizeOpenAIResponsesCompactRequest(c, zap.NewNop(), body)
	require.True(t, ok)
	require.Equal(t, "/v1/responses/compact", c.Request.URL.Path)
	require.False(t, gjson.GetBytes(normalized, "stream").Exists())
	require.False(t, gjson.GetBytes(normalized, "store").Exists())
	_, marked := c.Get(service.OpenAICompactClientStreamKeyForTest())
	require.False(t, marked)
}
