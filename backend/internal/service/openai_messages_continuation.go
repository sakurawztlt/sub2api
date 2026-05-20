package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type openAICompatSessionResponseBinding struct {
	ResponseID           string
	TurnState            string
	ContinuationDisabled bool
	ExpiresAt            time.Time
}

func openAICompatContinuationEnabled(account *Account, model string) bool {
	if account == nil || account.Type != AccountTypeAPIKey {
		return false
	}
	return shouldAutoInjectPromptCacheKeyForCompat(model)
}

func trimAnthropicCompatResponsesInputToLatestTurn(req *apicompat.ResponsesRequest) {
	if req == nil || len(req.Input) == 0 {
		return
	}

	var items []apicompat.ResponsesInputItem
	if err := json.Unmarshal(req.Input, &items); err != nil || len(items) == 0 {
		return
	}

	start := latestAnthropicCompatResponsesInputTurnStart(items)
	trimmed := append([]apicompat.ResponsesInputItem(nil), items[start:]...)
	if len(trimmed) == len(items) {
		return
	}
	if input, err := json.Marshal(trimmed); err == nil {
		req.Input = input
	}
}

func latestAnthropicCompatResponsesInputTurnStart(items []apicompat.ResponsesInputItem) int {
	if len(items) == 0 {
		return 0
	}

	start := len(items) - 1
	last := items[start]
	switch {
	case last.Type == "function_call_output":
		for start > 0 && items[start-1].Type == "function_call_output" {
			start--
		}
	case last.Type == "message" && last.Role == "user":
		for start > 0 && items[start-1].Type == "function_call_output" {
			start--
		}
	default:
		return start
	}

	return expandAnthropicCompatResponsesInputToolCallStart(items, start)
}

func expandAnthropicCompatResponsesInputToolCallStart(items []apicompat.ResponsesInputItem, start int) int {
	if start < 0 || start >= len(items) {
		return start
	}

	needed := make(map[string]struct{})
	for i := start; i < len(items); i++ {
		if items[i].Type != "function_call_output" {
			continue
		}
		callID := strings.TrimSpace(items[i].CallID)
		if callID != "" {
			needed[callID] = struct{}{}
		}
	}
	if len(needed) == 0 {
		return start
	}

	expandedStart := start
	for i := start - 1; i >= 0 && len(needed) > 0; i-- {
		if items[i].Type != "function_call" {
			continue
		}
		callID := strings.TrimSpace(items[i].CallID)
		if _, ok := needed[callID]; !ok {
			continue
		}
		delete(needed, callID)
		expandedStart = i
	}
	return expandedStart
}

func isOpenAICompatPreviousResponseNotFound(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusNotFound {
		return false
	}
	check := func(s string) bool {
		lower := strings.ToLower(strings.TrimSpace(s))
		return strings.Contains(lower, "previous_response_not_found") ||
			(strings.Contains(lower, "previous response") && strings.Contains(lower, "not found")) ||
			(strings.Contains(lower, "unsupported parameter") && strings.Contains(lower, "previous_response_id"))
	}
	if check(upstreamMsg) || check(string(upstreamBody)) {
		return true
	}
	return check(gjson.GetBytes(upstreamBody, "error.code").String()) ||
		check(gjson.GetBytes(upstreamBody, "error.message").String())
}

func isOpenAICompatPreviousResponseUnsupported(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	check := func(s string) bool {
		lower := strings.ToLower(strings.TrimSpace(s))
		if !strings.Contains(lower, "previous_response_id") {
			return false
		}
		return strings.Contains(lower, "unsupported parameter") ||
			strings.Contains(lower, "only supported on responses websocket") ||
			strings.Contains(lower, "not supported")
	}
	if check(upstreamMsg) || check(string(upstreamBody)) {
		return true
	}
	return check(gjson.GetBytes(upstreamBody, "error.code").String()) ||
		check(gjson.GetBytes(upstreamBody, "error.message").String())
}

func openAICompatSessionResponseKey(c *gin.Context, account *Account, promptCacheKey string) string {
	key := strings.TrimSpace(promptCacheKey)
	if account == nil || key == "" {
		return ""
	}
	apiKeyID := int64(0)
	if c != nil {
		apiKeyID = getAPIKeyIDFromContext(c)
	}
	return strings.Join([]string{
		strconv.FormatInt(account.ID, 10),
		strconv.FormatInt(apiKeyID, 10),
		key,
	}, "\x00")
}

// codex round36 fu55 (2026-05-20): enum constants for the
// turn_state_key_source observability field on the
// large_context_request summary log. These describe which derivation
// path openAICompatTurnStateKey took for the current request. They
// are NEVER fed back into key bytes — they exist only so ops grepping
// the log can quickly distinguish "Claude Code header path" from
// "rolling promptCacheKey path" from "no session signal at all".
//
// The "metadata_session" value is reserved: fu55 records whether the
// body has metadata.user_id.session_id (via the has_metadata_session
// bool) but does NOT yet derive the cache key from it. A future
// round may promote it to a key source between session_header and
// prompt_cache_key. Keeping the constant here documents the planned
// enum surface so log grep templates can be written ahead of time.
const (
	turnStateKeySourceNone            = "none"
	turnStateKeySourceSessionHeader   = "session_header"
	turnStateKeySourceMetadataSession = "metadata_session" // reserved for fu56+
	turnStateKeySourcePromptCacheKey  = "prompt_cache_key"
)

