//nolint:wsl_v5 // Row accounting and anchor resolution stay adjacent to their bounds checks.
package tui

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The transcript region keeps rendered rows for the conversation and a reading
// position over them. Rows are produced by the existing timeline renderers and
// reused per (width, theme, no-color), so a streaming delta re-renders only the
// record that is still growing.
//
// Residency is bounded. Every record keeps its identity, its row count and the
// renderer that reproduces its rows, but only the records the reading window
// touches keep rendered rows. Anything further away is released and rendered
// again on demand, which is what keeps a long conversation from pinning every
// row it ever produced.

// transcriptWindowMargin is how many rows beyond the visible window stay
// resident. It is a margin rather than a hard edge so a one-row scroll does not
// have to re-render the record that is about to enter the window.
const transcriptWindowMargin = 64

// maxResidentRowsPerRecord bounds one record's resident rows. A single long
// answer or tool output can be far taller than the window; keeping its whole
// rendering would bypass the residency bound. A window of rows around the
// reading position stays resident and the rest is rendered again on demand.
const maxResidentRowsPerRecord = 4096

// transcriptRecord is one settled or growing conversation entry.
type transcriptRecord struct {
	// id is the stable identity used for anchoring and row reuse. It is empty
	// only for entries the projection cannot name (for example a bare diagnostic).
	id string
	// lines is the record's row count at the store's current geometry. It stays
	// valid after the rows themselves are released.
	lines int
	// rows are the physical rows of this record, or empty once released. A live
	// record keeps its frozen prefix and its growing tail apart, so a frame hands
	// the store two slices instead of copying the whole live body's row headers.
	rows rowSegments
	// loaded reports whether rows holds the record's rendering. A record that
	// renders as nothing is loaded with no rows, which is not the same as having
	// been released.
	loaded bool
	// live marks a record that was rendered from a block that had not settled. Such
	// a record is never reused: a block can settle while keeping its identity, so
	// the next frame renders it again and drops this one. See [blockIsUnsettled].
	live bool
	// block is the projection this record renders, kept so the rows can be
	// reproduced after a release.
	block *timelineBlock
	// tool is the projection this record renders when it is a Tool entry, kept so
	// an addressed interaction can open that exact call's detail.
	tool *timelineBlock
	// prompt is the first line of a user turn's text, extracted once when the
	// record is rendered so the rail's preview never re-reads a prompt.
	prompt string
}

func (r *transcriptRecord) height() int {
	return r.lines
}

// hasLive reports whether the store still owns a record that may change without
// a settled rebuild. Its renderer keeps the corresponding live rows; once no
// such record remains they are stale and can be released.
func (s *transcriptStore) hasLive() bool {
	for index := range s.records {
		if s.records[index].live {
			return true
		}
	}

	return false
}

// release drops the rendered rows, keeping the row count and the projection so
// the reading position and every anchor still resolve and the rows can be
// rendered again.
func (r *transcriptRecord) release() {
	r.rows = rowSegments{}
	r.loaded = false
}

// transcriptStore owns the rendered rows, the row index and the residency
// window.
type transcriptStore struct {
	records []transcriptRecord
	// index maps a record identity to its position in records, so a rebuild
	// reuses the previous rendering instead of rendering it again. spareIndex is
	// cleared and swapped instead of reallocated each build.
	index      map[string]int
	spareIndex map[string]int
	// width, themeKey and noColor are the rendering key. Rows rendered at another
	// geometry cannot be reused.
	width    int
	themeKey themeFingerprint
	noColor  bool
	// theme and markdown are what a released record is rendered with again.
	theme    colorTheme
	markdown *markdownRenderer
	// leading is how many blank rows the region keeps above the records, for the
	// seam between terminal-native history and the mutable tail.
	leading int
	// starts[i] is records[i]'s first row in the region's coordinate space, which
	// includes leading. total is the row count below the leading rows.
	starts     []int
	total      int
	indexDirty bool
	// keepFrom and keepTo bound the rows that stay resident, and hasKeep reports
	// whether a window has been observed yet.
	keepFrom int
	keepTo   int
	hasKeep  bool
	// revision fingerprints everything a search scan reads.
	revision uint64
	// renders counts records re-rendered since the last reset, so tests can
	// assert that a stream delta does not re-render settled history.
	renders int
	// evictions counts records whose rows were released since the last reset.
	evictions int
	// liveRowsHashed counts the live record's rows read since the last reset, so
	// tests can assert that a frame's revision does not read them.
	liveRowsHashed int
	// materializations counts records whose rows were rendered (fresh or after a
	// release) since the last reset, and rowsMaterialized the rows they produced,
	// so a test can tell a windowed frame from a whole-document one.
	materializations int
	rowsMaterialized int
	// turns caches each prompt's start row and preview text, turnsRevision is the
	// revision it was computed for, and turnRebuilds counts rebuilds since the last
	// reset, so a test can assert that a frame which changed nothing re-reads no
	// record — the same way renders and evictions report the work a frame did.
	turns         []userTurn
	turnsRevision uint64
	turnsCached   bool
	turnRebuilds  int
}

