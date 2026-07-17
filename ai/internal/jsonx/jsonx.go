// Package jsonx is the ai module's single JSON encode/decode seam. Every
// wire-format (de)serialization in providers and httpx goes through it, so a
// future switch to encoding/json/v2 or a faster codec is a one-file change.
package jsonx

import "encoding/json"

// Marshal encodes v as JSON.
func Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// Unmarshal decodes JSON data into v.
func Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
