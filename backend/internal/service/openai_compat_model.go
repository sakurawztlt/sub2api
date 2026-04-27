package service

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

func NormalizeOpenAICompatRequestedModel(model string) string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return ""
	}

	normalized, _, ok := splitOpenAICompatReasoningModel(trimmed)
	if !ok || normalized == "" {
		return trimmed
	}
	return normalized
}

func applyOpenAICompatModelNormalization(req *apicompat.AnthropicRequest) {
	if req == nil {
		return
	}

	originalModel := strings.TrimSpace(req.Model)
	if originalModel == "" {
		return
	}

	normalizedModel, derivedEffort, hasReasoningSuffix := splitOpenAICompatReasoningModel(originalModel)
	if hasReasoningSuffix && normalizedModel != "" {
		req.Model = normalizedModel
	}

	if req.OutputConfig != nil && strings.TrimSpace(req.OutputConfig.Effort) != "" {
		return
	}

	claudeEffort := openAIReasoningEffortToClaudeOutputEffort(derivedEffort)
	if claudeEffort == "" {
		return
	}

	if req.OutputConfig == nil {
		req.OutputConfig = &apicompat.AnthropicOutputConfig{}
	}
	req.OutputConfig.Effort = claudeEffort
}

func splitOpenAICompatReasoningModel(model string) (normalizedModel string, reasoningEffort string, ok bool) {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return "", "", false
	}

	modelID := trimmed
	if strings.Contains(modelID, "/") {
		parts := strings.Split(modelID, "/")
		modelID = parts[len(parts)-1]
	}
	modelID = strings.TrimSpace(modelID)
	if !strings.HasPrefix(strings.ToLower(modelID), "gpt-") {
		return trimmed, "", false
	}

	if strings.HasSuffix(strings.TrimSpace(modelID), ")") {
		openParen := strings.LastIndex(modelID, "(")
		if openParen > 0 {
			suffix := strings.TrimSpace(strings.TrimSuffix(modelID[openParen+1:], ")"))
			if effort, valid := normalizeOpenAICompatReasoningSuffix(suffix); valid {
				baseModel := strings.TrimSpace(modelID[:openParen])
				if baseModel == "" {
					return trimmed, "", false
				}
				return normalizeCodexModel(baseModel), effort, true
			}
		}
	}

	parts := strings.FieldsFunc(strings.ToLower(modelID), func(r rune) bool {
		switch r {
		case '-', '_', ' ':
			return true
		default:
			return false
		}
	})
	if len(parts) == 0 {
		return trimmed, "", false
	}

	last := strings.NewReplacer("-", "", "_", "", " ", "").Replace(parts[len(parts)-1])
	reasoningEffort, ok = normalizeOpenAICompatReasoningSuffix(last)
	if !ok {
		return trimmed, "", false
	}

	return normalizeCodexModel(modelID), reasoningEffort, true
}

func normalizeOpenAICompatReasoningSuffix(raw string) (reasoningEffort string, ok bool) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return "", false
	}
	value = strings.NewReplacer("-", "", "_", "", " ", "").Replace(value)

	switch value {
	case "none", "minimal":
	case "low", "medium", "high":
		return value, true
	case "xhigh", "extrahigh":
		return "xhigh", true
	default:
		return "", false
	}
	return "", true
}

func openAIReasoningEffortToClaudeOutputEffort(effort string) string {
	switch strings.TrimSpace(effort) {
	case "low", "medium", "high":
		return effort
	case "xhigh":
		return "max"
	default:
		return ""
	}
}
