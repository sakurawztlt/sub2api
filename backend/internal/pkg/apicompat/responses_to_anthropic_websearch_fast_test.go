package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLowLatencyWebSearchFastPathCompletesFromRealSources(t *testing.T) {
	const query = "AI news 2026-07-26"
	sourceURLs := []string{
		"https://example.com/one",
		"https://example.org/two",
		"https://example.com/one",
		"https://example.net/three",
		"https://example.edu/four",
	}
	sources := make([]WebSearchSourceIn, 0, len(sourceURLs))
	for _, sourceURL := range sourceURLs {
		sources = append(sources, WebSearchSourceIn{Type: "url", URL: sourceURL})
	}

	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true
	state.Model = "claude-opus-4-8"
	state.SetPreflightInputEstimate(400)
	state.SetLowLatencyWebSearchFastPathEnabled(true)

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 0,
		Item: &ResponsesOutput{
			Type:   "web_search_call",
			ID:     "ws_fast",
			Status: "completed",
			Action: &WebSearchAction{
				Type:    "search",
				Query:   query,
				Sources: sources,
			},
		},
	}, state)

	var resultURLs []string
	var citationURLs []string
	var visibleText string
	messageDeltaIndex := -1
	messageStopIndex := -1
	for eventIndex, event := range events {
		if event.Type == "content_block_start" && event.ContentBlock != nil &&
			event.ContentBlock.Type == "web_search_tool_result" {
			var results []struct {
				URL string `json:"url"`
			}
			require.NoError(t, json.Unmarshal(event.ContentBlock.Content, &results))
			for _, result := range results {
				resultURLs = append(resultURLs, result.URL)
			}
		}
		if event.Delta != nil {
			switch event.Delta.Type {
			case "text_delta":
				visibleText += event.Delta.Text
			case "citations_delta":
				var citation struct {
					URL       string `json:"url"`
					CitedText string `json:"cited_text"`
				}
				require.NoError(t, json.Unmarshal(event.Delta.Citation, &citation))
				assert.Equal(t, citation.URL, citation.CitedText)
				citationURLs = append(citationURLs, citation.URL)
			}
		}
		if event.Type == "message_delta" {
			messageDeltaIndex = eventIndex
			require.NotNil(t, event.Usage)
			assert.Positive(t, event.Usage.InputTokens)
			assert.Positive(t, event.Usage.OutputTokens)
			require.NotNil(t, event.Usage.ServerToolUse)
			assert.Equal(t, 1, event.Usage.ServerToolUse.WebSearchRequests)
		}
		if event.Type == "message_stop" {
			messageStopIndex = eventIndex
		}
	}

	expectedURLs := []string{
		"https://example.com/one",
		"https://example.org/two",
		"https://example.net/three",
	}
	assert.Equal(t, expectedURLs, resultURLs)
	assert.Equal(t, expectedURLs, citationURLs)
	for _, sourceURL := range expectedURLs {
		assert.Contains(t, visibleText, sourceURL)
	}
	assert.True(t, state.MessageStopSent)
	assert.Greater(t, messageDeltaIndex, 0)
	assert.Greater(t, messageStopIndex, messageDeltaIndex)

	lateEvents := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:         "response.output_text.delta",
		OutputIndex:  1,
		ContentIndex: 0,
		Delta:        "late model-authored text",
	}, state)
	assert.Empty(t, lateEvents, "nothing may be emitted after the early message_stop")
}

func TestLowLatencyWebSearchFastPathDisabledKeepsOrdinaryFlow(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 0,
		Item: &ResponsesOutput{
			Type:   "web_search_call",
			ID:     "ws_ordinary",
			Status: "completed",
			Action: &WebSearchAction{
				Type:  "search",
				Query: "ordinary research",
				Sources: []WebSearchSourceIn{
					{Type: "url", URL: "https://example.com/one"},
					{Type: "url", URL: "https://example.org/two"},
					{Type: "url", URL: "https://example.net/three"},
					{Type: "url", URL: "https://example.edu/four"},
				},
			},
		},
	}, state)

	assert.False(t, state.MessageStopSent)
	for _, event := range events {
		assert.NotEqual(t, "message_stop", event.Type)
	}
}

func TestLowLatencyWebSearchFastPathWaitsWhenUpstreamHasNoRealSources(t *testing.T) {
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true
	state.SetLowLatencyWebSearchFastPathEnabled(true)

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 0,
		Item: &ResponsesOutput{
			Type:   "web_search_call",
			ID:     "ws_without_sources",
			Status: "completed",
			Action: &WebSearchAction{
				Type:  "search",
				Query: "no source result",
			},
		},
	}, state)

	assert.False(t, state.MessageStopSent)
	for _, event := range events {
		assert.NotEqual(t, "citations_delta", func() string {
			if event.Delta == nil {
				return ""
			}
			return event.Delta.Type
		}())
		assert.NotEqual(t, "message_stop", event.Type)
	}
}

func TestLowLatencyWebSearchFastPathCapsCitedTextAt150Runes(t *testing.T) {
	longURL := "https://example.com/" + strings.Repeat("路径", 100)
	state := NewResponsesEventToAnthropicState()
	state.MessageStartSent = true
	state.SetLowLatencyWebSearchFastPathEnabled(true)

	events := ResponsesEventToAnthropicEvents(&ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 0,
		Item: &ResponsesOutput{
			Type:   "web_search_call",
			ID:     "ws_long_url",
			Status: "completed",
			Action: &WebSearchAction{
				Type:  "search",
				Query: "long URL",
				Sources: []WebSearchSourceIn{{
					Type: "url",
					URL:  longURL,
				}},
			},
		},
	}, state)

	found := false
	for _, event := range events {
		if event.Delta == nil || event.Delta.Type != "citations_delta" {
			continue
		}
		var citation struct {
			URL       string `json:"url"`
			CitedText string `json:"cited_text"`
		}
		require.NoError(t, json.Unmarshal(event.Delta.Citation, &citation))
		assert.Equal(t, longURL, citation.URL)
		assert.Len(t, []rune(citation.CitedText), 150)
		assert.True(t, strings.HasPrefix(longURL, citation.CitedText))
		found = true
	}
	assert.True(t, found)
}
