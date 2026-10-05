package tui

import (
	"slices"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
)

// The managed viewport re-projects its whole timeline on every render tick. The
// committed part of that projection - the conversation already in the transcript
// and the tool activity that settled with it - cannot change while only the live
// draft grows, so it is cached here and reused frame after frame. Only the
// volatile tail is rebuilt. The uncached projection stays in place as the
// reference the cache is tested against.

// projectionHashSeed is the FNV-1a offset basis. These digests only have to tell
// two projections apart; they are not a security boundary.
const projectionHashSeed uint64 = 14695981039346656037

const (
	// projectionTranscriptWindow bounds how many trailing messages the transcript
	// digest reads. An append changes the transcript length; a wholesale
	// replacement (compaction, navigation, resume) changes the length, the head or
	// the tail. Walking every message would put the per-frame scan back that this
	// cache exists to remove.
	projectionTranscriptWindow = 16
	// projectionTextEdge is how many bytes of a payload the digest reads from each
	// end. Tool results are routinely large, so the digest covers their length
	// plus a bounded head and tail instead of every byte.
	projectionTextEdge = 24
)

func mixProjectionInt(hash uint64, value int) uint64 {
	hash ^= uint64(value)

	return hash * 1099511628211
}

func mixProjectionString(hash uint64, value string) uint64 {
	for index := 0; index < len(value); index++ {
		hash ^= uint64(value[index])
		hash *= 1099511628211
	}

	return hash
}

// mixProjectionText covers a payload by its length and a bounded head and tail.
func mixProjectionText(hash uint64, value string) uint64 {
	hash = mixProjectionInt(hash, len(value))

	head := min(len(value), projectionTextEdge)
	for index := 0; index < head; index++ {
		hash ^= uint64(value[index])
		hash *= 1099511628211
	}

	for index := max(0, len(value)-projectionTextEdge); index < len(value); index++ {
		hash ^= uint64(value[index])
		hash *= 1099511628211
	}

	return hash
}

func mixProjectionBytes(hash uint64, value []byte) uint64 {
	hash = mixProjectionInt(hash, len(value))

	head := min(len(value), projectionTextEdge)
	for index := 0; index < head; index++ {
		hash ^= uint64(value[index])
		hash *= 1099511628211
	}

	for index := max(0, len(value)-projectionTextEdge); index < len(value); index++ {
		hash ^= uint64(value[index])
		hash *= 1099511628211
	}

	return hash
}

func mixProjectionMedia(hash uint64, source ai.MediaSource) uint64 {
	hash = mixProjectionString(hash, source.ID)
	hash = mixProjectionString(hash, source.URL)
	hash = mixProjectionString(hash, source.MIMEType)

	return mixProjectionBytes(hash, source.Data)
}

// projectionPartDigest covers one content part. It reads parts without copying
// them, so it can run against a live State.
func projectionPartDigest(hash uint64, part ai.Part) uint64 {
	switch value := part.(type) {
	case ai.TextPart:
		return mixProjectionText(mixProjectionString(hash, "text"), value.Text)
	case ai.ReasoningPart:
		hash = mixProjectionText(mixProjectionString(hash, "reasoning"), value.Text)
		hash = mixProjectionString(hash, value.Signature)
		if value.Redacted {
			return mixProjectionInt(hash, 1)
		}

		return hash
	case ai.ToolCallPart:
		hash = mixProjectionString(hash, "call")
		hash = mixProjectionString(hash, value.ID)
		hash = mixProjectionString(hash, value.Name)

		return mixProjectionBytes(hash, value.Args)
	case ai.ToolResultPart:
		hash = mixProjectionString(hash, "result")
		hash = mixProjectionString(hash, value.ToolCallID)
		hash = mixProjectionString(hash, value.Name)
		hash = mixProjectionInt(hash, len(value.Content))
		if value.IsError {
			hash = mixProjectionInt(hash, 1)
		}

		for _, content := range value.Content {
			hash = projectionPartDigest(hash, content)
		}

		return hash
	case ai.ImagePart:
		return mixProjectionMedia(mixProjectionString(hash, "image"), value.Source)
	case ai.FilePart:
		hash = mixProjectionString(hash, "file")
		hash = mixProjectionString(hash, value.Name)

		return mixProjectionMedia(hash, value.Source)
	case ai.StructuredContentPart:
		return mixProjectionBytes(mixProjectionString(hash, "structured"), value.Data)
	case ai.ResourceLinkPart:
		hash = mixProjectionString(hash, "link")
		hash = mixProjectionString(hash, value.URI)
		hash = mixProjectionString(hash, value.Name)
		hash = mixProjectionString(hash, value.MIMEType)

		return hash
	case ai.EmbeddedResourcePart:
		hash = mixProjectionString(hash, "embedded")
		hash = mixProjectionString(hash, value.URI)
		hash = mixProjectionString(hash, value.MIMEType)
		hash = mixProjectionText(hash, value.Text)

		return mixProjectionBytes(hash, value.Blob)
	default:
		return mixProjectionString(hash, "unknown")
	}
}

