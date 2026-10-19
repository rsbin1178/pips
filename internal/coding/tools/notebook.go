package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// errInvalidNotebook marks a notebook file that is not a decodable nbformat v4
// document. It is a typed tool failure, not a crash: the model sees the normal
// text error envelope.
var errInvalidNotebook = errors.New("coding tools: invalid notebook")

// notebookDocument is the bounded nbformat v4 surface the read Tool renders.
// Only the fields that describe cells and outputs are decoded; every other
// notebook field is ignored so a future nbformat addition stays readable.
type notebookDocument struct {
	Nbformat int             `json:"nbformat"`
	Cells    *[]notebookCell `json:"cells"`
}

type notebookCell struct {
	CellType string           `json:"cell_type"`
	Source   json.RawMessage  `json:"source"`
	Outputs  []notebookOutput `json:"outputs"`
}

type notebookOutput struct {
	OutputType string                     `json:"output_type"`
	Text       json.RawMessage            `json:"text"`
	EName      string                     `json:"ename"`
	EValue     string                     `json:"evalue"`
	Data       map[string]json.RawMessage `json:"data"`
}

// renderNotebook parses one nbformat v4 document and renders bounded cell text.
// It never returns raw JSON: each cell becomes a cell-type line, its source
// lines, and one line per output MIME type. Image and other binary outputs are
// summarized by size instead of being echoed. The rendered lines pass through
// the same line-count and output-byte window as a text read, so truncation and
// next.offset behave identically.
func renderNotebook(
	ctx context.Context,
	name string,
	content []byte,
	offset int,
	limit int,
	limits Limits,
) (string, error) {
	var document notebookDocument

	if err := json.Unmarshal(content, &document); err != nil {
		return "", failure(readName, fmt.Errorf("%w: %q", errInvalidNotebook, name))
	}

	// The renderer is nbformat v4 only. A v3 or version-less document, and a
	// v4 document without a cells array, is not a notebook this tool can
	// render; returning an empty body would look like a successful read.
	if document.Nbformat != 4 || document.Cells == nil {
		return "", failure(readName, fmt.Errorf("%w: %q", errInvalidNotebook, name))
	}

	lines, err := notebookLines(document)
	if err != nil {
		return "", failure(readName, fmt.Errorf("%w: %q", errInvalidNotebook, name))
	}

	index := 0

	next := func() (string, bool) {
		if index >= len(lines) {
			return "", false
		}

		line := lines[index]
		index++

		return line, true
	}

	return writeNumberedLines(ctx, next, offset, limit, limits)
}

func notebookLines(document notebookDocument) ([]string, error) {
	var lines []string

	for _, cell := range *document.Cells {
		cellType := strings.TrimSpace(cell.CellType)
		if cellType == "" {
			cellType = "unknown"
		}

		lines = append(lines, cellType)

		source, err := notebookText(cell.Source)
		if err != nil {
			return nil, err
		}

		for _, line := range splitNotebookSource(source) {
			lines = append(lines, "  "+line)
		}

		outputs, err := notebookOutputLines(cell.Outputs)
		if err != nil {
			return nil, err
		}

		lines = append(lines, outputs...)
	}

	return lines, nil
}

func notebookOutputLines(outputs []notebookOutput) ([]string, error) {
	var lines []string

	for _, output := range outputs {
		switch output.OutputType {
		case "stream":
			text, err := notebookText(output.Text)
			if err != nil {
				return nil, err
			}

			lines = append(lines, notebookOutputLine("text/plain", notebookPreview(text)))
		case "execute_result", "display_data":
			mimeTypes := make([]string, 0, len(output.Data))
			for mimeType := range output.Data {
				mimeTypes = append(mimeTypes, mimeType)
			}

			sort.Slice(mimeTypes, func(left, right int) bool {
				leftText, rightText := isNotebookTextMIME(mimeTypes[left]), isNotebookTextMIME(mimeTypes[right])
				if leftText != rightText {
					return leftText
				}

				return mimeTypes[left] < mimeTypes[right]
			})

			for _, mimeType := range mimeTypes {
				line, err := notebookDataLine(mimeType, output.Data[mimeType])
				if err != nil {
					return nil, err
				}

				lines = append(lines, line)
			}
		case "error":
			lines = append(lines, "  output error: "+strings.TrimSpace(output.EName+": "+output.EValue))
		}
	}

	return lines, nil
}

func notebookOutputLine(mimeType, text string) string {
	return "  output " + mimeType + ": " + text
}

// notebookPreview collapses one output preview onto a single line so a
// multi-line text or stream output stays one numbered entry.
func notebookPreview(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func notebookDataLine(mimeType string, raw json.RawMessage) (string, error) {
	if isNotebookTextMIME(mimeType) {
		text, err := notebookText(raw)
		if err != nil {
			return "", err
		}

		return notebookOutputLine(mimeType, notebookPreview(text)), nil
	}

	size := notebookBinarySize(mimeType, raw)
	if strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		return fmt.Sprintf("  output %s: [image output %s, %d bytes]", mimeType, mimeType, size), nil
	}

	return fmt.Sprintf("  output %s: [%s output, %d bytes]", mimeType, mimeType, size), nil
}

func isNotebookTextMIME(mimeType string) bool {
	lower := strings.ToLower(mimeType)

	return strings.HasPrefix(lower, "text/") || lower == "application/json"
}

// notebookBinarySize reports the decoded byte count of one binary output
// payload. nbformat stores binary output as base64 text, so an image reports
// the size of its decoded bytes rather than the base64 spelling.
func notebookBinarySize(mimeType string, raw json.RawMessage) int {
	text, err := notebookText(raw)
	if err != nil {
		return len(raw)
	}

	if strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		if decoded, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(text)); decodeErr == nil {
			return len(decoded)
		}
	}

	return len(text)
}

// notebookText decodes an nbformat text field, which is either a string or a
// list of strings. A missing field is empty; any other shape is malformed.
func notebookText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}

	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single, nil
	}

	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err == nil {
		return strings.Join(multiple, ""), nil
	}

	return "", errInvalidNotebook
}

func splitNotebookSource(source string) []string {
	if source == "" {
		return nil
	}

	lines := strings.Split(source, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	return lines
}
