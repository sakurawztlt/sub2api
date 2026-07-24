package service

import (
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

const (
	openAICompatClaudeCodeTodoGuardMarker = "<sub2api-claude-code-todo-guard>"
	openAICompatClaudeCodeTodoGuardText   = openAICompatClaudeCodeTodoGuardMarker + "\nWhen using Claude Code todo or task tracking tools, keep the visible task list consistent. Do not send final or summary text while any item remains in_progress. Before finishing, asking the user to choose, or reporting a blocker, update the todo list so completed work is completed and deferred work is pending/open; leave an item in_progress only when active work will continue in the same turn.\n</sub2api-claude-code-todo-guard>"

	openAICompatDeferredToolGuardMarker = "<sub2api-deferred-tool-guard>"
	openAICompatDeferredToolGuardText   = openAICompatDeferredToolGuardMarker + "\nDeferred-tool notices describe availability, not a requirement to load those tools. Call ToolSearch only when the user's task actually requires a deferred tool whose schema is not loaded. When already-loaded implementation tools such as Bash, Write, or Edit can complete the task, use them directly. Do not load task or todo tracking tools only for bookkeeping. When a task requires several independent file creation, editing, or verification steps, batch compatible operations into the same Bash call when safe, complete the requested deliverable within the available tool turns, and verify the result before replying.\n</sub2api-deferred-tool-guard>"
)

func appendOpenAICompatClaudeCodeTodoGuard(req *apicompat.ResponsesRequest) bool {
	if req == nil || len(req.Input) == 0 {
		return false
	}

	var items []apicompat.ResponsesInputItem
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return false
	}
	if len(items) == 0 || responsesInputItemsContainText(items, openAICompatClaudeCodeTodoGuardMarker) {
		return false
	}

	content, err := json.Marshal([]apicompat.ResponsesContentPart{{
		Type: "input_text",
		Text: openAICompatClaudeCodeTodoGuardText,
	}})
	if err != nil {
		return false
	}

	guard := apicompat.ResponsesInputItem{
		Type:    "message",
		Role:    "developer",
		Content: content,
	}

	insertAt := 0
	for insertAt < len(items) && items[insertAt].Type == "message" && items[insertAt].Role == "developer" {
		insertAt++
	}

	items = append(items, apicompat.ResponsesInputItem{})
	copy(items[insertAt+1:], items[insertAt:])
	items[insertAt] = guard

	input, err := json.Marshal(items)
	if err != nil {
		return false
	}
	req.Input = input
	return true
}

func appendOpenAICompatClaudeCodeTodoGuardToRequestBody(reqBody map[string]any) bool {
	if reqBody == nil {
		return false
	}

	input, ok := reqBody["input"].([]any)
	if !ok || len(input) == 0 || inputContainsText(input, openAICompatClaudeCodeTodoGuardMarker) {
		return false
	}

	guard := map[string]any{
		"type": "message",
		"role": "developer",
		"content": []any{
			map[string]any{
				"type": "input_text",
				"text": openAICompatClaudeCodeTodoGuardText,
			},
		},
	}

	insertAt := 0
	for insertAt < len(input) {
		item, ok := input[insertAt].(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyString(item["type"])) != "message" || strings.TrimSpace(firstNonEmptyString(item["role"])) != "developer" {
			break
		}
		insertAt++
	}

	input = append(input, nil)
	copy(input[insertAt+1:], input[insertAt:])
	input[insertAt] = guard
	reqBody["input"] = input
	return true
}

// applyOpenAICompatOAuthMessagesBridgeGuards adds bridge-local compatibility
// guidance while preserving deferred tools for tasks that actually need them.
func applyOpenAICompatOAuthMessagesBridgeGuards(reqBody map[string]any) {
	appendOpenAICompatClaudeCodeTodoGuardToRequestBody(reqBody)
	appendOpenAICompatDeferredToolGuardToRequestBody(reqBody)
}