// projectionMessageDigest covers one message's shape and payload edges. It reads
// the parts in place, so a frame never pays for a defensive copy to fingerprint
// the conversation it is about to reuse.
func projectionMessageDigest(hash uint64, message ai.Message) uint64 {
	parts, err := ai.MessagePartsView(message)
	if err != nil {
		return mixProjectionString(hash, "unreadable")
	}

	hash = mixProjectionString(hash, projectionMessageKind(message))
	hash = mixProjectionInt(hash, len(parts))

	for _, part := range parts {
		hash = projectionPartDigest(hash, part)
	}

	return hash
}

func projectionMessageKind(message ai.Message) string {
	switch message.(type) {
	case ai.SystemMessage:
		return "system"
	case ai.UserMessage:
		return "user"
	case ai.AssistantMessage:
		return "assistant"
	case ai.ToolMessage:
		return "tool"
	default:
		return "other"
	}
}

// projectionTranscriptDigest covers the conversation length plus the head and the
// trailing window of messages.
func projectionTranscriptDigest(messages ai.Messages) uint64 {
	hash := mixProjectionInt(projectionHashSeed, len(messages))
	if len(messages) == 0 {
		return hash
	}

	hash = projectionMessageDigest(hash, messages[0])
	for index := max(1, len(messages)-projectionTranscriptWindow); index < len(messages); index++ {
		hash = projectionMessageDigest(hash, messages[index])
	}

	return hash
}

// projectionCandidatesDigest covers the assistant-message identities that name
// the streaming tail. Candidates grow with the transcript, so the length plus the
// newest identity is enough to notice a replacement.
func projectionCandidatesDigest(candidates []coding.CandidateIdentity) uint64 {
	hash := mixProjectionInt(projectionHashSeed, len(candidates))
	if len(candidates) == 0 {
		return hash
	}

	newest := candidates[len(candidates)-1]
	hash = mixProjectionString(hash, newest.RunID)

	return mixProjectionInt(hash, newest.Turn)
}

func projectionIndexesDigest(indexes []int) uint64 {
	hash := mixProjectionInt(projectionHashSeed, len(indexes))
	if len(indexes) == 0 {
		return hash
	}

	hash = mixProjectionInt(hash, indexes[0])

	return mixProjectionInt(hash, indexes[len(indexes)-1])
}

// projectionToolsDigest covers the live tool overlay. A completed ToolState is
// terminal - the reducer only ever updates a tool that is still active - so only
// running tools need their progress content fingerprinted.
func projectionToolsDigest(tools []coding.ToolState) uint64 {
	hash := mixProjectionInt(projectionHashSeed, len(tools))
	for index := range tools {
		if tools[index].Status != coding.ToolStatusRunning {
			continue
		}

		hash = mixProjectionInt(hash, index)
		hash = mixProjectionString(hash, tools[index].Call.ID)
		hash = mixProjectionString(hash, string(tools[index].Status))
		hash = mixProjectionInt(hash, len(tools[index].Update))

		for _, part := range tools[index].Update {
			hash = projectionPartDigest(hash, part)
		}
	}

	return hash
}

