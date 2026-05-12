// Package apicompat provides type definitions and conversion utilities for
// translating between Anthropic Messages and OpenAI Responses API formats.
// It enables multi-protocol support so that clients using different API
// formats can be served through a unified gateway.
package apicompat

import "encoding/json"

// ---------------------------------------------------------------------------
// Anthropic Messages API types
// ---------------------------------------------------------------------------

// AnthropicRequest is the request body for POST /v1/messages.
type AnthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      json.RawMessage    `json:"system,omitempty"` // string or []AnthropicContentBlock
	Messages    []AnthropicMessage `json:"messages"`
	Tools       []AnthropicTool    `json:"tools,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	StopSeqs    []string           `json:"stop_sequences,omitempty"`
	Thinking    *AnthropicThinking `json:"thinking,omitempty"`
	ToolChoice  json.RawMessage    `json:"tool_choice,omitempty"`
	// Metadata 会被原样透传给上游。OAuth/Claude-Code 路径依赖 metadata.user_id
	// 参与上游的"是否为官方 Claude Code 请求"判定；如果经由本结构体重新序列化
	// 时丢弃该字段，网关侧后续的 metadata 重写(ensureClaudeOAuthMetadataUserID/
	// RewriteUserIDWithMasking) 在 body 里拿不到起点，就无法重建一个合法的
	// user_id，进而导致请求被归类为第三方 app。
	Metadata     json.RawMessage        `json:"metadata,omitempty"`
	OutputConfig *AnthropicOutputConfig `json:"output_config,omitempty"`
}

// AnthropicOutputConfig controls output generation parameters.
type AnthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"` // "low" | "medium" | "high" | "max"
}

// AnthropicThinking configures extended thinking in the Anthropic API.
type AnthropicThinking struct {
	Type         string `json:"type"`                    // "enabled" | "adaptive" | "disabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"` // max thinking tokens
}

// AnthropicMessage is a single message in the Anthropic conversation.
type AnthropicMessage struct {
	Role    string          `json:"role"` // "user" | "assistant"
	Content json.RawMessage `json:"content"`
}

// AnthropicContentBlock is one block inside a message's content array.
type AnthropicContentBlock struct {
	Type string `json:"type"`

	// 2026-05-06 partial port of upstream 0584305e: cache_control breakpoint
	// at the block level lets prompt-cache-key derivation read individual
	// breakpoint anchors (Claude Code uses these as deterministic cache
	// segmentation markers).
	CacheControl *AnthropicCacheControl `json:"cache_control,omitempty"`

	// type=text
	Text string `json:"text,omitempty"`

	// type=thinking
	Thinking string `json:"thinking,omitempty"`

	// type=image | type=document
	// For image: source.type=base64 + media_type image/*.
	// For document: source.type is one of base64 | text | content | url.
	Source *AnthropicImageSource `json:"source,omitempty"`

	// type=document only: optional metadata
	Title   string `json:"title,omitempty"`
	Context string `json:"context,omitempty"`

	// type=tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// type=tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string or []AnthropicContentBlock
	IsError   bool            `json:"is_error,omitempty"`
}

// MarshalJSON ensures text/thinking blocks keep their required payload fields
// even when the value is an empty string. The Anthropic streaming protocol
// expects content_block_start for these block types to include text/thinking,
// and some SDKs break if the field is omitted entirely.
func (b AnthropicContentBlock) MarshalJSON() ([]byte, error) {
	type alias AnthropicContentBlock
	switch b.Type {
	case "text":
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{
			Type: b.Type,
			Text: b.Text,
		})
	case "thinking":
		return json.Marshal(struct {
			Type     string `json:"type"`
			Thinking string `json:"thinking"`
		}{
			Type:     b.Type,
			Thinking: b.Thinking,
		})
	default:
		return json.Marshal(alias(b))
	}
}

