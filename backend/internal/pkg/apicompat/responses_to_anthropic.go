package apicompat

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// generateAnthropicMessageID returns a synthetic id matching Anthropic's
// `msg_01<22-char-base64url>` shape (28 chars total). Used for both the
// non-streaming response and the stream `message_start` event so downstream
// clients never see the upstream OpenAI `resp_<hex>` id — that format leaks
// the impersonation and breaks clients that validate the `msg_01` prefix.
//
// Byte count 2026-04-21: bumped from 14 to 16 so the base64url body is 22
// chars (total 28 chars), matching real Anthropic ids like
// `msg_01XFDUDYJgAACzvnptvVoYEL`. The previous 14-byte version produced a
// 19-char body (25 chars total) that was 3 chars short of Anthropic's
// format — detectable by any client doing a simple length check. Must stay
// in sync with cc-api/main.go generateMessageID so A-track and B-track
// synthetic ids share the same shape.
func generateAnthropicMessageID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "msg_01" + base64.RawURLEncoding.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// Non-streaming: ResponsesResponse → AnthropicResponse
// ---------------------------------------------------------------------------

// ResponsesToAnthropic converts a Responses API response directly into an
// Anthropic Messages response. Reasoning output items are mapped to thinking
// blocks; function_call items become tool_use blocks.
func ResponsesToAnthropic(resp *ResponsesResponse, model string) *AnthropicResponse {
	out := &AnthropicResponse{
		ID:    generateAnthropicMessageID(),
		Type:  "message",
		Role:  "assistant",
		Model: model,
	}

	var blocks []AnthropicContentBlock

	for _, item := range resp.Output {
		switch item.Type {
		case "reasoning":
			summaryText := ""
			for _, s := range item.Summary {
				if s.Type == "summary_text" && s.Text != "" {
					summaryText += s.Text
				}
			}
			if summaryText != "" {
				blocks = append(blocks, AnthropicContentBlock{
					Type:     "thinking",
					Thinking: summaryText,
				})
			}
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" && part.Text != "" {
					blocks = append(blocks, AnthropicContentBlock{
						Type: "text",
						Text: part.Text,
					})
				}
			}
		case "function_call":
			blocks = append(blocks, AnthropicContentBlock{
				Type:  "tool_use",
				ID:    fromResponsesCallID(item.CallID),
				Name:  item.Name,
				Input: sanitizeAnthropicToolUseInput(item.Name, item.Arguments),
			})
		case "web_search_call":
			toolUseID := "srvtoolu_" + item.ID
			query := ""
			if item.Action != nil {
				query = item.Action.Query
			}
			inputJSON, _ := json.Marshal(map[string]string{"query": query})
			blocks = append(blocks, AnthropicContentBlock{
				Type:  "server_tool_use",
				ID:    toolUseID,
				Name:  "web_search",
				Input: inputJSON,
			})
			blocks = append(blocks, AnthropicContentBlock{
				Type:      "web_search_tool_result",
				ToolUseID: toolUseID,
				Content:   synthesizeWebSearchToolResultContent(query),
			})
		}
	}

	if len(blocks) == 0 {
		blocks = append(blocks, AnthropicContentBlock{Type: "text", Text: ""})
	}
	out.Content = blocks

	out.StopReason = responsesStatusToAnthropicStopReason(resp.Status, resp.IncompleteDetails, blocks)

	if resp.Usage != nil {
		cached := 0
		if resp.Usage.InputTokensDetails != nil {
			cached = resp.Usage.InputTokensDetails.CachedTokens
		}
		reasoning := 0
		if resp.Usage.OutputTokensDetails != nil {
			reasoning = resp.Usage.OutputTokensDetails.ReasoningTokens
		}
		input, creation, read := estimateAnthropicCacheUsage(resp.Usage.InputTokens, cached)
		out.Usage = AnthropicUsage{
			InputTokens:              input,
			OutputTokens:             visibleOutputTokens(resp.Usage.OutputTokens, reasoning),
			CacheCreationInputTokens: creation,
			CacheReadInputTokens:     read,
		}
	}

	return out
}