// transcriptEntry is one projected entry offered to the store. A record that is
// reused from the previous build is never rendered again. live marks a block that
// has not settled, so the store renders it instead of serving a record an earlier
// frame produced.
type transcriptEntry struct {
	id    string
	live  bool
	block *timelineBlock
}

// resetRenders clears the render and eviction counters.
func (s *transcriptStore) resetRenders() {
	s.renders = 0
	s.evictions = 0
	s.materializations = 0
	s.rowsMaterialized = 0
	s.turnRebuilds = 0
}

// keyMatches reports whether the resident rows are still valid for this
// rendering configuration.
func (s *transcriptStore) keyMatches(width int, theme themeFingerprint, noColor bool) bool {
	return s.width == width && s.themeKey == theme && s.noColor == noColor
}

// setLeading changes the blank rows the region keeps above the records.
func (s *transcriptStore) setLeading(rows int) {
	rows = max(0, rows)
	if s.leading == rows {
		return
	}

	s.leading = rows
	s.indexDirty = true
}

// sync refreshes the record set from one frame's entries. stable is the number of
// leading entries whose records did not change: the store then keeps those records
// - and the rows they already rendered - and rebuilds only the tail. Any other
// combination falls back to a full build.
// reset drops every cached record. A wholesale transcript replacement can reuse
// a position-based identity with different content; clearing here makes the next
// sync render the replacement from scratch instead of keeping the old rows.
func (s *transcriptStore) reset() {
	*s = transcriptStore{}
}

func (s *transcriptStore) sync(
	width int,
	theme colorTheme,
	themeKey themeFingerprint,
	noColor bool,
	markdown *markdownRenderer,
	leading int,
	prefix, tail []transcriptEntry,
	stable int,
) {
	s.setLeading(leading)

	if stable <= 0 || stable != len(prefix) || stable > len(s.records) ||
		!s.keyMatches(width, themeKey, noColor) {
		entries := prefix
		if len(tail) > 0 {
			entries = make([]transcriptEntry, 0, len(prefix)+len(tail))
			entries = append(entries, prefix...)
			entries = append(entries, tail...)
		}

		s.build(width, theme, themeKey, noColor, markdown, entries)

		return
	}

	s.theme = theme
	s.markdown = markdown
	s.buildTail(stable, tail)
}

// uniqueRecordID appends an ordinal to an identity already taken in taken. Two
// unnamed projections can share a fallback identity (several diagnostics sit at
// the same position); the ordinal keeps their rows and anchors distinct, and is
// stable while the projection order is.
func uniqueRecordID(taken map[string]int, id string) string {
	if id == "" {
		return ""
	}

	unique := id
	for ordinal := 1; ; ordinal++ {
		if _, exists := taken[unique]; !exists {
			return unique
		}

		unique = id + "\x00" + strconv.Itoa(ordinal)
	}
}

// newRecord builds the record one entry contributes, rendering its rows once so
// the region knows its height.
func (s *transcriptStore) newRecord(id string, entry transcriptEntry) transcriptRecord {
	s.renders++

	record := transcriptRecord{id: id, live: entry.live, block: entry.block}
	if (entry.block.kind == blockTool || entry.block.kind == blockSubagent ||
		entry.block.kind == blockQuestion) && len(entry.block.tools) > 0 {
		record.tool = entry.block
	}
	if entry.block.kind == blockUser {
		record.prompt = promptLine(entry.block.body)
	}

	s.materialize(&record)

	return record
}

// buildTail keeps the first keep records and rebuilds the rest from tail. The
// caller has established that the resident rows are still valid for this
// rendering configuration and that the entries behind records[:keep] did not
// change.
func (s *transcriptStore) buildTail(keep int, tail []transcriptEntry) {
	for index := keep; index < len(s.records); index++ {
		delete(s.index, s.records[index].id)
	}

	if keep < len(s.records) {
		clear(s.records[keep:])
	}

	s.records = s.records[:keep]

	for _, entry := range tail {
		id := uniqueRecordID(s.index, entry.id)
		s.index[id] = len(s.records)
		s.records = append(s.records, s.newRecord(id, entry))
	}

	s.indexDirty = true
	s.reindex()
	s.updateRevision()
}

