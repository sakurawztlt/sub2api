// 2026-05-11 codex audit / R29 traffic capture unit tests.
//
// 覆盖 critical path: truncate / redact / sampling / config / filter
package service

import (
	"strings"
	"testing"
)

func TestTruncateForCapture_NoTruncate(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7"}`)
	s, n, trunc := truncateForCapture(body, 1024)
	if s != string(body) {
		t.Errorf("expected unchanged, got %q", s)
	}
	if n != len(body) {
		t.Errorf("bytes = %d, want %d", n, len(body))
	}
	if trunc {
		t.Error("should not be truncated")
	}
}

func TestTruncateForCapture_Truncate(t *testing.T) {
	body := make([]byte, 10*1024)
	for i := range body {
		body[i] = 'A'
	}
	s, n, trunc := truncateForCapture(body, 100)
	if !trunc {
		t.Error("should be truncated")
	}
	if n != 10*1024 {
		t.Errorf("orig bytes = %d, want 10240", n)
	}
	if !strings.HasSuffix(s, "...(truncated)") {
		t.Errorf("missing truncated suffix, got tail=%q", s[len(s)-30:])
	}
	if len(s) != 100+len("...(truncated)") {
		t.Errorf("clamped len = %d, want %d", len(s), 100+len("...(truncated)"))
	}
}

func TestTruncateForCapture_Empty(t *testing.T) {
	s, n, trunc := truncateForCapture(nil, 1024)
	if s != "" || n != 0 || trunc {
		t.Errorf("nil body should be (\"\", 0, false), got (%q, %d, %v)", s, n, trunc)
	}
}

func TestRedactSensitiveHeaders(t *testing.T) {
	h := map[string]string{
		"Authorization":       "Bearer oat01-very-long-secret-token-here-shouldnt-leak",
		"x-api-key":           "sk-ant-api03-secret",
		"Content-Type":        "application/json",
		"User-Agent":          "claude-cli/2.1.0",
		"Cookie":              "session=abc123",
	}
	out := redactSensitiveHeaders(h)

	// 敏感 header 应被脱敏 — 前 12 char 保留, 后面变 redacted 标记
	if v := out["Authorization"]; !strings.Contains(v, "redacted") {
		t.Errorf("Authorization not redacted: %q", v)
	}
	// 完整 secret 不应在输出
	for k, v := range out {
		if strings.Contains(v, "very-long-secret") {
			t.Errorf("header %q leaks secret: %q", k, v)
		}
		if strings.Contains(v, "sk-ant-api03-secret") {
			t.Errorf("header %q leaks api key: %q", k, v)
		}
	}
	// 非敏感 header 应原样
	if out["Content-Type"] != "application/json" {
		t.Errorf("Content-Type changed: %q", out["Content-Type"])
	}
	if out["User-Agent"] != "claude-cli/2.1.0" {
		t.Errorf("User-Agent changed: %q", out["User-Agent"])
	}
}

func TestSamplingHit_AlwaysOnOff(t *testing.T) {
	if !samplingHit("any", 1.0) {
		t.Error("sampling=1.0 should always hit")
	}
	if samplingHit("any", 0.0) {
		t.Error("sampling=0.0 should never hit")
	}
}

func TestSamplingHit_Stable(t *testing.T) {
	// same reqID + rate → same result (deterministic for diagnosis)
	r1 := samplingHit("req-abc", 0.5)
	r2 := samplingHit("req-abc", 0.5)
	if r1 != r2 {
		t.Errorf("same reqID same rate should be stable, got %v vs %v", r1, r2)
	}
}

func TestLoadTrafficCaptureConfig_Disabled(t *testing.T) {
	t.Setenv("SUB2API_TRAFFIC_CAPTURE_ENABLED", "")
	cfg := LoadTrafficCaptureConfig()
	if cfg.Enabled {
		t.Error("env empty should be disabled")
	}
}

func TestLoadTrafficCaptureConfig_EnabledWithCustomCap(t *testing.T) {
	t.Setenv("SUB2API_TRAFFIC_CAPTURE_ENABLED", "true")
	t.Setenv("SUB2API_TRAFFIC_CAPTURE_MAX_BYTES", "5242880")
	t.Setenv("SUB2API_TRAFFIC_CAPTURE_TTL_HOURS", "48")
	t.Setenv("SUB2API_TRAFFIC_CAPTURE_SAMPLING", "0.5")
	cfg := LoadTrafficCaptureConfig()
	if !cfg.Enabled {
		t.Fatal("env=true should be enabled")
	}
	if cfg.MaxBytes != 5*1024*1024 {
		t.Errorf("MaxBytes = %d, want 5MB", cfg.MaxBytes)
	}
	if cfg.TTL.Hours() != 48 {
		t.Errorf("TTL = %s, want 48h", cfg.TTL)
	}
	if cfg.Sampling != 0.5 {
		t.Errorf("Sampling = %f, want 0.5", cfg.Sampling)
	}
}

func TestLoadTrafficCaptureConfig_Filter(t *testing.T) {
	t.Setenv("SUB2API_TRAFFIC_CAPTURE_ENABLED", "1")
	t.Setenv("SUB2API_TRAFFIC_CAPTURE_FILTER_API_KEY", "10, 20,30")
	t.Setenv("SUB2API_TRAFFIC_CAPTURE_FILTER_ACCOUNT", "5")
	cfg := LoadTrafficCaptureConfig()
	if len(cfg.APIKeyFilter) != 3 {
		t.Errorf("api_key filter size = %d, want 3", len(cfg.APIKeyFilter))
	}
	if !cfg.APIKeyFilter[10] || !cfg.APIKeyFilter[20] || !cfg.APIKeyFilter[30] {
		t.Errorf("api_key filter content wrong: %v", cfg.APIKeyFilter)
	}
	if !cfg.AccountFilter[5] {
		t.Errorf("account filter missing 5: %v", cfg.AccountFilter)
	}
}

func TestTrafficCaptureService_DisabledIsNoop(t *testing.T) {
	// nil client OK when disabled (won't call ent)
	cfg := TrafficCaptureConfig{Enabled: false}
	svc := NewTrafficCaptureService(nil, cfg)
	if svc.Enabled() {
		t.Error("disabled service should report not enabled")
	}
	// Submit should be no-op (no panic, no queue write)
	svc.Submit(TrafficCaptureEntry{InboundBody: []byte("hi")})
	written, dropped, failed := svc.Stats()
	if written != 0 || dropped != 0 || failed != 0 {
		t.Errorf("disabled should have 0 stats: w=%d d=%d f=%d", written, dropped, failed)
	}
}

func TestIsValidCaptureRequestID(t *testing.T) {
	good := []string{
		"req_abc-123",
		"X-Newapi-Request-Id-001",
		"202605111040121803812818268d9d6p2UHrGUE",
	}
	for _, g := range good {
		if !IsValidCaptureRequestID(g) {
			t.Errorf("%q should be valid", g)
		}
	}
	bad := []string{
		"",
		"with spaces",
		"with;semicolon",
		"with/slash",
		"with'quote",
		strings.Repeat("a", 130), // >128
	}
	for _, b := range bad {
		if IsValidCaptureRequestID(b) {
			t.Errorf("%q should be invalid", b)
		}
	}
}