// visibleOutputTokens computes the visible-output portion of an OpenAI
// Responses API `output_tokens` counter. OpenAI reports the total including
// hidden reasoning (chain-of-thought) tokens; Anthropic non-thinking
// `output_tokens` should cover only text the client actually sees in the
// response content. Subtract reasoning_tokens and clamp to zero.
//
// For Anthropic-thinking clients, the reasoning content is separately
// surfaced as thinking_delta events in the stream, whose summary text is
// NOT counted by OpenAI's reasoning_tokens (OpenAI accounts the full hidden
// chain there). Counting only the visible text matches what the client can
// actually render and audit in the response.
func visibleOutputTokens(total, reasoning int) int {
	if reasoning <= 0 {
		return total
	}
	v := total - reasoning
	if v < 0 {
		return 0
	}
	return v
}

// openaiPrefixCacheMinTokens is the minimum input-token size at which OpenAI's
// Responses API starts writing entries into its prefix cache. Below this, no
// cache slot is allocated and the "new" portion of input never becomes a
// future cache_read hit. Attributing short-request input to cache_creation
// would over-count both the field and (via NewAPI's 1.25x multiplier) billing.
const openaiPrefixCacheMinTokens = 1024

// estimateAnthropicCacheUsage maps OpenAI Responses API usage to the three
// disjoint Anthropic counters (input_tokens, cache_creation, cache_read).
//
// OpenAI reports only:
//   - total = resp.Usage.InputTokens       (all input tokens for this request)
//   - cached = InputTokensDetails.CachedTokens  (prefix-cache READ hits)
//
// The "new" portion (total - cached) is what the request processed uncached.
// For long-enough requests (≥ openaiPrefixCacheMinTokens) OpenAI will
// prefix-cache that portion and make it available for future reads, which is
// semantically the same as Anthropic's cache_creation_input_tokens. For
// short requests OpenAI skips cache write, so the new portion stays as plain
// input_tokens.
//
// Invariants (matching Anthropic's semantics — all three counters disjoint):
//   - input + creation + read == total
//   - creation > 0 implies input == 0 (new portion fully attributed to write)
//   - creation == 0 for total < openaiPrefixCacheMinTokens
//   - read always equals cached (exact, not an estimate)
//   - cached > total (rare upstream accounting drift) clamps read to total,
//     input/creation to 0
func estimateAnthropicCacheUsage(total, cached int) (input, creation, read int) {
	if total <= 0 {
		return 0, 0, 0
	}
	if cached < 0 {
		cached = 0
	}
	if cached > total {
		// Upstream drift: cached reported greater than total. Trust the
		// smaller of the two (total) for read so the three counters still
		// sum consistently, and zero the rest.
		return 0, 0, total
	}
	newPortion := total - cached
	if newPortion >= openaiPrefixCacheMinTokens {
		return 0, newPortion, cached
	}
	return newPortion, 0, cached
}

func responsesStatusToAnthropicStopReason(status string, details *ResponsesIncompleteDetails, blocks []AnthropicContentBlock) string {
	switch status {
	case "incomplete":
		if details != nil && details.Reason == "max_output_tokens" {
			return "max_tokens"
		}
		return "end_turn"
	case "completed":
		// 058 step 2: tool_use anywhere in the block list — even followed by
		// trailing text — terminates with stop_reason=tool_use. Last-block
		// detection missed cases where Codex emitted text after the tool
		// call but Claude Code still expected the chain to continue.
		if containsAnthropicToolUseBlock(blocks) {
			return "tool_use"
		}
		return "end_turn"
	default:
		return "end_turn"
	}
}

func containsAnthropicToolUseBlock(blocks []AnthropicContentBlock) bool {
	for _, block := range blocks {
		if block.Type == "tool_use" {
			return true
		}
	}
	return false
}

