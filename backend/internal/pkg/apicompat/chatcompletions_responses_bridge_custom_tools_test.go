package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
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

func TestHasToolSearchTool(t *testing.T) {
	assert.True(t, HasToolSearchTool([]ResponsesTool{{Type: "tool_search"}}))
	assert.False(t, HasToolSearchTool([]ResponsesTool{{Type: "function", Name: "tool_search"}}))
	assert.False(t, HasToolSearchTool(nil))
}

func TestResponsesToChatCompletionsRequest_NamespaceToolFlattensChildren(t *testing.T) {
	req := &ResponsesRequest{
		Model: "glm-5.2",
		Input: json.RawMessage(`"hi"`),
		Tools: []ResponsesTool{{
			Type: "namespace",
			Name: "gmail",
			Tools: []ResponsesTool{
				{Type: "function", Name: "send", Description: "Send mail", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)},
				{Type: "custom", Name: "ignored_child"},
			},
		}},
	}

	out, err := ResponsesToChatCompletionsRequest(req)
	require.NoError(t, err)
	require.Len(t, out.Tools, 1, "namespace 子工具中仅 function 类型被摊平")

	assert.Equal(t, "gmail__send", out.Tools[0].Function.Name)
	assert.Equal(t, "Send mail", out.Tools[0].Function.Description)
}

func TestResponsesToolsParsing_StringToolBecomesCustom(t *testing.T) {
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"glm-5.2","input":"hi","tools":["exec",{"type":"function","name":"wait"}]}`), &req))

	require.Len(t, req.Tools, 2)
	assert.Equal(t, "custom", req.Tools[0].Type)
	assert.Equal(t, "exec", req.Tools[0].Name)
	assert.Equal(t, "function", req.Tools[1].Type)

	assert.True(t, CustomToolNames(req.Tools)["exec"])
}

func TestFlattenNamespaceToolName_CapsAt64WithHashSuffix(t *testing.T) {
	assert.Equal(t, "gmail__send", flattenNamespaceToolName("gmail", "send"))

	long := flattenNamespaceToolName("very_long_namespace_prefix_for_testing_purposes", "and_a_rather_long_tool_name_too")
	assert.LessOrEqual(t, len(long), 64)
	assert.Contains(t, long, "__")
	// 同输入结果稳定
	assert.Equal(t, long, flattenNamespaceToolName("very_long_namespace_prefix_for_testing_purposes", "and_a_rather_long_tool_name_too"))
}

func TestResponsesInputToChatMessages_ToolSearchCallHistory(t *testing.T) {
	input := json.RawMessage(`[
		{"role":"user","content":"find tools"},
		{"type":"tool_search_call","call_id":"call_s","arguments":{"query":"gmail"}},
		{"type":"tool_search_output","call_id":"call_s","output":{"groups":["gmail"]}}
	]`)

	messages, err := responsesInputToChatMessages("", input)
	require.NoError(t, err)
	require.Len(t, messages, 3)

	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "tool_search", messages[1].ToolCalls[0].Function.Name)
	assert.JSONEq(t, `{"query":"gmail"}`, messages[1].ToolCalls[0].Function.Arguments)

	assert.Equal(t, "tool", messages[2].Role)
	assert.Equal(t, "call_s", messages[2].ToolCallID)
	assert.JSONEq(t, `"{\"groups\":[\"gmail\"]}"`, string(messages[2].Content))
}

func TestResponsesToChatCompletionsRequest_PromotesCompletedToolSearchDiscoveries(t *testing.T) {
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"gpt-5","tools":[
			{"type":"tool_search"},
			{"type":"function","name":"inspect","parameters":{"type":"object"}}
		],"input":[
			{"type":"tool_search_call","call_id":"search_1","arguments":{"query":"workspace"}},
			{"type":"tool_search_output","call_id":"search_1","status":"completed","execution":"client","tools":[
				{"type":"function","name":"inspect","parameters":{"type":"object"}},
				{"type":"custom","name":"exec"},
				{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object"}}]}
			]}
		]}`), &req))

	effective, err := EffectiveResponsesTools(&req)
	require.NoError(t, err)
	require.Len(t, effective, 4, "the identical static discovery must be deduplicated")
	assert.True(t, CustomToolNames(effective)["exec"])
	namespaces := NamespaceToolNames(effective)
	assert.Equal(t, NamespacedToolName{Namespace: "collaboration", Name: "spawn_agent"}, namespaces["collaboration__spawn_agent"])

	chatReq, err := ResponsesToChatCompletionsRequest(&req)
	require.NoError(t, err)
	require.Len(t, chatReq.Tools, 4)
	assert.Equal(t, []string{"tool_search", "inspect", "exec", "collaboration__spawn_agent"}, []string{
		chatReq.Tools[0].Function.Name, chatReq.Tools[1].Function.Name,
		chatReq.Tools[2].Function.Name, chatReq.Tools[3].Function.Name,
	})
	require.Len(t, chatReq.Messages, 2)
	assert.Equal(t, "tool", chatReq.Messages[1].Role)
	var discoveryPayload []map[string]any
	var serialized string
	require.NoError(t, json.Unmarshal(chatReq.Messages[1].Content, &serialized))
	require.NoError(t, json.Unmarshal([]byte(serialized), &discoveryPayload))
	require.Len(t, discoveryPayload, 3)

	responses := ChatCompletionsResponseToResponses(&ChatCompletionsResponse{Choices: []ChatChoice{{
		Message: ChatMessage{Role: "assistant", ToolCalls: []ChatToolCall{
			{ID: "call_1", Type: "function", Function: ChatFunctionCall{Name: "collaboration__spawn_agent", Arguments: `{}`}},
			{ID: "call_2", Type: "function", Function: ChatFunctionCall{Name: "exec", Arguments: `{"input":"pwd"}`}},
		}},
	}}}, req.Model, CustomToolNames(effective), FunctionToolNames(effective), HasToolSearchTool(effective), namespaces)
	require.Len(t, responses.Output, 2)
	assert.Equal(t, "function_call", responses.Output[0].Type)
	assert.Equal(t, "spawn_agent", responses.Output[0].Name)
	assert.Equal(t, "collaboration", responses.Output[0].Namespace)
	assert.Equal(t, "custom_tool_call", responses.Output[1].Type)
	assert.Equal(t, "exec", responses.Output[1].Name)
	assert.Equal(t, "pwd", responses.Output[1].Input)
}

