package service

import (
	"net/http"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// GrokMediaEligibleExtraKey is an optional per-account media-routing override.
const GrokMediaEligibleExtraKey = "grok_media_eligible"

// ValidateGrokMediaEligibilityExtra validates the optional per-account media
// routing override. A nil value removes the override and restores automatic
// provider-observation routing.
func ValidateGrokMediaEligibilityExtra(platform string, extra map[string]any) error {
	if platform != PlatformGrok || extra == nil {
		return nil
	}
	raw, exists := extra[GrokMediaEligibleExtraKey]
	if !exists || raw == nil {
		return nil
	}
	if _, ok := raw.(bool); !ok {
		return infraerrors.BadRequest("GROK_MEDIA_ELIGIBILITY_INVALID", "grok_media_eligible must be a boolean or null")
	}
	return nil
}

func normalizeGrokMediaEligibilityExtra(platform string, extra map[string]any) (map[string]any, error) {
	if platform != PlatformGrok {
		return extra, nil
	}
	if err := ValidateGrokMediaEligibilityExtra(platform, extra); err != nil {
		return nil, err
	}
	if extra == nil {
		return nil, nil
	}
	normalized := shallowCopyMap(extra)
	if normalized[GrokMediaEligibleExtraKey] == nil {
		delete(normalized, GrokMediaEligibleExtraKey)
	}
	return normalized, nil
}

func normalizeGrokMediaEligibilityUpdateExtra(account *Account, input *UpdateAccountInput, normalized map[string]any) (map[string]any, error) {
	if account == nil || account.Platform != PlatformGrok {
		return normalized, nil
	}
	if input == nil {
		return nil, infraerrors.BadRequest("INVALID_ACCOUNT_INPUT", "account update input is required")
	}
	if err := ValidateGrokMediaEligibilityExtra(account.Platform, input.Extra); err != nil {
		return nil, err
	}
	if normalized == nil {
		normalized = make(map[string]any)
	} else {
		normalized = shallowCopyMap(normalized)
	}
	raw, provided := input.Extra[GrokMediaEligibleExtraKey]
	if provided {
		if raw == nil {
			delete(normalized, GrokMediaEligibleExtraKey)
		}
		return normalized, nil
	}
	if current, ok := account.Extra[GrokMediaEligibleExtraKey].(bool); ok {
		normalized[GrokMediaEligibleExtraKey] = current
	}
	return normalized, nil
}

// GrokMediaGenerationEligibility reports whether a Grok account may receive
// a new image/video generation request. Explicit operator policy wins; OAuth
// accounts otherwise require positive paid-entitlement evidence.
func (a *Account) GrokMediaGenerationEligibility() (bool, string) {
	if a == nil || !a.IsGrok() {
		return false, "not_grok"
	}
	if override, ok := grokMediaEligibilityOverride(a.Extra); ok {
		if override {
			return true, "override_enabled"
		}
		return false, "override_disabled"
	}
	if a.Type != AccountTypeOAuth {
		return true, "non_oauth"
	}
	billing, err := grokBillingSnapshotFromExtra(a.Extra)
	if err != nil || billing == nil {
		return false, "billing_unobserved"
	}
	if billing.StatusCode == http.StatusForbidden || billing.WeeklyStatusCode == http.StatusForbidden || billing.MonthlyStatusCode == http.StatusForbidden {
		return false, "billing_forbidden"
	}
	if isKnownGrokFreeAccount(a) {
		return false, "billing_free_tier"
	}
	if !grokBillingHasAuthoritativeQuota(billing) {
		return false, "billing_inconclusive"
	}
	return true, "eligible"
}

func grokMediaEligibilityOverride(extra map[string]any) (bool, bool) {
	if extra == nil {
		return false, false
	}
	raw, exists := extra[GrokMediaEligibleExtraKey]
	if !exists || raw == nil {
		return false, false
	}
	value, ok := raw.(bool)
	return value, ok
}
