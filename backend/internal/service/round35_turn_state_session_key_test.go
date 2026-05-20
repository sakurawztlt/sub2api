package service

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// codex round35 fu54 (2026-05-20): regression tests for turn_state
// cache key derivation. The old behavior keyed turn_state strictly
// on (account, apiKeyID, promptCacheKey), which causes misses on
// every follow-up turn within the same conversation because the
// promptCacheKey rolls (it's a prefix hash of messages). The new
// behavior prefers a conversation-stable identifier
// (X-Claude-Code-Session-Id header) and falls back to
// promptCacheKey only when the header is absent.
//
// previous_response_id cache (store=true path) is intentionally
// NOT changed by this round — it still uses
// openAICompatSessionResponseKey. Both spaces share the same
// sync.Map; the "turnstate" namespace bytes in the new key
// guarantee disjoint key spaces.

func newRound35GinContextWithHeaders(headers map[string]string) *gin.Context {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c.Request = req
	return c
}

func TestRound35_TurnStateKey_PrefersSessionIdHeader(t *testing.T) {
	account := &Account{ID: 100}
	c := newRound35GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "session-abc",
	})

	key := openAICompatTurnStateKey(c, account, "rolling_hash_v1")
	require.Contains(t, key, "session-abc",
		"session id must be in the key when present")
	require.NotContains(t, key, "rolling_hash_v1",
		"promptCacheKey must be ignored when session id is present")
	require.Contains(t, key, "turnstate",
		"namespace bytes must guarantee disjoint key space from response_id cache")
}

func TestRound35_TurnStateKey_StableAcrossPromptCacheKeyRolls(t *testing.T) {
	// The whole point of codex round35: a single conversation rolls
	// through k1, k2, k3, ... but the turn_state key must stay stable.
	account := &Account{ID: 100}
	c := newRound35GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "session-abc",
	})

	key1 := openAICompatTurnStateKey(c, account, "hash_v1")
	key2 := openAICompatTurnStateKey(c, account, "hash_v2")
	key3 := openAICompatTurnStateKey(c, account, "hash_v3")

	require.Equal(t, key1, key2)
	require.Equal(t, key2, key3,
		"turn_state key MUST stay stable when only promptCacheKey rolls")
}

func TestRound35_TurnStateKey_FallbackToPromptCacheKeyWhenNoSessionId(t *testing.T) {
	// OpenAI Codex clients don't send X-Claude-Code-Session-Id.
	// They use metadata.user_id.session_id in body (handled by a
	// future round). For now, no header → fallback to promptCacheKey
	// so the existing behavior is preserved.
	account := &Account{ID: 100}
	c := newRound35GinContextWithHeaders(nil) // no headers

	key := openAICompatTurnStateKey(c, account, "rolling_hash_v1")
	require.NotEmpty(t, key)
	require.Contains(t, key, "rolling_hash_v1",
		"must fallback to promptCacheKey when no session id header")
}

func TestRound35_TurnStateKey_EmptyWhenNoSignal(t *testing.T) {
	account := &Account{ID: 100}
	c := newRound35GinContextWithHeaders(nil)

	key := openAICompatTurnStateKey(c, account, "")
	require.Empty(t, key,
		"no session id AND no promptCacheKey → empty key disables binding")
}

func TestRound35_TurnStateKey_EmptyWhenNilAccount(t *testing.T) {
	c := newRound35GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "session-abc",
	})

	key := openAICompatTurnStateKey(c, nil, "k1")
	require.Empty(t, key, "nil account → empty key")
}

func TestRound35_TurnStateKey_NamespaceDisjointFromResponseIdKey(t *testing.T) {
	// Both functions compute keys for the same sync.Map. They must
	// NEVER collide — even when fed the same primary identifier.
	account := &Account{ID: 100}
	c := newRound35GinContextWithHeaders(nil) // both functions fall to promptCacheKey

	turnStateKey := openAICompatTurnStateKey(c, account, "shared-primary")
	responseIdKey := openAICompatSessionResponseKey(c, account, "shared-primary")

	require.NotEqual(t, turnStateKey, responseIdKey,
		"turn_state and response_id cache namespaces MUST be disjoint")
	require.Contains(t, turnStateKey, "turnstate",
		"turn_state key must carry the namespace marker")
	require.NotContains(t, responseIdKey, "turnstate",
		"response_id key must NOT carry the turn_state namespace marker")
}

func TestRound35_TurnStateKey_PerAccountIsolation(t *testing.T) {
	c := newRound35GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "session-shared",
	})

	account1 := &Account{ID: 100}
	account2 := &Account{ID: 200}

	key1 := openAICompatTurnStateKey(c, account1, "k1")
	key2 := openAICompatTurnStateKey(c, account2, "k1")

	require.NotEqual(t, key1, key2,
		"different accounts MUST have different keys even with same session id")
}

