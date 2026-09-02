package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type openAIFastPolicyRepoStub struct {
	values map[string]string
}

func (s *openAIFastPolicyRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	panic("unexpected Get call")
}

func (s *openAIFastPolicyRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	if v, ok := s.values[key]; ok {
		return v, nil
	}
	return "", ErrSettingNotFound
}

func (s *openAIFastPolicyRepoStub) Set(ctx context.Context, key, value string) error {
	if s.values == nil {
		s.values = map[string]string{}
	}
	s.values[key] = value
	return nil
}

func (s *openAIFastPolicyRepoStub) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := s.values[key]; ok {
			result[key] = value
		}
	}
	return result, nil
}

func (s *openAIFastPolicyRepoStub) SetMultiple(ctx context.Context, settings map[string]string) error {
	panic("unexpected SetMultiple call")
}

func (s *openAIFastPolicyRepoStub) GetAll(ctx context.Context) (map[string]string, error) {
	panic("unexpected GetAll call")
}

func (s *openAIFastPolicyRepoStub) Delete(ctx context.Context, key string) error {
	panic("unexpected Delete call")
}

func newOpenAIGatewayServiceWithSettings(t *testing.T, settings *OpenAIFastPolicySettings) *OpenAIGatewayService {
	t.Helper()
	repo := &openAIFastPolicyRepoStub{values: map[string]string{}}
	if settings != nil {
		raw, err := json.Marshal(settings)
		require.NoError(t, err)
		repo.values[SettingKeyOpenAIFastPolicySettings] = string(raw)
	}
	return &OpenAIGatewayService{
		settingService: NewSettingService(repo, &config.Config{}),
	}
}

func TestEvaluateOpenAIFastPolicy_DefaultFiltersAllRecognizedTiers(t *testing.T) {
	svc := newOpenAIGatewayServiceWithSettings(t, DefaultOpenAIFastPolicySettings())
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	// codex 2026-05-16 round5 #2457: default policy is now ServiceTier=Any
	// + filter, so every recognized tier on every model gets filtered. The
	// whitelist stays empty (covers all models) because the user-facing
	// service_tier knob is orthogonal to model selection.
	// gpt-5.5 + priority → filter
	action, _ := svc.evaluateOpenAIFastPolicy(context.Background(), account, "gpt-5.5", OpenAIFastTierPriority)
	require.Equal(t, BetaPolicyActionFilter, action)

	// gpt-5.5-turbo → filter
	action, _ = svc.evaluateOpenAIFastPolicy(context.Background(), account, "gpt-5.5-turbo", OpenAIFastTierPriority)
	require.Equal(t, BetaPolicyActionFilter, action)

	// gpt-4 + priority → filter（默认策略覆盖所有模型）
	action, _ = svc.evaluateOpenAIFastPolicy(context.Background(), account, "gpt-4", OpenAIFastTierPriority)
	require.Equal(t, BetaPolicyActionFilter, action)

	// gpt-5.5 + flex → filter (new — Any covers flex too)
	action, _ = svc.evaluateOpenAIFastPolicy(context.Background(), account, "gpt-5.5", OpenAIFastTierFlex)
	require.Equal(t, BetaPolicyActionFilter, action)

	// empty tier → pass (no service_tier in body means nothing to filter)
	action, _ = svc.evaluateOpenAIFastPolicy(context.Background(), account, "gpt-5.5", "")
	require.Equal(t, BetaPolicyActionPass, action)
}

func TestEvaluateOpenAIFastPolicy_BlockRuleCarriesMessage(t *testing.T) {
	settings := &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{{
			ServiceTier:    OpenAIFastTierPriority,
			Action:         BetaPolicyActionBlock,
			Scope:          BetaPolicyScopeAll,
			ErrorMessage:   "fast mode is not allowed",
			ModelWhitelist: []string{"gpt-5.5"},
			FallbackAction: BetaPolicyActionPass,
		}},
	}
	svc := newOpenAIGatewayServiceWithSettings(t, settings)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	action, msg := svc.evaluateOpenAIFastPolicy(context.Background(), account, "gpt-5.5", OpenAIFastTierPriority)
	require.Equal(t, BetaPolicyActionBlock, action)
	require.Equal(t, "fast mode is not allowed", msg)
}

