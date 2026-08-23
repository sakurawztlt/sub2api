package service

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func openAICompatSSECompletedResponse(responseID, model string) *http.Response {
	body := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"` + responseID + `","object":"response","model":"` + model + `","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_continuation"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func requireOpenAIMessagesCodexIdentity(t *testing.T, req *http.Request, wantUserAgent, wantOriginator string) {
	t.Helper()
	require.NotNil(t, req)
	require.Equal(t, wantUserAgent, req.Header.Get("User-Agent"))
	require.Equal(t, wantOriginator, req.Header.Get("originator"))
	require.Equal(t, codexCLIVersion, req.Header.Get("version"))
	require.Equal(t, "responses=experimental", req.Header.Get("OpenAI-Beta"))
}