// sanitizeAnthropicToolUseInput drops empty Read.pages from upstream tool
// input. Every fallback path goes through safeRawJSON so empty/invalid
// arguments cannot become an invalid json.RawMessage that downstream JSON
// encoders panic on (fork's safeRawJSON contract).
func sanitizeAnthropicToolUseInput(name string, raw string) json.RawMessage {
	if name != "Read" || raw == "" {
		return safeRawJSON(raw)
	}

	var input map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return safeRawJSON(raw)
	}

	if pages, ok := input["pages"]; !ok || string(pages) != `""` {
		return safeRawJSON(raw)
	}

	delete(input, "pages")
	sanitized, err := json.Marshal(input)
	if err != nil {
		return safeRawJSON(raw)
	}
	return sanitized
}

// ---------------------------------------------------------------------------
// Streaming: ResponsesStreamEvent → []AnthropicStreamEvent (stateful converter)
// ---------------------------------------------------------------------------

// ResponsesEventToAnthropicState tracks state for converting a sequence of
// Responses SSE events directly into Anthropic SSE events.
type ResponsesEventToAnthropicState struct {
	MessageStartSent bool
	MessageStopSent  bool

	ContentBlockIndex   int
	ContentBlockOpen    bool
	CurrentBlockType    string // "text" | "thinking" | "tool_use"
	CurrentToolName     string
	CurrentToolArgs     string
	CurrentToolHadDelta bool // 058 step 2: true once a function_call_arguments.delta has been forwarded for the current tool_use block.
	HasToolCall         bool // 058 step 2: true if any function_call output_item.added has been seen during this stream.

	// OutputIndexToBlockIdx maps Responses output_index → Anthropic content block index.
	OutputIndexToBlockIdx map[int]int

	// Raw OpenAI-side usage as observed on the wire. The Anthropic-side
	// usage fields (InputTokens / CacheCreation / CacheRead / OutputTokens)
	// are derived from these at emission time via estimateAnthropicCacheUsage
	// and visibleOutputTokens so the stream and non-stream code paths share
	// identical mapping rules. RawReasoningTokens is OpenAI's hidden chain-
	// of-thought counter which is subtracted from RawOutputTokens before the
	// value is surfaced to Anthropic clients.
	RawTotalInputTokens  int
	RawCachedInputTokens int
	RawOutputTokens      int
	RawReasoningTokens   int

	ResponseID string
	Model      string
	Created    int64
}

// NewResponsesEventToAnthropicState returns an initialised stream state.
func NewResponsesEventToAnthropicState() *ResponsesEventToAnthropicState {
	return &ResponsesEventToAnthropicState{
		OutputIndexToBlockIdx: make(map[int]int),
		Created:               time.Now().Unix(),
	}
}

// ResponsesEventToAnthropicEvents converts a single Responses SSE event into
// zero or more Anthropic SSE events, updating state as it goes.
func ResponsesEventToAnthropicEvents(
	evt *ResponsesStreamEvent,
	state *ResponsesEventToAnthropicState,
) []AnthropicStreamEvent {
	switch evt.Type {
	case "response.created":
		return resToAnthHandleCreated(evt, state)
	case "response.output_item.added":
		return resToAnthHandleOutputItemAdded(evt, state)
	case "response.output_text.delta":
		return resToAnthHandleTextDelta(evt, state)
	case "response.output_text.done":
		return resToAnthHandleBlockDone(state)
	case "response.function_call_arguments.delta":
		return resToAnthHandleFuncArgsDelta(evt, state)
	case "response.function_call_arguments.done":
		return resToAnthHandleFuncArgsDone(evt, state)
	case "response.output_item.done":
		return resToAnthHandleOutputItemDone(evt, state)
	case "response.reasoning_summary_text.delta":
		return resToAnthHandleReasoningDelta(evt, state)
	case "response.reasoning_summary_text.done":
		return resToAnthHandleBlockDone(state)
	// response.done 是 Realtime/WS 与项目透传路径使用的终止别名；
	// 普通 Responses HTTP SSE 的公开终止事件仍以 response.completed 为主。
	case "response.completed", "response.done", "response.incomplete", "response.failed":
		return resToAnthHandleCompleted(evt, state)
	default:
		return nil
	}
}

