package upstreammodel

import "strings"

// MaxLength matches usage_logs.upstream_response_model VARCHAR(200).
const MaxLength = 200

// Normalize trims an untrusted upstream model declaration and bounds it by
// Unicode code points before it can reach persistence or diagnostic logs.
func Normalize(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	runes := []rune(model)
	if len(runes) > MaxLength {
		return string(runes[:MaxLength])
	}
	return model
}
