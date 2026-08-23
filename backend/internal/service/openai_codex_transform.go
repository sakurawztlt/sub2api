package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

var codexModelMap = map[string]string{
	"gpt-5.6-sol":                "gpt-5.6-sol",
	"gpt-5.6-terra":              "gpt-5.6-terra",
	"gpt-5.6-luna":               "gpt-5.6-luna",
	"gpt-5.5":                    "gpt-5.5",
	"gpt-5.5-pro":                "gpt-5.5-pro",
	"gpt-5.4":                    "gpt-5.4",
	"gpt-5.4-mini":               "gpt-5.4-mini",
	"gpt-5.4-none":               "gpt-5.4",
	"gpt-5.4-low":                "gpt-5.4",
	"gpt-5.4-medium":             "gpt-5.4",
	"gpt-5.4-high":               "gpt-5.4",
	"gpt-5.4-xhigh":              "gpt-5.4",
	"gpt-5.4-chat-latest":        "gpt-5.4",
	"gpt-5.3":                    "gpt-5.3-codex",
	"gpt-5.3-none":               "gpt-5.3-codex",
	"gpt-5.3-low":                "gpt-5.3-codex",
	"gpt-5.3-medium":             "gpt-5.3-codex",
	"gpt-5.3-high":               "gpt-5.3-codex",
	"gpt-5.3-xhigh":              "gpt-5.3-codex",
	"gpt-5.3-codex":              "gpt-5.3-codex",
	"gpt-5.3-codex-spark":        "gpt-5.3-codex-spark",
	"gpt-5.3-codex-spark-low":    "gpt-5.3-codex-spark",
	"gpt-5.3-codex-spark-medium": "gpt-5.3-codex-spark",
	"gpt-5.3-codex-spark-high":   "gpt-5.3-codex-spark",
	"gpt-5.3-codex-spark-xhigh":  "gpt-5.3-codex-spark",
	"gpt-5.3-codex-low":          "gpt-5.3-codex",
	"gpt-5.3-codex-medium":       "gpt-5.3-codex",
	"gpt-5.3-codex-high":         "gpt-5.3-codex",
	"gpt-5.3-codex-xhigh":        "gpt-5.3-codex",
	"gpt-5.2":                    "gpt-5.2",
	"gpt-5.2-none":               "gpt-5.2",
	"gpt-5.2-low":                "gpt-5.2",
	"gpt-5.2-medium":             "gpt-5.2",
	"gpt-5.2-high":               "gpt-5.2",
	"gpt-5.2-xhigh":              "gpt-5.2",
	"gpt-5":                      "gpt-5.4",
	"gpt-5-mini":                 "gpt-5.4",
	"gpt-5-nano":                 "gpt-5.4",
	"gpt-5.1":                    "gpt-5.4",
	"gpt-5.1-codex":              "gpt-5.3-codex",
	"gpt-5.1-codex-max":          "gpt-5.3-codex",
	"gpt-5.1-codex-mini":         "gpt-5.3-codex",
	"gpt-5.2-codex":              "gpt-5.2",
	"codex-mini-latest":          "gpt-5.3-codex",
	"codex-auto-review":          "codex-auto-review",
	"gpt-5-codex":                "gpt-5.3-codex",
}

var codexVersionModelPrefixes = []struct {
	prefix string
	target string
}{
	{prefix: "gpt-5.6-sol", target: "gpt-5.6-sol"},
	{prefix: "gpt-5.6-terra", target: "gpt-5.6-terra"},
	{prefix: "gpt-5.6-luna", target: "gpt-5.6-luna"},
	{prefix: "gpt-5.3-codex-spark", target: "gpt-5.3-codex-spark"},
	{prefix: "gpt-5.3-codex", target: "gpt-5.3-codex"},
	{prefix: "gpt-5.4-mini", target: "gpt-5.4-mini"},
	{prefix: "gpt-5.4-nano", target: "gpt-5.4-nano"},
	{prefix: "gpt-5.5-pro", target: "gpt-5.5-pro"},
	{prefix: "gpt-5.5", target: "gpt-5.5"},
	{prefix: "gpt-5.4", target: "gpt-5.4"},
	{prefix: "gpt-5.2", target: "gpt-5.2"},
}

// codexOAuthTransformOptions configures applyCodexOAuthTransformWithOptions.
// Older 4-arg call sites still use applyCodexOAuthTransform, which routes
// IsCodexCLI / IsCompact / StoreEnabled here. The 058 messages bridge passes
// SkipDefaultInstructions=true (the bridge supplies its own instructions
// from the developer message) and PreserveToolCallIDs=true (Anthropic tool
// IDs already match upstream call_ids — re-prefixing them with fc_ would
// break the round trip).
type codexOAuthTransformOptions struct {
	IsCodexCLI              bool
	IsCompact               bool
	StoreEnabled            bool
	SkipDefaultInstructions bool
	PreserveToolCallIDs     bool
}

type codexTransformResult struct {
	Modified        bool
	NormalizedModel string
	PromptCacheKey  string
	ToolNameReverse map[string]string
	Error           error
	// PostTransformRequiresLocalReject — codex round33 fu52 (2026-05-19).
	// Set to true when, after stripping item_reference + ids on the OAuth
	// store=false path, the input still contains function_call_output
	// items but has no inline function_call/tool_call providing the
	// continuation context (and no previous_response_id). Forwarding
	// such a request to ChatGPT's internal Responses backend would 502
	// with the upstream complaining about orphan function_call_output
	// — equivalent to the 404 PR #2523 was originally written to fix,
	// just in a different shape.
	//
	// Native OpenAI Responses path (openai_gateway_service.go::Forward)
	// honors this by emitting a local 400 instead of forwarding. Other
	// callers (chat-completions and Claude /v1/messages bridge) only
	// log: chat-completions because the upstream rejection there is
	// already shaped like an OpenAI error, and the messages bridge
	// because codex round33 explicitly carves it out ("不要影响
	// /v1/messages 的 Claude mimic 主链路").
	PostTransformRequiresLocalReject bool
}

const (
	codexCallIDMaxLength = 64
	codexCallIDPrefix    = "fc_"
)

func normalizeCodexCallID(id string) string {
	return normalizeCodexCallIDForItemType("function_call", id)
}

func normalizeCodexCallIDForItemType(itemType, id string) string {
	prefix := openAIResponsesToolCallIDPrefix(itemType) + "_"
	candidate := id
	switch {
	case id == "":
		return ""
	case strings.HasPrefix(id, strings.TrimSuffix(prefix, "_")):
	case strings.HasPrefix(id, "call_"):
		candidate = prefix + strings.TrimPrefix(id, "call_")
	default:
		candidate = prefix + trimOpenAIResponsesKnownCallIDPrefix(id)
	}
	if len(candidate) <= codexCallIDMaxLength {
		return candidate
	}
	return compactCodexCallIDForItemType(itemType, candidate)
}

func compactCodexCallIDForItemType(itemType, id string) string {
	prefix := openAIResponsesToolCallIDPrefix(itemType) + "_"
	digest := sha256.Sum256([]byte("sub2api:codex-call-id:v1:" + id))
	encoded := hex.EncodeToString(digest[:])
	return prefix + encoded[:codexCallIDMaxLength-len(prefix)]
}

func trimOpenAIResponsesKnownCallIDPrefix(id string) string {
	for _, prefix := range []string{"fc_", "ctc_", "tsc_"} {
		if strings.HasPrefix(id, prefix) {
			return strings.TrimPrefix(id, prefix)
		}
	}
	return id
}

const codexImageGenerationFunctionToolName = "image_gen.imagegen"

