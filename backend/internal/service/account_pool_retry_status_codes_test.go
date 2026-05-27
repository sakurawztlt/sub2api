package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetPoolModeRetryStatusCodes(t *testing.T) {
	tests := []struct {
		name     string
		account  *Account
		expected []int
	}{
		{
			name:     "nil_account_returns_nil",
			account:  nil,
			expected: nil,
		},
		{
			name: "nil_credentials_returns_nil",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
			},
			expected: nil,
		},
		{
			name: "missing_key_returns_nil",
			account: &Account{
				Type:        AccountTypeAPIKey,
				Platform:    PlatformOpenAI,
				Credentials: map[string]any{"pool_mode": true},
			},
			expected: nil,
		},
		{
			name: "empty_slice_is_preserved",
			account: &Account{
				Credentials: map[string]any{
					"pool_mode_retry_status_codes": []any{},
				},
			},
			expected: []int{},
		},
		{
			name: "float64_values_from_json_are_normalized",
			account: &Account{
				Credentials: map[string]any{
					"pool_mode_retry_status_codes": []any{float64(429), float64(401), float64(403)},
				},
			},
			expected: []int{401, 403, 429},
		},
		{
			name: "json_number_values_supported",
			account: &Account{
				Credentials: map[string]any{
					"pool_mode_retry_status_codes": []any{json.Number("502"), json.Number("503")},
				},
			},
			expected: []int{502, 503},
		},
		{
			name: "string_values_supported",
			account: &Account{
				Credentials: map[string]any{
					"pool_mode_retry_status_codes": []any{"520", "529"},
				},
			},
			expected: []int{520, 529},
		},
		{
			name: "duplicates_are_deduped",
			account: &Account{
				Credentials: map[string]any{
					"pool_mode_retry_status_codes": []any{float64(429), float64(429), float64(401)},
				},
			},
			expected: []int{401, 429},
		},
		{
			name: "out_of_range_values_dropped",
			account: &Account{
				Credentials: map[string]any{
					"pool_mode_retry_status_codes": []any{float64(99), float64(600), float64(429)},
				},
			},
			expected: []int{429},
		},
		{
			name: "invalid_string_dropped",
			account: &Account{
				Credentials: map[string]any{
					"pool_mode_retry_status_codes": []any{"oops", float64(429)},
				},
			},
			expected: []int{429},
		},
		{
			name: "non_array_value_returns_nil",
			account: &Account{
				Credentials: map[string]any{
					"pool_mode_retry_status_codes": "not-an-array",
				},
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, tt.account.GetPoolModeRetryStatusCodes())
		})
	}
}

func TestIsPoolModeRetryableStatus(t *testing.T) {
	tests := []struct {
		name       string
		account    *Account
		statusCode int
		expected   bool
	}{
		{
			name:       "nil_account_uses_default",
			account:    nil,
			statusCode: 429,
			expected:   true,
		},
		{
			name: "missing_config_uses_default",
			account: &Account{
				Credentials: map[string]any{},
			},
			statusCode: 403,
			expected:   true,
		},
		{
			name: "default_rejects_non_default",
			account: &Account{
				Credentials: map[string]any{},
			},
			statusCode: 502,
			expected:   false,
		},
		{
			name: "custom_codes_override_default",
			account: &Account{
				Credentials: map[string]any{
					"pool_mode_retry_status_codes": []any{float64(502), float64(503)},
				},
			},
			statusCode: 502,
			expected:   true,
		},
		{
			name: "custom_codes_do_not_include_default_unless_configured",
			account: &Account{
				Credentials: map[string]any{
					"pool_mode_retry_status_codes": []any{float64(502)},
				},
			},
			statusCode: 429,
			expected:   false,
		},
		{
			name: "empty_custom_codes_disable_status_retry",
			account: &Account{
				Credentials: map[string]any{
					"pool_mode_retry_status_codes": []any{},
				},
			},
			statusCode: 429,
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, tt.account.IsPoolModeRetryableStatus(tt.statusCode))
		})
	}
}
