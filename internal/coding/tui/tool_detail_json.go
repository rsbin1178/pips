package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The detail view renders arguments and JSON results as an order-preserving
// field tree instead of an escaped JSON blob, so `&&`, `<` and newlines in
// model-supplied values stay readable and the model's own field order survives.

const (
	detailTreeMaxNodes        = 512
	detailTreeMaxDepth        = 10
	detailTreeInlineListLimit = 8
	detailTreeInlineScalar    = 60
	// detailTreeMaxInputBytes bounds the decoded document itself, so a huge
	// payload cannot retain memory the renderer will never show.
	detailTreeMaxInputBytes = 512 << 10
)

var (
	errJSONTreeStructure = errors.New("coding tui: invalid JSON structure")
	errJSONTreeKey       = errors.New("coding tui: invalid JSON object key")
)

type jsonTreeKind uint8

const (
	jsonTreeScalar jsonTreeKind = iota
	jsonTreeList
	jsonTreeObject
	jsonTreeOmitted
)

// jsonTreeValue is one decoded JSON value with object fields kept in document
// order. It carries no type information beyond what the renderer needs.
type jsonTreeValue struct {
	kind   jsonTreeKind
	scalar string
	list   []jsonTreeValue
	fields []jsonTreeField
}

type jsonTreeField struct {
	key   string
	value jsonTreeValue
}

// decodeJSONTree decodes a JSON value into a bounded ordered tree. Trailing
// content and malformed input are rejected so callers fall back to plain text;
// a document that runs past the node or depth budget still decodes, keeping
// the fields it reached and marking the rest with an omission.
func decodeJSONTree(data []byte) (jsonTreeValue, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || len(trimmed) > detailTreeMaxInputBytes {
		return jsonTreeValue{}, false
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()

	walker := jsonTreeWalker{decoder: decoder, remaining: detailTreeMaxNodes}

	value, err := walker.value(0)
	if err != nil {
		return jsonTreeValue{}, false
	}

	// A truncated walk stops reading mid-document, so the trailing check only
	// applies to a complete one.
	if !walker.truncated {
		if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
			return jsonTreeValue{}, false
		}
	}

	return value, true
}

type jsonTreeWalker struct {
	decoder   *json.Decoder
	remaining int
	truncated bool
}

func (walker *jsonTreeWalker) value(depth int) (jsonTreeValue, error) {
	if walker.truncated {
		return jsonTreeValue{kind: jsonTreeOmitted}, nil
	}

	token, err := walker.decoder.Token()
	if err != nil {
		return jsonTreeValue{}, err
	}

	switch typed := token.(type) {
	case json.Delim:
		return walker.delimited(typed, depth)
	case string:
		return jsonTreeValue{kind: jsonTreeScalar, scalar: sanitizeToolText(typed)}, nil
	case json.Number:
		return jsonTreeValue{kind: jsonTreeScalar, scalar: typed.String()}, nil
	case bool:
		return jsonTreeValue{kind: jsonTreeScalar, scalar: strconv.FormatBool(typed)}, nil
	case nil:
		return jsonTreeValue{kind: jsonTreeScalar, scalar: "null"}, nil
	default:
		return jsonTreeValue{}, errJSONTreeStructure
	}
}

func (walker *jsonTreeWalker) delimited(delim json.Delim, depth int) (jsonTreeValue, error) {
	switch delim {
	case '{':
		return walker.object(depth)
	case '[':
		return walker.list(depth)
	default:
		return jsonTreeValue{}, errJSONTreeStructure
	}
}

func (walker *jsonTreeWalker) object(depth int) (jsonTreeValue, error) {
	if depth >= detailTreeMaxDepth {
		return jsonTreeValue{kind: jsonTreeOmitted}, walker.skip()
	}

	value := jsonTreeValue{kind: jsonTreeObject}

	for !walker.truncated && walker.decoder.More() {
		field, err := walker.objectField(depth)
		if err != nil {
			return jsonTreeValue{}, err
		}

		value.fields = append(value.fields, field)
	}

	if !walker.truncated {
		if _, err := walker.decoder.Token(); err != nil {
			return jsonTreeValue{}, err
		}
	}

	return value, nil
}