func TestEvaluateOpenAIFastPolicy_ScopeFiltersOAuth(t *testing.T) {
	settings := &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{{
			ServiceTier: OpenAIFastTierAny,
			Action:      BetaPolicyActionFilter,
			Scope:       BetaPolicyScopeOAuth,
		}},
	}
	svc := newOpenAIGatewayServiceWithSettings(t, settings)

	// OAuth account → rule matches
	oauthAccount := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	action, _ := svc.evaluateOpenAIFastPolicy(context.Background(), oauthAccount, "gpt-4", OpenAIFastTierPriority)
	require.Equal(t, BetaPolicyActionFilter, action)

	// API Key account → rule skipped → pass
	apiKeyAccount := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	action, _ = svc.evaluateOpenAIFastPolicy(context.Background(), apiKeyAccount, "gpt-4", OpenAIFastTierPriority)
	require.Equal(t, BetaPolicyActionPass, action)
}

func TestEvaluateOpenAIFastPolicy_UserScopedRuleOverridesGlobalRule(t *testing.T) {
	settings := &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{
			{
				ServiceTier: OpenAIFastTierPriority,
				Action:      BetaPolicyActionFilter,
				Scope:       BetaPolicyScopeAll,
			},
			{
				ServiceTier: OpenAIFastTierPriority,
				Action:      BetaPolicyActionPass,
				Scope:       BetaPolicyScopeAll,
				UserIDs:     []int64{42},
			},
		},
	}
	svc := newOpenAIGatewayServiceWithSettings(t, settings)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	allowedUserCtx := context.WithValue(context.Background(), ctxkey.UserID, int64(42))
	action, _ := svc.evaluateOpenAIFastPolicy(allowedUserCtx, account, "gpt-5.5", OpenAIFastTierPriority)
	require.Equal(t, BetaPolicyActionPass, action)

	otherUserCtx := context.WithValue(context.Background(), ctxkey.UserID, int64(43))
	action, _ = svc.evaluateOpenAIFastPolicy(otherUserCtx, account, "gpt-5.5", OpenAIFastTierPriority)
	require.Equal(t, BetaPolicyActionFilter, action)

	action, _ = svc.evaluateOpenAIFastPolicy(context.Background(), account, "gpt-5.5", OpenAIFastTierPriority)
	require.Equal(t, BetaPolicyActionFilter, action)
}

func TestApplyOpenAIFastPolicyToBody_FilterRemovesField(t *testing.T) {
	svc := newOpenAIGatewayServiceWithSettings(t, DefaultOpenAIFastPolicySettings())
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	// gpt-5.5 fast → service_tier stripped
	body := []byte(`{"model":"gpt-5.5","service_tier":"priority","messages":[]}`)
	updated, err := svc.applyOpenAIFastPolicyToBody(context.Background(), account, "gpt-5.5", body)
	require.NoError(t, err)
	require.NotContains(t, string(updated), `"service_tier"`)

	// Client sending "fast" (alias for priority) also filtered
	body = []byte(`{"model":"gpt-5.5","service_tier":"fast"}`)
	updated, err = svc.applyOpenAIFastPolicyToBody(context.Background(), account, "gpt-5.5", body)
	require.NoError(t, err)
	require.NotContains(t, string(updated), `"service_tier"`)

	// gpt-4 priority → 默认策略对所有模型 filter，service_tier 被移除
	body = []byte(`{"model":"gpt-4","service_tier":"priority"}`)
	updated, err = svc.applyOpenAIFastPolicyToBody(context.Background(), account, "gpt-4", body)
	require.NoError(t, err)
	require.NotContains(t, string(updated), `"service_tier"`)

	// No service_tier → no-op
	body = []byte(`{"model":"gpt-5.5"}`)
	updated, err = svc.applyOpenAIFastPolicyToBody(context.Background(), account, "gpt-5.5", body)
	require.NoError(t, err)
	require.Equal(t, string(body), string(updated))
}

