package service

import (
	"strings"

	"github.com/tidwall/gjson"
)

// openAIRequestPayloadView unwraps Responses WebSocket event envelopes while
// leaving ordinary HTTP objects untouched even when they contain a response
// field for another purpose.
func openAIRequestPayloadView(body []byte) gjson.Result {
	root := parseRawJSONView(body)
	eventType := strings.ToLower(strings.TrimSpace(root.Get("type").String()))
	if strings.HasPrefix(eventType, "response.") {
		if response := root.Get("response"); response.Exists() && response.IsObject() {
			return response
		}
	}
	return root
}
