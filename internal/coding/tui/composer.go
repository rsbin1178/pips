package tui

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/attachment"
)

const (
	maximumComposerElements       = 16
	maximumComposerHistoryEntries = 64
	maximumComposerHistoryBytes   = 16 << 20
	largePasteCharacterThreshold  = 1_000
	largePasteLineThreshold       = 8
)

var (
	errComposerElementLimit = errors.New("coding tui: composer attachment limit reached")
	errComposerInvalidPaste = errors.New("coding tui: pasted text is invalid or too large")
	errComposerCorruptDraft = errors.New("coding tui: composer protected element is invalid")
)

type composerElementKind uint8

const (
	composerElementUnknown composerElementKind = iota
	composerElementPaste
	composerElementFile
)

type composerElement struct {
	id      uint64
	kind    composerElementKind
	label   string
	payload string
	file    attachment.Reference
}

type composerPosition struct {
	line   int
	column int
}

type composerSnapshot struct {
	display  string
	position composerPosition
	elements []composerElement
}

func (s composerSnapshot) clone() composerSnapshot {
	s.elements = slices.Clone(s.elements)

	return s
}

func plainComposerSnapshot(value string) composerSnapshot {
	line, column := composerPositionAtByte(value, len(value))

	return composerSnapshot{
		display: value,
		position: composerPosition{
			line:   line,
			column: column,
		},
	}
}

type composerState struct {
	textarea.Model

	elements []composerElement
	nextID   uint64

	history           []composerSnapshot
	historyBytes      int
	historyIndex      int
	historyLive       composerSnapshot
	isBrowsingHistory bool
}

func newComposerState(editor textarea.Model) composerState {
	return composerState{Model: editor}
}

func (c *composerState) SetValue(value string) {
	c.Model.SetValue(value)
	c.elements = nil
	c.exitHistoryBrowse()
}

func (c *composerState) Reset() {
	c.Model.Reset()
	c.elements = nil
	c.exitHistoryBrowse()
}

func (c *composerState) InsertString(value string) {
	before := c.Snapshot()
	c.snapCursorToElementEdge()
	c.Model.InsertString(value)
	if !c.hasValidElements() {
		c.applySnapshot(before)

		return
	}
	c.exitHistoryOnChange(before.display)
}

func (c composerState) Update(message tea.Msg) (composerState, tea.Cmd) {
	before := c.Snapshot()
	if key, ok := message.(tea.KeyPressMsg); ok {
		if c.removeElementForKey(key) {
			c.exitHistoryOnChange(before.display)

			return c, nil
		}
		c.snapCursorToElementEdge()
	}

	var command tea.Cmd
	c.Model, command = c.Model.Update(message)
	if !c.hasValidElements() {
		c.applySnapshot(before)

		return c, nil
	}
	if key, ok := message.(tea.KeyPressMsg); ok {
		c.snapCursorAfterNavigation(key)
	}
	c.exitHistoryOnChange(before.display)

	return c, command
}

func (c *composerState) Snapshot() composerSnapshot {
	return composerSnapshot{
		display: c.Value(),
		position: composerPosition{
			line:   c.Line(),
			column: c.Column(),
		},
		elements: slices.Clone(c.elements),
	}
}

func (c *composerState) Restore(snapshot composerSnapshot) error {
	if !validComposerSnapshot(snapshot) {
		return errComposerCorruptDraft
	}

	c.applySnapshot(snapshot)
	c.exitHistoryBrowse()

	return nil
}

func (c *composerState) setDisplayPreservingElements(value string) error {
	before := c.Snapshot()
	c.Model.SetValue(value)
	if !c.hasValidElements() {
		c.applySnapshot(before)

		return errComposerCorruptDraft
	}
	c.exitHistoryOnChange(before.display)

	return nil
}

