package apicompat

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// AnthropicToResponses tests
// ---------------------------------------------------------------------------

func TestAnthropicToResponses_BasicText(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Stream:    true,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	assert.Equal(t, "gpt-5.2", resp.Model)
	assert.True(t, resp.Stream)
	assert.Equal(t, 1024, *resp.MaxOutputTokens)
	assert.False(t, *resp.Store)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	require.Len(t, items, 1)
	assert.Equal(t, "user", items[0].Role)
}

func TestAnthropicToResponses_SystemPrompt(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		req := &AnthropicRequest{
			Model:     "gpt-5.2",
			MaxTokens: 100,
			System:    json.RawMessage(`"You are helpful."`),
			Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
		}
		resp, err := AnthropicToResponses(req)
		require.NoError(t, err)

		var items []ResponsesInputItem
		require.NoError(t, json.Unmarshal(resp.Input, &items))
		require.Len(t, items, 2)
		// 058 step 2: system prompt → developer role with typed input_text parts.
		assert.Equal(t, "message", items[0].Type)
		assert.Equal(t, "developer", items[0].Role)
		var parts []ResponsesContentPart
		require.NoError(t, json.Unmarshal(items[0].Content, &parts))
		require.Len(t, parts, 1)
		assert.Equal(t, "input_text", parts[0].Type)
		assert.Equal(t, "You are helpful.", parts[0].Text)
	})

	t.Run("array", func(t *testing.T) {
		req := &AnthropicRequest{
			Model:     "gpt-5.2",
			MaxTokens: 100,
			System:    json.RawMessage(`[{"type":"text","text":"Part 1"},{"type":"text","text":"Part 2"}]`),
			Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
		}
		resp, err := AnthropicToResponses(req)
		require.NoError(t, err)

		var items []ResponsesInputItem
		require.NoError(t, json.Unmarshal(resp.Input, &items))
		require.Len(t, items, 2)
		assert.Equal(t, "message", items[0].Type)
		assert.Equal(t, "developer", items[0].Role)
		// 058 step 2: system text array → multiple typed input_text parts (no \n\n join).
		var parts []ResponsesContentPart
		require.NoError(t, json.Unmarshal(items[0].Content, &parts))
		require.Len(t, parts, 2)
		assert.Equal(t, "input_text", parts[0].Type)
		assert.Equal(t, "Part 1", parts[0].Text)
		assert.Equal(t, "input_text", parts[1].Type)
		assert.Equal(t, "Part 2", parts[1].Text)
	})

	t.Run("billing header dropped", func(t *testing.T) {
		req := &AnthropicRequest{
			Model:     "gpt-5.2",
			MaxTokens: 100,
			System:    json.RawMessage(`[{"type":"text","text":"x-anthropic-billing-header: cc_version=1;"},{"type":"text","text":"Project prompt"}]`),
			Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
		}
		resp, err := AnthropicToResponses(req)
		require.NoError(t, err)

		var items []ResponsesInputItem
		require.NoError(t, json.Unmarshal(resp.Input, &items))
		require.Len(t, items, 2)
		var parts []ResponsesContentPart
		require.NoError(t, json.Unmarshal(items[0].Content, &parts))
		require.Len(t, parts, 1)
		assert.Equal(t, "Project prompt", parts[0].Text)
	})
}