// FinalizeResponsesAnthropicStream emits synthetic termination events if the
// stream ended without a proper completion event.
//
// 2026-05-03 codex casjbcfasju 1322455-token incident: when the upstream
// produced ZERO output tokens AND no content blocks were emitted, we
// previously closed with message_delta(stop_reason="end_turn") +
// message_stop. NewAPI's local_count_tokens=true path then billed the
// inbound prompt as successful consumption (~3.3M quota / call,
// observed 7 times over 16 minutes for one user, ~33M total quota
// burned with zero output).
//
// Now: zero-output finalisation emits an Anthropic SSE `error` event
// instead. Clients that respect SSE error events do NOT treat this as
// a successful completion and skip the success-billing path.
func FinalizeResponsesAnthropicStream(state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	if !state.MessageStartSent || state.MessageStopSent {
		return nil
	}

	var events []AnthropicStreamEvent
	events = append(events, closeCurrentBlock(state)...)

	input, creation, read := estimateAnthropicCacheUsage(state.RawTotalInputTokens, state.RawCachedInputTokens)
	outputTokens := visibleOutputTokens(state.RawOutputTokens, state.RawReasoningTokens)

	// Detect "no real output" — both no visible output tokens AND no
	// content block ever opened. ContentBlockIndex starts at 0 and is
	// only bumped when a block opens, so an unbumped index combined
	// with zero output tokens means the upstream stream was empty.
	noOutput := outputTokens <= 0 && state.ContentBlockIndex == 0 && !state.ContentBlockOpen
	if noOutput {
		events = append(events, AnthropicStreamEvent{
			Type: "error",
			Error: &AnthropicErrorBody{
				Type:    "api_error",
				Message: "upstream returned no content tokens (empty stream)",
			},
		})
		state.MessageStopSent = true
		return events
	}

	// 058 step 2: stop_reason reflects whether a tool call was seen anywhere
	// in this stream, not the last-block heuristic. Codex sometimes emits text
	// after a tool call but Claude Code still relies on stop_reason=tool_use
	// to keep the chain going.
	stopReason := "end_turn"
	if state.HasToolCall {
		stopReason = "tool_use"
	}

	events = append(events,
		AnthropicStreamEvent{
			Type: "message_delta",
			Delta: &AnthropicDelta{
				StopReason: stopReason,
			},
			Usage: &AnthropicUsage{
				InputTokens:              input,
				OutputTokens:             outputTokens,
				CacheCreationInputTokens: creation,
				CacheReadInputTokens:     read,
			},
		},
		AnthropicStreamEvent{Type: "message_stop"},
	)
	state.MessageStopSent = true
	return events
}

// ResponsesAnthropicEventToSSE formats an AnthropicStreamEvent as an SSE line pair.
func ResponsesAnthropicEventToSSE(evt AnthropicStreamEvent) (string, error) {
	data, err := json.Marshal(evt)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("event: %s\ndata: %s\n\n", evt.Type, data), nil
}

// --- internal handlers ---

func resToAnthHandleCreated(evt *ResponsesStreamEvent, state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	if evt.Response != nil {
		// Only use upstream model if no override was set (e.g. originalModel)
		if state.Model == "" {
			state.Model = evt.Response.Model
		}
	}

	if state.MessageStartSent {
		return nil
	}
	state.MessageStartSent = true
	if state.ResponseID == "" {
		state.ResponseID = generateAnthropicMessageID()
	}

	return []AnthropicStreamEvent{{
		Type: "message_start",
		Message: &AnthropicResponse{
			ID:      state.ResponseID,
			Type:    "message",
			Role:    "assistant",
			Content: []AnthropicContentBlock{},
			Model:   state.Model,
			Usage: AnthropicUsage{
				InputTokens:  0,
				OutputTokens: 0,
			},
		},
	}}
}

