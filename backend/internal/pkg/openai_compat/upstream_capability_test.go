package openai_compat

import "testing"

func TestResolveResponsesSupport(t *testing.T) {
	tests := []struct {
		name  string
		extra map[string]any
		want  AccountResponsesSupport
	}{
		{"nil extra", nil, ResponsesSupportUnknown},
		{"empty extra", map[string]any{}, ResponsesSupportUnknown},
		{"key missing", map[string]any{"other": "value"}, ResponsesSupportUnknown},
		{"value true", map[string]any{ExtraKeyResponsesSupported: true}, ResponsesSupportYes},
		{"value false", map[string]any{ExtraKeyResponsesSupported: false}, ResponsesSupportNo},
		{"value wrong type string", map[string]any{ExtraKeyResponsesSupported: "true"}, ResponsesSupportUnknown},
		{"value wrong type number", map[string]any{ExtraKeyResponsesSupported: 1}, ResponsesSupportUnknown},
		{"value nil", map[string]any{ExtraKeyResponsesSupported: nil}, ResponsesSupportUnknown},
		{"force responses", map[string]any{ExtraKeyResponsesMode: string(ResponsesSupportModeForceResponses)}, ResponsesSupportYes},
		{"force chat completions", map[string]any{ExtraKeyResponsesMode: string(ResponsesSupportModeForceChatCompletions)}, ResponsesSupportNo},
		{"auto follows probe", map[string]any{ExtraKeyResponsesMode: string(ResponsesSupportModeAuto), ExtraKeyResponsesSupported: false}, ResponsesSupportNo},
		{"invalid mode follows probe", map[string]any{ExtraKeyResponsesMode: "bogus", ExtraKeyResponsesSupported: true}, ResponsesSupportYes},
		{"force responses overrides probe false", map[string]any{ExtraKeyResponsesMode: string(ResponsesSupportModeForceResponses), ExtraKeyResponsesSupported: false}, ResponsesSupportYes},
		{"force chat completions overrides probe true", map[string]any{ExtraKeyResponsesMode: string(ResponsesSupportModeForceChatCompletions), ExtraKeyResponsesSupported: true}, ResponsesSupportNo},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveResponsesSupport(tc.extra)
			if got != tc.want {
				t.Errorf("ResolveResponsesSupport(%v) = %v, want %v", tc.extra, got, tc.want)
			}
		})
	}
}

func TestShouldUseResponsesAPI(t *testing.T) {
	tests := []struct {
		name  string
		extra map[string]any
		want  bool
	}{
		// 关键不变量：未探测必须返回 true（保留旧行为）
		{"unknown defaults to true (preserve old behavior)", nil, true},
		{"unknown empty defaults to true", map[string]any{}, true},
		{"unknown wrong type defaults to true", map[string]any{ExtraKeyResponsesSupported: "yes"}, true},

		// 已探测：标记决定
		{"explicitly supported", map[string]any{ExtraKeyResponsesSupported: true}, true},
		{"explicitly unsupported", map[string]any{ExtraKeyResponsesSupported: false}, false},

		// 手动覆盖：覆盖自动探测结果
		{"force responses overrides unsupported probe", map[string]any{ExtraKeyResponsesMode: string(ResponsesSupportModeForceResponses), ExtraKeyResponsesSupported: false}, true},
		{"force chat completions overrides supported probe", map[string]any{ExtraKeyResponsesMode: string(ResponsesSupportModeForceChatCompletions), ExtraKeyResponsesSupported: true}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldUseResponsesAPI(tc.extra)
			if got != tc.want {
				t.Errorf("ShouldUseResponsesAPI(%v) = %v, want %v", tc.extra, got, tc.want)
			}
		})
	}
}

func TestShouldUseResponsesAPIForBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		extra   map[string]any
		baseURL string
		want    bool
	}{
		{
			name:    "unknown official defaults to responses",
			baseURL: "https://api.openai.com/v1",
			want:    true,
		},
		{
			name:    "known chat-only host routes raw chat",
			baseURL: "https://api.deepseek.com/v1",
			want:    false,
		},
		{
			name:    "known chat-only subdomain routes raw chat",
			baseURL: "https://proxy.moonshot.cn/v1",
			want:    false,
		},
		{
			name:    "missing scheme still normalizes",
			baseURL: "dashscope.aliyuncs.com/compatible-mode/v1",
			want:    false,
		},
		{
			name:    "force responses overrides chat-only host",
			extra:   map[string]any{ExtraKeyResponsesMode: string(ResponsesSupportModeForceResponses)},
			baseURL: "https://api.deepseek.com/v1",
			want:    true,
		},
		{
			name:    "force chat completions overrides official host",
			extra:   map[string]any{ExtraKeyResponsesMode: string(ResponsesSupportModeForceChatCompletions)},
			baseURL: "https://api.openai.com/v1",
			want:    false,
		},
		{
			name:    "probe false still routes raw chat",
			extra:   map[string]any{ExtraKeyResponsesSupported: false},
			baseURL: "https://api.openai.com/v1",
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldUseResponsesAPIForBaseURL(tc.extra, tc.baseURL)
			if got != tc.want {
				t.Errorf("ShouldUseResponsesAPIForBaseURL(%v, %q) = %v, want %v", tc.extra, tc.baseURL, got, tc.want)
			}
		})
	}
}

func TestIsKnownChatCompletionsOnlyBaseURL(t *testing.T) {
	tests := []struct {
		baseURL string
		want    bool
	}{
		{"https://api.deepseek.com", true},
		{"https://www.kimi.com/api", true},
		{"https://example.com", false},
		{"", false},
		{"%", false},
	}

	for _, tc := range tests {
		got := IsKnownChatCompletionsOnlyBaseURL(tc.baseURL)
		if got != tc.want {
			t.Errorf("IsKnownChatCompletionsOnlyBaseURL(%q) = %v, want %v", tc.baseURL, got, tc.want)
		}
	}
}

func TestNormalizeResponsesSupportMode(t *testing.T) {
	tests := []struct {
		name string
		mode string
		want ResponsesSupportMode
	}{
		{"empty", "", ResponsesSupportModeAuto},
		{"auto", "auto", ResponsesSupportModeAuto},
		{"force responses", "force_responses", ResponsesSupportModeForceResponses},
		{"force chat completions", "force_chat_completions", ResponsesSupportModeForceChatCompletions},
		{"invalid", "enabled", ResponsesSupportModeAuto},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeResponsesSupportMode(tc.mode)
			if got != tc.want {
				t.Errorf("NormalizeResponsesSupportMode(%q) = %q, want %q", tc.mode, got, tc.want)
			}
		})
	}
}