func TestAnthropicToResponses_ToolUse(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"What is the weather?"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"Let me check."},{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"NYC"}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_1","content":"Sunny, 72°F"}]`)},
		},
		Tools: []AnthropicTool{
			{Name: "get_weather", Description: "Get weather", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	// Check tools
	require.Len(t, resp.Tools, 1)
	assert.Equal(t, "function", resp.Tools[0].Type)
	assert.Equal(t, "get_weather", resp.Tools[0].Name)

	// Check input items
	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	// user + assistant + function_call + function_call_output = 4
	require.Len(t, items, 4)

	assert.Equal(t, "user", items[0].Role)
	assert.Equal(t, "assistant", items[1].Role)
	assert.Equal(t, "function_call", items[2].Type)
	// 058 step 2: call_id is preserved verbatim (no fc_ prefix).
	assert.Equal(t, "call_1", items[2].CallID)
	assert.Empty(t, items[2].ID)
	assert.Equal(t, "function_call_output", items[3].Type)
	assert.Equal(t, "call_1", items[3].CallID)
	assert.Equal(t, "Sunny, 72°F", items[3].Output)

	// 058 step 2: function tools must include strict=false to match Codex CLI.
	require.NotNil(t, resp.Tools[0].Strict)
	assert.False(t, *resp.Tools[0].Strict)
}

func TestAnthropicToResponses_ThinkingIgnored(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"deep thought"},{"type":"text","text":"Hi!"}]`)},
			{Role: "user", Content: json.RawMessage(`"More"`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	// user + assistant(text only, thinking ignored) + user = 3
	require.Len(t, items, 3)
	assert.Equal(t, "assistant", items[1].Role)
	// Assistant content should only have text, not thinking.
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[1].Content, &parts))
	require.Len(t, parts, 1)
	assert.Equal(t, "output_text", parts[0].Type)
	assert.Equal(t, "Hi!", parts[0].Text)
}

func TestAnthropicToResponses_MaxTokensFloor(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 10, // below minMaxOutputTokens (128)
		Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	assert.Equal(t, 128, *resp.MaxOutputTokens)
}

// ---------------------------------------------------------------------------
// ResponsesToAnthropic (non-streaming) tests
// ---------------------------------------------------------------------------

func TestResponsesToAnthropic_TextOnly(t *testing.T) {
	resp := &ResponsesResponse{
		ID:     "resp_123",
		Model:  "gpt-5.2",
		Status: "completed",
		Output: []ResponsesOutput{
			{
				Type: "message",
				Content: []ResponsesContentPart{
					{Type: "output_text", Text: "Hello there!"},
				},
			},
		},
		Usage: &ResponsesUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}

	anth := ResponsesToAnthropic(resp, "claude-opus-4-6")
	// The Anthropic-facing id is synthesised (msg_01...), NOT the upstream
	// OpenAI resp_<hex>. Leaking resp_ would reveal B-track impersonation.
	assert.True(t, strings.HasPrefix(anth.ID, "msg_01"), "id should start with msg_01, got %s", anth.ID)
	assert.NotEqual(t, "resp_123", anth.ID)
	assert.Equal(t, "claude-opus-4-6", anth.Model)
	assert.Equal(t, "end_turn", anth.StopReason)
	require.Len(t, anth.Content, 1)
	assert.Equal(t, "text", anth.Content[0].Type)
	assert.Equal(t, "Hello there!", anth.Content[0].Text)
	assert.Equal(t, 10, anth.Usage.InputTokens)
	assert.Equal(t, 5, anth.Usage.OutputTokens)
}

func TestResponsesToAnthropic_CachedTokensUseAnthropicInputSemantics(t *testing.T) {
	resp := &ResponsesResponse{
		ID:     "resp_cached",
		Model:  "gpt-5.2",
		Status: "completed",
		Output: []ResponsesOutput{
			{
				Type: "message",
				Content: []ResponsesContentPart{
					{Type: "output_text", Text: "Cached response"},
				},
			},
		},
		Usage: &ResponsesUsage{
			InputTokens:  54006,
			OutputTokens: 123,
			TotalTokens:  54129,
			InputTokensDetails: &ResponsesInputTokensDetails{
				CachedTokens: 50688,
			},
		},
	}

	anth := ResponsesToAnthropic(resp, "claude-sonnet-4-5-20250929")
	// Fork ledger v1: newPortion (3318) >= openaiPrefixCacheMinTokens (1024) → attribute
	// to cache_creation, not input. Sum invariant: input + creation + read == total.
	assert.Equal(t, 0, anth.Usage.InputTokens)
	assert.Equal(t, 3318, anth.Usage.CacheCreationInputTokens)
	assert.Equal(t, 50688, anth.Usage.CacheReadInputTokens)
	assert.Equal(t, 123, anth.Usage.OutputTokens)
}

func TestResponsesToAnthropic_CachedTokensClampInputTokens(t *testing.T) {
	resp := &ResponsesResponse{
		ID:     "resp_cached_clamp",
		Model:  "gpt-5.2",
		Status: "completed",
		Usage: &ResponsesUsage{
			InputTokens:  100,
			OutputTokens: 5,
			InputTokensDetails: &ResponsesInputTokensDetails{
				CachedTokens: 150,
			},
		},
	}

	anth := ResponsesToAnthropic(resp, "claude-sonnet-4-5-20250929")
	// Fork ledger v1: cached > total drift → clamp read to total so the three
	// disjoint counters still sum consistently. Prevents NewAPI over-billing.
	assert.Equal(t, 0, anth.Usage.InputTokens)
	assert.Equal(t, 100, anth.Usage.CacheReadInputTokens)
	assert.Equal(t, 5, anth.Usage.OutputTokens)
}

func TestResponsesToAnthropic_ToolUse(t *testing.T) {
	resp := &ResponsesResponse{
		ID:     "resp_456",
		Model:  "gpt-5.2",
		Status: "completed",
		Output: []ResponsesOutput{
			{
				Type: "message",
				Content: []ResponsesContentPart{
					{Type: "output_text", Text: "Let me check."},
				},
			},
			{
				Type:      "function_call",
				CallID:    "call_1",
				Name:      "get_weather",
				Arguments: `{"city":"NYC"}`,
			},
		},
	}

	anth := ResponsesToAnthropic(resp, "claude-opus-4-6")
	assert.Equal(t, "tool_use", anth.StopReason)
	require.Len(t, anth.Content, 2)
	assert.Equal(t, "text", anth.Content[0].Type)
	assert.Equal(t, "tool_use", anth.Content[1].Type)
	// 2026-05-07 codex 伪装泄漏修复: OpenAI call_xxx 必须映射成 toolu_xxx
	// 给 Anthropic 客户, 不能原样返回 (call_xxx 是 OpenAI 风格暴露).
	assert.Equal(t, "toolu_1", anth.Content[1].ID)
	assert.Equal(t, "get_weather", anth.Content[1].Name)
	assert.JSONEq(t, `{"city":"NYC"}`, string(anth.Content[1].Input))
}

// 2026-05-07 codex 伪装泄漏修复: 验证 toResponsesCallID + fromResponsesCallID
// round trip lossless. 客户上来 toolu_xxx → toResponsesCallID → call_xxx 给
// OpenAI 上游 → OpenAI 返 call_xxx → fromResponsesCallID → toolu_xxx 给客户.
func TestCallIDRoundTripLossless(t *testing.T) {
	cases := []string{
		"AAA",                          // 任意字符
		"ABCdef123",                    // alphanum
		"Pf2nMQX1m9",                   // OpenAI 真实风格
		"01HV6X8N3P9Q2K7T4M5R6Y8Z3W",   // ULID 风格
		"a-b_c.d~e",                    // 含 url-safe 特殊字符
	}
	for _, body := range cases {
		anthID := "toolu_" + body
		openaiID := toResponsesCallID(anthID)
		require.Equal(t, "call_"+body, openaiID, "toolu_→call_ 前缀替换")
		backToAnth := fromResponsesCallID(openaiID)
		require.Equal(t, anthID, backToAnth, "round trip lossless: %s", anthID)
	}
}

// 客户上来 call_xxx (历史泄漏的 OpenAI 风格 id) 仍能 work — passthrough 给上游
func TestToResponsesCallID_PassthroughCallPrefix(t *testing.T) {
	assert.Equal(t, "call_AAA", toResponsesCallID("call_AAA"))
}

// 客户上来 toolu_xxx 是 Anthropic 标准, fromResponsesCallID passthrough
func TestFromResponsesCallID_PassthroughTooluPrefix(t *testing.T) {
	assert.Equal(t, "toolu_AAA", fromResponsesCallID("toolu_AAA"))
}

// fc_ 老格式 legacy 兼容
func TestFromResponsesCallID_LegacyFcPrefix(t *testing.T) {
	// fc_call_xxx → toolu_xxx (剥 fc_ 后 call_xxx 再映射)
	assert.Equal(t, "toolu_AAA", fromResponsesCallID("fc_call_AAA"))
	// fc_toolu_xxx → toolu_xxx (剥 fc_)
	assert.Equal(t, "toolu_AAA", fromResponsesCallID("fc_toolu_AAA"))
}

func TestResponsesToAnthropic_ReadToolDropsEmptyPages(t *testing.T) {
	resp := &ResponsesResponse{
		ID:     "resp_read",
		Model:  "gpt-5.5",
		Status: "completed",
		Output: []ResponsesOutput{
			{
				Type:      "function_call",
				CallID:    "call_read",
				Name:      "Read",
				Arguments: `{"file_path":"/tmp/demo.py","limit":2000,"offset":0,"pages":""}`,
			},
		},
	}

	anth := ResponsesToAnthropic(resp, "claude-opus-4-6")
	require.Len(t, anth.Content, 1)
	assert.Equal(t, "tool_use", anth.Content[0].Type)
	assert.JSONEq(t, `{"file_path":"/tmp/demo.py","limit":2000,"offset":0}`, string(anth.Content[0].Input))
}

func TestResponsesToAnthropic_PreservesEmptyStringsForOtherTools(t *testing.T) {
	resp := &ResponsesResponse{
		ID:     "resp_other",
		Model:  "gpt-5.5",
		Status: "completed",
		Output: []ResponsesOutput{
			{
				Type:      "function_call",
				CallID:    "call_other",
				Name:      "Search",
				Arguments: `{"query":""}`,
			},
		},
	}

	anth := ResponsesToAnthropic(resp, "claude-opus-4-6")
	require.Len(t, anth.Content, 1)
	assert.JSONEq(t, `{"query":""}`, string(anth.Content[0].Input))
}

func TestResponsesToAnthropic_Reasoning(t *testing.T) {
	resp := &ResponsesResponse{
		ID:     "resp_789",
		Model:  "gpt-5.2",
		Status: "completed",
		Output: []ResponsesOutput{
			{
				Type: "reasoning",
				Summary: []ResponsesSummary{
					{Type: "summary_text", Text: "Thinking about the answer..."},
				},
			},
			{
				Type: "message",
				Content: []ResponsesContentPart{
					{Type: "output_text", Text: "42"},
				},
			},
		},
	}

	anth := ResponsesToAnthropic(resp, "claude-opus-4-6")
	require.Len(t, anth.Content, 2)
	assert.Equal(t, "thinking", anth.Content[0].Type)
	assert.Equal(t, "Thinking about the answer...", anth.Content[0].Thinking)
	assert.Equal(t, "text", anth.Content[1].Type)
	assert.Equal(t, "42", anth.Content[1].Text)
}

func TestResponsesToAnthropic_Incomplete(t *testing.T) {
	resp := &ResponsesResponse{
		ID:     "resp_inc",
		Model:  "gpt-5.2",
		Status: "incomplete",
		IncompleteDetails: &ResponsesIncompleteDetails{
			Reason: "max_output_tokens",
		},
		Output: []ResponsesOutput{
			{
				Type:    "message",
				Content: []ResponsesContentPart{{Type: "output_text", Text: "Partial..."}},
			},
		},
	}

	anth := ResponsesToAnthropic(resp, "claude-opus-4-6")
	assert.Equal(t, "max_tokens", anth.StopReason)
}

func TestResponsesToAnthropic_EmptyOutput(t *testing.T) {
	resp := &ResponsesResponse{
		ID:     "resp_empty",
		Model:  "gpt-5.2",
		Status: "completed",
		Output: []ResponsesOutput{},
	}

	anth := ResponsesToAnthropic(resp, "claude-opus-4-6")
	require.Len(t, anth.Content, 1)
	assert.Equal(t, "text", anth.Content[0].Type)
	assert.Equal(t, "", anth.Content[0].Text)
}

// ---------------------------------------------------------------------------
// Streaming: ResponsesEventToAnthropicEvents tests
// ---------------------------------------------------------------------------

func TestStreamingTextOnly(t *testing.T) {
	state := NewResponsesEventToAnthropicState()

	// 1. response.created
	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.created",
		Response: &ResponsesResponse{
			ID:    "resp_1",
			Model: "gpt-5.2",
		},
	}, state)
	require.Len(t, events, 1)
	assert.Equal(t, "message_start", events[0].Type)

	// 2. output_item.added (message)
	events = ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.added",
		OutputIndex: 0,
		Item:        &ResponsesOutput{Type: "message"},
	}, state)
	assert.Len(t, events, 0) // message item doesn't emit events

	// 3. text delta
	events = ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:  "response.output_text.delta",
		Delta: "Hello",
	}, state)
	require.Len(t, events, 2) // content_block_start + content_block_delta
	assert.Equal(t, "content_block_start", events[0].Type)
	assert.Equal(t, "text", events[0].ContentBlock.Type)
	assert.Equal(t, "content_block_delta", events[1].Type)
	assert.Equal(t, "text_delta", events[1].Delta.Type)
	assert.Equal(t, "Hello", events[1].Delta.Text)

	// 4. more text
	events = ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:  "response.output_text.delta",
		Delta: " world",
	}, state)
	require.Len(t, events, 1) // only delta, no new block start
	assert.Equal(t, "content_block_delta", events[0].Type)

	// 5. text done
	events = ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.output_text.done",
	}, state)
	require.Len(t, events, 1)
	assert.Equal(t, "content_block_stop", events[0].Type)

	// 6. completed
	events = ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.completed",
		Response: &ResponsesResponse{
			Status: "completed",
			Usage:  &ResponsesUsage{InputTokens: 10, OutputTokens: 5},
		},
	}, state)
	require.Len(t, events, 2) // message_delta + message_stop
	assert.Equal(t, "message_delta", events[0].Type)
	assert.Equal(t, "end_turn", events[0].Delta.StopReason)
	assert.Equal(t, 10, events[0].Usage.InputTokens)
	assert.Equal(t, 5, events[0].Usage.OutputTokens)
	assert.Equal(t, "message_stop", events[1].Type)
}

func TestResponsesEventToAnthropicEvents_ResponseDone(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	state.Model = "gpt-4o"

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.done",
		Response: &ResponsesResponse{
			Status: "completed",
			Usage:  &ResponsesUsage{InputTokens: 12, OutputTokens: 4},
		},
	}, state)
	require.Len(t, events, 2)
	assert.Equal(t, "message_delta", events[0].Type)
	assert.Equal(t, "end_turn", events[0].Delta.StopReason)
	assert.Equal(t, 12, events[0].Usage.InputTokens)
	assert.Equal(t, 4, events[0].Usage.OutputTokens)
	assert.Equal(t, "message_stop", events[1].Type)
	assert.Nil(t, FinalizeResponsesAnthropicStream(state))
}

func TestResponsesEventToAnthropicEvents_ResponseDoneIncomplete(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	state.Model = "gpt-4o"

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.done",
		Response: &ResponsesResponse{
			Status:            "incomplete",
			IncompleteDetails: &ResponsesIncompleteDetails{Reason: "max_output_tokens"},
			Usage:             &ResponsesUsage{InputTokens: 12, OutputTokens: 4},
		},
	}, state)
	require.Len(t, events, 2)
	assert.Equal(t, "message_delta", events[0].Type)
	assert.Equal(t, "max_tokens", events[0].Delta.StopReason)
	assert.Equal(t, "message_stop", events[1].Type)
	assert.Nil(t, FinalizeResponsesAnthropicStream(state))
}

func TestStreamingCachedTokensUseAnthropicInputSemantics(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:     "response.created",
		Response: &ResponsesResponse{ID: "resp_cached_stream", Model: "gpt-5.2"},
	}, state)

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.completed",
		Response: &ResponsesResponse{
			Status: "completed",
			Usage: &ResponsesUsage{
				InputTokens:  54006,
				OutputTokens: 123,
				TotalTokens:  54129,
				InputTokensDetails: &ResponsesInputTokensDetails{
					CachedTokens: 50688,
				},
			},
		},
	}, state)

	require.Len(t, events, 2)
	assert.Equal(t, "message_delta", events[0].Type)
	// Fork ledger v1: newPortion 3318 >= 1024 → cache_creation, not input.
	assert.Equal(t, 0, events[0].Usage.InputTokens)
	assert.Equal(t, 3318, events[0].Usage.CacheCreationInputTokens)
	assert.Equal(t, 50688, events[0].Usage.CacheReadInputTokens)
	assert.Equal(t, 123, events[0].Usage.OutputTokens)
	assert.Equal(t, "message_stop", events[1].Type)
}

func TestStreamingToolCall(t *testing.T) {
	state := NewResponsesEventToAnthropicState()

	// 1. response.created
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:     "response.created",
		Response: &ResponsesResponse{ID: "resp_2", Model: "gpt-5.2"},
	}, state)

	// 2. function_call added
	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.added",
		OutputIndex: 0,
		Item:        &ResponsesOutput{Type: "function_call", CallID: "call_1", Name: "get_weather"},
	}, state)
	require.Len(t, events, 1)
	assert.Equal(t, "content_block_start", events[0].Type)
	assert.Equal(t, "tool_use", events[0].ContentBlock.Type)
	assert.Equal(t, "toolu_1", events[0].ContentBlock.ID)

	// 3. arguments delta
	events = ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.function_call_arguments.delta",
		OutputIndex: 0,
		Delta:       `{"city":`,
	}, state)
	require.Len(t, events, 1)
	assert.Equal(t, "content_block_delta", events[0].Type)
	assert.Equal(t, "input_json_delta", events[0].Delta.Type)
	assert.Equal(t, `{"city":`, events[0].Delta.PartialJSON)

	// 4. arguments done
	events = ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.function_call_arguments.done",
	}, state)
	require.Len(t, events, 1)
	assert.Equal(t, "content_block_stop", events[0].Type)

	// 5. completed with tool_calls
	events = ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.completed",
		Response: &ResponsesResponse{
			Status: "completed",
			Usage:  &ResponsesUsage{InputTokens: 20, OutputTokens: 10},
		},
	}, state)
	require.Len(t, events, 2)
	assert.Equal(t, "tool_use", events[0].Delta.StopReason)
}

func TestStreamingReadToolDropsEmptyPages(t *testing.T) {
	state := NewResponsesEventToAnthropicState()

	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:     "response.created",
		Response: &ResponsesResponse{ID: "resp_read_stream", Model: "gpt-5.5"},
	}, state)

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.added",
		OutputIndex: 0,
		Item:        &ResponsesOutput{Type: "function_call", CallID: "call_read", Name: "Read"},
	}, state)
	require.Len(t, events, 1)
	assert.Equal(t, "content_block_start", events[0].Type)

	events = ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.function_call_arguments.delta",
		OutputIndex: 0,
		Delta:       `{"file_path":"/tmp/demo.py","limit":2000,"offset":0,"pages":""}`,
	}, state)
	assert.Len(t, events, 0)

	events = ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.function_call_arguments.done",
		OutputIndex: 0,
		Arguments:   `{"file_path":"/tmp/demo.py","limit":2000,"offset":0,"pages":""}`,
	}, state)
	require.Len(t, events, 2)
	assert.Equal(t, "content_block_delta", events[0].Type)
	assert.Equal(t, "input_json_delta", events[0].Delta.Type)
	assert.JSONEq(t, `{"file_path":"/tmp/demo.py","limit":2000,"offset":0}`, events[0].Delta.PartialJSON)
	assert.Equal(t, "content_block_stop", events[1].Type)
}

func TestStreamingReasoning(t *testing.T) {
	state := NewResponsesEventToAnthropicState()

	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:     "response.created",
		Response: &ResponsesResponse{ID: "resp_3", Model: "gpt-5.2"},
	}, state)

	// reasoning item added
	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.added",
		OutputIndex: 0,
		Item:        &ResponsesOutput{Type: "reasoning"},
	}, state)
	require.Len(t, events, 1)
	assert.Equal(t, "content_block_start", events[0].Type)
	assert.Equal(t, "thinking", events[0].ContentBlock.Type)

	// reasoning text delta
	events = ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.reasoning_summary_text.delta",
		OutputIndex: 0,
		Delta:       "Let me think...",
	}, state)
	require.Len(t, events, 1)
	assert.Equal(t, "content_block_delta", events[0].Type)
	assert.Equal(t, "thinking_delta", events[0].Delta.Type)
	assert.Equal(t, "Let me think...", events[0].Delta.Thinking)

	// reasoning done
	events = ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.reasoning_summary_text.done",
	}, state)
	require.Len(t, events, 1)
	assert.Equal(t, "content_block_stop", events[0].Type)
}

func TestStreamingIncomplete(t *testing.T) {
	state := NewResponsesEventToAnthropicState()

	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:     "response.created",
		Response: &ResponsesResponse{ID: "resp_4", Model: "gpt-5.2"},
	}, state)

	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:  "response.output_text.delta",
		Delta: "Partial output...",
	}, state)

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.incomplete",
		Response: &ResponsesResponse{
			Status:            "incomplete",
			IncompleteDetails: &ResponsesIncompleteDetails{Reason: "max_output_tokens"},
			Usage:             &ResponsesUsage{InputTokens: 100, OutputTokens: 4096},
		},
	}, state)

	// Should close the text block + message_delta + message_stop
	require.Len(t, events, 3)
	assert.Equal(t, "content_block_stop", events[0].Type)
	assert.Equal(t, "message_delta", events[1].Type)
	assert.Equal(t, "max_tokens", events[1].Delta.StopReason)
	assert.Equal(t, "message_stop", events[2].Type)
}

func TestFinalizeStream_NeverStarted(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	events := FinalizeResponsesAnthropicStream(state)
	assert.Nil(t, events)
}

func TestFinalizeStream_AlreadyCompleted(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true
	state.MessageStopSent = true
	events := FinalizeResponsesAnthropicStream(state)
	assert.Nil(t, events)
}

// 2026-05-03 codex casjbcfasju 1322455-token incident: when the
// upstream finishes WITHOUT emitting any content tokens, finalize
// must NOT close with normal message_delta(end_turn) + message_stop —
// that lets NewAPI's local_count_tokens=true bill the (huge) inbound
// prompt as successful consumption. Emit Anthropic SSE error event
// instead.
func TestFinalizeStream_NoOutputEmitsErrorEvent(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	// Stream started but produced zero content blocks and zero tokens
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:     "response.created",
		Response: &ResponsesResponse{ID: "resp_empty", Model: "gpt-5.2"},
	}, state)

	events := FinalizeResponsesAnthropicStream(state)
	if len(events) != 1 {
		t.Fatalf("expected 1 event (error), got %d: %+v", len(events), events)
	}
	if events[0].Type != "error" {
		t.Fatalf("expected error event, got %s", events[0].Type)
	}
	if events[0].Error == nil {
		t.Fatalf("error event must carry Error body")
	}
	if events[0].Error.Type != "api_error" {
		t.Errorf("expected api_error, got %s", events[0].Error.Type)
	}
	if events[0].Error.Message == "" {
		t.Errorf("error message must not be empty")
	}
}

// Counter-test: stream WITH content tokens still finalizes normally
// (message_delta(end_turn) + message_stop). Only the empty case
// switches to error.
func TestFinalizeStream_WithContentStillsCompletesNormally(t *testing.T) {
	state := NewResponsesEventToAnthropicState()

	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:     "response.created",
		Response: &ResponsesResponse{ID: "resp_n", Model: "gpt-5.2"},
	}, state)
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:  "response.output_text.delta",
		Delta: "hello",
	}, state)

	events := FinalizeResponsesAnthropicStream(state)
	if len(events) < 3 {
		t.Fatalf("non-empty stream should still emit content_block_stop + message_delta + message_stop, got %d events", len(events))
	}
	for _, e := range events {
		if e.Type == "error" {
			t.Fatalf("non-empty stream must NOT emit error event")
		}
	}
}

func TestFinalizeStream_AbnormalTermination(t *testing.T) {
	state := NewResponsesEventToAnthropicState()

	// Simulate a stream that started but never completed
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:     "response.created",
		Response: &ResponsesResponse{ID: "resp_5", Model: "gpt-5.2"},
	}, state)

	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:  "response.output_text.delta",
		Delta: "Interrupted...",
	}, state)

	// Stream ends without response.completed
	events := FinalizeResponsesAnthropicStream(state)
	require.Len(t, events, 3) // content_block_stop + message_delta + message_stop
	assert.Equal(t, "content_block_stop", events[0].Type)
	assert.Equal(t, "message_delta", events[1].Type)
	assert.Equal(t, "end_turn", events[1].Delta.StopReason)
	assert.Equal(t, "message_stop", events[2].Type)
}

func TestStreamingEmptyResponse(t *testing.T) {
	state := NewResponsesEventToAnthropicState()

	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:     "response.created",
		Response: &ResponsesResponse{ID: "resp_6", Model: "gpt-5.2"},
	}, state)

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.completed",
		Response: &ResponsesResponse{
			Status: "completed",
			Usage:  &ResponsesUsage{InputTokens: 5, OutputTokens: 0},
		},
	}, state)

	require.Len(t, events, 2) // message_delta + message_stop
	assert.Equal(t, "message_delta", events[0].Type)
	assert.Equal(t, "end_turn", events[0].Delta.StopReason)
}

func TestResponsesAnthropicEventToSSE(t *testing.T) {
	evt := AnthropicStreamEvent{
		Type: "message_start",
		Message: &AnthropicResponse{
			ID:   "resp_1",
			Type: "message",
			Role: "assistant",
		},
	}
	sse, err := ResponsesAnthropicEventToSSE(evt)
	require.NoError(t, err)
	assert.Contains(t, sse, "event: message_start\n")
	assert.Contains(t, sse, "data: ")
	assert.Contains(t, sse, `"resp_1"`)
}

// ---------------------------------------------------------------------------
// AnthropicContentBlock JSON serialization tests
// ---------------------------------------------------------------------------

func TestAnthropicContentBlock_TextAlwaysPresent(t *testing.T) {
	block := AnthropicContentBlock{Type: "text", Text: ""}
	data, err := json.Marshal(block)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"text":""`,
		"text field must be present even when empty")
}