// projectionSubagentsDigest covers the child-Session lifecycle the committed
// projection reads when it labels a subagent tool card.
func projectionSubagentsDigest(subagents []coding.SubagentState) uint64 {
	hash := mixProjectionInt(projectionHashSeed, len(subagents))
	for index := range subagents {
		value := &subagents[index]
		hash = mixProjectionString(hash, value.ChildSessionID)
		hash = mixProjectionString(hash, string(value.State))
		hash = mixProjectionString(hash, string(value.Role))
		hash = mixProjectionString(hash, value.Code)
		hash = mixProjectionString(hash, value.TaskPreview)
		hash = mixProjectionInt(hash, value.Turns)
		hash = mixProjectionInt(hash, value.ToolCalls)
		hash = mixProjectionInt(hash, int(value.DurationMillis))
		hash = mixProjectionString(hash, string(value.Activity.Action))
		hash = mixProjectionString(hash, value.Activity.Target)
	}

	return hash
}

// projectionMarkersDigest covers the completion markers the projection interleaves
// into the timeline.
func projectionMarkersDigest(markers []completionMarker) uint64 {
	hash := mixProjectionInt(projectionHashSeed, len(markers))
	for index := range markers {
		value := &markers[index]
		hash = mixProjectionString(hash, value.interactionID)
		hash = mixProjectionInt(hash, value.afterMessages)
		hash = mixProjectionString(hash, string(value.outcome))
		hash = mixProjectionString(hash, string(value.stop))
		hash = mixProjectionString(hash, value.model)
		hash = mixProjectionInt(hash, int(value.durationMillis))
	}

	return hash
}

// projectionNoticesDigest covers the operator-facing notices that lead the
// managed transcript.
func projectionNoticesDigest(notices []noticeEntry) uint64 {
	hash := mixProjectionInt(projectionHashSeed, len(notices))
	for index := range notices {
		hash = mixProjectionInt(hash, int(notices[index].sequence))
		hash = mixProjectionText(hash, notices[index].body)
	}

	return hash
}

// projectionHistoryDigest covers the durable history window that leads the
// conversation. Prepending an older page moves start and grows the window.
func projectionHistoryDigest(history historyState) uint64 {
	hash := mixProjectionInt(projectionHashSeed, len(history.messages))

	return mixProjectionInt(hash, history.start)
}

// projectionStamp fingerprints every input the committed prefix of the managed
// timeline projection reads. While the stamp is unchanged the prefix is reused
// verbatim; the volatile tail is rebuilt on every frame regardless.
type projectionStamp struct {
	sessionID    string
	width        int
	themeKey     themeFingerprint
	noColor      bool
	showThinking bool
	transcript   uint64
	candidates   uint64
	synthetics   uint64
	tools        uint64
	subagents    uint64
	markers      uint64
	notices      uint64
	history      uint64
}

// projectionStamp reads the committed-prefix inputs. Every component is O(1) or
// bounded by a small collection; none of them walks the transcript.
func (m *Model) projectionStamp() projectionStamp {
	return projectionStamp{
		sessionID:    m.state.SessionID,
		width:        m.width,
		themeKey:     m.themeFingerprint(),
		noColor:      m.options.NoColor,
		showThinking: m.showThinkingBlocks(),
		transcript:   projectionTranscriptDigest(m.state.Transcript),
		candidates:   projectionCandidatesDigest(m.state.MessageCandidates),
		synthetics:   projectionIndexesDigest(m.state.SyntheticMessages),
		tools:        projectionToolsDigest(m.state.Tools),
		subagents:    projectionSubagentsDigest(m.state.Subagents),
		markers:      projectionMarkersDigest(m.completionMarkers),
		notices:      projectionNoticesDigest(m.notices),
		history:      projectionHistoryDigest(m.history),
	}
}

