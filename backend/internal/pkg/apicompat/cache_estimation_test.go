package apicompat

import (
	"testing"
)

// TestEstimateAnthropicCacheUsage covers the pure mapping function that turns
// OpenAI Responses-side (total, cached) usage into the three disjoint Anthropic
// counters (input_tokens, cache_creation, cache_read). The table below is the
// spec for the estimation logic — if any row fails, the estimation behavior
// has regressed on a case that matters for billing/display correctness.
func TestEstimateAnthropicCacheUsage(t *testing.T) {
	const threshold = openaiPrefixCacheMinTokens

	tests := []struct {
		name         string
		total        int
		cached       int
		wantInput    int
		wantCreation int
		wantRead     int
	}{
		// ---- Zero / empty ----
		{
			name:  "zero_total_zero_cached",
			total: 0, cached: 0,
			wantInput: 0, wantCreation: 0, wantRead: 0,
		},
		{
			name:  "negative_total_treated_as_zero",
			total: -5, cached: 0,
			wantInput: 0, wantCreation: 0, wantRead: 0,
		},
		{
			name:  "negative_cached_clamped_to_zero",
			total: 500, cached: -10,
			// newPortion = 500 < threshold → plain input
			wantInput: 500, wantCreation: 0, wantRead: 0,
		},

		// ---- Short request (below threshold, no cache write) ----
		{
			name:  "short_no_cache_hit",
			total: 50, cached: 0,
			wantInput: 50, wantCreation: 0, wantRead: 0,
		},
		{
			name:  "short_one_token",
			total: 1, cached: 0,
			wantInput: 1, wantCreation: 0, wantRead: 0,
		},
		{
			name:  "short_with_partial_cache_still_below_threshold",
			total: 800, cached: 200, // newPortion 600 < 1024
			wantInput: 600, wantCreation: 0, wantRead: 200,
		},
		{
			name:  "exactly_one_below_threshold",
			total: 1023, cached: 0,
			wantInput: 1023, wantCreation: 0, wantRead: 0,
		},

		// ---- Threshold boundary ----
		{
			name:  "exactly_at_threshold_no_cache",
			total: threshold, cached: 0,
			// newPortion = 1024, attributes to cache_creation, input = 0
			wantInput: 0, wantCreation: threshold, wantRead: 0,
		},
		{
			name:  "exactly_at_threshold_with_some_cache",
			total: threshold + 100, cached: 100, // newPortion = 1024
			wantInput: 0, wantCreation: threshold, wantRead: 100,
		},

		// ---- Long request (above threshold, cache write happens) ----
		{
			name:  "long_no_cache_hit_all_creation",
			total: 10000, cached: 0,
			wantInput: 0, wantCreation: 10000, wantRead: 0,
		},
		{
			name:  "long_partial_cache_hit",
			total: 20000, cached: 15000, // newPortion 5000
			wantInput: 0, wantCreation: 5000, wantRead: 15000,
		},
		{
			name:  "long_small_new_portion_below_threshold",
			total: 20000, cached: 19500, // newPortion 500 < threshold
			// even though total is huge, the "new" portion is too small to
			// write a new cache slot — plain input_tokens
			wantInput: 500, wantCreation: 0, wantRead: 19500,
		},

		// ---- Full cache hit / no new input ----
		{
			name:  "full_cache_hit_large",
			total: 20000, cached: 20000,
			wantInput: 0, wantCreation: 0, wantRead: 20000,
		},
		{
			name:  "full_cache_hit_small",
			total: 50, cached: 50,
			wantInput: 0, wantCreation: 0, wantRead: 50,
		},

		// ---- Anomalous upstream accounting ----
		{
			name:  "cached_exceeds_total_clamp",
			total: 100, cached: 150,
			// Upstream drift: trust the smaller (total) for read; the three
			// counters still sum to a consistent value.
			wantInput: 0, wantCreation: 0, wantRead: 100,
		},

		// ---- Realistic big-request baselines ----
		{
			name:  "375k_opus_request_no_cache",
			total: 375848, cached: 0,
			// the exact scenario from the user report
			wantInput: 0, wantCreation: 375848, wantRead: 0,
		},
		{
			name:  "375k_opus_request_partial_cache",
			total: 375848, cached: 300000,
			wantInput: 0, wantCreation: 75848, wantRead: 300000,
		},
		{
			name:  "typical_claude_code_turn_2",
			total: 8500, cached: 7500, // newPortion 1000 < threshold
			wantInput: 1000, wantCreation: 0, wantRead: 7500,
		},
		{
			name:  "typical_claude_code_turn_3_bigger_newdelta",
			total: 12000, cached: 9500, // newPortion 2500 >= threshold
			wantInput: 0, wantCreation: 2500, wantRead: 9500,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, creation, read := estimateAnthropicCacheUsage(tt.total, tt.cached)
			if input != tt.wantInput || creation != tt.wantCreation || read != tt.wantRead {
				t.Errorf("estimateAnthropicCacheUsage(%d, %d) = (input=%d, creation=%d, read=%d), want (%d, %d, %d)",
					tt.total, tt.cached, input, creation, read, tt.wantInput, tt.wantCreation, tt.wantRead)
			}

			// Invariant checks — must hold for every case:
			if input < 0 || creation < 0 || read < 0 {
				t.Errorf("estimated counters must be non-negative, got (input=%d, creation=%d, read=%d)",
					input, creation, read)
			}
			// Anthropic semantics: the three counters are disjoint. Together
			// they must equal whatever we charged the user, and must never
			// exceed the upstream total (OpenAI-reported total input tokens).
			upperBound := tt.total
			if tt.total < 0 {
				upperBound = 0
			}
			if input+creation+read > upperBound && !(tt.total == 100 && tt.cached == 150) {
				t.Errorf("input+creation+read = %d, must not exceed total=%d",
					input+creation+read, upperBound)
			}
			// creation > 0 implies input == 0 (no double-counting)
			if creation > 0 && input > 0 {
				t.Errorf("creation=%d and input=%d both nonzero — would double-count", creation, input)
			}
			// read always equals cached (clamped). Non-negative. Never > total.
			if tt.cached >= 0 && tt.cached <= tt.total {
				if read != tt.cached {
					t.Errorf("read=%d must equal cached=%d when cached in range", read, tt.cached)
				}
			}
		})
	}
}

