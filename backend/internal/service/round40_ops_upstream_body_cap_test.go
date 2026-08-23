package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// codex round40 fu58 (2026-05-20) / upstream PR #2581 intent: cap the
// upstream request body captured for ops retry replay. Bodies over
// 4 MiB are replaced with a JSON marker envelope (size + sha256_short +
// base64-encoded head/tail preview) so gin context, ops_error_logs DB
// columns, and retry replay buffers don't have to carry pathological
// 39 MiB payloads.
//
// These tests pin:
//   - cap boundary behavior (at cap = stored verbatim; over cap = marker)
//   - marker envelope shape (all required fields present, JSON-valid)
//   - hash stability (same body always yields same hash)
//   - hash divergence for different bodies
//   - preview head and tail extraction correctness
//   - end-to-end readback through appendOpsUpstreamError so ops_error_logs
//     receives the marker JSON, not the raw 4MB+ bytes
//   - traffic_capture path is untouched (only the ops_upstream body
//     capture is capped; codex constraint "保留 traffic_capture")

func newRound40GinContext() *gin.Context {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	return c
}

func TestRound40_SetOpsUpstreamRequestBody_SmallBodyStoredVerbatim(t *testing.T) {
	c := newRound40GinContext()
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	setOpsUpstreamRequestBody(c, body)

	v, ok := c.Get(OpsUpstreamRequestBodyKey)
	require.True(t, ok)
	raw, ok := v.([]byte)
	require.True(t, ok, "small body must remain []byte (no marker envelope)")
	require.True(t, bytes.Equal(raw, body), "small body must round-trip unchanged")
}

func TestRound40_SetOpsUpstreamRequestBody_AtCapStoredVerbatim(t *testing.T) {
	// At exactly the cap: still verbatim (boundary is inclusive).
	c := newRound40GinContext()
	body := bytes.Repeat([]byte("a"), opsUpstreamRequestBodyCapBytes)
	require.Equal(t, opsUpstreamRequestBodyCapBytes, len(body))

	setOpsUpstreamRequestBody(c, body)
	v, ok := c.Get(OpsUpstreamRequestBodyKey)
	require.True(t, ok)
	raw, ok := v.([]byte)
	require.True(t, ok, "body == cap must remain []byte (inclusive boundary)")
	require.Equal(t, opsUpstreamRequestBodyCapBytes, len(raw))
}

func TestRound40_SetOpsUpstreamRequestBody_OverCapReplacedWithMarker(t *testing.T) {
	c := newRound40GinContext()
	// 1 byte over cap → must trigger truncation.
	body := bytes.Repeat([]byte("X"), opsUpstreamRequestBodyCapBytes+1)
	setOpsUpstreamRequestBody(c, body)

	v, ok := c.Get(OpsUpstreamRequestBodyKey)
	require.True(t, ok)
	marker, ok := v.(string)
	require.True(t, ok, "over-cap body must be replaced with a string marker envelope")
	// Marker envelope MUST be much smaller than the original body.
	require.Less(t, len(marker), opsUpstreamRequestBodyCapBytes/100,
		"marker envelope must be far smaller than the cap (memory protection requirement)")
	require.True(t, strings.HasPrefix(marker, `{"_truncated":true`),
		"marker envelope must start with the recognizable `_truncated` field so ops grep can identify capped rows")
}

func TestRound40_SetOpsUpstreamRequestBody_TrafficCaptureKeepsFullBodyWhenEnabled(t *testing.T) {
	t.Setenv("SUB2API_TRAFFIC_CAPTURE_ENABLED", "true")
	t.Setenv("SUB2API_TRAFFIC_CAPTURE_MAX_BYTES", "8388608")

	c := newRound40GinContext()
	body := bytes.Repeat([]byte("C"), opsUpstreamRequestBodyCapBytes+1)
	setOpsUpstreamRequestBody(c, body)

	opsValue, ok := c.Get(OpsUpstreamRequestBodyKey)
	require.True(t, ok)
	_, opsIsMarker := opsValue.(string)
	require.True(t, opsIsMarker, "ops path must keep 4 MiB marker behavior")

	captureValue, ok := c.Get(TrafficCaptureUpstreamRequestBodyKey)
	require.True(t, ok, "traffic_capture should get an independent full-body key when enabled")
	captureBody, ok := captureValue.([]byte)
	require.True(t, ok, "traffic_capture body under env cap must stay verbatim []byte")
	require.Equal(t, body, captureBody)
}