// extendableFrom reports whether every committed input except the conversation
// itself and the candidate identities is unchanged, which is the precondition for
// extending a cached prefix by appended messages. The transcript and the
// candidates are expected to grow; their covered ranges are verified separately.
func (s projectionStamp) extendableFrom(base projectionStamp) bool {
	return s.sessionID == base.sessionID &&
		s.width == base.width &&
		s.themeKey == base.themeKey &&
		s.noColor == base.noColor &&
		s.showThinking == base.showThinking &&
		s.synthetics == base.synthetics &&
		s.tools == base.tools &&
		s.subagents == base.subagents &&
		s.markers == base.markers &&
		s.notices == base.notices &&
		s.history == base.history
}

// timelineFrameCache is the committed prefix of one managed-viewport frame: the
// older history pages, the operator notices and the durable conversation, each
// with the transcript entry that names it. It stays valid while its stamp does.
// merged records whether the last frame folded a live tail card into the prefix,
// which decides how much of the store's tail is still reusable.
type timelineFrameCache struct {
	stamp          projectionStamp
	valid          bool
	entries        []transcriptEntry
	tailActivities []toolActivity
	// markerSplit is how many completion markers the cached prefix already
	// carries; the rest belong to the volatile tail.
	markerSplit int
	// mergedEntry is the index in entries whose store record holds the seam card
	// the last frame folded into the volatile tail, or -1 when no seam was folded.
	// It is an absolute index because entries can grow after the fold, and the
	// store reuses everything before the record it names.
	mergedEntry int
	// covered is how many transcript messages the committed blocks were projected
	// from, and cutoff the highest conversation position they commit.
	covered int
	cutoff  int
	// candidateCount is how many candidate identities the covered range had. The
	// stamp already fingerprints the whole transcript and candidate list, so the
	// append path proves the covered range unchanged against stamp.transcript and
	// stamp.candidates instead of keeping a second copy of each digest. The reducer
	// appends to a State without writing memory another State may read, so a
	// covered range is frozen by construction; those digests catch a replacement
	// that kept the shape.
	candidateCount int
	// toolScan is the transcript fold the covered range was scanned with, and
	// toolTailStart where the activity list it produced splits into committed and
	// volatile. lastCommitted is the last committed block, with lastCommittedEntry
	// naming its entry, because the exploration grouping can still fold an appended
	// card into it.
	toolScan           toolScanState
	toolTailStart      int
	lastCommitted      timelineBlock
	lastCommittedEntry int
}

// entryForBlock names the entry that holds a block, or -1 when the entries do not
// carry it. Entries after the last committed block are completion markers (and,
// when thinking is hidden, dropped reasoning), so the scan from the end is short.
func (m *Model) entryForBlock(entries []transcriptEntry, block timelineBlock) int {
	identity := m.blockIdentity(block)
	if identity == "" {
		return -1
	}

	for index := len(entries) - 1; index >= 0; index-- {
		if entries[index].id == identity {
			return index
		}
	}

	return -1
}

// frameProjection is one managed-viewport frame. The entries are split where the
// projection stopped changing, so the transcript store can keep the rendered rows
// of the prefix and rebuild only the tail.
type frameProjection struct {
	prefixEntries []transcriptEntry
	tailEntries   []transcriptEntry
	stable        int
}

