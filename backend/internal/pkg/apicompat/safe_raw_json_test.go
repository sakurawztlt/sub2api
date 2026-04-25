package apicompat

import (
	"encoding/json"
	"testing"
)

// 2026-04-25 codex 报告: NewAPI channel-7 看到 2 条 500
// "unexpected end of JSON input"。根因 — function_call.arguments 在
// OpenAI 协议里是字符串, 客户没传参时为 "" (空串)。本包 (apicompat)
// 把这个字符串直接 cast 成 json.RawMessage:
//
//   Input: json.RawMessage(item.Arguments)  // ""  → 非法 raw JSON
//
// json.RawMessage 是 []byte 别名, 不校验。下游 c.JSON 序列化时把空
// bytes 当 JSON literal 试图嵌入, encoder 报 "unexpected end of JSON
// input", 客户拿到坏 500。
//
// safeRawJSON 兜底: 空/非法 → "{}", 合法原样透。客户拿到的是 input:{}
// 这种 Anthropic-native "tool 没参数" 表达, 跟真 Anthropic 行为一致,
// 不会暴露 sub2api 中转层。

func TestSafeRawJSON_EmptyStringFallsBackToEmptyObject(t *testing.T) {
	got := safeRawJSON("")
	if string(got) != "{}" {
		t.Errorf("empty string should fall back to {}, got %q", got)
	}
	if !json.Valid(got) {
		t.Errorf("output must be valid JSON, got %q", got)
	}
}

func TestSafeRawJSON_WhitespaceOnlyFallsBackToEmptyObject(t *testing.T) {
	for _, s := range []string{"   ", "\t", "\n", " \t\n "} {
		got := safeRawJSON(s)
		if string(got) != "{}" {
			t.Errorf("whitespace %q should fall back to {}, got %q", s, got)
		}
	}
}

func TestSafeRawJSON_InvalidJSONFallsBackToEmptyObject(t *testing.T) {
	for _, s := range []string{"not json", "{", "{not:json}", "}{", "[1,2,"} {
		got := safeRawJSON(s)
		if string(got) != "{}" {
			t.Errorf("invalid %q should fall back to {}, got %q", s, got)
		}
	}
}

func TestSafeRawJSON_ValidJSONPassThrough(t *testing.T) {
	cases := []string{
		`{}`,
		`{"x":1}`,
		`{"city":"NYC","unit":"f"}`,
		`{"nested":{"a":[1,2,3]}}`,
		`[]`,
		`[1,2,3]`,
		`null`,
		`true`,
		`42`,
		`"plain string"`,
	}
	for _, s := range cases {
		got := safeRawJSON(s)
		if string(got) != s {
			t.Errorf("valid JSON %q should pass through unchanged, got %q", s, got)
		}
	}
}
