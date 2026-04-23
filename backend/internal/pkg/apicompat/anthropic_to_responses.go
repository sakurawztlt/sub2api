package apicompat

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	openAINameMaxLen   = 64
	openAINameFallback = "unknown_tool"
)

// reasoningSummaryGateEnabled reports whether Summary emission should be
// gated on the client's Anthropic `thinking` field. Default: off (Summary
// stays unconditionally "auto", preserving historical behaviour for 97.9%
// of B-track traffic). Flip the env var to "1" on a single environment
// first (test2) and observe for 24h before rolling to prod, because the
// change hides reasoning-summary thinking blocks from non-thinking
// clients — matches real Anthropic behaviour but is a visible output-
// shape change.
func reasoningSummaryGateEnabled() bool {
	return os.Getenv("SUB2API_GATE_REASONING_SUMMARY") == "1"
}

// AnthropicToResponses converts an Anthropic Messages request directly into
// a Responses API request. This preserves fields that would be lost in a
// Chat Completions intermediary round-trip (e.g. thinking, cache_control,
// structured system prompts).
func AnthropicToResponses(req *AnthropicRequest) (*ResponsesRequest, error) {
	input, err := convertAnthropicToResponsesInput(req.System, req.Messages)
	if err != nil {
		return nil, err
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}

	out := &ResponsesRequest{
		Model:       req.Model,
		Input:       inputJSON,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
		Include:     []string{"reasoning.encrypted_content"},
	}

	storeFalse := false
	out.Store = &storeFalse

	if req.MaxTokens > 0 {
		v := req.MaxTokens
		if v < minMaxOutputTokens {
			v = minMaxOutputTokens
		}
		out.MaxOutputTokens = &v
	}

	if len(req.Tools) > 0 {
		out.Tools = convertAnthropicToolsToResponses(req.Tools)
	}

	// Determine reasoning effort: only output_config.effort controls the
	// level; thinking.type is ignored. Default is high when unset (both
	// Anthropic and OpenAI default to high).
	// Anthropic levels map 1:1 to OpenAI: low→low, medium→medium, high→high, max→xhigh.
	effort := "high" // default → both sides' default
	if req.OutputConfig != nil && req.OutputConfig.Effort != "" {
		effort = req.OutputConfig.Effort
	}
	// Summary emission: default "auto" (historical behaviour). When the
	// opt-in env gate is on AND the client did not enable thinking, set
	// Summary="" so Codex stops emitting reasoning_summary_text. That text
	// otherwise becomes an Anthropic thinking block and inflates the
	// visible portion of output_tokens (see customer bug 2026-04-21:
	// output_tokens=198 on ~10 tokens of actual text). Hidden chain-of-
	// thought is already subtracted via visibleOutputTokens regardless.
	summary := "auto"
	if reasoningSummaryGateEnabled() {
		if req.Thinking == nil || req.Thinking.Type == "" || req.Thinking.Type == "disabled" {
			summary = ""
		}
	}
	out.Reasoning = &ResponsesReasoning{
		Effort:  mapAnthropicEffortToResponses(effort),
		Summary: summary,
	}

	// Convert tool_choice
	if len(req.ToolChoice) > 0 {
		tc, err := convertAnthropicToolChoiceToResponses(req.ToolChoice)
		if err != nil {
			return nil, fmt.Errorf("convert tool_choice: %w", err)
		}
		out.ToolChoice = tc
	}

	return out, nil
}

// convertAnthropicToolChoiceToResponses maps Anthropic tool_choice to Responses format.
//
//	{"type":"auto"}            → "auto"
//	{"type":"any"}             → "required"
//	{"type":"none"}            → "none"
//	{"type":"tool","name":"X"} → {"type":"function","name":"X"}
func convertAnthropicToolChoiceToResponses(raw json.RawMessage) (json.RawMessage, error) {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, err
	}

	switch tc.Type {
	case "auto":
		return json.Marshal("auto")
	case "any":
		return json.Marshal("required")
	case "none":
		return json.Marshal("none")
	case "tool":
		return json.Marshal(map[string]string{
			"type": "function",
			"name": sanitizeOpenAIName(tc.Name),
		})
	default:
		// Pass through unknown types as-is
		return raw, nil
	}
}

