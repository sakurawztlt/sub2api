package service

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// openAIStreamCredentialAuthFailure distinguishes credential failures from
// request/content permission denials carried inside an HTTP 200 stream.
func openAIStreamCredentialAuthFailure(payload []byte) bool {
	if len(bytes.TrimSpace(payload)) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	for _, path := range []string{"response.error.status_code", "error.status_code", "status_code"} {
		if int(gjson.GetBytes(payload, path).Int()) == http.StatusUnauthorized {
			return true
		}
	}
	for _, path := range []string{"response.error.type", "error.type", "type"} {
		errType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, path).String()))
		if errType == "authentication_error" || errType == "authentication_failed" || errType == "unauthorized_error" {
			return true
		}
	}
	for _, path := range []string{"response.error.code", "error.code", "code"} {
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, path).String())) {
		case "invalid_api_key", "api_key_disabled", "unauthorized", "authentication_error",
			"invalid_token", "access_token_invalid", "token_revoked", "token_invalidated",
			"invalid_credentials", "credential_invalid":
			return true
		}
	}
	return false
}

func openAIStream403AccountFailure(payload []byte, message string) bool {
	return isOpenAIUpstreamAccessStateError(message, payload) || openAIStreamCredentialAuthFailure(payload)
}

func openAIStreamErrorEventShouldFailover(payload []byte, message string) bool {
	if hit, _, _ := detectOpenAICyberPolicy(payload); hit {
		return false
	}
	if isOpenAIContextWindowError(message, payload) {
		return false
	}
	if isOpenAIUpstreamAccessStateError(message, payload) {
		return true
	}
	switch openAIStreamFailedEventSemanticStatus(payload, message) {
	case http.StatusForbidden:
		return openAIStream403AccountFailure(payload, message)
	case http.StatusUnauthorized, http.StatusTooManyRequests, 529:
		return true
	}
	if isOpenAITransientProcessingError(http.StatusBadRequest, message, payload) {
		return true
	}
	combined := strings.ToLower(strings.TrimSpace(message + " " +
		gjson.GetBytes(payload, "error.message").String() + " " +
		gjson.GetBytes(payload, "response.error.message").String()))
	return strings.Contains(combined, "temporary") ||
		strings.Contains(combined, "try again") ||
		strings.Contains(combined, "please retry")
}

func (s *OpenAIGatewayService) handleOpenAIStreamTerminalAccountSideEffects(
	c *gin.Context,
	account *Account,
	payload []byte,
	message string,
	headers http.Header,
	canonicalModel ...string,
) (int, bool) {
	statusCode := openAIStreamFailureStatus(payload, message)
	switch statusCode {
	case http.StatusForbidden:
		if !openAIStream403AccountFailure(payload, message) {
			return statusCode, false
		}
		fallthrough
	case http.StatusUnauthorized, http.StatusTooManyRequests, 529:
		ctx := context.Background()
		if c != nil && c.Request != nil {
			ctx = c.Request.Context()
		}
		model := firstNonEmpty(canonicalModel...)
		if model == "" {
			model = firstNonEmpty(gjson.GetBytes(payload, "model").String(), gjson.GetBytes(payload, "response.model").String())
		}
		accountHeaders := headers
		if statusCode == http.StatusTooManyRequests {
			accountHeaders = openAIWSSemantic429Headers(account, model, headers)
		}
		return statusCode, s.handleOpenAIAccountUpstreamError(ctx, account, statusCode, accountHeaders, payload, model)
	default:
		return statusCode, false
	}
}

func openAIWSSemantic429Headers(account *Account, model string, headers http.Header) http.Header {
	if isCodexSparkModel(model) && isOpenAIOAuthAccount(account) {
		return headers
	}
	return nil
}

// openAIResponsesCompletedEventIsEmpty reports whether a response.completed /
// response.done SSE payload carries no usage, no error and no output items.
// The accumulated usage is consulted too, because OpenAI may deliver usage on
// an earlier event. An empty terminal event after a stream with no semantic
// output is treated as a silent upstream refusal.
func openAIResponsesCompletedEventIsEmpty(data []byte, usage *OpenAIUsage) bool {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return false
	}
	if usage != nil && (usage.InputTokens > 0 || usage.OutputTokens > 0 ||
		usage.ImageInputTokens > 0 || usage.ImageOutputTokens > 0 ||
		usage.CacheCreationInputTokens > 0 || usage.CacheReadInputTokens > 0) {
		return false
	}
	if gjson.GetBytes(data, "usage").Exists() || gjson.GetBytes(data, "response.usage").Exists() {
		return false
	}
	if gjson.GetBytes(data, "error").Exists() || gjson.GetBytes(data, "response.error").Exists() {
		return false
	}
	if output := gjson.GetBytes(data, "response.output"); output.Exists() && output.IsArray() && len(output.Array()) > 0 {
		return false
	}
	return true
}