func TestAnthropicContentBlock_ThinkingAlwaysPresent(t *testing.T) {
	block := AnthropicContentBlock{Type: "thinking", Thinking: ""}
	data, err := json.Marshal(block)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"thinking":""`,
		"thinking field must be present even when empty")
}

func TestAnthropicContentBlock_ToolUseOmitsTextAndThinking(t *testing.T) {
	block := AnthropicContentBlock{
		Type:  "tool_use",
		ID:    "tool_1",
		Name:  "get_weather",
		Input: json.RawMessage(`{}`),
	}
	data, err := json.Marshal(block)
	require.NoError(t, err)
	s := string(data)
	assert.NotContains(t, s, `"text"`, "tool_use block must not include text field")
	assert.NotContains(t, s, `"thinking"`, "tool_use block must not include thinking field")
	assert.Contains(t, s, `"id":"tool_1"`)
	assert.Contains(t, s, `"name":"get_weather"`)
}

func TestStreamingContentBlockStartTextJSON(t *testing.T) {
	state := NewResponsesEventToAnthropicState()

	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:     "response.created",
		Response: &ResponsesResponse{ID: "resp_x", Model: "gpt-5.4"},
	}, state)

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:  "response.output_text.delta",
		Delta: "ok",
	}, state)
	require.GreaterOrEqual(t, len(events), 1)
	blockStart := events[0]
	require.Equal(t, "content_block_start", blockStart.Type)

	sse, err := ResponsesAnthropicEventToSSE(blockStart)
	require.NoError(t, err)
	assert.Contains(t, sse, `"text":""`,
		`content_block_start SSE must include "text":"" for text-type blocks`)
}

// ---------------------------------------------------------------------------
// response.failed tests
// ---------------------------------------------------------------------------

func TestStreamingFailed(t *testing.T) {
	state := NewResponsesEventToAnthropicState()

	// 1. response.created
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:     "response.created",
		Response: &ResponsesResponse{ID: "resp_fail_1", Model: "gpt-5.2"},
	}, state)

	// 2. Some text output before failure
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:  "response.output_text.delta",
		Delta: "Partial output before failure",
	}, state)

	// 3. response.failed
	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.failed",
		Response: &ResponsesResponse{
			Status: "failed",
			Error:  &ResponsesError{Code: "server_error", Message: "Internal error"},
			Usage:  &ResponsesUsage{InputTokens: 50, OutputTokens: 10},
		},
	}, state)

	// Should close text block + message_delta + message_stop
	require.Len(t, events, 3)
	assert.Equal(t, "content_block_stop", events[0].Type)
	assert.Equal(t, "message_delta", events[1].Type)
	assert.Equal(t, "end_turn", events[1].Delta.StopReason)
	assert.Equal(t, 50, events[1].Usage.InputTokens)
	assert.Equal(t, 10, events[1].Usage.OutputTokens)
	assert.Equal(t, "message_stop", events[2].Type)
}

func TestStreamingFailedNoOutput(t *testing.T) {
	state := NewResponsesEventToAnthropicState()

	// 1. response.created
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:     "response.created",
		Response: &ResponsesResponse{ID: "resp_fail_2", Model: "gpt-5.2"},
	}, state)

	// 2. response.failed with no prior output
	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.failed",
		Response: &ResponsesResponse{
			Status: "failed",
			Error:  &ResponsesError{Code: "rate_limit_error", Message: "Too many requests"},
			Usage:  &ResponsesUsage{InputTokens: 20, OutputTokens: 0},
		},
	}, state)

	// Should emit message_delta + message_stop (no block to close)
	require.Len(t, events, 2)
	assert.Equal(t, "message_delta", events[0].Type)
	assert.Equal(t, "end_turn", events[0].Delta.StopReason)
	assert.Equal(t, "message_stop", events[1].Type)
}

func TestResponsesToAnthropic_Failed(t *testing.T) {
	resp := &ResponsesResponse{
		ID:     "resp_fail_3",
		Model:  "gpt-5.2",
		Status: "failed",
		Error:  &ResponsesError{Code: "server_error", Message: "Something went wrong"},
		Output: []ResponsesOutput{},
		Usage:  &ResponsesUsage{InputTokens: 30, OutputTokens: 0},
	}

	anth := ResponsesToAnthropic(resp, "claude-opus-4-6")
	// Failed status defaults to "end_turn" stop reason
	assert.Equal(t, "end_turn", anth.StopReason)
	// Should have at least an empty text block
	require.Len(t, anth.Content, 1)
	assert.Equal(t, "text", anth.Content[0].Type)
}

// ---------------------------------------------------------------------------
// thinking → reasoning conversion tests
// ---------------------------------------------------------------------------

func TestAnthropicToResponses_ThinkingEnabled(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		Thinking:  &AnthropicThinking{Type: "enabled", BudgetTokens: 10000},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Reasoning)
	// thinking.type is ignored for effort; default medium applies (058 step 2).
	assert.Equal(t, "medium", resp.Reasoning.Effort)
	assert.Equal(t, "auto", resp.Reasoning.Summary)
	assert.Contains(t, resp.Include, "reasoning.encrypted_content")
	assert.NotContains(t, resp.Include, "reasoning.summary")
}

func TestAnthropicToResponses_ThinkingAdaptive(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		Thinking:  &AnthropicThinking{Type: "adaptive", BudgetTokens: 5000},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Reasoning)
	// thinking.type is ignored for effort; default medium applies (058 step 2).
	assert.Equal(t, "medium", resp.Reasoning.Effort)
	assert.Equal(t, "auto", resp.Reasoning.Summary)
	assert.NotContains(t, resp.Include, "reasoning.summary")
}

func TestAnthropicToResponses_ThinkingDisabled(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		Thinking:  &AnthropicThinking{Type: "disabled"},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	// Default effort applies (medium) even when thinking is disabled (058 step 2).
	require.NotNil(t, resp.Reasoning)
	assert.Equal(t, "medium", resp.Reasoning.Effort)
}

func TestAnthropicToResponses_NoThinking(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	// Default effort applies (medium) when no thinking/output_config is set (058 step 2).
	require.NotNil(t, resp.Reasoning)
	assert.Equal(t, "medium", resp.Reasoning.Effort)
}

// ---------------------------------------------------------------------------
// output_config.effort override tests
// ---------------------------------------------------------------------------

func TestAnthropicToResponses_OutputConfigOverridesDefault(t *testing.T) {
	// Default is high, but output_config.effort="low" overrides. low→low after mapping.
	req := &AnthropicRequest{
		Model:        "gpt-5.2",
		MaxTokens:    1024,
		Messages:     []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		Thinking:     &AnthropicThinking{Type: "enabled", BudgetTokens: 10000},
		OutputConfig: &AnthropicOutputConfig{Effort: "low"},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Reasoning)
	assert.Equal(t, "low", resp.Reasoning.Effort)
	assert.Equal(t, "auto", resp.Reasoning.Summary)
}

func TestAnthropicToResponses_OutputConfigWithoutThinking(t *testing.T) {
	// No thinking field, but output_config.effort="medium" → creates reasoning.
	// medium→medium after 1:1 mapping.
	req := &AnthropicRequest{
		Model:        "gpt-5.2",
		MaxTokens:    1024,
		Messages:     []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		OutputConfig: &AnthropicOutputConfig{Effort: "medium"},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Reasoning)
	assert.Equal(t, "medium", resp.Reasoning.Effort)
	assert.Equal(t, "auto", resp.Reasoning.Summary)
}

func TestAnthropicToResponses_OutputConfigHigh(t *testing.T) {
	// output_config.effort="high" → mapped to "high" (1:1, both sides' default).
	req := &AnthropicRequest{
		Model:        "gpt-5.2",
		MaxTokens:    1024,
		Messages:     []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		OutputConfig: &AnthropicOutputConfig{Effort: "high"},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Reasoning)
	assert.Equal(t, "high", resp.Reasoning.Effort)
	assert.Equal(t, "auto", resp.Reasoning.Summary)
}

func TestAnthropicToResponses_OutputConfigMax(t *testing.T) {
	// output_config.effort="max" → mapped to OpenAI's highest supported level "xhigh".
	req := &AnthropicRequest{
		Model:        "gpt-5.2",
		MaxTokens:    1024,
		Messages:     []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		OutputConfig: &AnthropicOutputConfig{Effort: "max"},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Reasoning)
	assert.Equal(t, "xhigh", resp.Reasoning.Effort)
	assert.Equal(t, "auto", resp.Reasoning.Summary)
}

func TestAnthropicToResponses_NoOutputConfig(t *testing.T) {
	// No output_config → default medium regardless of thinking.type (058 step 2).
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		Thinking:  &AnthropicThinking{Type: "enabled", BudgetTokens: 10000},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Reasoning)
	assert.Equal(t, "medium", resp.Reasoning.Effort)
}

func TestAnthropicToResponses_OutputConfigWithoutEffort(t *testing.T) {
	// output_config present but effort empty (e.g. only format set) → default medium (058 step 2).
	req := &AnthropicRequest{
		Model:        "gpt-5.2",
		MaxTokens:    1024,
		Messages:     []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		OutputConfig: &AnthropicOutputConfig{},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Reasoning)
	assert.Equal(t, "medium", resp.Reasoning.Effort)
}

// ---------------------------------------------------------------------------
// SUB2API_GATE_REASONING_SUMMARY tests
// ---------------------------------------------------------------------------

func TestAnthropicToResponses_SummaryGateOff_NoThinking(t *testing.T) {
	// Flag unset → historical behaviour: Summary="auto" even without thinking.
	t.Setenv("SUB2API_GATE_REASONING_SUMMARY", "")
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Reasoning)
	assert.Equal(t, "auto", resp.Reasoning.Summary)
}

func TestAnthropicToResponses_SummaryGateOn_NoThinking(t *testing.T) {
	// Flag on + no thinking field → Summary="" so Codex skips summary text.
	t.Setenv("SUB2API_GATE_REASONING_SUMMARY", "1")
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Reasoning)
	assert.Equal(t, "", resp.Reasoning.Summary)
}

func TestAnthropicToResponses_SummaryGateOn_DisabledThinking(t *testing.T) {
	// Flag on + thinking.type="disabled" → Summary="".
	t.Setenv("SUB2API_GATE_REASONING_SUMMARY", "1")
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
		Thinking:  &AnthropicThinking{Type: "disabled"},
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Reasoning)
	assert.Equal(t, "", resp.Reasoning.Summary)
}

func TestAnthropicToResponses_SummaryGateOn_EnabledThinking(t *testing.T) {
	// Flag on + thinking.type="enabled" → Summary stays "auto".
	t.Setenv("SUB2API_GATE_REASONING_SUMMARY", "1")
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
		Thinking:  &AnthropicThinking{Type: "enabled", BudgetTokens: 8192},
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Reasoning)
	assert.Equal(t, "auto", resp.Reasoning.Summary)
}

func TestAnthropicToResponses_SummaryGateOn_AdaptiveThinking(t *testing.T) {
	// Flag on + thinking.type="adaptive" (opus-4.7 default) → Summary stays "auto".
	t.Setenv("SUB2API_GATE_REASONING_SUMMARY", "1")
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
		Thinking:  &AnthropicThinking{Type: "adaptive"},
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Reasoning)
	assert.Equal(t, "auto", resp.Reasoning.Summary)
}

// ---------------------------------------------------------------------------
// 2026-05-07 prod 502 第二波 — codex 分析: tool_choice 修了之后剩的 400
// upstream errors 是 missing call_id / content[0].text. 根因:
//   - tool_result block 缺 tool_use_id → function_call_output.CallID="" →
//     omitempty 字段消失 → OpenAI 上游 400 "Missing required parameter:
//     'input[N].call_id'"
//   - user plain string content="" → input_text.Text="" → omitempty 字段
//     消失 → OpenAI 上游 400 "Missing required parameter:
//     'input[N].content[0].text'"
// 修法对齐已有的 assistant 空字符串保护 (line 384-388), 把 user 路径跟
// tool_result 缺 ID 的路径也补上.
// ---------------------------------------------------------------------------

// user plain string 空 → 不应该生成 input_text 空 text part
func TestAnthropicToResponses_UserPlainStringEmpty(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`""`)},
		},
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	for _, item := range items {
		if item.Type != "message" {
			continue
		}
		var parts []ResponsesContentPart
		require.NoError(t, json.Unmarshal(item.Content, &parts))
		for i, p := range parts {
			if p.Type == "input_text" {
				assert.NotEmpty(t, p.Text,
					"input_text content[%d] has empty Text → 序列化时 omitempty 丢字段 → OpenAI 400",
					i)
			}
		}
	}
}

// tool_result 缺 tool_use_id → 不能生成空 CallID function_call_output
func TestAnthropicToResponses_ToolResultMissingToolUseID(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{
				Role: "user",
				Content: json.RawMessage(`[
					{"type":"tool_result","tool_use_id":"","content":"the result"}
				]`),
			},
		},
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	for i, item := range items {
		if item.Type == "function_call_output" {
			assert.NotEmpty(t, item.CallID,
				"input[%d].call_id 为空 → omitempty 丢字段 → OpenAI 400 'input[N].call_id missing'", i)
		}
	}
}

// 序列化层防御: 任何 function_call_output 都必须有 CallID
func TestAnthropicToResponses_NoEmptyCallIDInOutput(t *testing.T) {
	// 正常路径 (有 tool_use_id) 不应该被打扰
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{
				Role: "user",
				Content: json.RawMessage(`[
					{"type":"tool_result","tool_use_id":"toolu_abc","content":"result"}
				]`),
			},
		},
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	found := false
	for _, item := range items {
		if item.Type == "function_call_output" {
			found = true
			assert.NotEmpty(t, item.CallID)
		}
	}
	assert.True(t, found, "正常 tool_use_id 应该生成 function_call_output")
}

// ---------------------------------------------------------------------------
// tool_choice conversion tests
// ---------------------------------------------------------------------------

// 2026-05-07 prod 502 风暴根因: 客户端 (Claude Code SDK / NewAPI) 发字符串
// 形式的 tool_choice ("auto" / "none" / 等), 之前只接受 Anthropic 标准对象
// 形式, json.Unmarshal 把 string 当 object 解 → "cannot unmarshal string into
// Go value of type struct" → 502. sub2api 5/7 build #19 时 142/146 forward_failed
// 都是这条 error.
//
// 修复: convertAnthropicToolChoiceToResponses 兼容 string 输入, 跟
// remapChatToolChoiceToResponses 行为对齐.

func TestAnthropicToResponses_ToolChoiceStringAuto(t *testing.T) {
	req := &AnthropicRequest{
		Model:      "gpt-5.2",
		MaxTokens:  1024,
		Messages:   []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		ToolChoice: json.RawMessage(`"auto"`),
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	var tc string
	require.NoError(t, json.Unmarshal(resp.ToolChoice, &tc))
	assert.Equal(t, "auto", tc)
}

func TestAnthropicToResponses_ToolChoiceStringNone(t *testing.T) {
	req := &AnthropicRequest{
		Model:      "gpt-5.2",
		MaxTokens:  1024,
		Messages:   []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		ToolChoice: json.RawMessage(`"none"`),
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	var tc string
	require.NoError(t, json.Unmarshal(resp.ToolChoice, &tc))
	assert.Equal(t, "none", tc)
}

// "any" / "required" 都 normalize 成 Responses API 的 "required"
func TestAnthropicToResponses_ToolChoiceStringAny(t *testing.T) {
	req := &AnthropicRequest{
		Model:      "gpt-5.2",
		MaxTokens:  1024,
		Messages:   []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		ToolChoice: json.RawMessage(`"any"`),
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	var tc string
	require.NoError(t, json.Unmarshal(resp.ToolChoice, &tc))
	assert.Equal(t, "required", tc)
}

func TestAnthropicToResponses_ToolChoiceStringRequired(t *testing.T) {
	req := &AnthropicRequest{
		Model:      "gpt-5.2",
		MaxTokens:  1024,
		Messages:   []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		ToolChoice: json.RawMessage(`"required"`),
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	var tc string
	require.NoError(t, json.Unmarshal(resp.ToolChoice, &tc))
	assert.Equal(t, "required", tc)
}

// 未知字符串 pass through, 不报错 (前向兼容未来 OpenAI 加新值)
func TestAnthropicToResponses_ToolChoiceStringUnknownPassThrough(t *testing.T) {
	req := &AnthropicRequest{
		Model:      "gpt-5.2",
		MaxTokens:  1024,
		Messages:   []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		ToolChoice: json.RawMessage(`"future_value"`),
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	assert.Equal(t, `"future_value"`, string(resp.ToolChoice))
}

func TestAnthropicToResponses_ToolChoiceAuto(t *testing.T) {
	req := &AnthropicRequest{
		Model:      "gpt-5.2",
		MaxTokens:  1024,
		Messages:   []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		ToolChoice: json.RawMessage(`{"type":"auto"}`),
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var tc string
	require.NoError(t, json.Unmarshal(resp.ToolChoice, &tc))
	assert.Equal(t, "auto", tc)
}

func TestAnthropicToResponses_ToolChoiceAny(t *testing.T) {
	req := &AnthropicRequest{
		Model:      "gpt-5.2",
		MaxTokens:  1024,
		Messages:   []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		ToolChoice: json.RawMessage(`{"type":"any"}`),
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var tc string
	require.NoError(t, json.Unmarshal(resp.ToolChoice, &tc))
	assert.Equal(t, "required", tc)
}

func TestAnthropicToResponses_ToolChoiceSpecific(t *testing.T) {
	req := &AnthropicRequest{
		Model:      "gpt-5.2",
		MaxTokens:  1024,
		Messages:   []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		ToolChoice: json.RawMessage(`{"type":"tool","name":"get_weather"}`),
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	// Responses API uses flat {"type":"function","name":"X"}, not nested.
	var tc map[string]any
	require.NoError(t, json.Unmarshal(resp.ToolChoice, &tc))
	assert.Equal(t, "function", tc["type"])
	assert.Equal(t, "get_weather", tc["name"])
	assert.NotContains(t, tc, "function")
}

// 2026-05-07 backup 108 ch15 cctest 行为验证 0/30 主因: 客户端 (Claude
// Code SDK / 经 NewAPI 转发) 发 OpenAI Chat Completions nested 形式
// {"type":"function","function":{"name":"X"}}. 之前 type=function 走
// default 原样透传, OpenAI Responses 上游回 "Unknown parameter:
// 'tool_choice.function'" 400. 修复后拍平成 {"type":"function","name":"X"}.
func TestAnthropicToResponses_ToolChoiceOpenAINestedFunction(t *testing.T) {
	req := &AnthropicRequest{
		Model:      "gpt-5.2",
		MaxTokens:  1024,
		Messages:   []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"get_weather"}}`),
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	var tc map[string]any
	require.NoError(t, json.Unmarshal(resp.ToolChoice, &tc))
	assert.Equal(t, "function", tc["type"])
	assert.Equal(t, "get_weather", tc["name"])
	assert.NotContains(t, tc, "function", "拍平后不能再含 nested function 子对象")
}