// viewportProjection projects one managed-viewport frame, reusing the committed
// prefix while its inputs are unchanged and extending it when messages were
// appended.
func (m *Model) viewportProjection() frameProjection {
	stamp := m.projectionStamp()
	cache := &m.frameCache

	reused := cache.valid && cache.stamp == stamp
	// extendedFrom is the index the store must rebuild from when this frame grew
	// the prefix by an append; -1 means the whole prefix is unchanged.
	extendedFrom := -1
	if !reused {
		if index, ok := m.extendFrameCache(stamp); ok {
			reused = true
			extendedFrom = index
		} else {
			m.rebuildFrameCache(stamp)
		}
	}

	tail := m.frameEntriesFor(m.volatileTimelineBlocks(cache.tailActivities, cache.markerSplit))
	prefix := cache.entries

	// The explore grouping folds left across the seam between the committed
	// prefix and the live tail, so a tail card can still merge into the last
	// committed one. The merged card replaces both entries, so the store rebuilds
	// from the seam.
	merged := len(prefix) > 0 && len(tail) > 0 &&
		isExploreBlock(prefix[len(prefix)-1].block) && isLoneExploreBlock(tail[0].block)
	if merged {
		card := prefix[len(prefix)-1].block
		card.tools = append(slices.Clone(card.tools), tail[0].block.tools...)
		card.id = tail[0].block.id
		tail[0] = m.entryFor(card)
		prefix = prefix[:len(prefix)-1]
	}

	// The store's record where the last frame folded a seam still holds that
	// frame's merged card rather than the entry the cache kept, so this frame must
	// rebuild from there. The index is absolute because entries can grow.
	wasMergedEntry := cache.mergedEntry
	if merged {
		cache.mergedEntry = len(cache.entries) - 1
	} else {
		cache.mergedEntry = -1
	}

	if reused {
		// A cache hit keeps the whole prefix; an extension keeps everything before
		// the appended entries.
		stable := len(prefix)
		if extendedFrom >= 0 {
			stable = min(extendedFrom, len(prefix))
		}
		if wasMergedEntry >= 0 {
			stable = min(stable, wasMergedEntry)
		}

		switch {
		case stable > 0 && stable == len(prefix):
			return frameProjection{prefixEntries: prefix, tailEntries: tail, stable: stable}
		case stable > 0:
			// The prefix changed part way through: the store keeps its leading
			// records and rebuilds the rest from the entries that follow them plus
			// the fresh tail.
			entries := make([]transcriptEntry, 0, len(prefix)-stable+len(tail))
			entries = append(entries, prefix[stable:]...)
			entries = append(entries, tail...)

			return frameProjection{prefixEntries: prefix[:stable], tailEntries: entries, stable: stable}
		}
	}

	// A rebuilt prefix can name different blocks, so the whole frame is offered
	// as one list the store rebuilds from scratch.
	entries := make([]transcriptEntry, 0, len(prefix)+len(tail))
	entries = append(entries, prefix...)
	entries = append(entries, tail...)

	return frameProjection{prefixEntries: entries}
}

// rebuildFrameCache re-projects the committed prefix and stores it with the stamp
// it was built for.
func (m *Model) rebuildFrameCache(stamp projectionStamp) {
	committed, scan := m.projectCommittedTimeline()
	history := m.historyBlocks()
	notices := m.noticeBlocks()

	blocks := make([]timelineBlock, 0, len(history)+len(notices)+len(committed.blocks)+1)
	blocks = append(blocks, history...)
	blocks = append(blocks, notices...)
	blocks = append(blocks, committed.blocks...)

	split := splitCompletionMarkers(m.completionMarkers, committed.cutoff)
	blocks = insertCompletionMarkers(blocks, m.completionMarkers[:split])
	if !stamp.showThinking {
		blocks = withoutThinkingBlocks(blocks)
	}

	entries := make([]transcriptEntry, 0, len(blocks))
	for _, block := range blocks {
		entries = append(entries, m.entryFor(block))
	}

	var lastCommitted timelineBlock
	if len(committed.blocks) > 0 {
		lastCommitted = committed.blocks[len(committed.blocks)-1]
	}

	m.frameCache = timelineFrameCache{
		stamp:              stamp,
		valid:              true,
		entries:            entries,
		tailActivities:     m.toolActivities[committed.tailStart:],
		markerSplit:        split,
		mergedEntry:        -1,
		covered:            len(m.state.Transcript),
		cutoff:             committed.cutoff,
		candidateCount:     len(m.state.MessageCandidates),
		toolScan:           scan,
		toolTailStart:      committed.tailStart,
		lastCommitted:      lastCommitted,
		lastCommittedEntry: m.entryForBlock(entries, lastCommitted),
	}
}

