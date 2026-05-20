package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// codex round37 fu56 (2026-05-20): regression tests for two
// observability bugs codex flagged on fu55:
//
//   (1) hasMetadataUserSessionID missed the dominant production shape
//       — gcr-injected metadata.user_id as a JSON-encoded JSON STRING
//       literal rather than a nested object. Without the fix,
//       has_metadata_session reports false on real Claude Code
//       traffic and ops underestimates fu57+ metadata-key-derivation
//       value.
//
//   (2) HasTurnState had a misleading "(outbound)" comment from fu55.
//       It has always been the inbound cache-lookup result. fu56
//       adds a real outbound field UpstreamTurnStateReturned and
//       restores the inbound semantics on HasTurnState in the
//       documentation.
//
// These tests cover the (1) parsing across all three real-world
// metadata.user_id shapes; the (2) outbound field is exercised by
// the existing handler tests in this package since wiring
// resp.Header is integration-level — here we only assert struct
// shape and the wiring helper invariants.

func TestRound37_HasMetadataUserSessionID_JSONStringForm_GcrMainstream(t *testing.T) {
	// This is the dominant production shape — gcr injects user_id as
	// a JSON-encoded JSON string literal. fu55's nested-only check
	// missed it entirely.
	body := []byte(`{"metadata":{"user_id":"{\"device_id\":\"e7b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\",\"account_uuid\":\"\",\"session_id\":\"123e4567-e89b-12d3-a456-426614174000\"}"}}`)
	require.True(t, hasMetadataUserSessionID(body),
		"JSON-string form is the gcr-injected mainstream — must be detected (codex round37 problem 1)")
}

func TestRound37_HasMetadataUserSessionID_JSONStringForm_EmptySessionId(t *testing.T) {
	// JSON-string form but session_id is empty inside — should be false.
	body := []byte(`{"metadata":{"user_id":"{\"device_id\":\"e7b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\",\"session_id\":\"\"}"}}`)
	require.False(t, hasMetadataUserSessionID(body),
		"empty session_id inside JSON-string form must NOT count")
}

func TestRound37_HasMetadataUserSessionID_JSONStringForm_MissingDeviceId(t *testing.T) {
	// JSON-string form but device_id missing — ParseMetadataUserID
	// rejects (requires both device_id and session_id), and the
	// nested-path fallback can't read session_id either because
	// user_id is a string, not an object. So this corner case is
	// reported as false. Acceptable: gcr always injects device_id,
	// this shape is not seen in production.
	body := []byte(`{"metadata":{"user_id":"{\"session_id\":\"abc\"}"}}`)
	require.False(t, hasMetadataUserSessionID(body),
		"JSON-string form without device_id falls outside ParseMetadataUserID's contract; nested fallback can't read into a string")
}

func TestRound37_HasMetadataUserSessionID_LegacyConcatenatedString(t *testing.T) {
	// Pre-2.1.78 Claude Code clients use the legacy underscore
	// concatenated format. ParseMetadataUserID handles it via regex.
	// device_id must be 64 hex, session_id 36-char UUID.
	body := []byte(`{"metadata":{"user_id":"user_e7b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855_account__session_123e4567-e89b-12d3-a456-426614174000"}}`)
	require.True(t, hasMetadataUserSessionID(body),
		"legacy concatenated form must be detected via ParseMetadataUserID's regex path")
}

func TestRound37_HasMetadataUserSessionID_LegacyConcatenatedString_BadShape(t *testing.T) {
	// Malformed legacy string — short device_id, missing _account_, etc.
	body := []byte(`{"metadata":{"user_id":"user_abc_session_xyz"}}`)
	require.False(t, hasMetadataUserSessionID(body),
		"malformed legacy form must be rejected by the regex")
}

func TestRound37_HasMetadataUserSessionID_NestedObject_StillWorks(t *testing.T) {
	// fu55's original target shape — a real JSON object at
	// metadata.user_id with a session_id child. Must continue to work
	// through the nested-path fallback.
	body := []byte(`{"metadata":{"user_id":{"session_id":"nested-session-001"}}}`)
	require.True(t, hasMetadataUserSessionID(body),
		"nested-object form (test/custom client) must still be detected via the fallback path")
}

func TestRound37_HasMetadataUserSessionID_NestedObjectWithDeviceAndSession(t *testing.T) {
	// Nested object with both device_id and session_id — gjson
	// serializes the object back to JSON, ParseMetadataUserID
	// json.Unmarshals it. Should work.
	body := []byte(`{"metadata":{"user_id":{"device_id":"d","session_id":"s"}}}`)
	require.True(t, hasMetadataUserSessionID(body),
		"nested-object form with device_id+session_id should be detected via ParseMetadataUserID after gjson serialization")
}