// 已经是 Responses flat 格式 (没 nested function) 直接 pass through, 不被双重改写
func TestAnthropicToResponses_ToolChoiceResponsesFlatFunctionPassThrough(t *testing.T) {
	raw := `{"type":"function","name":"get_weather"}`
	req := &AnthropicRequest{
		Model:      "gpt-5.2",
		MaxTokens:  1024,
		Messages:   []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		ToolChoice: json.RawMessage(raw),
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	assert.Equal(t, raw, string(resp.ToolChoice))
}

func TestResponsesToAnthropicRequest_ToolChoiceFunctionName(t *testing.T) {
	req := &ResponsesRequest{
		Model:      "gpt-5.2",
		Input:      json.RawMessage(`[{"role":"user","content":"Hello"}]`),
		ToolChoice: json.RawMessage(`{"type":"function","name":"get_weather"}`),
	}

	resp, err := ResponsesToAnthropicRequest(req)
	require.NoError(t, err)

	var tc map[string]string
	require.NoError(t, json.Unmarshal(resp.ToolChoice, &tc))
	assert.Equal(t, "tool", tc["type"])
	assert.Equal(t, "get_weather", tc["name"])
}

func TestResponsesToAnthropicRequest_ToolChoiceLegacyFunctionName(t *testing.T) {
	req := &ResponsesRequest{
		Model:      "gpt-5.2",
		Input:      json.RawMessage(`[{"role":"user","content":"Hello"}]`),
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"get_weather"}}`),
	}

	resp, err := ResponsesToAnthropicRequest(req)
	require.NoError(t, err)

	var tc map[string]string
	require.NoError(t, json.Unmarshal(resp.ToolChoice, &tc))
	assert.Equal(t, "tool", tc["type"])
	assert.Equal(t, "get_weather", tc["name"])
}

// ---------------------------------------------------------------------------
// Image content block conversion tests
// ---------------------------------------------------------------------------

func TestAnthropicToResponses_UserImageBlock(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"text","text":"What is in this image?"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	require.Len(t, items, 1)
	assert.Equal(t, "user", items[0].Role)

	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 2)
	assert.Equal(t, "input_text", parts[0].Type)
	assert.Equal(t, "What is in this image?", parts[0].Text)
	assert.Equal(t, "input_image", parts[1].Type)
	assert.Equal(t, "data:image/png;base64,iVBOR", parts[1].ImageURL)
}

func TestAnthropicToResponses_ImageOnlyUserMessage(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"/9j/4AAQ"}}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	require.Len(t, items, 1)

	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 1)
	assert.Equal(t, "input_image", parts[0].Type)
	assert.Equal(t, "data:image/jpeg;base64,/9j/4AAQ", parts[0].ImageURL)
}

func TestAnthropicToResponses_ToolResultWithImage(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"Read the screenshot"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/tmp/screen.png"}}]`)},
			{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}
				]}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	// user + function_call + function_call_output + user(image) = 4
	require.Len(t, items, 4)

	// function_call_output should have text-only output (no image).
	assert.Equal(t, "function_call_output", items[2].Type)
	assert.Equal(t, "call_1", items[2].CallID)
	assert.Equal(t, "(empty)", items[2].Output)

	// Image should be in a separate user message.
	assert.Equal(t, "user", items[3].Role)
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[3].Content, &parts))
	require.Len(t, parts, 1)
	assert.Equal(t, "input_image", parts[0].Type)
	assert.Equal(t, "data:image/png;base64,iVBOR", parts[0].ImageURL)
}