// extendFrameCache grows the cached committed prefix by the messages appended
// since the frame it was built for, instead of re-projecting the whole
// conversation. It reports the index the store must rebuild from, and whether the
// append was proven and applied; when it was not, the caller rebuilds the prefix
// from scratch.
//
// The reducer appends to a State without writing memory another State may read,
// so a covered range is frozen by construction; the cache still re-fingerprints
// that range and refuses the fast path whenever an input outside it moved.
func (m *Model) extendFrameCache(stamp projectionStamp) (int, bool) {
	cache := &m.frameCache
	if !cache.valid || cache.covered <= 0 || !stamp.extendableFrom(cache.stamp) {
		return 0, false
	}

	messages := m.state.Transcript
	candidates := m.state.MessageCandidates
	if len(messages) <= cache.covered || len(candidates) < cache.candidateCount {
		return 0, false
	}
	// The cached stamp already fingerprints the transcript and the candidate list
	// it covered, so proving the covered range is two digest comparisons.
	if projectionTranscriptDigest(messages[:cache.covered]) != cache.stamp.transcript {
		return 0, false
	}
	if projectionCandidatesDigest(candidates[:cache.candidateCount]) != cache.stamp.candidates {
		return 0, false
	}

	from := len(cache.entries)
	if messageRangeCarriesToolParts(messages[cache.covered:]) {
		return m.appendToolFrameCache(stamp, messages, candidates)
	}

	m.appendFrameCache(stamp, messages, candidates)

	return from, true
}

// messageRangeCarriesToolParts reports whether any message holds tool activity,
// which decides whether an append can keep the retained activity list.
func messageRangeCarriesToolParts(messages ai.Messages) bool {
	for _, message := range messages {
		if messageCarriesToolParts(message) {
			return true
		}
	}

	return false
}

// touchesCommittedToolRecord reports whether a message's tool activity names a
// record the cached prefix already placed. The prefix cannot absorb such a part:
// it would move that record's block, or change how it reads.
func touchesCommittedToolRecord(scan *toolScanState, message ai.Message, covered int) bool {
	parts, err := ai.MessagePartsView(message)
	if err != nil {
		return true
	}

	for _, part := range parts {
		var id string

		switch value := part.(type) {
		case ai.ToolCallPart:
			id = value.ID
		case ai.ToolResultPart:
			id = value.ToolCallID
		default:
			continue
		}

		record, ok := scan.records[id]
		if !ok {
			continue
		}
		if toolRecordCommitted(record, covered) {
			return true
		}
	}

	return false
}

// toolRecordCommitted reports whether the committed projection already consumed a
// record: it produces an activity, and its conversation position lies at or before
// the last transcript message the prefix covered.
func toolRecordCommitted(record *toolActivityRecord, covered int) bool {
	if !record.live && !record.hasResult {
		return false
	}
	if record.call.ID == "" || record.call.Name == "" {
		return false
	}

	return record.position > 0 && record.position <= covered
}

// committedActivityEnd reports where an ordered activity list stops being
// committed. The list is ordered by position, and a position past the last
// transcript message can only belong to the live overlay, which the volatile tail
// keeps.
func committedActivityEnd(activities []toolActivity, messages int) int {
	for index := range activities {
		if activities[index].position > messages {
			return index
		}
	}

	return len(activities)
}

// messageCarriesToolParts reports whether a message holds a tool call or a tool
// result. An unreadable message reports true so the caller rebuilds instead of
// trusting an append it cannot inspect.
func messageCarriesToolParts(message ai.Message) bool {
	parts, err := ai.MessagePartsView(message)
	if err != nil {
		return true
	}

	for _, part := range parts {
		switch part.(type) {
		case ai.ToolCallPart, ai.ToolResultPart:
			return true
		}
	}

	return false
}

