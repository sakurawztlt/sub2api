package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func normalizeOpenAIOAuthResponsesCompatibilityFields(reqBody map[string]any) bool {
	if reqBody == nil {
		return false
	}
	changed := false
	if prompt, exists := reqBody["prompt"]; exists {
		if input, hasInput := reqBody["input"]; !hasInput || input == nil {
			if prompt != nil {
				reqBody["input"] = prompt
			}
		}
		delete(reqBody, "prompt")
		changed = true
	}
	if _, exists := reqBody["commands"]; exists {
		delete(reqBody, "commands")
		changed = true
	}
	return changed
}

func normalizeOpenAIParallelToolCallsWithoutTools(body []byte, responsesLiteOpt ...bool) ([]byte, bool, error) {
	responsesLite := len(responsesLiteOpt) > 0 && responsesLiteOpt[0]
	if responsesLite {
		return body, false, nil
	}
	parallel := gjson.GetBytes(body, "parallel_tool_calls")
	if !parallel.Exists() {
		return body, false, nil
	}
	if openAIRequestBodyHasTools(body) {
		return body, false, nil
	}
	normalized, err := sjson.DeleteBytes(body, "parallel_tool_calls")
	if err != nil {
		return body, false, fmt.Errorf("normalize parallel_tool_calls without tools: %w", err)
	}
	return normalized, true, nil
}

// openAIRequestBodyHasTools recognizes both top-level tools and the Responses
// Lite input.additional_tools carrier.
func openAIRequestBodyHasTools(body []byte) bool {
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() && len(tools.Array()) > 0 {
		return true
	}
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
			continue
		}
		if tools := item.Get("tools"); tools.IsArray() && len(tools.Array()) > 0 {
			return true
		}
	}
	return false
}

// normalizeOpenAIResponsesReasoningContentReplay removes non-portable visible
// reasoning.content while retaining the portable reasoning item metadata.
func normalizeOpenAIResponsesReasoningContentReplay(body []byte) ([]byte, bool, error) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false, nil
	}

	needsNormalization := false
	input.ForEach(func(_, item gjson.Result) bool {
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			return true
		}
		content := item.Get("content")
		if content.IsArray() && len(content.Array()) > 0 {
			needsNormalization = true
			return false
		}
		return true
	})
	if !needsNormalization {
		return body, false, nil
	}

	var reqBody map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &reqBody); err != nil {
		return body, false, fmt.Errorf("normalize OpenAI reasoning content replay: %w", err)
	}
	items, ok := reqBody["input"].([]any)
	if !ok {
		return body, false, nil
	}
	changed := false
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyString(item["type"])) != "reasoning" {
			continue
		}
		content, ok := item["content"].([]any)
		if !ok || len(content) == 0 {
			continue
		}
		delete(item, "content")
		changed = true
	}
	if !changed {
		return body, false, nil
	}
	normalized, err := marshalOpenAIUpstreamJSON(reqBody)
	if err != nil {
		return body, false, fmt.Errorf("serialize normalized OpenAI reasoning content replay: %w", err)
	}
	return normalized, true, nil
}

func normalizeOpenAICodexCompactReasoningEffortForAccount(c *gin.Context, account *Account, body []byte) ([]byte, bool, error) {
	if account == nil || !account.IsOpenAIOAuthLike() || !isOpenAIResponsesCompactPath(c) {
		return body, false, nil
	}
	requestedModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	effectiveModel := account.GetMappedModel(requestedModel)
	return normalizeOpenAICodexCompactReasoningEffort(body, effectiveModel)
}

func normalizeOpenAICodexCompactReasoningEffort(body []byte, effectiveModel string) ([]byte, bool, error) {
	if !isOpenAIGPT56Model(effectiveModel) ||
		!strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String()), "max") {
		return body, false, nil
	}
	normalized, err := sjson.SetBytes(body, "reasoning.effort", "xhigh")
	if err != nil {
		return body, false, fmt.Errorf("normalize codex compact reasoning effort: %w", err)
	}
	return normalized, true, nil
}