const (
	codexImageGenerationBridgeMarker = "<sub2api-codex-image-generation>"
	codexImageGenerationBridgeText   = codexImageGenerationBridgeMarker + "\nWhen the user asks for raster image generation or editing, use the OpenAI Responses native `image_generation` tool attached to this request. The local Codex client may not expose an `image_gen` namespace, but that does not mean image generation is unavailable. Do not ask the user to switch to CLI fallback solely because `image_gen` is absent.\n</sub2api-codex-image-generation>"
	codexSparkImageUnsupportedMarker = "<sub2api-codex-spark-image-unsupported>"
	codexSparkImageUnsupportedText   = codexSparkImageUnsupportedMarker + "\nThe current model is gpt-5.3-codex-spark, which does not support image generation, image editing, image input, the `image_generation` tool, or Codex `image_gen`/`$imagegen` workflows. If the user asks for image generation or image editing, clearly explain this model limitation and ask them to switch to a non-Spark Codex model such as gpt-5.3-codex or gpt-5.4. Do not claim that the local environment merely lacks image_gen tooling, and do not suggest CLI fallback as the primary fix while the model remains Spark.\n</sub2api-codex-spark-image-unsupported>"
	codexGPT55Instructions           = "You are Codex, a coding agent based on GPT-5. You and the user share one workspace, and your job is to collaborate with them until their goal is genuinely handled."
)

var openAIChatGPTInternalUnsupportedFields = []string{
	"chat_template_kwargs",
	"user",
	"metadata",
	"prompt_cache_retention",
	"safety_identifier",
	"stream_options",
	"truncation",
	"stop_sequences",
}

var openAICodexOAuthUnsupportedFields = append([]string{
	"max_output_tokens",
	"max_completion_tokens",
	"temperature",
	"top_p",
	"frequency_penalty",
	"presence_penalty",
}, openAIChatGPTInternalUnsupportedFields...)

func applyCodexOAuthTransform(reqBody map[string]any, isCodexCLI bool, isCompact bool, storeEnabled bool) codexTransformResult {
	return applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{
		IsCodexCLI:   isCodexCLI,
		IsCompact:    isCompact,
		StoreEnabled: storeEnabled,
	})
}

func applyCodexOAuthTransformWithOptions(reqBody map[string]any, opts codexOAuthTransformOptions) codexTransformResult {
	isCodexCLI := opts.IsCodexCLI
	isCompact := opts.IsCompact
	storeEnabled := opts.StoreEnabled

	result := codexTransformResult{}
	if normalizeOpenAIOAuthResponsesCompatibilityFields(reqBody) {
		result.Modified = true
	}
	// codex round33 / upstream PR #2523 adapted (2026-05-19):
	//
	// Background. The OAuth path forces store=false (line ~150 below, plus
	// normalizeOpenAIPassthroughOAuthBody) because ChatGPT's internal
	// Responses backend rejected our earlier store=true attempts ("Store
	// must be set to false"). Under store=false the upstream does NOT
	// persist any prior turn's items, so any item_reference (or any
	// surviving item that still carries an `id` from a prior turn) gets
	// rejected with HTTP 404 ("Item with id '<fc_*|rs_*|...>' not found.
	// Items are not persisted when `store` is set to false."). gcr then
	// wraps that as a generic 502 to the client.
	//
	// Upstream PR #2523 (yetone, commit fa5af825) fixed this by passing
	// PreserveReferences=false unconditionally on the OAuth path. The
	// continuation context still works end-to-end because the standard
	// Responses-API continuation shape — function_call + matching
	// function_call_output inlined in the same input — carries the
	// full context.
	//
	// Local fork adaptation (codex round33). We have an extra knob
	// `opts.StoreEnabled` (per-account `openai_store_enabled` toggle)
	// that lets some accounts opt into store=true. For those, references
	// CAN be preserved — but only when the client's request body also
	// explicitly carries `store:true`. Conservative two-condition gate:
	//
	//   preserveReferences = false
	//   if opts.StoreEnabled AND requestStoreExplicitTrue(reqBody):
	//       preserveReferences = NeedsToolContinuation(reqBody)
	//
	// Missing-store-field never preserves (the previous behavior of
	// preserving on missing field is exactly what triggered the 404).
	preserveReferences := false
	if opts.StoreEnabled && requestStoreExplicitTrue(reqBody) {
		preserveReferences = NeedsToolContinuation(reqBody)
	}

	model := ""
	if v, ok := reqBody["model"].(string); ok {
		model = v
	}
	normalizedModel := strings.TrimSpace(model)
	if normalizedModel != "" {
		if model != normalizedModel {
			reqBody["model"] = normalizedModel
			result.Modified = true
		}
		result.NormalizedModel = normalizedModel
	}

	if isCompact {
		if _, ok := reqBody["store"]; ok {
			delete(reqBody, "store")
			result.Modified = true
		}
		if _, ok := reqBody["stream"]; ok {
			delete(reqBody, "stream")
			result.Modified = true
		}
	} else {
		// OAuth 走 ChatGPT internal API 时，默认 store=false，避免上游拒绝。
		// 若账号启用 openai_store_enabled，则保留客户端 store 值，允许上游存储响应以支持 previous_response_id 续链。
		if !storeEnabled {
			if v, ok := reqBody["store"].(bool); !ok || v {
				reqBody["store"] = false
				result.Modified = true
			}
		}
		if v, ok := reqBody["stream"].(bool); !ok || !v {
			reqBody["stream"] = true
			result.Modified = true
		}
	}
	if !isCompact && ensureCodexReasoningInclude(reqBody) {
		result.Modified = true
	}

	// Strip parameters unsupported by ChatGPT internal Codex endpoint.
	for _, key := range openAICodexOAuthUnsupportedFields {
		if _, ok := reqBody[key]; ok {
			delete(reqBody, key)
			result.Modified = true
		}
	}
	for _, key := range openAIResponsesUnsupportedFields {
		if _, ok := reqBody[key]; ok {
			delete(reqBody, key)
			result.Modified = true
		}
	}

	if stripUnsupportedOpenAIOAuthServiceTier(reqBody) {
		result.Modified = true
	}

	// 兼容遗留的 functions 和 function_call，转换为 tools 和 tool_choice
	if functionsRaw, ok := reqBody["functions"]; ok {
		if functions, k := functionsRaw.([]any); k {
			tools := make([]any, 0, len(functions))
			for _, f := range functions {
				tools = append(tools, map[string]any{
					"type":     "function",
					"function": f,
				})
			}
			reqBody["tools"] = tools
		}
		delete(reqBody, "functions")
		result.Modified = true
	}

	if fcRaw, ok := reqBody["function_call"]; ok {
		if fcStr, ok := fcRaw.(string); ok {
			// e.g. "auto", "none"
			reqBody["tool_choice"] = fcStr
		} else if fcObj, ok := fcRaw.(map[string]any); ok {
			// e.g. {"name": "my_func"}
			if name, ok := fcObj["name"].(string); ok && strings.TrimSpace(name) != "" {
				reqBody["tool_choice"] = map[string]any{
					"type": "function",
					"name": name,
				}
			}
		}
		delete(reqBody, "function_call")
		result.Modified = true
	}

	if normalizeCodexTools(reqBody) {
		result.Modified = true
	}
	// Collect aliases only after prompt/functions/function_call compatibility
	// has produced the final Responses protocol nodes. Otherwise references
	// introduced by those migrations can retain the reserved name.
	toolNameReverse, toolNamesChanged, err := aliasOpenAIOAuthReservedToolNames(reqBody)
	if err != nil {
		result.Error = err
		return result
	}
	result.ToolNameReverse = toolNameReverse
	if toolNamesChanged {
		result.Modified = true
	}
	if normalizeCodexToolChoice(reqBody) {
		result.Modified = true
	}

	if v, ok := reqBody["prompt_cache_key"].(string); ok {
		result.PromptCacheKey = strings.TrimSpace(v)
		// 058 step 2: when this transform is invoked by the Anthropic-messages
		// bridge, prompt_cache_key has already been captured into result and
		// must NOT be sent verbatim on the wire — Codex Pro upstream rejects
		// it as an unsupported field on the messages-bridged path.
		if isOpenAICompatMessagesBridgeRequestBody(reqBody) {
			delete(reqBody, "prompt_cache_key")
			result.Modified = true
		}
	}

	// 提取 input 中 role:"system" 消息至 instructions（OAuth 上游不支持 system role）。
	if extractSystemMessagesFromInput(reqBody) {
		result.Modified = true
	}

	// instructions 处理逻辑：根据是否是 Codex CLI 分别调用不同方法。
	// 058 step 2: messages bridge sets SkipDefaultInstructions to keep the
	// developer-message contents authoritative.
	if !opts.SkipDefaultInstructions && applyInstructions(reqBody, isCodexCLI) {
		result.Modified = true
	}
	if isCodexSparkModel(normalizedModel) && applyCodexSparkImageUnsupportedInstructions(reqBody) {
		result.Modified = true
	}
	// gpt-5.3-codex-spark rejects the image_generation tool upstream (HTTP 400,
	// param=tools); Codex CLI advertises it by default, so strip it for spark.
	if isCodexSparkModel(normalizedModel) && stripCodexSparkImageGenerationTools(reqBody) {
		result.Modified = true
	}

	// 续链场景保留 item_reference 与 id，避免 call_id 上下文丢失。
	if input, ok := reqBody["input"].([]any); ok {
		if normalizedInput, modified := normalizeCodexToolRoleMessages(input); modified {
			input = normalizedInput
			result.Modified = true
		}
		if normalizedInput, modified := normalizeCodexMessageContentText(input); modified {
			input = normalizedInput
			result.Modified = true
		}
		input = filterCodexInputWithOptions(input, codexInputFilterOptions{
			PreserveReferences: preserveReferences,
			PreserveCallIDs:    opts.PreserveToolCallIDs,
		})
		reqBody["input"] = input
		result.Modified = true

		// codex round33 fu52 (2026-05-19): post-filter shape check. If we
		// dropped item_reference (preserveReferences=false) AND the
		// resulting input still has function_call_output without an
		// inline function_call/tool_call providing the call_id context
		// (and no previous_response_id), the upstream Responses backend
		// will reject the request. Mark the result so the native
		// /v1/responses caller can emit a local 400 instead of
		// forwarding. Messages-bridge (Claude /v1/messages) caller
		// ignores this — codex round33 explicitly carves the Claude
		// mimic main path out of scope.
		if !preserveReferences {
			postValidation := ValidateFunctionCallOutputContext(reqBody)
			if postValidation.HasFunctionCallOutput && !postValidation.HasToolCallContext {
				prevID, _ := reqBody["previous_response_id"].(string)
				if strings.TrimSpace(prevID) == "" {
					result.PostTransformRequiresLocalReject = true
				}
			}
		}
	} else if inputStr, ok := reqBody["input"].(string); ok {
		// ChatGPT codex endpoint requires input to be a list, not a string.
		// Convert string input to the expected message array format.
		trimmed := strings.TrimSpace(inputStr)
		if trimmed != "" {
			reqBody["input"] = []any{
				map[string]any{
					"type":    "message",
					"role":    "user",
					"content": inputStr,
				},
			}
		} else {
			reqBody["input"] = []any{}
		}
		result.Modified = true
	}

	return result
}

