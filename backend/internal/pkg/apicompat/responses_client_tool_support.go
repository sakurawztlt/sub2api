package apicompat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// NamespacedToolName records the original identity of a flattened namespace
// function so native Responses payloads can be restored before returning.
type NamespacedToolName struct {
	Namespace string
	Name      string
}

const (
	responsesToolNameMaxLen = 64
	toolSearchProxyName     = "tool_search"
	customToolInputSchema   = `{"type":"object","properties":{"input":{"type":"string","description":"The raw input for this tool, passed through verbatim."}},"required":["input"]}`
	toolSearchProxySchema   = `{"type":"object","properties":{"query":{"type":"string","description":"Search query for tools or connectors to load."},"limit":{"type":"integer","description":"Maximum number of tool groups to return."}},"required":["query"]}`
)

func flattenNamespaceToolName(namespace, name string) string {
	full := namespace + "__" + name
	if len(full) <= responsesToolNameMaxLen {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "__" + hex.EncodeToString(sum[:4])
	prefixLen := responsesToolNameMaxLen - len(suffix)
	var prefix strings.Builder
	for _, ch := range full {
		encoded := string(ch)
		if prefix.Len()+len(encoded) > prefixLen {
			break
		}
		_, _ = prefix.WriteString(encoded)
	}
	return prefix.String() + suffix
}

func extractCustomToolCallInput(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return ""
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &object); err != nil {
		return trimmed
	}
	if raw, ok := object["input"]; ok {
		var input string
		if err := json.Unmarshal(raw, &input); err == nil {
			return input
		}
		return trimmed
	}
	if len(object) == 0 {
		return ""
	}
	return trimmed
}

func toolSearchCallArgumentsJSON(arguments string) json.RawMessage {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	fallback, _ := json.Marshal(arguments)
	return fallback
}