// convertAnthropicToResponsesInput builds the Responses API input items array
// from the Anthropic system field and message list.
func convertAnthropicToResponsesInput(system json.RawMessage, msgs []AnthropicMessage) ([]ResponsesInputItem, error) {
	var out []ResponsesInputItem

	// System prompt → system role input item.
	if len(system) > 0 {
		sysText, err := parseAnthropicSystemPrompt(system)
		if err != nil {
			return nil, err
		}
		if sysText != "" {
			content, _ := json.Marshal(sysText)
			out = append(out, ResponsesInputItem{
				Role:    "system",
				Content: content,
			})
		}
	}

	for _, m := range msgs {
		items, err := anthropicMsgToResponsesItems(m)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}

	// Defensive: synthesize placeholder function_call items for any
	// function_call_output items that have no matching preceding function_call
	// in the same request. This happens when a client's conversation history
	// carries a tool_result whose tool_use_id does not match any tool_use
	// earlier in the history (orphaned result — e.g. the client crossed a
	// session boundary, or a prior request returned a tool_use that was lost).
	// Without a synthesized placeholder, upstream OpenAI rejects with:
	//
	//	400 No tool call found for function call output with call_id X
	//
	// (observed >1600 times / 3 days on production NewAPI channel 7, single
	// client sending stale tool_result ids from a prior Claude-direct session).
	out = synthesizeMissingFunctionCalls(out)

	return out, nil
}

// synthesizeMissingFunctionCalls walks the input items in order, tracks the
// set of already-emitted function_call call_ids, and for every
// function_call_output whose call_id has not been seen, prepends a
// placeholder function_call with the same call_id immediately before the
// output. This satisfies OpenAI Responses API's strict "every
// function_call_output must reference a preceding function_call" validation.
//
// The placeholder uses a neutral name/arguments that cannot be mistaken for
// a real tool invocation. The model sees a completed tool call cycle
// (phantom call → result) and can reason about it as context.
func synthesizeMissingFunctionCalls(items []ResponsesInputItem) []ResponsesInputItem {
	emitted := make(map[string]bool, len(items))
	for _, it := range items {
		if it.Type == "function_call" && it.CallID != "" {
			emitted[it.CallID] = true
		}
	}
	// Short-circuit: if every function_call_output already has a match, no change.
	needsSynthesis := false
	for _, it := range items {
		if it.Type == "function_call_output" && it.CallID != "" && !emitted[it.CallID] {
			needsSynthesis = true
			break
		}
	}
	if !needsSynthesis {
		return items
	}

	out := make([]ResponsesInputItem, 0, len(items)+4)
	seen := make(map[string]bool, len(emitted))
	for k, v := range emitted {
		seen[k] = v
	}
	for _, it := range items {
		if it.Type == "function_call_output" && it.CallID != "" && !seen[it.CallID] {
			out = append(out, ResponsesInputItem{
				Type:      "function_call",
				CallID:    it.CallID,
				Name:      "orphan_tool_call_placeholder",
				Arguments: "{}",
			})
			seen[it.CallID] = true
		}
		out = append(out, it)
	}
	return out
}