func TestAnthropicToResponses_ToolResultMixed(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"Describe the file"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"toolu_2","name":"Read","input":{"file_path":"/tmp/photo.png"}}]`)},
			{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"toolu_2","content":[
					{"type":"text","text":"File metadata: 800x600 PNG"},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
				]}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	// user + function_call + function_call_output + user(image) = 4
	require.Len(t, items, 4)

	// function_call_output should have text-only output.
	assert.Equal(t, "function_call_output", items[2].Type)
	assert.Equal(t, "File metadata: 800x600 PNG", items[2].Output)

	// Image should be in a separate user message.
	assert.Equal(t, "user", items[3].Role)
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[3].Content, &parts))
	require.Len(t, parts, 1)
	assert.Equal(t, "input_image", parts[0].Type)
	assert.Equal(t, "data:image/png;base64,AAAA", parts[0].ImageURL)
}

func TestAnthropicToResponses_TextOnlyToolResultBackwardCompat(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"Check weather"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"NYC"}}]`)},
			{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"call_1","content":[
					{"type":"text","text":"Sunny, 72°F"}
				]}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	// user + function_call + function_call_output = 3
	require.Len(t, items, 3)

	// Text-only tool_result should produce a plain string.
	assert.Equal(t, "Sunny, 72°F", items[2].Output)
}