func resToAnthHandleOutputItemAdded(evt *ResponsesStreamEvent, state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	if evt.Item == nil {
		return nil
	}

	switch evt.Item.Type {
	case "function_call":
		var events []AnthropicStreamEvent
		events = append(events, closeCurrentBlock(state)...)

		idx := state.ContentBlockIndex
		state.OutputIndexToBlockIdx[evt.OutputIndex] = idx
		state.ContentBlockOpen = true
		state.CurrentBlockType = "tool_use"
		state.CurrentToolName = evt.Item.Name
		state.CurrentToolArgs = ""
		state.CurrentToolHadDelta = false
		state.HasToolCall = true

		events = append(events, AnthropicStreamEvent{
			Type:  "content_block_start",
			Index: &idx,
			ContentBlock: &AnthropicContentBlock{
				Type:  "tool_use",
				ID:    fromResponsesCallID(evt.Item.CallID),
				Name:  evt.Item.Name,
				Input: json.RawMessage("{}"),
			},
		})
		return events

	case "reasoning":
		var events []AnthropicStreamEvent
		events = append(events, closeCurrentBlock(state)...)

		idx := state.ContentBlockIndex
		state.OutputIndexToBlockIdx[evt.OutputIndex] = idx
		state.ContentBlockOpen = true
		state.CurrentBlockType = "thinking"

		events = append(events, AnthropicStreamEvent{
			Type:  "content_block_start",
			Index: &idx,
			ContentBlock: &AnthropicContentBlock{
				Type:     "thinking",
				Thinking: "",
			},
		})
		return events

	case "message":
		return nil
	}

	return nil
}

func resToAnthHandleTextDelta(evt *ResponsesStreamEvent, state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	if evt.Delta == "" {
		return nil
	}

	var events []AnthropicStreamEvent

	if !state.ContentBlockOpen || state.CurrentBlockType != "text" {
		events = append(events, closeCurrentBlock(state)...)

		idx := state.ContentBlockIndex
		state.ContentBlockOpen = true
		state.CurrentBlockType = "text"

		events = append(events, AnthropicStreamEvent{
			Type:  "content_block_start",
			Index: &idx,
			ContentBlock: &AnthropicContentBlock{
				Type: "text",
				Text: "",
			},
		})
	}

	idx := state.ContentBlockIndex
	events = append(events, AnthropicStreamEvent{
		Type:  "content_block_delta",
		Index: &idx,
		Delta: &AnthropicDelta{
			Type: "text_delta",
			Text: evt.Delta,
		},
	})
	return events
}

func resToAnthHandleFuncArgsDelta(evt *ResponsesStreamEvent, state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	if evt.Delta == "" {
		return nil
	}

	if state.CurrentBlockType == "tool_use" && state.CurrentToolName == "Read" {
		state.CurrentToolArgs += evt.Delta
		return nil
	}
	// 058 step 2: mark that a delta has been forwarded so the matching .done
	// event does NOT re-emit the full Arguments JSON (would duplicate input).
	if state.CurrentBlockType == "tool_use" {
		state.CurrentToolHadDelta = true
	}

	blockIdx, ok := state.OutputIndexToBlockIdx[evt.OutputIndex]
	if !ok {
		return nil
	}

	return []AnthropicStreamEvent{{
		Type:  "content_block_delta",
		Index: &blockIdx,
		Delta: &AnthropicDelta{
			Type:        "input_json_delta",
			PartialJSON: evt.Delta,
		},
	}}
}

func resToAnthHandleFuncArgsDone(evt *ResponsesStreamEvent, state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	if state.CurrentBlockType != "tool_use" {
		return resToAnthHandleBlockDone(state)
	}

	raw := evt.Arguments
	if raw == "" {
		raw = state.CurrentToolArgs
	}

	// 058 step 2: when no delta has been forwarded (some Codex shapes only
	// emit `function_call_arguments.done` with the full Arguments string),
	// synthesise a single input_json_delta carrying the entire payload so
	// downstream Anthropic clients see the JSON. If a delta has already been
	// streamed, just close the block — re-emitting would duplicate input.
	if raw == "" || state.CurrentToolHadDelta {
		return closeCurrentBlock(state)
	}

	if state.CurrentToolName == "Read" {
		// Fork: drop empty Read.pages via safeRawJSON.
		sanitized := sanitizeAnthropicToolUseInput(state.CurrentToolName, raw)
		if len(sanitized) == 0 {
			return closeCurrentBlock(state)
		}
		raw = string(sanitized)
	}

	idx := state.ContentBlockIndex
	events := []AnthropicStreamEvent{{
		Type:  "content_block_delta",
		Index: &idx,
		Delta: &AnthropicDelta{
			Type:        "input_json_delta",
			PartialJSON: raw,
		},
	}}
	events = append(events, closeCurrentBlock(state)...)
	return events
}

