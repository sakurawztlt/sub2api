package service

import (
	"fmt"
	"strings"
)

// Values interpolated into an upstream URL must be structurally inert path
// segments. Keep this as a closed allowlist: broadening it requires proving
// that the additional character cannot alter URL path/query semantics.
const (
	maxUpstreamPathSegmentLen = 128
	maxUpstreamPathSegments   = 8
)

func isSafeUpstreamPathSegmentByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_', b == '-', b == '.':
		return true
	default:
		return false
	}
}

func isSafeUpstreamPathSegment(segment string) bool {
	if segment == "" || len(segment) > maxUpstreamPathSegmentLen {
		return false
	}
	dotsOnly := true
	for i := 0; i < len(segment); i++ {
		if !isSafeUpstreamPathSegmentByte(segment[i]) {
			return false
		}
		if segment[i] != '.' {
			dotsOnly = false
		}
	}
	return !dotsOnly
}

// sanitizedUpstreamPathSuffix validates an optional /a/b suffix. It never
// rewrites invalid input into a different valid request.
func sanitizedUpstreamPathSuffix(raw string) (string, bool) {
	if raw != strings.TrimSpace(raw) {
		return "", false
	}
	suffix := raw
	if suffix == "" {
		return "", true
	}
	if !strings.HasPrefix(suffix, "/") {
		return "", false
	}
	segments := strings.Split(strings.TrimPrefix(suffix, "/"), "/")
	if len(segments) > maxUpstreamPathSegments {
		return "", false
	}
	for _, segment := range segments {
		if !isSafeUpstreamPathSegment(segment) {
			return "", false
		}
	}
	return suffix, true
}

func validateUpstreamPathSegment(kind, segment string) error {
	if isSafeUpstreamPathSegment(strings.TrimSpace(segment)) {
		return nil
	}
	// Do not echo the attacker-controlled input into logs or client errors.
	return fmt.Errorf("invalid %s for upstream url path", kind)
}