func TestAnthropicToResponses_ImageEmptyMediaType(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"image","source":{"type":"base64","media_type":"","data":"iVBOR"}}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	require.Len(t, items, 1)

	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 1)
	assert.Equal(t, "input_image", parts[0].Type)
	// Should default to image/png when media_type is empty.
	assert.Equal(t, "data:image/png;base64,iVBOR", parts[0].ImageURL)
}

// 2026-05-07 codex 多模态 5/10 修复: 客户端标错 MIME (PNG bytes 标 jpeg)
// 时, base64 magic header 嗅探覆盖 declared media_type → OpenAI OCR 不再
// 静默变差.
func TestAnthropicToResponses_ImageSniffsPNGWhenMediaTypeWrong(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"iVBORw0KGgo="}}
			]`)},
		},
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	require.Len(t, items, 1)
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 1)
	assert.Equal(t, "input_image", parts[0].Type)
	assert.Equal(t, "data:image/png;base64,iVBORw0KGgo=", parts[0].ImageURL,
		"PNG header → 应该嗅成 image/png, 覆盖 jpeg 标记")
}

// ---------------------------------------------------------------------------
// normalizeToolParameters tests
// ---------------------------------------------------------------------------

func TestNormalizeToolParameters(t *testing.T) {
	tests := []struct {
		name     string
		input    json.RawMessage
		expected string
	}{
		{
			name:     "nil input",
			input:    nil,
			expected: `{"type":"object","properties":{}}`,
		},
		{
			name:     "empty input",
			input:    json.RawMessage(``),
			expected: `{"type":"object","properties":{}}`,
		},
		{
			name:     "null input",
			input:    json.RawMessage(`null`),
			expected: `{"type":"object","properties":{}}`,
		},
		{
			name:     "object without properties",
			input:    json.RawMessage(`{"type":"object"}`),
			expected: `{"type":"object","properties":{}}`,
		},
		{
			name:     "object with properties",
			input:    json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
			expected: `{"type":"object","properties":{"city":{"type":"string"}}}`,
		},
		{
			name:     "non-object type",
			input:    json.RawMessage(`{"type":"string"}`),
			expected: `{"type":"string"}`,
		},
		{
			name:     "object with additional fields preserved",
			input:    json.RawMessage(`{"type":"object","required":["name"]}`),
			expected: `{"type":"object","required":["name"],"properties":{}}`,
		},
		{
			name:     "invalid JSON passthrough",
			input:    json.RawMessage(`not json`),
			expected: `not json`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeToolParameters(tt.input)
			if tt.name == "invalid JSON passthrough" {
				assert.Equal(t, tt.expected, string(result))
			} else {
				assert.JSONEq(t, tt.expected, string(result))
			}
		})
	}
}

func TestAnthropicToResponses_ToolWithoutProperties(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
		},
		Tools: []AnthropicTool{
			{Name: "mcp__pencil__get_style_guide_tags", Description: "Get style tags", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	require.Len(t, resp.Tools, 1)
	assert.Equal(t, "function", resp.Tools[0].Type)
	assert.Equal(t, "mcp__pencil__get_style_guide_tags", resp.Tools[0].Name)

	// Parameters must have "properties" field after normalization.
	var params map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(resp.Tools[0].Parameters, &params))
	assert.Contains(t, params, "properties")
}

func TestAnthropicToResponses_ToolWithNilSchema(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
		},
		Tools: []AnthropicTool{
			{Name: "simple_tool", Description: "A tool"},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	require.Len(t, resp.Tools, 1)
	var params map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(resp.Tools[0].Parameters, &params))
	assert.JSONEq(t, `"object"`, string(params["type"]))
	assert.JSONEq(t, `{}`, string(params["properties"]))
}

// ---------------------------------------------------------------------------
// Document content block tests (PDF and other file types)
// ---------------------------------------------------------------------------

func TestAnthropicToResponses_DocumentBase64PDF(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"text","text":"Summarize this PDF"},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0x"}}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	require.Len(t, items, 1)

	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 2)
	assert.Equal(t, "input_text", parts[0].Type)
	assert.Equal(t, "input_file", parts[1].Type)
	assert.Equal(t, "document.pdf", parts[1].Filename)
	assert.Equal(t, "data:application/pdf;base64,JVBERi0x", parts[1].FileData)
}

func TestAnthropicToResponses_DocumentBase64PDFWithTitle(t *testing.T) {
	// A title on the document block becomes the input_file filename.
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"document","title":"quarterly_report.pdf","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0x"}}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 1)
	assert.Equal(t, "input_file", parts[0].Type)
	assert.Equal(t, "quarterly_report.pdf", parts[0].Filename)
}

func TestAnthropicToResponses_DocumentBase64EmptyMediaTypeDefaultsToPDF(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"document","source":{"type":"base64","data":"JVBERi0x"}}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 1)
	assert.Equal(t, "data:application/pdf;base64,JVBERi0x", parts[0].FileData)
}

// 客户端标错 MIME (PDF bytes 标 application/octet-stream) 仍能正确识别
// + 自动补 .pdf 扩展名 (帮 OpenAI 类型推断更准)
func TestAnthropicToResponses_DocumentBase64PDFSniffsGenericMediaType(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"document","title":"cctest","source":{"type":"base64","media_type":"application/octet-stream","data":"JVBERi0xLjAK"}}
			]`)},
		},
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 1)
	assert.Equal(t, "input_file", parts[0].Type)
	assert.Equal(t, "cctest.pdf", parts[0].Filename, "title 缺扩展名时应自动补 .pdf")
	assert.Equal(t, "data:application/pdf;base64,JVBERi0xLjAK", parts[0].FileData,
		"PDF magic %PDF- → 应该嗅成 application/pdf, 覆盖 octet-stream")
}

