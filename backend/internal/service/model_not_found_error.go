package service

import (
	"net/http"
	"strings"
)

var upstreamModelNotFoundKeywords = []string{"model not found", "unknown model", "not found"}

func isUpstreamModelNotFoundError(statusCode int, body []byte) bool {
	if statusCode != http.StatusNotFound {
		return false
	}
	normalized := normalizeModelNotFoundBody(body)
	if normalized == "" || !strings.Contains(normalized, "model") {
		return false
	}
	return containsModelNotFoundKeyword(normalized)
}

func isOpenAICodexChatGPTModelUnsupportedError(statusCode int, message string, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	normalized := normalizeModelNotFoundText(message + " " + string(body))
	if normalized == "" {
		return false
	}
	return strings.Contains(normalized, "model") &&
		strings.Contains(normalized, "not supported") &&
		strings.Contains(normalized, "codex") &&
		strings.Contains(normalized, "chatgpt account")
}

func isUpstreamModelUnavailableForAccountError(statusCode int, body []byte) bool {
	return isUpstreamModelNotFoundError(statusCode, body) ||
		isOpenAICodexChatGPTModelUnsupportedError(statusCode, "", body)
}

func isModelNotFoundError(statusCode int, body []byte) bool {
	return isUpstreamModelNotFoundError(statusCode, body) || statusCode == http.StatusNotFound
}

func containsModelNotFoundKeyword(normalizedBody string) bool {
	if normalizedBody == "" {
		return false
	}
	for _, keyword := range upstreamModelNotFoundKeywords {
		if strings.Contains(normalizedBody, keyword) {
			return true
		}
	}
	return false
}

func normalizeModelNotFoundBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	return normalizeModelNotFoundText(string(body))
}

func normalizeModelNotFoundText(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	normalized := strings.ToLower(text)
	normalized = strings.NewReplacer("_", " ", "-", " ", "\n", " ", "\r", " ", "\t", " ").Replace(normalized)
	return strings.Join(strings.Fields(normalized), " ")
}