func (c *composerState) InsertPaste(content string) (bool, error) {
	if err := validatePastedText(content); err != nil {
		return false, err
	}

	c.snapCursorToElementEdge()
	characters := utf8.RuneCountInString(content)
	lines := logicalLineCount(content)
	if characters <= largePasteCharacterThreshold && lines <= largePasteLineThreshold {
		before := c.Snapshot()
		cursor := composerCursorByte(c.Value(), c.Line(), c.Column())
		expected := c.Value()[:cursor] + content + c.Value()[cursor:]
		c.Model.InsertString(content)
		if c.Value() != expected || !c.hasValidElements() {
			c.applySnapshot(before)

			return false, errComposerCorruptDraft
		}
		c.exitHistoryOnChange(before.display)

		return false, nil
	}
	if len(c.elements) >= maximumComposerElements {
		return false, errComposerElementLimit
	}

	before := c.Snapshot()
	var label string
	for {
		c.nextID++
		label = fmt.Sprintf(
			"[Pasted text #%d · %d chars · %d lines]",
			c.nextID,
			characters,
			lines,
		)
		if !strings.Contains(c.Value(), label) {
			break
		}
	}
	c.Model.InsertString(label)
	c.elements = append(c.elements, composerElement{
		id:      c.nextID,
		kind:    composerElementPaste,
		label:   label,
		payload: content,
	})
	if !c.hasValidElements() {
		c.applySnapshot(before)

		return false, errComposerCorruptDraft
	}
	c.exitHistoryOnChange(before.display)

	return true, nil
}

func (c *composerState) InsertFile(
	start int,
	end int,
	reference attachment.Reference,
) error {
	if len(c.elements) >= maximumComposerElements {
		return errComposerElementLimit
	}

	normalized, err := attachment.NormalizeReference(reference)
	if err != nil {
		return fmt.Errorf("coding tui: insert Workspace file: %w", err)
	}

	before := c.Snapshot()
	if !composerRangeAvailable(before, start, end) {
		return errComposerCorruptDraft
	}

	label := c.nextFileLabel(before.display, normalized.Path)

	updated := before.display[:start] + label + before.display[end:]
	c.Model.SetValue(updated)
	c.elements = append(c.elements, composerElement{
		id:    c.nextID,
		kind:  composerElementFile,
		label: label,
		file:  normalized,
	})
	line, column := composerPositionAtByte(updated, start+len(label))
	setComposerPosition(c, line, column)

	if !c.hasValidElements() {
		c.applySnapshot(before)

		return errComposerCorruptDraft
	}

	c.exitHistoryOnChange(before.display)

	return nil
}

func composerRangeAvailable(snapshot composerSnapshot, start, end int) bool {
	if start < 0 || end < start || end > len(snapshot.display) ||
		!utf8.ValidString(snapshot.display[:start]) || !utf8.ValidString(snapshot.display[end:]) {
		return false
	}

	for _, span := range composerElementSpans(snapshot) {
		if start < span.end && end > span.start {
			return false
		}
	}

	return true
}

func (c *composerState) nextFileLabel(display, name string) string {
	for {
		c.nextID++

		label := fmt.Sprintf("[File #%d · %s]", c.nextID, name)
		if !strings.Contains(display, label) {
			return label
		}
	}
}

func (c *composerState) Assemble() (string, error) {
	return assembleComposerSnapshot(c.Snapshot())
}

func (c *composerState) RecordHistory(snapshot composerSnapshot) error {
	if !validComposerSnapshot(snapshot) {
		return errComposerCorruptDraft
	}

	if !composerSnapshotHasFiles(snapshot) {
		if _, err := assembleComposerSnapshot(snapshot); err != nil {
			return err
		}
	}
	if len(c.history) > 0 && equalComposerStructure(c.history[len(c.history)-1], snapshot) {
		return nil
	}

	entry := snapshot.clone()
	c.history = append(c.history, entry)
	c.historyBytes += composerSnapshotBytes(entry)
	for len(c.history) > maximumComposerHistoryEntries ||
		c.historyBytes > maximumComposerHistoryBytes {
		c.historyBytes -= composerSnapshotBytes(c.history[0])
		c.history[0] = composerSnapshot{}
		c.history = c.history[1:]
	}
	c.exitHistoryBrowse()

	return nil
}

func (c *composerState) HistoryUp() bool {
	if len(c.history) == 0 {
		return false
	}
	if !c.isBrowsingHistory {
		c.historyLive = c.Snapshot().clone()
		c.historyIndex = len(c.history) - 1
		c.isBrowsingHistory = true
	} else if c.historyIndex > 0 {
		c.historyIndex--
	} else {
		return false
	}

	c.applySnapshot(c.history[c.historyIndex])

	return true
}