// describeTurnStateKeySource mirrors the precedence in
// openAICompatTurnStateKey for observability. Currently fu54 only
// honors header → promptCacheKey, so the returned value will be
// exactly one of: turnStateKeySourceSessionHeader,
// turnStateKeySourcePromptCacheKey, turnStateKeySourceNone. The
// metadata_session value is reserved (see the constant block above).
//
// IMPORTANT: this MUST stay in lock-step with openAICompatTurnStateKey
// — if a future round adds a new derivation source there, add it here
// too. Drift between the two will mislead ops triage.
func describeTurnStateKeySource(c *gin.Context, promptCacheKey string) string {
	if c != nil {
		if sid := strings.TrimSpace(c.GetHeader("X-Claude-Code-Session-Id")); sid != "" {
			return turnStateKeySourceSessionHeader
		}
	}
	if strings.TrimSpace(promptCacheKey) != "" {
		return turnStateKeySourcePromptCacheKey
	}
	return turnStateKeySourceNone
}

// hasMetadataUserSessionID reports whether the request body has a
// non-empty metadata.user_id.session_id field. fu55 records this only
// as an observability boolean (has_metadata_session) — the value is
// NOT used to derive the cache key yet. A fu56+ round may promote it
// to a real key source; for now, ops uses this signal to estimate how
// many requests would benefit from metadata-based derivation.
//
// Privacy: returns bool, never the session id itself.
func hasMetadataUserSessionID(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	v := gjson.GetBytes(body, "metadata.user_id.session_id")
	if !v.Exists() {
		return false
	}
	return strings.TrimSpace(v.String()) != ""
}

// openAICompatTurnStateKey derives the cache key for the
// x-codex-turn-state continuation (codex OAuth /v1/responses path).
//
// codex round35 fu54 (2026-05-20): prefer a conversation-stable
// session identifier (X-Claude-Code-Session-Id header) over the
// rolling promptCacheKey when one is present. The promptCacheKey
// rolls every turn (prefix hash of messages), so binding turn_state
// under that key means follow-up turns within the same conversation
// can never hit — gcr reports has_turn_state=false on every turn
// even when the conversation is continuing. A conversation-level
// session id stays stable across turns and tracks continuation state
// correctly.
//
// The previous_response_id cache (store=true path) intentionally
// still uses openAICompatSessionResponseKey above to keep this change
// scoped to the OAuth continuation issue codex actually flagged.
// Both keys share s.openaiCompatSessionResponses, but the "turnstate"
// namespace bytes guarantee the two key spaces are disjoint —
// a request that has no session id and falls back to promptCacheKey
// still gets a distinct entry from the response-id binding for the
// same (account, apiKeyID, promptCacheKey) triple.
func openAICompatTurnStateKey(c *gin.Context, account *Account, promptCacheKey string) string {
	if account == nil {
		return ""
	}
	primary := ""
	if c != nil {
		if sid := strings.TrimSpace(c.GetHeader("X-Claude-Code-Session-Id")); sid != "" {
			primary = sid
		}
	}
	if primary == "" {
		primary = strings.TrimSpace(promptCacheKey)
	}
	if primary == "" {
		return ""
	}
	apiKeyID := int64(0)
	if c != nil {
		apiKeyID = getAPIKeyIDFromContext(c)
	}
	return strings.Join([]string{
		strconv.FormatInt(account.ID, 10),
		strconv.FormatInt(apiKeyID, 10),
		"turnstate",
		primary,
	}, "\x00")
}

func (s *OpenAIGatewayService) getOpenAICompatSessionResponseID(_ context.Context, c *gin.Context, account *Account, promptCacheKey string) string {
	if s == nil {
		return ""
	}
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	if key == "" {
		return ""
	}
	raw, ok := s.openaiCompatSessionResponses.Load(key)
	if !ok {
		return ""
	}
	binding, ok := raw.(openAICompatSessionResponseBinding)
	if !ok {
		s.openaiCompatSessionResponses.Delete(key)
		return ""
	}
	if !binding.ExpiresAt.IsZero() && time.Now().After(binding.ExpiresAt) {
		s.openaiCompatSessionResponses.Delete(key)
		return ""
	}
	if binding.ContinuationDisabled {
		return ""
	}
	if strings.TrimSpace(binding.ResponseID) == "" {
		s.openaiCompatSessionResponses.Delete(key)
		return ""
	}
	return strings.TrimSpace(binding.ResponseID)
}

