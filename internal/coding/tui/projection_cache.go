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
	merged      bool
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
// prefix while its inputs are unchanged.
func (m *Model) viewportProjection() frameProjection {
	stamp := m.projectionStamp()
	reused := m.frameCache.valid && m.frameCache.stamp == stamp
	if !reused {
		m.rebuildFrameCache(stamp)
	}

	cache := &m.frameCache
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

	wasMerged := cache.merged
	cache.merged = merged

	if reused && len(prefix) > 0 {
		stable := len(prefix)
		if wasMerged {
			// The record at the seam still holds last frame's merged card.
			stable = len(cache.entries) - 1
		}

		if stable == len(prefix) {
			return frameProjection{prefixEntries: prefix, tailEntries: tail, stable: stable}
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
	committed := m.projectCommittedTimeline()
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

	m.frameCache = timelineFrameCache{
		stamp:          stamp,
		valid:          true,
		entries:        entries,
		tailActivities: m.toolActivities[committed.tailStart:],
		markerSplit:    split,
	}
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
		live:  blockIsLive(block),
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