// AnthropicImageSource describes the source data for an image OR document
// content block. Despite the historical name, it is reused for document
// blocks, which support more source shapes:
//
//   - image: Type="base64" with MediaType + Data
//   - document + base64: same as image (Type="base64" + MediaType + Data)
//   - document + text: Type="text" with MediaType="text/plain" + Data (raw text)
//   - document + content: Type="content" with Content (nested block array)
//   - document + url: Type="url" with URL (document URL reference)
type AnthropicImageSource struct {
	Type      string          `json:"type"` // "base64" | "text" | "content" | "url"
	MediaType string          `json:"media_type,omitempty"`
	Data      string          `json:"data,omitempty"`
	URL       string          `json:"url,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // nested []AnthropicContentBlock
}

// AnthropicTool describes a tool available to the model.
type AnthropicTool struct {
	Type         string                 `json:"type,omitempty"` // e.g. "web_search_20250305" for server tools
	Name         string                 `json:"name"`
	Description  string                 `json:"description,omitempty"`
	InputSchema  json.RawMessage        `json:"input_schema"` // JSON Schema object
	CacheControl *AnthropicCacheControl `json:"cache_control,omitempty"`
}

// AnthropicCacheControl 对应 Anthropic API 的 cache_control 字段。
// ttl 默认由调用方决定；本项目策略见 claude.DefaultCacheControlTTL。
type AnthropicCacheControl struct {
	Type string `json:"type"`          // "ephemeral"
	TTL  string `json:"ttl,omitempty"` // "5m" / "1h" / 省略=默认 5m（由 Anthropic 判定）
}

// AnthropicResponse is the non-streaming response from POST /v1/messages.
type AnthropicResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"` // "message"
	Role         string                  `json:"role"` // "assistant"
	Content      []AnthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   string                  `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence,omitempty"`
	Usage        AnthropicUsage          `json:"usage"`
}

// AnthropicUsage holds token counts in Anthropic format. Field declaration
// order MUST match real Anthropic response ordering so byte-level signature
// checks on the response body cannot distinguish us from the real thing.
// Real Anthropic order: input_tokens → cache_creation_input_tokens →
// cache_read_input_tokens → output_tokens → server_tool_use. Do not
// reorder.
type AnthropicUsage struct {
	InputTokens              int                       `json:"input_tokens"`
	CacheCreationInputTokens int                       `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                       `json:"cache_read_input_tokens"`
	OutputTokens             int                       `json:"output_tokens"`
	ServerToolUse            *AnthropicServerToolUsage `json:"server_tool_use,omitempty"`
}

// 2026-05-13 P1: server_tool_use counter for hosted Anthropic server tools.
// Real Claude emits message_delta.usage.server_tool_use.web_search_requests
// when the conversation involved N hosted web_search tool invocations.
// omitempty so the wire format is byte-identical to a regular non-search
// response. Only set when WebSearchRequestCount > 0 at finalisation.
type AnthropicServerToolUsage struct {
	WebSearchRequests int `json:"web_search_requests"`
}

// ---------------------------------------------------------------------------
// Anthropic SSE event types
// ---------------------------------------------------------------------------

// AnthropicStreamEvent is a single SSE event in the Anthropic streaming protocol.
type AnthropicStreamEvent struct {
	Type string `json:"type"`

	// message_start
	Message *AnthropicResponse `json:"message,omitempty"`

	// content_block_start
	Index        *int                   `json:"index,omitempty"`
	ContentBlock *AnthropicContentBlock `json:"content_block,omitempty"`

	// content_block_delta
	Delta *AnthropicDelta `json:"delta,omitempty"`

	// message_delta
	Usage *AnthropicUsage `json:"usage,omitempty"`

	// error event — emitted when sub2api detects an upstream condition
	// that should NOT be billed as a successful completion. Anthropic
	// SSE spec includes this terminal event shape; clients that respect
	// it (e.g. NewAPI 24h+) treat it as an upstream error and skip the
	// success-path billing logic.
	//
	// 2026-05-03 codex casjbcfasju 1322455-token incident: empty-output
	// stream was being normalized to message_delta(stop_reason=end_turn)
	// + message_stop, then NewAPI's local_count_tokens=true billed the
	// inbound 1.32M tokens as a successful consumption. Emitting an
	// error event instead breaks that billing path.
	Error *AnthropicErrorBody `json:"error,omitempty"`
}

// AnthropicErrorBody is the payload of an SSE error event.
type AnthropicErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// AnthropicDelta carries incremental content in streaming events.
type AnthropicDelta struct {
	Type string `json:"type,omitempty"` // "text_delta" | "input_json_delta" | "thinking_delta" | "signature_delta"

	// text_delta
	Text string `json:"text,omitempty"`

	// input_json_delta
	PartialJSON string `json:"partial_json,omitempty"`

	// thinking_delta
	Thinking string `json:"thinking,omitempty"`

	// signature_delta
	Signature string `json:"signature,omitempty"`

	// message_delta fields
	StopReason   string  `json:"stop_reason,omitempty"`
	StopSequence *string `json:"stop_sequence,omitempty"`
}

// ---------------------------------------------------------------------------
// OpenAI Responses API types
// ---------------------------------------------------------------------------

// ResponsesRequest is the request body for POST /v1/responses.
type ResponsesRequest struct {
	Model           string              `json:"model"`
	Instructions    string              `json:"instructions,omitempty"`
	Input           json.RawMessage     `json:"input"` // string or []ResponsesInputItem
	MaxOutputTokens *int                `json:"max_output_tokens,omitempty"`
	Temperature     *float64            `json:"temperature,omitempty"`
	TopP            *float64            `json:"top_p,omitempty"`
	Stream          bool                `json:"stream,omitempty"`
	Text            *ResponsesText      `json:"text,omitempty"`
	Tools           []ResponsesTool     `json:"tools,omitempty"`
	Include         []string            `json:"include,omitempty"`
	Store           *bool               `json:"store,omitempty"`
	// 2026-05-06 partial port of upstream 0584305e (Claude Code compat).
	// ParallelToolCalls/PromptCacheKey/PreviousResponseID/Text.Verbosity
	// are needed by openai_messages_continuation/replay_guard wiring to
	// chain Anthropic→Responses turns deterministically without losing
	// upstream prompt cache.
	ParallelToolCalls  *bool               `json:"parallel_tool_calls,omitempty"`
	Reasoning          *ResponsesReasoning `json:"reasoning,omitempty"`
	ToolChoice         json.RawMessage     `json:"tool_choice,omitempty"`
	ServiceTier        string              `json:"service_tier,omitempty"`
	PromptCacheKey     string              `json:"prompt_cache_key,omitempty"`
	PreviousResponseID string              `json:"previous_response_id,omitempty"`
}

// ResponsesText configures structured text output for the Responses API.
//
// 2026-05-06 fork merge: fork uses Format (raw json passthrough), upstream
// 058 added Verbosity. Both are kept — JSON omitempty means they never
// collide on the wire.
type ResponsesText struct {
	Format    json.RawMessage `json:"format,omitempty"`
	Verbosity string          `json:"verbosity,omitempty"` // "low" | "medium" | "high"
}

// ResponsesReasoning configures reasoning effort in the Responses API.
type ResponsesReasoning struct {
	Effort  string `json:"effort"`            // "low" | "medium" | "high" | "xhigh"
	Summary string `json:"summary,omitempty"` // "auto" | "concise" | "detailed"
}

// ResponsesInputItem is one item in the Responses API input array.
// The Type field determines which other fields are populated.
type ResponsesInputItem struct {
	// Common
	Type string `json:"type,omitempty"` // "" for role-based messages

	// Role-based messages (system/user/assistant)
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"` // string or []ResponsesContentPart

	// type=function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	ID        string `json:"id,omitempty"`

	// type=function_call_output
	Output string `json:"output,omitempty"`

	// type=reasoning — 2026-05-12 cctest profile 项 6 (codex audit): 历史
	// assistant thinking blocks 转 Responses input 时不再 ignore, 用 reasoning
	// item + summary 保留语义. 让 GPT 上游看到历史推理摘要, 多轮工具行为不漂.
	Summary []ResponsesSummary `json:"summary,omitempty"`
}