func TestAnthropicToResponses_DocumentBase64DocxFilenameExtension(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"document","source":{"type":"base64","media_type":"application/vnd.openxmlformats-officedocument.wordprocessingml.document","data":"UEsDBB"}}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 1)
	assert.Equal(t, "document.docx", parts[0].Filename)
}

func TestAnthropicToResponses_DocumentTextSource(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"text","text":"Review:"},
				{"type":"document","title":"notes","context":"meeting 2026-04-13","source":{"type":"text","media_type":"text/plain","data":"Decisions made today: ship the fix."}}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 2)
	assert.Equal(t, "input_text", parts[1].Type)
	assert.Contains(t, parts[1].Text, "notes")
	assert.Contains(t, parts[1].Text, "meeting 2026-04-13")
	assert.Contains(t, parts[1].Text, "Decisions made today: ship the fix.")
}

func TestAnthropicToResponses_DocumentContentSourceRecursive(t *testing.T) {
	// type=content source expands nested text+image blocks into flat parts,
	// wrapped in [Document...] / [/Document] markers.
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"document","title":"multi_part","source":{"type":"content","content":[
					{"type":"text","text":"Section 1"},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}},
					{"type":"text","text":"Section 2"}
				]}}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	// header + Section 1 + image + Section 2 + closing = 5
	require.Len(t, parts, 5)
	assert.Equal(t, "input_text", parts[0].Type)
	assert.Contains(t, parts[0].Text, "multi_part")
	assert.Equal(t, "Section 1", parts[1].Text)
	assert.Equal(t, "input_image", parts[2].Type)
	assert.Equal(t, "data:image/png;base64,iVBOR", parts[2].ImageURL)
	assert.Equal(t, "Section 2", parts[3].Text)
	assert.Equal(t, "[/Document]", parts[4].Text)
}

func TestAnthropicToResponses_DocumentTextSourceNoTitle(t *testing.T) {
	// Regression test: a text-source document with no title/context should
	// still be wrapped in [Document] / [/Document] markers so the model can
	// tell it apart from surrounding user text. Previously the header was
	// omitted when both title and context were empty, causing the model to
	// see the document content as a continuation of the user prompt.
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"text","text":"Find the marker."},
				{"type":"document","source":{"type":"text","media_type":"text/plain","data":"marker=ZEBRA_7"}}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 2)
	assert.Equal(t, "Find the marker.", parts[0].Text)
	// Second part: header + data + closing marker, even with no title/context.
	assert.Contains(t, parts[1].Text, "[Document]")
	assert.Contains(t, parts[1].Text, "marker=ZEBRA_7")
	assert.Contains(t, parts[1].Text, "[/Document]")
}

func TestAnthropicToResponses_DocumentURLSource(t *testing.T) {
	// URL source → placeholder text (Responses API doesn't accept URL files).
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"document","title":"spec","source":{"type":"url","url":"https://example.com/spec.pdf"}}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 1)
	assert.Equal(t, "input_text", parts[0].Type)
	assert.Contains(t, parts[0].Text, "spec")
	assert.Contains(t, parts[0].Text, "https://example.com/spec.pdf")
}

func TestAnthropicToResponses_DocumentMissingSourceDropped(t *testing.T) {
	// Document block without a source is silently dropped (text block stays).
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[
				{"type":"text","text":"Hi"},
				{"type":"document"}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 1)
	assert.Equal(t, "Hi", parts[0].Text)
}

func TestAnthropicToResponses_DocumentMimeDowngrade(t *testing.T) {
	cases := []struct {
		name          string
		inputMime     string
		expectedMime  string
		expectedFname string
	}{
		{"application/xml → text/xml", "application/xml", "text/xml", "document.xml"},
		{"text/tab-separated-values → text/plain", "text/tab-separated-values", "text/plain", "document.txt"},
		{"application/rtf → text/plain", "application/rtf", "text/plain", "document.txt"},
		{"text/rtf → text/plain", "text/rtf", "text/plain", "document.txt"},
		{"application/pdf unchanged", "application/pdf", "application/pdf", "document.pdf"},
		{"text/plain unchanged", "text/plain", "text/plain", "document.txt"},
		{"docx unchanged", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "document.docx"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &AnthropicRequest{
				Model:     "gpt-5.2",
				MaxTokens: 1024,
				Messages: []AnthropicMessage{
					{Role: "user", Content: json.RawMessage(fmt.Sprintf(`[
						{"type":"document","source":{"type":"base64","media_type":%q,"data":"REFUQQ=="}}
					]`, tc.inputMime))},
				},
			}
			resp, err := AnthropicToResponses(req)
			require.NoError(t, err)
			var items []ResponsesInputItem
			require.NoError(t, json.Unmarshal(resp.Input, &items))
			var parts []ResponsesContentPart
			require.NoError(t, json.Unmarshal(items[0].Content, &parts))
			require.Len(t, parts, 1)
			assert.Equal(t, "input_file", parts[0].Type)
			assert.Equal(t, tc.expectedFname, parts[0].Filename)
			assert.Equal(t, "data:"+tc.expectedMime+";base64,REFUQQ==", parts[0].FileData)
		})
	}
}

func TestAnthropicToResponses_OrphanedToolResultSynthesizesPlaceholder(t *testing.T) {
	// Regression: a client's conversation history carries a tool_result
	// whose tool_use_id has no matching tool_use earlier in the history.
	// sub2api must synthesize a placeholder function_call immediately
	// before the function_call_output so upstream OpenAI doesn't reject
	// with "No tool call found for function call output with call_id X".

	t.Run("orphan_tool_result_at_start", func(t *testing.T) {
		req := &AnthropicRequest{
			Model:     "gpt-5.2",
			MaxTokens: 1024,
			Messages: []AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`[
					{"type":"tool_result","tool_use_id":"toolu_orphan_1","content":"555"}
				]`)},
				{Role: "assistant", Content: json.RawMessage(`"ok"`)},
				{Role: "user", Content: json.RawMessage(`"continue"`)},
			},
		}
		resp, err := AnthropicToResponses(req)
		require.NoError(t, err)

		var items []ResponsesInputItem
		require.NoError(t, json.Unmarshal(resp.Input, &items))

		// Find the placeholder function_call and the output.
		var gotPlaceholder, gotOutput bool
		for i, it := range items {
			if it.Type == "function_call" && it.CallID == "call_orphan_1" {
				if it.Name != "orphan_tool_call_placeholder" {
					t.Errorf("expected placeholder name, got %q", it.Name)
				}
				gotPlaceholder = true
				// Next item should be the matching output
				if i+1 < len(items) && items[i+1].Type == "function_call_output" && items[i+1].CallID == "call_orphan_1" {
					gotOutput = true
				}
			}
		}
		assert.True(t, gotPlaceholder, "no placeholder function_call synthesized")
		assert.True(t, gotOutput, "placeholder not immediately before function_call_output")
	})

	t.Run("orphan_tool_result_after_user_text", func(t *testing.T) {
		req := &AnthropicRequest{
			Model:     "gpt-5.2",
			MaxTokens: 1024,
			Messages: []AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`"hi"`)},
				{Role: "assistant", Content: json.RawMessage(`"hi there"`)},
				{Role: "user", Content: json.RawMessage(`[
					{"type":"tool_result","tool_use_id":"toolu_ghost","content":"ghost result"}
				]`)},
			},
		}
		resp, err := AnthropicToResponses(req)
		require.NoError(t, err)

		var items []ResponsesInputItem
		require.NoError(t, json.Unmarshal(resp.Input, &items))

		// Must contain both placeholder and its output, placeholder first.
		var fcIdx, outIdx = -1, -1
		for i, it := range items {
			if it.Type == "function_call" && it.CallID == "call_ghost" {
				fcIdx = i
			}
			if it.Type == "function_call_output" && it.CallID == "call_ghost" {
				outIdx = i
			}
		}
		assert.NotEqual(t, -1, fcIdx, "placeholder function_call missing")
		assert.NotEqual(t, -1, outIdx, "function_call_output missing")
		assert.Less(t, fcIdx, outIdx, "placeholder must come before output")
	})

	t.Run("matched_tool_call_no_synthesis", func(t *testing.T) {
		// Well-formed conversation: one tool_use, one tool_result. No
		// placeholder should be synthesized.
		req := &AnthropicRequest{
			Model:     "gpt-5.2",
			MaxTokens: 1024,
			Messages: []AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`"compute 2+2"`)},
				{Role: "assistant", Content: json.RawMessage(`[
					{"type":"tool_use","id":"toolu_real","name":"calc","input":{"expr":"2+2"}}
				]`)},
				{Role: "user", Content: json.RawMessage(`[
					{"type":"tool_result","tool_use_id":"toolu_real","content":"4"}
				]`)},
			},
		}
		resp, err := AnthropicToResponses(req)
		require.NoError(t, err)

		var items []ResponsesInputItem
		require.NoError(t, json.Unmarshal(resp.Input, &items))

		for _, it := range items {
			if it.Name == "orphan_tool_call_placeholder" {
				t.Errorf("should not synthesize placeholder for matched tool call")
			}
		}
	})

	t.Run("mixed_matched_and_orphan", func(t *testing.T) {
		// One matched call, one orphan. Should synthesize placeholder only
		// for the orphan.
		req := &AnthropicRequest{
			Model:     "gpt-5.2",
			MaxTokens: 1024,
			Messages: []AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`"start"`)},
				{Role: "assistant", Content: json.RawMessage(`[
					{"type":"tool_use","id":"toolu_good","name":"calc","input":{}}
				]`)},
				{Role: "user", Content: json.RawMessage(`[
					{"type":"tool_result","tool_use_id":"toolu_good","content":"ok1"},
					{"type":"tool_result","tool_use_id":"toolu_orphan","content":"ok2"}
				]`)},
			},
		}
		resp, err := AnthropicToResponses(req)
		require.NoError(t, err)

		var items []ResponsesInputItem
		require.NoError(t, json.Unmarshal(resp.Input, &items))

		placeholderCount := 0
		realCount := 0
		for _, it := range items {
			if it.Type == "function_call" {
				if it.Name == "orphan_tool_call_placeholder" {
					placeholderCount++
					assert.Equal(t, "call_orphan", it.CallID)
				} else {
					realCount++
					assert.Equal(t, "call_good", it.CallID)
				}
			}
		}
		assert.Equal(t, 1, placeholderCount, "expected exactly one placeholder")
		assert.Equal(t, 1, realCount, "expected exactly one real function_call")
	})

	t.Run("multiple_orphans", func(t *testing.T) {
		req := &AnthropicRequest{
			Model:     "gpt-5.2",
			MaxTokens: 1024,
			Messages: []AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`[
					{"type":"tool_result","tool_use_id":"toolu_ghost_1","content":"a"},
					{"type":"tool_result","tool_use_id":"toolu_ghost_2","content":"b"}
				]`)},
				{Role: "assistant", Content: json.RawMessage(`"go"`)},
			},
		}
		resp, err := AnthropicToResponses(req)
		require.NoError(t, err)

		var items []ResponsesInputItem
		require.NoError(t, json.Unmarshal(resp.Input, &items))

		placeholderCount := 0
		for _, it := range items {
			if it.Type == "function_call" && it.Name == "orphan_tool_call_placeholder" {
				placeholderCount++
			}
		}
		assert.Equal(t, 2, placeholderCount, "expected two placeholders")
	})
}

