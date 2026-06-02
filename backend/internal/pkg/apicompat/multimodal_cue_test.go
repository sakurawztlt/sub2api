package apicompat

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codex round-multimodal (2026-05-22): user 反馈 "多模态 5/10 是 GPT fallback
// 可见回答太短". codex 诊断: streamed_bytes 6-8 / output_before 9-10 — GPT
// refuse on "I can't see images". 加 multimodal cue 让 GPT 不要拒.
//
// 测试: 含 image / document 的 user turn → parts 末尾追加 cue input_text;
// 纯文本 user turn → 不追加; env SUB2API_MULTIMODAL_CUE_ENABLED=0 → 不追加.

// Tiny valid PNG (1x1 red) — for image content tests.
var tinyRedPNGBase64 = base64.StdEncoding.EncodeToString([]byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9C, 0x62, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
	0x42, 0x60, 0x82,
})

func buildImageUserReq() *AnthropicRequest {
	contentBlocks := []map[string]any{
		{"type": "text", "text": "What's in this image?"},
		{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": "image/png",
				"data":       tinyRedPNGBase64,
			},
		},
	}
	contentJSON, _ := json.Marshal(contentBlocks)
	return &AnthropicRequest{
		Model:     "gpt-5.5",
		MaxTokens: 100,
		Messages: []AnthropicMessage{
			{Role: "user", Content: contentJSON},
		},
	}
}

// 正例: image user turn → cue appended
func TestRoundMultimodalCue_ImageUserTurn_AppendsCue(t *testing.T) {
	t.Setenv("SUB2API_MULTIMODAL_CUE_ENABLED", "")
	req := buildImageUserReq()
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))

	// Find user message
	var userItem *ResponsesInputItem
	for i := range items {
		if items[i].Type == "message" && items[i].Role == "user" {
			userItem = &items[i]
			break
		}
	}
	require.NotNil(t, userItem, "user message must exist")

	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(userItem.Content, &parts))

	// Must contain image part
	hasImage := false
	for _, p := range parts {
		if p.Type == "input_image" {
			hasImage = true
		}
	}
	require.True(t, hasImage, "image part must be present")

	// Last part must be the cue
	require.NotEmpty(t, parts, "parts must not be empty")
	last := parts[len(parts)-1]
	assert.Equal(t, "input_text", last.Type)
	assert.True(t, strings.Contains(last.Text, "attached image"),
		"cue text must mention attached image; got: %q", last.Text)
	assert.True(t, strings.Contains(last.Text, "refuse"),
		"cue must explicitly tell GPT not to refuse; got: %q", last.Text)
	assert.True(t, strings.Contains(last.Text, "OCR/transcription"),
		"cue must frame visible-text probes as OCR/transcription; got: %q", last.Text)
}

// 负例: 纯文本 user turn → 不追加 cue
func TestRoundMultimodalCue_TextOnlyUserTurn_NoCue(t *testing.T) {
	t.Setenv("SUB2API_MULTIMODAL_CUE_ENABLED", "")
	req := &AnthropicRequest{
		Model:     "gpt-5.5",
		MaxTokens: 100,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"Hello world"`)},
		},
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))

	require.Len(t, items, 1)
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))

	for _, p := range parts {
		if p.Type == "input_text" {
			assert.False(t, strings.Contains(p.Text, "attached image"),
				"text-only turn should NOT contain image cue; got: %q", p.Text)
		}
	}
}

// 不变量: env disable → 不追加 cue 即使有 image
func TestRoundMultimodalCue_EnvDisabled_NoCue(t *testing.T) {
	t.Setenv("SUB2API_MULTIMODAL_CUE_ENABLED", "0")
	req := buildImageUserReq()
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))

	var userItem *ResponsesInputItem
	for i := range items {
		if items[i].Type == "message" && items[i].Role == "user" {
			userItem = &items[i]
			break
		}
	}
	require.NotNil(t, userItem)

	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(userItem.Content, &parts))

	for _, p := range parts {
		if p.Type == "input_text" {
			assert.False(t, strings.Contains(p.Text, "Do not refuse"),
				"env=0 应禁用 cue; got: %q", p.Text)
		}
	}
}

// 不变量: document user turn 也触发 cue (跟 image 同 gate)
func TestRoundMultimodalCue_DocumentUserTurn_AppendsCue(t *testing.T) {
	t.Setenv("SUB2API_MULTIMODAL_CUE_ENABLED", "")
	tinyPDF := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4\n%test\n"))
	contentBlocks := []map[string]any{
		{"type": "text", "text": "Summarize this PDF"},
		{
			"type": "source",
			"source": map[string]any{
				"type":       "base64",
				"media_type": "application/pdf",
				"data":       tinyPDF,
			},
		},
	}
	// Actual document block type in Anthropic is "document".
	contentBlocks[1]["type"] = "document"
	contentJSON, _ := json.Marshal(contentBlocks)
	req := &AnthropicRequest{
		Model:     "gpt-5.5",
		MaxTokens: 100,
		Messages: []AnthropicMessage{
			{Role: "user", Content: contentJSON},
		},
	}
	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))

	var userItem *ResponsesInputItem
	for i := range items {
		if items[i].Type == "message" && items[i].Role == "user" {
			userItem = &items[i]
			break
		}
	}
	require.NotNil(t, userItem)

	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(userItem.Content, &parts))

	hasFile := false
	hasCue := false
	for _, p := range parts {
		if p.Type == "input_file" {
			hasFile = true
		}
		if p.Type == "input_text" && strings.Contains(p.Text, "attached image") {
			hasCue = true
		}
	}
	require.True(t, hasFile, "document must convert to input_file")
	assert.True(t, hasCue, "PDF user turn must also append cue (mentions 'attached image(s) / document(s)')")
}