func resToAnthHandleReasoningDelta(evt *ResponsesStreamEvent, state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	if evt.Delta == "" {
		return nil
	}

	blockIdx, ok := state.OutputIndexToBlockIdx[evt.OutputIndex]
	if !ok {
		return nil
	}

	return []AnthropicStreamEvent{{
		Type:  "content_block_delta",
		Index: &blockIdx,
		Delta: &AnthropicDelta{
			Type:     "thinking_delta",
			Thinking: evt.Delta,
		},
	}}
}

func resToAnthHandleBlockDone(state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	if !state.ContentBlockOpen {
		return nil
	}
	return closeCurrentBlock(state)
}

func resToAnthHandleOutputItemDone(evt *ResponsesStreamEvent, state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	if evt.Item == nil {
		return nil
	}

	// Handle web_search_call → synthesize server_tool_use + web_search_tool_result blocks.
	if evt.Item.Type == "web_search_call" && evt.Item.Status == "completed" {
		return resToAnthHandleWebSearchDone(evt, state)
	}

	if state.ContentBlockOpen {
		return closeCurrentBlock(state)
	}
	return nil
}

// resToAnthHandleWebSearchDone converts an OpenAI web_search_call output item
// into Anthropic server_tool_use + web_search_tool_result content block pairs.
// This allows Claude Code to count the searches performed.
func resToAnthHandleWebSearchDone(evt *ResponsesStreamEvent, state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	var events []AnthropicStreamEvent
	events = append(events, closeCurrentBlock(state)...)

	toolUseID := "srvtoolu_" + evt.Item.ID
	query := ""
	if evt.Item.Action != nil {
		query = evt.Item.Action.Query
	}
	inputJSON, _ := json.Marshal(map[string]string{"query": query})

	// Emit server_tool_use block (start + stop).
	idx1 := state.ContentBlockIndex
	events = append(events, AnthropicStreamEvent{
		Type:  "content_block_start",
		Index: &idx1,
		ContentBlock: &AnthropicContentBlock{
			Type:  "server_tool_use",
			ID:    toolUseID,
			Name:  "web_search",
			Input: inputJSON,
		},
	})
	events = append(events, AnthropicStreamEvent{
		Type:  "content_block_stop",
		Index: &idx1,
	})
	state.ContentBlockIndex++

	// Emit web_search_tool_result block (start + stop). Content carries a
	// single synthesized web_search_result placeholder — see
	// synthesizeWebSearchToolResultContent for rationale. The Codex backend
	// folds actual search hits into the model's text output and does NOT
	// expose a structured result array on the web_search_call output item,
	// so we can't surface real titles/URLs here. But emitting an empty
	// content array made Claude Code CLI display "Did 0 searches in Ns",
	// which confused the model into retrying the search and eventually
	// giving up with "I can't search" to the user. A single placeholder
	// item keeps the counter honest and the model's downstream reasoning
	// uncorrupted.
	idx2 := state.ContentBlockIndex
	events = append(events, AnthropicStreamEvent{
		Type:  "content_block_start",
		Index: &idx2,
		ContentBlock: &AnthropicContentBlock{
			Type:      "web_search_tool_result",
			ToolUseID: toolUseID,
			Content:   synthesizeWebSearchToolResultContent(query),
		},
	})
	events = append(events, AnthropicStreamEvent{
		Type:  "content_block_stop",
		Index: &idx2,
	})
	state.ContentBlockIndex++

	return events
}

