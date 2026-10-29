//nolint:wsl_v5 // The worker's ownership and its request/result bounds stay adjacent.
package tui

import (
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// A long, un-freezable live tail costs the whole body to render (measured: 128 KiB
// of open fenced code is ~260 ms and ~260 MB of cumulative allocation since the
// dependency patch, and ~350 ms/~1.9 GB before it). The event loop must not pay
// that, because the same goroutine receives every keystroke, wheel notch and
// resize.
//
// The rendererer therefore hands that one version to a single background worker
// and keeps painting the frame it already has. Ownership is explicit:
//
//   - the worker owns its own markdownRenderer, so the event-loop renderer's
//     caches and engine are never read or written off the event loop;
//   - a request and a result are plain immutable values plus strings, so nothing
//     is shared but immutable data;
//   - at most one task executes and at most one request waits, and a newer
//     version replaces the waiting one;
//   - the newest version is always requested, so the final content is always
//     rendered, and every completed result is installed under its own version,
//     so a continuously growing stream still paints new frames;
//   - an epoch drops results whose projection was replaced.

// liveRenderRequest is one immutable version handed to the background renderer.
type liveRenderRequest struct {
	sequence uint64
	epoch    uint64
	miss     liveRenderMiss
}

// liveRenderResult is one version's rendered rows and what they cost.
type liveRenderResult struct {
	worker   *liveRenderWorker
	sequence uint64
	epoch    uint64
	miss     liveRenderMiss
	rows     rowSegments
	cost     time.Duration
	err      error
}

// liveRenderReadyMsg delivers one background result to the event loop.
type liveRenderReadyMsg struct {
	result liveRenderResult
}

// liveCatchUpMsg is the one-shot fallback that repaints a stale live frame when
// no background render is outstanding.
type liveCatchUpMsg struct {
	generation uint64
}

// liveRenderWorker is the single background renderer for live Markdown tails.
type liveRenderWorker struct {
	requests chan liveRenderRequest
	results  chan liveRenderResult
	done     chan struct{}
	stop     sync.Once
	// Only the event loop owns delivery bookkeeping. Replaced queued requests
	// need no result: the single worker completes surviving sequences in order.
	latest   liveRenderRequest
	received uint64
}

func newLiveRenderWorker() *liveRenderWorker {
	worker := &liveRenderWorker{
		requests: make(chan liveRenderRequest, liveRenderQueue),
		results:  make(chan liveRenderResult, 1),
		done:     make(chan struct{}),
	}
	go worker.run()

	return worker
}

// liveRenderDutyFactor bounds how much wall time the background renderer may
// spend rendering: one finished version stands for its own cost times this
// factor, so a whole-body render that costs 50 ms is repeated at most every
// 150 ms and the worker holds a third of a core instead of all of it.
//
// The frame on screen stays correct while the worker waits — it is the previous
// whole render of a shorter version of the same body — and the newest version is
// always the next one rendered, so this bounds CPU without dropping content. For
// an ordinary tail the wait is shorter than the next frame, which leaves
// realtime presentation unchanged; only an un-freezable body large enough to
// cost more than a frame is throttled, and that is exactly the body that used to
// hold a core for the length of the stream.
const liveRenderDutyFactor = 3

// liveRenderPause is how long the worker stands on a finished version before it
// takes the next one. A zero cost costs no pause, so a fast render never waits.
func liveRenderPause(cost time.Duration) time.Duration {
	return max(0, (liveRenderDutyFactor-1)*cost)
}

func (w *liveRenderWorker) run() {
	// The worker's renderer is private. Its capacity is small because it exists to
	// render one version at a time, not to cache a stream.
	renderer := newMarkdownRenderer(2)

	for {
		select {
		case <-w.done:
			return
		case request := <-w.requests:
			started := time.Now()
			rows, err := request.miss.render(renderer)
			cost := time.Since(started)
			result := liveRenderResult{
				sequence: request.sequence, epoch: request.epoch, miss: request.miss, rows: rows,
				cost: cost, err: err,
			}

			select {
			case w.results <- result:
			case <-w.done:
				return
			}

			// The installed version stays on screen while the worker waits, and a
			// request queued during the wait replaces any older one, so the pause
			// bounds the share of a core the renderer may take without ever
			// dropping content.
			if pause := liveRenderPause(cost); pause > 0 {
				timer := time.NewTimer(pause)

				select {
				case <-w.done:
					timer.Stop()

					return
				case <-timer.C:
				}
			}
		}
	}
}

// submit publishes the newest version without blocking the event loop. A waiting
// request is replaced; the executing one always finishes, so the newest version
// is always the next one rendered.
func (w *liveRenderWorker) submit(request liveRenderRequest) {
	if w.pending() && w.latest.epoch == request.epoch && w.latest.miss.sameVersion(request.miss) {
		return
	}
	request.sequence = w.latest.sequence + 1
	w.latest = request

	for range 2 {
		select {
		case w.requests <- request:
			return
		default:
		}

		select {
		case <-w.requests:
		default:
		}
	}
}

// pending remains true while any submitted version still needs delivery,
// independently of whether the model currently has a receive command armed.
func (w *liveRenderWorker) pending() bool {
	return w != nil && w.received < w.latest.sequence
}

func (w *liveRenderWorker) delivered(sequence uint64) {
	w.received = max(w.received, sequence)
	if !w.pending() {
		// A parked worker need not retain the source of its completed request.
		w.latest.miss = liveRenderMiss{}
	}
}

func (miss liveRenderMiss) sameVersion(other liveRenderMiss) bool {
	return miss.slot == other.slot && miss.content == other.content &&
		miss.width == other.width && miss.inset == other.inset &&
		miss.theme.fingerprint == other.theme.fingerprint &&
		miss.noColor == other.noColor && miss.window == other.window
}

// wait arms one result delivery. The results channel holds one entry and the
// event loop re-arms after each message, so one outstanding wait is enough.
func (w *liveRenderWorker) wait() tea.Cmd {
	return func() tea.Msg {
		select {
		case result := <-w.results:
			result.worker = w

			return liveRenderReadyMsg{result: result}
		case <-w.done:
			return nil
		}
	}
}

func (w *liveRenderWorker) shutdown() {
	if w == nil {
		return
	}
	w.stop.Do(func() { close(w.done) })
}

// render produces the exact rows for one version. It renders the whole document
// through the same row helper the synchronous fallback uses, so its output is
// byte-identical to the event loop's. There is no incremental highlighting here:
// the whole block is parsed and highlighted every time, so a multi-line comment,
// a string or any other lexer state cannot drift across lines.
func (miss liveRenderMiss) render(renderer *markdownRenderer) (rowSegments, error) {
	rendered, err := renderer.renderCached("", miss.content, max(1, miss.width), miss.theme, miss.noColor)
	if err != nil {
		return rowSegments{}, err
	}

	rows := rowSegments{
		frozen:   insetRowSlice(splitTranscriptRows(rendered), miss.inset),
		detached: miss.inset > 0,
	}
	if miss.window && rows.count() > maxLiveResidentRows {
		rows = rows.window(rows.count()-maxLiveResidentRows, rows.count())
	}

	return rows, nil
}
