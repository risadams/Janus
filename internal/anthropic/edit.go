package anthropic

import (
	"bytes"
	"encoding/json"
)

// SetResponseModel rewrites the model name in a Messages response: either a
// whole message, or a stream's message_start event, where the message is
// nested one level down. Everything else is left byte for byte as it was.
// ok is false if data is neither, and data is then returned unchanged.
//
// It exists so that a request answered by a local model can report the
// name the client asked for, which is what the client expects to see.
func SetResponseModel(data []byte, model string) (out []byte, ok bool) {
	members, err := parseObject(bytes.TrimSpace(data))
	if err != nil {
		return data, false
	}
	var kind string
	if v, found := find(members, "type"); found {
		json.Unmarshal(v, &kind)
	}

	switch kind {
	case "message":
		if _, found := find(members, "model"); !found {
			return data, false
		}
		return encodeObject(set(members, "model", encode(model))), true
	case "message_start":
		inner, found := find(members, "message")
		if !found {
			return data, false
		}
		rewritten, ok := SetResponseModel(inner, model)
		if !ok {
			return data, false
		}
		return encodeObject(set(members, "message", rewritten)), true
	}
	return data, false
}