func (c *composerState) HistoryDown() bool {
	if !c.isBrowsingHistory {
		return false
	}
	if c.historyIndex < len(c.history)-1 {
		c.historyIndex++
		c.applySnapshot(c.history[c.historyIndex])

		return true
	}

	c.applySnapshot(c.historyLive)
	c.exitHistoryBrowse()

	return true
}

func (c *composerState) AtFirstVisualRow() bool {
	line := c.LineInfo()

	return c.Line() == 0 && line.RowOffset == 0
}

func (c *composerState) AtLastVisualRow() bool {
	line := c.LineInfo()

	return c.Line() == c.LineCount()-1 && line.RowOffset >= max(0, line.Height-1)
}

func (c *composerState) applySnapshot(snapshot composerSnapshot) {
	c.Model.SetValue(snapshot.display)
	c.elements = slices.Clone(snapshot.elements)
	setComposerPosition(c, snapshot.position.line, snapshot.position.column)
}

func (c *composerState) exitHistoryOnChange(previous string) {
	if previous != c.Value() {
		c.exitHistoryBrowse()
	}
}

func (c *composerState) exitHistoryBrowse() {
	c.historyIndex = 0
	c.historyLive = composerSnapshot{}
	c.isBrowsingHistory = false
}

func (c *composerState) hasValidElements() bool {
	return validComposerSnapshot(c.Snapshot())
}

func (c *composerState) removeElementForKey(key tea.KeyPressMsg) bool {
	if key.Code != tea.KeyBackspace && key.Code != tea.KeyDelete {
		return false
	}

	cursor := composerCursorByte(c.Value(), c.Line(), c.Column())
	for _, span := range composerElementSpans(c.Snapshot()) {
		touches := key.Code == tea.KeyBackspace && cursor > span.start && cursor <= span.end
		touches = touches || key.Code == tea.KeyDelete && cursor >= span.start && cursor < span.end
		if !touches {
			continue
		}

		value := c.Value()[:span.start] + c.Value()[span.end:]
		c.elements = slices.Delete(c.elements, span.elementIndex, span.elementIndex+1)
		c.Model.SetValue(value)
		line, column := composerPositionAtByte(value, span.start)
		setComposerPosition(c, line, column)

		return true
	}

	return false
}

func (c *composerState) snapCursorToElementEdge() {
	cursor := composerCursorByte(c.Value(), c.Line(), c.Column())
	for _, span := range composerElementSpans(c.Snapshot()) {
		if cursor <= span.start || cursor >= span.end {
			continue
		}

		target := span.start
		if cursor-span.start > span.end-cursor {
			target = span.end
		}
		line, column := composerPositionAtByte(c.Value(), target)
		setComposerPosition(c, line, column)

		return
	}
}

func (c *composerState) snapCursorAfterNavigation(key tea.KeyPressMsg) {
	cursor := composerCursorByte(c.Value(), c.Line(), c.Column())
	for _, span := range composerElementSpans(c.Snapshot()) {
		if cursor <= span.start || cursor >= span.end {
			continue
		}

		target := span.start
		if key.Code == tea.KeyRight || key.Code == tea.KeyDown {
			target = span.end
		}
		line, column := composerPositionAtByte(c.Value(), target)
		setComposerPosition(c, line, column)

		return
	}
}

type composerElementSpan struct {
	start        int
	end          int
	elementIndex int
}

func composerElementSpans(snapshot composerSnapshot) []composerElementSpan {
	spans := make([]composerElementSpan, 0, len(snapshot.elements))
	for index, element := range snapshot.elements {
		start := strings.Index(snapshot.display, element.label)
		if start < 0 {
			continue
		}
		spans = append(spans, composerElementSpan{
			start:        start,
			end:          start + len(element.label),
			elementIndex: index,
		})
	}
	sort.Slice(spans, func(i, j int) bool {
		return spans[i].start < spans[j].start
	})

	return spans
}

