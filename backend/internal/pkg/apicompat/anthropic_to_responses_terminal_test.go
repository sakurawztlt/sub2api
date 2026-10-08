package apicompat

import "testing"

func TestAnthropicResponsesStreamTerminalReason(t *testing.T) {
	for _, stopReason := range []string{"max_tokens", "end_turn", "tool_use", ""} {
		name := stopReason
		if name == "" {
			name = "unspecified"
		}
		for _, synthetic := range []bool{false, true} {
			ending := "message_stop"
			if synthetic {
				ending = "synthetic_finalizer"
			}
			t.Run(name+"/"+ending, func(t *testing.T) {
				state := NewAnthropicEventToResponsesState()
				index := 0
				for _, event := range []*AnthropicStreamEvent{
					{Type: "message_start", Message: &AnthropicResponse{ID: "msg_terminal", Model: "claude-sonnet-4-5", Usage: AnthropicUsage{InputTokens: 7, CacheReadInputTokens: 3, CacheCreationInputTokens: 2}}},
					{Type: "content_block_start", Index: &index, ContentBlock: &AnthropicContentBlock{Type: "text"}},
					{Type: "content_block_delta", Index: &index, Delta: &AnthropicDelta{Type: "text_delta", Text: "partial answer"}},
					{Type: "content_block_stop", Index: &index},
					{Type: "message_delta", Delta: &AnthropicDelta{StopReason: stopReason}, Usage: &AnthropicUsage{OutputTokens: 5}},
					// A later usage update must not erase the already observed stop reason.
					{Type: "message_delta", Usage: &AnthropicUsage{OutputTokens: 6}},
				} {
					AnthropicEventToResponsesEvents(event, state)
				}

				var events []ResponsesStreamEvent
				if synthetic {
					events = FinalizeAnthropicResponsesStream(state)
				} else {
					events = AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "message_stop"}, state)
				}
				if len(events) == 0 {
					t.Fatal("stream ended without a terminal response")
				}
				terminal := events[len(events)-1]
				wantStatus := "completed"
				if stopReason == "max_tokens" {
					wantStatus = "incomplete"
				}
				if terminal.Type != "response."+wantStatus || terminal.Response == nil || terminal.Response.Status != wantStatus {
					t.Fatalf("terminal = %+v, want response.%s with matching status", terminal, wantStatus)
				}
				response := terminal.Response
				if stopReason == "max_tokens" {
					if response.IncompleteDetails == nil || response.IncompleteDetails.Reason != "max_output_tokens" {
						t.Fatalf("incomplete details = %+v, want max_output_tokens", response.IncompleteDetails)
					}
				} else if response.IncompleteDetails != nil {
					t.Fatalf("completed response has incomplete details: %+v", response.IncompleteDetails)
				}
				if len(response.Output) != 1 || len(response.Output[0].Content) != 1 || response.Output[0].Content[0].Text != "partial answer" {
					t.Fatalf("terminal lost open text output: %+v", response.Output)
				}
				if response.Usage == nil || response.Usage.InputTokens != 12 || response.Usage.OutputTokens != 6 || response.Usage.TotalTokens != 18 || response.Usage.CacheCreationInputTokens != 2 || response.Usage.InputTokensDetails == nil || response.Usage.InputTokensDetails.CachedTokens != 3 {
					t.Fatalf("terminal lost cached/input/output usage: %+v", response.Usage)
				}
				if repeated := AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "message_stop"}, state); len(repeated) != 0 {
					t.Fatalf("message_stop emitted duplicate terminal events: %+v", repeated)
				}
				if repeated := FinalizeAnthropicResponsesStream(state); len(repeated) != 0 {
					t.Fatalf("finalizer emitted duplicate terminal events: %+v", repeated)
				}
			})
		}
	}
}
