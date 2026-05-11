package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func mustRawJSON(t *testing.T, s string) json.RawMessage {
	t.Helper()
	return json.RawMessage(s)
}

func TestShouldAutoInjectPromptCacheKeyForCompat(t *testing.T) {
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-5.5"))
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-5.4"))
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-5.4-mini"))
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-5.2"))
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-5.3"))
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-5.3-codex"))
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-5.3-codex-spark"))
	require.False(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-4o"))
}

func TestDeriveCompatPromptCacheKey_StableAcrossLaterTurns(t *testing.T) {
	base := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.4",
		Messages: []apicompat.ChatMessage{
			{Role: "system", Content: mustRawJSON(t, `"You are helpful."`)},
			{Role: "user", Content: mustRawJSON(t, `"Hello"`)},
		},
	}
	extended := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.4",
		Messages: []apicompat.ChatMessage{
			{Role: "system", Content: mustRawJSON(t, `"You are helpful."`)},
			{Role: "user", Content: mustRawJSON(t, `"Hello"`)},
			{Role: "assistant", Content: mustRawJSON(t, `"Hi there!"`)},
			{Role: "user", Content: mustRawJSON(t, `"How are you?"`)},
		},
	}

	k1 := deriveCompatPromptCacheKey(base, "gpt-5.4")
	k2 := deriveCompatPromptCacheKey(extended, "gpt-5.4")
	require.Equal(t, k1, k2, "cache key should be stable across later turns")
	require.NotEmpty(t, k1)
}

func TestDeriveCompatPromptCacheKey_DiffersAcrossSessions(t *testing.T) {
	req1 := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.4",
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: mustRawJSON(t, `"Question A"`)},
		},
	}
	req2 := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.4",
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: mustRawJSON(t, `"Question B"`)},
		},
	}

	k1 := deriveCompatPromptCacheKey(req1, "gpt-5.4")
	k2 := deriveCompatPromptCacheKey(req2, "gpt-5.4")
	require.NotEqual(t, k1, k2, "different first user messages should yield different keys")
}

func TestDeriveCompatPromptCacheKey_UsesResolvedSparkFamily(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.3-codex-spark",
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: mustRawJSON(t, `"Question A"`)},
		},
	}

	k1 := deriveCompatPromptCacheKey(req, "gpt-5.3-codex-spark")
	k2 := deriveCompatPromptCacheKey(req, " openai/gpt-5.3-codex-spark ")
	require.NotEmpty(t, k1)
	require.Equal(t, k1, k2, "resolved spark family should derive a stable compat cache key")
}

func TestDeriveAnthropicCompatPromptCacheKey_StableAcrossLaterTurns(t *testing.T) {
	base := &apicompat.AnthropicRequest{
		Model:  "claude-sonnet-4-5",
		System: mustRawJSON(t, `"You are helpful."`),
		Messages: []apicompat.AnthropicMessage{
			{Role: "user", Content: mustRawJSON(t, `"Open repo"`)},
		},
	}
	extended := &apicompat.AnthropicRequest{
		Model:  "claude-sonnet-4-5",
		System: mustRawJSON(t, `"You are helpful."`),
		Messages: []apicompat.AnthropicMessage{
			{Role: "user", Content: mustRawJSON(t, `"Open repo"`)},
			{Role: "assistant", Content: mustRawJSON(t, `"Opened."`)},
			{Role: "user", Content: mustRawJSON(t, `"Run tests"`)},
		},
	}

	k1 := deriveAnthropicCompatPromptCacheKey(base, "gpt-5.3-codex")
	k2 := deriveAnthropicCompatPromptCacheKey(extended, "gpt-5.3-codex")
	require.NotEmpty(t, k1)
	require.Equal(t, k1, k2, "cache key should stay stable as later Claude Code turns append history")
}

