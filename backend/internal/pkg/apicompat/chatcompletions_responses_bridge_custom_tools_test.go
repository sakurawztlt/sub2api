package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEffectiveResponsesToolsIncludesAdditionalTools(t *testing.T) {
	req := &ResponsesRequest{Input: json.RawMessage(`[
		{"type":"additional_tools","tools":[
			{"type":"custom","name":"exec"},
			{"type":"tool_search"},
			{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"send_message"}]}
		]}
	]`)}

	tools, err := EffectiveResponsesTools(req)
	require.NoError(t, err)
	require.True(t, CustomToolNames(tools)["exec"])
	require.True(t, HasToolSearchTool(tools))
	require.Equal(t, NamespacedToolName{Namespace: "collaboration", Name: "send_message"}, NamespaceToolNames(tools)["collaboration__send_message"])
}

func TestChatCompletionsChunkToResponsesEventsRestoresCustomToolLifecycle(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("glm-5.2")
	state.CustomTools = map[string]bool{"exec": true}
	idx := 0
	events := ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{Choices: []ChatChunkChoice{{Delta: ChatDelta{ToolCalls: []ChatToolCall{{
		Index: &idx, ID: "call_1", Function: ChatFunctionCall{Name: "exec", Arguments: `{"input":"pwd"}`},
	}}}}}}, state)
	events = append(events, FinalizeChatCompletionsResponsesStream(state)...)

	requireStreamToolLifecycle(t, events, "custom_tool_call", "exec", "")
	require.True(t, hasResponsesEventType(events, "response.custom_tool_call_input.done"))
	require.False(t, hasResponsesEventType(events, "response.function_call_arguments.done"))
}

func TestChatCompletionsChunkToResponsesEventsRestoresToolSearchLifecycle(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("glm-5.2")
	state.ToolSearchDeclared = true
	idx := 0
	events := ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{Choices: []ChatChunkChoice{{Delta: ChatDelta{ToolCalls: []ChatToolCall{{
		Index: &idx, ID: "call_2", Function: ChatFunctionCall{Name: toolSearchProxyName, Arguments: `{"query":"docs"}`},
	}}}}}}, state)
	events = append(events, FinalizeChatCompletionsResponsesStream(state)...)

	requireStreamToolLifecycle(t, events, "tool_search_call", "", "")
	require.False(t, hasResponsesEventType(events, "response.function_call_arguments.done"))
}

func TestChatCompletionsChunkToResponsesEventsRestoresNamespaceLifecycle(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("glm-5.2")
	state.NamespaceTools = map[string]NamespacedToolName{
		"collaboration__send_message": {Namespace: "collaboration", Name: "send_message"},
	}
	idx := 0
	events := ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{Choices: []ChatChunkChoice{{Delta: ChatDelta{ToolCalls: []ChatToolCall{{
		Index: &idx, ID: "call_3", Function: ChatFunctionCall{Name: "collaboration__send_message", Arguments: `{}`},
	}}}}}}, state)
	events = append(events, FinalizeChatCompletionsResponsesStream(state)...)

	requireStreamToolLifecycle(t, events, "function_call", "send_message", "collaboration")
	require.True(t, hasResponsesEventType(events, "response.function_call_arguments.done"))
}

func requireStreamToolLifecycle(t *testing.T, events []ResponsesStreamEvent, itemType, name, namespace string) {
	t.Helper()
	var added, done *ResponsesOutput
	for i := range events {
		if events[i].Type == "response.output_item.added" && events[i].Item != nil && events[i].Item.Type == itemType {
			added = events[i].Item
		}
		if events[i].Type == "response.output_item.done" && events[i].Item != nil && events[i].Item.Type == itemType {
			done = events[i].Item
		}
	}
	require.NotNil(t, added)
	require.NotNil(t, done)
	require.Equal(t, name, done.Name)
	require.Equal(t, namespace, done.Namespace)
}

func hasResponsesEventType(events []ResponsesStreamEvent, eventType string) bool {
	for i := range events {
		if events[i].Type == eventType {
			return true
		}
	}
	return false
}