func TestApplyOpenAIFastPolicyToBody_UserScopedRuleOverridesGlobalRule(t *testing.T) {
	settings := &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{
			{
				ServiceTier: OpenAIFastTierPriority,
				Action:      BetaPolicyActionFilter,
				Scope:       BetaPolicyScopeAll,
			},
			{
				ServiceTier: OpenAIFastTierPriority,
				Action:      BetaPolicyActionPass,
				Scope:       BetaPolicyScopeAll,
				UserIDs:     []int64{42},
			},
		},
	}
	svc := newOpenAIGatewayServiceWithSettings(t, settings)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	body := []byte(`{"model":"gpt-5.5","service_tier":"priority"}`)

	allowedUserCtx := context.WithValue(context.Background(), ctxkey.UserID, int64(42))
	updated, err := svc.applyOpenAIFastPolicyToBody(allowedUserCtx, account, "gpt-5.5", body)
	require.NoError(t, err)
	require.Equal(t, "priority", gjson.GetBytes(updated, "service_tier").String())

	otherUserCtx := context.WithValue(context.Background(), ctxkey.UserID, int64(43))
	updated, err = svc.applyOpenAIFastPolicyToBody(otherUserCtx, account, "gpt-5.5", body)
	require.NoError(t, err)
	require.NotContains(t, string(updated), `"service_tier"`)
}

func TestApplyOpenAIFastPolicyToBody_ForcePriorityRewritesKnownTier(t *testing.T) {
	settings := &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{{
			ServiceTier: OpenAIFastTierAny,
			Action:      OpenAIFastPolicyActionForcePriority,
			Scope:       BetaPolicyScopeAll,
		}},
	}
	svc := newOpenAIGatewayServiceWithSettings(t, settings)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	for _, tier := range []string{"flex", "auto", "default", "scale", "fast", "priority"} {
		body := []byte(`{"model":"gpt-5.5","service_tier":"` + tier + `"}`)
		updated, err := svc.applyOpenAIFastPolicyToBody(context.Background(), account, "gpt-5.5", body)
		require.NoError(t, err)
		require.Equal(t, OpenAIFastTierPriority, gjson.GetBytes(updated, "service_tier").String(),
			"tier %q should be forced to priority", tier)
	}
}

// TestApplyOpenAIFastPolicyToBody_DefaultStripsAllRecognizedTiers verifies
// the codex 2026-05-16 round5 #2457 narrow-scope hardening: the default
// policy is now ServiceTier=Any+filter, so every recognized tier
// (priority/fast/flex AND the OpenAI-official auto/default/scale that
// normalize() accepts) gets stripped under the default rule. This keeps the
// public service insulated from caller-controlled cost/latency overrides
// when no admin policy is configured in the settings table.
func TestApplyOpenAIFastPolicyToBody_DefaultStripsAllRecognizedTiers(t *testing.T) {
	svc := newOpenAIGatewayServiceWithSettings(t, DefaultOpenAIFastPolicySettings())
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	for _, tier := range []string{"priority", "fast", "flex", "auto", "default", "scale"} {
		body := []byte(`{"model":"gpt-5.5","service_tier":"` + tier + `"}`)
		updated, err := svc.applyOpenAIFastPolicyToBody(context.Background(), account, "gpt-5.5", body)
		require.NoError(t, err, "tier %q should pass without error", tier)
		require.NotContains(t, string(updated), `"service_tier"`,
			"tier %q must be stripped under the default Any+filter rule", tier)
	}

	// evaluate 层应判定为 filter（默认规则 ServiceTier=Any 匹配任意已识别 tier）
	for _, tier := range []string{"priority", "fast", "flex", "auto", "default", "scale"} {
		action, _ := svc.evaluateOpenAIFastPolicy(context.Background(), account, "gpt-5.5", tier)
		require.Equal(t, BetaPolicyActionFilter, action, "tier %q should evaluate to filter under default policy", tier)
	}
}