// 5/10 codex P3 #5: cache_control anchor function MUST include tools /
// tool_choice / model in seed so different tools/models with same cached
// content don't collide. Two requests with same cache_control but different
// tools should produce different keys.
func TestDeriveAnthropicCacheControlPromptCacheKey_DifferentToolsProduceDifferentKeys(t *testing.T) {
	base := &apicompat.AnthropicRequest{
		Model: "claude-sonnet-4-5",
		System: mustRawJSON(t, `[{"type":"text","text":"system text","cache_control":{"type":"ephemeral"}}]`),
		Messages: []apicompat.AnthropicMessage{
			{Role: "user", Content: mustRawJSON(t, `[{"type":"text","text":"user anchor","cache_control":{"type":"ephemeral"}}]`)},
		},
	}
	withToolsA := *base
	withToolsA.Tools = []apicompat.AnthropicTool{
		{Name: "tool_a", Description: "tool a"},
	}
	withToolsB := *base
	withToolsB.Tools = []apicompat.AnthropicTool{
		{Name: "tool_b", Description: "tool b"},
	}
	kBase := deriveAnthropicCacheControlPromptCacheKey(base)
	kA := deriveAnthropicCacheControlPromptCacheKey(&withToolsA)
	kB := deriveAnthropicCacheControlPromptCacheKey(&withToolsB)
	require.NotEmpty(t, kBase)
	require.NotEqual(t, kBase, kA, "adding tools should change key")
	require.NotEqual(t, kA, kB, "different tools should produce different keys")
}

func TestDeriveAnthropicCacheControlPromptCacheKey_DifferentModelProducesDifferentKey(t *testing.T) {
	base := &apicompat.AnthropicRequest{
		Model: "claude-sonnet-4-5",
		System: mustRawJSON(t, `[{"type":"text","text":"system text","cache_control":{"type":"ephemeral"}}]`),
		Messages: []apicompat.AnthropicMessage{
			{Role: "user", Content: mustRawJSON(t, `[{"type":"text","text":"user anchor","cache_control":{"type":"ephemeral"}}]`)},
		},
	}
	other := *base
	other.Model = "claude-opus-4-7"
	k1 := deriveAnthropicCacheControlPromptCacheKey(base)
	k2 := deriveAnthropicCacheControlPromptCacheKey(&other)
	require.NotEqual(t, k1, k2, "different model should produce different key")
}

// 5/10 codex P3 #5 namespace: applyOpenAICompatPromptCacheKeyNamespace prefixes
// derived key with `a{accountID}k{apiKeyID}_`, so two sub2api accounts hitting
// the same content don't share an upstream OpenAI prefix cache bucket.
func TestApplyOpenAICompatPromptCacheKeyNamespace(t *testing.T) {
	cases := []struct {
		name      string
		account   *Account
		apiKeyID  int64
		key       string
		want      string
	}{
		{"nil account no-op", nil, 0, "anthropic-cache-deadbeef", "anthropic-cache-deadbeef"},
		{"empty account no-op", &Account{}, 0, "anthropic-cache-deadbeef", "anthropic-cache-deadbeef"},
		{"empty key no-op", &Account{ID: 1}, 1, "", ""},
		{"prefixes a1k2", &Account{ID: 1}, 2, "anthropic-cache-deadbeef", "a1k2_anthropic-cache-deadbeef"},
		{"prefixes a99k0 zero apikey", &Account{ID: 99}, 0, "anthropic-digest-x", "a99k0_anthropic-digest-x"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := applyOpenAICompatPromptCacheKeyNamespace(tt.account, tt.apiKeyID, tt.key)
			require.Equal(t, tt.want, got)
		})
	}
}

// 5/10 codex P3 #5: digest chain MUST include model + tools + tool_choice
// so different tools/models with same messages don't collide.
func TestBuildOpenAICompatAnthropicDigestChain_DifferentToolsProduceDifferentChain(t *testing.T) {
	base := &apicompat.AnthropicRequest{
		Model:    "claude-sonnet-4-5",
		System:   mustRawJSON(t, `"You are helpful."`),
		Messages: []apicompat.AnthropicMessage{{Role: "user", Content: mustRawJSON(t, `"hi"`)}},
	}
	withTool := *base
	withTool.Tools = []apicompat.AnthropicTool{{Name: "mytool", Description: "x"}}

	c1 := buildOpenAICompatAnthropicDigestChain(base)
	c2 := buildOpenAICompatAnthropicDigestChain(&withTool)
	require.NotEqual(t, c1, c2, "tools change should change digest chain")
	require.True(t, strings.HasPrefix(c2, "m:claude-sonnet-4-5-tl:"), "tools digest segment must follow model: got %q", c2)
}