func TestRound40_SetOpsUpstreamRequestBody_TrafficCaptureUsesMarkerOverEnvCap(t *testing.T) {
	t.Setenv("SUB2API_TRAFFIC_CAPTURE_ENABLED", "1")
	t.Setenv("SUB2API_TRAFFIC_CAPTURE_MAX_BYTES", "1024")

	c := newRound40GinContext()
	body := bytes.Repeat([]byte("C"), opsUpstreamRequestBodyCapBytes+1)
	setOpsUpstreamRequestBody(c, body)

	captureValue, ok := c.Get(TrafficCaptureUpstreamRequestBodyKey)
	require.True(t, ok)
	marker, ok := captureValue.(string)
	require.True(t, ok, "traffic_capture body above env cap must use marker")
	require.Contains(t, marker, `"_capture_cap_bytes":1024`)
}

func TestRound40_TruncationMarker_EnvelopeShape(t *testing.T) {
	// Construct a deterministic over-cap body so we can compute the
	// expected hash and verify each field of the envelope.
	body := make([]byte, opsUpstreamRequestBodyCapBytes+4096)
	for i := range body {
		body[i] = byte(i % 251) // varied bytes — not just zeros
	}
	marker := buildOpsUpstreamRequestBodyTruncationMarker(body)

	var env struct {
		Truncated   bool   `json:"_truncated"`
		SizeBytes   int    `json:"_size_bytes"`
		Sha256Short string `json:"_sha256_short"`
		PreviewHead string `json:"_preview_head_b64"`
		PreviewTail string `json:"_preview_tail_b64"`
	}
	require.NoError(t, json.Unmarshal([]byte(marker), &env), "marker must be valid JSON")
	require.True(t, env.Truncated)
	require.Equal(t, len(body), env.SizeBytes)
	require.Len(t, env.Sha256Short, 16, "sha256_short must be exactly 16 hex chars (64-bit truncation)")

	// Verify the hash field is the real sha256 short hash.
	want := sha256.Sum256(body)
	require.Equal(t, hex.EncodeToString(want[:])[:16], env.Sha256Short)

	// Verify preview head decodes to the first opsUpstreamRequestBodyPreviewBytes.
	headBytes, err := base64.StdEncoding.DecodeString(env.PreviewHead)
	require.NoError(t, err)
	require.Equal(t, opsUpstreamRequestBodyPreviewBytes, len(headBytes))
	require.True(t, bytes.Equal(headBytes, body[:opsUpstreamRequestBodyPreviewBytes]),
		"preview_head must be body[:N]")

	// Verify preview tail decodes to the last opsUpstreamRequestBodyPreviewBytes.
	tailBytes, err := base64.StdEncoding.DecodeString(env.PreviewTail)
	require.NoError(t, err)
	require.Equal(t, opsUpstreamRequestBodyPreviewBytes, len(tailBytes))
	require.True(t, bytes.Equal(tailBytes, body[len(body)-opsUpstreamRequestBodyPreviewBytes:]),
		"preview_tail must be body[len-N:]")
}

func TestRound40_TruncationMarker_HashStableAcrossCalls(t *testing.T) {
	body := bytes.Repeat([]byte{0xAB, 0xCD}, opsUpstreamRequestBodyCapBytes)

	m1 := buildOpsUpstreamRequestBodyTruncationMarker(body)
	m2 := buildOpsUpstreamRequestBodyTruncationMarker(body)
	require.Equal(t, m1, m2, "same body MUST yield byte-identical marker (deterministic hash)")
}

func TestRound40_TruncationMarker_HashDiffersForDifferentBodies(t *testing.T) {
	body1 := bytes.Repeat([]byte{0xAA}, opsUpstreamRequestBodyCapBytes+1)
	body2 := bytes.Repeat([]byte{0xBB}, opsUpstreamRequestBodyCapBytes+1)

	var e1, e2 struct {
		Sha256Short string `json:"_sha256_short"`
	}
	require.NoError(t, json.Unmarshal([]byte(buildOpsUpstreamRequestBodyTruncationMarker(body1)), &e1))
	require.NoError(t, json.Unmarshal([]byte(buildOpsUpstreamRequestBodyTruncationMarker(body2)), &e2))
	require.NotEqual(t, e1.Sha256Short, e2.Sha256Short,
		"different bodies MUST have different short hashes (regression guard)")
}