// TestApplyOpenAIFastPolicyToBody_AllRuleStripsOfficialTiers 验证管理员显式配置
// ServiceTier=all + Action=filter 规则后，auto/default/scale 等官方 tier 也会
// 被剥离。这是符合预期的——首条匹配 short-circuit，"all" 覆盖任意已识别 tier。
func TestApplyOpenAIFastPolicyToBody_AllRuleStripsOfficialTiers(t *testing.T) {
	settings := &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{{
			ServiceTier: OpenAIFastTierAny,
			Action:      BetaPolicyActionFilter,
			Scope:       BetaPolicyScopeAll,
		}},
	}
	svc := newOpenAIGatewayServiceWithSettings(t, settings)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	for _, tier := range []string{"auto", "default", "scale", "priority", "flex"} {
		body := []byte(`{"model":"gpt-5.5","service_tier":"` + tier + `"}`)
		updated, err := svc.applyOpenAIFastPolicyToBody(context.Background(), account, "gpt-5.5", body)
		require.NoError(t, err)
		require.NotContains(t, string(updated), `"service_tier"`,
			"tier %q should be stripped under ServiceTier=all + filter rule", tier)
	}
}

// TestApplyOpenAIFastPolicyToBody_UnknownTierStripped 验证真未知 tier 仍被剥离
// （normalize 返回 nil → normalizeResponsesBodyServiceTier 删除字段；
// applyOpenAIFastPolicyToBody 在 normTier 为空时直接 no-op，因为字段已不可能存在
// 于经过前置归一化的请求里。这里直接调 apply 验证它对未识别值不会异常）。
func TestApplyOpenAIFastPolicyToBody_UnknownTierStripped(t *testing.T) {
	svc := newOpenAIGatewayServiceWithSettings(t, DefaultOpenAIFastPolicySettings())
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	// normalize 阶段会将未知值剥离
	require.Nil(t, normalizeOpenAIServiceTier("xxx"))

	// codex 2026-05-16 round7: applyOpenAIFastPolicyToBody now also strips
	// unknown service_tier values directly. Previously this returned the
	// body unchanged, leaving e.g. service_tier="fixel" or other typo'd /
	// experimental values to leak upstream and serve as a probe surface
	// for callers hitting sub2api directly (bypassing the gcr scrub).
	for _, tier := range []string{"xxx", "fixel", "preemium", "speedy"} {
		body := []byte(`{"model":"gpt-5.5","service_tier":"` + tier + `"}`)
		updated, err := svc.applyOpenAIFastPolicyToBody(context.Background(), account, "gpt-5.5", body)
		require.NoError(t, err, "tier %q", tier)
		require.NotContains(t, string(updated), `"service_tier"`,
			"tier %q must be stripped (unknown-tier hardening)", tier)
	}
}

// TestApplyOpenAIFastPolicyToWSResponseCreate_UnknownTierStripped mirrors
// the HTTP-side unknown-tier hardening on the WS Realtime path so callers
// can't probe via response.create either.
func TestApplyOpenAIFastPolicyToWSResponseCreate_UnknownTierStripped(t *testing.T) {
	svc := newOpenAIGatewayServiceWithSettings(t, DefaultOpenAIFastPolicySettings())
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	for _, tier := range []string{"xxx", "fixel", "preemium", "speedy"} {
		frame := []byte(`{"type":"response.create","model":"gpt-5.5","service_tier":"` + tier + `"}`)
		out, blocked, err := svc.applyOpenAIFastPolicyToWSResponseCreate(context.Background(), account, "gpt-5.5", frame)
		require.NoError(t, err, "tier %q", tier)
		require.Nil(t, blocked, "tier %q", tier)
		require.NotContains(t, string(out), `"service_tier"`,
			"tier %q must be stripped from WS frame", tier)
	}
}

func TestApplyOpenAIFastPolicyToBody_BlockReturnsTypedError(t *testing.T) {
	settings := &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{{
			ServiceTier:    OpenAIFastTierPriority,
			Action:         BetaPolicyActionBlock,
			Scope:          BetaPolicyScopeAll,
			ErrorMessage:   "fast mode is blocked for gpt-5.5",
			ModelWhitelist: []string{"gpt-5.5"},
			FallbackAction: BetaPolicyActionPass,
		}},
	}
	svc := newOpenAIGatewayServiceWithSettings(t, settings)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	body := []byte(`{"model":"gpt-5.5","service_tier":"priority"}`)
	updated, err := svc.applyOpenAIFastPolicyToBody(context.Background(), account, "gpt-5.5", body)
	require.Error(t, err)
	var blocked *OpenAIFastBlockedError
	require.True(t, errors.As(err, &blocked))
	require.Contains(t, blocked.Message, "fast mode is blocked")
	require.Equal(t, string(body), string(updated)) // body not mutated on block
}

