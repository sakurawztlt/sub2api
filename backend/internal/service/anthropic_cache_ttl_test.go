package service

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeAnthropicCacheTTLString(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"5m", "5m"}, {"5min", "5m"}, {"5mins", "5m"},
		{"5minute", "5m"}, {"5minutes", "5m"}, {"300s", "5m"},
		{" 5MIN ", "5m"},
		{"1h", "1h"}, {"1hr", "1h"}, {"1hour", "1h"},
		{"60m", "1h"}, {"60min", "1h"}, {"3600s", "1h"},
		{"", ""}, {"unknown", ""}, {"30m", ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := NormalizeAnthropicCacheTTLString(c.in); got != c.want {
				t.Errorf("normalize(%q)=%q want %q", c.in, got, c.want)
			}
		})
	}
}

func TestCanonicalizeAnthropicCacheControlTTLInBody(t *testing.T) {
	body := []byte(`{
		"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"5min"}}],
		"tools":[{"name":"foo","cache_control":{"type":"ephemeral","ttl":"1hr"}}],
		"messages":[{"role":"user","content":[
			{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"60min"}},
			{"type":"text","text":"b","cache_control":{"type":"ephemeral","ttl":"forever"}}
		]}]
	}`)
	out := CanonicalizeAnthropicCacheControlTTLInBody(body)

	if g := gjson.GetBytes(out, "system.0.cache_control.ttl").String(); g != "5m" {
		t.Errorf("system 5min → %q want 5m", g)
	}
	if g := gjson.GetBytes(out, "tools.0.cache_control.ttl").String(); g != "1h" {
		t.Errorf("tools 1hr → %q want 1h", g)
	}
	if g := gjson.GetBytes(out, "messages.0.content.0.cache_control.ttl").String(); g != "1h" {
		t.Errorf("messages 60min → %q want 1h", g)
	}
	if g := gjson.GetBytes(out, "messages.0.content.1.cache_control.ttl").String(); g != "1h" {
		t.Errorf("forever should default to 1h, got %q", g)
	}
}

// 5/9 codex audit: forceEphemeralCacheControlTTL 不覆盖客户显式合法 ttl
func TestForceEphemeralCacheControlTTL_DoesNotOverrideValid5m(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"5m"}}]}`)
	out := forceEphemeralCacheControlTTL(body, "1h")
	if g := gjson.GetBytes(out, "system.0.cache_control.ttl").String(); g != "5m" {
		t.Errorf("force-1h should NOT override explicit 5m, got %q", g)
	}
}

func TestForceEphemeralCacheControlTTL_DoesNotOverrideValid1h(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`)
	out := forceEphemeralCacheControlTTL(body, "1h")
	if g := gjson.GetBytes(out, "system.0.cache_control.ttl").String(); g != "1h" {
		t.Errorf("force-1h should keep 1h, got %q", g)
	}
}

// 5/9 codex audit: 缺失 ttl 时 force 1h 仍补
func TestForceEphemeralCacheControlTTL_BackfillsMissingTTL(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}`)
	out := forceEphemeralCacheControlTTL(body, "1h")
	if g := gjson.GetBytes(out, "system.0.cache_control.ttl").String(); g != "1h" {
		t.Errorf("missing ttl should be set to 1h, got %q", g)
	}
}

func TestCanonicalizeAnthropicCacheControlTTLInBody_NoOp(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out := CanonicalizeAnthropicCacheControlTTLInBody(body)
	if string(out) != string(body) {
		t.Errorf("no cache_control → unchanged")
	}
}

func TestCanonicalizeAnthropicCacheControlTTLInBody_AlreadyCanonical(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"5m"}}]}`)
	out := CanonicalizeAnthropicCacheControlTTLInBody(body)
	if string(out) != string(body) {
		t.Errorf("already canonical → unchanged: %s", out)
	}
}
