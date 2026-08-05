package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const anthropicMessageIDPattern = `^msg_01[A-Za-z0-9]{22}$`

func TestDetectInterceptType_MaxTokensOneHaikuRequiresClaudeCodeClient(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)

	notClaudeCode := detectInterceptType(body, "claude-haiku-4-5", 1, false, false)
	require.Equal(t, InterceptTypeNone, notClaudeCode)

	isClaudeCode := detectInterceptType(body, "claude-haiku-4-5", 1, false, true)
	require.Equal(t, InterceptTypeMaxTokensOneHaiku, isClaudeCode)
}

func TestDetectInterceptType_SuggestionModeUnaffected(t *testing.T) {
	body := []byte(`{
		"messages":[{
			"role":"user",
			"content":[{"type":"text","text":"[SUGGESTION MODE:foo]"}]
		}],
		"system":[]
	}`)

	got := detectInterceptType(body, "claude-sonnet-4-5", 256, false, false)
	require.Equal(t, InterceptTypeSuggestionMode, got)
}

func TestSendMockInterceptResponse_MaxTokensOneHaiku(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)

	sendMockInterceptResponse(ctx, "claude-haiku-4-5", InterceptTypeMaxTokensOneHaiku)

	require.Equal(t, http.StatusOK, rec.Code)

	var response map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Equal(t, "max_tokens", response["stop_reason"])

	id, ok := response["id"].(string)
	require.True(t, ok)
	require.Regexp(t, anthropicMessageIDPattern, id)
	require.Contains(t, response, "stop_details")
	require.Nil(t, response["stop_details"])

	content, ok := response["content"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, content)

	firstBlock, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "#", firstBlock["text"])

	usage, ok := response["usage"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(1), usage["output_tokens"])
	require.NotContains(t, usage, "total_tokens")
}

func TestSendMockInterceptResponse_SuggestionAndWarmupUseAnthropicIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name          string
		interceptType InterceptType
	}{
		{name: "suggestion", interceptType: InterceptTypeSuggestionMode},
		{name: "warmup", interceptType: InterceptTypeWarmup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)

			sendMockInterceptResponse(ctx, "claude-sonnet-4-5", tc.interceptType)

			var response map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
			require.Regexp(t, anthropicMessageIDPattern, response["id"])
		})
	}
}

func TestSendMockInterceptStream_UsesAnthropicMessageShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)

	sendMockInterceptStream(ctx, "claude-sonnet-4-5", InterceptTypeSuggestionMode)

	body := rec.Body.String()
	require.Regexp(t,
		`data: \{"type":"message_start","message":\{"model":"claude-sonnet-4-5","id":"msg_01[A-Za-z0-9]{22}"`,
		body,
		"message_start must retain the observed Anthropic field order",
	)

	messageStart := decodeInterceptSSEEvent(t, body, "message_start")
	message, ok := messageStart["message"].(map[string]any)
	require.True(t, ok)
	require.Regexp(t, anthropicMessageIDPattern, message["id"])
	require.Contains(t, message, "stop_details")
	require.Nil(t, message["stop_details"])
	usage, ok := message["usage"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, map[string]any{
		"input_tokens":                float64(10),
		"cache_creation_input_tokens": float64(0),
		"cache_read_input_tokens":     float64(0),
		"output_tokens":               float64(0),
	}, usage)

	messageDelta := decodeInterceptSSEEvent(t, body, "message_delta")
	delta, ok := messageDelta["delta"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, delta, "stop_details")
	require.Nil(t, delta["stop_details"])
	deltaUsage, ok := messageDelta["usage"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, map[string]any{"output_tokens": float64(1)}, deltaUsage)
}

func decodeInterceptSSEEvent(t *testing.T, body, eventName string) map[string]any {
	t.Helper()
	for _, chunk := range strings.Split(body, "\n\n") {
		lines := strings.Split(chunk, "\n")
		if len(lines) < 2 || lines[0] != "event: "+eventName || !strings.HasPrefix(lines[1], "data: ") {
			continue
		}
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &payload))
		return payload
	}
	t.Fatalf("SSE event %q not found in %q", eventName, body)
	return nil
}
