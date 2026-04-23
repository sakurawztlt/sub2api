package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConvertResponsesToAnthropicToolChoice guards the reverse path that was
// silently broken when the forward Anthropic→Responses mapping switched from
// nested Chat format to the flat Responses format. The reverse converter must
// accept both shapes and always emit Anthropic's {"type":"tool","name":"X"}.
func TestConvertResponsesToAnthropicToolChoice(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"auto string", `"auto"`, `{"type":"auto"}`},
		{"required string", `"required"`, `{"type":"any"}`},
		{"none string", `"none"`, `{"type":"none"}`},
		{
			name:  "flat responses format",
			input: `{"type":"function","name":"get_weather"}`,
			want:  `{"name":"get_weather","type":"tool"}`,
		},
		{
			name:  "legacy nested format (defensive)",
			input: `{"type":"function","function":{"name":"get_weather"}}`,
			want:  `{"name":"get_weather","type":"tool"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := convertResponsesToAnthropicToolChoice(json.RawMessage(tc.input))
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(got))
		})
	}
}