func normalizeOpenAIAPIKeyStoreFalseReasoningReplay(body []byte, knownStoreFalse bool) ([]byte, bool, error) {
	if !knownStoreFalse && gjson.GetBytes(body, "store").Type != gjson.False {
		return body, false, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false, nil
	}

	var reqBody map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &reqBody); err != nil {
		return body, false, fmt.Errorf("normalize API-key store=false reasoning replay: %w", err)
	}
	items, ok := reqBody["input"].([]any)
	if !ok {
		return body, false, nil
	}
	filtered := make([]any, 0, len(items))
	changed := false
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			filtered = append(filtered, rawItem)
			continue
		}
		typ := strings.TrimSpace(firstNonEmptyString(item["type"]))
		id := strings.TrimSpace(firstNonEmptyString(item["id"]))
		switch typ {
		case "reasoning":
			encryptedContent, hasEncryptedContent := item["encrypted_content"].(string)
			if !hasEncryptedContent || strings.TrimSpace(encryptedContent) == "" {
				changed = true
				continue
			}
			if strings.HasPrefix(id, "rs_") {
				delete(item, "id")
				changed = true
			}
			if summary, ok := item["summary"]; !ok || summary == nil {
				item["summary"] = []any{}
				changed = true
			}
		case "item_reference":
			if strings.HasPrefix(id, "rs_") {
				changed = true
				continue
			}
		}
		if shouldStripOpenAIResponsesNonPairCallID(typ) {
			if _, hasCallID := item["call_id"]; hasCallID {
				delete(item, "call_id")
				changed = true
			}
		}
		filtered = append(filtered, item)
	}
	if !changed {
		return body, false, nil
	}
	reqBody["input"] = filtered
	normalized, err := marshalOpenAIUpstreamJSON(reqBody)
	if err != nil {
		return body, false, fmt.Errorf("serialize API-key store=false reasoning replay: %w", err)
	}
	return normalized, true, nil
}

func normalizeOpenAIOAuthResponsesCompatibilityBody(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	normalized := body
	changed := false
	prompt := gjson.GetBytes(normalized, "prompt")
	if prompt.Exists() {
		input := gjson.GetBytes(normalized, "input")
		if prompt.Type != gjson.Null && (!input.Exists() || input.Type == gjson.Null) {
			next, err := sjson.SetRawBytes(normalized, "input", []byte(prompt.Raw))
			if err != nil {
				return body, false, fmt.Errorf("normalize oauth responses prompt: %w", err)
			}
			normalized = next
		}
		next, err := sjson.DeleteBytes(normalized, "prompt")
		if err != nil {
			return body, false, fmt.Errorf("normalize oauth responses delete prompt: %w", err)
		}
		normalized = next
		changed = true
	}
	if gjson.GetBytes(normalized, "commands").Exists() {
		next, err := sjson.DeleteBytes(normalized, "commands")
		if err != nil {
			return body, false, fmt.Errorf("normalize oauth responses delete commands: %w", err)
		}
		normalized = next
		changed = true
	}
	return normalized, changed, nil
}

func normalizeOpenAIResponsesReasoningMode(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	mode := gjson.GetBytes(body, "reasoning.mode")
	if !mode.Exists() || mode.Type != gjson.String {
		return body, false, nil
	}
	updated := body
	effort := gjson.GetBytes(body, "reasoning.effort")
	if (!effort.Exists() || effort.Type == gjson.Null || strings.TrimSpace(effort.String()) == "") &&
		strings.EqualFold(strings.TrimSpace(mode.String()), "pro") {
		var err error
		updated, err = sjson.SetBytes(updated, "reasoning.effort", "max")
		if err != nil {
			return body, false, fmt.Errorf("set reasoning effort for mode=pro: %w", err)
		}
	}
	updated, err := sjson.DeleteBytes(updated, "reasoning.mode")
	if err != nil {
		return body, false, fmt.Errorf("delete unsupported reasoning.mode: %w", err)
	}
	if reasoning := gjson.GetBytes(updated, "reasoning"); reasoning.Exists() && reasoning.IsObject() && len(reasoning.Map()) == 0 {
		updated, err = sjson.DeleteBytes(updated, "reasoning")
		if err != nil {
			return body, false, fmt.Errorf("delete empty reasoning object: %w", err)
		}
	}
	return updated, true, nil
}

func normalizeOpenAIResponseFormatSchemasBody(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	textFormat := strings.TrimSpace(gjson.GetBytes(body, "text.format.type").String())
	responseFormat := strings.TrimSpace(gjson.GetBytes(body, "response_format.type").String())
	if textFormat != "json_schema" && responseFormat != "json_schema" {
		return body, false, nil
	}
	var reqBody map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &reqBody); err != nil {
		return body, false, fmt.Errorf("normalize responses schema body: %w", err)
	}
	if !normalizeOpenAIResponseFormatSchemas(reqBody) {
		return body, false, nil
	}
	normalized, err := json.Marshal(reqBody)
	if err != nil {
		return body, false, fmt.Errorf("serialize normalized responses schema body: %w", err)
	}
	return normalized, true, nil
}

