package service

import (
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// codex round36 fu55 (2026-05-20): observability fields for the fu54
// turn_state cache. These tests verify the helpers feeding the
// large_context_request summary log fields:
//   - turn_state_key_source
//   - turn_state_hit (derived from getOpenAICompatSessionTurnState result)
//   - has_session_header
//   - has_metadata_session
//
// Constraint from codex: NEVER log the raw session id. The helpers
// must only return enums / booleans. This test file pins both the
// behavior (correct enum value) and the privacy property (no raw
// values escape the helpers).

func newRound36GinContextWithHeaders(headers map[string]string) *gin.Context {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c.Request = req
	return c
}

func TestRound36_TurnStateKeySource_SessionHeaderWins(t *testing.T) {
	c := newRound36GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "secret-session-must-not-leak",
	})
	got := describeTurnStateKeySource(c, "some-rolling-pck")
	require.Equal(t, turnStateKeySourceSessionHeader, got)
	require.Equal(t, "session_header", got, "log enum literal pinned for downstream grep templates")
}

func TestRound36_TurnStateKeySource_FallsBackToPromptCacheKey(t *testing.T) {
	c := newRound36GinContextWithHeaders(nil)
	got := describeTurnStateKeySource(c, "rolling-pck-abc")
	require.Equal(t, turnStateKeySourcePromptCacheKey, got)
	require.Equal(t, "prompt_cache_key", got)
}

func TestRound36_TurnStateKeySource_NoneWhenAllAbsent(t *testing.T) {
	c := newRound36GinContextWithHeaders(nil)
	got := describeTurnStateKeySource(c, "")
	require.Equal(t, turnStateKeySourceNone, got)
	require.Equal(t, "none", got)
}

func TestRound36_TurnStateKeySource_NilContextFallsThrough(t *testing.T) {
	got := describeTurnStateKeySource(nil, "rolling-pck")
	require.Equal(t, turnStateKeySourcePromptCacheKey, got)
}

func TestRound36_TurnStateKeySource_WhitespaceHeaderFallsThrough(t *testing.T) {
	c := newRound36GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "   ",
	})
	got := describeTurnStateKeySource(c, "real-pck")
	require.Equal(t, turnStateKeySourcePromptCacheKey, got,
		"whitespace-only session header must NOT count — must fall to promptCacheKey")
}

func TestRound36_TurnStateKeySource_MirrorsKeyDerivation(t *testing.T) {
	// Critical invariant: describeTurnStateKeySource MUST stay in
	// lock-step with openAICompatTurnStateKey. If the source says
	// "session_header" then the key must contain the session id
	// (and NOT contain the promptCacheKey).
	account := &Account{ID: 99}

	c := newRound36GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "session-xyz",
	})
	source := describeTurnStateKeySource(c, "rolling-pck")
	key := openAICompatTurnStateKey(c, account, "rolling-pck")

	require.Equal(t, "session_header", source)
	require.Contains(t, key, "session-xyz",
		"source=session_header MUST mean the key carries the session id")
	require.NotContains(t, key, "rolling-pck",
		"source=session_header MUST mean promptCacheKey is NOT in the key")
}

func TestRound36_HasMetadataUserSessionID_Present(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":{"session_id":"abc-123"}}}`)
	require.True(t, hasMetadataUserSessionID(body))
}

func TestRound36_HasMetadataUserSessionID_EmptyString(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":{"session_id":""}}}`)
	require.False(t, hasMetadataUserSessionID(body),
		"empty session_id must count as absent")
}

func TestRound36_HasMetadataUserSessionID_WhitespaceOnly(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":{"session_id":"   "}}}`)
	require.False(t, hasMetadataUserSessionID(body),
		"whitespace-only session_id must count as absent")
}

func TestRound36_HasMetadataUserSessionID_MissingSessionField(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":{"other_field":"x"}}}`)
	require.False(t, hasMetadataUserSessionID(body))
}

func TestRound36_HasMetadataUserSessionID_MissingUserIdField(t *testing.T) {
	body := []byte(`{"metadata":{"other":"x"}}`)
	require.False(t, hasMetadataUserSessionID(body))
}