// requestStoreExplicitTrue reports whether the client request body
// CARRIES an explicit `store: true` literal. Returns false for:
//
//   - missing field
//   - explicit `store: false`
//   - non-boolean value (string, null, object, etc.)
//
// codex round33 / fu52 (2026-05-19): used as the second condition for
// preserving item_reference / id refs on the OAuth path. The
// conservative choice — only honor explicit literal true, never default
// to preserve — matches codex's explicit guidance: "保守一点: 只认显式
// store:true, 不要因为字段缺失就默认保留引用, 避免又踩 store=false 404".
func requestStoreExplicitTrue(reqBody map[string]any) bool {
	if reqBody == nil {
		return false
	}
	v, ok := reqBody["store"]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	if !ok {
		return false
	}
	return b
}

func normalizeCodexToolChoice(reqBody map[string]any) bool {
	choice, ok := reqBody["tool_choice"]
	if !ok || choice == nil {
		return false
	}
	choiceMap, ok := choice.(map[string]any)
	if !ok {
		return false
	}
	choiceType := strings.TrimSpace(firstNonEmptyString(choiceMap["type"]))
	if choiceType == "" {
		return false
	}
	modified := false
	if choiceType == "function" {
		name := strings.TrimSpace(firstNonEmptyString(choiceMap["name"]))
		if name == "" {
			if function, ok := choiceMap["function"].(map[string]any); ok {
				name = strings.TrimSpace(firstNonEmptyString(function["name"]))
			}
		}
		if name == "" {
			reqBody["tool_choice"] = "auto"
			return true
		}
		if strings.TrimSpace(firstNonEmptyString(choiceMap["name"])) != name {
			choiceMap["name"] = name
			modified = true
		}
		if _, ok := choiceMap["function"]; ok {
			delete(choiceMap, "function")
			modified = true
		}
		if !codexToolsContainFunctionName(reqBody["tools"], name) {
			reqBody["tool_choice"] = "auto"
			return true
		}
		return modified
	}
	if codexToolsContainType(reqBody["tools"], choiceType) || codexInputAdditionalToolsContainType(reqBody["input"], choiceType) {
		return modified
	}
	reqBody["tool_choice"] = "auto"
	return true
}

func codexInputAdditionalToolsContainType(rawInput any, toolType string) bool {
	input, ok := rawInput.([]any)
	if !ok || strings.TrimSpace(toolType) == "" {
		return false
	}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyString(item["type"])) != "additional_tools" {
			continue
		}
		if codexToolsContainType(item["tools"], toolType) {
			return true
		}
	}
	return false
}

func codexToolsContainType(rawTools any, toolType string) bool {
	tools, ok := rawTools.([]any)
	if !ok || strings.TrimSpace(toolType) == "" {
		return false
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyString(tool["type"])) == toolType {
			return true
		}
	}
	return false
}

func codexToolsContainFunctionName(rawTools any, name string) bool {
	tools, ok := rawTools.([]any)
	if !ok || strings.TrimSpace(name) == "" {
		return false
	}
	normalizedName := strings.TrimSpace(name)
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyString(tool["type"])) != "function" {
			continue
		}
		toolName := strings.TrimSpace(firstNonEmptyString(tool["name"]))
		if toolName == "" {
			if function, ok := tool["function"].(map[string]any); ok {
				toolName = strings.TrimSpace(firstNonEmptyString(function["name"]))
			}
		}
		if toolName == normalizedName {
			return true
		}
	}
	return false
}

func normalizeCodexToolRoleMessages(input []any) ([]any, bool) {
	if len(input) == 0 {
		return input, false
	}

	modified := false
	normalized := make([]any, 0, len(input))
	for _, item := range input {
		m, ok := item.(map[string]any)
		if !ok {
			normalized = append(normalized, item)
			continue
		}
		role, _ := m["role"].(string)
		if strings.TrimSpace(role) != "tool" {
			normalized = append(normalized, item)
			continue
		}

		callID := firstNonEmptyString(m["call_id"], m["tool_call_id"], m["id"])
		callID = strings.TrimSpace(callID)
		if callID == "" {
			// Responses does not accept role:"tool". If no call id is available,
			// preserve the text as a user message instead of sending invalid input.
			fallback := make(map[string]any, len(m))
			for key, value := range m {
				fallback[key] = value
			}
			fallback["role"] = "user"
			delete(fallback, "tool_call_id")
			normalized = append(normalized, fallback)
			modified = true
			continue
		}

		output := extractTextFromContent(m["content"])
		if output == "" {
			if value, ok := m["output"].(string); ok {
				output = value
			}
		}
		if output == "" && m["content"] != nil {
			if b, err := json.Marshal(m["content"]); err == nil {
				output = string(b)
			}
		}

		normalized = append(normalized, map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  output,
		})
		modified = true
	}
	if !modified {
		return input, false
	}
	return normalized, true
}

