package service

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// 5/8 codex audit #1+#2: 验 isMeaningfulAnthropicEvent 分类.
// 真实数据 → true; 元数据 (会让客户傻等的那种) → false.

func TestIsMeaningfulAnthropicEvent_RealData_Meaningful(t *testing.T) {
	cases := []struct {
		name string
		ev   apicompat.AnthropicStreamEvent
	}{
		{"text_delta", apicompat.AnthropicStreamEvent{Type: "content_block_delta", Delta: &apicompat.AnthropicDelta{Type: "text_delta"}}},
		{"thinking_delta", apicompat.AnthropicStreamEvent{Type: "content_block_delta", Delta: &apicompat.AnthropicDelta{Type: "thinking_delta"}}},
		{"input_json_delta", apicompat.AnthropicStreamEvent{Type: "content_block_delta", Delta: &apicompat.AnthropicDelta{Type: "input_json_delta"}}},
		{"signature_delta", apicompat.AnthropicStreamEvent{Type: "content_block_delta", Delta: &apicompat.AnthropicDelta{Type: "signature_delta"}}},
		{"tool_use_block_start", apicompat.AnthropicStreamEvent{Type: "content_block_start", ContentBlock: &apicompat.AnthropicContentBlock{Type: "tool_use"}}},
		{"server_tool_use_block_start", apicompat.AnthropicStreamEvent{Type: "content_block_start", ContentBlock: &apicompat.AnthropicContentBlock{Type: "server_tool_use"}}},
		{"message_delta_with_usage", apicompat.AnthropicStreamEvent{Type: "message_delta"}},
		{"message_stop", apicompat.AnthropicStreamEvent{Type: "message_stop"}},
		{"error", apicompat.AnthropicStreamEvent{Type: "error"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !isMeaningfulAnthropicEvent(c.ev) {
				t.Errorf("%s should be meaningful (true), got false", c.name)
			}
		})
	}
}

func TestIsMeaningfulAnthropicEvent_Metadata_NotMeaningful(t *testing.T) {
	cases := []struct {
		name string
		ev   apicompat.AnthropicStreamEvent
	}{
		// 这些是导致"成功 200 但 0 输出 等 180s"的元数据事件 — 不应触发 WriteHeader
		{"message_start", apicompat.AnthropicStreamEvent{Type: "message_start"}},
		{"ping", apicompat.AnthropicStreamEvent{Type: "ping"}},
		{"empty_text_block_start", apicompat.AnthropicStreamEvent{Type: "content_block_start", ContentBlock: &apicompat.AnthropicContentBlock{Type: "text"}}},
		{"empty_thinking_block_start", apicompat.AnthropicStreamEvent{Type: "content_block_start", ContentBlock: &apicompat.AnthropicContentBlock{Type: "thinking"}}},
		{"content_block_start_no_block", apicompat.AnthropicStreamEvent{Type: "content_block_start"}},
		{"content_block_stop", apicompat.AnthropicStreamEvent{Type: "content_block_stop"}},
		{"unknown_type", apicompat.AnthropicStreamEvent{Type: "future_unknown"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if isMeaningfulAnthropicEvent(c.ev) {
				t.Errorf("%s should NOT be meaningful (false), got true", c.name)
			}
		})
	}
}

// codex 5/8 audit: keepalive ping must NOT fire before the first meaningful
// event — gin.ResponseWriter.Write would implicitly commit HTTP 200, the
// firstMeaningfulEventTimeout would no longer be able to return a clean
// 502, and the client would sit on a "successful" empty stream until
// upstream closes. shouldEmitKeepalivePing encodes the gate.

func TestShouldEmitKeepalivePing_BlocksBeforeFirstMeaningful(t *testing.T) {
	// 30s quiet, well past 10s interval — but firstMeaningfulSeen=false.
	// Must not emit; firstMeaningfulEventTimeout owns the failure path.
	if shouldEmitKeepalivePing(false /*disconnected*/, false /*firstMeaningfulSeen*/, 30*time.Second, 10*time.Second) {
		t.Errorf("keepalive emitted BEFORE first meaningful event — would commit 200 and bypass firstMeaningfulEventTimeout")
	}
}

func TestShouldEmitKeepalivePing_BlocksWhenDisconnected(t *testing.T) {
	if shouldEmitKeepalivePing(true /*disconnected*/, true, 30*time.Second, 10*time.Second) {
		t.Errorf("keepalive emitted to disconnected client")
	}
}

func TestShouldEmitKeepalivePing_BlocksBeforeInterval(t *testing.T) {
	// firstMeaningful seen, but only 5s since last data — interval is 10s.
	if shouldEmitKeepalivePing(false, true, 5*time.Second, 10*time.Second) {
		t.Errorf("keepalive emitted before interval elapsed")
	}
}

func TestShouldEmitKeepalivePing_AllowsAfterFirstMeaningfulAndInterval(t *testing.T) {
	if !shouldEmitKeepalivePing(false, true, 15*time.Second, 10*time.Second) {
		t.Errorf("keepalive blocked when all conditions met")
	}
}
