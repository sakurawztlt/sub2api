package service

import "testing"

// codex 2026-05-16 account-124 incident: OpenAI OAuth account that
// suddenly returns 404 on /v1/messages requests (Codex backend feature/
// org-binding revoked) was leaking "Upstream error: 404" to clients.
// Fix: shouldFailoverUpstreamErrorForAccount returns true for 404 on
// OAuth accounts so the request retries against the next account.

func TestShouldFailoverUpstreamErrorForAccount_404OAuthFailover(t *testing.T) {
	s := &OpenAIGatewayService{}
	acc := &Account{Type: AccountTypeOAuth}
	if !s.shouldFailoverUpstreamErrorForAccount(404, acc) {
		t.Errorf("404 on OAuth account MUST trigger failover (account-scoped)")
	}
}

func TestShouldFailoverUpstreamErrorForAccount_404APIKeyDoesNotFailover(t *testing.T) {
	s := &OpenAIGatewayService{}
	acc := &Account{Type: AccountTypeAPIKey}
	if s.shouldFailoverUpstreamErrorForAccount(404, acc) {
		t.Errorf("404 on API-key account should NOT trigger failover (likely client typo on model)")
	}
}

func TestShouldFailoverUpstreamErrorForAccount_404NilAccountDoesNotFailover(t *testing.T) {
	s := &OpenAIGatewayService{}
	if s.shouldFailoverUpstreamErrorForAccount(404, nil) {
		t.Errorf("404 with nil account should NOT trigger failover")
	}
}

func TestShouldFailoverUpstreamErrorForAccount_PreservesExistingStatusFailover(t *testing.T) {
	s := &OpenAIGatewayService{}
	acc := &Account{Type: AccountTypeOAuth}
	for _, code := range []int{401, 402, 403, 429, 529, 500, 502, 503, 504} {
		if !s.shouldFailoverUpstreamErrorForAccount(code, acc) {
			t.Errorf("status %d should still trigger failover (existing rule)", code)
		}
	}
	for _, code := range []int{200, 400, 405, 422} {
		if s.shouldFailoverUpstreamErrorForAccount(code, acc) {
			t.Errorf("status %d should NOT trigger failover", code)
		}
	}
}

func TestShouldFailoverOpenAIUpstreamResponseForAccount_404OAuthFailover(t *testing.T) {
	s := &OpenAIGatewayService{}
	acc := &Account{Type: AccountTypeOAuth}
	if !s.shouldFailoverOpenAIUpstreamResponseForAccount(404, "Not Found", []byte(`{"error":{"message":"Not Found"}}`), acc) {
		t.Errorf("404 OAuth: response-level helper must also failover")
	}
}

func TestShouldFailoverOpenAIUpstreamResponseForAccount_404APIKeyNoFailover(t *testing.T) {
	s := &OpenAIGatewayService{}
	acc := &Account{Type: AccountTypeAPIKey}
	if s.shouldFailoverOpenAIUpstreamResponseForAccount(404, "Not Found", []byte(`{"error":{"message":"Not Found"}}`), acc) {
		t.Errorf("404 API-key: response-level helper must NOT failover")
	}
}

// regression: legacy shouldFailoverOpenAIUpstreamResponse (no account
// param) must still behave as before — 404 NOT failover (used by
// internal paths where account scope isn't known).
func TestShouldFailoverOpenAIUpstreamResponse_404StillNotFailover(t *testing.T) {
	s := &OpenAIGatewayService{}
	if s.shouldFailoverOpenAIUpstreamResponse(404, "Not Found", []byte(`{"error":{"message":"Not Found"}}`)) {
		t.Errorf("legacy shouldFailoverOpenAIUpstreamResponse 404: regression — must NOT failover (account-aware variant handles 404)")
	}
}
