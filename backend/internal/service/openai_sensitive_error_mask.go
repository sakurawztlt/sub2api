package service

import (
	"net/http"
	"strings"
)

const openAICompatSensitiveBackendErrorMessage = "Network error. Please retry."

func openAICompatSensitiveBackendErrorBody() []byte {
	return []byte(`{"error":{"message":"` + openAICompatSensitiveBackendErrorMessage + `","type":"api_error"},"type":"error"}`)
}

func sanitizeOpenAICompatFailoverError(statusCode int, upstreamMsg string, body []byte, account *Account) (int, []byte) {
	if isOpenAIOAuthSensitiveBackendError(account, statusCode, upstreamMsg, body) {
		return http.StatusBadGateway, openAICompatSensitiveBackendErrorBody()
	}
	return statusCode, body
}

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