// appendFrameCache projects the messages in messages[covered:] and appends their
// entries to the committed prefix. The caller has proven that the covered range
// is unchanged and that the appended messages carry no tool activity.
func (m *Model) appendFrameCache(
	stamp projectionStamp,
	messages ai.Messages,
	candidates []coding.CandidateIdentity,
) {
	cache := &m.frameCache
	covered := cache.covered

	synthetic := make(map[int]struct{})
	for _, index := range m.state.SyntheticMessages {
		if index >= covered {
			synthetic[index] = struct{}{}
		}
	}

	blocks := make([]timelineBlock, 0, len(messages)-covered)
	cutoff := cache.cutoff
	for index := covered; index < len(messages); index++ {
		if _, skip := synthetic[index]; skip {
			continue
		}

		candidateID := ""
		if index < len(candidates) {
			candidateID = candidates[index].Key()
		}

		committed := 0
		blocks, committed = appendCommittedMessageBlocks(blocks, messages[index], index+1, candidateID)
		cutoff = max(cutoff, committed)
	}

	split := splitCompletionMarkers(m.completionMarkers, cutoff)
	// Markers that leave the volatile tail for the committed prefix are placed
	// among the appended blocks: every cached block sits at or before the old
	// cutoff, so a marker with a later position can only belong after them.
	blocks = insertCompletionMarkers(blocks, m.completionMarkers[cache.markerSplit:split])
	if !stamp.showThinking {
		blocks = withoutThinkingBlocks(blocks)
	}

	// Appended messages carry no tool parts, so no appended block can be an
	// exploration card and the committed blocks need no grouping across the seam.
	entries := make([]transcriptEntry, 0, len(blocks))
	for _, block := range blocks {
		entries = append(entries, m.entryFor(block))
	}

	// A live activity's conversation position is the end of the transcript, so
	// the cached tail activities move with the appended messages.
	for index := range cache.tailActivities {
		if cache.tailActivities[index].position > covered {
			cache.tailActivities[index].position = len(messages) + 1
		}
	}

	cache.entries = append(cache.entries, entries...)
	cache.covered = len(messages)
	cache.cutoff = cutoff
	cache.markerSplit = split
	cache.stamp = stamp
	cache.candidateCount = len(candidates)
	m.frameExtends++
}