// ResponsesContentPart is a typed content part in a Responses message.
type ResponsesContentPart struct {
	Type string `json:"type"` // "input_text" | "output_text" | "input_image" | "input_file"
	Text string `json:"text,omitempty"`

	// type=input_image
	ImageURL string `json:"image_url,omitempty"` // data URI

	// type=input_file — either FileID (pre-uploaded to OpenAI Files API)
	// or Filename + FileData (data URI with base64 content).
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}

// ResponsesTool describes a tool in the Responses API.
type ResponsesTool struct {
	Type        string          `json:"type"` // "function" | "web_search" | "local_shell" etc.
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ResponsesResponse is the non-streaming response from POST /v1/responses.
type ResponsesResponse struct {
	ID     string            `json:"id"`
	Object string            `json:"object"` // "response"
	Model  string            `json:"model"`
	Status string            `json:"status"` // "completed" | "incomplete" | "failed"
	Output []ResponsesOutput `json:"output"`
	Usage  *ResponsesUsage   `json:"usage,omitempty"`

	// incomplete_details is present when status="incomplete"
	IncompleteDetails *ResponsesIncompleteDetails `json:"incomplete_details,omitempty"`

	// Error is present when status="failed"
	Error *ResponsesError `json:"error,omitempty"`
}

// ResponsesError describes an error in a failed response.
type ResponsesError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ResponsesIncompleteDetails explains why a response is incomplete.
type ResponsesIncompleteDetails struct {
	Reason string `json:"reason"` // "max_output_tokens" | "content_filter"
}

// ResponsesOutput is one output item in a Responses API response.
type ResponsesOutput struct {
	Type string `json:"type"` // "message" | "reasoning" | "function_call" | "web_search_call"

	// type=message
	ID      string                 `json:"id,omitempty"`
	Role    string                 `json:"role,omitempty"`
	Content []ResponsesContentPart `json:"content,omitempty"`
	Status  string                 `json:"status,omitempty"`

	// type=reasoning
	EncryptedContent string             `json:"encrypted_content,omitempty"`
	Summary          []ResponsesSummary `json:"summary,omitempty"`

	// type=function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// type=web_search_call
	Action *WebSearchAction `json:"action,omitempty"`
}

// WebSearchAction describes the search action in a web_search_call output item.
//
// 2026-05-13 P2: added Queries + Sources fields. OpenAI Responses API exposes
// real consulted URLs in `action.sources` when the outgoing request includes
// `web_search_call.action.sources` in the include list (confirmed via codex
// 5/12 doc lookup). Queries is the new plural form of the deprecated Query
// field (both kept — populated only one or the other, omitempty handles wire).
type WebSearchAction struct {
	Type    string              `json:"type,omitempty"`    // "search"
	Query   string              `json:"query,omitempty"`   // deprecated singular form (older Codex versions)
	Queries []string            `json:"queries,omitempty"` // new plural form, list of search keywords
	Sources []WebSearchSourceIn `json:"sources,omitempty"` // requires include=web_search_call.action.sources on the request
}

// WebSearchSourceIn is one entry under WebSearchAction.Sources — a real URL
// that the upstream web search visited / cited. Only present when the
// outgoing Responses request opted in via the include list.
//
// Shape per OpenAI docs: `{"type": "url", "url": "https://..."}`. No title /
// snippet / page_age are exposed here — those are fabricated downstream
// when synthesizing Anthropic-style web_search_result blocks.
type WebSearchSourceIn struct {
	Type string `json:"type,omitempty"` // "url"
	URL  string `json:"url,omitempty"`
}

// ResponsesSummary is a summary text block inside a reasoning output.
type ResponsesSummary struct {
	Type string `json:"type"` // "summary_text"
	Text string `json:"text"`
}

// ResponsesUsage holds token counts in Responses API format.
type ResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`

	// Optional detailed breakdown
	InputTokensDetails  *ResponsesInputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *ResponsesOutputTokensDetails `json:"output_tokens_details,omitempty"`
}

// ResponsesInputTokensDetails breaks down input token usage.
type ResponsesInputTokensDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// ResponsesOutputTokensDetails breaks down output token usage.
type ResponsesOutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// ---------------------------------------------------------------------------
// Responses SSE event types
// ---------------------------------------------------------------------------

// ResponsesStreamEvent is a single SSE event in the Responses streaming protocol.
// The Type field corresponds to the "type" in the JSON payload.
type ResponsesStreamEvent struct {
	Type string `json:"type"`

	// response.created / response.completed / response.done / response.failed / response.incomplete
	Response *ResponsesResponse `json:"response,omitempty"`

	// response.output_item.added / response.output_item.done
	Item *ResponsesOutput `json:"item,omitempty"`

	// response.output_text.delta / response.output_text.done
	OutputIndex  int    `json:"output_index,omitempty"`
	ContentIndex int    `json:"content_index,omitempty"`
	Delta        string `json:"delta,omitempty"`
	Text         string `json:"text,omitempty"`
	ItemID       string `json:"item_id,omitempty"`

	// response.function_call_arguments.delta / done
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// response.reasoning_summary_text.delta / done
	// Reuses Text/Delta fields above, SummaryIndex identifies which summary part
	SummaryIndex int `json:"summary_index,omitempty"`

	// error event fields
	Code  string `json:"code,omitempty"`
	Param string `json:"param,omitempty"`

	// Sequence number for ordering events
	SequenceNumber int `json:"sequence_number,omitempty"`
}

// ---------------------------------------------------------------------------
// OpenAI Chat Completions API types
// ---------------------------------------------------------------------------

// ChatCompletionsRequest is the request body for POST /v1/chat/completions.
type ChatCompletionsRequest struct {
	Model               string             `json:"model"`
	Messages            []ChatMessage      `json:"messages"`
	Instructions        string             `json:"instructions,omitempty"` // OpenAI Responses API compat
	MaxTokens           *int               `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int               `json:"max_completion_tokens,omitempty"`
	Temperature         *float64           `json:"temperature,omitempty"`
	TopP                *float64           `json:"top_p,omitempty"`
	Stream              bool               `json:"stream,omitempty"`
	StreamOptions       *ChatStreamOptions `json:"stream_options,omitempty"`
	Tools               []ChatTool         `json:"tools,omitempty"`
	ToolChoice          json.RawMessage    `json:"tool_choice,omitempty"`
	ResponseFormat      json.RawMessage    `json:"response_format,omitempty"`
	ReasoningEffort     string             `json:"reasoning_effort,omitempty"` // "low" | "medium" | "high" | "xhigh"
	ServiceTier         string             `json:"service_tier,omitempty"`
	Stop                json.RawMessage    `json:"stop,omitempty"` // string or []string

	// Legacy function calling (deprecated but still supported)
	Functions    []ChatFunction  `json:"functions,omitempty"`
	FunctionCall json.RawMessage `json:"function_call,omitempty"`
}