func TestAnthropicToResponses_AnthropicServerSideToolsDropped(t *testing.T) {
	// Anthropic server-side tools that have NO equivalent hosted tool in
	// OpenAI Responses API must be dropped at conversion time. This covers
	// computer_*, text_editor_*, and bash_*. web_search_* is handled
	// separately — see TestAnthropicToResponses_WebSearchTranslatedToPreview.
	cases := []struct {
		name string
		tool AnthropicTool
	}{
		{"computer_20241022", AnthropicTool{Type: "computer_20241022", Name: "computer"}},
		{"text_editor_20241022", AnthropicTool{Type: "text_editor_20241022", Name: "str_replace_editor"}},
		{"bash_20241022", AnthropicTool{Type: "bash_20241022", Name: "bash"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &AnthropicRequest{
				Model:     "gpt-5.2",
				MaxTokens: 1024,
				Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
				Tools: []AnthropicTool{
					{Name: "my_custom_tool", Description: "A real tool", InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)},
					tc.tool,
					{Name: "another_custom", Description: "Another real tool", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
				},
			}
			resp, err := AnthropicToResponses(req)
			require.NoError(t, err)
			require.Len(t, resp.Tools, 2)
			assert.Equal(t, "function", resp.Tools[0].Type)
			assert.Equal(t, "my_custom_tool", resp.Tools[0].Name)
			assert.Equal(t, "function", resp.Tools[1].Type)
			assert.Equal(t, "another_custom", resp.Tools[1].Name)
			for _, rt := range resp.Tools {
				assert.NotContains(t, rt.Type, "computer_")
				assert.NotContains(t, rt.Type, "text_editor_")
				assert.NotContains(t, rt.Type, "bash_")
			}
		})
	}
}

func TestAnthropicToResponses_WebSearchTranslatedToPreview(t *testing.T) {
	// Anthropic's web_search_* tool family translates to OpenAI Responses
	// API's hosted `web_search_preview` tool. Output items of type
	// web_search_call emitted by OpenAI get converted back to
	// server_tool_use + web_search_tool_result by responses_to_anthropic.go.
	cases := []struct {
		name string
		tool AnthropicTool
	}{
		{"web_search_20250305", AnthropicTool{Type: "web_search_20250305", Name: "web_search"}},
		{"web_search_20250101", AnthropicTool{Type: "web_search_20250101", Name: "web_search"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &AnthropicRequest{
				Model:     "gpt-5.2",
				MaxTokens: 1024,
				Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"search for news"`)}},
				Tools: []AnthropicTool{
					{Name: "my_custom_tool", Description: "A real tool", InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)},
					tc.tool,
				},
			}
			resp, err := AnthropicToResponses(req)
			require.NoError(t, err)
			require.Len(t, resp.Tools, 2)
			assert.Equal(t, "function", resp.Tools[0].Type)
			assert.Equal(t, "my_custom_tool", resp.Tools[0].Name)
			assert.Equal(t, "web_search", resp.Tools[1].Type, "Codex hosted search uses the bare name")
			assert.Empty(t, resp.Tools[1].Name, "hosted tools should not carry a function name")
			assert.Nil(t, resp.Tools[1].Parameters, "hosted tools should not carry parameters")
		})
	}
}

func TestAnthropicToResponses_EmptyAssistantStringIsSkipped(t *testing.T) {
	// Regression guard for "Missing required parameter: 'input[N].content[0].text'"
	// observed ~500+ times/hour on backup prod channel 15 before fix. When a
	// client sends {"role":"assistant","content":""} (empty string form —
	// possible during conversation replay / resume), the old path emitted
	// [{"type":"output_text","text":""}] which Go's json.Marshal serializes
	// as [{"type":"output_text"}] because Text has json:"text,omitempty".
	// Codex upstream then 400s on the missing text field. The fix skips the
	// message entirely when the content string is empty.
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "assistant", Content: json.RawMessage(`""`)},
			{Role: "user", Content: json.RawMessage(`"are you there?"`)},
		},
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	// Empty assistant message is dropped — expect only the 2 user messages.
	require.Len(t, items, 2, "empty assistant string should be skipped entirely")
	assert.Equal(t, "user", items[0].Role)
	assert.Equal(t, "user", items[1].Role)

	// Belt-and-suspenders: make sure the serialized request body does NOT
	// contain the exact malformed shape `{"type":"output_text"}` (missing
	// text field) anywhere.
	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `{"type":"output_text"}`,
		"must not emit output_text without a text field")
}

func TestAnthropicToResponses_OnlyDroppedServerSideToolsResultsInEmptyTools(t *testing.T) {
	// Edge case: request has ONLY a dropped server-side tool. After dropping,
	// the resulting tools list should be empty (not nil-crashed, and not
	// leaking the dropped tool back in).
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []AnthropicTool{
			{Type: "computer_20241022", Name: "computer"},
		},
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	assert.Empty(t, resp.Tools)
}

func TestAnthropicToResponses_ToolResultWithDocument(t *testing.T) {
	// A document inside tool_result is extracted into a follow-up user message,
	// mirroring how images inside tool_result are handled (the function_call_output
	// text stays "(empty)" because all content was non-text).
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"read the pdf"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"toolu_pdf","name":"ReadFile","input":{"path":"/tmp/report.pdf"}}]`)},
			{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"toolu_pdf","content":[
					{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0x"}}
				]}
			]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	// user + function_call + function_call_output + user(document) = 4
	require.Len(t, items, 4)

	assert.Equal(t, "function_call_output", items[2].Type)
	assert.Equal(t, "call_pdf", items[2].CallID)
	assert.Equal(t, "(empty)", items[2].Output)

	assert.Equal(t, "user", items[3].Role)
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[3].Content, &parts))
	require.Len(t, parts, 1)
	assert.Equal(t, "input_file", parts[0].Type)
	assert.Equal(t, "document.pdf", parts[0].Filename)
	assert.Equal(t, "data:application/pdf;base64,JVBERi0x", parts[0].FileData)
}

func TestSanitizeOpenAIName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "legal unchanged", in: "get_weather-1", want: "get_weather-1"},
		{name: "spaces replaced", in: "Read File", want: "Read_File"},
		{name: "unicode replaced", in: "天气", want: "__"},
		{name: "special chars replaced", in: "calc(*)", want: "calc___"},
		{name: "empty fallback", in: "", want: openAINameFallback},
		{name: "trim to max len", in: strings.Repeat("a", openAINameMaxLen+8), want: strings.Repeat("a", openAINameMaxLen)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeOpenAIName(tt.in)
			assert.Equal(t, tt.want, got)
			assert.True(t, isValidOpenAIName(got))
			assert.LessOrEqual(t, len(got), openAINameMaxLen)
		})
	}
}

func TestAnthropicToResponses_InvalidToolNamesSanitized(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-5.2",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"use the tool"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"call_1","name":"Read File!","input":{"path":"/tmp/a.txt"}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_1","content":"done"}]`)},
		},
		Tools: []AnthropicTool{
			{Name: "Read File!", Description: "Read a file", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
		},
		ToolChoice: json.RawMessage(`{"type":"tool","name":"Read File!"}`),
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	require.Len(t, resp.Tools, 1)
	assert.Equal(t, "Read_File_", resp.Tools[0].Name)
	assert.True(t, isValidOpenAIName(resp.Tools[0].Name))

	var toolChoice map[string]any
	require.NoError(t, json.Unmarshal(resp.ToolChoice, &toolChoice))
	assert.Equal(t, "function", toolChoice["type"])
	assert.Equal(t, "Read_File_", toolChoice["name"])

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	require.Len(t, items, 3)
	assert.Equal(t, "function_call", items[1].Type)
	assert.Equal(t, "Read_File_", items[1].Name)
	assert.True(t, isValidOpenAIName(items[1].Name))
	assert.Equal(t, "function_call_output", items[2].Type)
}