// parseAnthropicSystemPrompt handles the Anthropic system field which can be
// a plain string or an array of text blocks.
func parseAnthropicSystemPrompt(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", err
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// anthropicMsgToResponsesItems converts a single Anthropic message into one
// or more Responses API input items.
func anthropicMsgToResponsesItems(m AnthropicMessage) ([]ResponsesInputItem, error) {
	switch m.Role {
	case "user":
		return anthropicUserToResponses(m.Content)
	case "assistant":
		return anthropicAssistantToResponses(m.Content)
	default:
		return anthropicUserToResponses(m.Content)
	}
}

// anthropicUserToResponses handles an Anthropic user message. Content can be a
// plain string or an array of blocks. tool_result blocks are extracted into
// function_call_output items. Image blocks are converted to input_image parts.
func anthropicUserToResponses(raw json.RawMessage) ([]ResponsesInputItem, error) {
	// Try plain string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		content, _ := json.Marshal(s)
		return []ResponsesInputItem{{Role: "user", Content: content}}, nil
	}

	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}

	var out []ResponsesInputItem
	var toolResultImageParts []ResponsesContentPart

	// Extract tool_result blocks → function_call_output items.
	// Images inside tool_results are extracted separately because the
	// Responses API function_call_output.output only accepts strings.
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		outputText, imageParts := convertToolResultOutput(b)
		out = append(out, ResponsesInputItem{
			Type:   "function_call_output",
			CallID: toResponsesCallID(b.ToolUseID),
			Output: outputText,
		})
		toolResultImageParts = append(toolResultImageParts, imageParts...)
	}

	// Remaining text + image + document blocks → user message with content parts.
	// Also include images/documents extracted from tool_results so the model can see them.
	var parts []ResponsesContentPart
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, ResponsesContentPart{Type: "input_text", Text: b.Text})
			}
		case "image":
			if uri := anthropicImageToDataURI(b.Source); uri != "" {
				parts = append(parts, ResponsesContentPart{Type: "input_image", ImageURL: uri})
			}
		case "document":
			parts = append(parts, convertAnthropicDocumentBlock(b)...)
		}
	}
	parts = append(parts, toolResultImageParts...)

	if len(parts) > 0 {
		content, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		out = append(out, ResponsesInputItem{Role: "user", Content: content})
	}

	return out, nil
}

// anthropicAssistantToResponses handles an Anthropic assistant message.
// Text content → assistant message with output_text parts.
// tool_use blocks → function_call items.
// thinking blocks → ignored (OpenAI doesn't accept them as input).
func anthropicAssistantToResponses(raw json.RawMessage) ([]ResponsesInputItem, error) {
	// Try plain string. An empty string MUST be skipped entirely: emitting
	// {Type:"output_text", Text:""} serializes to {"type":"output_text"}
	// (the Text field has `omitempty`), which the Responses upstream rejects
	// as `Missing required parameter: 'input[N].content[0].text'`. Observed
	// ~500+ times/hour on backup prod channel 15 before this guard.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		parts := []ResponsesContentPart{{Type: "output_text", Text: s}}
		partsJSON, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		return []ResponsesInputItem{{Role: "assistant", Content: partsJSON}}, nil
	}

	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}

	var items []ResponsesInputItem

	// Text content → assistant message with output_text content parts.
	text := extractAnthropicTextFromBlocks(blocks)
	if text != "" {
		parts := []ResponsesContentPart{{Type: "output_text", Text: text}}
		partsJSON, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		items = append(items, ResponsesInputItem{Role: "assistant", Content: partsJSON})
	}

	// tool_use → function_call items.
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		args := "{}"
		if len(b.Input) > 0 {
			args = string(b.Input)
		}
		fcID := toResponsesCallID(b.ID)
		items = append(items, ResponsesInputItem{
			Type:      "function_call",
			CallID:    fcID,
			Name:      sanitizeOpenAIName(b.Name),
			Arguments: args,
		})
	}

	return items, nil
}

// sanitizeOpenAIName preserves already-valid OpenAI names verbatim and
// rewrites only illegal names into the narrow ASCII charset that Responses
// accepts for tool/function names.
func sanitizeOpenAIName(name string) string {
	if isValidOpenAIName(name) {
		return name
	}

	var b strings.Builder
	b.Grow(openAINameMaxLen)
	for _, r := range name {
		if b.Len() >= openAINameMaxLen {
			break
		}
		if isAllowedOpenAINameRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}

	if b.Len() == 0 {
		return openAINameFallback
	}
	return b.String()
}

func isValidOpenAIName(name string) bool {
	if name == "" || len(name) > openAINameMaxLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isAllowedOpenAINameByte(name[i]) {
			return false
		}
	}
	return true
}

func isAllowedOpenAINameRune(r rune) bool {
	return r <= 0x7f && isAllowedOpenAINameByte(byte(r))
}

func isAllowedOpenAINameByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '_' || b == '-':
		return true
	default:
		return false
	}
}