func TestSetOpenAIFastPolicySettings_Validation(t *testing.T) {
	repo := &openAIFastPolicyRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, &config.Config{})

	// Invalid action rejected
	err := svc.SetOpenAIFastPolicySettings(context.Background(), &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{{
			ServiceTier: OpenAIFastTierPriority,
			Action:      "bogus",
			Scope:       BetaPolicyScopeAll,
		}},
	})
	require.Error(t, err)

	// Invalid service_tier rejected
	err = svc.SetOpenAIFastPolicySettings(context.Background(), &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{{
			ServiceTier: "turbo",
			Action:      BetaPolicyActionPass,
			Scope:       BetaPolicyScopeAll,
		}},
	})
	require.Error(t, err)

	// Non-positive and duplicate user IDs are rejected.
	err = svc.SetOpenAIFastPolicySettings(context.Background(), &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{{
			ServiceTier: OpenAIFastTierPriority,
			Action:      BetaPolicyActionPass,
			Scope:       BetaPolicyScopeAll,
			UserIDs:     []int64{0},
		}},
	})
	require.Error(t, err)

	err = svc.SetOpenAIFastPolicySettings(context.Background(), &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{{
			ServiceTier: OpenAIFastTierPriority,
			Action:      BetaPolicyActionPass,
			Scope:       BetaPolicyScopeAll,
			UserIDs:     []int64{42, 42},
		}},
	})
	require.Error(t, err)

	// Valid settings persisted
	err = svc.SetOpenAIFastPolicySettings(context.Background(), &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{{
			ServiceTier: OpenAIFastTierPriority,
			Action:      OpenAIFastPolicyActionForcePriority,
			Scope:       BetaPolicyScopeAll,
			UserIDs:     []int64{42, 43},
		}},
	})
	require.NoError(t, err)

	got, err := svc.GetOpenAIFastPolicySettings(context.Background())
	require.NoError(t, err)
	require.Len(t, got.Rules, 1)
	require.Equal(t, OpenAIFastTierPriority, got.Rules[0].ServiceTier)
	require.Equal(t, OpenAIFastPolicyActionForcePriority, got.Rules[0].Action)
	require.Equal(t, []int64{42, 43}, got.Rules[0].UserIDs)
}

// TestApplyOpenAIFastPolicyToWSResponseCreate_PassNormalizesFastAlias verifies
// codex 2026-05-16 round5 #2457 narrow scope: the WS response.create filter
// should normalize the "fast" alias to its canonical "priority" form on the
// pass branch, mirroring what the HTTP body path already does. Previously
// the WS path returned the frame untouched on pass, leaving "fast" leaking
// upstream and forcing whoever bills off the post-policy frame to special-
// case the alias.
func TestApplyOpenAIFastPolicyToWSResponseCreate_PassNormalizesFastAlias(t *testing.T) {
	// Use a custom pass-through policy so the fast→priority normalize path
	// is reachable (the default Any+filter would strip the field instead).
	svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{{
			ServiceTier:    OpenAIFastTierFlex, // matches flex only
			Action:         BetaPolicyActionFilter,
			Scope:          BetaPolicyScopeAll,
			FallbackAction: BetaPolicyActionPass,
		}},
	})
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	// fast → priority on pass
	in := []byte(`{"type":"response.create","model":"gpt-5.5","service_tier":"fast"}`)
	out, blocked, err := svc.applyOpenAIFastPolicyToWSResponseCreate(context.Background(), account, "gpt-5.5", in)
	require.NoError(t, err)
	require.Nil(t, blocked)
	require.Equal(t, "priority", gjson.GetBytes(out, "service_tier").String(),
		"WS pass branch must rewrite fast → priority canonical form")

	// priority stays priority (no alias rewrite needed)
	in = []byte(`{"type":"response.create","model":"gpt-5.5","service_tier":"priority"}`)
	out, blocked, err = svc.applyOpenAIFastPolicyToWSResponseCreate(context.Background(), account, "gpt-5.5", in)
	require.NoError(t, err)
	require.Nil(t, blocked)
	require.Equal(t, string(in), string(out), "WS pass with priority must not mutate frame")
}