func appendOpenAICompatDeferredToolGuard(req *apicompat.ResponsesRequest) bool {
	if req == nil || len(req.Input) == 0 || !responsesToolsSupportDirectImplementation(req.Tools) {
		return false
	}

	var items []apicompat.ResponsesInputItem
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return false
	}
	if len(items) == 0 ||
		responsesInputItemsContainText(items, openAICompatDeferredToolGuardMarker) ||
		!responsesInputItemsContainDeferredToolNotice(items) {
		return false
	}

	content, err := json.Marshal([]apicompat.ResponsesContentPart{{
		Type: "input_text",
		Text: openAICompatDeferredToolGuardText,
	}})
	if err != nil {
		return false
	}

	guard := apicompat.ResponsesInputItem{
		Type:    "message",
		Role:    "developer",
		Content: content,
	}
	insertAt := 0
	for insertAt < len(items) && items[insertAt].Type == "message" && items[insertAt].Role == "developer" {
		insertAt++
	}

	items = append(items, apicompat.ResponsesInputItem{})
	copy(items[insertAt+1:], items[insertAt:])
	items[insertAt] = guard

	input, err := json.Marshal(items)
	if err != nil {
		return false
	}
	req.Input = input
	return true
}

func appendOpenAICompatDeferredToolGuardToRequestBody(reqBody map[string]any) bool {
	if reqBody == nil || !requestBodyToolsSupportDirectImplementation(reqBody["tools"]) {
		return false
	}

	input, ok := reqBody["input"].([]any)
	if !ok || len(input) == 0 ||
		inputContainsText(input, openAICompatDeferredToolGuardMarker) ||
		!inputContainsDeferredToolNotice(input) {
		return false
	}

	guard := map[string]any{
		"type": "message",
		"role": "developer",
		"content": []any{
			map[string]any{
				"type": "input_text",
				"text": openAICompatDeferredToolGuardText,
			},
		},
	}

	insertAt := 0
	for insertAt < len(input) {
		item, ok := input[insertAt].(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyString(item["type"])) != "message" || strings.TrimSpace(firstNonEmptyString(item["role"])) != "developer" {
			break
		}
		insertAt++
	}

	input = append(input, nil)
	copy(input[insertAt+1:], input[insertAt:])
	input[insertAt] = guard
	reqBody["input"] = input
	return true
}

func responsesToolsSupportDirectImplementation(tools []apicompat.ResponsesTool) bool {
	hasToolSearch := false
	hasDirectTool := false
	for _, tool := range tools {
		switch strings.TrimSpace(tool.Name) {
		case "ToolSearch":
			hasToolSearch = true
		case "Bash", "Write", "Edit":
			hasDirectTool = true
		}
	}
	return hasToolSearch && hasDirectTool
}

func requestBodyToolsSupportDirectImplementation(rawTools any) bool {
	tools, ok := rawTools.([]any)
	if !ok {
		return false
	}

	hasToolSearch := false
	hasDirectTool := false
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(firstNonEmptyString(tool["name"]))
		if name == "" {
			if function, ok := tool["function"].(map[string]any); ok {
				name = strings.TrimSpace(firstNonEmptyString(function["name"]))
			}
		}
		switch name {
		case "ToolSearch":
			hasToolSearch = true
		case "Bash", "Write", "Edit":
			hasDirectTool = true
		}
	}
	return hasToolSearch && hasDirectTool
}

func responsesInputItemsContainDeferredToolNotice(items []apicompat.ResponsesInputItem) bool {
	for _, item := range items {
		if isDeferredToolNoticeText(string(item.Content)) {
			return true
		}
	}
	return false
}

func inputContainsDeferredToolNotice(input []any) bool {
	for _, item := range input {
		b, err := json.Marshal(item)
		if err == nil && isDeferredToolNoticeText(string(b)) {
			return true
		}
	}
	return false
}

func isDeferredToolNoticeText(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "deferred tools") &&
		strings.Contains(text, "toolsearch") &&
		(strings.Contains(text, "schemas are not loaded") ||
			strings.Contains(text, "available via toolsearch"))
}

func responsesInputItemsContainText(items []apicompat.ResponsesInputItem, needle string) bool {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return false
	}
	for _, item := range items {
		if containsPlainOrJSONEscapedText(string(item.Content), needle) {
			return true
		}
	}
	return false
}

func inputContainsText(input []any, needle string) bool {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return false
	}
	for _, item := range input {
		b, err := json.Marshal(item)
		if err == nil && containsPlainOrJSONEscapedText(string(b), needle) {
			return true
		}
	}
	return false
}

func containsPlainOrJSONEscapedText(haystack, needle string) bool {
	if strings.Contains(haystack, needle) {
		return true
	}
	encoded, err := json.Marshal(needle)
	if err != nil || len(encoded) < 2 {
		return false
	}
	return strings.Contains(haystack, string(encoded[1:len(encoded)-1]))
}