func normalizeCodexMessageContentText(input []any) ([]any, bool) {
	if len(input) == 0 {
		return input, false
	}

	modified := false
	normalized := make([]any, 0, len(input))
	for _, item := range input {
		m, ok := item.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyString(m["type"])) != "message" {
			normalized = append(normalized, item)
			continue
		}
		parts, ok := m["content"].([]any)
		if !ok {
			normalized = append(normalized, item)
			continue
		}

		var newItem map[string]any
		var newParts []any
		ensureItemCopy := func() {
			if newItem != nil {
				return
			}
			newItem = make(map[string]any, len(m))
			for key, value := range m {
				newItem[key] = value
			}
			newParts = make([]any, len(parts))
			copy(newParts, parts)
		}

		for i, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			text, hasText := part["text"]
			if !hasText {
				continue
			}
			if _, ok := text.(string); ok {
				continue
			}

			ensureItemCopy()
			newPart := make(map[string]any, len(part))
			for key, value := range part {
				newPart[key] = value
			}
			newPart["text"] = stringifyCodexContentText(text)
			newParts[i] = newPart
			modified = true
		}

		if newItem != nil {
			newItem["content"] = newParts
			normalized = append(normalized, newItem)
			continue
		}
		normalized = append(normalized, item)
	}
	if !modified {
		return input, false
	}
	return normalized, true
}

func stringifyCodexContentText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		return fmt.Sprint(v)
	}
}

func normalizeCodexModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "gpt-5.4"
	}
	if mapped, ok := normalizeKnownCodexModel(model); ok {
		return mapped
	}
	return model
}

func normalizeKnownCodexModel(model string) (string, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", false
	}
	if isOpenAIImageGenerationModel(model) {
		return model, true
	}

	modelID := lastOpenAIModelSegment(model)

	if normalized := canonicalizeOpenAIModelAliasSpelling(modelID); normalized != "" {
		modelID = normalized
	}
	if mapped := normalizeKnownOpenAICodexModel(modelID); mapped != "" {
		return mapped, true
	}

	key := codexModelLookupKey(modelID)
	if key == "" {
		return "", false
	}
	if mapped := getNormalizedCodexModel(key); mapped != "" {
		return mapped, true
	}
	for _, item := range codexVersionModelPrefixes {
		if key == item.prefix {
			return item.target, true
		}
		suffix, ok := strings.CutPrefix(key, item.prefix+"-")
		if ok && isKnownCodexModelSuffix(suffix) {
			return item.target, true
		}
	}
	return "", false
}

func codexModelLookupKey(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return ""
	}
	if strings.Contains(modelID, "/") {
		parts := strings.Split(modelID, "/")
		modelID = parts[len(parts)-1]
	}
	return strings.ToLower(strings.Join(strings.Fields(modelID), "-"))
}

func isKnownCodexModelSuffix(suffix string) bool {
	switch suffix {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return true
	}
	return isCodexDateSuffix(suffix)
}

func isCodexDateSuffix(suffix string) bool {
	parts := strings.Split(suffix, "-")
	if len(parts) != 3 || len(parts[0]) != 4 || len(parts[1]) != 2 || len(parts[2]) != 2 {
		return false
	}
	for _, part := range parts {
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func isCodexSparkModel(model string) bool {
	return normalizeCodexModel(model) == "gpt-5.3-codex-spark"
}

func hasOpenAIImageGenerationTool(reqBody map[string]any) bool {
	if toolsContainImageGeneration(reqBody["tools"]) {
		return true
	}
	return inputContainsImageGenerationTool(reqBody["input"])
}

func hasCodexImageGenerationFunctionTool(reqBody map[string]any) bool {
	return len(reqBody) > 0 &&
		codexToolsContainFunctionName(reqBody["tools"], codexImageGenerationFunctionToolName)
}

func toolsContainImageGeneration(rawTools any) bool {
	if rawTools == nil {
		return false
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return false
	}
	for _, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if isOpenAIImageGenerationToolMap(toolMap) {
			return true
		}
	}
	return false
}

func isOpenAIImageGenerationToolMap(tool map[string]any) bool {
	return isOpenAIImageGenerationType(firstNonEmptyString(tool["type"])) ||
		isImageGenNamespaceToolMap(tool)
}

func isImageGenNamespaceToolMap(tool map[string]any) bool {
	return strings.TrimSpace(firstNonEmptyString(tool["type"])) == "namespace" &&
		isOpenAIImageGenNamespaceName(firstNonEmptyString(tool["name"]))
}

func inputContainsImageGenerationTool(rawInput any) bool {
	input, ok := rawInput.([]any)
	if !ok {
		return false
	}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyString(item["type"])) != "additional_tools" {
			continue
		}
		if toolsContainImageGeneration(item["tools"]) {
			return true
		}
	}
	return false
}

func stripOpenAIImageGenerationTools(reqBody map[string]any) bool {
	if reqBody == nil {
		return false
	}
	modified := stripOpenAIImageGenerationToolList(reqBody, "tools")
	if stripOpenAIImageGenerationToolsFromInput(reqBody) {
		modified = true
	}
	if openAIAnyToolChoiceSelectsImageGeneration(reqBody["tool_choice"]) {
		delete(reqBody, "tool_choice")
		modified = true
	}
	return modified
}

func stripOpenAIImageGenerationToolList(container map[string]any, key string) bool {
	rawTools, ok := container[key]
	if !ok || rawTools == nil {
		return false
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return false
	}
	filtered := make([]any, 0, len(tools))
	removed := false
	for _, rawTool := range tools {
		if toolMap, ok := rawTool.(map[string]any); ok && isOpenAIImageGenerationToolMap(toolMap) {
			removed = true
			continue
		}
		filtered = append(filtered, rawTool)
	}
	if !removed {
		return false
	}
	if len(filtered) == 0 {
		delete(container, key)
	} else {
		container[key] = filtered
	}
	return true
}

func stripOpenAIImageGenerationToolsFromInput(reqBody map[string]any) bool {
	input, ok := reqBody["input"].([]any)
	if !ok {
		return false
	}
	filteredInput := make([]any, 0, len(input))
	modified := false
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyString(item["type"])) != "additional_tools" {
			filteredInput = append(filteredInput, rawItem)
			continue
		}
		if !stripOpenAIImageGenerationToolList(item, "tools") {
			filteredInput = append(filteredInput, rawItem)
			continue
		}
		modified = true
		if _, hasTools := item["tools"]; hasTools {
			filteredInput = append(filteredInput, rawItem)
		}
	}
	if modified {
		reqBody["input"] = filteredInput
	}
	return modified
}

// stripOpenAIImageGenerationToolsFromRawPayload is shared by raw HTTP and
// WebSocket paths that bypass the normal request map transform.
func stripOpenAIImageGenerationToolsFromRawPayload(payload []byte) ([]byte, bool, error) {
	if !openAIRequestBodyHasImageGenerationDeclaration(payload) {
		if json.Valid(payload) {
			return payload, false, nil
		}
		var invalidPayload map[string]any
		return payload, false, json.Unmarshal(payload, &invalidPayload)
	}
	payloadMap := make(map[string]any)
	if err := json.Unmarshal(payload, &payloadMap); err != nil {
		return payload, false, err
	}
	if !stripOpenAIImageGenerationTools(payloadMap) {
		return payload, false, nil
	}
	rebuilt, err := json.Marshal(payloadMap)
	if err != nil {
		return payload, false, err
	}
	return rebuilt, true, nil
}

// stripCodexSparkImageGenerationTools removes image_generation tool entries from
// reqBody["tools"]. gpt-5.3-codex-spark rejects that tool upstream with HTTP 400
// (invalid_request_error, param=tools), and Codex CLI advertises it by default, so
// it must be dropped for spark. When the tools list becomes empty the key is removed.
// Returns true when the body was modified.
func stripCodexSparkImageGenerationTools(reqBody map[string]any) bool {
	return stripOpenAIImageGenerationTools(reqBody)
}

func hasOpenAIInputImage(reqBody map[string]any) bool {
	if reqBody == nil {
		return false
	}
	return hasOpenAIInputImageValue(reqBody["input"]) || hasOpenAIInputImageValue(reqBody["messages"])
}