func (s *OpenAIGatewayService) bindOpenAICompatSessionResponseID(_ context.Context, c *gin.Context, account *Account, promptCacheKey, responseID string) {
	if s == nil {
		return
	}
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	id := strings.TrimSpace(responseID)
	if key == "" || id == "" {
		return
	}
	binding := openAICompatSessionResponseBinding{
		ResponseID: id,
		ExpiresAt:  time.Now().Add(s.openAIWSResponseStickyTTL()),
	}
	if raw, ok := s.openaiCompatSessionResponses.Load(key); ok {
		if existing, ok := raw.(openAICompatSessionResponseBinding); ok {
			if existing.ContinuationDisabled {
				existing.ResponseID = ""
				existing.ExpiresAt = time.Now().Add(s.openAIWSResponseStickyTTL())
				s.openaiCompatSessionResponses.Store(key, existing)
				return
			}
			binding.TurnState = existing.TurnState
		}
	}
	s.openaiCompatSessionResponses.Store(key, binding)
}

func (s *OpenAIGatewayService) deleteOpenAICompatSessionResponseID(_ context.Context, c *gin.Context, account *Account, promptCacheKey string) {
	if s == nil {
		return
	}
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	if key == "" {
		return
	}
	raw, ok := s.openaiCompatSessionResponses.Load(key)
	if !ok {
		return
	}
	binding, ok := raw.(openAICompatSessionResponseBinding)
	if !ok {
		s.openaiCompatSessionResponses.Delete(key)
		return
	}
	binding.ResponseID = ""
	if strings.TrimSpace(binding.TurnState) == "" && !binding.ContinuationDisabled {
		s.openaiCompatSessionResponses.Delete(key)
		return
	}
	binding.ExpiresAt = time.Now().Add(s.openAIWSResponseStickyTTL())
	s.openaiCompatSessionResponses.Store(key, binding)
}

func (s *OpenAIGatewayService) disableOpenAICompatSessionContinuation(_ context.Context, c *gin.Context, account *Account, promptCacheKey string) {
	if s == nil {
		return
	}
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	if key == "" {
		return
	}
	binding := openAICompatSessionResponseBinding{
		ContinuationDisabled: true,
		ExpiresAt:            time.Now().Add(s.openAIWSResponseStickyTTL()),
	}
	if raw, ok := s.openaiCompatSessionResponses.Load(key); ok {
		if existing, ok := raw.(openAICompatSessionResponseBinding); ok {
			binding.TurnState = existing.TurnState
		}
	}
	s.openaiCompatSessionResponses.Store(key, binding)
}

func (s *OpenAIGatewayService) isOpenAICompatSessionContinuationDisabled(_ context.Context, c *gin.Context, account *Account, promptCacheKey string) bool {
	if s == nil {
		return false
	}
	key := openAICompatSessionResponseKey(c, account, promptCacheKey)
	if key == "" {
		return false
	}
	raw, ok := s.openaiCompatSessionResponses.Load(key)
	if !ok {
		return false
	}
	binding, ok := raw.(openAICompatSessionResponseBinding)
	if !ok {
		s.openaiCompatSessionResponses.Delete(key)
		return false
	}
	if !binding.ExpiresAt.IsZero() && time.Now().After(binding.ExpiresAt) {
		s.openaiCompatSessionResponses.Delete(key)
		return false
	}
	return binding.ContinuationDisabled
}

func (s *OpenAIGatewayService) getOpenAICompatSessionTurnState(_ context.Context, c *gin.Context, account *Account, promptCacheKey string) string {
	if s == nil {
		return ""
	}
	// codex round35 fu54: turn_state key derives from session-id when
	// present (stable across follow-up turns) — see openAICompatTurnStateKey.
	key := openAICompatTurnStateKey(c, account, promptCacheKey)
	if key == "" {
		return ""
	}
	raw, ok := s.openaiCompatSessionResponses.Load(key)
	if !ok {
		return ""
	}
	binding, ok := raw.(openAICompatSessionResponseBinding)
	if !ok || strings.TrimSpace(binding.TurnState) == "" {
		return ""
	}
	if !binding.ExpiresAt.IsZero() && time.Now().After(binding.ExpiresAt) {
		s.openaiCompatSessionResponses.Delete(key)
		return ""
	}
	return strings.TrimSpace(binding.TurnState)
}

func (s *OpenAIGatewayService) bindOpenAICompatSessionTurnState(_ context.Context, c *gin.Context, account *Account, promptCacheKey, turnState string) {
	if s == nil {
		return
	}
	// codex round35 fu54: see getOpenAICompatSessionTurnState — same key
	// namespace.
	key := openAICompatTurnStateKey(c, account, promptCacheKey)
	state := strings.TrimSpace(turnState)
	if key == "" || state == "" {
		return
	}
	binding := openAICompatSessionResponseBinding{
		TurnState: state,
		ExpiresAt: time.Now().Add(s.openAIWSResponseStickyTTL()),
	}
	// The turnstate-namespaced key never collides with the response_id key
	// space, but preserve any state that might already be there
	// (e.g. a future caller in the same namespace) defensively.
	if raw, ok := s.openaiCompatSessionResponses.Load(key); ok {
		if existing, ok := raw.(openAICompatSessionResponseBinding); ok {
			binding.ResponseID = existing.ResponseID
			binding.ContinuationDisabled = existing.ContinuationDisabled
		}
	}
	s.openaiCompatSessionResponses.Store(key, binding)
}
