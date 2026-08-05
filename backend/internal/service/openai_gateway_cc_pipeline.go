package service

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
)

// failoverOpenAIUpstreamHTTPError applies account-aware failover policy before
// any response bytes are committed. In this relay tree the Messages transport
// still lives in openai_gateway_messages.go, so this helper intentionally
// contains only the shared decision and side effects from upstream PR #5164.
func (s *OpenAIGatewayService) failoverOpenAIUpstreamHTTPError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	resp *http.Response,
	respBody []byte,
	upstreamMsg string,
	upstreamModel string,
) *UpstreamFailoverError {
	if resp == nil || account == nil {
		return nil
	}

	shouldFailover := s.shouldFailoverOpenAIUpstreamResponseForAccount(resp.StatusCode, upstreamMsg, respBody, account)
	tempUnscheduled := false
	if c != nil && account.Platform != PlatformGrok && !shouldFailover && !IsResponseCommitted(c) && s.rateLimitService != nil {
		tempUnscheduled = s.rateLimitService.CheckErrorPolicy(ctx, account, resp.StatusCode, respBody) == ErrorPolicyTempUnscheduled
		shouldFailover = tempUnscheduled
	}
	if account.Platform == PlatformGrok {
		shouldFailover = s.shouldFailoverGrokUpstreamError(resp.StatusCode, respBody)
		s.handleGrokAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
	}
	if !shouldFailover {
		return nil
	}

	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(respBody), maxBytes)
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               "failover",
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})

	if account.Platform != PlatformGrok && !tempUnscheduled {
		s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody, upstreamModel)
	}

	failoverStatus, failoverBody := sanitizeOpenAICompatFailoverError(resp.StatusCode, upstreamMsg, respBody, account)
	return &UpstreamFailoverError{
		StatusCode:             failoverStatus,
		ResponseBody:           failoverBody,
		RetryableOnSameAccount: account.IsPoolMode() && (account.IsPoolModeRetryableStatus(resp.StatusCode) || isOpenAITransientProcessingError(resp.StatusCode, upstreamMsg, respBody)),
	}
}
