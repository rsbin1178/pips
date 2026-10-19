//nolint:wsl_v5 // The selection editor keeps source scanning, edits, and validation adjacent.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const (
	selectionProvidersTable = "providers"
	selectionModelsTable    = "models"
)

var (
	// ErrSelectionEditUnsupported means the active config's source shape cannot
	// be edited without risking an unrelated table or value.
	ErrSelectionEditUnsupported = errors.New("coding config: unsupported model selection edit shape")
	// ErrSelectionDurability means the replacement committed but the parent
	// directory could not be synchronized.
	ErrSelectionDurability = errors.New("coding config: model selection commit durability uncertain")
	// ErrSelectionConflict aliases the shared revision conflict for selection
	// callers.
	ErrSelectionConflict = ErrConflict
	// ErrSelectionUnsafe aliases the shared unsafe-target error for selection
	// callers.
	ErrSelectionUnsafe = ErrUnsafe
)

// SelectionPreference is the durable form of a model-picker selection.
type SelectionPreference struct {
	Model     ModelRef
	Variant   string
	Reasoning *ReasoningLevel
}

// SaveSelectionWithOptions persists a complete model/variant/reasoning
// selection to the active config file. The selected model becomes the only
// model with default = true; variant and reasoning are written as top-level
// keys. Empty variant/reasoning values remove their keys.
func SaveSelectionWithOptions(path string, selection SelectionPreference, options TUIConfigSaveOptions) error {
	if err := validateSelectionPreference(selection); err != nil {
		return err
	}

	absolute, err := normalizeThemePath(path)
	if err != nil {
		return err
	}
	unlock := globalThemePathLocks.lock(absolute)
	defer unlock()

	return saveSelectionLocked(absolute, selection, options)
}

func validateSelectionPreference(selection SelectionPreference) error {
	if _, err := ParseModelRef(selection.Model.String()); err != nil {
		return err
	}
	if selection.Variant != "" {
		if _, err := ParseVariant(selection.Variant); err != nil {
			return err
		}
	}
	if selection.Reasoning != nil {
		if _, err := ParseReasoningLevel(string(*selection.Reasoning)); err != nil {
			return err
		}
	}

	return nil
}

func saveSelectionLocked(
	path string,
	selection SelectionPreference,
	options TUIConfigSaveOptions,
) error {
	parent := filepath.Dir(path)
	parentIdentity, err := ensureThemeParent(parent, options.AllowCreate)
	if err != nil {
		return err
	}

	revision, err := readThemeRevision(path, parentIdentity, options.AllowCreate)
	if err != nil {
		return err
	}

	var edited []byte
	if revision.exists {
		edited, err = editSelectionSource(revision.data, selection)
	} else {
		edited = selectionSource(selection)
	}
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrSelectionEditUnsupported, path, err)
	}
	if err := validateSelectionConfig(path, edited, selection); err != nil {
		return err
	}

	return atomicallyReplaceConfig(path, parent, revision, edited, ErrSelectionDurability)
}

func selectionSource(selection SelectionPreference) []byte {
	var builder strings.Builder
	if selection.Variant != "" {
		builder.WriteString("variant = " + strconv.Quote(selection.Variant) + "\n")
	}
	if selection.Reasoning != nil && *selection.Reasoning != "" {
		builder.WriteString("reasoning = " + strconv.Quote(string(*selection.Reasoning)) + "\n")
	}
	if builder.Len() > 0 {
		builder.WriteByte('\n')
	}
	builder.WriteString(modelTableHeader(selection.Model))
	builder.WriteString("\ndefault = true\n")

	return []byte(builder.String())
}

func modelTableHeader(ref ModelRef) string {
	return "[providers." + tomlKey(string(ref.Provider)) + ".models." + tomlKey(ref.Model) + "]"
}

func tomlKey(value string) string {
	if isTOMLBareKey(value) {
		return value
	}

	return quoteTOMLKey(value)
}