func hasOpenAIInputImageValue(value any) bool {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if hasOpenAIInputImageValue(item) {
				return true
			}
		}
	case map[string]any:
		if strings.TrimSpace(firstNonEmptyString(v["type"])) == "input_image" {
			return true
		}
		if _, ok := v["image_url"]; ok {
			return true
		}
		return hasOpenAIInputImageValue(v["content"])
	}
	return false
}

func validateCodexSparkInput(reqBody map[string]any, model string) error {
	if !isCodexSparkModel(model) || !hasOpenAIInputImage(reqBody) {
		return nil
	}
	return fmt.Errorf("model %q does not support image input", strings.TrimSpace(model))
}

func normalizeOpenAIResponsesImageGenerationTools(reqBody map[string]any) bool {
	rawTools, ok := reqBody["tools"]
	if !ok || rawTools == nil {
		return false
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return false
	}

	modified := false
	for _, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyString(toolMap["type"])) != "image_generation" {
			continue
		}
		if _, ok := toolMap["output_format"]; !ok {
			if value := strings.TrimSpace(firstNonEmptyString(toolMap["format"])); value != "" {
				toolMap["output_format"] = value
				modified = true
			}
		}
		if _, ok := toolMap["output_compression"]; !ok {
			if value, exists := toolMap["compression"]; exists && value != nil {
				toolMap["output_compression"] = value
				modified = true
			}
		}
		if _, ok := toolMap["format"]; ok {
			delete(toolMap, "format")
			modified = true
		}
		if _, ok := toolMap["compression"]; ok {
			delete(toolMap, "compression")
			modified = true
		}
		imageModel := strings.ToLower(strings.TrimSpace(firstNonEmptyString(toolMap["model"])))
		if strings.HasPrefix(imageModel, "gpt-image-2") {
			if _, ok := toolMap["input_fidelity"]; ok {
				delete(toolMap, "input_fidelity")
				modified = true
			}
		}
	}
	return modified
}

func normalizeOpenAIResponseFormatSchemas(reqBody map[string]any) bool {
	if reqBody == nil {
		return false
	}
	modified := false
	normalizeFormat := func(format map[string]any) {
		if format == nil || strings.TrimSpace(firstNonEmptyString(format["type"])) != "json_schema" {
			return
		}
		if schema, ok := format["schema"].(map[string]any); ok && normalizeOpenAIResponseJSONSchema(schema) {
			modified = true
		}
		if jsonSchema, ok := format["json_schema"].(map[string]any); ok {
			if schema, ok := jsonSchema["schema"].(map[string]any); ok && normalizeOpenAIResponseJSONSchema(schema) {
				modified = true
			}
		}
	}
	if text, ok := reqBody["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			normalizeFormat(format)
		}
	}
	if responseFormat, ok := reqBody["response_format"].(map[string]any); ok {
		normalizeFormat(responseFormat)
	}
	return modified
}

func normalizeOpenAIResponseJSONSchema(schema map[string]any) bool {
	if schema == nil {
		return false
	}
	modified := false
	for _, key := range []string{"uniqueItems", "minProperties"} {
		if _, exists := schema[key]; exists {
			delete(schema, key)
			modified = true
		}
	}
	if rawType, exists := schema["type"]; !exists || rawType == nil {
		switch {
		case schema["properties"] != nil:
			schema["type"] = "object"
			modified = true
		case schema["items"] != nil:
			schema["type"] = "array"
			modified = true
		}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		for _, raw := range properties {
			if child, ok := raw.(map[string]any); ok && normalizeOpenAIResponseJSONSchema(child) {
				modified = true
			}
		}
	}
	switch items := schema["items"].(type) {
	case map[string]any:
		if normalizeOpenAIResponseJSONSchema(items) {
			modified = true
		}
	case []any:
		for _, raw := range items {
			if child, ok := raw.(map[string]any); ok && normalizeOpenAIResponseJSONSchema(child) {
				modified = true
			}
		}
	}
	for _, key := range []string{
		"additionalProperties",
		"additionalItems",
		"contains",
		"not",
		"if",
		"then",
		"else",
		"propertyNames",
		"unevaluatedProperties",
		"unevaluatedItems",
	} {
		if child, ok := schema[key].(map[string]any); ok && normalizeOpenAIResponseJSONSchema(child) {
			modified = true
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		children, _ := schema[key].([]any)
		for _, raw := range children {
			if child, ok := raw.(map[string]any); ok && normalizeOpenAIResponseJSONSchema(child) {
				modified = true
			}
		}
	}
	for _, key := range []string{"$defs", "definitions", "patternProperties", "dependentSchemas"} {
		children, _ := schema[key].(map[string]any)
		for _, raw := range children {
			if child, ok := raw.(map[string]any); ok && normalizeOpenAIResponseJSONSchema(child) {
				modified = true
			}
		}
	}
	if dependencies, ok := schema["dependencies"].(map[string]any); ok {
		for _, raw := range dependencies {
			if child, ok := raw.(map[string]any); ok && normalizeOpenAIResponseJSONSchema(child) {
				modified = true
			}
		}
	}
	return modified
}

// codexImageGenerationBridgeShouldFire reports whether the request carries an
// explicit image-generation signal. Without such a signal we leave the body
// alone so that plain Codex coding requests don't get an unwanted
// image_generation tool injected (issue #2280).
//
// Recognized signals:
//   - tools[] already declares image_generation (user-opted in)
//   - tool_choice selects image_generation (issue #2254)
//   - input contains an input_image part (image edit / multi-modal turn)
//   - input contains a previous image_generation_call item (continuation turn)
func codexImageGenerationBridgeShouldFire(reqBody map[string]any) bool {
	if len(reqBody) == 0 {
		return false
	}
	if hasOpenAIImageGenerationTool(reqBody) {
		return true
	}
	if openAIAnyToolChoiceSelectsImageGeneration(reqBody["tool_choice"]) {
		return true
	}
	if hasOpenAIInputImage(reqBody) {
		return true
	}
	if hasOpenAIImageGenerationCallItem(reqBody["input"]) {
		return true
	}
	return openAIInputTextLikelyRequestsImageGeneration(reqBody["input"])
}

// openAIAnyToolChoiceSelectsImageGeneration checks whether tool_choice — in
// either the OpenAI Responses ("type":"image_generation"), legacy ChatCompletions
// nested ({"type":"function","function":{"name":"image_generation"}}), or string
// shape — explicitly selects image_generation.
//
// fork helper added 5/9 to satisfy PR #2294 reference (upstream merged in a
// later PR that we haven't pulled).
func openAIAnyToolChoiceSelectsImageGeneration(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return isOpenAIImageGenerationType(v)
	case map[string]any:
		choiceType := strings.TrimSpace(firstNonEmptyString(v["type"]))
		if isOpenAIImageGenerationType(choiceType) {
			return true
		}
		if choiceType == "namespace" &&
			(isOpenAIImageGenNamespaceName(firstNonEmptyString(v["name"])) ||
				isOpenAIImageGenNamespaceName(firstNonEmptyString(v["namespace"]))) {
			return true
		}
		// Chat Completions nested function shape.
		if fn, ok := v["function"].(map[string]any); ok {
			if isOpenAIImageGenerationType(firstNonEmptyString(fn["name"])) {
				return true
			}
		}
		if tool, ok := v["tool"].(map[string]any); ok {
			if openAIAnyToolChoiceSelectsImageGeneration(tool) {
				return true
			}
		}
		if isOpenAIImageGenerationType(firstNonEmptyString(v["name"])) {
			return true
		}
	case []any:
		for _, item := range v {
			if openAIAnyToolChoiceSelectsImageGeneration(item) {
				return true
			}
		}
	}
	return false
}

func hasOpenAIImageGenerationCallItem(value any) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyString(item["type"])) == "image_generation_call" {
			return true
		}
	}
	return false
}