// TestApplyOpenAIFastPolicyToWSResponseCreate_DefaultFiltersFlex pins the
// new default ALL-filter semantics on the WS path: under the production
// default policy (no admin override), flex requests on response.create get
// service_tier stripped before going upstream, same as priority/fast.
func TestApplyOpenAIFastPolicyToWSResponseCreate_DefaultFiltersFlex(t *testing.T) {
	svc := newOpenAIGatewayServiceWithSettings(t, DefaultOpenAIFastPolicySettings())
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	for _, tier := range []string{"priority", "fast", "flex"} {
		in := []byte(`{"type":"response.create","model":"gpt-5.5","service_tier":"` + tier + `"}`)
		out, blocked, err := svc.applyOpenAIFastPolicyToWSResponseCreate(context.Background(), account, "gpt-5.5", in)
		require.NoError(t, err, "tier %q", tier)
		require.Nil(t, blocked, "tier %q", tier)
		require.NotContains(t, string(out), `"service_tier"`,
			"tier %q must be stripped by default WS policy", tier)
	}
}

// TestNativeResponsesForwardReqBodyMap_ServiceTierStrip pins the contract
// for the native /v1/responses Forward path block at
// openai_gateway_service.go:2755. That block operates on a parsed
// map[string]any (not a raw byte body), so it sits on a separate code path
// from applyOpenAIFastPolicyToBody (raw-bytes / passthrough) and
// applyOpenAIFastPolicyToWSResponseCreate (WS frame).
//
// codex 2026-05-16 round8 spotted a gap: the block previously only ran the
// policy switch when normalizedOpenAIServiceTierValue returned non-empty.
// Unknown string tiers (e.g. user-supplied "fixel" / "preemium") were left
// in reqBody and leaked upstream. Round8 added an else-branch that strips
// unknown string tiers, with non-string types still passing through
// unchanged (the outer `.(string)` type assertion handles that).
//
// This test inline-mirrors the block (with the line number called out in
// the comment) so future refactors must keep the contract identical. It
// exercises the SAME production helpers — normalizedOpenAIServiceTierValue
// and svc.evaluateOpenAIFastPolicy — so a change in those helpers also
// surfaces here.
func TestNativeResponsesForwardReqBodyMap_ServiceTierStrip(t *testing.T) {
	svc := newOpenAIGatewayServiceWithSettings(t, DefaultOpenAIFastPolicySettings())
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	const upstreamModel = "gpt-5.5"

	// Mirror of openai_gateway_service.go:2755 — keep in sync with the
	// production block. codex round9: presence-based strip, including
	// non-string types. If you change the block, mirror the change here.
	runBlock := func(reqBody map[string]any) (blocked *OpenAIFastBlockedError, modified bool) {
		if _, exists := reqBody["service_tier"]; !exists {
			return nil, false
		}
		rawTier, isString := reqBody["service_tier"].(string)
		if !isString {
			// Non-string → strip (round9).
			delete(reqBody, "service_tier")
			return nil, true
		}
		normTier := normalizedOpenAIServiceTierValue(rawTier)
		if normTier == "" {
			// Unknown / empty string → strip (round7+round8).
			delete(reqBody, "service_tier")
			return nil, true
		}
		action, errMsg := svc.evaluateOpenAIFastPolicy(context.Background(), account, upstreamModel, normTier)
		switch action {
		case BetaPolicyActionBlock:
			msg := errMsg
			if msg == "" {
				msg = "openai service_tier=" + normTier + " is not allowed for model " + upstreamModel
			}
			return &OpenAIFastBlockedError{Message: msg}, false
		case BetaPolicyActionFilter:
			delete(reqBody, "service_tier")
			return nil, true
		default:
			if normTier != rawTier {
				reqBody["service_tier"] = normTier
				return nil, true
			}
			return nil, false
		}
	}

	cases := []struct {
		name      string
		input     map[string]any
		expectKey bool // true → service_tier should still be in map
		expectMod bool
		expectBlk bool
	}{
		// Round8: unknown string tiers all get stripped.
		{name: "unknown_string_fixel", input: map[string]any{"service_tier": "fixel"}, expectKey: false, expectMod: true},
		{name: "unknown_string_preemium", input: map[string]any{"service_tier": "preemium"}, expectKey: false, expectMod: true},
		{name: "unknown_string_speedy", input: map[string]any{"service_tier": "speedy"}, expectKey: false, expectMod: true},
		// Recognized tiers + default Any+filter policy: all stripped.
		{name: "priority_default_filter", input: map[string]any{"service_tier": "priority"}, expectKey: false, expectMod: true},
		{name: "fast_default_filter", input: map[string]any{"service_tier": "fast"}, expectKey: false, expectMod: true},
		{name: "flex_default_filter", input: map[string]any{"service_tier": "flex"}, expectKey: false, expectMod: true},
		{name: "auto_default_filter", input: map[string]any{"service_tier": "auto"}, expectKey: false, expectMod: true},
		// codex round9: non-string types now also stripped (was: passthrough).
		{name: "number_stripped", input: map[string]any{"service_tier": 1.0}, expectKey: false, expectMod: true},
		{name: "nested_object_stripped", input: map[string]any{"service_tier": map[string]any{"k": "v"}}, expectKey: false, expectMod: true},
		{name: "nil_stripped", input: map[string]any{"service_tier": nil}, expectKey: false, expectMod: true},
		{name: "array_stripped", input: map[string]any{"service_tier": []any{"priority"}}, expectKey: false, expectMod: true},
		{name: "bool_stripped", input: map[string]any{"service_tier": true}, expectKey: false, expectMod: true},
		// No service_tier → no-op.
		{name: "absent_field_noop", input: map[string]any{"model": "x"}, expectKey: false, expectMod: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.input
			blocked, modified := runBlock(body)
			require.Equal(t, tc.expectBlk, blocked != nil, "blocked flag")
			require.Equal(t, tc.expectMod, modified, "modified flag")
			_, present := body["service_tier"]
			require.Equal(t, tc.expectKey, present, "service_tier presence in reqBody after block")
		})
	}
}