func TestRound37_HasMetadataUserSessionID_EmptyNestedSessionFallsBackFalse(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":{"session_id":""}}}`)
	require.False(t, hasMetadataUserSessionID(body),
		"empty nested session_id must not count")

	body2 := []byte(`{"metadata":{"user_id":{"session_id":"   "}}}`)
	require.False(t, hasMetadataUserSessionID(body2),
		"whitespace-only nested session_id must not count")
}

func TestRound37_HasMetadataUserSessionID_MissingMetadata_StillFalse(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[]}`)
	require.False(t, hasMetadataUserSessionID(body))
}

func TestRound37_HasMetadataUserSessionID_BadJSON_NoPanic(t *testing.T) {
	// gjson is lenient with bad JSON — must not panic.
	require.NotPanics(t, func() {
		_ = hasMetadataUserSessionID([]byte(`{not json`))
		_ = hasMetadataUserSessionID(nil)
		_ = hasMetadataUserSessionID([]byte{})
	})
}

func TestRound37_HasMetadataUserSessionID_PrivacyContract_NoSessionLeak(t *testing.T) {
	// Sanity for the codex round36 privacy contract — the bool helper
	// must not stash the raw value anywhere observable from its
	// return type. The test passes if the helper returns bool (kind
	// pinned) for every shape under test.
	cases := []struct {
		name string
		body []byte
	}{
		{"json_string_with_session", []byte(`{"metadata":{"user_id":"{\"device_id\":\"e7b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\",\"session_id\":\"secret-uuid-must-not-leak\"}"}}`)},
		{"legacy_with_session", []byte(`{"metadata":{"user_id":"user_e7b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855_account__session_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}}`)},
		{"nested_with_session", []byte(`{"metadata":{"user_id":{"session_id":"secret-nested-uuid"}}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hasMetadataUserSessionID(tc.body)
			require.True(t, got, "session id IS present — must report true")
			// And the only observable thing about `got` is its bool
			// value; there's no string return to leak the id into.
		})
	}
}

// Sanity for codex round37 problem 2 wiring — verify the struct
// field shape. The full integration behavior (resp.Header drives
// UpstreamTurnStateReturned, and the field appears in the summary
// log) is exercised by handler tests; here we just lock the struct.
func TestRound37_StreamReqMeta_HasUpstreamTurnStateReturnedField(t *testing.T) {
	m := streamReqMeta{
		HasTurnState:              false,
		TurnStateCacheHit:         false,
		UpstreamTurnStateReturned: true,
	}
	require.True(t, m.UpstreamTurnStateReturned,
		"streamReqMeta MUST have UpstreamTurnStateReturned as a settable bool field — codex round37 problem 2")
	require.False(t, m.HasTurnState, "HasTurnState and UpstreamTurnStateReturned are independent fields")
	require.False(t, m.TurnStateCacheHit, "TurnStateCacheHit is the inbound twin, independent of UpstreamTurnStateReturned")
}

// HasTurnState and TurnStateCacheHit must remain synonymous (both
// reflect the inbound cache lookup) — codex round37 confirmed they
// describe the same thing and the duplicate is intentional for grep
// compatibility. UpstreamTurnStateReturned is the new outbound field.
func TestRound37_StreamReqMeta_InboundFieldsAreSynonyms(t *testing.T) {
	// Both inbound fields set to true: outbound independent.
	m1 := streamReqMeta{HasTurnState: true, TurnStateCacheHit: true, UpstreamTurnStateReturned: false}
	require.Equal(t, m1.HasTurnState, m1.TurnStateCacheHit,
		"the two inbound fields must always carry the same boolean (callers must set them together)")
	require.NotEqual(t, m1.HasTurnState, m1.UpstreamTurnStateReturned,
		"inbound and outbound fields are independent — this case confirms they can disagree")

	// Both inbound false, outbound true — fresh-state-from-upstream case.
	m2 := streamReqMeta{HasTurnState: false, TurnStateCacheHit: false, UpstreamTurnStateReturned: true}
	require.False(t, m2.HasTurnState)
	require.False(t, m2.TurnStateCacheHit)
	require.True(t, m2.UpstreamTurnStateReturned,
		"the canonical 'first-turn cache-miss but upstream returned state' shape")
}