// build renders the offered entries, reusing the previous rendering of a settled
// record whose identity is unchanged. A change to the width, theme or color mode
// drops every reused row: rows rendered at another geometry cannot be reused.
func (s *transcriptStore) build(
	width int,
	theme colorTheme,
	themeKey themeFingerprint,
	noColor bool,
	markdown *markdownRenderer,
	entries []transcriptEntry,
) {
	if !s.keyMatches(width, themeKey, noColor) {
		s.records = nil
		s.index = nil
		s.spareIndex = nil

		s.width = width
		s.themeKey = themeKey
		s.noColor = noColor
	}

	s.theme = theme
	s.markdown = markdown

	// previous is what the last build produced and reuseIndex names its records,
	// so a settled entry whose identity is unchanged keeps its rows. next and the
	// scratch map are cleared and reused rather than allocated, so a frame's
	// bookkeeping does not grow with the conversation.
	previous := s.records
	reuseIndex := s.index
	next := s.spareIndex
	if next == nil {
		next = make(map[string]int, len(entries))
	} else {
		clear(next)
	}

	records := make([]transcriptRecord, 0, len(entries))

	for _, entry := range entries {
		id := uniqueRecordID(next, entry.id)
		// Reuse the previous rendering only when neither side is unsettled: a block
		// can settle while keeping its identity, and the record it leaves behind was
		// rendered from the block that frame has just replaced.
		if !entry.live && id != "" {
			if at, ok := reuseIndex[id]; ok && at < len(previous) && previous[at].id == id &&
				!previous[at].live {
				record := previous[at]
				record.live = false
				next[id] = len(records)
				records = append(records, record)

				continue
			}
		}

		next[id] = len(records)
		records = append(records, s.newRecord(id, entry))
	}

	s.records = records
	s.index = next
	s.spareIndex = reuseIndex
	s.indexDirty = true
	s.reindex()
	s.updateRevision()
}

// reindex rebuilds the row index from the records' row counts.
func (s *transcriptStore) reindex() {
	if !s.indexDirty {
		return
	}

	if cap(s.starts) < len(s.records) {
		s.starts = make([]int, len(s.records))
	} else {
		s.starts = s.starts[:len(s.records)]
	}

	position := s.leading

	for index := range s.records {
		if index > 0 {
			position++
		}

		s.starts[index] = position
		position += s.records[index].lines
	}

	s.total = position - s.leading
	s.indexDirty = false
}

// rowOfPreviousUserTurn reports the row the newest user entry above `before` starts
// at, which is where the turn the reader is walking back to began. ok is false when
// no user entry sits above that row.
func (s *transcriptStore) rowOfPreviousUserTurn(before int) (int, bool) {
	s.reindex()

	for index := range slices.Backward(s.records) {
		if s.records[index].block.kind != blockUser {
			continue
		}
		if s.starts[index] < before {
			return s.starts[index], true
		}
	}

	return 0, false
}

// userTurn is one prompt in the store: the region row it starts at, and the first
// line of its text, which is what the rail's preview calls it.
type userTurn struct {
	start  int
	prompt string
}

// userTurns returns every user turn, ascending. It is the rail's whole model of
// the conversation, so it is cached against the revision: a frame in which
// nothing changed re-uses the list rather than walking the records. The slice
// belongs to the store and is rebuilt in place when the revision changes, so a
// caller must not hold it across frames.
func (s *transcriptStore) userTurns() []userTurn {
	s.reindex()

	if s.turnsCached && s.turnsRevision == s.revision {
		return s.turns
	}

	s.turns = s.turns[:0]
	for index := range s.records {
		if s.records[index].block.kind == blockUser {
			s.turns = append(s.turns, userTurn{start: s.starts[index], prompt: s.records[index].prompt})
		}
	}

	s.turnsRevision = s.revision
	s.turnsCached = true
	s.turnRebuilds++

	return s.turns
}

// promptLine returns the first non-empty line of a prompt as one row of plain
// text: the band shows it on a single row, so styling, control characters and
// tabs have no place in it.
func promptLine(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(strings.ReplaceAll(sanitizeInspectionText(line), "\t", " "))
		if line != "" {
			return line
		}
	}

	return ""
}

