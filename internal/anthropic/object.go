package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// member is one key of a JSON object with its value kept as the bytes it
// arrived in. Holding objects as ordered members, rather than decoding into
// a map or a struct, is what lets janus carry through fields it has never
// heard of, in their original order and spelling.
type member struct {
	key string
	val json.RawMessage
}

func parseObject(data []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("expected a JSON object")
	}
	var members []member
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("expected an object key, found %v", tok)
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		members = append(members, member{key, val})
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the JSON object")
	}
	return members, nil
}

func encodeObject(members []member) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(encode(m.key))
		b.WriteByte(':')
		b.Write(m.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func encodeArray(items []json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(item)
	}
	b.WriteByte(']')
	return b.Bytes()
}

// encode marshals a value janus itself produced. HTML escaping is off: this
// JSON goes to an API, not a browser, and "<" should stay "<".
func encode(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// Only strings, bools, and numbers are passed here.
		panic(err)
	}
	return bytes.TrimRight(b.Bytes(), "\n")
}

func find(members []member, key string) (json.RawMessage, bool) {
	for _, m := range members {
		if m.key == key {
			return m.val, true
		}
	}
	return nil, false
}

// set replaces key's value in place, keeping its position, or appends it if
// the object did not have one.
func set(members []member, key string, val json.RawMessage) []member {
	out := make([]member, len(members), len(members)+1)
	copy(out, members)
	for i := range out {
		if out[i].key == key {
			out[i].val = val
			return out
		}
	}
	return append(out, member{key, val})
}
