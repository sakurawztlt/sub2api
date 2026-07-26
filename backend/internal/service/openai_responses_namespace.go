package service

import (
	"bytes"
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// stripOpenAIResponsesInputNamespaces removes namespace only from direct input
// array items. Tool declarations and nested namespace fields are intentionally
// preserved so this API-key HTTP compatibility fix cannot disturb tool
// identity or Claude/Codex call pairing.
func stripOpenAIResponsesInputNamespaces(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"namespace"`)) {
		return body, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}

	var rebuilt bytes.Buffer
	rebuilt.Grow(len(input.Raw))
	_ = rebuilt.WriteByte('[')
	changed := false
	first := true
	var stripErr error
	input.ForEach(func(_, item gjson.Result) bool {
		if !first {
			_ = rebuilt.WriteByte(',')
		}
		first = false
		itemBody := []byte(item.Raw)
		if item.IsObject() && item.Get("namespace").Exists() {
			itemBody, stripErr = sjson.DeleteBytes(itemBody, "namespace")
			if stripErr != nil {
				return false
			}
			changed = true
		}
		_, _ = rebuilt.Write(itemBody)
		return true
	})
	if stripErr != nil {
		return body, fmt.Errorf("delete input namespace: %w", stripErr)
	}
	if !changed {
		return body, nil
	}
	_ = rebuilt.WriteByte(']')
	stripped, err := sjson.SetRawBytes(body, "input", rebuilt.Bytes())
	if err != nil {
		return body, fmt.Errorf("replace input after namespace deletion: %w", err)
	}
	return stripped, nil
}