func isTOMLBareKey(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		switch {
		case char >= 'A' && char <= 'Z',
			char >= 'a' && char <= 'z',
			char >= '0' && char <= '9',
			char == '_', char == '-':
		default:
			return false
		}
	}

	return true
}

func quoteTOMLKey(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\b':
			builder.WriteString(`\b`)
		case '\t':
			builder.WriteString(`\t`)
		case '\n':
			builder.WriteString(`\n`)
		case '\f':
			builder.WriteString(`\f`)
		case '\r':
			builder.WriteString(`\r`)
		default:
			if char < 0x20 || char == 0x7f {
				fmt.Fprintf(&builder, `\u%04X`, char)

				continue
			}
			builder.WriteRune(char)
		}
	}
	builder.WriteByte('"')

	return builder.String()
}

type selectionScalar struct {
	lineStart  int
	lineEnd    int
	valueStart int
	valueEnd   int
}

type selectionModelKey struct {
	provider string
	model    string
}

type selectionModelTable struct {
	headerEnd     int
	hasDefault    bool
	defaultValue  bool
	defaultSource selectionScalar
}

type selectionSourceIndex struct {
	scalars          map[string]selectionScalar
	modelTables      map[selectionModelKey]selectionModelTable
	topLevelInsertAt int
	firstTableStart  int
}

//nolint:gocyclo // Every accepted source shape and its edit is deliberately explicit.
func editSelectionSource(data []byte, selection SelectionPreference) ([]byte, error) {
	index, err := indexSelectionSource(data)
	if err != nil {
		return nil, err
	}

	edits := make([]selectionEdit, 0, 8)
	inserted := make([]string, 0, 2)

	for _, field := range []struct {
		name string
		want string
	}{
		{name: "variant", want: selection.Variant},
		{name: "reasoning", want: reasoningValue(selection.Reasoning)},
	} {
		scalar, present := index.scalars[field.name]
		if field.want == "" {
			if present {
				edits = append(edits, removeTopLevelScalar(data, scalar))
			}

			continue
		}
		quoted := strconv.Quote(field.want)
		if present {
			edits = append(edits, selectionEdit{
				start: scalar.valueStart, end: scalar.valueEnd, text: quoted,
			})

			continue
		}
		inserted = append(inserted, field.name+" = "+quoted)
	}

	selectedKey := selectionModelKey{
		provider: string(selection.Model.Provider),
		model:    selection.Model.Model,
	}
	selectedTable, hasSelectedTable := index.modelTables[selectedKey]
	appendSelectedTable := !hasSelectedTable

	if hasSelectedTable {
		switch {
		case selectedTable.hasDefault && !selectedTable.defaultValue:
			edits = append(edits, selectionEdit{
				start: selectedTable.defaultSource.valueStart,
				end:   selectedTable.defaultSource.valueEnd,
				text:  "true",
			})
		case !selectedTable.hasDefault:
			insertAt := selectedTable.headerEnd
			text := "default = true\n"
			if insertAt > 0 && data[insertAt-1] != '\n' {
				text = "\n" + text
			}
			edits = append(edits, selectionEdit{start: insertAt, end: insertAt, text: text})
		}
	}

	for key, table := range index.modelTables {
		if key == selectedKey || !table.hasDefault || !table.defaultValue {
			continue
		}
		edits = append(edits, selectionEdit{
			start: table.defaultSource.valueStart,
			end:   table.defaultSource.valueEnd,
			text:  "false",
		})
	}

	topLevelInsertAt := index.topLevelInsertAt
	if len(inserted) > 0 && topLevelInsertAt == len(data) && appendSelectedTable {
		inserted = append(inserted, "", modelTableHeader(selection.Model), "default = true")
		appendSelectedTable = false
	}

	if len(inserted) > 0 {
		text := strings.Join(inserted, "\n") + "\n"
		if topLevelInsertAt > 0 && data[topLevelInsertAt-1] != '\n' {
			text = "\n" + text
		}
		edits = append(edits, selectionEdit{start: topLevelInsertAt, end: topLevelInsertAt, text: text})
	}

	if appendSelectedTable {
		text := appendModelTable(data, selection.Model)
		edits = append(edits, selectionEdit{start: len(data), end: len(data), text: text})
	}

	return applySelectionEdits(data, edits)
}

