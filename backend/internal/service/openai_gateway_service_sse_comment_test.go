package service

import "testing"

func TestIsSSECommentLine(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{":", true},
		{": keepalive", true},
		{":heartbeat", true},
		{"  : leading space", true},
		{"\t: leading tab", true},
		{"data: {\"foo\":1}", false},
		{"event: response.created", false},
		{"", false},
		{"  ", false},
	}
	for _, c := range cases {
		if got := isSSECommentLine(c.line); got != c.want {
			t.Errorf("isSSECommentLine(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}