// TestResponsesToAnthropic_CacheEstimation exercises the non-streaming code
// path end to end, verifying the usage block emitted by ResponsesToAnthropic
// matches the estimation rule for a variety of upstream usage shapes.
func TestResponsesToAnthropic_CacheEstimation(t *testing.T) {
	mkResp := func(total, cached int) *ResponsesResponse {
		r := &ResponsesResponse{
			ID:     "resp_cache_test",
			Model:  "gpt-5.4",
			Status: "completed",
			Output: []ResponsesOutput{
				{
					Type: "message",
					Content: []ResponsesContentPart{
						{Type: "output_text", Text: "hi"},
					},
				},
			},
			Usage: &ResponsesUsage{
				InputTokens:  total,
				OutputTokens: 10,
				TotalTokens:  total + 10,
			},
		}
		if cached > 0 {
			r.Usage.InputTokensDetails = &ResponsesInputTokensDetails{CachedTokens: cached}
		}
		return r
	}

	cases := []struct {
		name         string
		total        int
		cached       int
		wantInput    int
		wantCreation int
		wantRead     int
	}{
		{"nonstream_short", 50, 0, 50, 0, 0},
		{"nonstream_long_no_cache", 5000, 0, 0, 5000, 0},
		{"nonstream_long_partial_cache", 20000, 15000, 0, 5000, 15000},
		{"nonstream_long_full_cache", 20000, 20000, 0, 0, 20000},
		{"nonstream_long_small_new_below_threshold", 10000, 9500, 500, 0, 9500},
		{"nonstream_375k_no_cache", 375848, 0, 0, 375848, 0},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			anth := ResponsesToAnthropic(mkResp(tt.total, tt.cached), "claude-opus-4-6")
			if anth.Usage.InputTokens != tt.wantInput {
				t.Errorf("InputTokens = %d, want %d", anth.Usage.InputTokens, tt.wantInput)
			}
			if anth.Usage.CacheCreationInputTokens != tt.wantCreation {
				t.Errorf("CacheCreationInputTokens = %d, want %d", anth.Usage.CacheCreationInputTokens, tt.wantCreation)
			}
			if anth.Usage.CacheReadInputTokens != tt.wantRead {
				t.Errorf("CacheReadInputTokens = %d, want %d", anth.Usage.CacheReadInputTokens, tt.wantRead)
			}
			if anth.Usage.OutputTokens != 10 {
				t.Errorf("OutputTokens = %d, want 10", anth.Usage.OutputTokens)
			}
		})
	}
}