func reasoningValue(level *ReasoningLevel) string {
	if level == nil {
		return ""
	}

	return string(*level)
}

func appendModelTable(data []byte, ref ModelRef) string {
	var builder strings.Builder
	if len(data) > 0 && data[len(data)-1] != '\n' {
		builder.WriteByte('\n')
	}
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n\n")) {
		builder.WriteByte('\n')
	}
	builder.WriteString(modelTableHeader(ref))
	builder.WriteString("\ndefault = true\n")

	return builder.String()
}

type selectionEdit struct {
	start int
	end   int
	text  string
}

func removeTopLevelScalar(data []byte, scalar selectionScalar) selectionEdit {
	tail := data[scalar.valueEnd:scalar.lineEnd]
	if comment := bytes.IndexByte(tail, '#'); comment >= 0 {
		return selectionEdit{start: scalar.lineStart, end: scalar.valueEnd + comment, text: ""}
	}

	return selectionEdit{start: scalar.lineStart, end: scalar.lineEnd, text: ""}
}

func applySelectionEdits(data []byte, edits []selectionEdit) ([]byte, error) {
	sort.SliceStable(edits, func(left, right int) bool {
		if edits[left].start == edits[right].start {
			return edits[left].end < edits[right].end
		}

		return edits[left].start < edits[right].start
	})

	previousEnd := 0
	for _, edit := range edits {
		if edit.start < previousEnd || edit.start < 0 || edit.end < edit.start || edit.end > len(data) {
			return nil, errors.New("overlapping or out-of-range selection edits")
		}
		previousEnd = edit.end
	}

	result := slices.Clone(data)
	for _, edit := range slices.Backward(edits) {
		replacement := []byte(edit.text)
		next := make([]byte, 0, len(result)-(edit.end-edit.start)+len(replacement))
		next = append(next, result[:edit.start]...)
		next = append(next, replacement...)
		next = append(next, result[edit.end:]...)
		result = next
	}

	return result, nil
}

