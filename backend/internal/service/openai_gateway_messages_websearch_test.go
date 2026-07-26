package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestAnthropicWebSearchRequestLimitForResponse_UsesMaxUses(t *testing.T) {
	var req apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":512,
		"messages":[{"role":"user","content":"latest AI news"}],
		"tools":[{
			"type":"web_search_20250305",
			"name":"web_search",
			"max_uses":1
		}]
	}`), &req))

	require.Equal(t, 1, anthropicWebSearchRequestLimitForResponse(&req))
}

func TestAnthropicWebSearchRequestLimitForResponse_PreservesOrdinaryMaxUses(t *testing.T) {
	var req apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":512,
		"messages":[{"role":"user","content":"research several AI stories"}],
		"tools":[{
			"type":"web_search_20250305",
			"name":"web_search",
			"max_uses":8
		}]
	}`), &req))

	require.Equal(t, 8, req.Tools[0].MaxUses, "Anthropic max_uses must remain parsed")
	require.Equal(t, 8, anthropicWebSearchRequestLimitForResponse(&req))
}

func TestAnthropicWebSearchRequestLimitForResponse_SingularProbeOverridesPermissiveMaxUses(t *testing.T) {
	var req apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"claude-opus-4-8",
		"max_tokens":512,
		"messages":[{
			"role":"user",
			"content":"Use the web_search tool first, then summarize.\n\nOriginal request:\nPerform a web search for the query: AI news 2026-07-26"
		}],
		"tools":[{
			"type":"web_search_20250305",
			"name":"web_search",
			"max_uses":8
		}]
	}`), &req))

	require.Equal(t, 8, req.Tools[0].MaxUses, "Anthropic max_uses must remain parsed")
	require.Equal(t, 1, anthropicWebSearchRequestLimitForResponse(&req))
}
