package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func requireAnySlice(t *testing.T, value any) []any {
	t.Helper()
	items, ok := value.([]any)
	require.True(t, ok)
	return items
}

func requireAnyMap(t *testing.T, value any) map[string]any {
	t.Helper()
	item, ok := value.(map[string]any)
	require.True(t, ok)
	return item
}

func TestFlattenResponsesNamespaces_RewritesDeclarationHistoryAndChoice(t *testing.T) {
	req := map[string]any{
		"model": "gpt-5.5",
		"tools": []any{
			map[string]any{"type": "function", "name": "plain", "description": "keep"},
			map[string]any{
				"type": "namespace",
				"name": "collaboration",
				"tools": []any{
					map[string]any{"type": "function", "name": "spawn_agent", "description": "spawn", "parameters": map[string]any{"type": "object"}},
				},
			},
		},
		"tool_choice": map[string]any{"type": "function", "name": "spawn_agent", "namespace": "collaboration"},
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "spawn_agent", "namespace": "collaboration", "arguments": "{}"},
			map[string]any{"type": "message", "role": "user", "content": "hi", "name": "spawn_agent", "namespace": "collaboration"},
		},
	}

	names, changed, err := FlattenResponsesNamespaces(req)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, ResponsesNamespaceName{Namespace: "collaboration", Name: "spawn_agent"}, names["collaboration__spawn_agent"])

	tools := requireAnySlice(t, req["tools"])
	require.Len(t, tools, 2)
	require.Equal(t, "plain", requireAnyMap(t, tools[0])["name"])
	require.Equal(t, "collaboration__spawn_agent", requireAnyMap(t, tools[1])["name"])
	require.Equal(t, "spawn", requireAnyMap(t, tools[1])["description"])

	choice := requireAnyMap(t, req["tool_choice"])
	require.Equal(t, "collaboration__spawn_agent", choice["name"])
	require.NotContains(t, choice, "namespace")

	input := requireAnySlice(t, req["input"])
	call := requireAnyMap(t, input[0])
	require.Equal(t, "collaboration__spawn_agent", call["name"])
	require.NotContains(t, call, "namespace")
	message := requireAnyMap(t, input[1])
	require.Equal(t, "spawn_agent", message["name"])
	require.Equal(t, "collaboration", message["namespace"])
	require.Equal(t, "gpt-5.5", req["model"])
}

func TestFlattenResponsesNamespaces_RejectsFlatNameCollision(t *testing.T) {
	req := map[string]any{"tools": []any{
		map[string]any{"type": "function", "name": "collaboration__spawn_agent"},
		map[string]any{"type": "namespace", "name": "collaboration", "tools": []any{
			map[string]any{"type": "function", "name": "spawn_agent"},
		}},
	}}

	_, _, err := FlattenResponsesNamespaces(req)
	require.ErrorContains(t, err, "conflicts with a top-level tool")
}

func TestFlattenResponsesNamespaces_NamespaceGroupChoiceFallsBackToAuto(t *testing.T) {
	req := map[string]any{
		"tools": []any{map[string]any{
			"type": "namespace", "name": "collaboration", "tools": []any{
				map[string]any{"type": "function", "name": "spawn_agent"},
				map[string]any{"type": "function", "name": "send_message"},
			},
		}},
		"tool_choice": map[string]any{"type": "namespace", "name": "collaboration"},
	}

	_, changed, err := FlattenResponsesNamespaces(req)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "auto", req["tool_choice"])
}

func TestFlattenResponsesNamespacesExcept_PreservesBuiltInNamespaceAndChoice(t *testing.T) {
	req := map[string]any{
		"tools": []any{
			map[string]any{"type": "namespace", "name": "image_gen", "tools": []any{
				map[string]any{"type": "function", "name": "imagegen"},
			}},
			map[string]any{"type": "namespace", "name": "collaboration", "tools": []any{
				map[string]any{"type": "function", "name": "spawn_agent"},
			}},
		},
		"tool_choice": map[string]any{"type": "namespace", "name": "image_gen"},
	}

	names, changed, err := FlattenResponsesNamespacesExcept(req, map[string]bool{"image_gen": true})
	require.NoError(t, err)
	require.True(t, changed)
	require.Contains(t, names, "collaboration__spawn_agent")
	tools := requireAnySlice(t, req["tools"])
	require.Equal(t, "namespace", requireAnyMap(t, tools[0])["type"])
	require.Equal(t, "image_gen", requireAnyMap(t, tools[0])["name"])
	require.Equal(t, "function", requireAnyMap(t, tools[1])["type"])
	require.Equal(t, "collaboration__spawn_agent", requireAnyMap(t, tools[1])["name"])
	require.Equal(t, map[string]any{"type": "namespace", "name": "image_gen"}, req["tool_choice"])
}

func TestFlattenResponsesNamespaces_RejectsNamespaceCollision(t *testing.T) {
	req := map[string]any{"tools": []any{
		map[string]any{"type": "namespace", "name": "a", "tools": []any{
			map[string]any{"type": "function", "name": "b__c"},
		}},
		map[string]any{"type": "namespace", "name": "a__b", "tools": []any{
			map[string]any{"type": "function", "name": "c"},
		}},
	}}

	_, _, err := FlattenResponsesNamespaces(req)
	require.ErrorContains(t, err, "both flatten")
}

func TestRestoreResponsesNamespaceCalls_RewritesOnlyFunctionCalls(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"output":[{"type":"function_call","name":"collaboration__spawn_agent","call_id":"call_1","arguments":"{}","extra":"keep"},{"type":"function_call","name":"plain","arguments":"{}"},{"type":"message","name":"collaboration__spawn_agent","content":"<tag>&value</tag>"}]}}`)
	names := map[string]ResponsesNamespaceName{
		"collaboration__spawn_agent": {Namespace: "collaboration", Name: "spawn_agent"},
	}

	got, changed, err := RestoreResponsesNamespaceCalls(payload, names)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"type":"response.completed","response":{"output":[{"type":"function_call","name":"spawn_agent","namespace":"collaboration","call_id":"call_1","arguments":"{}","extra":"keep"},{"type":"function_call","name":"plain","arguments":"{}"},{"type":"message","name":"collaboration__spawn_agent","content":"<tag>&value</tag>"}]}}`, string(got))
	require.Contains(t, string(got), "<tag>&value</tag>")
	require.NotContains(t, string(got), `\u003c`)
}

func TestRestoreResponsesNamespaceCalls_RewritesLifecycleItems(t *testing.T) {
	for _, eventType := range []string{"response.output_item.added", "response.output_item.done"} {
		t.Run(eventType, func(t *testing.T) {
			payload := []byte(`{"type":"` + eventType + `","item":{"type":"function_call","name":"collaboration__spawn_agent","arguments":"{}"}}`)
			got, changed, err := RestoreResponsesNamespaceCalls(payload, map[string]ResponsesNamespaceName{
				"collaboration__spawn_agent": {Namespace: "collaboration", Name: "spawn_agent"},
			})
			require.NoError(t, err)
			require.True(t, changed)
			require.JSONEq(t, `{"type":"`+eventType+`","item":{"type":"function_call","name":"spawn_agent","namespace":"collaboration","arguments":"{}"}}`, string(got))
		})
	}
}