//nolint:gocyclo,nestif // The scanner is intentionally explicit about every accepted source shape.
func indexSelectionSource(data []byte) (selectionSourceIndex, error) {
	if bytes.Contains(data, []byte(`"""`)) || bytes.Contains(data, []byte(`'''`)) {
		return selectionSourceIndex{}, errors.New("multiline TOML strings are not safely editable")
	}

	index := selectionSourceIndex{
		scalars:         make(map[string]selectionScalar),
		modelTables:     make(map[selectionModelKey]selectionModelTable),
		firstTableStart: -1,
	}
	lines := sourceLines(data)
	var activePath []string
	activeArray := false

	for lineIndex, line := range lines {
		lineEnd := len(data)
		if lineIndex+1 < len(lines) {
			lineEnd = lines[lineIndex+1].start
		}
		trimmed := strings.TrimSpace(line.text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			if index.firstTableStart < 0 {
				index.topLevelInsertAt = lineEnd
			}

			continue
		}

		if name, array, ok := parseThemeTableHeader(trimmed); ok {
			path, err := parseTOMLKeyPath(name)
			if err != nil {
				return selectionSourceIndex{}, fmt.Errorf("table header: %w", err)
			}
			if index.firstTableStart < 0 {
				index.firstTableStart = line.start
			}
			if array && len(path) > 0 && path[0] == selectionProvidersTable {
				return selectionSourceIndex{}, errors.New("array tables under providers are not supported")
			}
			activePath = path
			activeArray = array
			if !array && isModelTablePath(path) {
				key := selectionModelKey{provider: path[1], model: path[3]}
				if _, exists := index.modelTables[key]; exists {
					return selectionSourceIndex{}, fmt.Errorf("model table %q/%q is repeated", path[1], path[3])
				}
				index.modelTables[key] = selectionModelTable{
					headerEnd: lineEnd,
				}
			}

			continue
		}

		key, ok := themeAssignmentKey(trimmed)
		if !ok {
			continue
		}
		keyPath, err := parseTOMLKeyPath(key)
		if err != nil {
			return selectionSourceIndex{}, fmt.Errorf("assignment key: %w", err)
		}
		if len(activePath) == 0 {
			if len(keyPath) > 0 && keyPath[0] == selectionProvidersTable {
				return selectionSourceIndex{}, errors.New("dotted providers assignments are not supported")
			}
			if len(keyPath) != 1 {
				continue
			}
			switch keyPath[0] {
			case "variant", "reasoning":
				valueStart, valueEnd, rangeErr := themeValueRange(line.text, line.start)
				if rangeErr != nil {
					return selectionSourceIndex{}, rangeErr
				}
				if _, exists := index.scalars[keyPath[0]]; exists {
					return selectionSourceIndex{}, fmt.Errorf("%s is repeated", keyPath[0])
				}
				index.scalars[keyPath[0]] = selectionScalar{
					lineStart: line.start, lineEnd: lineEnd,
					valueStart: valueStart, valueEnd: valueEnd,
				}
				index.topLevelInsertAt = lineEnd
			}

			continue
		}
		if activeArray {
			continue
		}
		if len(activePath) == 2 && activePath[0] == selectionProvidersTable &&
			len(keyPath) == 1 && keyPath[0] == selectionModelsTable {
			return selectionSourceIndex{}, errors.New("inline provider model maps are not supported")
		}
		if len(activePath) == 3 && activePath[0] == selectionProvidersTable && activePath[2] == selectionModelsTable {
			return selectionSourceIndex{}, errors.New("inline model definitions are not supported")
		}
		if !isModelTablePath(activePath) {
			continue
		}
		if len(keyPath) != 1 || keyPath[0] != "default" {
			continue
		}
		table := index.modelTables[selectionModelKey{provider: activePath[1], model: activePath[3]}]
		if table.hasDefault {
			return selectionSourceIndex{}, errors.New("default is repeated")
		}
		valueStart, valueEnd, value, rangeErr := boolValueRange(line.text, line.start)
		if rangeErr != nil {
			return selectionSourceIndex{}, rangeErr
		}
		table.hasDefault = true
		table.defaultValue = value
		table.defaultSource = selectionScalar{
			lineStart: line.start, lineEnd: lineEnd,
			valueStart: valueStart, valueEnd: valueEnd,
		}
		index.modelTables[selectionModelKey{provider: activePath[1], model: activePath[3]}] = table
	}

	return index, nil
}

func isModelTablePath(path []string) bool {
	return len(path) == 4 && path[0] == selectionProvidersTable && path[2] == selectionModelsTable
}

//nolint:gocyclo // TOML key quoting is parsed one segment at a time.
func parseTOMLKeyPath(value string) ([]string, error) {
	segments := make([]string, 0, 4)
	index := 0
	for {
		for index < len(value) && (value[index] == ' ' || value[index] == '\t') {
			index++
		}
		if index >= len(value) {
			return nil, errors.New("empty key segment")
		}

		var segment string
		switch value[index] {
		case '"':
			end := index + 1
			for ; end < len(value); end++ {
				if value[end] == '\\' {
					end++

					continue
				}
				if value[end] == '"' {
					break
				}
			}
			if end >= len(value) {
				return nil, errors.New("unterminated quoted key")
			}
			unquoted, err := strconv.Unquote(value[index : end+1])
			if err != nil {
				return nil, fmt.Errorf("invalid quoted key: %w", err)
			}
			segment = unquoted
			index = end + 1
		case '\'':
			end := strings.IndexByte(value[index+1:], '\'')
			if end < 0 {
				return nil, errors.New("unterminated literal key")
			}
			segment = value[index+1 : index+1+end]
			index += end + 2
		default:
			start := index
			for index < len(value) && isBareTOMLKeyByte(value[index]) {
				index++
			}
			if start == index {
				return nil, errors.New("invalid key segment")
			}
			segment = value[start:index]
		}
		segments = append(segments, segment)

		for index < len(value) && (value[index] == ' ' || value[index] == '\t') {
			index++
		}
		if index == len(value) {
			return segments, nil
		}
		if value[index] != '.' {
			return nil, errors.New("expected a dotted key separator")
		}
		index++
	}
}

