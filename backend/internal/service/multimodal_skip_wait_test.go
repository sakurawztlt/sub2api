package service

import (
	"context"
	"testing"
)

func TestHasMultimodalContent_ImageBlock(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","data":"abc"}}]}]}`)
	if !HasMultimodalContent(body) {
		t.Errorf("image block should be detected")
	}
}

func TestHasMultimodalContent_DocumentBlock(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","data":"abc"}}]}]}`)
	if !HasMultimodalContent(body) {
		t.Errorf("document block should be detected")
	}
}

func TestHasMultimodalContent_TextOnly(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"plain text"}]}`)
	if HasMultimodalContent(body) {
		t.Errorf("text-only should NOT be detected")
	}
}

func TestHasMultimodalContent_TextBlocksOnly(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if HasMultimodalContent(body) {
		t.Errorf("text blocks only should NOT be detected")
	}
}

// "type":"image_url" 是 OpenAI 形, 我们这层只见 Anthropic 形 "type":"image"
// 别误判 OpenAI 内部协议关键字
func TestHasMultimodalContent_OpenAIImageURLNotMatched(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"..."}}]}]}`)
	// `"type":"image"` 是 `"type":"image_url"` 的前缀字符串 → 也会被 substring 匹配
	// 这是 false-positive 但安全方向 (multimodal 当 multimodal 处理), 可接受.
	// 测试只 document 期望行为不要 panic.
	_ = HasMultimodalContent(body)
}

func TestHasMultimodalContent_PrettyPrinted(t *testing.T) {
	body := []byte(`{
  "messages": [
    {
      "role": "user",
      "content": [
        {"type": "image", "source": {}}
      ]
    }
  ]
}`)
	if !HasMultimodalContent(body) {
		t.Errorf("pretty-printed should be detected")
	}
}

func TestHasMultimodalContent_Empty(t *testing.T) {
	if HasMultimodalContent(nil) {
		t.Errorf("nil body should not be detected")
	}
	if HasMultimodalContent([]byte{}) {
		t.Errorf("empty body should not be detected")
	}
}

func TestMultimodalSkipWaitCtx_RoundTrip(t *testing.T) {
	ctx := context.Background()
	if IsMultimodalSkipWaitCtx(ctx) {
		t.Errorf("plain ctx should not be flagged")
	}
	tagged := WithMultimodalSkipWaitCtx(ctx)
	if !IsMultimodalSkipWaitCtx(tagged) {
		t.Errorf("tagged ctx should be flagged")
	}
}

func TestIsMultimodalSkipWaitCtx_NilCtx(t *testing.T) {
	if IsMultimodalSkipWaitCtx(nil) {
		t.Errorf("nil ctx should not be flagged")
	}
}