func resToAnthHandleCompleted(evt *ResponsesStreamEvent, state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	if state.MessageStopSent {
		return nil
	}

	var events []AnthropicStreamEvent
	events = append(events, closeCurrentBlock(state)...)

	stopReason := "end_turn"
	if evt.Response != nil {
		if evt.Response.Usage != nil {
			state.RawTotalInputTokens = evt.Response.Usage.InputTokens
			state.RawOutputTokens = evt.Response.Usage.OutputTokens
			if evt.Response.Usage.InputTokensDetails != nil {
				state.RawCachedInputTokens = evt.Response.Usage.InputTokensDetails.CachedTokens
			}
			if evt.Response.Usage.OutputTokensDetails != nil {
				state.RawReasoningTokens = evt.Response.Usage.OutputTokensDetails.ReasoningTokens
			}
		}
		switch evt.Response.Status {
		case "incomplete":
			if evt.Response.IncompleteDetails != nil && evt.Response.IncompleteDetails.Reason == "max_output_tokens" {
				stopReason = "max_tokens"
			}
		case "completed":
			// 058 step 2: HasToolCall is set on output_item.added so this
			// holds even when text is emitted after the tool call.
			if state.HasToolCall {
				stopReason = "tool_use"
			}
		}
	}

	input, creation, read := estimateAnthropicCacheUsage(state.RawTotalInputTokens, state.RawCachedInputTokens)
	outputTokens := visibleOutputTokens(state.RawOutputTokens, state.RawReasoningTokens)

	events = append(events,
		AnthropicStreamEvent{
			Type: "message_delta",
			Delta: &AnthropicDelta{
				StopReason: stopReason,
			},
			Usage: &AnthropicUsage{
				InputTokens:              input,
				OutputTokens:             outputTokens,
				CacheCreationInputTokens: creation,
				CacheReadInputTokens:     read,
			},
		},
		AnthropicStreamEvent{Type: "message_stop"},
	)
	state.MessageStopSent = true
	return events
}

func closeCurrentBlock(state *ResponsesEventToAnthropicState) []AnthropicStreamEvent {
	if !state.ContentBlockOpen {
		return nil
	}
	idx := state.ContentBlockIndex
	state.ContentBlockOpen = false
	state.ContentBlockIndex++
	state.CurrentToolName = ""
	state.CurrentToolArgs = ""
	state.CurrentToolHadDelta = false
	return []AnthropicStreamEvent{{
		Type:  "content_block_stop",
		Index: &idx,
	}}
}

// synthesizeWebSearchToolResultContent returns a minimal non-empty content
// payload for an Anthropic `web_search_tool_result` block. The Codex upstream
// that sub2api reverses does not expose individual search hits on the
// `web_search_call` output item — actual search results are folded into the
// assistant's text output as markdown links rather than a structured result
// array. Emitting `content: []` caused Claude Code CLI to display
// "Did 0 searches in Ns" and prompted the model to retry searches in a
// loop, eventually giving up. A single placeholder item:
//
//  1. Keeps Claude Code CLI's search-count display honest (shows 1 search
//     instead of 0 for every tool call that actually ran)
//  2. Doesn't lie about content — the URL field is empty and the title
//     reflects the actual query that was executed
//  3. Allows the model's downstream text output (which DOES contain the
//     real search-informed content) to reach the user uncorrupted
//
// fakeEncryptedContent 生成 128 bytes random base64 (~172 chars) 占位
// encrypted_content 字段, 让 web_search_tool_result 看起来更像真 Anthropic
// 输出. 真 Anthropic 此字段是 opaque 加密 blob, 我们没真加密, 只为长度合理.
func fakeEncryptedContent() string {
	var b [128]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b[:])
}

// 2026-05-07 codex 伪装泄漏 #6: 之前 encrypted_content="" 是合成痕迹,
// 真 Anthropic 这字段是 opaque encrypted blob (几百 char base64). cctest
// 检测器盯 server tool 细节时 "" 更容易暴露. 改填 random base64 128 bytes
// (~172 chars) 看起来更像真加密. 不能 100% 隐身 (cctest 解 base64 验
// 合法 ciphertext 仍能识别), 但避免 "明显 0 长度" 直接暴露.
func synthesizeWebSearchToolResultContent(query string) json.RawMessage {
	title := "Search: " + query
	if query == "" {
		title = "Search completed"
	}
	items := []map[string]string{
		{
			"type":              "web_search_result",
			"title":             title,
			"url":               "",
			"encrypted_content": fakeEncryptedContent(),
		},
	}
	out, err := json.Marshal(items)
	if err != nil {
		return json.RawMessage(`[]`)
	}
	return out
}
