package apicompat

import (
	"encoding/json"
	"fmt"
	"os"
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
	assert.Empty(t, doneEvents, "keep the text block open until output_item.done can supply terminal annotations")

	itemDoneEvents := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 1,
		Item: &ResponsesOutput{
			Type: "message",
			Content: []ResponsesContentPart{{
				Type: "output_text",
				Text: "Reuters reported new AI policy.",
			}},
		},
	}, state)
	assert.Empty(t, itemDoneEvents, "an annotation-free item snapshot must wait for the terminal fallback")

	terminalEvents := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:     "response.completed",
		Response: &ResponsesResponse{Status: "completed"},
	}, state)
	require.GreaterOrEqual(t, len(terminalEvents), 3)
	assert.Equal(t, "content_block_stop", terminalEvents[0].Type)
	require.NotNil(t, terminalEvents[0].Index)
	assert.Equal(t, 2, *terminalEvents[0].Index)
	assert.Empty(t, state.outputTextByPart)
	assert.Empty(t, state.textPartToBlockIdx)
	assert.Empty(t, state.seenTextAnnotations)
	assert.Zero(t, state.cachedOutputTextBytes)
}

func TestResponsesEventToAnthropicEvents_MapsContentPartDoneAnnotationsBeforeStop(t *testing.T) {
	const sourceURL = "https://www.reuters.com/technology/content-part"
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true

	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 0,
		Item: &ResponsesOutput{
			Type:   "web_search_call",
			ID:     "ws_content_part",
			Status: "completed",
			Action: &WebSearchAction{
				Type:    "search",
				Query:   "latest AI news",
				Sources: []WebSearchSourceIn{{Type: "url", URL: sourceURL}},
			},
		},
	}, state)
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		OutputIndex:  1,
		ContentIndex: 0,
		Delta:        "Reuters reported the update.",
	}, state)
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.done",
		OutputIndex:  1,
		ContentIndex: 0,
		Text:         "Reuters reported the update.",
	}, state)

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.content_part.done",
		OutputIndex:  1,
		ContentIndex: 0,
		Part: &ResponsesContentPart{
			Type: "output_text",
			Text: "Reuters reported the update.",
			Annotations: []json.RawMessage{json.RawMessage(`{
				"type":"url_citation",
				"start_index":0,
				"end_index":7,
				"url":"https://www.reuters.com/technology/content-part",
				"title":"Reuters content part"
			}`)},
		},
	}, state)

	require.Len(t, events, 2)
	require.NotNil(t, events[0].Delta)
	assert.Equal(t, "citations_delta", events[0].Delta.Type)
	assert.Equal(t, "content_block_stop", events[1].Type)
	assert.Empty(t, state.outputTextByPart)
}

func TestResponsesEventToAnthropicEvents_WrongTerminalAnnotationPartDoesNotCloseText(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true
	state.webSearchCitationSources["https://example.com/source"] = webSearchCitationSource{
		URL: "https://example.com/source", Title: "Example", EncryptedIndex: "opaque",
	}
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		OutputIndex:  2,
		ContentIndex: 0,
		Delta:        "Example",
	}, state)
	require.True(t, state.ContentBlockOpen)

	annotation := json.RawMessage(`{
		"type":"url_citation",
		"start_index":0,
		"end_index":7,
		"url":"https://example.com/source",
		"title":"Example"
	}`)
	contentPartEvents := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.content_part.done",
		OutputIndex:  99,
		ContentIndex: 0,
		Part: &ResponsesContentPart{
			Type: "output_text", Text: "Example", Annotations: []json.RawMessage{annotation},
		},
	}, state)
	assert.Empty(t, contentPartEvents)
	assert.True(t, state.ContentBlockOpen)

	itemEvents := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 99,
		Item: &ResponsesOutput{
			Type: "message",
			Content: []ResponsesContentPart{{
				Type: "output_text", Text: "Example", Annotations: []json.RawMessage{annotation},
			}},
		},
	}, state)
	assert.Empty(t, itemEvents)
	assert.True(t, state.ContentBlockOpen)
}