// updateRevision fingerprints the record set, its geometry and the live
// record's height, so a search scan can skip a frame in which nothing it reads
// changed. It deliberately does not read the live record's text: only a search
// looks at that, and hashing a growing thought on every frame is what made the
// frame cost scale with the stream. [fingerprint] adds the text when asked.
//
// The block kind is part of the fingerprint because the rail's user turn cache
// keys on the same value: two records of equal height can trade places across a
// rebuild, and a cached turn list that did not see the change would point at the
// wrong rows.
func (s *transcriptStore) updateRevision() {
	hash := uint64(14695981039346656037)
	mix := func(value uint64) {
		hash ^= value
		hash *= 1099511628211
	}

	mix(uint64(s.width))
	mix(uint64(s.leading))
	mix(uint64(len(s.records)))
	mix(uint64(s.rowCount()))

	if s.noColor {
		mix(1)
	}

	for index := range s.themeKey {
		mix(uint64(s.themeKey[index]))
	}

	for index := range s.records {
		mix(uint64(s.records[index].lines)) //nolint:gosec // lines is a row count; it is never negative.
		// The block kind is what tells a user turn from its answer. Two records
		// with the same height can trade places across a rebuild, and the rail's
		// cached turn list has to see that as a change.
		kind := uint64(0)
		if block := s.records[index].block; block != nil {
			kind = uint64(block.kind)
		}

		mix(kind)
	}

	s.revision = hash
}

// fingerprint returns what a search scan compares against: the frame's revision
// with the live record's text folded in. A live record can change its text
// without changing its height, so the text is the only thing that tells a search
// it has to read the rows again.
func (s *transcriptStore) fingerprint() uint64 {
	hash := s.revision

	for index := range s.records {
		record := &s.records[index]
		if !record.live {
			continue
		}

		s.liveRowsHashed += record.rows.count()

		for _, row := range record.rows.frozen {
			hash = hashTranscriptRow(hash, row)
		}

		for _, row := range record.rows.tail {
			hash = hashTranscriptRow(hash, row)
		}
	}

	return hash
}

// hashTranscriptRow folds one row into a search fingerprint.
func hashTranscriptRow(hash uint64, row string) uint64 {
	for offset := range len(row) {
		hash = (hash ^ uint64(row[offset])) * 1099511628211
	}

	return hash
}

// loadRecord returns one record's complete rows, rendering them if they were
// released or had been reduced to a window. Callers that scan or flatten the
// document need every logical row; the windowed frame path uses
// [transcriptStore.loadRecordRange].
func (s *transcriptStore) loadRecord(index int) rowSegments {
	record := &s.records[index]
	if !record.loaded || record.rows.base != 0 ||
		record.rows.residentCount() != record.lines {
		s.materializeFull(record)
	}

	return record.rows
}

// loadRecordRange returns the rows needed for logical record rows [from, to).
// A resident window that already covers the range is reused; otherwise the
// record is rendered in full and only a bounded window around the request is
// kept.
func (s *transcriptStore) loadRecordRange(index, from, to int) rowSegments {
	record := &s.records[index]
	if from >= to {
		return rowSegments{base: from}
	}

	residentStart := record.rows.base
	residentEnd := residentStart + record.rows.residentCount()
	if record.loaded && from >= residentStart && to <= residentEnd {
		return record.rows
	}

	s.materializeFull(record)
	s.retainRecordRange(record, from, to)

	return record.rows
}

// materialize renders one live record's rows in place. A managed live block
// already returns a bounded tail window; a settled oversized record is trimmed
// by [transcriptStore.trim] once the reading window is known.
func (s *transcriptStore) materialize(record *transcriptRecord) {
	record.rows = s.render(*record.block)
	record.lines = record.rows.count()
	record.loaded = true
	s.materializations++
	s.rowsMaterialized += record.lines
}

// materializeFull renders every row of one record, bypassing the live row
// window. It is used when the reader or a full-document scan asks for rows the
// resident window no longer holds.
func (s *transcriptStore) materializeFull(record *transcriptRecord) {
	record.rows = s.renderFull(*record.block)
	record.lines = record.rows.count()
	record.loaded = true
	s.materializations++
	s.rowsMaterialized += record.lines
}