// toResponsesCallID converts an Anthropic tool ID (toolu_xxx / call_xxx) to a
// Responses API function_call ID that starts with "fc_".
func toResponsesCallID(id string) string {
	if strings.HasPrefix(id, "fc_") {
		return id
	}
	return "fc_" + id
}

// fromResponsesCallID reverses toResponsesCallID, stripping the "fc_" prefix
// that was added during request conversion.
func fromResponsesCallID(id string) string {
	if after, ok := strings.CutPrefix(id, "fc_"); ok {
		// Only strip if the remainder doesn't look like it was already "fc_" prefixed.
		// E.g. "fc_toolu_xxx" → "toolu_xxx", "fc_call_xxx" → "call_xxx"
		if strings.HasPrefix(after, "toolu_") || strings.HasPrefix(after, "call_") {
			return after
		}
	}
	return id
}

// anthropicImageToDataURI converts an AnthropicImageSource to a data URI string.
// Returns "" if the source is nil or has no data.
func anthropicImageToDataURI(src *AnthropicImageSource) string {
	if src == nil || src.Data == "" {
		return ""
	}
	mediaType := src.MediaType
	if mediaType == "" {
		mediaType = "image/png"
	}
	return "data:" + mediaType + ";base64," + src.Data
}

// convertToolResultOutput extracts text and image content from a tool_result
// block. Returns the text as a string for the function_call_output Output
// field, plus any image parts that must be sent in a separate user message
// (the Responses API output field only accepts strings).
func convertToolResultOutput(b AnthropicContentBlock) (string, []ResponsesContentPart) {
	if len(b.Content) == 0 {
		return "(empty)", nil
	}

	// Try plain string content.
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		if s == "" {
			s = "(empty)"
		}
		return s, nil
	}

	// Array of content blocks — may contain text and/or images.
	var inner []AnthropicContentBlock
	if err := json.Unmarshal(b.Content, &inner); err != nil {
		return "(empty)", nil
	}

	// Separate text (for function_call_output) from non-text parts (images/
	// documents, which go into a follow-up user message because
	// function_call_output.output is string-only).
	var textParts []string
	var imageParts []ResponsesContentPart
	for _, ib := range inner {
		switch ib.Type {
		case "text":
			if ib.Text != "" {
				textParts = append(textParts, ib.Text)
			}
		case "image":
			if uri := anthropicImageToDataURI(ib.Source); uri != "" {
				imageParts = append(imageParts, ResponsesContentPart{Type: "input_image", ImageURL: uri})
			}
		case "document":
			imageParts = append(imageParts, convertAnthropicDocumentBlock(ib)...)
		}
	}

	text := strings.Join(textParts, "\n\n")
	if text == "" {
		text = "(empty)"
	}
	return text, imageParts
}

