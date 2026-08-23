package service

import "strings"

// isSSECommentLine reports whether line is an SSE comment line (begins with
// ':' after optional whitespace) per the EventSource spec. Comment lines are
// ignored by clients but useful as keepalives across idle proxies.
func isSSECommentLine(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(trimmed, ":")
}
