package openai

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPairCodexClientIdentity(t *testing.T) {
	tests := []struct {
		name           string
		ua             string
		wantOriginator string
		wantUA         string
		wantOK         bool
	}{
		{
			name:           "cli leading identity",
			ua:             "codex_cli_rs/0.144.1 (Ubuntu 22.4.0; x86_64) xterm-256color",
			wantOriginator: "codex_cli_rs",
			wantUA:         "codex_cli_rs/0.144.1 (Ubuntu 22.4.0; x86_64) xterm-256color",
			wantOK:         true,
		},
		{
			name:           "tui leading identity",
			ua:             "codex-tui/0.140.2 (Mac OS X 14.0; arm64) iTerm (codex-tui; 0.140.2)",
			wantOriginator: "codex-tui",
			wantUA:         "codex-tui/0.140.2 (Mac OS X 14.0; arm64) iTerm (codex-tui; 0.140.2)",
			wantOK:         true,
		},
		{
			name:           "trailer restores overridden identity",
			ua:             "cccc/0.142.0 (Ubuntu 22.4.0; x86_64) screen (codex-tui; 0.142.0)",
			wantOriginator: "codex-tui",
			wantUA:         "codex-tui/0.142.0 (Ubuntu 22.4.0; x86_64) screen (codex-tui; 0.142.0)",
			wantOK:         true,
		},
		{
			name:           "canonicalizes official identity case",
			ua:             "CODEX_CLI_RS/1.0.0",
			wantOriginator: "codex_cli_rs",
			wantUA:         "codex_cli_rs/1.0.0",
			wantOK:         true,
		},
		{name: "rejects slash in trailer", ua: "foo/1.0 (Codex Desktop/2; 1.0)", wantOK: false},
		{name: "rejects control byte", ua: "Codex \x01evil/1.0.0", wantOK: false},
		{name: "rejects non ASCII", ua: "Codex \xc3\xa9vil/1.0.0", wantOK: false},
		{name: "rejects long originator", ua: "Codex " + strings.Repeat("a", 80) + "/1.0.0", wantOK: false},
		{name: "rejects third party", ua: "luna/1.0.0", wantOK: false},
		{name: "rejects forged prefix", ua: "codex_cli_rs_evil/1.0.0", wantOK: false},
		{name: "rejects browser", ua: "Mozilla/5.0", wantOK: false},
		{name: "rejects missing slash", ua: "curl", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originator, pairedUA, ok := PairCodexClientIdentity(tt.ua)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.wantOriginator, originator)
			require.Equal(t, tt.wantUA, pairedUA)
		})
	}
}