// extractAnthropicTextFromBlocks joins all text blocks, ignoring thinking/
// tool_use/tool_result blocks.
func extractAnthropicTextFromBlocks(blocks []AnthropicContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// mapAnthropicEffortToResponses converts Anthropic reasoning effort levels to
// OpenAI Responses API effort levels.
//
// Both APIs default to "high". The mapping is 1:1 for shared levels;
// only Anthropic's "max" (Opus 4.6 exclusive) maps to OpenAI's "xhigh"
// (GPT-5.2+ exclusive) as both represent the highest reasoning tier.
//
//	low    → low
//	medium → medium
//	high   → high
//	max    → xhigh
func mapAnthropicEffortToResponses(effort string) string {
	if effort == "max" {
		return "xhigh"
	}
	return effort // low→low, medium→medium, high→high, unknown→passthrough
}

// convertAnthropicToolsToResponses maps Anthropic tool definitions to
// Responses API tools.
//
//  1. Regular (function) tools → Responses function tools with their schema.
//  2. Anthropic `web_search_*` (e.g. web_search_20250305) → sub2api Codex
//     hosted `web_search` tool. The Codex wire protocol uses the bare name
//     `web_search`, confirmed by:
//     - types.go:212 ResponsesTool.Type comment listing `"web_search"`
//     - responses_to_anthropic_request.go:432 reverse mapping
//     `web_search` → `web_search_20250305`
//     - responses_to_anthropic.go:57 output-item handler for
//     `web_search_call` → server_tool_use + web_search_tool_result
//     Earlier attempts that emitted `{"type":"web_search_preview"}` (the
//     OpenAI public Responses API name) were rejected by Codex upstream
//     with "Unsupported tool type" — Codex uses the bare name.
//  3. Other Anthropic server-side tools (`computer_*`, `text_editor_*`,
//     `bash_*`) are still DROPPED because sub2api's Codex path has no
//     matching hosted tool in the Responses output side.
func convertAnthropicToolsToResponses(tools []AnthropicTool) []ResponsesTool {
	var out []ResponsesTool
	for _, t := range tools {
		if strings.HasPrefix(t.Type, "web_search") {
			out = append(out, ResponsesTool{Type: "web_search"})
			continue
		}
		if isAnthropicDroppedServerTool(t.Type) {
			continue
		}
		out = append(out, ResponsesTool{
			Type:        "function",
			Name:        sanitizeOpenAIName(t.Name),
			Description: t.Description,
			Parameters:  normalizeToolParameters(t.InputSchema),
		})
	}
	return out
}

// isAnthropicDroppedServerTool reports whether a tool Type is an Anthropic
// server-side hosted tool that has NO equivalent in OpenAI Responses API and
// must be silently dropped at conversion time. `web_search_*` is handled
// separately by convertAnthropicToolsToResponses and is NOT included here.
func isAnthropicDroppedServerTool(toolType string) bool {
	if toolType == "" {
		return false
	}
	switch {
	case strings.HasPrefix(toolType, "computer_"),
		strings.HasPrefix(toolType, "text_editor_"),
		strings.HasPrefix(toolType, "bash_"):
		return true
	}
	return false
}

// normalizeToolParameters ensures the tool parameter schema is valid for
// OpenAI's Responses API, which requires "properties" on object schemas.
//
//   - nil/empty → {"type":"object","properties":{}}
//   - type=object without properties → adds "properties": {}
//   - otherwise → returned unchanged
func normalizeToolParameters(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 || string(schema) == "null" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(schema, &m); err != nil {
		return schema
	}

	typ := m["type"]
	if string(typ) != `"object"` {
		return schema
	}

	if _, ok := m["properties"]; ok {
		return schema
	}

	m["properties"] = json.RawMessage(`{}`)
	out, err := json.Marshal(m)
	if err != nil {
		return schema
	}
	return out
}

// convertAnthropicDocumentBlock maps an Anthropic `document` content block
// to one or more Responses API content parts. Anthropic documents have four
// source sub-types; each maps to the closest OpenAI Responses representation:
//
//   - source.type=base64  → input_file with data URI (PDF, docx, etc.)
//   - source.type=text    → input_text inlined (optionally prefixed with a
//     title/context header so the model sees the boundary)
//   - source.type=content → recursively expand nested text/image blocks
//   - source.type=url     → input_text placeholder (OpenAI Responses API
//     does not accept URL-referenced files as input)
//
// Returns nil when the source is missing or has no usable data; caller
// should treat that as "drop the block".
func convertAnthropicDocumentBlock(b AnthropicContentBlock) []ResponsesContentPart {
	if b.Source == nil {
		return nil
	}
	src := b.Source
	switch src.Type {
	case "base64":
		if src.Data == "" {
			return nil
		}
		mediaType := src.MediaType
		if mediaType == "" {
			mediaType = "application/pdf"
		}
		// Downgrade MIME types that OpenAI's Responses API rejects in its
		// input_file allowlist (e.g. application/xml, text/tab-separated-values)
		// to an equivalent type it accepts. See downgradeFileMediaType for the
		// full map and rationale.
		mediaType = downgradeFileMediaType(mediaType)
		filename := b.Title
		if filename == "" {
			filename = defaultFilenameForMediaType(mediaType)
		}
		return []ResponsesContentPart{{
			Type:     "input_file",
			Filename: filename,
			FileData: "data:" + mediaType + ";base64," + src.Data,
		}}

	case "text":
		if src.Data == "" {
			return nil
		}
		// Always wrap with an opening header and closing marker so the model
		// sees a clear document boundary even when title/context are absent;
		// otherwise the raw text looks like a continuation of the user prompt.
		return []ResponsesContentPart{{
			Type: "input_text",
			Text: documentHeader(b.Title, b.Context) + src.Data + "\n[/Document]",
		}}

	case "content":
		if len(src.Content) == 0 {
			return nil
		}
		var inner []AnthropicContentBlock
		if err := json.Unmarshal(src.Content, &inner); err != nil {
			return nil
		}
		out := []ResponsesContentPart{
			{Type: "input_text", Text: documentHeader(b.Title, b.Context)},
		}
		for _, ib := range inner {
			switch ib.Type {
			case "text":
				if ib.Text != "" {
					out = append(out, ResponsesContentPart{Type: "input_text", Text: ib.Text})
				}
			case "image":
				if uri := anthropicImageToDataURI(ib.Source); uri != "" {
					out = append(out, ResponsesContentPart{Type: "input_image", ImageURL: uri})
				}
			}
		}
		// Drop if only the header was emitted (no usable inner content).
		if len(out) <= 1 {
			return nil
		}
		out = append(out, ResponsesContentPart{Type: "input_text", Text: "[/Document]"})
		return out

	case "url":
		if src.URL == "" {
			return nil
		}
		title := b.Title
		if title == "" {
			title = "document"
		}
		return []ResponsesContentPart{{
			Type: "input_text",
			Text: "[Document reference: " + title + " (" + src.URL + ")]",
		}}
	}
	return nil
}

// documentHeader builds an inline header describing the document title and
// context. Always returns a non-empty prefix so the model can distinguish
// document content from surrounding user text, even when title/context are
// both empty.
func documentHeader(title, context string) string {
	var sb strings.Builder
	sb.WriteString("[Document")
	if title != "" {
		sb.WriteString(": ")
		sb.WriteString(title)
	}
	if context != "" {
		sb.WriteString(" — ")
		sb.WriteString(context)
	}
	sb.WriteString("]\n\n")
	return sb.String()
}

// defaultFilenameForMediaType returns a reasonable filename with extension
// for a given document MIME type, used when the document block has no title.
func defaultFilenameForMediaType(mediaType string) string {
	switch mediaType {
	case "application/pdf":
		return "document.pdf"
	case "text/plain":
		return "document.txt"
	case "text/markdown":
		return "document.md"
	case "text/csv":
		return "document.csv"
	case "text/xml":
		return "document.xml"
	case "text/html":
		return "document.html"
	case "application/json":
		return "document.json"
	case "application/yaml":
		return "document.yaml"
	case "application/msword":
		return "document.doc"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return "document.docx"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return "document.xlsx"
	}
	return "document"
}

// downgradeFileMediaType maps MIME types that the OpenAI Responses API's
// input_file allowlist rejects (or accepts but does not parse) to an
// equivalent MIME type that works end-to-end.
//
// Source of truth: live probing against gpt-5.x on 2026-04-13 through
// gcr → sub2api → OpenAI Responses. For each mapped type the downgrade
// was verified to return the marker embedded in the test file:
//
//   - application/xml → text/xml
//     Upstream rejects application/xml with "unsupported MIME type" but
//     accepts and parses text/xml. File contents stay byte-identical.
//
//   - text/tab-separated-values → text/plain
//     Upstream rejects the TSV MIME. TSV is valid UTF-8 plain text so the
//     model can still extract tab-delimited values when the bytes come in
//     as text/plain.
//
//   - application/rtf / text/rtf → text/plain
//     Upstream ACCEPTS either RTF MIME but silently does not parse the
//     content — the model sees no attachment. RTF is ASCII-compatible
//     enough that forwarding the raw bytes as text/plain lets the model
//     read the visible strings and ignore \rtf control words; verified
//     with markers embedded in the body.
//
// All other MIME types pass through unchanged. Unsupported types not on
// this list will still hit upstream and return a proper 400 that now
// propagates to the client (see handleCompatErrorResponse).
func downgradeFileMediaType(mediaType string) string {
	switch mediaType {
	case "application/xml":
		return "text/xml"
	case "text/tab-separated-values":
		return "text/plain"
	case "application/rtf", "text/rtf":
		return "text/plain"
	}
	return mediaType
}