// retainRecordRange reduces one oversized record to a bounded window covering
// the requested logical range. from and to are record-relative and clamped.
func (s *transcriptStore) retainRecordRange(record *transcriptRecord, from, to int) {
	if record.lines <= maxResidentRowsPerRecord {
		return
	}

	from = min(max(0, from), record.lines)
	to = min(max(from, to), record.lines)
	if from >= to {
		from, to = 0, min(record.lines, maxResidentRowsPerRecord)
	}

	residentStart := record.rows.base
	residentEnd := residentStart + record.rows.residentCount()
	if record.rows.residentCount() <= maxResidentRowsPerRecord &&
		from >= residentStart && to <= residentEnd {
		return
	}
	if from < residentStart || to > residentEnd {
		s.materializeFull(record)
	}

	span := maxResidentRowsPerRecord
	start := max(0, from-transcriptWindowMargin)
	if to-start > span {
		start = max(0, to-span)
	}
	if start+span < to {
		start = max(0, to-span)
	}
	end := min(record.lines, start+span)
	if end-start < span && end == record.lines {
		start = max(0, end-span)
	}

	record.rows = record.rows.window(start, end)
}

// render produces one block's rows at the store's current geometry. A live
// Markdown block is assembled as its frozen rows and its tail directly, so a
// frame hands the store the rows it keeps without joining the whole frozen
// prefix into one string and splitting it again.
func (s *transcriptStore) render(block timelineBlock) rowSegments {
	width := max(1, s.width)
	if rows, ok := liveEntryRows(block, s.markdown, width, s.theme, s.noColor); ok {
		return rows
	}

	return rowSegments{frozen: splitTranscriptRows(renderTimelineEntry(
		block, s.markdown, width, s.theme, s.noColor, timelineRenderOptions{},
	))}
}

// renderFull renders a block without the live row window. Drafts and the live
// thinking section are rendered through their settled shape; the body and its
// styling are identical, only the source of the rows differs.
func (s *transcriptStore) renderFull(block timelineBlock) rowSegments {
	full := block
	switch full.kind {
	case blockDraft:
		full.kind = blockAssistant
	case blockThinking:
		if full.id == draftThinkingID {
			full.id = ""
		}
	default:
		// Settled blocks already have their complete render shape.
	}

	return rowSegments{frozen: splitTranscriptRows(renderTimelineEntry(
		full, s.markdown, max(1, s.width), s.theme, s.noColor, timelineRenderOptions{},
	))}
}

// rowCount reports the region's row count, including the leading rows.
func (s *transcriptStore) rowCount() int {
	s.reindex()

	return s.leading + s.total
}

// setKeep records the window whose neighbourhood stays resident.
func (s *transcriptStore) setKeep(start, end int) {
	s.keepFrom = start - transcriptWindowMargin
	s.keepTo = end + transcriptWindowMargin
	s.hasKeep = true
}

// trim releases the rows of every record outside the retention window and
// reduces an oversized record inside it to a bounded window. The row counts
// stay, so scrolling, anchors and hit testing keep working.
func (s *transcriptStore) trim() {
	if !s.hasKeep {
		return
	}

	for index := range s.records {
		record := &s.records[index]
		if !record.loaded {
			continue
		}

		start := s.starts[index]
		end := start + record.height()
		if end <= s.keepFrom || start >= s.keepTo {
			record.release()
			s.evictions++

			continue
		}
		if record.lines <= maxResidentRowsPerRecord {
			continue
		}

		from := max(0, s.keepFrom-start)
		to := min(record.lines, s.keepTo-start)
		s.retainRecordRange(record, from, to)
	}
}

// rowsIn returns the region rows in [start, end), materialising only the records
// that range touches and releasing the ones the window has left behind.
func (s *transcriptStore) rowsIn(start, end int) []string {
	total := s.rowCount()
	start = min(max(0, start), total)
	end = min(max(start, end), total)
	if start == end {
		return nil
	}

	rows := make([]string, 0, end-start)
	position := start

	if start < s.leading {
		fill := min(end, s.leading) - start
		rows = append(rows, make([]string, fill)...)
		position += fill
	}

	for index := range s.records {
		recordStart := s.starts[index]
		recordEnd := recordStart + s.records[index].lines

		// The blank row separating this record from the one above is part of the
		// region, so it is emitted whenever the window includes it, even when the
		// record itself starts below the window.
		if index > 0 {
			if separator := recordStart - 1; separator >= position && separator < end {
				rows = append(rows, "")
				position = separator + 1
			}
		}

		if recordEnd <= start || recordStart >= end {
			continue
		}

		from := max(0, position-recordStart)
		to := min(s.records[index].lines, end-recordStart)
		if from >= to {
			continue
		}

		recordRows := s.loadRecordRange(index, from, to)
		rows = recordRows.appendWindow(rows, from, to)
		position = recordStart + to
	}

	s.setKeep(start, end)
	s.trim()

	return rows
}