// appendToolFrameCache extends the committed prefix by messages that carry tool
// activity. It resumes the retained transcript fold at the appended messages,
// re-derives the activity list from the retained records, and appends the blocks
// the new activity commits. It reports the index the store must rebuild from, and
// whether the append was proven.
func (m *Model) appendToolFrameCache(
	stamp projectionStamp,
	messages ai.Messages,
	candidates []coding.CandidateIdentity,
) (int, bool) {
	cache := &m.frameCache
	scan := &cache.toolScan
	if scan.records == nil {
		return 0, false
	}

	for _, message := range messages[cache.covered:] {
		if touchesCommittedToolRecord(scan, message, cache.covered) {
			return 0, false
		}
	}

	scan.scanTranscript(messages, nil)
	scan.overlayLive(m.state.Tools, len(messages), nil)

	activities := describeToolActivities(scan, &m.toolProjection)
	m.toolProjection.retain(scan.records)

	// Every appended part is new to the fold, so it can only add an activity or
	// move one later; the committed activities stay outside the appended range.
	newTailStart := committedActivityEnd(activities, len(messages))
	if newTailStart < cache.toolTailStart {
		return 0, false
	}

	synthetic := make(map[int]struct{})
	for _, index := range m.state.SyntheticMessages {
		if index >= cache.covered {
			synthetic[index] = struct{}{}
		}
	}

	rangeBlocks, rangeCutoff := projectCommittedRange(
		m.state, cache.covered, synthetic, candidates,
		activities[cache.toolTailStart:newTailStart],
	)

	// The exploration grouping folds left, so an appended card can still merge
	// into the last committed one. The merged card replaces both entries.
	grouped := groupExploreBlocks(rangeBlocks)
	seam := len(grouped) > 0 && isExploreBlock(cache.lastCommitted) && isLoneExploreBlock(grouped[0])
	if seam && cache.lastCommittedEntry < 0 {
		return 0, false
	}

	cutoff := max(cache.cutoff, rangeCutoff)
	from := len(cache.entries)
	lastCommitted := cache.lastCommitted

	if seam {
		card := cache.lastCommitted
		card.tools = append(slices.Clone(card.tools), grouped[0].tools...)
		card.id = grouped[0].id
		cache.entries[cache.lastCommittedEntry] = m.entryFor(card)
		lastCommitted = card
		from = cache.lastCommittedEntry
		grouped = grouped[1:]
	}

	// Markers the new cutoff moves into the prefix are placed among the appended
	// blocks: every cached block sits at or before the old cutoff, so a marker with
	// a later position can only belong after them.
	split := splitCompletionMarkers(m.completionMarkers, cutoff)
	visible := insertCompletionMarkers(grouped, m.completionMarkers[cache.markerSplit:split])
	if !stamp.showThinking {
		visible = withoutThinkingBlocks(visible)
	}

	entries := make([]transcriptEntry, 0, len(visible))
	for _, block := range visible {
		entries = append(entries, m.entryFor(block))
	}

	if len(grouped) > 0 {
		lastCommitted = grouped[len(grouped)-1]
	}

	cache.entries = append(cache.entries, entries...)
	cache.covered = len(messages)
	cache.cutoff = cutoff
	cache.markerSplit = split
	cache.toolTailStart = newTailStart
	cache.tailActivities = activities[newTailStart:]
	cache.stamp = stamp
	cache.candidateCount = len(candidates)
	cache.lastCommitted = lastCommitted
	cache.lastCommittedEntry = m.entryForBlock(cache.entries, lastCommitted)
	m.toolActivities = activities
	m.frameExtends++

	return from, true
}

// volatileTimelineBlocks projects the live tail: the activity still running, the
// growing draft, the stream error and the notices that trail the conversation.
func (m *Model) volatileTimelineBlocks(activities []toolActivity, markerSplit int) []timelineBlock {
	blocks := projectVolatileBlocks(m.state, activities)
	blocks = insertCompletionMarkers(blocks, m.completionMarkers[markerSplit:])
	if !m.showThinkingBlocks() {
		blocks = withoutThinkingBlocks(blocks)
	}

	if m.streamErr != nil {
		blocks = append(blocks, timelineBlock{
			kind: blockError, title: "Operation", body: safeError(m.streamErr),
		})
	}

	blocks = append(blocks, m.activePlanModeNoticeBlocks()...)

	return append(blocks, m.activeTeamAttemptBlocks()...)
}

// entryFor names one projected block for the transcript store.
func (m *Model) entryFor(block timelineBlock) transcriptEntry {
	return transcriptEntry{
		id:    m.blockIdentity(block),
		live:  blockIsUnsettled(block),
		block: block,
	}
}

// frameEntriesFor names the blocks the store has to rebuild. The buffer is reused
// across frames; the store copies what it keeps.
func (m *Model) frameEntriesFor(blocks []timelineBlock) []transcriptEntry {
	entries := m.frameEntries[:0]
	for _, block := range blocks {
		entries = append(entries, m.entryFor(block))
	}

	m.frameEntries = entries

	return entries
}

// splitCompletionMarkers reports how many markers belong to the committed prefix.
// A marker is emitted before the first block whose position is greater than its
// afterMessages, so a marker belongs to the prefix exactly while the prefix still
// holds such a block. Markers are ordered by afterMessages, which the recorder
// preserves.
func splitCompletionMarkers(markers []completionMarker, cutoff int) int {
	for index := range markers {
		if markers[index].afterMessages >= cutoff {
			return index
		}
	}

	return len(markers)
}
