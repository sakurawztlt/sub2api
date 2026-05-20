package service

import (
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// stripSamplingParamsForReasoningModelBody removes top-level
// temperature and top_p from a JSON request body when the target model
// is a gpt-5.x reasoning model. Returns (modifiedBody, anyStripped,
// error). The caller is responsible for swapping the returned slice
// back into its own variable on success.
//
// codex round43 fu61 (2026-05-20): consolidates three earlier ad-hoc
// strips into one place so future paths can join with a single call
// and stay consistent. The previous fu59/fu60 versions repeated the
// same `IsReasoningModel + sjson.DeleteBytes("temperature") +
// sjson.DeleteBytes("top_p")` pattern in three call sites, and a
// fourth path (forwardOpenAIPassthrough) was missed entirely.
//
// Why DeleteBytes (not SetBytes-to-null): upstream returns 400
// "Unsupported parameter: temperature" if the field is present AT
// ALL, even with a JSON null value. The previous fu60 native path
// used markPatchSet("temperature", nil) which on a SINGLE-field
// request would have written "temperature": null through the
// fast-patch path and the upstream would still reject. fu61 native
// path moved to markPatchDelete; this helper goes one level deeper
// and operates directly on the JSON bytes, so any caller that has
// a byte body in hand (Cursor Responses-shape branch, OpenAI
// passthrough) can call it without participating in the patch
// bookkeeping.
//
// Behaviour:
//   - non-reasoning model: returns (body, false, nil) unchanged
//   - reasoning model, neither field present: returns (body, false, nil)
//   - reasoning model, at least one field present: returns the body
//     with those fields removed and modified=true
//   - sjson error on either delete: returns the body as far as it
//     got plus an error
func stripSamplingParamsForReasoningModelBody(model string, body []byte) ([]byte, bool, error) {
	if !apicompat.IsReasoningModel(model) {
		return body, false, nil
	}
	modified := false
	for _, field := range []string{"temperature", "top_p"} {
		if !gjson.GetBytes(body, field).Exists() {
			continue
		}
		stripped, err := sjson.DeleteBytes(body, field)
		if err != nil {
			return body, modified, fmt.Errorf("strip %s for reasoning model %q: %w", field, model, err)
		}
		body = stripped
		modified = true
	}
	return body, modified, nil
}