func TestEffectiveResponsesTools_RejectsDiscoveredConflict(t *testing.T) {
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{"tools":[{"type":"tool_search"},{"type":"function","name":"inspect","parameters":{"type":"object"}}],"input":[{"type":"tool_search_output","status":"completed","tools":[{"type":"function","name":"inspect","parameters":{"type":"string"}}]}]}`), &req))
	_, err := EffectiveResponsesTools(&req)
	require.ErrorContains(t, err, "conflicts")
}

func TestResponsesInputToChatMessages_NamespacedFunctionCallHistory(t *testing.T) {
	input := json.RawMessage(`[
		{"type":"function_call","call_id":"call_n","name":"send","namespace":"gmail","arguments":"{\"to\":\"a\"}"},
		{"type":"function_call_output","call_id":"call_n","output":"ok"}
	]`)

	messages, err := responsesInputToChatMessages("", input)
	require.NoError(t, err)
	require.Len(t, messages, 2)

	require.Len(t, messages[0].ToolCalls, 1)
	assert.Equal(t, "gmail__send", messages[0].ToolCalls[0].Function.Name)
}

func TestChatCompletionsChunkToResponsesEvents_CustomToolNameArrivesLate(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("glm-5.2")
	state.CustomTools = map[string]bool{"exec": true}

	idx := 0
	chunk1 := &ChatCompletionsChunk{Choices: []ChatChunkChoice{{Delta: ChatDelta{
		ToolCalls: []ChatToolCall{{Index: &idx, ID: "call_1", Function: ChatFunctionCall{Arguments: `{"inp`}}},
	}}}}
	chunk2 := &ChatCompletionsChunk{Choices: []ChatChunkChoice{{Delta: ChatDelta{
		ToolCalls: []ChatToolCall{{Index: &idx, Function: ChatFunctionCall{Name: "exec", Arguments: `ut": "dir"}`}}},
	}}}}

	var events []ResponsesStreamEvent
	events = append(events, ChatCompletionsChunkToResponsesEvents(chunk1, state)...)
	events = append(events, ChatCompletionsChunkToResponsesEvents(chunk2, state)...)
	events = append(events, FinalizeChatCompletionsResponsesStream(state)...)

	addedCount := 0
	for _, evt := range events {
		switch evt.Type {
		case "response.output_item.added":
			if evt.Item != nil && evt.Item.Type != "reasoning" && evt.Item.Type != "message" {
				addedCount++
				assert.Equal(t, "custom_tool_call", evt.Item.Type, "迟到的名字命中 custom 工具时按 custom_tool_call 宣告")
				assert.Equal(t, "exec", evt.Item.Name)
			}
		case "response.function_call_arguments.delta", "response.function_call_arguments.done":
			t.Fatalf("custom 调用不应产出 function 参数事件: %s", evt.Type)
		case "response.custom_tool_call_input.done":
			assert.Equal(t, "dir", evt.Input)
		}
	}
	assert.Equal(t, 1, addedCount, "工具调用只宣告一次")
}