// ChatStreamOptions configures streaming behavior.
type ChatStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatMessage is a single message in the Chat Completions conversation.
type ChatMessage struct {
	Role             string          `json:"role"` // "system" | "user" | "assistant" | "tool" | "function"
	Content          json.RawMessage `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Name             string          `json:"name,omitempty"`
	ToolCalls        []ChatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`

	// Legacy function calling
	FunctionCall *ChatFunctionCall `json:"function_call,omitempty"`
}

// ChatContentPart is a typed content part in a multi-modal message.
type ChatContentPart struct {
	Type     string        `json:"type"` // "text" | "image_url" | "file"
	Text     string        `json:"text,omitempty"`
	ImageURL *ChatImageURL `json:"image_url,omitempty"`
	File     *ChatFile     `json:"file,omitempty"`
}

// ChatImageURL contains the URL for an image content part.
type ChatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // "auto" | "low" | "high"
}

// ChatFile contains file data for a file content part (OpenAI Chat
// Completions multimodal file input). Either FileID (pre-uploaded) or
// Filename + FileData (base64 data URI in file_data).
type ChatFile struct {
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
	FileID   string `json:"file_id,omitempty"`
}

// ChatTool describes a tool available to the model.
type ChatTool struct {
	Type     string        `json:"type"` // "function"
	Function *ChatFunction `json:"function,omitempty"`
}

