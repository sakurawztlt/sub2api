package service

import (
	"context"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
)

func isOpenAICapacityShedMessage(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "server is overloaded") ||
		strings.Contains(lower, "servers are overloaded") ||
		strings.Contains(lower, "servers are currently overloaded")
}

func isOpenAIRequestScopedCapacityShed(upstreamMsg string, upstreamBody []byte) bool {
	return isOpenAIUpstreamCapacityShedEvent(upstreamBody) ||
		isOpenAICapacityShedMessage(upstreamMsg) ||
		isOpenAICapacityShedMessage(gjson.GetBytes(upstreamBody, "error.message").String()) ||
		isOpenAICapacityShedMessage(gjson.GetBytes(upstreamBody, "response.error.message").String()) ||
		(!gjson.ValidBytes(upstreamBody) && isOpenAICapacityShedMessage(string(upstreamBody)))
}

// OpenAIRequestBodyTooLargeClientMessage is the fixed downstream message used
// after all account-specific request body limit failovers are exhausted.
const OpenAIRequestBodyTooLargeClientMessage = "Request payload is too large"

const (
	openAIRequestBodyTooLargeReason   = GatewayFailureReason("openai_request_body_too_large")
	openAIRequestScopedCapacityReason = GatewayFailureReason("openai_request_scoped_capacity")
)

func isOpenAIRequestBodyTooLargeError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	return statusCode == http.StatusRequestEntityTooLarge && !isOpenAIContextWindowError(upstreamMsg, upstreamBody)
}

func newOpenAIUpstreamFailoverError(
	statusCode int,
	responseHeaders http.Header,
	responseBody []byte,
	upstreamMsg string,
	retryableOnSameAccount bool,
) *UpstreamFailoverError {
	requestScopedCapacity := isOpenAIRequestScopedCapacityShed(upstreamMsg, responseBody)
	failoverErr := &UpstreamFailoverError{
		StatusCode:             statusCode,
		ResponseBody:           responseBody,
		ResponseHeaders:        responseHeaders.Clone(),
		RetryableOnSameAccount: retryableOnSameAccount || requestScopedCapacity,
		RequestScopedTransient: requestScopedCapacity,
	}
	if isOpenAIRequestBodyTooLargeError(statusCode, upstreamMsg, responseBody) {
		failoverErr.RetryableOnSameAccount = false
		failoverErr.RequestScopedTransient = false
		failoverErr.Scope = GatewayFailureScopeAccount
		failoverErr.Reason = openAIRequestBodyTooLargeReason
		failoverErr.NextAccountAction = NextAccountRetry
		failoverErr.ClientStatusCode = http.StatusRequestEntityTooLarge
		failoverErr.ClientMessage = OpenAIRequestBodyTooLargeClientMessage
	}
	if isOpenAIHTTPUpstreamAccessStateError(statusCode, upstreamMsg, responseBody) {
		failoverErr.RetryableOnSameAccount = false
		failoverErr.RequestScopedTransient = false
		failoverErr.Stage = GatewayFailureStageAccountAuth
		failoverErr.Scope = GatewayFailureScopeAccount
		failoverErr.Reason = OpenAIUpstreamAccessStateReason
		failoverErr.NextAccountAction = NextAccountRetry
		failoverErr.ClientStatusCode = http.StatusBadGateway
		failoverErr.ClientMessage = openAIUpstreamAccessUnavailableClientMessage
	} else if requestScopedCapacity {
		failoverErr.Reason = openAIRequestScopedCapacityReason
		failoverErr.ClientStatusCode = http.StatusServiceUnavailable
		failoverErr.ClientMessage = openAICapacityShedClientMessage(upstreamMsg, responseBody)
	}
	return failoverErr
}

func (s *OpenAIGatewayService) newOpenAIAccountFailoverError(
	account *Account,
	statusCode int,
	responseHeaders http.Header,
	responseBody []byte,
	upstreamMsg string,
	shouldDisable bool,
	retryableOnSameAccount bool,
) *UpstreamFailoverError {
	return s.newOpenAIAccountFailoverErrorWithClassificationHeaders(account, statusCode, responseHeaders, responseHeaders, responseBody, upstreamMsg, shouldDisable, retryableOnSameAccount)
}

func (s *OpenAIGatewayService) newOpenAIAccountFailoverErrorWithClassificationHeaders(
	account *Account,
	statusCode int,
	responseHeaders http.Header,
	classificationHeaders http.Header,
	responseBody []byte,
	upstreamMsg string,
	shouldDisable bool,
	retryableOnSameAccount bool,
) *UpstreamFailoverError {
	oauth429Retry := s.shouldRetryOpenAIOAuth429OnSameAccountWithResponse(account, statusCode, shouldDisable, classificationHeaders, responseBody)
	failoverErr := newOpenAIUpstreamFailoverError(
		statusCode,
		responseHeaders,
		responseBody,
		upstreamMsg,
		retryableOnSameAccount || oauth429Retry,
	)
	if oauth429Retry {
		failoverErr.SameAccountRetryDeadline = s.openAIOAuth429RetryDeadline(account)
		failoverErr.SameAccountRetryDelay = openAIOAuth429SameAccountRetryDelay(responseHeaders, failoverErr.SameAccountRetryDeadline)
	}
	return failoverErr
}

