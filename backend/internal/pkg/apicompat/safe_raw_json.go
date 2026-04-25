package apicompat

import (
	"encoding/json"
	"strings"
)

// safeRawJSON wraps a tool/function arguments string into a json.RawMessage
// that is guaranteed to be valid JSON. The OpenAI Responses API delivers
// function_call.arguments as a string; if the caller sent a tool with no
// args, the string is empty ("") — and direct json.RawMessage("") yields
// invalid raw JSON that crashes downstream encoder
// ("unexpected end of JSON input"), surfacing as a 500 with broken body
// to the client (codex 2026-04-25 NewAPI channel-7 report).
//
// Behaviour:
//   - empty / whitespace-only → {}
//   - non-parseable JSON      → {}
//   - any valid JSON          → original bytes pass through unchanged
//
// The {} fallback is the Anthropic-native shape for "tool was called
// with no parameters" — clients can't distinguish it from a real
// no-args tool call. No sub2api fingerprint leaked.
func safeRawJSON(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{}`)
	}
	if !json.Valid([]byte(s)) {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(s)
}