// objectField reads one `"key": value` pair. A depleted node budget stops the
// document at that field with an explicit omission instead of discarding every
// earlier field.
func (walker *jsonTreeWalker) objectField(depth int) (jsonTreeField, error) {
	key, err := walker.objectKey()
	if err != nil {
		return jsonTreeField{}, err
	}

	if !walker.consume() {
		walker.truncated = true

		return jsonTreeField{key: key, value: jsonTreeValue{kind: jsonTreeOmitted}}, nil
	}

	child, err := walker.value(depth + 1)
	if err != nil {
		return jsonTreeField{}, err
	}

	return jsonTreeField{key: key, value: child}, nil
}

func (walker *jsonTreeWalker) list(depth int) (jsonTreeValue, error) {
	if depth >= detailTreeMaxDepth {
		return jsonTreeValue{kind: jsonTreeOmitted}, walker.skip()
	}

	value := jsonTreeValue{kind: jsonTreeList}

	for !walker.truncated && walker.decoder.More() {
		if !walker.consume() {
			value.list = append(value.list, jsonTreeValue{kind: jsonTreeOmitted})
			walker.truncated = true

			break
		}

		child, err := walker.value(depth + 1)
		if err != nil {
			return jsonTreeValue{}, err
		}

		value.list = append(value.list, child)
	}

	if !walker.truncated {
		if _, err := walker.decoder.Token(); err != nil {
			return jsonTreeValue{}, err
		}
	}

	return value, nil
}

func (walker *jsonTreeWalker) objectKey() (string, error) {
	token, err := walker.decoder.Token()
	if err != nil {
		return "", err
	}

	key, ok := token.(string)
	if !ok {
		return "", errJSONTreeKey
	}

	// Object keys come from a Tool payload, so they get the same ANSI/control
	// stripping as values and are flattened to one line.
	return oneLineToolText(key), nil
}

func (walker *jsonTreeWalker) consume() bool {
	walker.remaining--

	return walker.remaining > 0
}

// skip drains one complete nested value the renderer will not show. It stops
// with an omission marker when the node budget runs out mid-value.
func (walker *jsonTreeWalker) skip() error {
	depth := 1
	for depth > 0 {
		if !walker.consume() {
			walker.truncated = true

			return nil
		}

		token, err := walker.decoder.Token()
		if err != nil {
			return err
		}

		delim, ok := token.(json.Delim)
		if !ok {
			continue
		}

		switch delim {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		default:
			return errJSONTreeStructure
		}
	}

	return nil
}

// jsonSummaryLine renders a bounded single-line summary of a JSON value for
// compact transcript rows, for example `items: 2 items · total_count: 2`.
func jsonSummaryLine(value jsonTreeValue) string {
	switch value.kind {
	case jsonTreeScalar:
		return truncateText(oneLineJSONScalar(value.scalar), detailTreeInlineScalar)
	case jsonTreeList:
		return fmt.Sprintf("%d %s", len(value.list), pluralWord(len(value.list), "item", "items"))
	case jsonTreeObject:
		parts := make([]string, 0, len(value.fields))
		for _, field := range value.fields {
			if len(parts) == 3 {
				parts = append(parts, "…")

				break
			}

			parts = append(parts, field.key+": "+jsonSummaryValue(field.value))
		}

		return strings.Join(parts, " · ")
	case jsonTreeOmitted:
		return "…"
	default:
		return ""
	}
}

func jsonSummaryValue(value jsonTreeValue) string {
	switch value.kind {
	case jsonTreeScalar:
		return truncateText(oneLineJSONScalar(value.scalar), 40)
	case jsonTreeList:
		return fmt.Sprintf("%d %s", len(value.list), pluralWord(len(value.list), "item", "items"))
	case jsonTreeObject:
		return "{" + strconv.Itoa(len(value.fields)) + " fields}"
	case jsonTreeOmitted:
		return "…"
	default:
		return ""
	}
}

func oneLineJSONScalar(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func pluralWord(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}

	return plural
}
