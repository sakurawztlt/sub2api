package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponsesEventToAnthropicEvents_MapsWebSearchURLCitation(t *testing.T) {
	const sourceURL = "https://www.reuters.com/technology/artificial-intelligence/?z=1&a=2"
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true

	searchEvents := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 0,
		Item: &ResponsesOutput{
			Type:   "web_search_call",
			ID:     "ws_citation",
			Status: "completed",
			Action: &WebSearchAction{
				Type:  "search",
				Query: "latest AI news",
				Sources: []WebSearchSourceIn{{
					Type: "url",
					URL:  sourceURL,
				}},
			},
		},
	}, state)

	var encryptedContent string
	var emittedTitle string
	for _, event := range searchEvents {
		if event.Type != "content_block_start" || event.ContentBlock == nil ||
			event.ContentBlock.Type != "web_search_tool_result" {
			continue
		}
		var results []map[string]string
		require.NoError(t, json.Unmarshal(event.ContentBlock.Content, &results))
		for _, result := range results {
			if result["url"] == sourceURL {
				encryptedContent = result["encrypted_content"]
				emittedTitle = result["title"]
				break
			}
		}
	}
	require.NotEmpty(t, encryptedContent, "the cited URL must exist in the emitted search results")
	require.NotEmpty(t, emittedTitle)

	textEvents := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		OutputIndex:  1,
		ContentIndex: 0,
		Delta:        "Reuters reported new AI policy.",
	}, state)
	require.NotEmpty(t, textEvents)

	var annotationEvent ResponsesStreamEvent
	require.NoError(t, json.Unmarshal([]byte(`{
		"type":"response.output_text.annotation.added",
		"item_id":"msg_citation",
		"output_index":1,
		"content_index":0,
		"annotation_index":0,
		"annotation":{
			"type":"url_citation",
			"start_index":0,
			"end_index":7,
			"url":"https://www.reuters.com/technology/artificial-intelligence/?z=1&a=2&utm_source=openai",
			"title":"OpenAI annotation title"
		}
	}`), &annotationEvent))

	citationEvents := ResponsesEventToAnthropicEvents(&annotationEvent, state)
	require.Len(t, citationEvents, 1)
	citationEvent := citationEvents[0]
	require.Equal(t, "content_block_delta", citationEvent.Type)
	require.NotNil(t, citationEvent.Index)
	assert.Equal(t, 2, *citationEvent.Index)
	require.NotNil(t, citationEvent.Delta)
	assert.Equal(t, "citations_delta", citationEvent.Delta.Type)

	deltaJSON, err := json.Marshal(citationEvent.Delta)
	require.NoError(t, err)
	var deltaWire struct {
		Citation json.RawMessage `json:"citation"`
	}
	require.NoError(t, json.Unmarshal(deltaJSON, &deltaWire))
	var citation map[string]string
	require.NoError(t, json.Unmarshal(deltaWire.Citation, &citation))
	assert.Equal(t, "web_search_result_location", citation["type"])
	assert.Equal(t, sourceURL, citation["url"])
	assert.Equal(t, emittedTitle, citation["title"])
	assert.Equal(t, "Reuters", citation["cited_text"])
	assert.NotEmpty(t, citation["encrypted_index"])
	assert.NotEqual(t, encryptedContent, citation["encrypted_index"],
		"encrypted_index and encrypted_content are separate Anthropic protocol fields")

	sse, err := ResponsesAnthropicEventToSSE(citationEvent)
	require.NoError(t, err)
	assert.Contains(t, sse, `"type":"citations_delta"`)
	assert.Contains(t, sse, `"encrypted_index":"`+citation["encrypted_index"]+`"`)

	doneEvents := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.done",
		OutputIndex:  1,
		ContentIndex: 0,
		Text:         "Reuters reported new AI policy.",
	}, state)
	require.Len(t, doneEvents, 1)
	assert.Equal(t, "content_block_stop", doneEvents[0].Type)
	require.NotNil(t, doneEvents[0].Index)
	assert.Equal(t, 2, *doneEvents[0].Index)
	assert.Empty(t, state.outputTextByPart)
	assert.Empty(t, state.textPartToBlockIdx)
	assert.Empty(t, state.seenTextAnnotations)
	assert.Zero(t, state.cachedOutputTextBytes)
}

func TestResponsesEventToAnthropicEvents_RetainsEveryRealWebSearchSource(t *testing.T) {
	realSources := []WebSearchSourceIn{
		{Type: "url", URL: "https://www.reuters.com/technology/one"},
		{Type: "url", URL: "https://www.reuters.com/technology/two"},
		{Type: "url", URL: "https://techcrunch.com/ai/one"},
		{Type: "url", URL: "https://www.theverge.com/ai/one"},
		{Type: "url", URL: "https://www.informationweek.com/ai/one"},
		{Type: "url", URL: "https://www.bloomberg.com/ai/one"},
		{Type: "url", URL: "https://www.wired.com/ai/one"},
		{Type: "url", URL: "https://arstechnica.com/ai/one"},
	}
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 0,
		Item: &ResponsesOutput{
			Type:   "web_search_call",
			ID:     "ws_all_sources",
			Status: "completed",
			Action: &WebSearchAction{
				Type:    "search",
				Query:   "latest AI news",
				Sources: realSources,
			},
		},
	}, state)

	var emitted []map[string]string
	for _, event := range events {
		if event.Type == "content_block_start" && event.ContentBlock != nil &&
			event.ContentBlock.Type == "web_search_tool_result" {
			require.NoError(t, json.Unmarshal(event.ContentBlock.Content, &emitted))
			break
		}
	}
	require.NotEmpty(t, emitted)
	emittedURLs := make(map[string]struct{}, len(emitted))
	for _, result := range emitted {
		emittedURLs[result["url"]] = struct{}{}
	}
	for _, source := range realSources {
		_, ok := emittedURLs[source.URL]
		assert.Truef(t, ok, "real source %q was omitted from web_search_tool_result", source.URL)
	}
}

