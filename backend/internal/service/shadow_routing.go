package service

// parentHealthyForShadow reports whether a spark shadow's parent remains a
// usable OpenAI OAuth credential source. Missing, disabled, expired, or
// temporarily-unschedulable parents fail closed. Parent global 429/overload
// state deliberately does not block the independent spark quota dimension.
func parentHealthyForShadow(account *Account, lookup func(int64) *Account) bool {
	if account == nil || !account.IsShadow() {
		return true
	}
	parent := lookup(*account.ParentAccountID)
	if parent == nil {
		return false
	}
	return parent.IsOpenAIOAuth() && parent.IsCredentialUsableForShadow()
}

func sparkModelVariants() []string {
	out := make([]string, 0, 1)
	for alias, target := range codexModelMap {
		if alias == target && target == "gpt-5.3-codex-spark" {
			out = append(out, alias)
		}
	}
	return out
}

func defaultSparkShadowModelMapping() map[string]any {
	variants := sparkModelVariants()
	mapping := make(map[string]any, len(variants))
	for _, model := range variants {
		mapping[model] = model
	}
	return mapping
}