func TestRound35_TurnStateKey_PerAPIKeyIsolation(t *testing.T) {
	// Two requests with the same session id but different
	// authenticated apikey id must have isolated turn_state caches.
	// getAPIKeyIDFromContext reads c.Get("api_key") expecting *APIKey.
	account := &Account{ID: 100}

	c1 := newRound35GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "session-shared",
	})
	c1.Set("api_key", &APIKey{ID: 11})

	c2 := newRound35GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "session-shared",
	})
	c2.Set("api_key", &APIKey{ID: 22})

	key1 := openAICompatTurnStateKey(c1, account, "k")
	key2 := openAICompatTurnStateKey(c2, account, "k")
	require.NotEqual(t, key1, key2,
		"different apikey ids MUST have different keys")
}

func TestRound35_TurnStateBindGet_HitAcrossPromptCacheKeyRoll(t *testing.T) {
	// The end-to-end behavior codex asked for: bind under
	// (session-S, k1); read under (session-S, k2) — must hit, because
	// the conversation is the same even though prompt_cache_key rolled.
	s := &OpenAIGatewayService{cfg: &config.Config{}}
	account := &Account{ID: 100}

	bind := newRound35GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "session-S",
	})
	s.bindOpenAICompatSessionTurnState(context.Background(), bind, account, "k1", "turnstate-payload-1")

	getCtx := newRound35GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "session-S",
	})
	state := s.getOpenAICompatSessionTurnState(context.Background(), getCtx, account, "k2_different_rolling_hash")
	require.Equal(t, "turnstate-payload-1", state,
		"turn_state must hit across promptCacheKey rolls when session id is stable (codex round35 fu54 primary goal)")
}

func TestRound35_TurnStateBindGet_IsolatesDifferentSessions(t *testing.T) {
	s := &OpenAIGatewayService{cfg: &config.Config{}}
	account := &Account{ID: 100}

	bind := newRound35GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "session-A",
	})
	s.bindOpenAICompatSessionTurnState(context.Background(), bind, account, "k1", "turnstate-A")

	getCtx := newRound35GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "session-B",
	})
	state := s.getOpenAICompatSessionTurnState(context.Background(), getCtx, account, "k1")
	require.Empty(t, state,
		"different session ids MUST isolate turn_state — a new conversation must not pick up a stale turn_state from another conversation")
}

func TestRound35_TurnStateBindGet_FallbackPromptCacheKeyClientStillWorks(t *testing.T) {
	// Clients without session id header (e.g. OpenAI Codex which uses
	// metadata.user_id.session_id in body — not yet supported by this
	// round) fall through to promptCacheKey behavior. Their cache
	// hit/miss pattern is unchanged from before fu54.
	s := &OpenAIGatewayService{cfg: &config.Config{}}
	account := &Account{ID: 100}

	bind := newRound35GinContextWithHeaders(nil)
	s.bindOpenAICompatSessionTurnState(context.Background(), bind, account, "stable_pck", "ts-fallback")

	getCtx := newRound35GinContextWithHeaders(nil)
	state := s.getOpenAICompatSessionTurnState(context.Background(), getCtx, account, "stable_pck")
	require.Equal(t, "ts-fallback", state,
		"fallback path (no session id, use promptCacheKey) must still bind/get correctly")

	// And: when promptCacheKey rolls AND there's no session id, miss is expected
	// (this is the legacy behavior — fixing it requires the metadata.user_id.session_id
	// support not yet shipped).
	missCtx := newRound35GinContextWithHeaders(nil)
	missState := s.getOpenAICompatSessionTurnState(context.Background(), missCtx, account, "different_pck")
	require.Empty(t, missState,
		"without session id, rolling promptCacheKey still misses — fallback path preserves old behavior")
}

func TestRound35_TurnStateKey_StableForEmptyStringSessionIdHeader(t *testing.T) {
	// Whitespace-only or empty session id header must not be honored —
	// fall through to promptCacheKey. Otherwise a misconfigured client
	// that sends `X-Claude-Code-Session-Id: ` (empty) would collapse
	// all its requests onto one shared turn_state.
	account := &Account{ID: 100}
	c := newRound35GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "   ", // whitespace
	})

	key := openAICompatTurnStateKey(c, account, "real_pck")
	require.Contains(t, key, "real_pck",
		"whitespace-only session id must fall back to promptCacheKey")
}

func TestRound35_TurnStateKey_TrimsSessionId(t *testing.T) {
	account := &Account{ID: 100}
	c1 := newRound35GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "  session-padded  ",
	})
	c2 := newRound35GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "session-padded",
	})

	key1 := openAICompatTurnStateKey(c1, account, "k")
	key2 := openAICompatTurnStateKey(c2, account, "k")
	require.Equal(t, key1, key2,
		"trimmed session ids must produce identical keys")
	require.NotContains(t, key1, "  ",
		"leading/trailing whitespace must be stripped from key")
}

// Sanity: the namespace bytes are literally "turnstate" — if a future
// refactor changes this, ops will need to know to flush stale entries.
func TestRound35_TurnStateKey_NamespaceMarkerIsExplicit(t *testing.T) {
	account := &Account{ID: 100}
	c := newRound35GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "s",
	})
	key := openAICompatTurnStateKey(c, account, "")
	parts := strings.Split(key, "\x00")
	require.Len(t, parts, 4, "key is account.ID \\x00 apiKeyID \\x00 'turnstate' \\x00 primary")
	require.Equal(t, "turnstate", parts[2], "third field is the namespace marker")
}
