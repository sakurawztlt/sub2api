package apicompat

import "encoding/json"

// responsesTextFormatToChatResponseFormat converts the Responses API's flat
// json_schema shape into the nested Chat Completions response_format shape.
// Unknown and already-compatible formats pass through byte-for-byte.
func responsesTextFormatToChatResponseFormat(raw json.RawMessage) json.RawMessage {
	raw = bytesTrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	var format map[string]json.RawMessage
	if err := json.Unmarshal(raw, &format); err != nil || rawString(format["type"]) != "json_schema" {
		return raw
	}
	if _, alreadyChatShape := format["json_schema"]; alreadyChatShape {
		return raw
	}

	schema := make(map[string]json.RawMessage, len(format))
	for key, value := range format {
		if key != "type" {
			schema[key] = value
		}
	}
	if len(schema) == 0 {
		return raw
	}

	schemaRaw, err := json.Marshal(schema)
	if err != nil {
		return raw
	}
	typeRaw, _ := json.Marshal("json_schema")
	converted, err := json.Marshal(map[string]json.RawMessage{
		"type":        typeRaw,
		"json_schema": schemaRaw,
	})
	if err != nil {
		return raw
	}
	return converted
}

func chatResponseFormatToResponsesTextFormat(raw json.RawMessage) json.RawMessage {
	raw = normalizedRawJSON(raw)
	if len(raw) == 0 {
		return nil
	}

	obj, ok := rawJSONObject(raw)
	if !ok || rawString(obj["type"]) != "json_schema" {
		return raw
	}

	schemaRaw := normalizedRawJSON(obj["json_schema"])
	if len(schemaRaw) == 0 {
		return raw
	}

	var schema map[string]json.RawMessage
	if err := json.Unmarshal(schemaRaw, &schema); err != nil {
		return raw
	}
	schema["type"] = rawJSONString("json_schema")

	out, err := json.Marshal(schema)
	if err != nil {
		return raw
	}
	return out
}

func normalizedRawJSON(raw json.RawMessage) json.RawMessage {
	raw = bytesTrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func rawJSONObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	return obj, true
}

func rawJSONString(value string) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}