func TestChatCompletionsChunkToResponsesEvents_FunctionToolNameArrivesLate(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("glm-5.2")
	state.CustomTools = map[string]bool{"exec": true}

	idx := 0
	chunk1 := &ChatCompletionsChunk{Choices: []ChatChunkChoice{{Delta: ChatDelta{
		ToolCalls: []ChatToolCall{{Index: &idx, ID: "call_9", Function: ChatFunctionCall{Arguments: `{"cell`}}},
	}}}}
	chunk2 := &ChatCompletionsChunk{Choices: []ChatChunkChoice{{Delta: ChatDelta{
		ToolCalls: []ChatToolCall{{Index: &idx, Function: ChatFunctionCall{Name: "wait", Arguments: `_id": 3}`}}},
	}}}}

	var events []ResponsesStreamEvent
	events = append(events, ChatCompletionsChunkToResponsesEvents(chunk1, state)...)
	events = append(events, ChatCompletionsChunkToResponsesEvents(chunk2, state)...)
	events = append(events, FinalizeChatCompletionsResponsesStream(state)...)

	deltas := ""
	argsDone := ""
	for _, evt := range events {
		switch evt.Type {
		case "response.function_call_arguments.delta":
			deltas += evt.Delta
		case "response.function_call_arguments.done":
			argsDone = evt.Arguments
		case "response.custom_tool_call_input.done":
			t.Fatal("function 调用不应产出 custom 事件")
		}
	}
	assert.Equal(t, `{"cell_id": 3}`, deltas, "宣告前累积的参数需在宣告时补发")
	assert.Equal(t, `{"cell_id": 3}`, argsDone)
}

// 序列化层（MarshalJSON → responsesItemWire）单独走白名单重组，事件结构体上的字段
// 齐全不代表落到 SSE 线上的 JSON 齐全，必须在 wire 层再断言一次。
func TestResponsesEventToSSE_CustomToolCallItemCarriesAllFields(t *testing.T) {
	evt := ResponsesStreamEvent{
		Type:        "response.output_item.done",
		OutputIndex: 1,
		Item: &ResponsesOutput{
			Type:   "custom_tool_call",
			ID:     "item_1",
			CallID: "call_1",
			Name:   "exec",
			Input:  "dir",
			Status: "completed",
		},
	}

	sse, err := ResponsesEventToSSE(evt)
	require.NoError(t, err)

	assert.Contains(t, sse, `"call_id":"call_1"`)
	assert.Contains(t, sse, `"name":"exec"`)
	assert.Contains(t, sse, `"input":"dir"`)
	assert.Contains(t, sse, `"type":"custom_tool_call"`)
}