func TestResponsesEventToAnthropicEvents_MapsTerminalItemAnnotationsBeforeStop(t *testing.T) {
	const sourceURL = "https://www.reuters.com/technology/terminal-item"
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true

	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 0,
		Item: &ResponsesOutput{
			Type:   "web_search_call",
			ID:     "ws_terminal_item",
			Status: "completed",
			Action: &WebSearchAction{
				Type:    "search",
				Query:   "latest AI news",
				Sources: []WebSearchSourceIn{{Type: "url", URL: sourceURL}},
			},
		},
	}, state)
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		OutputIndex:  1,
		ContentIndex: 0,
		Delta:        "Reuters reported the update.",
	}, state)

	done := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.done",
		OutputIndex:  1,
		ContentIndex: 0,
		Text:         "Reuters reported the update.",
	}, state)
	require.Empty(t, done)

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 1,
		Item: &ResponsesOutput{
			Type: "message",
			Content: []ResponsesContentPart{{
				Type: "output_text",
				Text: "Reuters reported the update.",
				Annotations: []json.RawMessage{json.RawMessage(`{
					"type":"url_citation",
					"start_index":0,
					"end_index":7,
					"url":"https://www.reuters.com/technology/terminal-item?utm_source=openai",
					"title":"Reuters terminal annotation"
				}`)},
			}},
		},
	}, state)

	require.Len(t, events, 2)
	require.Equal(t, "citations_delta", events[0].Delta.Type)
	require.Equal(t, "content_block_stop", events[1].Type)
	assert.Empty(t, state.outputTextByPart)
	assert.Zero(t, state.cachedOutputTextBytes)
}

func TestResponsesEventToAnthropicEvents_MapsTerminalResponseAnnotationsWithoutItemDone(t *testing.T) {
	const sourceURL = "https://www.reuters.com/technology/terminal-response"
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true

	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 0,
		Item: &ResponsesOutput{
			Type:   "web_search_call",
			ID:     "ws_terminal_response",
			Status: "completed",
			Action: &WebSearchAction{
				Type:    "search",
				Query:   "latest AI news",
				Sources: []WebSearchSourceIn{{Type: "url", URL: sourceURL}},
			},
		},
	}, state)
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		OutputIndex:  1,
		ContentIndex: 0,
		Delta:        "Reuters reported the update.",
	}, state)
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.done",
		OutputIndex:  1,
		ContentIndex: 0,
		Text:         "Reuters reported the update.",
	}, state)

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type: "response.incomplete",
		Response: &ResponsesResponse{
			Status: "incomplete",
			Output: []ResponsesOutput{
				{Type: "web_search_call"},
				{
					Type: "message",
					Content: []ResponsesContentPart{{
						Type: "output_text",
						Text: "Reuters reported the update.",
						Annotations: []json.RawMessage{json.RawMessage(`{
							"type":"url_citation",
							"start_index":0,
							"end_index":7,
							"url":"https://www.reuters.com/technology/terminal-response",
							"title":"Reuters terminal response"
						}`)},
					}},
				},
			},
			IncompleteDetails: &ResponsesIncompleteDetails{Reason: "max_output_tokens"},
		},
	}, state)

	require.GreaterOrEqual(t, len(events), 4)
	assert.Equal(t, "citations_delta", events[0].Delta.Type)
	assert.Equal(t, "content_block_stop", events[1].Type)
	assert.Equal(t, "message_delta", events[len(events)-2].Type)
	assert.Equal(t, "message_stop", events[len(events)-1].Type)
}

