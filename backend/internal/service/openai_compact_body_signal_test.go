//go:build unit

package service

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestIsOpenAINativeCompactionV2ReadsOnlyRequestMarker(t *testing.T) {
	c, _ := gin.CreateTestContext(nil)
	require.False(t, IsOpenAINativeCompactionV2(c))

	MarkOpenAINativeCompactionV2(c)
	require.True(t, IsOpenAINativeCompactionV2(c))
	require.False(t, IsOpenAINativeCompactionV2(nil))
}

func TestHasCompactionTriggerInInput(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want bool
	}{
		{name: "trigger", body: []byte(`{"model":"gpt-5.5","input":[{"type":"message"},{"type":"compaction_trigger"}]}`), want: true},
		{name: "trigger only", body: []byte(`{"input":[{"type":"compaction_trigger"}]}`), want: true},
		{name: "no trigger", body: []byte(`{"input":[{"type":"message"}]}`), want: false},
		{name: "empty input", body: []byte(`{"input":[]}`), want: false},
		{name: "string input", body: []byte(`{"input":"compaction_trigger"}`), want: false},
		{name: "missing input", body: []byte(`{"model":"gpt-5.5"}`), want: false},
		{name: "empty body", body: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, HasCompactionTriggerInInput(tt.body))
		})
	}
}

func TestNormalizeCompactionTriggerInputOrder_MovesAndCollapsesTriggers(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":[{"type":"compaction_trigger"},{"type":"message","role":"user","content":"tail"},{"type":"compaction_trigger"}]}`)
	normalized, changed, err := NormalizeCompactionTriggerInputOrder(body)
	require.NoError(t, err)
	require.True(t, changed)
	items := gjson.GetBytes(normalized, "input").Array()
	require.Len(t, items, 2)
	require.Equal(t, "message", items[0].Get("type").String())
	require.Equal(t, "compaction_trigger", items[1].Get("type").String())
}

func TestNormalizeCompactionTriggerInputOrder_AlreadyFinalPreservesBytes(t *testing.T) {
	body := []byte(`{"input":[{"type":"message"},{"type":"compaction_trigger"}]}`)
	normalized, changed, err := NormalizeCompactionTriggerInputOrder(body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, string(body), string(normalized))
}

func TestNormalizeCompactionTriggerInputOrder_PreservesHistoryAndLargeNumbers(t *testing.T) {
	body := []byte(`{"input":[{"type":"compaction_trigger"},{"type":"message","id":"msg_1","content":"visible"},{"type":"function_call_output","call_id":"call_1","output":"result","sequence":9007199254740993}]}`)

	normalized, changed, err := NormalizeCompactionTriggerInputOrder(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "message", gjson.GetBytes(normalized, "input.0.type").String())
	require.Equal(t, "function_call_output", gjson.GetBytes(normalized, "input.1.type").String())
	require.Equal(t, "9007199254740993", gjson.GetBytes(normalized, "input.1.sequence").Raw)
	require.Equal(t, "compaction_trigger", gjson.GetBytes(normalized, "input.2.type").String())

	second, changedAgain, err := NormalizeCompactionTriggerInputOrder(normalized)
	require.NoError(t, err)
	require.False(t, changedAgain)
	require.Equal(t, normalized, second)
}

func TestWebSocketCompatibilityNormalizesTriggerAfterPairedOutputCleanup(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"gpt-5.4","input":[{"type":"compaction_trigger"},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"},{"type":"message","role":"user","content":"visible"}]}`)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	normalized, changed, err := normalizeOpenAIResponsesWebSocketCompatibilityBody(body, account, false)
	require.NoError(t, err)
	require.True(t, changed)
	items := gjson.GetBytes(normalized, "input").Array()
	require.Len(t, items, 4)
	require.Equal(t, "function_call", items[0].Get("type").String())
	require.Equal(t, "function_call_output", items[1].Get("type").String())
	require.Equal(t, "message", items[2].Get("type").String())
	require.Equal(t, "compaction_trigger", items[3].Get("type").String())
}