// TestNativeResponsesForwardReqBodyMap_RecognizedTierPassNormalize covers
// the alias-normalize pass-through case via a custom policy that lets the
// tier through (so the default Any+filter doesn't short-circuit the test).
// This pins the contract that on pass, "fast" gets rewritten to its
// canonical "priority" form (and recognized tiers like "priority" stay).
func TestNativeResponsesForwardReqBodyMap_RecognizedTierPassNormalize(t *testing.T) {
	// Use a policy that filters flex only — so priority + fast hit
	// the fall-through pass branch where the alias-normalize happens.
	settings := &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{{
			ServiceTier:    OpenAIFastTierFlex,
			Action:         BetaPolicyActionFilter,
			Scope:          BetaPolicyScopeAll,
			FallbackAction: BetaPolicyActionPass,
		}},
	}
	svc := newOpenAIGatewayServiceWithSettings(t, settings)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	const upstreamModel = "gpt-5.5"

	runBlock := func(reqBody map[string]any) {
		if rawTier, ok := reqBody["service_tier"].(string); ok {
			if normTier := normalizedOpenAIServiceTierValue(rawTier); normTier != "" {
				action, _ := svc.evaluateOpenAIFastPolicy(context.Background(), account, upstreamModel, normTier)
				switch action {
				case BetaPolicyActionFilter:
					delete(reqBody, "service_tier")
				case BetaPolicyActionPass:
					if normTier != rawTier {
						reqBody["service_tier"] = normTier
					}
				}
			} else {
				delete(reqBody, "service_tier")
			}
		}
	}

	// fast alias on pass → rewritten to canonical priority.
	body := map[string]any{"service_tier": "fast"}
	runBlock(body)
	require.Equal(t, "priority", body["service_tier"], "fast alias should normalize to priority on pass")

	// Canonical priority on pass → stays untouched.
	body = map[string]any{"service_tier": "priority"}
	runBlock(body)
	require.Equal(t, "priority", body["service_tier"], "canonical priority must not be rewritten")

	// flex hits the explicit filter rule → stripped.
	body = map[string]any{"service_tier": "flex"}
	runBlock(body)
	_, present := body["service_tier"]
	require.False(t, present, "flex must be stripped by custom flex-filter policy")
}