// transcriptDocument is a stable, paged snapshot of the conversation. It keeps
// the record identities, row counts and block pointers but no rendered rows, so
// the escape hatch and the exit handoff materialise one page at a time instead
// of retaining the whole flattened document.
type transcriptDocument struct {
	store transcriptStore
}

// snapshotDocument captures the current record set and geometry. The source
// store keeps its reading window; the document re-renders only the records a
// page touches and releases them immediately.
func (s *transcriptStore) snapshotDocument() *transcriptDocument {
	s.reindex()

	snapshot := transcriptStore{
		width: s.width, theme: s.theme, themeKey: s.themeKey, noColor: s.noColor,
		markdown: s.markdown, leading: s.leading, total: s.total,
		records: make([]transcriptRecord, len(s.records)),
		starts:  slices.Clone(s.starts),
	}
	for index, record := range s.records {
		record.rows = rowSegments{}
		record.loaded = false
		snapshot.records[index] = record
	}

	return &transcriptDocument{store: snapshot}
}

func (d *transcriptDocument) rowCount() int {
	if d == nil {
		return 0
	}

	return d.store.leading + d.store.total
}

func (d *transcriptDocument) rowsIn(start, end int) []string {
	if d == nil {
		return nil
	}

	return d.store.rowsIn(start, end)
}

// release drops every rendered row the last page materialised.
func (d *transcriptDocument) release() {
	if d == nil {
		return
	}

	for index := range d.store.records {
		d.store.records[index].release()
	}
}

// all materialises the whole document. Tests and small diagnostic reads use it;
// the escape hatch and the exit handoff always page.
func (d *transcriptDocument) all() []string {
	if d == nil {
		return nil
	}

	return d.store.flatten()
}

// flatten returns every region row. Export and small diagnostic reads read the
// document once, so they materialise it and then hand the residency window back
// to the reader.
func (s *transcriptStore) flatten() []string {
	total := s.rowCount()
	if total == 0 {
		return nil
	}

	rows := make([]string, 0, total)
	if s.leading > 0 {
		rows = append(rows, make([]string, s.leading)...)
	}

	for index := range s.records {
		if index > 0 {
			rows = append(rows, "")
		}

		rows = append(rows, s.loadRecord(index).all()...)
	}

	s.trim()

	return rows
}

// searchRows returns the flattened rows containing the query, ascending. It
// reads the rendered rows — the same text a reader can see — so a search matches
// exactly what the frame shows. Records the window does not need are released
// again before it returns.
func (s *transcriptStore) searchRows(query string) []int {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil
	}

	s.reindex()

	matches := make([]int, 0, 8)
	for index := range s.records {
		recordRows := s.loadRecord(index)
		offset := 0

		for _, row := range recordRows.frozen {
			if rowMatches(row, query) {
				matches = append(matches, s.starts[index]+offset)
			}

			offset++
		}

		for _, row := range recordRows.tail {
			if rowMatches(row, query) {
				matches = append(matches, s.starts[index]+offset)
			}

			offset++
		}
	}

	s.trim()

	return matches
}

// residentRowCount reports how many rendered rows the store currently holds.
func (s *transcriptStore) residentRowCount() int {
	s.reindex()

	resident := 0
	for index := range s.records {
		if s.records[index].loaded {
			resident += s.records[index].rows.residentCount()
		}
	}

	return resident
}

// residentRowBytes reports the byte size of the rendered rows the store holds.
func (s *transcriptStore) residentRowBytes() int {
	resident := 0
	for index := range s.records {
		if !s.records[index].loaded {
			continue
		}

		for _, row := range s.records[index].rows.frozen {
			resident += len(row)
		}

		for _, row := range s.records[index].rows.tail {
			resident += len(row)
		}
	}

	return resident
}

// recordIndexForRow reports which record owns a region row and the offset within
// it. Records are separated by a blank row, so a row may fall between two
// records; such a row is reported as the first row of the following record and
// flagged as a separator, which keeps an anchor from landing on a gap that later
// disappears.
func (s *transcriptStore) recordIndexForRow(row int) (int, int, bool) {
	s.reindex()
	if len(s.starts) == 0 || s.total == 0 {
		return -1, 0, false
	}

	if row < s.leading {
		// A leading row belongs to the gap above the first record, so it snaps
		// to that record's first row.
		return 0, 0, false
	}

	if row >= s.leading+s.total {
		last := len(s.records) - 1

		return last, max(0, s.records[last].lines-1), true
	}

	index := 0
	for candidate := range s.starts {
		if s.starts[candidate] <= row {
			index = candidate

			continue
		}

		break
	}

	offset := row - s.starts[index]
	if offset >= s.records[index].lines {
		// A separator row between records belongs to the record below it.
		if index+1 < len(s.records) {
			return index + 1, 0, false
		}

		return index, max(0, s.records[index].lines-1), true
	}

	return index, offset, true
}