func validComposerSnapshot(snapshot composerSnapshot) bool {
	if len(snapshot.elements) > maximumComposerElements || !utf8.ValidString(snapshot.display) {
		return false
	}

	// Payloads and file contents are validated at their insertion/resolution
	// boundaries. Do not rescan retained large content on every textarea update.
	labels := make(map[string]struct{}, len(snapshot.elements))
	for _, element := range snapshot.elements {
		if element.id == 0 || element.label == "" ||
			strings.Count(snapshot.display, element.label) != 1 {
			return false
		}

		switch element.kind {
		case composerElementPaste:
			if element.file != (attachment.Reference{}) {
				return false
			}
		case composerElementFile:
			normalized, err := attachment.NormalizeReference(element.file)
			if err != nil || normalized != element.file || element.payload != "" {
				return false
			}
		default:
			return false
		}
		if _, exists := labels[element.label]; exists {
			return false
		}
		labels[element.label] = struct{}{}
	}

	spans := composerElementSpans(snapshot)
	if len(spans) != len(snapshot.elements) {
		return false
	}
	for index := 1; index < len(spans); index++ {
		if spans[index].start < spans[index-1].end {
			return false
		}
	}

	return true
}

func assembleComposerSnapshot(snapshot composerSnapshot) (string, error) {
	if !validComposerSnapshot(snapshot) {
		return "", errComposerCorruptDraft
	}

	spans := composerElementSpans(snapshot)
	var builder strings.Builder
	builder.Grow(len(snapshot.display))
	last := 0
	for _, span := range spans {
		builder.WriteString(snapshot.display[last:span.start])
		element := snapshot.elements[span.elementIndex]
		switch element.kind {
		case composerElementPaste:
			builder.WriteString(element.payload)
		case composerElementFile:
			return "", errComposerCorruptDraft
		case composerElementUnknown:
			return "", errComposerCorruptDraft
		default:
			return "", errComposerCorruptDraft
		}
		last = span.end
	}
	builder.WriteString(snapshot.display[last:])
	assembled := builder.String()
	if err := coding.ValidatePromptText(assembled); err != nil {
		return "", fmt.Errorf("coding tui: assemble composer: %w", err)
	}

	return assembled, nil
}

func equalComposerStructure(left, right composerSnapshot) bool {
	if len(left.elements) != len(right.elements) {
		return false
	}
	for index := range left.elements {
		if left.elements[index].kind != right.elements[index].kind ||
			left.elements[index].payload != right.elements[index].payload ||
			left.elements[index].file != right.elements[index].file {
			return false
		}
	}

	return composerStructure(left) == composerStructure(right)
}

func composerStructure(snapshot composerSnapshot) string {
	spans := composerElementSpans(snapshot)
	var builder strings.Builder
	last := 0
	for index, span := range spans {
		builder.WriteString(snapshot.display[last:span.start])
		builder.WriteByte('\x00')
		builder.WriteString(strconv.Itoa(int(snapshot.elements[span.elementIndex].kind)))
		builder.WriteByte(':')
		builder.WriteString(strconv.Itoa(index))
		builder.WriteByte('\x00')
		last = span.end
	}
	builder.WriteString(snapshot.display[last:])

	return builder.String()
}

func composerSnapshotBytes(snapshot composerSnapshot) int {
	size := len(snapshot.display)
	for _, element := range snapshot.elements {
		size += len(element.label) + len(element.payload) + len(element.file.Path)
	}

	return size
}

func composerSnapshotHasFiles(snapshot composerSnapshot) bool {
	for _, element := range snapshot.elements {
		if element.kind == composerElementFile {
			return true
		}
	}

	return false
}

func validatePastedText(content string) error {
	if len(content) > coding.MaxPromptTextBytes || !utf8.ValidString(content) {
		return errComposerInvalidPaste
	}
	for _, character := range content {
		isAllowedControl := character == '\n' || character == '\r' || character == '\t'
		if character == '\x00' || unicode.IsControl(character) && !isAllowedControl {
			return errComposerInvalidPaste
		}
	}

	return nil
}

func logicalLineCount(content string) int {
	lines := 1
	var previous rune
	for _, character := range content {
		switch character {
		case '\n':
			lines++
		case '\r':
			lines++
		}
		if character == '\n' && previous == '\r' {
			lines--
		}
		previous = character
	}

	return lines
}