func TestResponsesEventToAnthropicEvents_UnmappableTerminalAnnotationsWaitForLiteralFallback(t *testing.T) {
	const sourceURL = "https://example.com/real-source"
	annotationCases := []struct {
		name       string
		annotation json.RawMessage
	}{
		{
			name: "file citation",
			annotation: json.RawMessage(`{
				"type":"file_citation",
				"file_id":"file_unmappable",
				"index":0
			}`),
		},
		{
			name: "unknown URL source",
			annotation: json.RawMessage(`{
				"type":"url_citation",
				"start_index":0,
				"end_index":7,
				"url":"https://unknown.example/not-a-search-result"
			}`),
		},
		{
			name: "empty URL citation range",
			annotation: json.RawMessage(`{
				"type":"url_citation",
				"start_index":0,
				"end_index":0,
				"url":"https://example.com/real-source"
			}`),
		},
	}
	terminalCases := []struct {
		name string
		done func(state *ResponsesEventToAnthropicState, part ResponsesContentPart) []AnthropicStreamEvent
	}{
		{
			name: "content part done",
			done: func(state *ResponsesEventToAnthropicState, part ResponsesContentPart) []AnthropicStreamEvent {
				return ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
					Type:         "response.content_part.done",
					OutputIndex:  1,
					ContentIndex: 0,
					Part:         &part,
				}, state)
			},
		},
		{
			name: "output item done",
			done: func(state *ResponsesEventToAnthropicState, part ResponsesContentPart) []AnthropicStreamEvent {
				return ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
					Type:        "response.output_item.done",
					OutputIndex: 1,
					Item: &ResponsesOutput{
						Type:    "message",
						Content: []ResponsesContentPart{part},
					},
				}, state)
			},
		},
	}

	for _, terminalCase := range terminalCases {
		for _, annotationCase := range annotationCases {
			t.Run(terminalCase.name+"/"+annotationCase.name, func(t *testing.T) {
				state := NewResponsesEventToAnthropicState()
				state.MessageStartSent = true
				state.SetIncrementalLiteralCitationsEnabled(true)
				state.webSearchCitationSources[normalizeWebSearchCitationURL(sourceURL)] = webSearchCitationSource{
					URL:            sourceURL,
					Title:          "Real source",
					EncryptedIndex: "opaque",
				}
				text := "Result: " + sourceURL
				ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
					Type:         "response.output_text.delta",
					OutputIndex:  1,
					ContentIndex: 0,
					Delta:        text,
				}, state)
				textDoneEvents := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
					Type:         "response.output_text.done",
					OutputIndex:  1,
					ContentIndex: 0,
					Text:         text,
				}, state)
				require.Len(t, textDoneEvents, 1)
				require.NotNil(t, textDoneEvents[0].Delta)
				assert.Equal(t, "citations_delta", textDoneEvents[0].Delta.Type)

				doneEvents := terminalCase.done(state, ResponsesContentPart{
					Type:        "output_text",
					Text:        text,
					Annotations: []json.RawMessage{annotationCase.annotation},
				})
				require.Len(t, doneEvents, 1)
				assert.Equal(t, "content_block_stop", doneEvents[0].Type)
				require.False(t, state.ContentBlockOpen)

				completedEvents := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
					Type:     "response.completed",
					Response: &ResponsesResponse{Status: "completed"},
				}, state)
				require.Len(t, completedEvents, 2)
				assert.Equal(t, "message_delta", completedEvents[0].Type)
				assert.Equal(t, "message_stop", completedEvents[1].Type)

				var citation struct {
					URL       string `json:"url"`
					CitedText string `json:"cited_text"`
				}
				require.NoError(t, json.Unmarshal(textDoneEvents[0].Delta.Citation, &citation))
				assert.Equal(t, sourceURL, citation.URL)
				assert.Equal(t, sourceURL, citation.CitedText)
			})
		}
	}
}

func TestResponsesEventToAnthropicEvents_EmitsLiteralCitationAsSoonAsURLIsComplete(t *testing.T) {
	const sourceURL = "https://example.com/real-source"
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true
	state.SetIncrementalLiteralCitationsEnabled(true)
	state.webSearchCitationSources[normalizeWebSearchCitationURL(sourceURL)] = webSearchCitationSource{
		URL:            sourceURL,
		Title:          "Real source",
		EncryptedIndex: "opaque",
	}

	first := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		OutputIndex:  1,
		ContentIndex: 0,
		Delta:        "Source: https://example.com/real-",
	}, state)
	require.Len(t, first, 2)
	assert.Equal(t, "content_block_start", first[0].Type)
	assert.Equal(t, "text_delta", first[1].Delta.Type)

	second := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		OutputIndex:  1,
		ContentIndex: 0,
		Delta:        "source",
	}, state)
	require.Len(t, second, 1, "a URL at the current buffer end may still be extended")
	assert.Equal(t, "text_delta", second[0].Delta.Type)

	third := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		OutputIndex:  1,
		ContentIndex: 0,
		Delta:        ". Next item.",
	}, state)
	require.Len(t, third, 2)
	assert.Equal(t, "text_delta", third[0].Delta.Type)
	require.NotNil(t, third[1].Delta)
	assert.Equal(t, "citations_delta", third[1].Delta.Type)

	done := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.done",
		OutputIndex:  1,
		ContentIndex: 0,
		Text:         "Source: " + sourceURL + ". Next item.",
	}, state)
	assert.Empty(t, done, "the incrementally emitted citation must not be duplicated")
}