// toolAt reports the Tool projection owning a region row.
func (s *transcriptStore) toolAt(row int) (timelineBlock, bool) {
	index, _, _ := s.recordIndexForRow(row)
	if index < 0 || index >= len(s.records) || s.records[index].tool == nil {
		return timelineBlock{}, false
	}

	return *s.records[index].tool, true
}

// anchor identifies a reading position by record identity rather than by row, so
// a resize that reflows the rows above it cannot move the reader.
type anchor struct {
	recordID string
	row      int
}

// captureAnchor converts a region row into a stable anchor. Rows that fall on a
// separator snap to the record below them, so the anchor always names a real
// row.
func (s *transcriptStore) captureAnchor(row int) (anchor, int) {
	index, offset, _ := s.recordIndexForRow(row)
	if index < 0 || index >= len(s.records) {
		return anchor{}, -1
	}

	resolved := s.starts[index] + offset

	return anchor{recordID: s.records[index].id, row: offset}, resolved
}

// resolveAnchor maps an anchor back to a region row, or -1 when the record is no
// longer loaded.
func (s *transcriptStore) resolveAnchor(value anchor) int {
	return s.rowForAnchor(value.recordID, value.row)
}

// rowForAnchor resolves an anchor back to a region row, or -1 when the record is
// gone.
func (s *transcriptStore) rowForAnchor(recordID string, rowInRecord int) int {
	if recordID == "" {
		return -1
	}

	s.reindex()

	for index, record := range s.records {
		if record.id != recordID {
			continue
		}

		return s.starts[index] + min(max(0, rowInRecord), max(0, record.lines-1))
	}

	return -1
}

// recordIDAt reports the identity of the record owning a region row.
func (s *transcriptStore) recordIDAt(row int) string {
	index, _, _ := s.recordIndexForRow(row)
	if index < 0 || index >= len(s.records) {
		return ""
	}

	return s.records[index].id
}

// maxTranscriptNotices bounds operator-facing output (banner, help, status, goal
// reports) that the fullscreen region keeps in memory. The inline path hands the
// same text to the terminal and never needs a bound; the managed path must not
// grow without one.
const maxTranscriptNotices = 64

// noticeEntry is one operator-facing block kept for the fullscreen transcript.
type noticeEntry struct {
	sequence uint64
	body     string
	// notice reports that the entry is operator output pinned to the frame edge.
	// The header is transcript content instead, so it takes the content column.
	notice bool
	// banner re-renders the header at the current width and theme, so a resize
	// cannot re-wrap a string that was rendered for an earlier one.
	banner *startupBannerContext
}

// appendNotice records operator-facing output for the managed transcript. It
// keeps the frame edge, because inline mode hands the same text to the terminal.
func (m *Model) appendNotice(content string) {
	m.appendNoticeEntry(content, true, nil)
}

// appendBanner records the startup and `/new` header, which is plain transcript
// content: without a box it has no frame edge to line up with, so it takes the
// same content column as a message.
func (m *Model) appendBanner(content string) {
	context := m.bannerContext(max(1, m.width-2*timelineInset(m.width)))
	m.appendNoticeEntry(content, false, &context)
}

func (m *Model) appendNoticeEntry(content string, notice bool, banner *startupBannerContext) {
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return
	}

	m.noticeSequence++
	m.notices = append(m.notices, noticeEntry{
		sequence: m.noticeSequence,
		body:     content,
		notice:   notice,
		banner:   banner,
	})
	if len(m.notices) > maxTranscriptNotices {
		m.notices = m.notices[len(m.notices)-maxTranscriptNotices:]
	}
}

// resetNotices drops operator-facing output that belonged to a replaced
// session, so the managed viewport shows one session at a time and the new
// banner becomes its first record. Inline mode never keeps these notices: it
// hands the same text to the terminal, whose native history survives the
// replacement. The sequence stays monotonic so no record identity repeats.
func (m *Model) resetNotices() {
	m.notices = nil
}