func TestNamespaceToolNames_MapsFlattenedNames(t *testing.T) {
	tools := []ResponsesTool{
		{Type: "namespace", Name: "gmail", Tools: []ResponsesTool{
			{Type: "function", Name: "send"},
			{Type: "custom", Name: "skip_me"},
		}},
		{Type: "namespace", Name: "crm", Children: []ResponsesTool{
			{Type: "function", Name: "query"},
		}},
		{Type: "function", Name: "wait"},
	}

	m := NamespaceToolNames(tools)
	require.Len(t, m, 2)
	assert.Equal(t, NamespacedToolName{Namespace: "gmail", Name: "send"}, m["gmail__send"])
	assert.Equal(t, NamespacedToolName{Namespace: "crm", Name: "query"}, m["crm__query"])

	// 摊平名超长时截断加哈希，无法按字符串切分还原，必须经映射反查。
	longNS := "very_long_namespace_prefix_for_testing_purposes"
	longChild := "and_a_rather_long_tool_name_too"
	m2 := NamespaceToolNames([]ResponsesTool{{
		Type: "namespace", Name: longNS,
		Tools: []ResponsesTool{{Type: "function", Name: longChild}},
	}})
	assert.Equal(t, NamespacedToolName{Namespace: longNS, Name: longChild},
		m2[flattenNamespaceToolName(longNS, longChild)])

	assert.Nil(t, NamespaceToolNames(nil))
}

// 内置 tool_search 降级后的代理 function 与客户端声明的同名工具无法区分：回程会把
// 普通工具的调用劫持成 tool_search_call，必须显式拒绝（代理不能改名，codex 的模型
// 侧按 tool_search 这个名字调用）。
func TestResponsesToChatCompletionsRequest_RejectsToolSearchNameConflict(t *testing.T) {
	// 与顶层 function 工具同名。
	_, err := ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model: "glm-5.2",
		Input: json.RawMessage(`"hi"`),
		Tools: []ResponsesTool{
			{Type: "tool_search"},
			{Type: "function", Name: "tool_search"},
		},
	})
	require.Error(t, err, "与内置 tool_search 代理撞名的 function 工具必须拒绝")
	assert.Contains(t, err.Error(), "tool_search")

	// 与顶层 custom 工具同名。
	_, err = ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model: "glm-5.2",
		Input: json.RawMessage(`"hi"`),
		Tools: []ResponsesTool{
			{Type: "custom", Name: "tool_search"},
			{Type: "tool_search"},
		},
	})
	require.Error(t, err, "与内置 tool_search 代理撞名的 custom 工具必须拒绝")

	// 重复声明 type=tool_search 去重后只产出一个代理，不拒绝。
	out, err := ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model: "glm-5.2",
		Input: json.RawMessage(`"hi"`),
		Tools: []ResponsesTool{{Type: "tool_search"}, {Type: "tool_search"}},
	})
	require.NoError(t, err)
	require.Len(t, out.Tools, 1)
	assert.Equal(t, "tool_search", out.Tools[0].Function.Name)
}