func TestResponsesEventToAnthropicEvents_RealAnnotationDoesNotDuplicateIncrementalCitation(t *testing.T) {
	const sourceURL = "https://example.com/real-source"
	const prefix = "Source: "
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true
	state.SetIncrementalLiteralCitationsEnabled(true)
	state.webSearchCitationSources[normalizeWebSearchCitationURL(sourceURL)] = webSearchCitationSource{
		URL:            sourceURL,
		Title:          "Real source",
		EncryptedIndex: "opaque",
	}

	text := prefix + sourceURL + "."
	incremental := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		OutputIndex:  1,
		ContentIndex: 0,
		Delta:        text,
	}, state)
	require.Len(t, incremental, 3)
	assert.Equal(t, "citations_delta", incremental[2].Delta.Type)

	annotation, err := json.Marshal(map[string]any{
		"type":        "url_citation",
		"start_index": len([]rune(prefix)),
		"end_index":   len([]rune(prefix + sourceURL)),
		"url":         sourceURL,
	})
	require.NoError(t, err)
	duplicate := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:            "response.output_text.annotation.added",
		OutputIndex:     1,
		ContentIndex:    0,
		AnnotationIndex: 0,
		Annotation:      annotation,
	}, state)
	assert.Empty(t, duplicate)
}

func TestResponsesEventToAnthropicEvents_CapturedCodexTerminalWithoutAnnotationsUsesLiteralRealURLs(t *testing.T) {
	fixture, err := os.ReadFile("testdata/websearch_codex_terminal_no_annotations.jsonl")
	require.NoError(t, err)

	state := NewResponsesEventToAnthropicState()
	state.SetIncrementalLiteralCitationsEnabled(true)
	var emitted []AnthropicStreamEvent
	rawAnnotationEvents := 0
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(fixture)), "\n") {
		var upstream ResponsesStreamEvent
		require.NoErrorf(t, json.Unmarshal([]byte(line), &upstream), "fixture line %d", lineNumber+1)
		if upstream.Type == "response.output_text.annotation.added" {
			rawAnnotationEvents++
		}
		emitted = append(emitted, ResponsesEventToAnthropicEvents(&upstream, state)...)
	}
	require.Zero(t, rawAnnotationEvents, "the captured Codex shape has no upstream annotation events")

	resultURLs := make(map[string]struct{})
	textBlockIndex := -1
	textStopPosition := -1
	citationPositions := make([]int, 0)
	citationURLs := make([]string, 0)
	citedTexts := make([]string, 0)
	for position, event := range emitted {
		if event.Type == "content_block_start" && event.ContentBlock != nil {
			switch event.ContentBlock.Type {
			case "web_search_tool_result":
				var results []struct {
					URL string `json:"url"`
				}
				require.NoError(t, json.Unmarshal(event.ContentBlock.Content, &results))
				for _, result := range results {
					resultURLs[result.URL] = struct{}{}
				}
			case "text":
				require.NotNil(t, event.Index)
				textBlockIndex = *event.Index
			}
		}
		if event.Type == "content_block_stop" && event.Index != nil &&
			*event.Index == textBlockIndex {
			textStopPosition = position
		}
		if event.Delta == nil || event.Delta.Type != "citations_delta" {
			continue
		}
		citationPositions = append(citationPositions, position)
		var citation struct {
			URL       string `json:"url"`
			CitedText string `json:"cited_text"`
		}
		require.NoError(t, json.Unmarshal(event.Delta.Citation, &citation))
		citationURLs = append(citationURLs, citation.URL)
		citedTexts = append(citedTexts, citation.CitedText)
	}

	require.Equal(t, []string{
		"https://creati.ai/ai-news/2026-07-26/",
		"https://techcrunch.com/2026/07/",
		"https://aidailypost.com/archives/2026/07",
	}, citationURLs)
	assert.Equal(t, citationURLs, citedTexts, "literal URL rune ranges must exclude Markdown punctuation")
	require.Len(t, citationPositions, 3, "duplicate and non-source URLs must not become citations")
	require.GreaterOrEqual(t, textStopPosition, 0)
	for index, position := range citationPositions {
		assert.Lessf(t, position, textStopPosition, "citation %d must precede content_block_stop", index)
		_, exists := resultURLs[citationURLs[index]]
		assert.Truef(t, exists, "citation URL %q must exist in the emitted real results", citationURLs[index])
	}
	assert.Empty(t, state.outputTextByPart)
	assert.Zero(t, state.cachedOutputTextBytes)
}

func TestLiteralWebSearchCitationMatches_DeduplicatesAndCapsAt64(t *testing.T) {
	sources := make(map[string]webSearchCitationSource)
	var text strings.Builder
	for index := 0; index < maxAnnotationsPerTextPart+10; index++ {
		rawURL := fmt.Sprintf("https://example.com/source/%d", index)
		key := normalizeWebSearchCitationURL(rawURL)
		sources[key] = webSearchCitationSource{
			URL:            rawURL,
			Title:          fmt.Sprintf("Source %d", index),
			EncryptedIndex: "opaque",
		}
		fmt.Fprintf(&text, "%s ", rawURL)
	}
	_, _ = text.WriteString("https://example.com/source/0")

	matches := literalWebSearchCitationMatches(
		[]rune(text.String()),
		sources,
		maxAnnotationsPerTextPart,
	)
	require.Len(t, matches, maxAnnotationsPerTextPart)
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		_, duplicate := seen[match.URL]
		assert.False(t, duplicate)
		seen[match.URL] = struct{}{}
	}
}