func TestResponsesStreamEventWire_PreservesZeroIndexAnnotationAndUnknownPayload(t *testing.T) {
	raw := []byte(`{
		"type":"response.output_text.annotation.added",
		"item_id":"msg_zero",
		"output_index":0,
		"content_index":0,
		"annotation_index":0,
		"annotation":{"type":"file_citation","file_id":"file_123","index":0},
		"sequence_number":0
	}`)
	var event ResponsesStreamEvent
	require.NoError(t, json.Unmarshal(raw, &event))

	wire, err := json.Marshal(event)
	require.NoError(t, err)

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &fields))
	for _, key := range []string{
		"output_index",
		"content_index",
		"annotation_index",
		"sequence_number",
		"annotation",
	} {
		assert.Contains(t, fields, key)
	}
	assert.JSONEq(t,
		`{"type":"file_citation","file_id":"file_123","index":0}`,
		string(fields["annotation"]),
	)
}

func TestResponsesEventToAnthropicEvents_DeduplicatesAndScopesAnnotationsByTextPart(t *testing.T) {
	const sourceURL = "https://www.reuters.com/technology/artificial-intelligence/"
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true

	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 0,
		Item: &ResponsesOutput{
			Type:   "web_search_call",
			ID:     "ws_scoped",
			Status: "completed",
			Action: &WebSearchAction{
				Type:  "search",
				Query: "latest AI news",
				Sources: []WebSearchSourceIn{{
					Type: "url",
					URL:  sourceURL,
				}},
			},
		},
	}, state)
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		OutputIndex:  1,
		ContentIndex: 0,
		Delta:        "Reuters",
	}, state)

	annotation := json.RawMessage(`{
		"type":"url_citation",
		"start_index":0,
		"end_index":7,
		"url":"https://www.reuters.com/technology/artificial-intelligence/?utm_source=openai",
		"title":"Reuters"
	}`)
	first := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:            "response.output_text.annotation.added",
		OutputIndex:     1,
		ContentIndex:    0,
		AnnotationIndex: 0,
		Annotation:      annotation,
	}, state)
	require.Len(t, first, 1)

	duplicate := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:            "response.output_text.annotation.added",
		OutputIndex:     1,
		ContentIndex:    0,
		AnnotationIndex: 0,
		Annotation:      annotation,
	}, state)
	assert.Empty(t, duplicate)

	wrongPart := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:            "response.output_text.annotation.added",
		OutputIndex:     1,
		ContentIndex:    1,
		AnnotationIndex: 0,
		Annotation:      annotation,
	}, state)
	assert.Empty(t, wrongPart)
}

func TestResponsesEventToAnthropicEvents_DropsCitationAbsentFromToolResult(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		OutputIndex:  1,
		ContentIndex: 0,
		Delta:        "Unknown",
	}, state)

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:            "response.output_text.annotation.added",
		OutputIndex:     1,
		ContentIndex:    0,
		AnnotationIndex: 0,
		Annotation: json.RawMessage(`{
			"type":"url_citation",
			"start_index":0,
			"end_index":7,
			"url":"https://example.invalid/not-in-results",
			"title":"Unknown"
		}`),
	}, state)
	assert.Empty(t, events)
}

func TestResponsesEventToAnthropicEvents_BoundsCitationTextCache(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true
	partKey := responsesTextPartKey{OutputIndex: 1, ContentIndex: 0}

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		OutputIndex:  1,
		ContentIndex: 0,
		Delta:        strings.Repeat("x", maxCitationTextBytesPerPart+1),
	}, state)
	require.NotEmpty(t, events, "the visible text stream must not be truncated")
	assert.Contains(t, state.overflowedTextParts, partKey)
	assert.NotContains(t, state.outputTextByPart, partKey)
	assert.Zero(t, state.cachedOutputTextBytes)

	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.done",
		OutputIndex:  1,
		ContentIndex: 0,
	}, state)
	assert.Empty(t, state.overflowedTextParts)
}

func TestRuneTextRange_ClampsAnthropicCitedTextLimit(t *testing.T) {
	assert.Len(t, []rune(runeTextRange(strings.Repeat("界", 200), 0, 200)), 150)
}

func TestResponsesEventToAnthropicEvents_DisablesCitationCacheAfterPartLimit(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true

	for contentIndex := 0; contentIndex <= maxCitationTextParts; contentIndex++ {
		events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
			Type:         "response.output_text.delta",
			OutputIndex:  1,
			ContentIndex: contentIndex,
			Delta:        "x",
		}, state)
		require.NotEmpty(t, events, "visible text must still stream after citation tracking stops")
	}

	assert.True(t, state.citationTrackingDisabled)
	assert.Nil(t, state.outputTextByPart)
	assert.Nil(t, state.textPartToBlockIdx)
	assert.Nil(t, state.seenTextAnnotations)
	assert.Zero(t, state.cachedOutputTextBytes)
}
