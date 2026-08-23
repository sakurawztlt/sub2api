package service

import (
	"context"
	"strings"
)

// redactAgentIdentitySensitiveBody removes credential values before an
// upstream error can reach logs, ops events, or returned error text.
func redactAgentIdentitySensitiveBodyForAccount(ctx context.Context, repo AccountRepository, account *Account, body []byte) []byte {
	if account == nil || len(body) == 0 {
		return body
	}
	_ = ctx
	_ = repo
	credAccount := account
	redacted := string(body)
	for _, key := range []string{
		"agent_private_key",
		"agent_runtime_id",
		"task_id",
		"access_token",
		"refresh_token",
		"id_token",
		"api_key",
		"session_key",
		"cookie",
	} {
		if value := strings.TrimSpace(credAccount.GetCredential(key)); value != "" {
			redacted = strings.ReplaceAll(redacted, value, "[redacted]")
		}
	}
	const assertionPrefix = "AgentAssertion "
	for offset := 0; offset < len(redacted); {
		relativeStart := strings.Index(redacted[offset:], assertionPrefix)
		if relativeStart < 0 {
			break
		}
		start := offset + relativeStart
		valueStart := start + len(assertionPrefix)
		end := valueStart
		for end < len(redacted) && !strings.ContainsRune(" \t\r\n\"',}", rune(redacted[end])) {
			end++
		}
		redacted = redacted[:valueStart] + "[redacted]" + redacted[end:]
		offset = valueStart + len("[redacted]")
	}
	return []byte(redacted)
}

func (s *OpenAIGatewayService) redactAgentIdentitySensitiveBody(ctx context.Context, account *Account, body []byte) []byte {
	if s == nil || account == nil {
		return body
	}
	return redactAgentIdentitySensitiveBodyForAccount(ctx, s.accountRepo, account, body)
}
