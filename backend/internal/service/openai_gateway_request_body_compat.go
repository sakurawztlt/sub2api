package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func explicitRequestedReasoningEffortFromBody(body []byte) string {
	raw := strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String())
	if raw == "" {
		raw = strings.TrimSpace(gjson.GetBytes(body, "reasoning_effort").String())
	}
	if raw == "" {
		raw = strings.TrimSpace(gjson.GetBytes(body, "output_config.effort").String())
	}
	return raw
}

// CanonicalRequestedReasoningEffort captures client intent before group
// policy and model-family mapping rewrite it. Unknown values return nil.
func CanonicalRequestedReasoningEffort(body []byte, modelCandidates ...string) *string {
	if raw := explicitRequestedReasoningEffortFromBody(body); raw != "" {
		canonical := NormalizeMaxReasoningEffort(raw)
		if canonical == "" {
			return nil
		}
		return &canonical
	}
	for _, model := range modelCandidates {
		if value := canonicalReasoningEffortFromModelSuffix(model); value != "" {
			return &value
		}
	}
	if model := strings.TrimSpace(gjson.GetBytes(body, "model").String()); model != "" {
		if value := canonicalReasoningEffortFromModelSuffix(model); value != "" {
			return &value
		}
	}
	return nil
}

func canonicalReasoningEffortFromModelSuffix(model string) string {
	modelID := strings.TrimSpace(model)
	if modelID == "" {
		return ""
	}
	if strings.Contains(modelID, "/") {
		parts := strings.Split(modelID, "/")
		modelID = parts[len(parts)-1]
	}
	parts := strings.FieldsFunc(strings.ToLower(modelID), func(r rune) bool {
		switch r {
		case '-', '_', ' ':
			return true
		default:
			return false
		}
	})
	if len(parts) == 0 {
		return ""
	}
	return NormalizeMaxReasoningEffort(parts[len(parts)-1])
}

type ErrInvalidOpenAIServiceTier struct {
	Value string
}

func (e *ErrInvalidOpenAIServiceTier) Error() string {
	return fmt.Sprintf("invalid service_tier %q: must be one of auto, default, fast, flex, priority, scale, ultrafast", e.Value)
}

const invalidOpenAIServiceTierValueMaxLen = 64

func boundInvalidOpenAIServiceTierValue(raw string) string {
	if len(raw) <= invalidOpenAIServiceTierValueMaxLen {
		return raw
	}
	return raw[:invalidOpenAIServiceTierValueMaxLen] + "..."
}

// ValidateOpenAIServiceTierField validates the service_tier field of a raw
// OpenAI-compatible request body (/v1/responses and /v1/chat/completions).
//
//   - absent / null → valid, returns "" (field omitted keeps current behavior)
//   - "fast" → normalized to "priority" (the two are equivalent; the canonical
//     value is what reaches the OpenAI upstream)
//   - "priority" / "flex" / "auto" / "default" / "scale" / "ultrafast" → valid, returned as-is
//   - an explicitly present non-string value, an empty string, or any other
//     unknown value → *ErrInvalidOpenAIServiceTier (handler maps to HTTP 400),
//     matching OpenAI's enum validation semantics
func ValidateOpenAIServiceTierField(body []byte) (string, error) {
	tierResult := gjson.GetBytes(body, "service_tier")
	if !tierResult.Exists() || tierResult.Type == gjson.Null {
		return "", nil
	}
	if tierResult.Type != gjson.String {
		return "", &ErrInvalidOpenAIServiceTier{Value: "<non-string>"}
	}
	raw := strings.TrimSpace(tierResult.String())
	if raw == "" {
		return "", &ErrInvalidOpenAIServiceTier{Value: raw}
	}
	normalized := normalizedOpenAIServiceTierValue(raw)
	if normalized == "" {
		return "", &ErrInvalidOpenAIServiceTier{Value: boundInvalidOpenAIServiceTierValue(raw)}
	}
	return normalized, nil
}

func shouldPreserveOpenAIResponsesNoneReasoningEffort(account *Account) bool {
	if account == nil {
		return false
	}
	if account.IsOpenAIPassthroughEnabled() {
		return true
	}
	if account.IsOpenAIOAuthLike() {
		return true
	}
	if !account.IsOpenAIApiKey() {
		return false
	}
	baseURL := strings.TrimSpace(account.GetCredential("base_url"))
	return baseURL == "" || isOfficialOpenAIModelsBaseURL(baseURL)
}

func filterOpenAIResponsesNoneReasoningEffortForAccount(account *Account, body []byte) ([]byte, error) {
	if len(body) == 0 || shouldPreserveOpenAIResponsesNoneReasoningEffort(account) {
		return body, nil
	}

	out := body
	for _, path := range []string{"reasoning.effort", "reasoning_effort"} {
		effort := gjson.GetBytes(out, path)
		if effort.Type != gjson.String || !strings.EqualFold(strings.TrimSpace(effort.String()), "none") {
			continue
		}
		next, err := sjson.DeleteBytes(out, path)
		if err != nil {
			return body, fmt.Errorf("strip %s none placeholder: %w", path, err)
		}
		out = next
	}
	if reasoning := gjson.GetBytes(out, "reasoning"); reasoning.IsObject() && len(reasoning.Map()) == 0 {
		next, err := sjson.DeleteBytes(out, "reasoning")
		if err != nil {
			return body, fmt.Errorf("strip empty reasoning object: %w", err)
		}
		out = next
	}
	return out, nil
}

func deleteOpenAIResponsesNoneReasoningEffortFromObject(account *Account, body map[string]any) {
	if body == nil || shouldPreserveOpenAIResponsesNoneReasoningEffort(account) {
		return
	}
	if effort, ok := body["reasoning_effort"].(string); ok && strings.EqualFold(strings.TrimSpace(effort), "none") {
		delete(body, "reasoning_effort")
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		return
	}
	if effort, ok := reasoning["effort"].(string); ok && strings.EqualFold(strings.TrimSpace(effort), "none") {
		delete(reasoning, "effort")
	}
	if len(reasoning) == 0 {
		delete(body, "reasoning")
	}
}
