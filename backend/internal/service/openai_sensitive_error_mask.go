package service

import "strings"

const openAICompatSensitiveBackendErrorMessage = "Network error. Please retry."

func containsOpenAICompatSensitiveBackendTerm(message string, body []byte) bool {
	combined := strings.ToLower(message + " " + string(body))
	if strings.TrimSpace(combined) == "" {
		return false
	}
	normalized := strings.NewReplacer("_", " ", "-", " ", "\n", " ", "\r", " ", "\t", " ").Replace(combined)
	normalized = strings.Join(strings.Fields(normalized), " ")

	return strings.Contains(normalized, "codex") ||
		strings.Contains(normalized, "chatgpt") ||
		strings.Contains(normalized, "gpt ") ||
		strings.Contains(combined, "gpt-") ||
		strings.Contains(combined, "gpt_") ||
		strings.Contains(combined, "5.4") ||
		strings.Contains(combined, "5.5")
}