func normalizeOpenAIResponsesWebSocketCompatibilityBody(body []byte, account *Account, responsesLiteOpt ...bool) ([]byte, bool, error) {
	if account == nil || !account.IsOpenAI() {
		return body, false, nil
	}
	responsesLite := len(responsesLiteOpt) > 0 && responsesLiteOpt[0]
	normalized := body
	changed := false
	if account.IsOpenAIOAuthLike() {
		var err error
		normalized, changed, err = normalizeOpenAIResponsesLegacyIngress(body)
		if err != nil {
			return body, false, err
		}
	}
	if next, normalizedReasoningContent, err := normalizeOpenAIResponsesReasoningContentReplay(normalized); err != nil {
		return body, false, err
	} else if normalizedReasoningContent {
		normalized = next
		changed = true
	}
	if account.IsOpenAIApiKey() {
		if next, normalizedParallel, err := normalizeOpenAIParallelToolCallsWithoutTools(normalized, responsesLite); err != nil {
			return body, false, err
		} else if normalizedParallel {
			normalized = next
			changed = true
		}
		if next, normalizedReasoning, err := normalizeOpenAIAPIKeyStoreFalseReasoningReplay(normalized, false); err != nil {
			return body, false, err
		} else if normalizedReasoning {
			normalized = next
			changed = true
		}
	}
	if sanitized, idsChanged, err := sanitizeOpenAIResponsesInputItemIDs(normalized); err != nil {
		return body, false, fmt.Errorf("sanitize websocket Responses input item IDs: %w", err)
	} else if idsChanged {
		normalized = sanitized
		changed = true
	}
	if account.IsOAuth() {
		if reasoningBody, reasoningChanged, err := normalizeOpenAIResponsesReasoningMode(normalized); err != nil {
			return body, false, err
		} else if reasoningChanged {
			normalized = reasoningBody
			changed = true
		}
	}
	if account.IsOpenAIOAuthLike() {
		oauthBody, oauthChanged, err := normalizeOpenAIOAuthResponsesCompatibilityBody(normalized)
		if err != nil {
			return body, false, err
		}
		normalized = oauthBody
		changed = changed || oauthChanged
		for _, field := range openAIChatGPTInternalUnsupportedFields {
			if !gjson.GetBytes(normalized, field).Exists() {
				continue
			}
			next, deleteErr := sjson.DeleteBytes(normalized, field)
			if deleteErr != nil {
				return body, false, fmt.Errorf("normalize websocket body delete %s: %w", field, deleteErr)
			}
			normalized = next
			changed = true
		}
	}
	needsOrphanCleanup := account.IsOpenAIOAuthLike() && gjson.GetBytes(normalized, "input").IsArray()
	if needsOrphanCleanup || openAIResponsesInputMayNeedTruncation(normalized) {
		var reqBody map[string]any
		if err := decodeOpenAIJSONUseNumber(normalized, &reqBody); err != nil {
			return body, false, fmt.Errorf("normalize websocket Responses body: %w", err)
		}
		mapChanged := false
		if needsOrphanCleanup {
			if input, ok := reqBody["input"].([]any); ok && sanitizeOpenAIResponsesOrphanToolOutputs(
				reqBody,
				input,
				strings.TrimSpace(firstNonEmptyString(reqBody["previous_response_id"])) != "",
			) {
				mapChanged = true
			}
		}
		if truncateOpenAIResponsesInputText(reqBody) {
			mapChanged = true
		}
		if mapChanged {
			next, err := marshalOpenAIUpstreamJSON(reqBody)
			if err != nil {
				return body, false, fmt.Errorf("serialize normalized websocket Responses body: %w", err)
			}
			normalized = next
			changed = true
		}
	}
	if schemaBody, schemaChanged, err := normalizeOpenAIResponseFormatSchemasBody(normalized); err != nil {
		return body, false, err
	} else if schemaChanged {
		normalized = schemaBody
		changed = true
	}
	if openAIRequestBodyImageGenerationToolNeedsNormalization(normalized) {
		var reqBody map[string]any
		if err := json.Unmarshal(normalized, &reqBody); err != nil {
			return body, false, fmt.Errorf("normalize websocket image tool body: %w", err)
		}
		if normalizeOpenAIResponsesImageGenerationTools(reqBody) {
			next, err := json.Marshal(reqBody)
			if err != nil {
				return body, false, fmt.Errorf("serialize normalized websocket image tool body: %w", err)
			}
			normalized = next
			changed = true
		}
	}
	if schemaBody, schemaChanged, err := sanitizeOpenAIResponsesToolSchemasForPlatform(normalized, account.Platform); err != nil {
		return body, false, fmt.Errorf("normalize websocket tool schemas: %w", err)
	} else if schemaChanged {
		normalized = schemaBody
		changed = true
	}
	if triggerBody, triggerChanged, err := NormalizeCompactionTriggerInputOrder(normalized); err != nil {
		return body, false, fmt.Errorf("normalize websocket compaction trigger order: %w", err)
	} else if triggerChanged {
		normalized = triggerBody
		changed = true
	}
	return normalized, changed, nil
}