func TestResponsesToChatCompletionsRequest_RejectsDuplicateTopLevelExecutableNames(t *testing.T) {
	for _, tools := range [][]ResponsesTool{
		{{Type: "custom", Name: "exec"}, {Type: "function", Name: "exec"}},
		{{Type: "function", Name: "exec"}, {Type: "function", Name: "exec"}},
		{{Type: "custom", Name: "exec"}, {Type: "custom", Name: "exec"}},
	} {
		_, err := ResponsesToChatCompletionsRequest(&ResponsesRequest{
			Model: "glm-5.2",
			Input: json.RawMessage(`"hi"`),
			Tools: tools,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exec")
		assert.Contains(t, err.Error(), "cannot disambiguate")
	}
}

// tool_choice 指向被转换丢弃的工具（如 web_search）或不存在的名字时不能原样转发，
// chat 上游会因选择项指向未声明工具而 400；字符串形式与指向幸存工具的选择保持转发。
func TestResponsesToChatCompletionsRequest_DropsToolChoiceForDroppedTool(t *testing.T) {
	// 强制选择被丢弃的 web_search：工具没了，选择项也必须丢。
	out, err := ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model: "glm-5.2",
		Input: json.RawMessage(`"hi"`),
		Tools: []ResponsesTool{
			{Type: "function", Name: "wait", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)},
			{Type: "web_search"},
		},
		ToolChoice: json.RawMessage(`{"type":"web_search"}`),
	})
	require.NoError(t, err)
	require.Len(t, out.Tools, 1)
	assert.Empty(t, out.ToolChoice, "指向被丢弃服务端工具的 tool_choice 必须丢弃")

	out, err = ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model: "glm-5.2",
		Input: json.RawMessage(`"hi"`),
		Tools: []ResponsesTool{
			{Type: "function", Name: "wait", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)},
			{Type: "web_search"},
			{Type: "x_search"},
		},
		ToolChoice: json.RawMessage(`{"type":"function","name":"web_search"}`),
	})
	require.NoError(t, err)
	require.Len(t, out.Tools, 2)
	assert.Empty(t, out.ToolChoice, "surviving x_search must not keep a function tool_choice named web_search")

	// 具名选择指向不存在的工具名。
	out, err = ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model:      "glm-5.2",
		Input:      json.RawMessage(`"hi"`),
		Tools:      []ResponsesTool{{Type: "function", Name: "wait"}},
		ToolChoice: json.RawMessage(`{"type":"function","name":"missing"}`),
	})
	require.NoError(t, err)
	assert.Empty(t, out.ToolChoice, "指向不存在工具名的 tool_choice 必须丢弃")

	// 字符串形式与指向幸存工具的选择保持原有转发行为。
	out, err = ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model:      "glm-5.2",
		Input:      json.RawMessage(`"hi"`),
		Tools:      []ResponsesTool{{Type: "function", Name: "wait"}},
		ToolChoice: json.RawMessage(`"auto"`),
	})
	require.NoError(t, err)
	assert.JSONEq(t, `"auto"`, string(out.ToolChoice))

	out, err = ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model:      "glm-5.2",
		Input:      json.RawMessage(`"hi"`),
		Tools:      []ResponsesTool{{Type: "function", Name: "wait"}},
		ToolChoice: json.RawMessage(`{"type":"function","name":"wait"}`),
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"function","function":{"name":"wait"}}`, string(out.ToolChoice))
}

// tool_search 工具没有被丢弃而是降级为同名 function 代理，强制选择它的 tool_choice
// 必须同步降级为指向代理的 function 选择，不能静默丢弃（丢弃会把强制搜索退化为
// 自动选择，模型可以不执行搜索）。
func TestResponsesToChatCompletionsRequest_ToolSearchToolChoiceMapsToProxy(t *testing.T) {
	out, err := ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model:      "glm-5.2",
		Input:      json.RawMessage(`"hi"`),
		Tools:      []ResponsesTool{{Type: "tool_search"}},
		ToolChoice: json.RawMessage(`{"type":"tool_search"}`),
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"function","function":{"name":"tool_search"}}`, string(out.ToolChoice))

	// 未声明 type=tool_search 时强制选择它没有可指向的代理，丢弃选择项。
	out, err = ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model:      "glm-5.2",
		Input:      json.RawMessage(`"hi"`),
		Tools:      []ResponsesTool{{Type: "function", Name: "wait"}},
		ToolChoice: json.RawMessage(`{"type":"tool_search"}`),
	})
	require.NoError(t, err)
	assert.Empty(t, out.ToolChoice)
}