func isBareTOMLKeyByte(value byte) bool {
	return value >= 'A' && value <= 'Z' ||
		value >= 'a' && value <= 'z' ||
		value >= '0' && value <= '9' ||
		value == '_' || value == '-'
}

func boolValueRange(line string, lineStart int) (int, int, bool, error) {
	equal := strings.IndexByte(line, '=')
	if equal < 0 {
		return 0, 0, false, errors.New("missing default assignment")
	}
	valueStart := equal + 1
	for valueStart < len(line) && (line[valueStart] == ' ' || line[valueStart] == '\t') {
		valueStart++
	}
	valueEnd := valueStart
	var parsed bool
	switch {
	case strings.HasPrefix(line[valueStart:], "true"):
		parsed = true
		valueEnd += len("true")
	case strings.HasPrefix(line[valueStart:], "false"):
		parsed = false
		valueEnd += len("false")
	default:
		return 0, 0, false, errors.New("default must be an ordinary boolean")
	}
	rest := strings.TrimSpace(line[valueEnd:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return 0, 0, false, errors.New("default has an unsupported trailing expression")
	}

	return lineStart + valueStart, lineStart + valueEnd, parsed, nil
}

func validateSelectionConfig(path string, data []byte, selection SelectionPreference) error {
	layer, err := decodeFile(path, data)
	if err != nil {
		return err
	}
	if layer.patch.Model == nil || *layer.patch.Model != selection.Model {
		return fmt.Errorf("%w: saved model selection does not resolve to %q", ErrInvalid, selection.Model)
	}
	if !equalOptionalString(layer.patch.Variant, selection.Variant) {
		return fmt.Errorf("%w: saved variant does not resolve to %q", ErrInvalid, selection.Variant)
	}
	if !equalOptionalReasoning(layer.patch.Reasoning, selection.Reasoning) {
		return fmt.Errorf("%w: saved reasoning does not resolve to %q", ErrInvalid, reasoningValue(selection.Reasoning))
	}

	var model *ModelConfig
	for index := range layer.models {
		if layer.models[index].Ref == selection.Model {
			model = &layer.models[index]

			break
		}
	}
	if model == nil {
		return fmt.Errorf("%w: saved model %q is missing", ErrInvalid, selection.Model)
	}
	if selection.Variant != "" {
		if _, ok := model.Variants[selection.Variant]; !ok {
			return fmt.Errorf(
				"%w: model %q has no variant %q", ErrInvalid, selection.Model, selection.Variant,
			)
		}
	}
	if selection.Reasoning != nil && !slices.Contains(model.ReasoningLevels, *selection.Reasoning) {
		return fmt.Errorf(
			"%w: model %q does not support reasoning level %q",
			ErrInvalid, selection.Model, *selection.Reasoning,
		)
	}

	return nil
}

func equalOptionalString(value *string, want string) bool {
	if value == nil {
		return want == ""
	}

	return *value == want
}

func equalOptionalReasoning(value, want *ReasoningLevel) bool {
	if value == nil || want == nil {
		return value == nil && want == nil
	}

	return *value == *want
}