func TestBuildOpenAICompatAnthropicDigestChain_DifferentModelProducesDifferentChain(t *testing.T) {
	base := &apicompat.AnthropicRequest{
		Model:    "claude-sonnet-4-5",
		System:   mustRawJSON(t, `"You are helpful."`),
		Messages: []apicompat.AnthropicMessage{{Role: "user", Content: mustRawJSON(t, `"hi"`)}},
	}
	other := *base
	other.Model = "claude-opus-4-7"
	c1 := buildOpenAICompatAnthropicDigestChain(base)
	c2 := buildOpenAICompatAnthropicDigestChain(&other)
	require.NotEqual(t, c1, c2)
}

// 同样 model + system + messages → 同 chain (deterministic + 跨轮稳定).
func TestBuildOpenAICompatAnthropicDigestChain_PrefixRelationshipPreserved(t *testing.T) {
	round1 := &apicompat.AnthropicRequest{
		Model:    "claude-sonnet-4-5",
		System:   mustRawJSON(t, `"You are helpful."`),
		Messages: []apicompat.AnthropicMessage{{Role: "user", Content: mustRawJSON(t, `"hi"`)}},
	}
	round2 := *round1
	round2.Messages = []apicompat.AnthropicMessage{
		{Role: "user", Content: mustRawJSON(t, `"hi"`)},
		{Role: "assistant", Content: mustRawJSON(t, `"hello"`)},
		{Role: "user", Content: mustRawJSON(t, `"how"`)},
	}
	c1 := buildOpenAICompatAnthropicDigestChain(round1)
	c2 := buildOpenAICompatAnthropicDigestChain(&round2)
	require.True(t, strings.HasPrefix(c2, c1), "prefix relationship must hold across turns: c1=%q c2=%q", c1, c2)
}

// 5/10 codex xhigh 复核 #1: chat completions 路径调 deriveCompatPromptCacheKey
// 之后必须 namespace 包一层. 这个测验单元 helper 输出符合预期 — caller
// integration test 在 chat completions handler 测试里 (此 PR 改了 line 88
// + line 226-228 两处).
func TestDeriveCompatPromptCacheKey_NamespaceWrapping(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Model: "gpt-5.4",
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: mustRawJSON(t, `"hello"`)},
		},
	}
	bare := deriveCompatPromptCacheKey(req, "gpt-5.4")
	require.NotEmpty(t, bare)
	require.True(t, strings.HasPrefix(bare, compatPromptCacheKeyPrefix))

	// account.ID=42, apiKeyID=7 → expect a42k7_ prefix wrap.
	wrapped := applyOpenAICompatPromptCacheKeyNamespace(&Account{ID: 42}, 7, bare)
	require.True(t, strings.HasPrefix(wrapped, "a42k7_"))
	require.Contains(t, wrapped, bare)

	// Different account → different wrapped key
	wrappedB := applyOpenAICompatPromptCacheKeyNamespace(&Account{ID: 99}, 7, bare)
	require.NotEqual(t, wrapped, wrappedB, "different account must produce different wrapped key")
}

func TestDeriveAnthropicCompatPromptCacheKey_UsesCacheControlAnchors(t *testing.T) {
	base := &apicompat.AnthropicRequest{
		Model: "claude-sonnet-4-5",
		System: mustRawJSON(t, `[
			{"type":"text","text":"project instructions","cache_control":{"type":"ephemeral"}}
		]`),
		Messages: []apicompat.AnthropicMessage{
			{Role: "user", Content: mustRawJSON(t, `[
				{"type":"text","text":"repo anchor","cache_control":{"type":"ephemeral"}}
			]`)},
		},
	}
	extended := &apicompat.AnthropicRequest{
		Model:  base.Model,
		System: base.System,
		Messages: []apicompat.AnthropicMessage{
			base.Messages[0],
			{Role: "assistant", Content: mustRawJSON(t, `[{"type":"text","text":"Opened."}]`)},
			{Role: "user", Content: mustRawJSON(t, `[{"type":"text","text":"Run tests"}]`)},
		},
	}

	k1 := deriveAnthropicCompatPromptCacheKey(base, "gpt-5.4")
	k2 := deriveAnthropicCompatPromptCacheKey(extended, "gpt-5.4")
	require.NotEmpty(t, k1)
	require.Equal(t, k1, k2)
	require.True(t, strings.HasPrefix(k1, "anthropic-cache-"))
	require.False(t, strings.HasPrefix(k1, compatPromptCacheKeyPrefix))
}
