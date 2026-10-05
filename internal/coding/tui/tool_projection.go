package tui

import "github.com/rsbin1178/pips/internal/coding"

// toolProjectionKey contains only what describeToolActivity observes. Text parts
// in defensive State copies share their immutable strings, so equality avoids
// rescanning large unchanged outputs. Structured/multipart content still uses the
// same visible-text projection as an uncached tool card.
type toolProjectionKey struct {
	id, name, arguments, runID string
	update, result             string
	turn, position, order      int
	live, hasResult, isError   bool
	status                     coding.ToolStatus
}

type toolProjectionEntry struct {
	key      toolProjectionKey
	activity toolActivity
}

type toolProjectionCache struct {
	sessionID string
	entries   map[string]toolProjectionEntry
}

// projectActivities returns the ordered tool activity the timeline projection
// consumes, reusing the per-tool description cache.
func (m *Model) projectActivities(state coding.State, excluded map[string]struct{}) []toolActivity {
	if m.toolProjection.sessionID != state.SessionID {
		m.toolProjection = toolProjectionCache{sessionID: state.SessionID}
	}

	return projectToolActivitiesCached(state, excluded, &m.toolProjection)
}

func (m *Model) projectTimeline(state coding.State, excluded map[string]struct{}) []timelineBlock {
	return projectTimelineActivities(state, m.projectActivities(state, excluded))
}

// projectCommittedTimeline projects the durable half of the timeline and keeps
// the activity list, so a cached frame can rebuild only the volatile tail.
func (m *Model) projectCommittedTimeline() committedProjection {
	m.toolActivities = m.projectActivities(m.state, nil)

	return projectCommittedBlocks(m.state, m.toolActivities)
}

func (c *toolProjectionCache) describe(record toolActivityRecord) toolActivity {
	if c == nil {
		return describeToolActivity(record)
	}

	key := toolProjectionKey{
		id: record.call.ID, name: record.call.Name, arguments: string(record.call.Arguments),
		runID: record.runID, turn: record.turn, position: record.position, order: record.order,
		live: record.live, status: record.status, hasResult: record.hasResult,
		isError: record.result.IsError,
		update:  visibleToolParts(record.update), result: visibleToolParts(record.result.Content),
	}
	if entry, ok := c.entries[key.id]; ok && entry.key == key {
		return entry.activity
	}

	activity := describeToolActivity(record)

	if c.entries == nil {
		c.entries = make(map[string]toolProjectionEntry)
	}

	c.entries[key.id] = toolProjectionEntry{key: key, activity: activity}

	return activity
}

func (c *toolProjectionCache) retain(records map[string]*toolActivityRecord) {
	if c == nil {
		return
	}

	for id := range c.entries {
		if _, ok := records[id]; !ok {
			delete(c.entries, id)
		}
	}
}
