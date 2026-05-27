package service

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// sub2api fu70 codex round-two-stage-header (2026-05-24): tests for the
// early-flush gate + helper.

func TestIsEarlyFlushEligible_HeaderSet(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-GCR-Early-Flush", "1")
	if !isEarlyFlushEligible(req, 100) {
		t.Errorf("X-GCR-Early-Flush:1 must be eligible regardless of body size")
	}
}

func TestIsEarlyFlushEligible_HeaderUnset_SmallBody_Rejected(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	if isEarlyFlushEligible(req, 1024) {
		t.Errorf("no header + small body must NOT be eligible")
	}
}

func TestIsEarlyFlushEligible_LargeBody_Accepted(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	if !isEarlyFlushEligible(req, 100*1024) {
		t.Errorf("100KB body must be eligible (> 64KB threshold)")
	}
}

func TestIsEarlyFlushEligible_EstimatedTokensAccepted(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-GCR-Estimated-Tokens", "10609")
	if !isEarlyFlushEligible(req, 42*1024) {
		t.Errorf("10.6K estimated tokens must be eligible even when body is below 64KB")
	}
}

func TestIsEarlyFlushEligible_EstimatedTokensBelowThresholdRejected(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-GCR-Estimated-Tokens", "7999")
	if isEarlyFlushEligible(req, 42*1024) {
		t.Errorf("estimated tokens below threshold must not be eligible by itself")
	}
}

func TestIsEarlyFlushEligible_EstimatedTokensMalformedIgnored(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-GCR-Estimated-Tokens", "unknown")
	if isEarlyFlushEligible(req, 42*1024) {
		t.Errorf("malformed estimated tokens header must be ignored")
	}
}

func TestIsEarlyFlushEligible_AtBoundary(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	if isEarlyFlushEligible(req, earlyFlushBodyThreshold) {
		t.Errorf("body == threshold (not strictly greater) MUST NOT be eligible")
	}
	if !isEarlyFlushEligible(req, earlyFlushBodyThreshold+1) {
		t.Errorf("body == threshold+1 must be eligible")
	}
}

func TestIsEarlyFlushEligible_HeaderWrongValue(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-GCR-Early-Flush", "0")
	if isEarlyFlushEligible(req, 1024) {
		t.Errorf("X-GCR-Early-Flush:0 (explicit off) must NOT be eligible")
	}
}

func TestIsEarlyFlushEligible_NilRequest(t *testing.T) {
	if !isEarlyFlushEligible(nil, 100*1024) {
		t.Errorf("nil request with large body must still be eligible")
	}
	if isEarlyFlushEligible(nil, 1024) {
		t.Errorf("nil request with small body must NOT be eligible")
	}
}

// --- hasMessageStartInPending ---

func TestHasMessageStartInPending_Empty(t *testing.T) {
	if hasMessageStartInPending(nil) {
		t.Errorf("empty events must report no message_start")
	}
}

func TestHasMessageStartInPending_Found(t *testing.T) {
	events := []apicompat.AnthropicStreamEvent{
		{Type: "ping"},
		{Type: "message_start"},
		{Type: "content_block_start"},
	}
	if !hasMessageStartInPending(events) {
		t.Errorf("must find message_start in middle of list")
	}
}

func TestHasMessageStartInPending_NotFound(t *testing.T) {
	events := []apicompat.AnthropicStreamEvent{
		{Type: "ping"},
		{Type: "content_block_start"},
	}
	if hasMessageStartInPending(events) {
		t.Errorf("no message_start in list — must return false")
	}
}
