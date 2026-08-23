package service

import "github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"

// shouldForwardOpenAIResponsesViaRawChatCompletions keeps explicit CN protocol
// choices authoritative over asynchronously probed compatibility metadata.
func shouldForwardOpenAIResponsesViaRawChatCompletions(account *Account) bool {
	if account == nil || account.Type != AccountTypeAPIKey {
		return false
	}
	if account.IsCNProvider() {
		switch account.GetAPIProtocol() {
		case APIProtocolChatCompletions:
			return true
		case APIProtocolAdaptive:
			return account.Platform != PlatformDeepseek
		default:
			return false
		}
	}
	return !openai_compat.ShouldUseResponsesAPI(account.Extra)
}