// End-to-end: setOpsUpstreamRequestBody → appendOpsUpstreamError →
// the resulting OpsUpstreamErrorEvent.UpstreamRequestBody string is
// the marker envelope, NOT 4MB+ of bytes.
func TestRound40_AppendOpsUpstreamError_CarriesTruncationMarker(t *testing.T) {
	c := newRound40GinContext()
	body := bytes.Repeat([]byte("Z"), opsUpstreamRequestBodyCapBytes+512)
	setOpsUpstreamRequestBody(c, body)

	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		UpstreamStatusCode: 500,
		Kind:               "http_error",
		Message:            "upstream 500",
	})

	v, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 1)

	ub := events[0].UpstreamRequestBody
	require.NotEmpty(t, ub)
	require.Less(t, len(ub), 100*1024, "ops_error_logs row must NOT carry 4MB+ verbatim — must be capped marker")
	require.True(t, strings.HasPrefix(ub, `{"_truncated":true`),
		"ops_error_logs row must carry the truncation marker, not the raw body")

	// Verify the marker is still parseable and round-trips the body size.
	var env struct {
		Truncated bool `json:"_truncated"`
		SizeBytes int  `json:"_size_bytes"`
	}
	require.NoError(t, json.Unmarshal([]byte(ub), &env))
	require.True(t, env.Truncated)
	require.Equal(t, len(body), env.SizeBytes,
		"marker must record the original body size so ops can identify the bucket")
}

// Negative: small bodies must NOT be wrapped in a marker — preserves
// existing ops retry behavior for non-pathological traffic.
func TestRound40_AppendOpsUpstreamError_SmallBodyNotWrapped(t *testing.T) {
	c := newRound40GinContext()
	body := []byte(`{"model":"gpt-5.2","messages":[]}`)
	setOpsUpstreamRequestBody(c, body)

	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		UpstreamStatusCode: 400,
		Kind:               "http_error",
	})

	v, exists := c.Get(OpsUpstreamErrorsKey)
	require.True(t, exists)
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Equal(t, string(body), events[0].UpstreamRequestBody,
		"small body must reach ops_error_logs verbatim — no envelope wrapping")
}

// Sanity for codex constraint "保留 traffic_capture": the traffic_capture
// context keys are independent from OpsUpstreamRequestBodyKey. fu58 only
// caps the latter; traffic_capture's outbound headers/metadata are not
// passed through this helper at all.
func TestRound40_TrafficCaptureKeysIndependent(t *testing.T) {
	c := newRound40GinContext()
	body := bytes.Repeat([]byte("L"), opsUpstreamRequestBodyCapBytes+1)
	setOpsUpstreamRequestBody(c, body)

	// Confirm ops body was capped (marker string in slot).
	v, _ := c.Get(OpsUpstreamRequestBodyKey)
	_, isString := v.(string)
	require.True(t, isString, "fu58: ops body slot must be marker string for over-cap input")

	// traffic_capture slots untouched by setOpsUpstreamRequestBody.
	for _, key := range []string{
		trafficCaptureAccountIDKey,
		trafficCaptureGroupIDKey,
		trafficCapturePlatformKey,
		trafficCaptureAccountTypeKey,
		trafficCaptureModelKey,
		trafficCaptureUpstreamReqIDKey,
		trafficCaptureOutboundHeadersKey,
	} {
		_, present := c.Get(key)
		require.False(t, present, "traffic_capture key %q must not be set by setOpsUpstreamRequestBody", key)
	}
}

// Sanity: cap and preview constants chosen such that the marker envelope
// is bounded. Pin the constants to detect accidental reductions that
// could reintroduce memory pressure or, conversely, accidental
// inflation that loses the memory-protection property.
func TestRound40_CapConstants_Reasonable(t *testing.T) {
	require.Equal(t, 4*1024*1024, opsUpstreamRequestBodyCapBytes,
		"cap pinned at 4 MiB — see codex round40 reasoning")
	require.Equal(t, 8*1024, opsUpstreamRequestBodyPreviewBytes,
		"preview head/tail size pinned at 8 KiB each (16 KiB total preview)")
	require.Less(t, 2*opsUpstreamRequestBodyPreviewBytes, opsUpstreamRequestBodyCapBytes,
		"head + tail preview must fit inside the cap by a wide margin")
}