// ChatFunction describes a function tool definition.
type ChatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ChatToolCall represents a tool call made by the assistant.
// Index is only populated in streaming chunks (omitted in non-streaming responses).
type ChatToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"` // "function"
	Function ChatFunctionCall `json:"function"`
}

// ChatFunctionCall contains the function name and arguments.
type ChatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatCompletionsResponse is the non-streaming response from POST /v1/chat/completions.
type ChatCompletionsResponse struct {
	ID                string       `json:"id"`
	Object            string       `json:"object"` // "chat.completion"
	Created           int64        `json:"created"`
	Model             string       `json:"model"`
	Choices           []ChatChoice `json:"choices"`
	Usage             *ChatUsage   `json:"usage,omitempty"`
	SystemFingerprint string       `json:"system_fingerprint,omitempty"`
	ServiceTier       string       `json:"service_tier,omitempty"`
}

// ChatChoice is a single completion choice.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"` // "stop" | "length" | "tool_calls" | "content_filter"
}

// ChatUsage holds token counts in Chat Completions format.
type ChatUsage struct {
	PromptTokens        int               `json:"prompt_tokens"`
	CompletionTokens    int               `json:"completion_tokens"`
	TotalTokens         int               `json:"total_tokens"`
	PromptTokensDetails *ChatTokenDetails `json:"prompt_tokens_details,omitempty"`
}

// ChatTokenDetails provides a breakdown of token usage.
type ChatTokenDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// ChatCompletionsChunk is a single streaming chunk from POST /v1/chat/completions.
type ChatCompletionsChunk struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"` // "chat.completion.chunk"
	Created           int64             `json:"created"`
	Model             string            `json:"model"`
	Choices           []ChatChunkChoice `json:"choices"`
	Usage             *ChatUsage        `json:"usage,omitempty"`
	SystemFingerprint string            `json:"system_fingerprint,omitempty"`
	ServiceTier       string            `json:"service_tier,omitempty"`
}

// ChatChunkChoice is a single choice in a streaming chunk.
type ChatChunkChoice struct {
	Index        int       `json:"index"`
	Delta        ChatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"` // pointer: null when not final
}

// ChatDelta carries incremental content in a streaming chunk.
type ChatDelta struct {
	Role             string         `json:"role,omitempty"`
	Content          *string        `json:"content,omitempty"` // pointer: omit when not present, null vs "" matters
	ReasoningContent *string        `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
}

// ---------------------------------------------------------------------------
// Shared constants
// ---------------------------------------------------------------------------

// minMaxOutputTokens is the floor for max_output_tokens in a Responses request.
// Very small values may cause upstream API errors, so we enforce a minimum.
const minMaxOutputTokens = 128