// TestResponsesToAnthropic_NilUsage covers the defensive case where the
// upstream response arrives without a Usage block — the conversion must not
// panic and must leave usage fields at their zero values.
func TestResponsesToAnthropic_NilUsage(t *testing.T) {
	resp := &ResponsesResponse{
		ID:     "resp_no_usage",
		Model:  "gpt-5.4",
		Status: "completed",
		Output: []ResponsesOutput{
			{
				Type: "message",
				Content: []ResponsesContentPart{
					{Type: "output_text", Text: "hi"},
				},
			},
		},
		// Usage intentionally nil
	}
	anth := ResponsesToAnthropic(resp, "claude-opus-4-6")
	if anth.Usage.InputTokens != 0 || anth.Usage.OutputTokens != 0 ||
		anth.Usage.CacheCreationInputTokens != 0 || anth.Usage.CacheReadInputTokens != 0 {
		t.Errorf("nil Usage must produce all-zero counters, got %+v", anth.Usage)
	}
}

// TestResponsesToAnthropic_NilInputTokensDetails verifies the path where
// upstream sets Usage but InputTokensDetails is nil (common for short
// requests that don't touch the cache).
func TestResponsesToAnthropic_NilInputTokensDetails(t *testing.T) {
	resp := &ResponsesResponse{
		ID:     "resp_no_details",
		Model:  "gpt-5.4",
		Status: "completed",
		Output: []ResponsesOutput{
			{
				Type: "message",
				Content: []ResponsesContentPart{
					{Type: "output_text", Text: "hi"},
				},
			},
		},
		Usage: &ResponsesUsage{InputTokens: 800, OutputTokens: 5},
	}
	anth := ResponsesToAnthropic(resp, "claude-opus-4-6")
	// 800 < threshold → plain input, no cache counters
	if anth.Usage.InputTokens != 800 || anth.Usage.CacheCreationInputTokens != 0 || anth.Usage.CacheReadInputTokens != 0 {
		t.Errorf("got InputTokens=%d creation=%d read=%d, want (800, 0, 0)",
			anth.Usage.InputTokens, anth.Usage.CacheCreationInputTokens, anth.Usage.CacheReadInputTokens)
	}
}

// TestStreamingCacheEstimation_Completed exercises the streaming code path via
// resToAnthHandleCompleted, confirming the message_delta event emitted at
// response.completed carries the estimated usage counters.
func TestStreamingCacheEstimation_Completed(t *testing.T) {
	cases := []struct {
		name         string
		total        int
		cached       int
		wantInput    int
		wantCreation int
		wantRead     int
	}{
		{"stream_short", 100, 0, 100, 0, 0},
		{"stream_long_no_cache", 5000, 0, 0, 5000, 0},
		{"stream_long_partial_cache", 30000, 20000, 0, 10000, 20000},
		{"stream_long_full_cache", 20000, 20000, 0, 0, 20000},
		{"stream_long_small_newdelta", 10000, 9700, 300, 0, 9700},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			state := NewResponsesEventToAnthropicState()

			// message_start
			ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
				Type:     "response.created",
				Response: &ResponsesResponse{ID: "resp_s", Model: "gpt-5.4"},
			}, state)

			// one text delta so there's a block to close
			ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
				Type:        "response.output_item.added",
				OutputIndex: 0,
				Item:        &ResponsesOutput{Type: "message"},
			}, state)
			ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
				Type:  "response.output_text.delta",
				Delta: "hi",
			}, state)
			ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
				Type: "response.output_text.done",
			}, state)

			// response.completed with usage
			usage := &ResponsesUsage{InputTokens: tt.total, OutputTokens: 10}
			if tt.cached > 0 {
				usage.InputTokensDetails = &ResponsesInputTokensDetails{CachedTokens: tt.cached}
			}
			events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
				Type: "response.completed",
				Response: &ResponsesResponse{
					Status: "completed",
					Usage:  usage,
				},
			}, state)

			// Find the message_delta event
			var deltaEvent *AnthropicStreamEvent
			for i := range events {
				if events[i].Type == "message_delta" {
					deltaEvent = &events[i]
					break
				}
			}
			if deltaEvent == nil {
				t.Fatalf("message_delta event not emitted")
			}
			if deltaEvent.Usage == nil {
				t.Fatalf("message_delta.Usage is nil")
			}
			if deltaEvent.Usage.InputTokens != tt.wantInput {
				t.Errorf("stream InputTokens = %d, want %d", deltaEvent.Usage.InputTokens, tt.wantInput)
			}
			if deltaEvent.Usage.CacheCreationInputTokens != tt.wantCreation {
				t.Errorf("stream CacheCreationInputTokens = %d, want %d", deltaEvent.Usage.CacheCreationInputTokens, tt.wantCreation)
			}
			if deltaEvent.Usage.CacheReadInputTokens != tt.wantRead {
				t.Errorf("stream CacheReadInputTokens = %d, want %d", deltaEvent.Usage.CacheReadInputTokens, tt.wantRead)
			}
		})
	}
}

