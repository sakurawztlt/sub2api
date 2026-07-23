package service

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/google/uuid"
)

const codexUpstreamMinVersion = "0.144.0"

func ensureCodexIdentityHeaders(h http.Header) {
	if h == nil {
		return
	}
	if strings.TrimSpace(h.Get("user-agent")) == "" {
		h.Set("user-agent", codexCLIUserAgent)
	}
	if strings.TrimSpace(h.Get("originator")) == "" {
		h.Set("originator", "codex_cli_rs")
	}
	if strings.TrimSpace(h.Get("version")) == "" {
		h.Set("version", codexCLIVersion)
	}
	h.Set("OpenAI-Beta", "responses=experimental")
}

func applyOpenAICodexProbeHeaders(h http.Header) {
	if h == nil {
		return
	}
	ensureCodexIdentityHeaders(h)
	h.Set("X-Codex-Window-ID", uuid.NewString())
}

// enforceCodexIdentityHeaders must run after every User-Agent override.
func enforceCodexIdentityHeaders(h http.Header) {
	if h == nil || strings.TrimSpace(h.Get("originator")) == "" {
		return
	}
	originator, pairedUA, ok := openai.PairCodexClientIdentity(h.Get("user-agent"))
	if !ok {
		originator, pairedUA = "codex_cli_rs", codexCLIUserAgent
	}
	h.Set("user-agent", pairedUA)
	h.Set("originator", originator)
	if v := strings.TrimSpace(h.Get("version")); v != "" && CompareVersions(v, codexUpstreamMinVersion) < 0 {
		h.Set("version", codexCLIVersion)
	}
}