func openAIInputTextLikelyRequestsImageGeneration(value any) bool {
	switch v := value.(type) {
	case string:
		return openAITextLikelyRequestsImageGeneration(v)
	case []any:
		for _, item := range v {
			if openAIInputTextLikelyRequestsImageGeneration(item) {
				return true
			}
		}
	case map[string]any:
		if text := firstNonEmptyString(v["text"], v["content"]); openAITextLikelyRequestsImageGeneration(text) {
			return true
		}
		if openAIInputTextLikelyRequestsImageGeneration(v["content"]) {
			return true
		}
		if openAIInputTextLikelyRequestsImageGeneration(v["input"]) {
			return true
		}
	}
	return false
}

func openAITextLikelyRequestsImageGeneration(text string) bool {
	text = strings.TrimSpace(strings.ToLower(text))
	if text == "" {
		return false
	}
	englishTriggers := []string{
		"draw ",
		"draw a ",
		"draw an ",
		"generate an image",
		"generate image",
		"create an image",
		"create image",
		"make an image",
		"make image",
		"render an image",
		"render image",
		"image generation",
	}
	for _, trigger := range englishTriggers {
		if strings.Contains(text, trigger) {
			return true
		}
	}
	chineseTriggers := []string{"画一", "画张", "画个", "绘制", "生成图片", "生成一张", "生成图像", "做一张图", "出一张图"}
	for _, trigger := range chineseTriggers {
		if strings.Contains(text, trigger) {
			return true
		}
	}
	return false
}

func ensureOpenAIResponsesImageGenerationTool(reqBody map[string]any) bool {
	if len(reqBody) == 0 {
		return false
	}
	if isCodexSparkModel(firstNonEmptyString(reqBody["model"])) {
		return false
	}
	if hasCodexImageGenerationFunctionTool(reqBody) {
		return false
	}
	if hasOpenAIImageGenerationTool(reqBody) {
		return false
	}

	tool := map[string]any{
		"type":          "image_generation",
		"output_format": "png",
	}

	rawTools, ok := reqBody["tools"]
	if !ok || rawTools == nil {
		reqBody["tools"] = []any{tool}
		return true
	}

	tools, ok := rawTools.([]any)
	if !ok {
		reqBody["tools"] = []any{tool}
		return true
	}
	for _, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyString(toolMap["type"])) == "image_generation" {
			return false
		}
	}

	reqBody["tools"] = append(tools, tool)
	return true
}

func ensureOpenAIResponsesImageGenerationToolChoiceAuto(reqBody map[string]any) bool {
	if len(reqBody) == 0 || hasCodexImageGenerationFunctionTool(reqBody) || !hasOpenAIImageGenerationTool(reqBody) {
		return false
	}
	if isCodexSparkModel(firstNonEmptyString(reqBody["model"])) {
		return false
	}
	if _, ok := reqBody["tool_choice"]; ok {
		return false
	}
	reqBody["tool_choice"] = "auto"
	return true
}

func applyCodexImageGenerationBridgeInstructions(reqBody map[string]any) bool {
	if len(reqBody) == 0 || hasCodexImageGenerationFunctionTool(reqBody) || !hasOpenAIImageGenerationTool(reqBody) {
		return false
	}
	if isCodexSparkModel(firstNonEmptyString(reqBody["model"])) {
		return false
	}

	existing, _ := reqBody["instructions"].(string)
	if strings.Contains(existing, codexImageGenerationBridgeMarker) {
		return false
	}

	existing = strings.TrimRight(existing, " \t\r\n")
	if strings.TrimSpace(existing) == "" {
		reqBody["instructions"] = codexImageGenerationBridgeText
		return true
	}

	reqBody["instructions"] = existing + "\n\n" + codexImageGenerationBridgeText
	return true
}

func applyCodexSparkImageUnsupportedInstructions(reqBody map[string]any) bool {
	if len(reqBody) == 0 {
		return false
	}
	existing, _ := reqBody["instructions"].(string)
	if strings.Contains(existing, codexSparkImageUnsupportedMarker) {
		return false
	}
	existing = strings.TrimRight(existing, " \t\r\n")
	if strings.TrimSpace(existing) == "" {
		reqBody["instructions"] = codexSparkImageUnsupportedText
		return true
	}
	reqBody["instructions"] = existing + "\n\n" + codexSparkImageUnsupportedText
	return true
}

func validateOpenAIResponsesImageModel(reqBody map[string]any, model string) error {
	if !hasOpenAIImageGenerationTool(reqBody) {
		return nil
	}
	model = strings.TrimSpace(model)
	if !isOpenAIImageGenerationModel(model) {
		return nil
	}
	return fmt.Errorf("/v1/responses image_generation requests require a Responses-capable text model; image-only model %q is not allowed", model)
}

func normalizeOpenAIResponsesImageOnlyModel(reqBody map[string]any) bool {
	if len(reqBody) == 0 {
		return false
	}
	imageModel := strings.TrimSpace(firstNonEmptyString(reqBody["model"]))
	if !isOpenAIImageGenerationModel(imageModel) {
		return false
	}

	modified := false
	tools, _ := reqBody["tools"].([]any)
	imageToolIndex := -1
	for i, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyString(toolMap["type"])) == "image_generation" {
			imageToolIndex = i
			break
		}
	}
	if imageToolIndex < 0 {
		tools = append(tools, map[string]any{
			"type":  "image_generation",
			"model": imageModel,
		})
		imageToolIndex = len(tools) - 1
		reqBody["tools"] = tools
		modified = true
	}

	if toolMap, ok := tools[imageToolIndex].(map[string]any); ok {
		if strings.TrimSpace(firstNonEmptyString(toolMap["model"])) == "" {
			toolMap["model"] = imageModel
			modified = true
		}
		for _, key := range []string{
			"size",
			"quality",
			"background",
			"output_format",
			"output_compression",
			"moderation",
			"style",
			"partial_images",
		} {
			if value, exists := reqBody[key]; exists && value != nil {
				if _, toolHas := toolMap[key]; !toolHas {
					toolMap[key] = value
				}
				delete(reqBody, key)
				modified = true
			}
		}
	}

	if prompt := strings.TrimSpace(firstNonEmptyString(reqBody["prompt"])); prompt != "" {
		if _, hasInput := reqBody["input"]; !hasInput {
			reqBody["input"] = prompt
		}
		delete(reqBody, "prompt")
		modified = true
	}

	if _, ok := reqBody["tool_choice"]; !ok {
		reqBody["tool_choice"] = map[string]any{"type": "image_generation"}
		modified = true
	}
	if imageModel != openAIImagesResponsesMainModel {
		modified = true
	}
	reqBody["model"] = openAIImagesResponsesMainModel
	return modified
}

func normalizeOpenAIModelForUpstream(account *Account, model string) string {
	if account == nil || account.UsesOpenAICodexProtocol() {
		return normalizeCodexModel(model)
	}
	return strings.TrimSpace(model)
}

func SupportsVerbosity(model string) bool {
	if !strings.HasPrefix(model, "gpt-") {
		return true
	}

	var major, minor int
	n, _ := fmt.Sscanf(model, "gpt-%d.%d", &major, &minor)

	if major > 5 {
		return true
	}
	if major < 5 {
		return false
	}

	// gpt-5
	if n == 1 {
		return true
	}

	return minor >= 3
}

func getNormalizedCodexModel(modelID string) string {
	key := codexModelLookupKey(modelID)
	if key == "" {
		return ""
	}
	if mapped, ok := codexModelMap[key]; ok {
		return mapped
	}
	return ""
}

