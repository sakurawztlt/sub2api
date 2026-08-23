package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// buildOpenAIAuthenticationHeaders is the shared Codex HTTP authentication
// seam for the supported OAuth/PAT account modes. Agent Identity intentionally
// remains out of scope for this release.
func (s *OpenAIGatewayService) buildOpenAIAuthenticationHeaders(ctx context.Context, account *Account, token string) (http.Header, error) {
	if account == nil {
		return nil, errors.New("account is nil")
	}
	credentialAccount := account
	if account.IsShadow() {
		resolved, err := resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil {
			return nil, err
		}
		credentialAccount = resolved
	}
	if credentialAccount == nil {
		return nil, errors.New("OpenAI credential account is missing")
	}
	if strings.EqualFold(strings.TrimSpace(credentialAccount.GetCredential(openAIAuthModeCredentialKey)), "agentIdentity") ||
		strings.EqualFold(strings.TrimSpace(credentialAccount.GetCredential(openAIAuthModeLegacyCredentialKey)), "agentIdentity") {
		return nil, errors.New("OpenAI Agent Identity is not supported in this release")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("OpenAI access token is missing")
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token)
	return headers, nil
}
