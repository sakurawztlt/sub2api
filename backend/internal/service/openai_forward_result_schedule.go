package service

// SucceededForScheduling reports whether this result is an upstream success
// that may clear model-scoped transient state. The zero value remains a success
// for existing non-WebSocket callers.
func (r *OpenAIForwardResult) SucceededForScheduling() bool {
	if r == nil || !r.OpenAIWSMode || r.UpstreamTerminalEvent == "" {
		return true
	}
	switch r.UpstreamTerminalEvent {
	case "response.completed", "response.done":
		return true
	default:
		return false
	}
}
