package service

import "strings"

const (
	// codex round54 fu64 (2026-05-21) Phase 1: Opus 升 gpt-5.5 ($5/$0.5/$30
	// per 1M, ~2x gpt-5.4 价). 跟 gcr ModelMap (claude-opus-4-7/4-6 → gpt-5.5)
	// 同步; sub2api pricing 同 commit 加 gpt-5.5 fallback 不再静默走 gpt-5.4.
	// Sonnet 4.x / Haiku 4.x 暂留 gpt-5.4 family (codex: 先灰度), 这边只动 Opus.
	// 客户绕过 gcr 直打 sub2api 的 Anthropic-shape 请求也走相同路径.
	// Group.MessagesDispatchModelConfig.OpusMappedModel / ExactModelMappings
	// 仍优先 (DB 可显式覆盖此默认值).
	defaultOpenAIMessagesDispatchOpusMappedModel   = "gpt-5.5"
	defaultOpenAIMessagesDispatchSonnetMappedModel = "gpt-5.3-codex"
	defaultOpenAIMessagesDispatchHaikuMappedModel  = "gpt-5.4-mini"
)

func normalizeOpenAIMessagesDispatchMappedModel(model string) string {
	model = NormalizeOpenAICompatRequestedModel(strings.TrimSpace(model))
	return strings.TrimSpace(model)
}

func normalizeOpenAIMessagesDispatchModelConfig(cfg OpenAIMessagesDispatchModelConfig) OpenAIMessagesDispatchModelConfig {
	out := OpenAIMessagesDispatchModelConfig{
		OpusMappedModel:   normalizeOpenAIMessagesDispatchMappedModel(cfg.OpusMappedModel),
		SonnetMappedModel: normalizeOpenAIMessagesDispatchMappedModel(cfg.SonnetMappedModel),
		HaikuMappedModel:  normalizeOpenAIMessagesDispatchMappedModel(cfg.HaikuMappedModel),
	}

	if len(cfg.ExactModelMappings) > 0 {
		out.ExactModelMappings = make(map[string]string, len(cfg.ExactModelMappings))
		for requestedModel, mappedModel := range cfg.ExactModelMappings {
			requestedModel = strings.TrimSpace(requestedModel)
			mappedModel = normalizeOpenAIMessagesDispatchMappedModel(mappedModel)
			if requestedModel == "" || mappedModel == "" {
				continue
			}
			out.ExactModelMappings[requestedModel] = mappedModel
		}
		if len(out.ExactModelMappings) == 0 {
			out.ExactModelMappings = nil
		}
	}

	return out
}

func claudeMessagesDispatchFamily(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if !strings.HasPrefix(normalized, "claude") {
		return ""
	}
	switch {
	case strings.Contains(normalized, "opus"):
		return "opus"
	case strings.Contains(normalized, "sonnet"):
		return "sonnet"
	case strings.Contains(normalized, "haiku"):
		return "haiku"
	default:
		return ""
	}
}

// ResolveMessagesDispatchModel returns the OpenAI Responses-API model this
// group should dispatch an Anthropic-shaped request to.
//
// Resolution order (high → low priority):
//  1. Exact per-model mapping from MessagesDispatchModelConfig.ExactModelMappings
//  2. Family-specific mapping (OpusMappedModel / SonnetMappedModel / HaikuMappedModel)
//  3. **Group.DefaultMappedModel** — fork-local catch-all (2026-04-23 originally
//     picked from upstream pr-1606 via commit 4f80dddb; lost during
//     2026-04-23 merge of upstream/main and restored here 2026-04-24 after
//     cctest opus-4-7 behavior validation regressed from 85% to 60% because
//     groups that relied on DefaultMappedModel for Claude requests were
//     silently falling to the hard-coded family defaults.)
//  4. Hard-coded family default (gpt-5.4 / gpt-5.3-codex / gpt-5.4-mini)
//
// Fork deliberately diverges from upstream/main here: upstream went back to
// no groupDefault fallback. Our groups historically configured
// DefaultMappedModel and depend on (3) holding.
func (g *Group) ResolveMessagesDispatchModel(requestedModel string) string {
	if g == nil {
		return ""
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return ""
	}

	cfg := normalizeOpenAIMessagesDispatchModelConfig(g.MessagesDispatchModelConfig)
	if mappedModel := strings.TrimSpace(cfg.ExactModelMappings[requestedModel]); mappedModel != "" {
		return mappedModel
	}

	groupDefault := strings.TrimSpace(g.DefaultMappedModel)

	switch claudeMessagesDispatchFamily(requestedModel) {
	case "opus":
		if mappedModel := strings.TrimSpace(cfg.OpusMappedModel); mappedModel != "" {
			return mappedModel
		}
		if groupDefault != "" {
			return groupDefault
		}
		return defaultOpenAIMessagesDispatchOpusMappedModel
	case "sonnet":
		if mappedModel := strings.TrimSpace(cfg.SonnetMappedModel); mappedModel != "" {
			return mappedModel
		}
		if groupDefault != "" {
			return groupDefault
		}
		return defaultOpenAIMessagesDispatchSonnetMappedModel
	case "haiku":
		if mappedModel := strings.TrimSpace(cfg.HaikuMappedModel); mappedModel != "" {
			return mappedModel
		}
		if groupDefault != "" {
			return groupDefault
		}
		return defaultOpenAIMessagesDispatchHaikuMappedModel
	default:
		return ""
	}
}

func sanitizeGroupMessagesDispatchFields(g *Group) {
	if g == nil || g.Platform == PlatformOpenAI {
		return
	}
	g.AllowMessagesDispatch = false
	g.DefaultMappedModel = ""
	g.MessagesDispatchModelConfig = OpenAIMessagesDispatchModelConfig{}
}