// extractTextFromContent extracts plain text from a content value that is either
// a Go string or a []any of text-like content-part maps. 058 step 2: the
// Anthropic→Responses bridge emits typed input_text/output_text parts (Codex
// Pro shape), not the bare type:"text" used by chat-completions, so all three
// variants must be honoured.
func extractTextFromContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, part := range v {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch t, _ := m["type"].(string); t {
			case "text", "input_text", "output_text":
				if text, ok := m["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}

// extractSystemMessagesFromInput scans the input array for items with
// role=="system" or role=="developer", removes them, and merges their
// content into reqBody["instructions"]. 058 step 2: the Anthropic→Responses
// bridge maps Anthropic `system` to a developer message; Codex OAuth still
// expects the prompt to live in the instructions field, and fork's forced-
// template feature reads the rendered text out of `reqBody["instructions"]`.
// If instructions is already non-empty, extracted content is prepended with
// "\n\n". Returns true if any system/developer messages were extracted.
func extractSystemMessagesFromInput(reqBody map[string]any) bool {
	input, ok := reqBody["input"].([]any)
	if !ok || len(input) == 0 {
		return false
	}

	var systemTexts []string
	remaining := make([]any, 0, len(input))

	for _, item := range input {
		m, ok := item.(map[string]any)
		if !ok {
			remaining = append(remaining, item)
			continue
		}
		role, _ := m["role"].(string)
		if role != "system" && role != "developer" {
			remaining = append(remaining, item)
			continue
		}
		if text := extractTextFromContent(m["content"]); text != "" {
			if strings.Contains(text, openAICompatClaudeCodeTodoGuardMarker) {
				remaining = append(remaining, item)
				continue
			}
			systemTexts = append(systemTexts, text)
		}
	}

	if len(systemTexts) == 0 {
		return false
	}

	extracted := strings.Join(systemTexts, "\n\n")
	if existing, ok := reqBody["instructions"].(string); ok && strings.TrimSpace(existing) != "" {
		reqBody["instructions"] = extracted + "\n\n" + existing
	} else {
		reqBody["instructions"] = extracted
	}
	reqBody["input"] = remaining
	return true
}

// extractPromptLikeInstructionsFromInput collects developer/system message
// text already present in the input array. The 058 messages bridge uses this
// to derive a non-empty instructions block so SkipDefaultInstructions=true
// does not strand the request with an empty instructions field upstream.
func extractPromptLikeInstructionsFromInput(reqBody map[string]any) string {
	input, ok := reqBody["input"].([]any)
	if !ok || len(input) == 0 {
		return ""
	}
	var texts []string
	for _, item := range input {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "developer", "system":
			if text := strings.TrimSpace(extractTextFromContent(m["content"])); text != "" {
				texts = append(texts, text)
			}
		}
	}
	return strings.Join(texts, "\n\n")
}

// defaultCodexSynthInstructions returns the closest captured Codex CLI base
// prompt for synthesized requests whose instructions field is empty.
func defaultCodexSynthInstructions(model string) string {
	if instructions := strings.TrimSpace(openai.CodexBaseInstructionsForModel(model)); instructions != "" {
		return instructions
	}
	return "You are a helpful coding assistant."
}

// ensureCodexReasoningInclude mirrors the Codex CLI request shape whenever
// reasoning is enabled, while preserving existing include entries.
func ensureCodexReasoningInclude(reqBody map[string]any) bool {
	reasoning, ok := reqBody["reasoning"].(map[string]any)
	if !ok || len(reasoning) == 0 {
		return false
	}
	const encrypted = "reasoning.encrypted_content"
	switch existing := reqBody["include"].(type) {
	case nil:
		reqBody["include"] = []any{encrypted}
		return true
	case []any:
		for _, value := range existing {
			if text, ok := value.(string); ok && text == encrypted {
				return false
			}
		}
		reqBody["include"] = append(existing, encrypted)
		return true
	default:
		return false
	}
}

// applyCodexClientMetadata adds the account's real installation identifier
// without overwriting client-provided metadata or inventing an identifier.
func applyCodexClientMetadata(reqBody map[string]any, account *Account) bool {
	if account == nil {
		return false
	}
	deviceID := strings.TrimSpace(account.GetOpenAIDeviceID())
	if deviceID == "" {
		return false
	}
	const key = "x-codex-installation-id"
	switch existing := reqBody["client_metadata"].(type) {
	case map[string]any:
		if value, ok := existing[key].(string); ok && strings.TrimSpace(value) != "" {
			return false
		}
		existing[key] = deviceID
		reqBody["client_metadata"] = existing
		return true
	case map[string]string:
		if strings.TrimSpace(existing[key]) != "" {
			return false
		}
		next := make(map[string]any, len(existing)+1)
		for name, value := range existing {
			next[name] = value
		}
		next[key] = deviceID
		reqBody["client_metadata"] = next
		return true
	case nil:
		reqBody["client_metadata"] = map[string]any{key: deviceID}
		return true
	default:
		return false
	}
}

// applyInstructions 处理 instructions 字段：仅在 instructions 为空时填充默认值。
func applyInstructions(reqBody map[string]any, isCodexCLI bool) bool {
	if !isInstructionsEmpty(reqBody) {
		return false
	}
	_ = isCodexCLI
	model, _ := reqBody["model"].(string)
	reqBody["instructions"] = defaultCodexSynthInstructions(model)
	return true
}

func defaultCodexOAuthInstructions(reqBody map[string]any) string {
	model, _ := reqBody["model"].(string)
	return defaultCodexSynthInstructions(model)
}

// isInstructionsEmpty 检查 instructions 字段是否为空
// 处理以下情况：字段不存在、nil、空字符串、纯空白字符串
func isInstructionsEmpty(reqBody map[string]any) bool {
	val, exists := reqBody["instructions"]
	if !exists {
		return true
	}
	if val == nil {
		return true
	}
	str, ok := val.(string)
	if !ok {
		return true
	}
	return strings.TrimSpace(str) == ""
}

// codexInputFilterOptions tunes filterCodexInput behaviour for 058's
// messages-bridge path. PreserveCallIDs keeps Anthropic-style toolu_/call_
// ids verbatim instead of fc_-prefixing them — round-tripping through the
// Anthropic compatibility layer requires lossless ids.
type codexInputFilterOptions struct {
	PreserveReferences bool
	PreserveCallIDs    bool
}

// filterCodexInput 按需过滤 item_reference 与 id。
// preserveReferences 为 true 时保持引用与 id，以满足续链请求对上下文的依赖。
func filterCodexInput(input []any, preserveReferences bool) []any {
	return filterCodexInputWithOptions(input, codexInputFilterOptions{
		PreserveReferences: preserveReferences,
	})
}

func normalizeCodexFilterCallID(itemType, id string, preserve bool) string {
	if preserve && len(id) <= codexCallIDMaxLength {
		return id
	}
	return normalizeCodexCallIDForItemType(itemType, id)
}

func codexItemReferenceIDMappings(input []any, preserveCallIDs bool) map[string]string {
	mappings := make(map[string]string)
	ambiguous := make(map[string]struct{})
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		itemType := strings.TrimSpace(firstNonEmptyString(item["type"]))
		if !isCodexToolCallItemType(itemType) {
			continue
		}
		rawCallID := strings.TrimSpace(firstNonEmptyString(item["call_id"]))
		if rawCallID == "" {
			continue
		}
		normalized := normalizeCodexFilterCallID(itemType, rawCallID, preserveCallIDs)
		if existing, exists := mappings[rawCallID]; exists && existing != normalized {
			delete(mappings, rawCallID)
			ambiguous[rawCallID] = struct{}{}
			continue
		}
		if _, conflict := ambiguous[rawCallID]; !conflict {
			mappings[rawCallID] = normalized
		}
	}
	return mappings
}

func codexInputItemIDs(input []any) map[string]struct{} {
	itemIDs := make(map[string]struct{})
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyString(item["type"])) == "item_reference" {
			continue
		}
		itemType := strings.TrimSpace(firstNonEmptyString(item["type"]))
		id := strings.TrimSpace(firstNonEmptyString(item["id"]))
		if id != "" && !shouldStripOpenAIResponsesInputItemID(itemType, id) {
			itemIDs[id] = struct{}{}
		}
	}
	return itemIDs
}

func codexInputCallIDs(input []any) map[string]struct{} {
	callIDs := make(map[string]struct{})
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || !isCodexToolCallItemType(strings.TrimSpace(firstNonEmptyString(item["type"]))) {
			continue
		}
		if callID := strings.TrimSpace(firstNonEmptyString(item["call_id"])); callID != "" {
			callIDs[callID] = struct{}{}
		}
	}
	return callIDs
}