// 客户端请求在原生 Responses API 上合法（namespace 子工具按 namespace+name 路由），
// 是摊平转换让名字产生歧义；歧义无法消除时必须显式拒绝整个请求（400），而不是
// 静默降级——否则重复声明发给上游、回程还原到错误工具，问题只能靠抓包定位。
func TestResponsesToChatCompletionsRequest_RejectsAmbiguousFlattenedNames(t *testing.T) {
	// 摊平名与顶层 function 工具撞名。
	_, err := ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model: "glm-5.2",
		Input: json.RawMessage(`"hi"`),
		Tools: []ResponsesTool{
			{Type: "function", Name: "gmail__send"},
			{Type: "namespace", Name: "gmail", Tools: []ResponsesTool{{Type: "function", Name: "send"}}},
		},
	})
	require.Error(t, err, "与顶层工具撞名的摊平必须拒绝")
	assert.Contains(t, err.Error(), "gmail__send")

	// 不同 namespace 组合产生相同摊平名。
	_, err = ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model: "glm-5.2",
		Input: json.RawMessage(`"hi"`),
		Tools: []ResponsesTool{
			{Type: "namespace", Name: "a", Tools: []ResponsesTool{{Type: "function", Name: "b__c"}}},
			{Type: "namespace", Name: "a__b", Tools: []ResponsesTool{{Type: "function", Name: "c"}}},
		},
	})
	require.Error(t, err, "跨 namespace 撞名的摊平必须拒绝")
	assert.Contains(t, err.Error(), "a__b__c")
}

// 完全相同的 (namespace, 子工具) 重复声明不构成歧义：去重后正常转换，不拒绝。
func TestResponsesToChatCompletionsRequest_DedupesIdenticalNamespaceChildren(t *testing.T) {
	out, err := ResponsesToChatCompletionsRequest(&ResponsesRequest{
		Model: "glm-5.2",
		Input: json.RawMessage(`"hi"`),
		Tools: []ResponsesTool{
			{Type: "namespace", Name: "gmail", Tools: []ResponsesTool{
				{Type: "function", Name: "send"},
				{Type: "function", Name: "send"},
			}},
		},
	})
	require.NoError(t, err)
	require.Len(t, out.Tools, 1, "重复声明的同一子工具只声明一次")
	assert.Equal(t, "gmail__send", out.Tools[0].Function.Name)
}

// codex 按 namespace+name 路由 namespace 子工具的调用：回程必须把摊平名还原为
// 裸子工具名并带独立 namespace 字段，平铺名的 function_call 会被 codex 判为
// unsupported call 拒绝执行。
func TestChatCompletionsResponseToResponses_NamespacedToolCallRestored(t *testing.T) {
	resp := &ChatCompletionsResponse{
		ID: "cc-1",
		Choices: []ChatChoice{{
			Message: ChatMessage{
				Role: "assistant",
				ToolCalls: []ChatToolCall{
					{ID: "call_n", Function: ChatFunctionCall{Name: "mcp__svc__echo", Arguments: `{"text":"hi"}`}},
					{ID: "call_9", Function: ChatFunctionCall{Name: "wait", Arguments: `{"cell_id": 3}`}},
				},
			},
		}},
	}
	nsTools := map[string]NamespacedToolName{
		"mcp__svc__echo": {Namespace: "mcp__svc", Name: "echo"},
	}

	out := ChatCompletionsResponseToResponses(resp, "glm-5.2", nil, nil, false, nsTools)
	require.Len(t, out.Output, 2)

	item := out.Output[0]
	assert.Equal(t, "function_call", item.Type)
	assert.Equal(t, "echo", item.Name)
	assert.Equal(t, "mcp__svc", item.Namespace)
	assert.Equal(t, "call_n", item.CallID)
	assert.Equal(t, `{"text":"hi"}`, item.Arguments)

	// 非流式响应体走 ResponsesOutput.MarshalJSON，namespace 必须落到线上 JSON。
	b, err := json.Marshal(item)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"namespace":"mcp__svc"`)
	assert.Contains(t, string(b), `"name":"echo"`)

	// 未命中映射的普通 function 调用不受影响，且不携带 namespace 字段。
	assert.Equal(t, "wait", out.Output[1].Name)
	assert.Empty(t, out.Output[1].Namespace)
	b2, err := json.Marshal(out.Output[1])
	require.NoError(t, err)
	assert.NotContains(t, string(b2), `"namespace"`)
}

func TestChatCompletionsChunkToResponsesEvents_NamespacedToolCallStream(t *testing.T) {
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
