package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestTrimOpenAIEncryptedReasoningItems_Compaction(t *testing.T) {
	tests := []struct {
		name      string
		itemType  string
		encrypted bool
		changed   bool
	}{
		{name: "compaction", itemType: "compaction", encrypted: true, changed: true},
		{name: "compaction summary", itemType: "compaction_summary", encrypted: true, changed: true},
		{name: "unencrypted compaction", itemType: "compaction", encrypted: false, changed: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := map[string]any{"type": tt.itemType, "id": "cmp_stale"}
			if tt.encrypted {
				item["encrypted_content"] = "gAAA"
			}
			reqBody := map[string]any{"input": []any{
				item,
				map[string]any{"type": "message", "content": "hi"},
			}}

			changed := trimOpenAIEncryptedReasoningItems(reqBody)
			require.Equal(t, tt.changed, changed)
			input, ok := reqBody["input"].([]any)
			require.True(t, ok)
			require.NotEmpty(t, input)
			first, ok := input[0].(map[string]any)
			require.True(t, ok)
			if tt.changed {
				require.Len(t, input, 1)
				require.Equal(t, "message", first["type"])
				return
			}
			require.Len(t, input, 2)
			require.Equal(t, tt.itemType, first["type"])
		})
	}
}

func TestSanitizeOpenAICrossModeFailoverReasoning_DropsWholeEncryptedItem(t *testing.T) {
	body := []byte(`{"model":"gpt-5.1","input":[` +
		`{"type":"message","role":"user","content":"hi"},` +
		`{"type":"reasoning","id":"rs_kiro_1","encrypted_content":"ENC","summary":[{"type":"summary_text","text":"t"}]},` +
		`{"type":"message","role":"assistant","content":"yo"}` +
		`]}`)

	sanitized, changed, err := SanitizeOpenAICrossModeFailoverReasoning(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotContains(t, string(sanitized), "reasoning")
	require.NotContains(t, string(sanitized), "rs_kiro_1")
	require.NotContains(t, string(sanitized), "summary_text")
	require.Equal(t, int64(2), gjson.GetBytes(sanitized, "input.#").Int())
}

func TestSanitizeOpenAICrossModeFailoverReasoning_NoEncryptedIsNoop(t *testing.T) {
	body := []byte(`{"model":"gpt-5.1","input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"t"}]}]}`)
	sanitized, changed, err := SanitizeOpenAICrossModeFailoverReasoning(body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, string(body), string(sanitized))
}

func TestSanitizeOpenAICrossModeFailoverReasoning_NoInputIsNoop(t *testing.T) {
	body := []byte(`{"model":"gpt-5.1"}`)
	sanitized, changed, err := SanitizeOpenAICrossModeFailoverReasoning(body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, string(body), string(sanitized))
}

func TestSanitizeOpenAICrossModeFailoverReasoning_PreservesLargeIntegers(t *testing.T) {
	body := []byte(`{"model":"gpt-5.1","input":[` +
		`{"type":"reasoning","id":"rs_kiro_1","encrypted_content":"ENC"},` +
		`{"type":"message","role":"user","content":"hi"}` +
		`],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"id":{"const":9007199254740993}}}}}]}`)

	sanitized, changed, err := SanitizeOpenAICrossModeFailoverReasoning(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Contains(t, string(sanitized), `"const":9007199254740993`)
}

func TestNormalizeOpenAIAPIKeyStoreFalseReasoningReplay(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","store":false,"input":[` +
		`{"type":"reasoning","id":"rs_encrypted","call_id":"remove","encrypted_content":"cipher","summary":null,"opaque":9007199254740993},` +
		`{"type":"reasoning","id":"rs_server_only","summary":[{"type":"summary_text","text":"drop"}]},` +
		`{"type":"item_reference","id":"rs_server_only"},` +
		`{"type":"item_reference","id":"msg_keep"},` +
		`{"type":"message","id":"msg_keep","role":"user","content":"continue"}` +
		`]}`)

	normalized, changed, err := normalizeOpenAIAPIKeyStoreFalseReasoningReplay(body, false)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, int64(3), gjson.GetBytes(normalized, "input.#").Int())
	require.Equal(t, "reasoning", gjson.GetBytes(normalized, "input.0.type").String())
	require.False(t, gjson.GetBytes(normalized, "input.0.id").Exists())
	require.False(t, gjson.GetBytes(normalized, "input.0.call_id").Exists())
	require.Equal(t, "cipher", gjson.GetBytes(normalized, "input.0.encrypted_content").String())
	require.True(t, gjson.GetBytes(normalized, "input.0.summary").IsArray())
	require.Equal(t, "9007199254740993", gjson.GetBytes(normalized, "input.0.opaque").Raw)
	require.Equal(t, "msg_keep", gjson.GetBytes(normalized, "input.1.id").String())
	require.Equal(t, "message", gjson.GetBytes(normalized, "input.2.type").String())
}

func TestNormalizeOpenAIAPIKeyStoreFalseReasoningReplayRequiresExplicitStoreFalse(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"type":"reasoning","id":"rs_keep"}]}`,
		`{"store":true,"input":[{"type":"reasoning","id":"rs_keep"}]}`,
	} {
		normalized, changed, err := normalizeOpenAIAPIKeyStoreFalseReasoningReplay([]byte(body), false)
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, body, string(normalized))
	}
}

func TestNormalizeOpenAIAPIKeyStoreFalseReasoningReplayKnownCompactMode(t *testing.T) {
	body := []byte(`{"input":[{"type":"reasoning","id":"rs_drop","summary":[]},{"type":"message","content":"continue"}]}`)

	normalized, changed, err := normalizeOpenAIAPIKeyStoreFalseReasoningReplay(body, true)

	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, int64(1), gjson.GetBytes(normalized, "input.#").Int())
	require.Equal(t, "message", gjson.GetBytes(normalized, "input.0.type").String())
}

func TestNormalizeOpenAIAPIKeyStoreFalseReasoningReplayRejectsEmptyEncryptedContent(t *testing.T) {
	for _, encrypted := range []string{"null", `""`, `"   "`, "123"} {
		body := []byte(`{"store":false,"input":[{"type":"reasoning","id":"rs_drop","encrypted_content":` + encrypted + `},{"type":"message","content":"continue"}]}`)
		normalized, changed, err := normalizeOpenAIAPIKeyStoreFalseReasoningReplay(body, false)
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, int64(1), gjson.GetBytes(normalized, "input.#").Int())
		require.Equal(t, "message", gjson.GetBytes(normalized, "input.0.type").String())
	}
}

func TestNormalizeOpenAIParallelToolCallsWithoutTools(t *testing.T) {
	withTools := []byte(`{"tools":[{"type":"function","name":"lookup"}],"parallel_tool_calls":false}`)
	normalized, changed, err := normalizeOpenAIParallelToolCallsWithoutTools(withTools)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, string(withTools), string(normalized))

	withoutTools := []byte(`{"input":"hi","parallel_tool_calls":true}`)
	normalized, changed, err = normalizeOpenAIParallelToolCallsWithoutTools(withoutTools)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(normalized, "parallel_tool_calls").Exists())
}
