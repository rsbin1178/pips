package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// validateCheckpointJSON keeps new checkpoint seeds canonical even though the
// general AI message decoder tolerates unknown wire fields. AI remains the sole
// owner of message decoding/encoding; comparing its canonical projection avoids
// maintaining a second role/part schema here.
func validateCheckpointJSON(data []byte, checkpoint ContextCheckpoint) error {
	raw, err := readUniqueCheckpointJSON(json.NewDecoder(bytes.NewReader(data)), 0)
	if err != nil {
		return err
	}

	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}

	var canonical map[string]any
	if err := json.Unmarshal(encoded, &canonical); err != nil {
		return err
	}

	object, ok := raw.(map[string]any)
	if !ok || !reflect.DeepEqual(object["seed_messages"], canonical["seed_messages"]) {
		return errors.New("harness: context checkpoint must use canonical AI message fields")
	}

	return nil
}

// readUniqueCheckpointJSON is restricted to the shallow versioned checkpoint
// envelope, not arbitrary legacy application data. Duplicate keys must not
// select a different source boundary or archive depending on the reader.
func readUniqueCheckpointJSON(decoder *json.Decoder, depth int) (any, error) {
	if depth > 32 {
		return nil, errors.New("harness: context checkpoint JSON is too deeply nested")
	}

	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}

	switch token {
	case json.Delim('{'):
		object := make(map[string]any)

		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}

			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("harness: invalid checkpoint object key")
			}

			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("harness: duplicate checkpoint field %q", key)
			}

			value, err := readUniqueCheckpointJSON(decoder, depth+1)
			if err != nil {
				return nil, err
			}

			object[key] = value
		}

		_, err = decoder.Token()

		return object, err
	case json.Delim('['):
		array := make([]any, 0)

		for decoder.More() {
			value, err := readUniqueCheckpointJSON(decoder, depth+1)
			if err != nil {
				return nil, err
			}

			array = append(array, value)
		}

		_, err = decoder.Token()

		return array, err
	default:
		return token, nil
	}
}