func filterCodexInputWithOptions(input []any, opts codexInputFilterOptions) []any {
	preserveReferences := opts.PreserveReferences
	filtered := make([]any, 0, len(input))
	referenceIDMappings := codexItemReferenceIDMappings(input, opts.PreserveCallIDs)
	inputItemIDs := codexInputItemIDs(input)
	inputCallIDs := codexInputCallIDs(input)
	for _, item := range input {
		m, ok := item.(map[string]any)
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		typ, _ := m["type"].(string)

		// chatgpt.com codex (OAuth path) runs with store=false (forced by
		// applyCodexOAuthTransform). Replaying a reasoning item with its rs_*
		// id but no encrypted_content 404s upstream ("Item with id 'rs_...'
		// not found") — the 404 is triggered by the id lookup, not by the
		// reasoning item itself. So strip the id (always, independent of
		// PreserveReferences) yet keep the item: under store=false
		// encrypted_content is the official channel for carrying reasoning
		// context across turns, and dropping the whole item silently degrades
		// multi-turn agent reasoning. Preserve encrypted_content/content/
		// summary and every other field verbatim. Upstream additionally
		// requires a summary field — a missing one is rejected with 400
		// "Missing required parameter 'input[N].summary'" — so backfill an
		// empty array when it is absent. Contracts verified end-to-end against
		// chatgpt.com codex (gpt-5.5); see issue #1957.
		// compaction_summary items (cmp_*) are the other encrypted_content
		// carrier. Verified against the live backend: they require
		// encrypted_content (a missing one is rejected with 400), and with it
		// present the cmp_* id does not 404 whether kept or stripped. Being
		// neither reasoning nor tool calls, they flow through the generic path
		// below (id stripped when !PreserveReferences, encrypted_content
		// preserved either way), which is safe and needs no special-casing.
		if typ == "reasoning" {
			newItem := make(map[string]any, len(m))
			for key, value := range m {
				if key == "id" || key == "call_id" {
					// rs_* id replayed under store=false 404s; strip it.
					continue
				}
				newItem[key] = value
			}
			if summary, ok := newItem["summary"]; !ok || summary == nil {
				// Upstream requires a summary field; an empty array satisfies it.
				newItem["summary"] = []any{}
			}
			filtered = append(filtered, newItem)
			continue
		}

		// 仅修正真正的 tool/function call 标识，避免误改普通 message/reasoning id；
		// 若 item_reference 指向 legacy call_* 标识，则仅修正该引用本身。
		fixCallIDPrefix := func(id string) string {
			return normalizeCodexFilterCallID(typ, id, opts.PreserveCallIDs)
		}

		if typ == "item_reference" {
			if !preserveReferences {
				continue
			}
			newItem := make(map[string]any, len(m))
			for key, value := range m {
				newItem[key] = value
			}
			if id, ok := newItem["id"].(string); ok && strings.HasPrefix(strings.TrimSpace(id), "call_") {
				trimmedID := strings.TrimSpace(id)
				_, referencesExistingItem := inputItemIDs[trimmedID]
				if !referencesExistingItem {
					if normalizedID, mapped := referenceIDMappings[trimmedID]; mapped {
						newItem["id"] = normalizedID
					} else if _, hasSameTurnCall := inputCallIDs[trimmedID]; !hasSameTurnCall {
						// A bare call_* reference is a legacy function-call identifier.
						// Normalize it even when its call item lives in an earlier turn.
						newItem["id"] = normalizeCodexCallID(trimmedID)
					}
				}
			}
			filtered = append(filtered, newItem)
			continue
		}

		newItem := m
		copied := false
		// 仅在需要修改字段时创建副本，避免直接改写原始输入。
		ensureCopy := func() {
			if copied {
				return
			}
			newItem = make(map[string]any, len(m))
			for key, value := range m {
				newItem[key] = value
			}
			copied = true
		}

		if isCodexToolCallItemType(typ) {
			callID, ok := m["call_id"].(string)
			if !ok || strings.TrimSpace(callID) == "" {
				if id, ok := m["id"].(string); ok && strings.TrimSpace(id) != "" {
					callID = id
					ensureCopy()
					newItem["call_id"] = callID
				}
			}

			if callID != "" {
				fixedCallID := fixCallIDPrefix(callID)
				if fixedCallID != callID {
					ensureCopy()
					newItem["call_id"] = fixedCallID
				}
			}
		}

		if !isCodexToolCallItemType(typ) {
			ensureCopy()
			delete(newItem, "call_id")
		}

		if codexInputItemRequiresName(typ) {
			if strings.TrimSpace(firstNonEmptyString(m["name"])) == "" {
				name := firstNonEmptyString(m["tool_name"])
				if name == "" {
					if function, ok := m["function"].(map[string]any); ok {
						name = firstNonEmptyString(function["name"])
					}
				}
				if name == "" {
					name = "tool"
				}
				ensureCopy()
				newItem["name"] = name
			}
		}

		if !preserveReferences {
			ensureCopy()
			delete(newItem, "id")
		} else if id, ok := m["id"].(string); ok && shouldStripOpenAIResponsesInputItemID(typ, id) {
			ensureCopy()
			delete(newItem, "id")
		}

		filtered = append(filtered, newItem)
	}
	return filtered
}

func isCodexToolCallItemType(typ string) bool {
	switch typ {
	case "function_call",
		"tool_call",
		"local_shell_call",
		"tool_search_call",
		"custom_tool_call",
		"mcp_tool_call",
		"function_call_output",
		"mcp_tool_call_output",
		"custom_tool_call_output",
		"tool_search_output":
		return true
	default:
		return false
	}
}

func isCodexToolCallInputType(typ string) bool {
	switch typ {
	case "function_call",
		"tool_call",
		"local_shell_call",
		"tool_search_call",
		"custom_tool_call",
		"mcp_tool_call":
		return true
	default:
		return false
	}
}

func codexInputItemRequiresName(typ string) bool {
	switch strings.TrimSpace(typ) {
	case "function_call", "custom_tool_call", "mcp_tool_call":
		return true
	default:
		return false
	}
}

func normalizeCodexTools(reqBody map[string]any) bool {
	rawTools, ok := reqBody["tools"]
	if !ok || rawTools == nil {
		return false
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return false
	}

	modified := false
	validTools := make([]any, 0, len(tools))

	for _, tool := range tools {
		toolMap, ok := tool.(map[string]any)
		if !ok {
			// Keep unknown structure as-is to avoid breaking upstream behavior.
			validTools = append(validTools, tool)
			continue
		}

		toolType, _ := toolMap["type"].(string)
		toolType = strings.TrimSpace(toolType)
		if toolType != "function" {
			validTools = append(validTools, toolMap)
			continue
		}

		// OpenAI Responses-style tools use top-level name/parameters.
		if name, ok := toolMap["name"].(string); ok && strings.TrimSpace(name) != "" {
			validTools = append(validTools, toolMap)
			continue
		}

		// ChatCompletions-style tools use {type:"function", function:{...}}.
		functionValue, hasFunction := toolMap["function"]
		function, ok := functionValue.(map[string]any)
		if !hasFunction || functionValue == nil || !ok || function == nil {
			// Drop invalid function tools.
			modified = true
			continue
		}

		if _, ok := toolMap["name"]; !ok {
			if name, ok := function["name"].(string); ok && strings.TrimSpace(name) != "" {
				toolMap["name"] = name
				modified = true
			}
		}
		if _, ok := toolMap["description"]; !ok {
			if desc, ok := function["description"].(string); ok && strings.TrimSpace(desc) != "" {
				toolMap["description"] = desc
				modified = true
			}
		}
		if _, ok := toolMap["parameters"]; !ok {
			if params, ok := function["parameters"]; ok {
				toolMap["parameters"] = params
				modified = true
			}
		}
		if _, ok := toolMap["strict"]; !ok {
			if strict, ok := function["strict"]; ok {
				toolMap["strict"] = strict
				modified = true
			}
		}

		validTools = append(validTools, toolMap)
	}

	if modified {
		reqBody["tools"] = validTools
	}

	return modified
}
