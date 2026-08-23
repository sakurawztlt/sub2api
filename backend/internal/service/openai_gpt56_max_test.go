package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestExtractOpenAIReasoningEffortPreservesMaxForModelCandidates(t *testing.T) {
	body := []byte(`{"reasoning":{"effort":"max"}}`)
	require.Equal(t, "max", *extractOpenAIReasoningEffortFromBody(body, "gpt-5.6-sol", "alias-model"))
	require.Equal(t, "max", *extractOpenAIReasoningEffortFromBody(body, "deepseek-v4-pro", "alias-model"))
	require.Equal(t, "xhigh", *extractOpenAIReasoningEffortFromBody(body, "gpt-5.5", "gpt-5.6-sol"), "the first effective model candidate governs explicit effort support")
}

func TestExtractOpenAIReasoningEffortDerivesFromFallbackModelCandidate(t *testing.T) {
	require.Equal(t, "max", *extractOpenAIReasoningEffortFromBody([]byte(`{}`), "gpt-5.6-sol", "gpt-5.6-sol-max"))
}

func TestNormalizeOpenAICodexCompactReasoningEffortForAccountScopesCompatibility(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.6-sol","input":"compact me","reasoning":{"effort":"max"}}`)
	tests := []struct {
		name    string
		path    string
		account *Account
		changed bool
		want    string
	}{
		{name: "OpenAI OAuth compact", path: "/v1/responses/compact", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, changed: true, want: "xhigh"},
		{name: "OpenAI OAuth normal", path: "/v1/responses", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, want: "max"},
		{name: "OpenAI API key compact", path: "/v1/responses/compact", account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, want: "max"},
		{name: "Grok OAuth compact", path: "/v1/responses/compact", account: &Account{Platform: PlatformGrok, Type: AccountTypeOAuth}, want: "max"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, nil)
			normalized, changed, err := normalizeOpenAICodexCompactReasoningEffortForAccount(c, tt.account, body)
			require.NoError(t, err)
			require.Equal(t, tt.changed, changed)
			require.Equal(t, tt.want, gjson.GetBytes(normalized, "reasoning.effort").String())
		})
	}
}
