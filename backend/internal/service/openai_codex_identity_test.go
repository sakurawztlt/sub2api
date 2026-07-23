package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnforceCodexIdentityHeaders(t *testing.T) {
	tests := []struct {
		name           string
		ua             string
		originator     string
		version        string
		wantUA         string
		wantOriginator string
		wantVersion    string
	}{
		{
			name:           "preserves paired tui identity",
			ua:             "codex-tui/0.144.1 (Mac OS X; arm64) iTerm",
			originator:     "codex_cli_rs",
			version:        "0.144.1",
			wantUA:         "codex-tui/0.144.1 (Mac OS X; arm64) iTerm",
			wantOriginator: "codex-tui",
			wantVersion:    "0.144.1",
		},
		{
			name:           "restores trailer identity and minimum version",
			ua:             "cccc/0.142.0 (Ubuntu; x86_64) screen (codex-tui; 0.142.0)",
			originator:     "cccc",
			version:        "0.142.0",
			wantUA:         "codex-tui/0.142.0 (Ubuntu; x86_64) screen (codex-tui; 0.142.0)",
			wantOriginator: "codex-tui",
			wantVersion:    codexCLIVersion,
		},
		{
			name:           "falls back from third party identity",
			ua:             "luna/1.2.0",
			originator:     "luna",
			version:        "0.145.0",
			wantUA:         codexCLIUserAgent,
			wantOriginator: "codex_cli_rs",
			wantVersion:    "0.145.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := make(http.Header)
			h.Set("user-agent", tt.ua)
			h.Set("originator", tt.originator)
			h.Set("version", tt.version)
			enforceCodexIdentityHeaders(h)
			require.Equal(t, tt.wantUA, h.Get("user-agent"))
			require.Equal(t, tt.wantOriginator, h.Get("originator"))
			require.Equal(t, tt.wantVersion, h.Get("version"))
		})
	}
}

func TestEnforceCodexIdentityHeaders_NoOriginatorIsNoop(t *testing.T) {
	h := make(http.Header)
	h.Set("user-agent", "luna/1.0.0")
	enforceCodexIdentityHeaders(h)
	require.Equal(t, "luna/1.0.0", h.Get("user-agent"))
	require.Empty(t, h.Get("originator"))
}

func requireOpenAICodexProbeHeaders(t *testing.T, h http.Header) {
	t.Helper()
	require.Equal(t, codexCLIUserAgent, h.Get("User-Agent"))
	require.Equal(t, "codex_cli_rs", h.Get("Originator"))
	require.Equal(t, codexCLIVersion, h.Get("Version"))
	require.Equal(t, "responses=experimental", h.Get("OpenAI-Beta"))
	require.NotEmpty(t, h.Get("X-Codex-Window-ID"))
}