func TestLiteralWebSearchCitationMatches_ASCIIHTTPCaseAndMarkdownSuffixes(t *testing.T) {
	tests := []struct {
		name      string
		sourceURL string
		text      string
		literal   string
	}{
		{
			name:      "mixed-case HTTPS scheme",
			sourceURL: "https://example.com/mixed",
			text:      "See HtTpS://Example.COM/mixed for details",
			literal:   "HtTpS://Example.COM/mixed",
		},
		{
			name:      "uppercase HTTP scheme",
			sourceURL: "http://example.com/plain",
			text:      "See HTTP://EXAMPLE.COM/plain for details",
			literal:   "HTTP://EXAMPLE.COM/plain",
		},
		{
			name:      "Markdown bold",
			sourceURL: "https://example.com/bold",
			text:      "See **https://example.com/bold** now",
			literal:   "https://example.com/bold",
		},
		{
			name:      "Markdown underline",
			sourceURL: "https://example.com/underline",
			text:      "See __https://example.com/underline__ now",
			literal:   "https://example.com/underline",
		},
		{
			name:      "Markdown strikethrough",
			sourceURL: "https://example.com/strike",
			text:      "See ~~https://example.com/strike~~ now",
			literal:   "https://example.com/strike",
		},
		{
			name:      "Markdown table cell",
			sourceURL: "https://example.com/table",
			text:      "source|https://example.com/table|notes",
			literal:   "https://example.com/table",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sources := map[string]webSearchCitationSource{
				normalizeWebSearchCitationURL(test.sourceURL): {
					URL:            test.sourceURL,
					Title:          test.name,
					EncryptedIndex: "opaque",
				},
			}
			matches := literalWebSearchCitationMatches([]rune(test.text), sources, 1)
			require.Len(t, matches, 1)
			assert.Equal(t, test.literal, matches[0].URL)
			assert.Equal(t, test.literal, string([]rune(test.text)[matches[0].StartIndex:matches[0].EndIndex]))
		})
	}
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

func TestResponsesEventToAnthropicEvents_BoundsWebSearchSourceCandidates(t *testing.T) {
	realSources := make([]WebSearchSourceIn, maxRealWebSearchSourceCandidates+100)
	for i := range realSources {
		realSources[i] = WebSearchSourceIn{
			Type: "url",
			URL:  fmt.Sprintf("https://example.com/source/%d", i),
		}
	}
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 0,
		Item: &ResponsesOutput{
			Type:   "web_search_call",
			ID:     "ws_bounded_sources",
			Status: "completed",
			Action: &WebSearchAction{
				Type:    "search",
				Query:   "bounded sources",
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
	require.Len(t, emitted, maxRealWebSearchResultSources)
	assert.LessOrEqual(t, len(state.webSearchCitationSources), maxRealWebSearchResultSources)
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

func TestResponsesEventToAnthropicEvents_PreservesDistinctSpansForSameRealURL(t *testing.T) {
	const sourceURL = "https://www.reuters.com/technology/artificial-intelligence/"
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true
	state.webSearchCitationSources[normalizeWebSearchCitationURL(sourceURL)] = webSearchCitationSource{
		URL: sourceURL, Title: "Reuters", EncryptedIndex: "opaque",
	}
	ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		OutputIndex:  1,
		ContentIndex: 0,
		Delta:        "Reuters and Reuters",
	}, state)

	makeAnnotation := func(index, start, end int) []AnthropicStreamEvent {
		annotation, err := json.Marshal(map[string]any{
			"type":        "url_citation",
			"start_index": start,
			"end_index":   end,
			"url":         sourceURL,
		})
		require.NoError(t, err)
		return ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
			Type:            "response.output_text.annotation.added",
			OutputIndex:     1,
			ContentIndex:    0,
			AnnotationIndex: index,
			Annotation:      annotation,
		}, state)
	}

	first := makeAnnotation(0, 0, 7)
	second := makeAnnotation(1, 12, 19)
	require.Len(t, first, 1)
	require.Len(t, second, 1)
	assert.Equal(t, "citations_delta", first[0].Delta.Type)
	assert.Equal(t, "citations_delta", second[0].Delta.Type)
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
