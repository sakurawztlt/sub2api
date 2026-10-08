package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

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
	// Codex can attach internal message metadata when a custom provider is
	// named OpenAI. ChatGPT rejects this field on input items (#7066).
	input, _ := reqBody["input"].([]any)
	for _, value := range input {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := item["internal_chat_message_metadata_passthrough"]; exists {
			delete(item, "internal_chat_message_metadata_passthrough")
			changed = true
		}
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
	input := parseRawJSONView(body).Get("input")
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
	root := parseRawJSONView(body)
	input := root.Get("input")
	if !input.IsArray() {
		return body, false, nil
	}
	if !root.IsObject() || !gjson.ValidBytes(body) || !utf8.Valid(body) || hasDuplicateJSONObjectKeys(root) {
		return normalizeOpenAIAPIKeyStoreFalseReasoningReplayDecoded(body, knownStoreFalse)
	}

	// Only reasoning metadata needs decoding. Keep large image/tool results as
	// slices of the original JSON and copy them once into the final request.
	items := make([]string, 0)
	changed := false
	fallback := false
	var itemErr error
	input.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() {
			items = append(items, item.Raw)
			return true
		}
		if hasDuplicateJSONObjectKeys(item) {
			fallback = true
			return false
		}
		typ := strings.TrimSpace(item.Get("type").String())
		id := strings.TrimSpace(item.Get("id").String())
		encrypted := item.Get("encrypted_content")
		if (typ == "reasoning" && (encrypted.Type != gjson.String || strings.TrimSpace(encrypted.Str) == "")) ||
			(typ == "item_reference" && strings.HasPrefix(id, "rs_")) {
			changed = true
			return true
		}
		stripID := typ == "reasoning" && strings.HasPrefix(id, "rs_")
		addSummary := typ == "reasoning" && item.Get("summary").Type == gjson.Null
		stripCallID := shouldStripOpenAIResponsesNonPairCallID(typ) && item.Get("call_id").Exists()
		if !stripID && !addSummary && !stripCallID {
			items = append(items, item.Raw)
			return true
		}
		var decoded map[string]any
		if err := decodeOpenAIJSONUseNumber([]byte(item.Raw), &decoded); err != nil {
			itemErr = err
			return false
		}
		if stripID {
			delete(decoded, "id")
		}
		if addSummary {
			decoded["summary"] = []any{}
		}
		if stripCallID {
			delete(decoded, "call_id")
		}
		encoded, err := marshalOpenAIUpstreamJSON(decoded)
		if err != nil {
			itemErr = err
			return false
		}
		items = append(items, string(encoded))
		changed = true
		return true
	})
	if fallback {
		return normalizeOpenAIAPIKeyStoreFalseReasoningReplayDecoded(body, knownStoreFalse)
	}
	if itemErr != nil {
		return body, false, fmt.Errorf("normalize API-key store=false reasoning replay: %w", itemErr)
	}
	if !changed {
		return body, false, nil
	}
	return replaceOpenAIRawInput(body, input, items), true, nil
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
	// Only remove the input-item field, never same-named user content.
	input := gjson.GetBytes(normalized, "input")
	if !input.IsArray() {
		return normalized, changed, nil
	}
	for i, item := range input.Array() {
		if !item.IsObject() || !item.Get("internal_chat_message_metadata_passthrough").Exists() {
			continue
		}
		next, err := sjson.DeleteBytes(normalized, fmt.Sprintf("input.%d.internal_chat_message_metadata_passthrough", i))
		if err != nil {
			return body, false, fmt.Errorf("normalize oauth input metadata: %w", err)
		}
		normalized = next
		changed = true
	}
	return normalized, changed, nil
}

func normalizeOpenAIResponsesReasoningMode(body []byte, models ...string) ([]byte, bool, error) {
	model := ""
	if len(models) > 0 {
		model = models[0]
	}
	if len(body) == 0 {
		return body, false, nil
	}
	// GPT-6 treats reasoning.mode and reasoning.effort as independent
	// official fields. Preserve both verbatim; earlier models retain the
	// established mode stripping and pro-to-max compatibility behavior.
	if model == "" {
		model = gjson.GetBytes(body, "model").String()
	}
	if isOpenAIGPT6Model(model) {
		return normalizeGPT6ResponsesSampling(body, model)
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
	responsesLite := len(responsesLiteOpt) > 0 && responsesLiteOpt[0]
	if account == nil || !account.IsOpenAI() {
		return body, false, nil
	}
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
	if account != nil && account.IsOpenAI() && account.IsOAuth() {
		if reasoningBody, reasoningChanged, err := normalizeOpenAIResponsesReasoningMode(normalized, account.GetMappedModel(gjson.GetBytes(normalized, "model").String())); err != nil {
			return body, false, err
		} else if reasoningChanged {
			normalized = reasoningBody
			changed = true
		}
	}
	if account != nil && account.IsOpenAIOAuthLike() {
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
	needsOrphanCleanup := account != nil && account.IsOpenAIOAuthLike() &&
		gjson.GetBytes(normalized, "input").IsArray()
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
	if account != nil {
		if schemaBody, schemaChanged, err := sanitizeOpenAIResponsesToolSchemasForPlatform(normalized, account.Platform); err != nil {
			return body, false, fmt.Errorf("normalize websocket tool schemas: %w", err)
		} else if schemaChanged {
			normalized = schemaBody
			changed = true
		}
	}
	// Keep this last: earlier compatibility passes may filter or rebuild input.
	// Remote compaction v2 requires one trigger as the final input item.
	if triggerBody, triggerChanged, err := NormalizeCompactionTriggerInputOrder(normalized); err != nil {
		return body, false, fmt.Errorf("normalize websocket compaction trigger order: %w", err)
	} else if triggerChanged {
		normalized = triggerBody
		changed = true
	}
	return normalized, changed, nil
}