// noticeBlocks renders the operator-facing notices as leading transcript entries.
func (m *Model) noticeBlocks() []timelineBlock {
	if len(m.notices) == 0 {
		return nil
	}

	blocks := make([]timelineBlock, 0, len(m.notices))
	for _, notice := range m.notices {
		body := notice.body
		if notice.banner != nil {
			// The header is presentation, not a frozen record: it re-renders at
			// the frame's current geometry and theme.
			context := *notice.banner
			context.width = max(1, m.width-2*timelineInset(m.width))
			context.theme = m.theme
			context.noColor = m.options.NoColor
			body = renderStartupBanner(context)
		}
		blocks = append(blocks, timelineBlock{
			kind:     blockDiagnostic,
			id:       "notice:" + strconv.FormatUint(notice.sequence, 10),
			body:     body,
			position: 0,
			notice:   notice.notice,
		})
	}

	return blocks
}

// rowSource supplies the flattened rows a reading position moves over.
type rowSource interface {
	rowCount() int
	rowsIn(start, end int) []string
}

// sliceRows adapts a plain row slice to the reading position.
type sliceRows []string

func (rows sliceRows) rowCount() int {
	return len(rows)
}

func (rows sliceRows) rowsIn(start, end int) []string {
	start = min(max(0, start), len(rows))
	end = min(max(start, end), len(rows))

	return rows[start:end]
}

// scrollRegion is the reading position over a row source. It is deliberately not
// a widget: the TUI owns the rows and only needs window math plus an anchor that
// survives a resize or a new message.
type scrollRegion struct {
	source rowSource
	height int
	offset int
	// follow keeps the newest row visible. It is cleared by any user scroll away
	// from the bottom and restored by an explicit return-to-latest action or by
	// scrolling back to the end.
	follow bool
}

func newScrollRegion() scrollRegion {
	return scrollRegion{follow: true}
}

// setSource replaces the content, preserving the reading position. While
// following, the window stays pinned to the newest row; otherwise the caller
// re-anchors.
func (r *scrollRegion) setSource(source rowSource) {
	wasFollowing := r.follow
	r.source = source

	if wasFollowing {
		r.gotoBottom()

		return
	}

	r.clampOffset()
}

// setRows installs a plain row slice. The managed transcript installs the store
// instead, so that its rows stay windowed.
func (r *scrollRegion) setRows(rows []string) {
	r.setSource(sliceRows(rows))
}

// rowCount reports the row count the region moves over.
func (r *scrollRegion) rowCount() int {
	if r.source == nil {
		return 0
	}

	return r.source.rowCount()
}

// setHeight changes the window height, keeping the bottom row visible while
// following and clamping the offset otherwise.
func (r *scrollRegion) setHeight(height int) {
	r.height = max(0, height)
	if r.follow {
		r.gotoBottom()

		return
	}

	r.clampOffset()
}

func (r *scrollRegion) maxOffset() int {
	return max(0, r.rowCount()-max(0, r.height))
}

func (r *scrollRegion) clampOffset() {
	r.offset = min(max(0, r.offset), r.maxOffset())
	if r.offset == r.maxOffset() && r.maxOffset() > 0 {
		return
	}
	if r.maxOffset() == 0 {
		r.follow = true
	}
}

// atBottom reports whether the window already shows the newest row.
func (r *scrollRegion) atBottom() bool {
	return r.offset >= r.maxOffset()
}

// scrollBy moves the window, pausing follow when it leaves the bottom.
func (r *scrollRegion) scrollBy(delta int) {
	r.offset = min(max(0, r.offset+delta), r.maxOffset())
	r.follow = r.atBottom()
}

// scrollTo moves the window to an absolute offset.
func (r *scrollRegion) scrollTo(offset int) {
	r.scrollBy(offset - r.offset)
}

func (r *scrollRegion) gotoTop() {
	r.scrollTo(0)
}

// gotoBottom restores following.
func (r *scrollRegion) gotoBottom() {
	r.offset = r.maxOffset()
	r.follow = true
}

// visible returns the window's rows.
func (r *scrollRegion) visible() string {
	if r.height <= 0 || r.source == nil {
		return ""
	}

	end := min(r.rowCount(), r.offset+r.height)
	if end <= r.offset {
		return ""
	}

	return strings.Join(r.source.rowsIn(r.offset, end), "\n")
}

// scrollToRow moves the transcript window so one row sits at the top, and asks for
// the repaint the move needs. A row too close to the end stops at the tail, which
// is as far as the viewport scrolls.
func (m *Model) scrollToRow(row int) tea.Cmd {
	m.transcriptScroll.scrollTo(row)

	return redrawFrame()
}

// rowMatches reports whether one rendered row contains the query. Matching is
// case-insensitive over the row's visible text, which is what the terminal's own
// search does and what a reader can see.
func rowMatches(row, query string) bool {
	return query != "" && strings.Contains(strings.ToLower(ansi.Strip(row)), query)
}
