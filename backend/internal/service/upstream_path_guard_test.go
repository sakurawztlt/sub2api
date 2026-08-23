package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSanitizedUpstreamPathSuffixRejectsNonConformingSegments(t *testing.T) {
	rejected := []string{
		"/..", "/../..", "/../../x/y", "/./compact", "/compact/..",
		`/..\..\x`, `/compact\..`, "/?a=b", "/compact?a=b", "/compact#frag",
		"/compact%2f..", "/100%", "//double", "/compact//detail", "/compact/",
		"/ compact", "/compact\x00", "/compact\nX-Injected: 1", "/模型", "compact",
		" /compact", "/compact ",
		"/a:b", "/a;b", "/a,b", "/a=b", "/a&b", "/a~b", "/a@b", "/a+b",
		"/a|b", "/a*b", "/a$b", "/a(b)", "/a'b", "/a\"b", "/a<b", "/a\tb",
		"/a b", "/a∕b", "/a／b", "/...", "/....", "/compact/...",
	}
	for _, suffix := range rejected {
		t.Run("reject_"+suffix, func(t *testing.T) {
			got, ok := sanitizedUpstreamPathSuffix(suffix)
			require.False(t, ok, "suffix %q must be rejected", suffix)
			require.Empty(t, got)
		})
	}

	accepted := map[string]string{
		"": "", "/compact": "/compact", "/compact/detail": "/compact/detail",
		"/resp_68f0a1b2c3d4/cancel": "/resp_68f0a1b2c3d4/cancel",
		"/gemini-2.5-pro_v1.2":      "/gemini-2.5-pro_v1.2",
		"/a.b.c":                    "/a.b.c",
	}
	for suffix, want := range accepted {
		t.Run("accept_"+suffix, func(t *testing.T) {
			got, ok := sanitizedUpstreamPathSuffix(suffix)
			require.True(t, ok, "suffix %q must be accepted", suffix)
			require.Equal(t, want, got)
		})
	}
}

func TestSanitizedUpstreamPathSuffixEnforcesBounds(t *testing.T) {
	_, ok := sanitizedUpstreamPathSuffix("/" + strings.Repeat("a", maxUpstreamPathSegmentLen+1))
	require.False(t, ok)
	_, ok = sanitizedUpstreamPathSuffix(strings.Repeat("/a", maxUpstreamPathSegments+1))
	require.False(t, ok)
}

func TestOpenAIResponsesRequestPathSuffixRejectsNonConformingSubpaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{
		"/v1/responses/../../x/y", "/v1/responses/..%2f..%2fx/y",
		"/v1/responses/%2e%2e/%2e%2e/x", "/responses/%2e%2e%2fx",
		"/backend-api/codex/responses/../../../x", `/v1/responses/..\..\x`,
		"/v1/responses/%3fa=b", "/v1/responses/x%23frag", "/v1/responses//double",
		"/v1/responses/compact%20", "/v1/responses/compact//",
	} {
		t.Run(path, func(t *testing.T) {
			c := newResponsesSuffixTestContext(t, path)
			require.False(t, IsForwardableOpenAIResponsesRequestPath(c))
			require.Empty(t, openAIResponsesRequestPathSuffix(c))
			require.Equal(t, chatgptCodexURL,
				appendOpenAIResponsesRequestPathSuffix(chatgptCodexURL, openAIResponsesRequestPathSuffix(c)))
			require.False(t, isOpenAIResponsesCompactPath(c))
		})
	}

	for path, want := range map[string]string{
		"/v1/responses": "", "/v1/responses/compact": "/compact",
		"/responses/compact/":                  "/compact",
		"/backend-api/codex/responses/compact": "/compact",
	} {
		t.Run("forwardable_"+path, func(t *testing.T) {
			c := newResponsesSuffixTestContext(t, path)
			require.True(t, IsForwardableOpenAIResponsesRequestPath(c))
			require.Equal(t, want, openAIResponsesRequestPathSuffix(c))
		})
	}
}

func TestIsOpenAIResponsesInputTokensRequestPath(t *testing.T) {
	for _, path := range []string{"/v1/responses/input_tokens", "/responses/input_tokens", "/backend-api/codex/responses/input_tokens"} {
		c := newResponsesSuffixTestContext(t, path)
		require.True(t, IsOpenAIResponsesInputTokensRequestPath(c), "path=%s", path)
	}
	c := newResponsesSuffixTestContext(t, "/v1/responses/compact")
	require.False(t, IsOpenAIResponsesInputTokensRequestPath(c))
}

func TestIsOpenAIResponsesCompactPathUsesLegacyEndpointShape(t *testing.T) {
	legacyPaths := []string{
		"/v1/responses/compact",
		"/v1/responses/compact/detail",
		"/responses/compact/",
	}
	for _, path := range legacyPaths {
		t.Run("legacy_"+path, func(t *testing.T) {
			c := newResponsesSuffixTestContext(t, path)
			require.True(t, IsOpenAIResponsesCompactPath(c))
		})
	}

	nonLegacyPaths := []string{
		"/v1/responses",
		"/openai/v1/responses",
		"/responses",
		"/backend-api/codex/responses",
		"/v1/responses/resp_123/cancel",
	}
	for _, path := range nonLegacyPaths {
		t.Run("non_legacy_"+path, func(t *testing.T) {
			c := newResponsesSuffixTestContext(t, path)
			require.False(t, IsOpenAIResponsesCompactPath(c))
		})
	}
}

func TestAppendOpenAIResponsesRequestPathSuffixRefusesUnsafeSuffix(t *testing.T) {
	require.Equal(t, chatgptCodexURL, appendOpenAIResponsesRequestPathSuffix(chatgptCodexURL, "/../../x"))
	require.Equal(t, chatgptCodexURL, appendOpenAIResponsesRequestPathSuffix(chatgptCodexURL, "/?a=b"))
	require.Equal(t, chatgptCodexURL+"/compact", appendOpenAIResponsesRequestPathSuffix(chatgptCodexURL, "/compact"))
}

func newResponsesSuffixTestContext(t *testing.T, path string) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	return c
}