func TestRound36_HasMetadataUserSessionID_MissingMetadata(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[]}`)
	require.False(t, hasMetadataUserSessionID(body))
}

func TestRound36_HasMetadataUserSessionID_NilBody(t *testing.T) {
	require.False(t, hasMetadataUserSessionID(nil))
	require.False(t, hasMetadataUserSessionID([]byte{}))
}

func TestRound36_HasMetadataUserSessionID_BadJSON(t *testing.T) {
	// gjson tolerates broken JSON by returning empty results — should
	// not panic, should return false.
	body := []byte(`{not json`)
	require.False(t, hasMetadataUserSessionID(body))
}

func TestRound36_HelpersReturnEnumAndBoolOnly_NeverSessionId(t *testing.T) {
	// Pin the privacy contract: the return types of the observability
	// helpers must be string-enum / bool, never the raw session id.
	// If a future refactor changes a signature to leak the id, this
	// test will trip on the reflect.Kind check.
	sourceType := reflect.TypeOf(describeTurnStateKeySource).Out(0)
	require.Equal(t, reflect.String, sourceType.Kind(),
		"source helper must return string (an enum value)")

	hasType := reflect.TypeOf(hasMetadataUserSessionID).Out(0)
	require.Equal(t, reflect.Bool, hasType.Kind(),
		"metadata helper must return bool")

	// Also verify the source helper actually returns one of the four
	// reserved enum values for arbitrary input — no raw passthrough.
	c := newRound36GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "raw-secret-id-12345",
	})
	got := describeTurnStateKeySource(c, "raw-pck-67890")
	validEnums := map[string]bool{
		turnStateKeySourceNone:            true,
		turnStateKeySourceSessionHeader:   true,
		turnStateKeySourceMetadataSession: true,
		turnStateKeySourcePromptCacheKey:  true,
	}
	require.True(t, validEnums[got],
		"helper returned %q which is NOT in the reserved enum set — privacy contract broken", got)
	require.NotContains(t, got, "raw-secret-id-12345",
		"helper return must NEVER carry the raw session id")
	require.NotContains(t, got, "raw-pck-67890",
		"helper return must NEVER carry the raw promptCacheKey")
}

func TestRound36_EnumConstants_AreStable(t *testing.T) {
	// Downstream log greppers will template against these literals.
	// Renaming any of them is a breaking change — pin the strings.
	require.Equal(t, "none", turnStateKeySourceNone)
	require.Equal(t, "session_header", turnStateKeySourceSessionHeader)
	require.Equal(t, "metadata_session", turnStateKeySourceMetadataSession)
	require.Equal(t, "prompt_cache_key", turnStateKeySourcePromptCacheKey)
}

func TestRound36_MetadataSessionEnum_NotYetReachable(t *testing.T) {
	// fu55 invariant: describeTurnStateKeySource MUST NOT return
	// metadata_session yet, even when has_metadata_session is true.
	// The has_metadata_session bool is the observability hook;
	// promoting metadata to a real key source is fu56+ work.
	c := newRound36GinContextWithHeaders(nil) // no session header
	body := []byte(`{"metadata":{"user_id":{"session_id":"meta-only"}}}`)

	source := describeTurnStateKeySource(c, "pck")
	require.NotEqual(t, turnStateKeySourceMetadataSession, source,
		"fu55 must not yet derive source from metadata.user_id.session_id")
	require.Equal(t, turnStateKeySourcePromptCacheKey, source,
		"with no header, must fall back to promptCacheKey")
	require.True(t, hasMetadataUserSessionID(body),
		"but the body-level signal IS detected — that's how fu56+ will decide whether to promote")
}

func TestRound36_TurnStateKeySource_TrimmedHeaderRecognized(t *testing.T) {
	// Sanity: trimming on the helper side mirrors the trimming on the
	// key-derivation side; "  s  " is treated the same as "s".
	c1 := newRound36GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "  session-s  ",
	})
	c2 := newRound36GinContextWithHeaders(map[string]string{
		"X-Claude-Code-Session-Id": "session-s",
	})
	require.Equal(t,
		describeTurnStateKeySource(c1, "pck"),
		describeTurnStateKeySource(c2, "pck"),
		"trimmed and untrimmed headers must yield the same source")
}

// Sanity that the enum strings don't accidentally collide with the
// namespace marker baked into the cache key. If they ever did,
// log searches that filter by source could be confused by entries
// that just happen to contain "turnstate" elsewhere.
func TestRound36_EnumStringsDoNotCollideWithKeyNamespace(t *testing.T) {
	for _, enum := range []string{
		turnStateKeySourceNone,
		turnStateKeySourceSessionHeader,
		turnStateKeySourceMetadataSession,
		turnStateKeySourcePromptCacheKey,
	} {
		require.NotEqual(t, "turnstate", enum,
			"source enum %q must not collide with the cache-key namespace marker", enum)
		require.False(t, strings.Contains(enum, "\x00"),
			"source enum %q must not contain the cache-key field separator", enum)
	}
}
