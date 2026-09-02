package service

import (
	"errors"
	"strings"
)

func validateOpenAIWSBearerToken(account *Account, token string) error {
	if account == nil {
		return errors.New("account is nil")
	}
	if strings.TrimSpace(token) == "" && !account.IsOpenAIAgentIdentity() {
		return errors.New("token is empty")
	}
	return nil
}