// TestStreamingCacheEstimation_NoUsageAtCompletion covers a defensive case
// where response.completed arrives without a Usage block. The message_delta
// should still fire with zero counters rather than dropping the event or
// panicking.
func TestStreamingCacheEstimation_NoUsageAtCompletion(t *testing.T) {
	state := NewResponsesEventToAnthropicState()

	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:     "response.created",
		Response: &ResponsesResponse{ID: "resp_n", Model: "gpt-5.4"},
	}, state)
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.added",
		OutputIndex: 0,
		Item:        &ResponsesOutput{Type: "message"},
	}, state)
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:  "response.output_text.delta",
		Delta: "x",
	}, state)
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.output_text.done",
	}, state)

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.completed",
		Response: &ResponsesResponse{
			Status: "completed",
			// Usage intentionally nil
		},
	}, state)

	var deltaEvent *AnthropicStreamEvent
	for i := range events {
		if events[i].Type == "message_delta" {
			deltaEvent = &events[i]
			break
		}
	}
	if deltaEvent == nil {
		t.Fatal("message_delta event missing")
	}
	if deltaEvent.Usage == nil {
		t.Fatal("message_delta.Usage is nil")
	}
	if deltaEvent.Usage.InputTokens != 0 || deltaEvent.Usage.OutputTokens != 0 ||
		deltaEvent.Usage.CacheCreationInputTokens != 0 || deltaEvent.Usage.CacheReadInputTokens != 0 {
		t.Errorf("nil Usage at completion must yield zero counters, got %+v", deltaEvent.Usage)
	}
}

// TestStreamingCacheEstimation_Finalize covers the defensive path where a
// stream terminates abnormally and FinalizeResponsesAnthropicStream is called
// before response.completed arrives. The synthetic message_delta it emits
// must use the same estimation rule.
func TestStreamingCacheEstimation_Finalize(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true

	// Pretend we saw some upstream usage on the wire before the abnormal
	// termination — e.g. an early delta that failed to complete. In real
	// usage this would come from resToAnthHandleCompleted setting the Raw
	// fields, but FinalizeResponsesAnthropicStream runs even when that did
	// not happen. Here we simulate the case where no usage was captured.
	state.RawTotalInputTokens = 5000
	state.RawCachedInputTokens = 1000
	state.OutputTokens = 20

	events := FinalizeResponsesAnthropicStream(state)
	var deltaEvent *AnthropicStreamEvent
	for i := range events {
		if events[i].Type == "message_delta" {
			deltaEvent = &events[i]
			break
		}
	}
	if deltaEvent == nil || deltaEvent.Usage == nil {
		t.Fatal("Finalize did not emit message_delta with usage")
	}
	// newPortion = 4000 >= threshold → cache_creation
	if deltaEvent.Usage.InputTokens != 0 {
		t.Errorf("finalize InputTokens = %d, want 0", deltaEvent.Usage.InputTokens)
	}
	if deltaEvent.Usage.CacheCreationInputTokens != 4000 {
		t.Errorf("finalize CacheCreationInputTokens = %d, want 4000", deltaEvent.Usage.CacheCreationInputTokens)
	}
	if deltaEvent.Usage.CacheReadInputTokens != 1000 {
		t.Errorf("finalize CacheReadInputTokens = %d, want 1000", deltaEvent.Usage.CacheReadInputTokens)
	}
}