const (
	openAIUpstreamAccessUnavailableClientMessage = "Upstream access is temporarily unavailable, please retry later"
	// OpenAIUpstreamAccessStateReason marks a provider credential whose
	// account, workspace, or organization is unavailable.
	OpenAIUpstreamAccessStateReason = GatewayFailureReason("openai_upstream_access_state")
	// OpenAIHTTPContinuationUnsupportedReason identifies accounts that cannot
	// preserve an official Responses HTTP continuation without dropping state.
	OpenAIHTTPContinuationUnsupportedReason = GatewayFailureReason("openai_http_continuation_unsupported")
)

// isOpenAIUpstreamAccessStateError recognizes provider-side credential state
// failures only from explicit structured codes. Free-form messages may contain
// echoed user input, including inside stream terminal error.message fields.
func isOpenAIUpstreamAccessStateError(_ string, body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	for _, path := range []string{"error.code", "response.error.code", "detail.code", "code"} {
		if isOpenAIUpstreamAccessStateCode(gjson.GetBytes(body, path).String()) {
			return true
		}
	}
	return false
}

func isOpenAIUpstreamAccessStateCode(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "deactivated_workspace" {
		return true
	}
	for _, subject := range []string{"workspace", "account", "organization", "org"} {
		for _, state := range []string{"deactivated", "disabled", "suspended"} {
			if value == subject+"_"+state || value == state+"_"+subject {
				return true
			}
		}
	}
	return false
}

// isOpenAIHTTPUpstreamAccessStateError is deliberately status-independent:
// known provider codes are durable evidence, while 401/403 messages without
// such a code must flow through the existing authentication/403 policies.
func isOpenAIHTTPUpstreamAccessStateError(_ int, _ string, body []byte) bool {
	return isOpenAIUpstreamAccessStateError("", body)
}

func openAICapacityShedClientMessage(upstreamMsg string, body []byte) string {
	for _, candidate := range []string{
		upstreamMsg,
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "response.error.message").String(),
		gjson.GetBytes(body, "message").String(),
	} {
		candidate = sanitizeUpstreamErrorMessage(strings.TrimSpace(candidate))
		if candidate != "" && isOpenAICapacityShedMessage(candidate) {
			return candidate
		}
	}
	return "Upstream service is temporarily overloaded, please retry later"
}

// IsOpenAIRequestBodyTooLarge reports whether another account may accept the
// same request even though the selected account rejected its serialized size.
func (e *UpstreamFailoverError) IsOpenAIRequestBodyTooLarge() bool {
	return e != nil && e.Reason == openAIRequestBodyTooLargeReason
}

// IsOpenAICapacityShed reports whether typed client fields were derived from a
// recognized provider overload rather than supplied by an unrelated failure.
func (e *UpstreamFailoverError) IsOpenAICapacityShed() bool {
	return e != nil && e.RequestScopedTransient &&
		(e.Reason == openAIRequestScopedCapacityReason || isOpenAIRequestScopedCapacityShed("", e.ResponseBody))
}

// invalidNonStreamingJSONFailoverError normalizes a non-JSON 2xx upstream
// response into a failover error shared by Anthropic passthrough paths.
func invalidNonStreamingJSONFailoverError(
	ctx context.Context,
	rateLimitService *RateLimitService,
	resp *http.Response,
	account *Account,
	body []byte,
	parseErr error,
	requestedModel ...string,
) error {
	const statusCode = http.StatusBadGateway

	accountID := int64(0)
	accountName := ""
	retryableOnSameAccount := false
	if account != nil {
		accountID = account.ID
		accountName = account.Name
		retryableOnSameAccount = account.IsPoolMode() && account.IsPoolModeRetryableStatus(statusCode)
	}

	logger.LegacyPrintf(
		"service.gateway",
		"Account %d(%s): upstream returned non-JSON 2xx response, attempting failover: status=%d request_id=%s error=%v",
		accountID,
		accountName,
		resp.StatusCode,
		resp.Header.Get("x-request-id"),
		parseErr,
	)

	if rateLimitService != nil && account != nil {
		if len(requestedModel) > 0 {
			rateLimitService.HandleUpstreamError(ctx, account, statusCode, resp.Header, body, requestedModel[0])
		} else {
			rateLimitService.HandleUpstreamError(ctx, account, statusCode, resp.Header, body)
		}
	}

	return &UpstreamFailoverError{
		StatusCode:             statusCode,
		ResponseBody:           body,
		ResponseHeaders:        resp.Header,
		RetryableOnSameAccount: retryableOnSameAccount,
	}
}
