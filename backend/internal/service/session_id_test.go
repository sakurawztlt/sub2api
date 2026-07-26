package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newPersistedSessionContext(t *testing.T, headers map[string]string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"prompt_cache_key":"body-cache","metadata":{"user_id":{"session_id":"body-session"}}}`,
	))
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	c.Request = req
	return c
}

func TestSanitizeSessionIDStrictValidation(t *testing.T) {
	asciiBound := strings.Repeat("a", maxPersistedSessionIDLength)
	multibyteBound := strings.Repeat("好", maxPersistedSessionIDLength)
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty"},
		{name: "spaces only", raw: "   "},
		{name: "trim spaces", raw: "  session-123  ", want: "session-123"},
		{name: "ascii bound", raw: asciiBound, want: asciiBound},
		{name: "multibyte bound", raw: multibyteBound, want: multibyteBound},
		{name: "ascii over bound", raw: asciiBound + "a"},
		{name: "multibyte over bound", raw: multibyteBound + "好"},
		{name: "invalid utf8", raw: string([]byte{'s', 0xff})},
		{name: "carriage return", raw: "a\rb"},
		{name: "line feed", raw: "a\nb"},
		{name: "tab", raw: "a\tb"},
		{name: "nul", raw: "a\x00b"},
		{name: "delete", raw: "a\x7fb"},
		{name: "unicode next line", raw: "a\u0085b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, sanitizeSessionID(tc.raw))
		})
	}
}

func TestExtractClientSessionIDExplicitHeaders(t *testing.T) {
	tests := []struct {
		header string
		value  string
	}{
		{header: "conversation_id", value: "conversation"},
		{header: "session_id", value: "session"},
		{header: openCodeSessionIDHeader, value: "opencode-id"},
		{header: openCodeNativeSessionHeader, value: "opencode-native"},
		{header: codeBuddyConversationHeader, value: "codebuddy"},
		{header: "X-Session-Affinity", value: "affinity"},
	}
	for _, tc := range tests {
		t.Run(tc.header, func(t *testing.T) {
			c := newPersistedSessionContext(t, map[string]string{tc.header: tc.value})
			require.Equal(t, tc.value, ExtractClientSessionID(c))
		})
	}
}

func TestExtractClientSessionIDNilAndAbsent(t *testing.T) {
	require.Empty(t, ExtractClientSessionID(nil))
	require.Empty(t, ExtractClientSessionID(&gin.Context{}))
	require.Empty(t, ExtractClientSessionID(newPersistedSessionContext(t, nil)))
}

func TestExtractClientSessionIDMatchesStickyHeaderPrecedence(t *testing.T) {
	c := newPersistedSessionContext(t, map[string]string{
		"conversation_id":           "conversation-first",
		"session_id":                "session-second",
		openCodeSessionIDHeader:     "extended-third",
		codeBuddyConversationHeader: "extended-fourth",
		"X-Session-Affinity":        "affinity-last",
	})
	require.Equal(t, "conversation-first", ExtractClientSessionID(c))
}

func TestExtractClientSessionIDRejectsSyntheticAndBodySignals(t *testing.T) {
	c := newPersistedSessionContext(t, map[string]string{
		"X-Claude-Code-Session-Id": "relay-synthetic",
		"X-GCR-Request-Id":         "relay-request",
		"prompt_cache_key":         "header-cache",
		"X-Request-Id":             "request-id",
	})
	require.Empty(t, ExtractClientSessionID(c))
}

func TestExtractClientSessionIDDoesNotMutateHeadersOrStickyRouting(t *testing.T) {
	c := newPersistedSessionContext(t, map[string]string{
		"session_id":         "explicit-session",
		"X-Session-Affinity": "affinity",
	})
	body := []byte(`{"prompt_cache_key":"body-cache","model":"gpt-5","input":"hello"}`)
	beforeHeaders := c.Request.Header.Clone()
	svc := &OpenAIGatewayService{}
	beforeHash := svc.GenerateSessionHash(c, body)

	require.Equal(t, "explicit-session", ExtractClientSessionID(c))
	require.Equal(t, beforeHeaders, c.Request.Header)
	require.Equal(t, beforeHash, svc.GenerateSessionHash(c, body))
}
