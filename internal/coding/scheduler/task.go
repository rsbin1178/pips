package scheduler

import "time"

// Task is one durable scheduled task. Every task pips records is recurring: a
// scheduled task keeps firing until it is deleted.
type Task struct {
	ID           string    `json:"id"`
	Prompt       string    `json:"prompt"`
	IntervalSecs int64     `json:"interval_seconds"`
	Recurring    bool      `json:"recurring"`
	Durable      bool      `json:"durable"`
	CreatedAt    time.Time `json:"created_at"`
	// LastFiredAt is zero until the first fire.
	LastFiredAt time.Time `json:"last_fired_at,omitzero"`
	// NextFireAt is the authoritative schedule. A fire advances it from its own
	// previous value, so the phase survives a late or a skipped fire.
	NextFireAt time.Time `json:"next_fire_at"`
	// LastChildID is the child the task's last fire started. The next fire is
	// skipped while that child still runs.
	LastChildID string `json:"last_child_id,omitempty"`
}

// Interval returns the task's interval as a duration.
func (t Task) Interval() time.Duration {
	return time.Duration(t.IntervalSecs) * time.Second
}

// NextFireAfter returns the first scheduled time strictly after now, advancing
// from the task's own next fire so the schedule keeps its phase. A next fire
// already in the past is skipped forward to the next future occurrence rather
// than replayed, so an overdue task fires once instead of once per missed
// interval.
func NextFireAfter(next time.Time, interval time.Duration, now time.Time) time.Time {
	if interval <= 0 || next.After(now) {
		return next
	}

	elapsed := now.Sub(next)

	// Skip forward whole intervals in one step: the next occurrence strictly
	// after now, without replaying every missed interval.
	return next.Add(elapsed - elapsed%interval + interval)
}
