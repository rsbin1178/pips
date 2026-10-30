//nolint:wsl_v5 // The MVU state machine keeps transition effects adjacent.
package tui

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	codingclipboard "github.com/rsbin1178/pips/internal/coding/clipboard"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/runtimecontrol"
	"github.com/rsbin1178/pips/internal/coding/statusline"
)

var errStatusLinePersistenceUnavailable = errors.New("status-line persistence is unavailable")

const (
	defaultWidth          = 80
	defaultHeight         = 24
	statusHorizontalInset = 2
	composerMaxLines      = 8
	conversationGapHeight = 1
	// composerBoxMinWidth is the narrowest frame that still draws the bordered
	// Composer. Conversation rows align with that box's text, so a narrower
	// frame — which has no box — keeps the full width.
	composerBoxMinWidth = 24
	// composerModeGap keeps the permission mode's label off the box's bottom
	// border rule.
	composerModeGap      = 2
	renderFrame          = 33 * time.Millisecond
	exitConfirmTime      = 2 * time.Second
	maxCompletionMarkers = 256
)

type lifecycle uint8

const (
	lifecycleTrust lifecycle = iota
	lifecycleLoading
	lifecycleReady
	lifecycleFatal
)

type bootstrapResult struct {
	controller Controller
	err        error
}

type submissionKind uint8

const (
	submissionPrompt submissionKind = iota
	submissionQueued
)

type composerResolvedMsg struct {
	generation uint64
	snapshot   composerSnapshot
	kind       submissionKind
	message    ai.Message
	err        error
}

type cancelResultMsg struct {
	bridge      *eventBridge
	generation  uint64
	beforeStart bool
	err         error
}

type exitResetMsg struct{}

// Model is the root Bubble Tea state machine.
type Model struct {
	ctx              context.Context //nolint:containedctx // Program context owns every asynchronous command.
	options          Options
	lifecycle        lifecycle
	width            int
	height           int
	sizeReady        bool
	startupPublished bool
	allow            bool
	err              error

	controller           Controller
	state                coding.State
	modelUsage           modelUsageTally
	usageProjection      usageProjectionView
	usageProjectionSeq   uint64
	childStates          map[string]coding.State
	composer             composerState
	toolProjection       toolProjectionCache
	markdown             *markdownRenderer
	theme                colorTheme
	themeSelection       string
	themeRegistry        themeRegistry
	themeBackgroundKnown bool
	themeIsDark          bool
	themeDiagnostics     []themeDiagnostic
	transcript           transcriptStore
	transcriptScroll     scrollRegion
	// turnRail is the turn rail's window across frames; see turn_rail.go.
	turnRail turnRailState
	// childScroll is the read-only child page's reading position. It is the shared
	// region type, so the page opens on its newest row and follows appended content
	// exactly as the conversation does.
	childScroll      scrollRegion
	selection        selectionState
	search           searchState
	mouseCaptureOff  bool
	transcriptMode   transcriptModeState
	transcriptHanded bool // Escape hatch already wrote the rows to the main buffer.
	frameHit         frameHitMap
	// tasksPane is the model-local tasks overlay, and tasksPaneHit is where the
	// last painted pane sat.
	tasksPane        tasksPane
	tasksPaneHit     tasksPaneHitMap
	history          historyState
	notices          []noticeEntry
	noticeSequence   uint64
	scrollback       scrollbackCursor
	scrollbackOutput bool
	streaming        streamProjection
	renderWait       bool
	renderDirty      bool
	// liveRender is the single background renderer for an expensive live
	// Markdown tail; liveRenderWait reports one result delivery is already armed.
	// liveRenderEpoch is bumped when the projection is replaced so a late result
	// cannot reach the new frame. liveCatchUpWait/liveCatchUpGen bound the one-shot
	// catch-up that repaints a stale live frame.
	liveRender      *liveRenderWorker
	liveRenderWait  bool
	liveRenderEpoch uint64
	liveGeometry    liveGeometry
	liveCatchUpWait bool
	liveCatchUpGen  uint64
	// viewStable marks an update that did not change what the frame renders: a
	// deferred delta the reader cannot see yet, or a delivery the subscription
	// already owns and the operation iterator only repeats. View() then reuses the
	// last composition instead of re-styling the whole frame for it. The OSC 7501
	// report is part of that composition, so a status change must clear this flag
	// as well.
	// viewStale records that the cached composition was invalidated outside an
	// Update - a direct rerenderTranscript - and no View call has adopted that
	// invalidation yet, so no update may reuse the cache until it has.
	// viewCached reports that viewCache holds a composition, and viewComposes
	// counts them.
	viewStable   bool
	viewStale    bool
	viewCached   bool
	viewCache    tea.View
	viewComposes int
	// statusReport is the current OSC 7501 report and statusFields its comparable
	// identity. Reusing the pointer while the fields hold lets Bubble Tea emit
	// nothing for an unchanged state.
	statusReport *tea.ProgramStatus
	statusFields programStatusFields
	// frameCache reuses the committed prefix of the managed timeline between
	// frames, and frameEntries is the buffer the volatile tail is named in.
	// toolActivities is the activity list that prefix was projected from, so the
	// volatile tail can be rebuilt without rescanning the transcript.
	frameCache     timelineFrameCache
	frameEntries   []transcriptEntry
	toolActivities []*toolActivity
	// pending holds drained stream items whose events only touch the live draft.
	// They are applied once per frame at the render tick instead of once per
	// event, so a burst of deltas costs one state advance and one render. Durable
	// events, errors, approvals and session transitions never wait here.
	pending []streamItem
	// frameAdvances and frameRenders count the managed frame's state advances and
	// re-projections, so a test can assert the frame-boundary reduction.
	frameAdvances int
	frameRenders  int
	// frameExtends counts committed prefixes the append path extended instead of
	// rebuilding, so a test can tell an incremental commit frame from a full one.
	frameExtends int
	// streamDeliveries and subscriptionDeliveries count the messages the event
	// loop handled on each bridge, and streamItems the drained records they
	// carried, so a test can tell per-event delivery from per-batch delivery.
	// streamBatches counts the drained updates that applied a batch.
	streamDeliveries       int
	subscriptionDeliveries int
	streamItems            int
	streamBatches          int
	bridge                 *eventBridge
	subscription           *subscriptionBridge
	subscriptionSeq        uint64
	subscriptionMode       bool
	// runtimeState marks a subscription to a controller that reduces events
	// itself, so the parent projection is read from the Runtime instead of
	// being reduced a second time (route A of the single-reduction design).
	runtimeState bool
	// observedSequence is the sequence of the last parent-Session event
	// delivered to this Model. It is not the projection's sequence: an adopted
	// Runtime snapshot can legitimately be ahead of the delivered records.
	observedSequence uint64
	// lastModelEventAt is when the parent stream last carried model progress: a
	// new turn, a streaming delta, or a retry notice. The activity row uses it
	// to report a silent model instead of looking merely slow.
	lastModelEventAt    time.Time
	starting            bool
	streamSeq           uint64
	cancelStart         bool
	waiting             bool
	streamErr           error
	composerResolving   bool
	composerResolveSeq  uint64
	composerCancel      context.CancelFunc
	clipboardLoading    bool
	clipboardGeneration uint64
	clipboardCancel     context.CancelFunc
	queued              int
	requests            requestQueue
	canceling           bool
	exitArmed           bool
	bannerPrinted       bool
	// permissionMode caches the effective sandbox mode the Composer's permission
	// row shows. Reading it while rendering would clone the Controller's whole
	// configuration, so it is refreshed when a control result can change it.
	permissionMode config.SandboxMode
	// reasoningLevel caches the effective reasoning level the status line shows
	// beside the model name, for the same reason: Controller.Model() clones the
	// resolved snapshot, and the status line is drawn on every frame.
	// reasoningDeclared records whether the bound model declares any level, so a
	// model with no reasoning knob shows no suffix.
	reasoningLevel    string
	reasoningDeclared bool
	// copiedNotice is the reserved band's copy confirmation. It is separate from
	// statusNotice because a drag copy writes the clipboard alone: there is no
	// file to name, so the status line has nothing to add.
	copiedNotice    string
	copiedNoticeSeq uint64
	// clipboardCopySeq generations the clipboard-only copy's outcome, so a stale
	// result cannot clear a newer selection.
	clipboardCopySeq     uint64
	textSaveSeq          uint64
	statusNotice         string
	statusNoticeErr      bool
	statusNoticeSeq      uint64
	picker               pickerState
	pickerSeq            uint64
	route                routeState
	routeSeq             uint64
	teamProjection       teamProjectionState
	teamInteractions     teamInteractionQueueState
	subagentInteractions subagentInteractionState
	directAgent          *directAgentSelection
	teamPanel            teamPanelState
	presentation         presentationState
	presentationSnapshot presentationSnapshot
	prompt               promptState
	promptSeq            uint64
	completionMarkers    []completionMarker
	// interactionRuns holds the run each interaction is in, keyed by interaction
	// id and bounded to maxCompletionMarkers. Events are delivered contiguously,
	// so the last run an interaction started is its final run even when the
	// Runtime snapshot is ahead of the event being consumed. That run is the
	// stable anchor a completion marker follows; a message count or a content
	// digest cannot tell two identical answers apart.
	interactionRuns    map[string]string
	interactionRunKeys []string
	// questionPreviews holds the cards the user just answered or canceled while
	// the Runtime is still publishing the durable Tool result.
	questionPreviews   map[string]questionPreview
	questionPreviewSeq uint64
	planNoticeSeq      uint64
	worktreeLoading    bool
	worktreeGeneration uint64
	worktreeCancel     context.CancelFunc
	worktreeSummary    string
	activity           activityIndicator
	statusLineItems    []statusline.Item
}

func newModel(ctx context.Context, options Options) *Model {
	if options.Clipboard == nil {
		options.Clipboard = codingclipboard.New()
	}
	lifecycle := lifecycleTrust
	if options.Trusted {
		lifecycle = lifecycleLoading
	}

	editor := textarea.New()
	editor.Prompt = ""
	editor.SetPromptFunc(inputPromptWidth, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return inputArrow + " "
		}

		return ""
	})
	editor.Placeholder = ""
	editor.ShowLineNumbers = false
	editor.DynamicHeight = true
	editor.MinHeight = 1
	editor.MaxHeight = composerMaxLines
	editor.MaxContentHeight = 200
	editor.SetVirtualCursor(false)
	editor.SetWidth(composerEditorWidth(defaultWidth))
	editor.SetStyles(composerStyles(themeDark, options.NoColor))
	composer := newComposerState(editor)
	statusLineItems := options.StatusLine
	if statusLineItems == nil {
		statusLineItems = statusline.Default()
	}

	model := &Model{
		ctx:              ctx,
		options:          options,
		transcriptScroll: newScrollRegion(),
		childScroll:      newScrollRegion(),
		lifecycle:        lifecycle,
		width:            defaultWidth,
		height:           defaultHeight,
		composer:         composer,
		markdown:         newMarkdownRenderer(markdownCacheCapacity),
		theme:            themeDark,
		themeSelection:   config.ThemeAuto,
		themeRegistry:    loadThemeRegistry(""),
		themeIsDark:      true,
		activity:         newActivityIndicator(),
		childStates:      make(map[string]coding.State),
		statusLineItems:  slices.Clone(statusLineItems),
	}
	model.setLayout()

	return model
}

// Init starts bootstrap immediately for an already trusted Workspace.
func (m *Model) Init() tea.Cmd {
	commands := make([]tea.Cmd, 0, 4)
	if !m.options.NoColor {
		commands = append(commands, tea.RequestBackgroundColor)
		// Ask the terminal to report the desktop's light/dark preference whenever it
		// changes. The enable sequence expects no reply, so it is safe to write
		// directly; the extension's one-shot query is not sent because it does expect
		// one, and Bubble Tea only gates its own reply-carrying queries on input being
		// enabled.
		commands = append(commands, tea.Raw(ansi.SetModeLightDark))
	}
	if m.lifecycle == lifecycleLoading {
		commands = append(commands, m.bootstrap(true))
	}
	if command := m.waitBridgeImage(); command != nil {
		commands = append(commands, command)
	}

	return tea.Batch(commands...)
}

// Update applies terminal input and asynchronous bootstrap results.
//
//nolint:funlen,gocyclo // The sealed Tea message union stays visible in one dispatcher.
func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	// Only the deferred-delta path marks the composed view stable again; every
	// other update can change what the frame renders. viewStale intentionally
	// outlives this update: an invalidation that no View call has adopted yet must
	// keep the cache unusable for the updates that follow it.
	m.viewStable = false

	switch message := message.(type) {
	case tea.WindowSizeMsg:
		if message.Width <= 0 || message.Height <= 0 {
			return m, nil
		}
		m.sizeReady = true
		m.width = max(1, message.Width)
		m.height = message.Height
		m.setLayout()
		if !m.startupPublished {
			return m, m.publishStartup()
		}
		m.rerenderTranscript(false)

		return m, nil
	case uv.DarkColorSchemeEvent:
		return m, m.applyAppearance(true)
	case uv.LightColorSchemeEvent:
		return m, m.applyAppearance(false)
	case tea.BackgroundColorMsg:
		// NO_COLOR is a strict no-style mode. Do not let injected or late
		// background messages mutate adaptive-theme state, either: doing so can
		// make a later theme selection depend on terminal probing that was never
		// requested and can invalidate geometry-sensitive rendering tests.
		if m.options.NoColor {
			return m, nil
		}

		m.themeBackgroundKnown = true
		m.themeIsDark = message.IsDark()
		// The detected canvas decides which surfaces may be painted for every
		// selection, so the effective snapshot is re-stamped here too. An explicit
		// theme selection is still never replaced by a background message.
		m.applyTheme(m.themeForSelection(m.themeSelection))

		return m, nil
	case bootstrapResult:
		if message.err != nil {
			m.err = message.err
			var trustErr *TrustError
			if m.allow && !m.options.Trusted && errors.As(message.err, &trustErr) {
				m.lifecycle = lifecycleTrust
			} else {
				m.lifecycle = lifecycleFatal
			}

			return m, nil
		}
		if message.controller == nil {
			m.err = errors.New("coding tui: bootstrap returned no controller")
			m.lifecycle = lifecycleFatal

			return m, nil
		}

		m.controller = message.controller
		m.dropPending()
		m.state = message.controller.Snapshot()
		m.refreshPermissionMode()
		m.refreshReasoningLevel()
		m.resetHistory()
		configuredTUI := message.controller.Config().TUI
		if configuredTUI.StatusLine == nil {
			configuredTUI.StatusLine = statusline.Default()
		}
		m.statusLineItems = slices.Clone(configuredTUI.StatusLine)
		m.loadInitialTheme(configuredTUI.Theme)
		m.resetTeamProjection(m.state.SessionID)
		m.resetScrollback()
		m.lifecycle = lifecycleReady
		m.syncApprovalPrompt()
		m.setLayout()
		return m, m.publishStartup()
	case usageProjectionResultMsg:
		m.applyUsageProjectionResult(message)

		return m, nil
	case subscriptionStartedMsg:
		if message.generation != m.subscriptionSeq {
			if message.bridge != m.subscription {
				message.bridge.stop()
			}

			return m, nil
		}
		activityWasVisible := m.activityClockVisible()
		m.subscriptionMode = message.supported
		m.runtimeState = message.supported
		if message.err != nil {
			m.streamErr = message.err
			m.setLayout()

			return m, m.commitStableTimeline()
		}
		if !message.supported {
			return m, tea.Batch(m.continueIfPaused(), m.refreshTeamProjectionSnapshot())
		}
		if m.subscription != nil {
			m.subscription.stop()
		}

		m.subscription = message.bridge
		previousSessionID := m.state.SessionID
		m.dropPending()
		m.state = message.observation.State
		m.observedSequence = m.state.Sequence
		m.rebaseCompletionMarkers()
		if previousSessionID != m.state.SessionID {
			m.resetRequestQueue()
			m.stopTeamWorkerRouteSubscription()
			m.resetTeamProjection(m.state.SessionID)
			m.resetTasksPane()
		}
		m.childStates = liveChildStates(message.observation.Children)
		m.resetSubagentInteractions()
		m.syncTasksPane(time.Now())
		m.syncApprovalPrompt()
		m.setLayout()
		commit := m.commitStableTimeline()
		wait := tea.Batch(
			message.bridge.wait(),
			m.continueIfPaused(),
			m.refreshTeamProjectionSnapshot(),
			m.loadPausedSubagentControls(),
			m.loadPlanViewIfNeeded(),
			m.startActivityClock(activityWasVisible),
			m.requestAdvanceCommand(),
		)
		return m, m.afterScrollback(commit, wait)
	case subscriptionEventMsg:
		return m.updateSubscription(message)
	case bridgeStartedMsg:
		if message.generation != m.streamSeq {
			message.bridge.once.Do(message.bridge.cancel)

			return m, nil
		}
		m.starting = false
		if m.bridge != nil {
			message.bridge.once.Do(message.bridge.cancel)

			return m, nil
		}

		m.bridge = message.bridge
		m.waiting = true
		m.streamErr = nil
		m.scrollback.streamError = ""
		m.setLayout()
		if m.cancelStart {
			m.cancelStart = false
			m.waiting = false

			return m, tea.Batch(m.stopBridge(message.bridge), m.activity.Tick())
		}

		return m, tea.Batch(message.bridge.wait(), m.activity.Tick())
	case streamItemMsg:
		return m.updateStream(message)
	case requestAdvanceMsg:
		return m, m.advanceRequestQueue(message)
	case composerResolvedMsg:
		if !m.composerResolving || message.generation != m.composerResolveSeq {
			return m, nil
		}
		m.composerResolving = false
		if m.composerCancel != nil {
			m.composerCancel()
			m.composerCancel = nil
		}
		if message.err != nil {
			m.streamErr = message.err
			m.setLayout()

			return m, m.afterScrollback(m.commitStableTimeline(), m.requestAdvanceCommand())
		}

		return m, m.dispatchSubmission(message.kind, message.snapshot, message.message)
	case clipboardImageMsg:
		if !m.clipboardLoading || message.generation != m.clipboardGeneration {
			return m, nil
		}
		if m.clipboardCancel != nil {
			m.clipboardCancel()
			m.clipboardCancel = nil
		}
		m.clipboardLoading = false
		if message.err != nil {
			return m, m.publishRefusal(message.err)
		}

		return m, m.publishRefusal(m.insertComposerImage(message.image))
	case planDocumentMsg:
		return m.applyPlanDocument(message)
	case bridgeImageMsg:
		return m.updateBridgeImage(message)
	case goalControlResultMsg:
		return m.applyGoalControl(message)
	case cancelResultMsg:
		if message.generation != m.streamSeq {
			return m, nil
		}
		if message.beforeStart {
			m.streamErr = errors.Join(m.streamErr, message.err)
			if !m.subscriptionMode {
				m.state = m.controller.Snapshot()
				m.rebaseCompletionMarkers()
			}
			m.syncApprovalPrompt()
			m.clearCompletedDirectAgent()
			m.setLayout()
			return m, m.commitStableTimeline()
		}
		if message.bridge != nil && message.bridge != m.bridge {
			if !m.canceling || m.bridge != nil {
				return m, nil
			}
			m.canceling = false
			m.streamErr = errors.Join(m.streamErr, message.err)
			m.setLayout()

			return m, m.afterScrollback(m.commitStableTimeline(), m.requestAdvanceCommand())
		}
		if message.bridge != nil {
			select {
			case <-message.bridge.stopped():
			default:
				m.canceling = false
				m.streamErr = errors.Join(m.streamErr, message.err)
				m.setLayout()
				commit := m.commitStableTimeline()
				if !m.waiting {
					m.waiting = true

					return m, m.afterScrollback(commit, message.bridge.wait())
				}

				return m, commit
			}
		}
		m.bridge = nil
		m.waiting = false
		m.canceling = false
		m.streamErr = errors.Join(m.streamErr, message.err)
		m.flushPending()
		if !m.subscriptionMode {
			m.state = m.controller.Snapshot()
			m.rebaseCompletionMarkers()
		}
		m.syncApprovalPrompt()
		m.clearCompletedDirectAgent()
		m.finishRequestOperation()
		m.setLayout()
		return m, m.commitStableTimeline()
	case subagentRouteDataMsg:
		if m.route.kind != routeChild || m.route.childKind != childSubagent ||
			message.generation != m.route.generation ||
			message.childSessionID != m.route.childSessionID {
			return m, nil
		}
		if message.background {
			return m.applySubagentRouteRefresh(message)
		}
		m.route.loading = false
		if message.hasState || message.hasDetail {
			m.route.err = nil
		} else {
			m.route.err = message.err
		}
		if message.hasState {
			state := message.state
			m.route.childState = &state
			m.adoptChildState(message.childSessionID, state)
		}
		if message.hasDetail {
			detail := message.detail
			m.route.detail = &detail
		}
		if m.route.refreshPending && m.route.childState != nil {
			m.route.refreshPending = false

			return m, m.refreshSubagentRoute()
		}

		return m, nil
	case subagentControlDataMsg:
		m.applySubagentControlData(message)

		return m, nil
	case subagentControlResultMsg:
		m.applySubagentControlResult(message)

		return m, nil
	case subagentCancelResultMsg:
		if (m.route.kind != routeAgents && m.route.kind != routeChild) ||
			message.generation != m.route.generation {
			return m, nil
		}
		m.route.controlling = false
		m.route.err = message.err

		return m, nil
	case backgroundKillResultMsg:
		return m, m.applyBackgroundKillResult(message)
	case backgroundInspectResultMsg:
		return m, m.applyBackgroundInspectResult(message)
	case teamWorkerRouteDataMsg:
		return m.applyTeamWorkerRouteData(message)
	case teamWorkerRouteEventMsg:
		return m.applyTeamWorkerRouteEvent(message)
	case teamWorkerControlResultMsg:
		return m, m.applyTeamWorkerControl(message)
	case teamPanelControlResultMsg:
		return m, m.applyTeamPanelControl(message)
	case teamRouteResultMsg:
		return m.applyTeamRouteResult(message)
	case teamProjectionRefreshMsg:
		return m, m.applyTeamProjectionRefresh(message)
	case sessionPickerDataMsg:
		if m.route.kind != routeSessions || message.generation != m.route.generation {
			return m, nil
		}
		m.route.loading = false
		m.route.err = message.err
		m.route.sessions = message.sessions
		m.route.sessionRecovery = message.teamRecovery
		if m.route.sessionRecovery == nil {
			m.route.sessionRecovery = make(map[string]runtimecontrol.TeamRecoveryHint)
		}
		m.route.cursor = 0

		return m, nil
	case skillsRouteDataMsg:
		if m.route.kind != routeSkills || message.generation != m.route.generation {
			return m, nil
		}
		m.route.loading = false
		m.route.err = message.err
		m.route.skills = slices.Clone(message.snapshot.Skills)
		m.route.diagnostics = slices.Clone(message.snapshot.Diagnostics)
		m.route.cursor = 0

		return m, nil
	case mcpRouteDataMsg:
		return m, m.applyMCPRouteData(message)
	case statusRouteDataMsg:
		m.applyStatusPanelData(message)

		return m, nil
	case mcpRoutePollMsg:
		return m, m.applyMCPRoutePoll(message)
	case skillToggleResultMsg:
		if m.route.kind != routeSkills || message.generation != m.route.generation {
			return m, nil
		}
		m.route.controlling = false
		m.route.err = message.err
		if message.err != nil {
			return m, nil
		}
		for index := range m.route.skills {
			if m.route.skills[index].ID == message.id {
				m.route.skills[index].Enabled = message.enabled
				break
			}
		}

		return m, nil
	case agentsRouteDataMsg:
		if m.route.kind != routeAgents || message.generation != m.route.generation {
			return m, nil
		}
		m.route.loading = false
		m.route.err = message.err
		m.route.agentLibrary = message.library.Clone().Entries
		views := make([]coding.TeamView, 0, len(message.teamViews))
		for _, view := range message.teamViews {
			merged := m.mergeTeamProjectionView(view)
			m.storeTeamProjectionView(merged)
			views = append(views, merged)
		}
		m.route.children = childSummaries(message.agents, views)
		m.route.cursor = 0
		commands := make([]tea.Cmd, 0, len(views))
		for _, view := range views {
			commands = append(commands, m.reconcileTeamInteractionView(view))
		}

		return m, tea.Batch(commands...)
	case teamInteractionDataMsg:
		return m, m.applyTeamInteractionData(message)
	case teamInteractionControlMsg:
		m.applyTeamInteractionControlResult(message)

		return m, nil
	case treeRouteDataMsg:
		if m.route.kind != routeTree || message.generation != m.route.generation {
			return m, nil
		}
		m.route.loading = false
		m.route.err = message.err
		m.route.tree = message.tree.Clone()
		m.route.cursor = 0

		return m, nil
	case compactPreviewMsg:
		if m.prompt.kind != promptCompact || message.generation != m.prompt.generation {
			return m, nil
		}
		m.prompt.loading = false
		m.prompt.err = message.err
		m.prompt.preview = message.preview

		return m, nil
	case skillPickerDataMsg:
		if m.picker.kind != pickerSkill || message.generation != m.picker.generation {
			return m, nil
		}
		m.picker.loading = false
		m.picker.err = message.err
		m.picker.skills = slices.Clone(message.snapshot.Skills)
		m.picker.diagnostics = len(message.snapshot.Diagnostics)
		m.picker.cursor = 0
		m.setLayout()

		return m, nil
	case filePickerDataMsg:
		if m.picker.kind != pickerFile || message.generation != m.picker.generation {
			return m, nil
		}
		m.picker.loading = false
		m.picker.err = message.err
		m.picker.files = slices.Clone(message.snapshot.Files)
		m.picker.truncated = message.snapshot.Truncated
		m.picker.cursor = 0
		m.setLayout()

		return m, nil
	case statusLineSavedMsg:
		if m.picker.kind != pickerStatusLine || message.generation != m.picker.generation {
			return m, nil
		}
		m.picker.loading = false
		m.picker.controlling = false
		m.picker.err = message.err
		if message.err != nil && !errors.Is(message.err, config.ErrStatusLineDurability) {
			m.setLayout()

			return m, nil
		}
		m.statusLineItems = slices.Clone(message.items)
		if errors.Is(message.err, config.ErrStatusLineDurability) {
			m.setLayout()

			return m, nil
		}
		m.picker = pickerState{}

		return m, m.composer.Focus()
	case themeSavedMsg:
		if m.picker.kind != pickerTheme || message.generation != m.picker.generation {
			return m, nil
		}
		m.picker.loading = false
		m.picker.controlling = false
		m.picker.err = message.err
		if message.err != nil && !errors.Is(message.err, config.ErrThemeDurability) {
			m.setLayout()

			return m, nil
		}
		m.themeRegistry = m.picker.themeRegistry
		m.themeDiagnostics = slices.DeleteFunc(
			slices.Clone(m.picker.themeDiagnostics),
			func(diagnostic themeDiagnostic) bool {
				return diagnostic.category == themeDiagnosticSelection
			},
		)
		m.picker.themeDiagnostics = slices.Clone(m.themeDiagnostics)
		m.themeSelection = message.selection
		m.applyTheme(m.themeForSelection(message.selection))
		if errors.Is(message.err, config.ErrThemeDurability) {
			m.picker.loading = false
			m.picker.controlling = false
			m.picker.themeSelection = message.selection
			m.setLayout()

			return m, nil
		}
		m.picker = pickerState{}

		return m, m.composer.Focus()
	case controlResultMsg:
		pickerControl := m.picker.kind != pickerNone && m.picker.controlling
		routeControl := m.route.kind != routeNone && m.route.controlling
		replacementControl := message.operation != operationReload &&
			message.operation != operationMode
		switch {
		case pickerControl:
			m.picker.loading = false
			m.picker.controlling = false
		case routeControl:
			m.route.loading = false
			m.route.controlling = false
		}
		if replacementControl {
			// Accepted native output survives replacement, but its old session's
			// subscription/focus continuations must not run against the new one.
			for index := range m.presentation.writes {
				m.presentation.writes[index].after = nil
			}
			m.dropPendingScrollback()
			m.presentation.pendingRoute = routeOpenRequest{}
			m.stopTeamWorkerRouteSubscription()
			m.stopSubscription()
			m.dropPending()
			m.state = m.controller.Snapshot()
			m.rebaseCompletionMarkers()
		} else if !m.subscriptionMode {
			m.dropPending()
			m.state = m.controller.Snapshot()
			m.rebaseCompletionMarkers()
		}
		if message.err != nil {
			switch {
			case pickerControl:
				m.picker.err = message.err
			case routeControl:
				m.route.err = message.err
			}
			m.renderTranscript(false)

			if replacementControl {
				return m, m.startSubscription()
			}

			return m, nil
		}
		switch message.operation {
		case operationNew, operationResume, operationFork:
			m.resetRequestQueue()
			m.completionMarkers = nil
			m.transcriptHanded = false
			m.resetTeamProjection(m.state.SessionID)
			m.resetNotices()
			m.resetScrollback()
			m.resetHistory()
		case operationModel, operationReload, operationMode, operationPermissions:
		}
		// A control result can carry a permission change (the picker's apply, a
		// reload, a Full Access confirmation), so the Composer's cached row is
		// refreshed with it instead of on every frame. The same result can have
		// replaced the bound model, so the status line's cached reasoning level is
		// refreshed here too.
		m.refreshPermissionMode()
		m.refreshReasoningLevel()
		switch {
		case pickerControl:
			if m.picker.kind == pickerCommand {
				previous := m.picker.previousComposer
				m.closeCommandPicker(false)
				if message.operation == operationReload || message.operation == operationMode {
					m.restoreCommandComposer(previous)
				}
			} else {
				m.picker = pickerState{}
			}
		case routeControl:
			if m.routeUsesSearch() {
				m.dismissSessionPicker(false)
			} else {
				m.route = routeState{}
			}
		default:
			m.picker = pickerState{}
		}
		m.streamErr = nil
		m.syncApprovalPrompt()
		notice := m.selectionSaveNotice(message)
		var commit tea.Cmd
		switch message.operation {
		case operationNew:
			commit = m.commitNewSessionOutput()
		case operationResume:
			if m.fullscreen() {
				// A resumed session starts with the banner as its first managed
				// record, so scrolling to the top shows the same header a fresh
				// session shows at startup.
				m.appendBanner(m.renderBanner())
			}
			commit = m.commitStableTimeline()
		default:
			commit = m.commitStableTimeline()
		}

		if replacementControl {
			commands := []tea.Cmd{m.startSubscription(), notice}
			if message.operation == operationResume {
				commands = append(commands, m.probeTeamRecoveryAfterResume())
			}

			return m, m.afterScrollback(commit, commands...)
		}

		return m, m.afterScrollback(commit, m.continueIfPaused(), notice)
	case teamRecoveryProbeMsg:
		if message.err != nil || message.sessionID == "" ||
			message.sessionID != m.state.SessionID || len(message.values) == 0 ||
			m.route.kind != routeNone || m.picker.kind != pickerNone ||
			m.prompt.kind != promptNone {
			return m, nil
		}

		return m, m.activateTeamRecoveryReview(message.values)
	case workspaceStatusResultMsg:
		if message.generation != m.worktreeGeneration {
			return m, nil
		}
		if m.worktreeCancel != nil {
			m.worktreeCancel()
			m.worktreeCancel = nil
		}
		m.worktreeLoading = false
		if m.route.kind == routeStatus {
			m.applyStatusRepositoryData(message)

			return m, nil
		}
		// The read outlived its surface: keep the loaded summary for the next
		// /status, and drop a failure no surface can act on.
		if message.err == nil {
			m.worktreeSummary = compactWorktreeSummary(message.status)
		}

		return m, nil
	case exitResetMsg:
		m.exitArmed = false

		return m, nil
	case statusNoticeExpiredMsg:
		if message.generation != m.statusNoticeSeq {
			return m, nil
		}
		m.statusNotice = ""
		m.statusNoticeErr = false
		m.setLayout()

		return m, nil
	case copiedNoticeExpiredMsg:
		if message.generation != m.copiedNoticeSeq {
			return m, nil
		}
		m.copiedNotice = ""
		m.setLayout()

		return m, nil
	case frameRedrawMsg:
		return m, nil
	case textSavedMsg:
		if message.generation != m.textSaveSeq {
			return m, nil
		}
		if message.err != nil {
			return m, m.setStatusError(
				"could not write " + string(message.kind) + ": " + safeError(message.err),
			)
		}

		return m, m.setStatusNotice(
			textSavedNotice(message.kind, message.path, message.clipboard),
		)
	case clipboardResultMsg:
		// An error is always worth reporting, whichever copy produced it; the
		// success effects belong to the drag gesture the reader just made.
		if message.err != nil {
			return m, m.setStatusError("could not copy: " + safeError(message.err))
		}
		if !message.selection || message.generation != m.clipboardCopySeq {
			return m, nil
		}
		m.clearSelection()
		m.setLayout()

		return m, m.showCopiedNotice(message.lines)
	case tea.MouseWheelMsg:
		return m, m.scrollWheel(message)
	case tea.MouseMsg:
		return m, nil
	case tea.PasteMsg:
		if m.lifecycle != lifecycleReady || !m.sizeReady {
			return m, nil
		}
		if m.searchOwnsKeys() {
			return m, m.updateSearchPaste(message)
		}
		if m.routeUsesSearch() {
			var command tea.Cmd
			m.route.search, command = m.route.search.Update(message)

			return m, command
		}
		if m.route.kind == routeTeam && m.route.team != nil &&
			teamRouteInputStage(m.route.team.stage) {
			_, err := m.composer.InsertPaste(message.Content)
			m.setLayout()

			return m, m.publishRefusal(err)
		}
		if m.route.kind == routeChild && m.route.childKind == childTeamWorker {
			_, err := m.composer.InsertPaste(message.Content)
			m.setLayout()

			return m, m.publishRefusal(err)
		}
		if m.route.kind != routeNone || m.prompt.kind != promptNone || m.composerResolving ||
			m.clipboardLoading ||
			m.picker.kind != pickerNone {
			return m, nil
		}

		_, err := m.composer.InsertPaste(message.Content)
		m.setLayout()

		return m, m.publishRefusal(err)
	case tea.KeyPressMsg:
		// A keystroke can remove the attachment a refusal was about, so the notice is
		// re-checked against the draft the key just changed.
		model, command := m.updateKey(message)
		if commit := m.reconcileComposerRefusal(); commit != nil {
			return model, m.afterScrollback(commit, command)
		}

		return model, command
	case renderTickMsg:
		m.renderWait = false
		var commit tea.Cmd
		if m.renderDirty || len(m.pending) > 0 {
			// The frame boundary: the deltas accumulated since the last tick
			// advance the parent state once, and the frame is projected once.
			m.flushPending()
			m.setLayout()
			commit = m.commitStableTimeline()
		}
		m.refreshToolDetailRoute()

		return m, tea.Batch(commit, m.armLiveWork())
	case liveRenderReadyMsg:
		return m, m.acceptLiveRender(message.result)
	case liveCatchUpMsg:
		if message.generation != m.liveCatchUpGen {
			return m, nil
		}
		m.liveCatchUpWait = false
		if _, stale := m.markdown.liveCatchUpDelay(); !stale {
			return m, nil
		}
		// The stalled frame must reach the newest content, so this render pays for
		// the tail rather than serving the frame it already has. A background
		// render in flight already owns that work, and forcing here would compete
		// with it on the event loop.
		if !m.liveRender.pending() {
			m.markdown.forceExact = true
		}
		m.rerenderTranscript(false)

		return m, m.armLiveWork()
	case scrollbackWriteDoneMsg:
		return m, m.finishScrollbackWrite(message.sequence)
	case transcriptPageMsg:
		return m, m.updateTranscriptPage(message)
	case historyPageMsg:
		m.applyHistoryPage(message)

		return m, nil
	case activityTickMsg:
		if !m.activityClockVisible() {
			return m, nil
		}

		// The clock is also what re-reads a running watch's output, so a page
		// that is following one asks for the next read on every frame.
		return m, tea.Batch(m.activity.Update(message), m.followOpenWatchPage())
	default:
		if m.lifecycle == lifecycleReady {
			// The find box has focus while it is open, so its cursor blink and any
			// other input message belong to it rather than to the hidden composer.
			if m.searchOwnsKeys() {
				var command tea.Cmd
				m.search.input, command = m.search.input.Update(message)

				return m, command
			}
			if m.route.kind == routeSessions {
				var command tea.Cmd
				m.route.search, command = m.route.search.Update(message)

				return m, command
			}
			var command tea.Cmd
			m.composer, command = m.composer.Update(message)
			m.setLayout()

			return m, command
		}

		return m, nil
	}
}

// View composes the frame the renderer paints. The framework calls it after
// every message, and a deferred delta changes none of its inputs, so the
// composition of those updates is reused instead of repeated. A delivery the
// subscription already owns is reused the same way.
func (m *Model) View() tea.View {
	// Reuse requires a composition that still describes the current state: one an
	// invalidation outside an Update took away is composed again, not resurrected.
	if m.viewStable && m.viewCached && !m.viewStale {
		return m.viewCache
	}

	m.viewComposes++
	m.viewCache = m.composeView()
	m.viewCache.ProgramStatus = m.programStatus()
	m.viewCached = true
	m.viewStale = false

	return m.viewCache
}

// composeView builds the frame: the inline lifecycle shell, or the ready chat
// layout.
func (m *Model) composeView() tea.View {
	// The per-model tally is a projection of state, so it observes state wherever a
	// frame observes it: every state change goes through a composition.
	m.modelUsage.observe(m.state)
	if m.lifecycle == lifecycleReady && !m.sizeReady {
		// A frame composed before the terminal reports its size would lay the
		// ready layout out against placeholder geometry, so this waits for that
		// size with nothing drawn rather than a transient label.
		return tea.NewView("")
	}
	var content string

	switch m.lifecycle {
	case lifecycleTrust:
		content = m.trustView()
	case lifecycleLoading:
		// The banner and any stable history are published once the frame is
		// ready, so the loading lifecycle draws nothing rather than leaving a
		// transient label where that output belongs.
	case lifecycleReady:
		return m.readyView()
	case lifecycleFatal:
		content = fmt.Sprintf(
			"Pips could not start\n\n%s\n\nPress q to quit",
			safeError(m.err),
		)
	}

	view := tea.NewView(content)
	view.AltScreen = false
	view.MouseMode = tea.MouseModeNone
	view.WindowTitle = appTitle

	return view
}

func (m *Model) updateKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := message.String()
	if m.lifecycle == lifecycleReady && !m.sizeReady {
		if key == keyCtrlC {
			return m, tea.Quit
		}

		return m, nil
	}

	switch m.lifecycle {
	case lifecycleTrust:
		return m.updateTrustKey(key)
	case lifecycleLoading:
		return m, nil
	case lifecycleReady, lifecycleFatal:
		if m.lifecycle == lifecycleFatal && (key == "q" || key == keyCtrlC) {
			return m, tea.Quit
		}
		if m.lifecycle == lifecycleReady {
			return m.updateReadyKey(message)
		}
	}

	return m, nil
}

func (m *Model) updateTrustKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "up":
		m.allow = true
	case "down":
		m.allow = false
	case keyLeft, keyRight, keyTab:
		m.allow = !m.allow
	case "a":
		m.allow = true

		return m.confirmTrust()
	case "d", keyEscape:
		m.allow = false

		return m.confirmTrust()
	case keyEnter:
		return m.confirmTrust()
	case keyCtrlC, "q":
		return m, tea.Quit
	}

	return m, nil
}

func (m *Model) trustView() string {
	title := "Pips needs your trust decision"
	workspaceLabel := "Workspace:"
	trustWord := "Trust"
	projectRoot := ".pips"
	help := "Up/Down choose • Enter confirm • a allow • d deny"
	if !m.options.NoColor {
		palette := paletteFor(m.theme)
		accent := palette.session
		muted := palette.muted
		title = lipgloss.NewStyle().Bold(true).Foreground(accent).Render(title)
		workspaceLabel = lipgloss.NewStyle().Bold(true).Render(workspaceLabel)
		trustWord = lipgloss.NewStyle().Bold(true).Foreground(accent).Render(trustWord)
		projectRoot = lipgloss.NewStyle().Bold(true).Foreground(accent).Render(projectRoot)
		help = lipgloss.NewStyle().Bold(true).Foreground(muted).Render(help)
	}

	content := strings.Join([]string{
		title,
		"",
		workspaceLabel + " " + m.options.Workspace,
		"",
		trustWord + " enables this project's " + projectRoot + " resources.",
		"It does not load project config or approve tools, MCP servers, or full access.",
		"",
		m.trustChoice("Allow", "Enable project Skills, Bundles, MCP, and permissions", m.allow),
		m.trustChoice("Deny", "Continue without trusting this workspace", !m.allow),
		"",
		help,
	}, "\n")
	content += trustErrorMessage(m.err)

	width := max(1, m.width)
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		lines[index] = ansi.Truncate(line, width, "…")
	}

	return strings.Join(lines, "\n")
}

func (m *Model) trustChoice(label, description string, selected bool) string {
	marker := " "
	if selected {
		marker = ">"
	}
	line := fmt.Sprintf("%s %-5s %s", marker, label, description)
	if m.options.NoColor {
		return line
	}

	color := paletteFor(m.theme).session
	if !selected {
		color = paletteFor(m.theme).muted
	}

	return lipgloss.NewStyle().Bold(true).Foreground(color).Render(line)
}

//nolint:gocyclo,nestif,gocritic // Contextual input precedence is an explicit product state machine.
func (m *Model) updateReadyKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.composerResolving {
		if key := message.String(); key == keyEscape || key == keyCtrlC {
			if m.composerCancel != nil {
				m.composerCancel()
			}
			m.composerCancel = nil
			m.composerResolving = false
			m.composerResolveSeq++
			m.setLayout()

			return m, m.afterScrollback(m.commitStableTimeline(), m.requestAdvanceCommand())
		}

		return m, nil
	}
	if m.clipboardLoading {
		if key := message.String(); key == keyEscape || key == keyCtrlC {
			m.cancelClipboardImageRead()
		}

		return m, nil
	}
	// The escape hatch owns every key while it is open: the terminal has the
	// mouse and scroll keys back, so there is no composer to feed here.
	if m.transcriptMode.active {
		return m.updateTranscriptModeKey(message)
	}
	inlineTeamRoute := m.teamRouteIsInline()
	if m.route.kind != routeNone && !inlineTeamRoute {
		return m.updateRouteKey(message)
	}
	if m.state.Goal.ID != "" && message.String() == "ctrl+k" && m.picker.kind == pickerNone {
		m.openCommandPicker()
		m.picker.query = "goal "
		m.syncCommandInput()
		return m, nil
	}
	if m.goalCommandPickerActive() {
		return m.updatePickerKey(message)
	}
	if m.prompt.kind != promptNone {
		return m.updatePromptKey(message)
	}
	if m.picker.kind != pickerNone {
		return m.updatePickerKey(message)
	}
	if inlineTeamRoute {
		return m.updateTeamRouteKey(message)
	}
	if command, handled := m.updateTeamPanelKey(message); handled {
		return m, command
	}
	// The tasks pane is an optional layer between the pickers and the reading
	// surfaces: it is revealed and focused by one chord, and once focused it owns
	// the frame's keys the way any other focused overlay does.
	if model, command, handled := m.updateTasksPaneReadyKey(message); handled {
		return model, command
	}
	// The find box owns the ready view's keys while it is open: it is a reading
	// surface, so the composer, its history and the command picker stay untouched
	// until it closes.
	if m.searchOwnsKeys() {
		return m.updateSearchKey(message)
	}
	key := message.String()
	// Escape ends the gesture the reader just made, the way the find box does: a drag
	// selection or a whole-draft selection answers it before the cancel/exit binding
	// behind it.
	if command, handled := m.dismissReadingGesture(key); handled {
		return m, command
	}
	if key == keyEscape && m.directAgent != nil && !m.directAgent.running &&
		m.composer.Value() == "" {
		m.directAgent = nil
		m.setLayout()

		return m, nil
	}
	if key == "up" && !m.composer.SelectionActive() && m.composer.AtFirstVisualRow() && m.composer.HistoryUp() {
		m.setLayout()

		return m, nil
	}
	if key == keyDown && !m.composer.SelectionActive() && m.composer.AtLastVisualRow() && m.composer.HistoryDown() {
		m.setLayout()

		return m, nil
	}
	if command, handled := m.updateTranscriptScrollKey(message); handled {
		return m, command
	}
	if command, handled := m.updateTurnRailKey(message); handled {
		return m, command
	}
	if m.worktreeLoading && (key == keyCtrlC || key == keyEscape) {
		m.cancelWorkspaceStatus()

		return m, nil
	}
	context := m.actionContext()
	action, matched := resolveAction(defaultActions, context, key)
	if key == "/" && m.composer.Value() != "" {
		matched = false
	}
	if matched {
		switch action {
		case actionNewline:
			m.composer.InsertString("\n")
			m.setLayout()
		case actionCancel:
			return m.cancelOrExit(context)
		case actionTranscript:
			return m, m.toggleTranscriptMode()
		case actionCopy:
			return m, m.copyAssistant(1, "")
		case actionSearch:
			if !m.fullscreen() {
				return m, m.setStatusNotice(
					"search needs the managed viewport; the terminal owns the conversation inline",
				)
			}

			m.openSearch("")

			return m, nil
		case actionTodo:
			return m, m.openTodoDetailRoute()
		case actionSubmit:
			return m, m.submit(context)
		case actionFollowUp:
			return m, m.queueMessage()
		case actionToggleTool:
			return m, m.toggleLatestTool()
		case actionToggleMouseCapture:
			return m, m.toggleMouseCapture()
		case actionToggleMode:
			mode := coding.ModePlan
			if m.controller.Mode().Current == coding.ModePlan {
				mode = coding.ModeAgent
			}

			return m, m.runModeControl(mode)
		case actionPasteImage:
			return m, m.readClipboardImage()
		case actionSelectAll:
			return m, m.selectAllDraft()
		case actionCommand, actionHelp:
			if action == actionCommand {
				m.openCommandPicker()

				return m, nil
			}

			return m, m.openHelp()
		}

		return m, nil
	}

	var command tea.Cmd
	m.composer, command = m.composer.Update(message)
	if context == contextIdle && message.Key().Text == "$" && m.canOpenSkillPickerAtCursor() {
		return m, tea.Batch(command, m.openSkillPickerAfterDollar())
	}
	if message.Key().Text == "@" && m.canOpenFilePickerAtCursor() {
		return m, tea.Batch(command, m.openFilePickerAfterAt())
	}
	m.setLayout()

	return m, command
}

// cancelOrExit is the shared meaning of Ctrl+C: stop the running turn, clear a
// draft, or arm and then perform the quit.
func (m *Model) cancelOrExit(context actionContext) (tea.Model, tea.Cmd) {
	if context == contextRunning {
		return m, m.cancelStream()
	}
	if m.composer.Value() != "" {
		m.composer.Reset()
		m.setLayout()
		m.exitArmed = false

		return m, nil
	}
	if m.exitArmed {
		return m, tea.Quit
	}
	m.exitArmed = true

	return m, tea.Tick(exitConfirmTime, func(time.Time) tea.Msg {
		return exitResetMsg{}
	})
}

//nolint:gocyclo,funlen // Footer composition follows the explicit route/prompt/picker state machine.
func (m *Model) readyView() tea.View {
	if m.transcriptMode.active {
		return m.transcriptModeView()
	}
	if m.route.kind != routeNone && !m.teamRouteIsInline() {
		return m.routeView()
	}

	// A focused rail mark is only current while the reading position sits where the
	// step left it; settle it before the frame resolves the rail's window.
	m.settleTurnRailFocus()

	caps := m.layout()
	footer := make([]string, 0, 5)
	promptFooterIndex := -1
	promptContent := ""
	if activity := m.activityLine(); activity != "" && caps.activity {
		for range conversationGapHeight {
			footer = append(footer, "")
		}
		footer = append(footer, activity)
	}
	promptHit := questionPromptHit{top: -1}
	if prompt, hit := m.promptView(); prompt != "" && !m.goalCommandPickerActive() {
		// The band shares the Composer's text column, so its accent gutter lines
		// up with the transcript's accented blocks instead of the frame edge.
		prompt = insetRows(prompt, timelineInset(m.width))
		if caps.promptRows > 0 {
			if len(hit.rows) > caps.promptRows {
				hit.cropTop(len(hit.rows) - caps.promptRows)
			}
			prompt = truncateTailHeight(prompt, caps.promptRows)
		} else {
			prompt = ""
			hit = questionPromptHit{top: -1}
		}
		promptFooterIndex = len(footer)
		promptContent = prompt
		promptHit = hit
		footer = append(footer, prompt)
	}
	if inline := m.inlineTeamRouteView(); inline != "" {
		footer = append(footer, truncateHeight(inline, max(1, caps.panelRows)))
	}
	if notice := m.directAgentNotice(); notice != "" {
		footer = append(footer, truncateHeight(notice, 1))
	}
	// The reserved band occupies the row the frame used to leave blank here, so
	// the Composer's position does not depend on whether the band has anything to
	// say. It is part of the footer, which means it is already counted in the
	// transcript's elastic height below. Whether the band affords a click is
	// decided here, with the contents it draws, and recorded below.
	bandHint := m.bandScrollHint()
	bandFooterIndex := len(footer)
	footer = append(footer, m.reservedBand())
	composerFooterIndex := len(footer)
	footer = append(footer, m.composerBand())
	// The pane is the frame's last band, below the status line or below the picker
	// that replaces it. Its rows are measured here, before the picker sizes itself,
	// so a picker opened while something is running cannot push the pane out of the
	// frame.
	pane := m.tasksPaneBand(time.Now())
	if m.picker.kind != pickerNone {
		usedHeight := lipgloss.Height(lipgloss.JoinVertical(lipgloss.Left, footer...)) +
			lipgloss.Height(pane)
		availableRows := m.pickerBandRows(caps, m.height-usedHeight)
		switch m.picker.kind {
		case pickerModel:
			footer = append(footer, m.pickerView(availableRows))
		case pickerMode:
			footer = append(footer, m.modePickerView(availableRows))
		case pickerPermissions:
			footer = append(footer, m.permissionsPickerView(availableRows))
		case pickerStatusLine:
			footer = append(footer, m.statusLinePickerView(availableRows))
		case pickerTheme:
			footer = append(footer, m.themePickerView(availableRows))
		case pickerSkill:
			footer = append(footer, m.skillPickerView(availableRows))
		case pickerFile:
			footer = append(footer, m.filePickerView(availableRows))
		default:
			footer = append(footer, m.commandPickerView(availableRows))
		}
		footer[len(footer)-1] = truncateHeight(footer[len(footer)-1], availableRows)
	} else {
		footer = append(footer, m.statusLineView())
		if panel := m.teamPanelView(); panel != "" {
			footer = append(footer, truncateHeight(panel, max(1, caps.panelRows)))
		}
	}

	// The pane is appended last, so the bands above it keep their positions whatever
	// it lists; its rows come from the transcript's elastic height.
	tasksPaneIndex := -1
	if pane != "" {
		tasksPaneIndex = len(footer)
		footer = append(footer, pane)
	}

	parts := make([]string, 0, len(footer)+1)
	timelineHeight := max(
		0,
		m.height-lipgloss.Height(lipgloss.JoinVertical(lipgloss.Left, footer...)),
	)
	// The transcript region is the only elastic band: it receives whatever the
	// fixed bands leave. Its window is owned here rather than by the terminal, so
	// a scrolled-up reader keeps the same rows when new output arrives.
	transcriptRows := 0
	if timeline := m.transcriptWindow(timelineHeight); timeline != "" {
		transcriptRows = lipgloss.Height(timeline)
		parts = append(parts, timeline)
	}
	composerIndex := len(parts) + composerFooterIndex
	bandIndex := len(parts) + bandFooterIndex
	tasksPaneFrameIndex := -1
	if tasksPaneIndex >= 0 {
		tasksPaneFrameIndex = len(parts) + tasksPaneIndex
	}
	promptIndex := -1
	if promptFooterIndex >= 0 {
		promptIndex = len(parts) + promptFooterIndex
	}
	parts = append(parts, footer...)
	composerOffset := lipgloss.Height(lipgloss.JoinVertical(
		lipgloss.Left,
		parts[:composerIndex]...,
	))
	content := lipgloss.JoinVertical(lipgloss.Left, parts...)
	// An over-tall frame keeps its tail, so whatever is dropped off the top is
	// transcript rows and the hit map has to account for them.
	dropped := clampFrameTailOffset(content, m.height)
	transcriptVisible := max(0, transcriptRows-min(dropped, transcriptRows))
	// The rail fills the margin the transcript band leaves blank. It is painted on
	// the clamped frame, so its marks and the pointer map they carry are already in
	// the frame's own coordinates.
	content, railHits := m.drawTurnRail(clampFrameTail(content, m.height), transcriptVisible)
	view := m.presentationView(content)
	promptTop := frameRowOf(parts, promptIndex, dropped)
	promptHit.place(promptTop, timelineInset(m.width))
	m.frameHit = frameHitMap{
		painted:      m.sizeReady,
		transcript:   transcriptVisible,
		topDropped:   min(dropped, transcriptRows),
		band:         m.bandRowOf(parts, bandIndex, dropped, bandHint != ""),
		turnRail:     railHits,
		question:     promptHit,
		composerTop:  frameRowOf(parts, composerIndex, dropped),
		composerRows: lipgloss.Height(footer[composerFooterIndex]),
	}
	m.tasksPaneHit.top = frameRowOf(parts, tasksPaneFrameIndex, dropped)
	// presentationView installs the pointer handler for every frame, so the ready
	// view only has to record where its transcript band sat.
	view.Cursor = m.composer.Cursor()
	if m.searchOwnsKeys() {
		view.Cursor = m.search.input.Cursor()
	}
	if (m.prompt.kind != promptNone && !m.goalCommandPickerActive()) || m.pickerHidesComposerCursor() || m.teamPanel.isFocused ||
		m.tasksPaneFocused() ||
		(m.teamRouteIsInline() && (m.route.loading || !teamRouteInputStage(m.route.team.stage))) {
		view.Cursor = nil
	}
	// The prompt bands sit above the Composer, so their caret is already measured
	// against the frame. Applying the Composer's box offset to it would drag it
	// down onto the Composer's row, which is where the caret wrongly landed while
	// a question editor was open.
	cursorPlaced := false
	if m.prompt.kind == promptQuestion && promptHit.cursor && promptHit.top >= 0 {
		if cursor := m.prompt.question.editor.Cursor(); cursor != nil {
			cursor.X = promptHit.cursorX
			cursor.Y = promptHit.cursorY
			view.Cursor = cursor
			cursorPlaced = true
		}
	}
	if !cursorPlaced {
		cursorPlaced = m.placePlanReviewCursor(&view, promptTop, promptContent)
	}
	if view.Cursor != nil && !cursorPlaced {
		cursorX, cursorY := m.composerBoxCursorOffset()
		view.Cursor.X += cursorX
		view.Cursor.Y += composerOffset + cursorY - clampFrameTailOffset(content, m.height)
	}

	return view
}

func (m *Model) pickerHidesComposerCursor() bool {
	switch m.picker.kind {
	case pickerModel, pickerMode, pickerPermissions, pickerStatusLine, pickerTheme:
		return true
	default:
		return false
	}
}

func editorOffset(prompt, editor string) (int, int, bool) {
	before, _, ok := strings.Cut(prompt, editor)
	if !ok {
		return 0, 0, false
	}
	prefix := before
	lineStart := strings.LastIndex(prefix, "\n") + 1

	return ansi.StringWidth(prefix[lineStart:]), strings.Count(prefix, "\n"), true
}

func (m *Model) statusLine() string {
	return m.statusLineWithItems(m.statusLineItems)
}

func (m *Model) statusLineWithItems(items []statusline.Item) string {
	width := m.statusLineWidth()
	type segment struct {
		item  statusline.Item
		value string
	}
	segments := make([]segment, 0, len(items))
	for _, item := range items {
		if value := m.statusLineItem(item, width); value != "" {
			segments = append(segments, segment{item: item, value: value})
		}
	}
	separator := "  ·  "
	if !m.options.NoColor {
		separator = lipgloss.NewStyle().Foreground(paletteFor(m.theme).muted).Render(separator)
	}

	rightStart := len(segments)
	for rightStart > 0 {
		item := segments[rightStart-1].item
		if item != statusline.Mode && item != statusline.Team {
			break
		}
		rightStart--
	}
	leftSegments := segments[:rightStart]
	left := make([]string, 0, len(leftSegments)+3)
	phaseIndex := -1
	for index, segment := range leftSegments {
		left = append(left, segment.value)
		if segment.item == statusline.Phase {
			phaseIndex = index
		}
	}
	extras := m.statusExtras()
	right := make([]string, 0, len(segments)-rightStart)
	for _, segment := range segments[rightStart:] {
		right = append(right, segment.value)
	}
	if len(segments)-rightStart == 2 && segments[rightStart].item == statusline.Mode &&
		segments[rightStart+1].item == statusline.Team {
		right = []string{combinedStatusRight(
			m.state.Mode,
			m.teamStatusLabel(width),
			width,
		)}
	}
	leftValue := strings.Join(append(left, extras...), separator)
	if phaseIndex > 0 {
		leftValue = fitStatusLeft(
			strings.Join(left[:phaseIndex], separator),
			strings.Join(append(slices.Clone(left[phaseIndex:]), extras...), separator),
			separator,
			width,
		)
	}
	if len(right) == 0 {
		return ansi.Truncate(leftValue, width, "…")
	}

	return alignStatusLine(leftValue, strings.Join(right, separator), width)
}

// modelNameTokens pins the spelling of vendor tokens that plain capitalization
// would render wrong. Everything else is capitalized from the id as written.
var modelNameTokens = map[string]string{
	"deepseek": "DeepSeek",
	"gpt":      "GPT",
	"glm":      "GLM",
	"vl":       "VL",
}

// modelDisplayName names the active model the way the reference status line
// does: a short display name instead of provider/model. The provider prefix and
// any nested catalog path are dropped, so clinepass plus
// cline-pass/deepseek-v4.1-flash reads as DeepSeek V4.1 Flash.
func modelDisplayName(provider ai.Provider, modelID string) string {
	name := strings.TrimSpace(modelID)
	if name == "" {
		return string(provider)
	}

	if index := strings.LastIndex(name, "/"); index >= 0 {
		name = name[index+1:]
	}

	tokens := strings.FieldsFunc(name, func(character rune) bool {
		return character == '-' || character == '_'
	})
	words := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if word, ok := modelNameTokens[strings.ToLower(token)]; ok {
			words = append(words, word)

			continue
		}

		words = append(words, capitalizeFirst(token))
	}
	if len(words) == 0 {
		return string(provider)
	}

	return strings.Join(words, " ")
}

// capitalizeFirst upper-cases the first rune of a model token and keeps the rest
// as written, so v4.1 stays a version rather than becoming a word.
func capitalizeFirst(token string) string {
	first, size := utf8.DecodeRuneInString(token)
	if size == 0 {
		return token
	}

	return string(unicode.ToUpper(first)) + token[size:]
}

// promptCacheHitRate reports the share of prompt tokens the provider served from
// its cache, as a whole percentage. Input tokens already include the cached
// ones, so the rate is cached over input; false means the caller has no reported
// prompt size to divide by.
func promptCacheHitRate(usage coding.TokenUsage) (int, bool) {
	if usage.InputTokens <= 0 {
		return 0, false
	}

	cached := min(max(usage.CachedInputTokens, 0), usage.InputTokens)

	return int(float64(cached) / float64(usage.InputTokens) * 100), true
}

//nolint:gocyclo // Each closed status field has one visible formatting branch.
func (m *Model) statusLineItem(item statusline.Item, width int) string {
	workspaceName := filepath.Base(m.options.Workspace)
	if workspaceName == "." || workspaceName == string(filepath.Separator) {
		workspaceName = m.options.Workspace
	}

	sessionLabel := m.state.SessionID
	if m.state.IsSessionProvisional() {
		sessionLabel = "new"
	}
	phase := m.effectivePhase()
	phaseLabel := string(phase)
	if m.composerResolving {
		phaseLabel = "resolving files"
	} else if m.clipboardLoading {
		phaseLabel = "reading clipboard"
	}
	if m.state.LastError != nil && m.state.Interaction.Outcome != coding.InteractionCanceled {
		phaseLabel = "error"
	}

	value := ""
	style := lipgloss.NewStyle()
	palette := paletteFor(m.theme)
	switch item {
	case statusline.Workspace:
		value = workspaceName
		style = style.Bold(true).Foreground(palette.workspace)
	case statusline.Session:
		value = sessionLabel
		style = style.Bold(true).Foreground(palette.session)
	case statusline.Model:
		value = m.modelStatusValue()
		style = style.Foreground(palette.model)
	case statusline.ContextUsed:
		window := m.state.ContextWindow
		if window > 0 {
			percent := min(100, max(0, int(
				(float64(m.state.ContextTokens)/float64(window))*100,
			)))
			value = fmt.Sprintf("%d%% ctx", percent)
			style = style.Foreground(palette.model)
		}
	case statusline.CacheHitRate:
		if percent, ok := promptCacheHitRate(m.state.Interaction.Usage); ok {
			value = fmt.Sprintf("%d%% cache", percent)
			style = style.Foreground(palette.model)
		}
	case statusline.TodoProgress:
		// The strip is a progress cue, not a completion badge: once every item
		// is terminal there is no work left to track, so the item disappears.
		// The full checklist remains available through Ctrl+P and /todos.
		if m.state.Tasks.Total > 0 &&
			m.state.Tasks.Completed+m.state.Tasks.Cancelled < m.state.Tasks.Total {
			value = fmt.Sprintf("Todo %d/%d", m.state.Tasks.Completed, m.state.Tasks.Total)
			style = style.Foreground(palette.workspace)
		}
	case statusline.Phase:
		value = phaseLabel
		style = style.Bold(true).Foreground(phaseColor(m.state, phase, palette))
	case statusline.Mode:
		value = statusModeLabel(m.state.Mode, width)
		style = style.Bold(true).Foreground(palette.session)
	case statusline.Team:
		value = m.teamStatusLabel(width)
		style = style.Bold(true).Foreground(palette.workspace)
	}
	if value == "" || m.options.NoColor {
		return value
	}

	return style.Render(value)
}

func statusModeLabel(mode coding.OperatingMode, width int) string {
	if mode != coding.ModePlan {
		return ""
	}

	switch {
	case width >= 64:
		return "Plan mode (shift+tab to cycle)"
	case width >= 24:
		return "Plan mode"
	default:
		return "plan"
	}
}

func (m *Model) statusLineView() string {
	inset := min(statusHorizontalInset, max(0, (m.width-1)/2))

	return strings.Repeat(" ", inset) + m.statusLine()
}

func (m *Model) statusLineWidth() int {
	inset := min(statusHorizontalInset, max(0, (m.width-1)/2))

	return max(1, m.width-(inset*2))
}

func alignStatusLine(left, right string, width int) string {
	rightWidth := ansi.StringWidth(right)
	if rightWidth >= width {
		return ansi.Truncate(right, width, "…")
	}

	left = ansi.Truncate(left, width-rightWidth-1, "…")
	padding := max(1, width-ansi.StringWidth(left)-rightWidth)

	return left + strings.Repeat(" ", padding) + right
}

func fitStatusLeft(head, tail, separator string, width int) string {
	if width <= 0 {
		return ""
	}

	full := head + separator + tail
	if ansi.StringWidth(full) <= width {
		return full
	}

	tailWidth := ansi.StringWidth(tail)
	separatorWidth := ansi.StringWidth(separator)
	if tailWidth+separatorWidth >= width {
		return ansi.Truncate(tail, width, "…")
	}

	head = ansi.Truncate(head, width-tailWidth-separatorWidth, "…")

	return head + separator + tail
}

func (m *Model) statusExtras() []string {
	extras := make([]string, 0, 5)
	// A copy or export confirmation is transient, so it leads and a narrow
	// terminal truncates the standing hints instead of the outcome.
	notice := strings.TrimSpace(sanitizeInspectionText(m.statusNotice))
	if notice != "" {
		if !m.options.NoColor {
			color := paletteFor(m.theme).muted
			if m.statusNoticeErr {
				color = paletteFor(m.theme).error
			}
			notice = lipgloss.NewStyle().Foreground(color).Render(notice)
		}
		extras = append(extras, notice)
	}
	// The find box owns the composer's band, so its state is reported here, where
	// the match counter stays visible while the query is typed.
	if m.searchOwnsKeys() {
		extras = append(extras, m.searchStatusText())
	}
	if m.state.Goal.ID != "" {
		extras = append(extras, "goal "+sanitizeInspectionText(string(m.state.Goal.Status)))
	}
	if m.queued > 0 {
		extras = append(extras, fmt.Sprintf("%d queued", m.queued))
		if m.requests.paused {
			extras = append(extras, "queue paused · /queue resume or discard")
		}
	}
	// While the runtime is idle and something is still running, the cue stands
	// in for the tasks pane the automatic rule may have closed (or that a short
	// terminal never paints).
	if cue := m.stillRunningCue(); cue != "" {
		extras = append(extras, cue)
	}
	if m.exitArmed {
		extras = append(extras, "press Ctrl+C again to quit")
	}
	if m.worktreeLoading {
		extras = append(extras, "inspecting Git")
	}
	// A history read is transient and reports its own failure, so it is stated
	// before the standing scroll hint that a narrow terminal would otherwise keep.
	switch {
	case m.history.loading:
		extras = append(extras, "loading older history")
	case m.history.err != nil:
		extras = append(extras, "older history unavailable")
	case m.historyOffered() && !m.transcriptScroll.follow:
		extras = append(extras, "PgUp for older history")
	}
	// A paused reader learns that the newest output is off screen from the arrow
	// the reserved band draws under the transcript, and returns to it with End or
	// a click on that band. The status line no longer repeats the hint: two
	// surfaces saying the same thing compete for a narrow line.
	if !m.options.NoColor {
		style := lipgloss.NewStyle().Foreground(paletteFor(m.theme).muted)
		for index := range extras {
			extras[index] = style.Render(extras[index])
		}
	}

	return extras
}

func (m *Model) setLayout() {
	width := max(1, m.width)
	m.composer.SetWidth(composerEditorWidth(width))
	composerHeight := min(m.composerEditorRows(), max(layoutComposerMinRows, m.composer.Height()))
	m.composer.SetHeight(composerHeight)
	if m.prompt.kind == promptQuestion {
		m.prompt.question.editor.SetWidth(max(1, m.promptBandWidth()-4))
	}
	if m.prompt.kind == promptPlanReview {
		m.prompt.planReview.editor.SetWidth(max(1, m.promptBandWidth()-4))
		m.prompt.planReview.commentEditor.SetWidth(max(1, m.promptBandWidth()-4))
	}
	if m.routeUsesSearch() {
		m.route.search.SetWidth(routeSearchInputWidth(width))
	}
	if m.search.active {
		m.search.input.SetWidth(composerEditorWidth(width))
	}
}

func (m *Model) composerBox() string {
	return m.composerBoxContent(m.composer.View())
}

// composerBand is the ready frame's bottom band: the composer, or the find box
// while it is open. They share the box decoration and the cursor offset, so the
// frame height and the caret math do not change when the search opens.
func (m *Model) composerBand() string {
	if !m.searchOwnsKeys() {
		return m.composerBox()
	}

	return m.composerBoxContent(routeSearchLine(
		m.search.input.View(),
		composerEditorWidth(max(1, m.width)),
	))
}

func (m *Model) composerBoxContent(content string) string {
	caps := m.layout()
	if !m.drawsComposerBox(caps) {
		return content
	}

	border := lipgloss.RoundedBorder()
	if m.options.NoColor {
		border = lipgloss.NormalBorder()
	}

	// The bottom side is drawn by hand below, because the permission mode
	// replaces part of its rule instead of taking an inner row of the input.
	style := lipgloss.NewStyle().
		Width(max(1, m.width)).
		Padding(0, 1).
		Border(border, true, true, false, true)
	if !m.options.NoColor {
		style = style.BorderForeground(paletteFor(m.theme).separator)
	}

	return style.Render(content) + "\n" + m.composerBottomBorder(caps, border)
}

// drawsComposerMode reports whether the box's bottom border carries the
// permission mode. Only a comfortable window names the mode; a shorter frame
// draws the plain border.
func (m *Model) drawsComposerMode(caps layoutCaps) bool {
	return caps.composerMode && m.drawsComposerBox(caps)
}

// composerBottomBorder closes the box and right-aligns the active permission
// mode inside that rule, so the mode reads as a label of the frame rather than
// another line of the input.
func (m *Model) composerBottomBorder(caps layoutCaps, border lipgloss.Border) string {
	// Cells between the two corners. The label keeps one gap on each side and a
	// single rule cell before the bottom-right corner, so its distance to that
	// corner does not change with the box width.
	inner := max(0, max(1, m.width)-2)
	label := ""

	if m.drawsComposerMode(caps) {
		available := inner - 2*composerModeGap - ansi.StringWidth(border.Bottom)
		if available > 0 {
			label = ansi.Truncate(permissionModeText(m.permissionMode), available, "…")
		}
	}

	if label == "" {
		return border.BottomLeft + strings.Repeat(border.Bottom, inner) + border.BottomRight
	}

	rule := max(0, inner-2*composerModeGap-ansi.StringWidth(border.Bottom)-ansi.StringWidth(label))
	gap := strings.Repeat(" ", composerModeGap)
	head := border.BottomLeft + strings.Repeat(border.Bottom, rule) + gap
	tail := gap + border.Bottom + border.BottomRight

	if m.options.NoColor {
		return head + label + tail
	}

	palette := paletteFor(m.theme)
	ruleStyle := lipgloss.NewStyle().Foreground(palette.separator)

	return ruleStyle.Render(head) +
		lipgloss.NewStyle().Foreground(palette.muted).Render(label) +
		ruleStyle.Render(tail)
}

func (m *Model) hasComposerBox() bool {
	return m.width >= composerBoxMinWidth
}

// composerBoxInnerWidth is the box's content width: the frame minus the border and
// one padding cell on each side.
func composerBoxInnerWidth(width int) int {
	return max(1, width-4)
}

func composerEditorWidth(width int) int {
	if width >= composerBoxMinWidth {
		return composerBoxInnerWidth(width)
	}

	return max(1, width)
}

func (m *Model) composerBoxCursorOffset() (int, int) {
	if !m.drawsComposerBox(m.layout()) {
		return 0, 0
	}

	return 2, 1
}

func (m *Model) activityStatus() (activityStatus, bool) {
	return resolveActivity(activityContext{
		state:                      m.state,
		workspace:                  m.options.Workspace,
		isStarting:                 m.starting,
		hasBridge:                  m.bridge != nil,
		isCanceling:                m.canceling,
		isResolvingAllowedApproval: m.mainApprovalExecutionStarting(),
		now:                        time.Now(),
		lastModelEventAt:           m.lastModelEventAt,
	})
}

func (m *Model) activityLine() string {
	status, visible := m.activityStatus()
	if !visible {
		return ""
	}

	// The activity row is part of the footer that describes the live turn, so it
	// shares the Composer's text column with the status line and the input.
	inset := timelineInset(m.width)

	return insetRows(
		ansi.Truncate(
			m.activity.View(status, m.theme, m.options.NoColor),
			max(1, m.width-2*inset),
			"…",
		),
		inset,
	)
}

func (m *Model) effectivePhase() coding.Phase {
	bridgeActive := m.starting || m.bridge != nil || m.canceling
	if bridgeActive &&
		(m.state.Phase != coding.PhasePaused || m.mainApprovalExecutionStarting()) {
		return coding.PhaseRunning
	}

	return m.state.Phase
}

func (m *Model) renderTranscript(forceBottom bool) {
	m.renderTranscriptContent(forceBottom)
}

// rerenderTranscript rebuilds the region for a change that did not touch the
// conversation itself, such as a resize or a theme switch.
func (m *Model) rerenderTranscript(forceBottom bool) {
	m.renderTranscriptContent(forceBottom)
}

// liveGeometry is what a live frame was rendered for. A frame rendered at
// another width, inset, theme or colour mode cannot be extended, so the work in
// flight for it is retired instead of being installed into the new frame.
type liveGeometry struct {
	width   int
	inset   int
	theme   themeFingerprint
	noColor bool
}

// noteLiveGeometry retires background work when the frame's geometry changed.
// Geometry is part of every frame, so this is checked once per rebuild rather
// than at each place that can change it.
func (m *Model) noteLiveGeometry() {
	geometry := liveGeometry{
		width: m.width, inset: timelineInset(m.width),
		theme: m.themeFingerprint(), noColor: m.options.NoColor,
	}
	if m.liveGeometry == geometry {
		return
	}
	m.liveGeometry = geometry
	m.retireLiveRenderState()
}

func (m *Model) renderTranscriptContent(forceBottom bool) {
	m.renderDirty = false
	m.frameRenders++
	m.noteLiveGeometry()
	m.rebaseScrollbackWindow()
	// The transcript region changed, so a composed view is stale whatever the last
	// update was, and this invalidation outlives the update until View adopts it.
	m.viewStable = false
	m.viewStale = true
	if m.fullscreen() {
		m.renderManagedTranscript(forceBottom)

		return
	}

	blocks := m.transcriptBlocks()
	// The managed store paints this frame, so no conversation string is built
	// here. Rendering every active block only to test it for emptiness was work
	// the frame had no reader for, and in a long un-promoted window it was a
	// whole-conversation render on every frame.
	//
	// The seam between terminal-native history and the mutable region is part of
	// the managed content, so the region's rows must include it too.
	leading := 0
	if m.managedTimelineNeedsLeadingGap(blocks) {
		leading = conversationGapHeight
	}

	m.syncTranscriptStore(blocks, leading)
	m.finishTranscriptFrame(forceBottom)
}

// renderManagedTranscript renders one fullscreen frame. Fullscreen has no
// terminal-native history, so the whole conversation lives in the transcript
// store and the leading gap never applies. The committed prefix of the projection
// is reused while its inputs are unchanged, and the store keeps the rendered rows
// of that prefix, so a streaming frame costs the live tail.
func (m *Model) renderManagedTranscript(forceBottom bool) {
	// The streaming tail rewrites a block that may already be committed, so the
	// incremental frame does not apply here. It is set by the inline scrollback
	// path, which fullscreen never runs.
	if m.streaming.active {
		blocks := m.transcriptBlocks()
		m.syncTranscriptStore(blocks, 0)
		m.finishTranscriptFrame(forceBottom)

		return
	}

	frame := m.viewportProjection()
	m.syncTranscriptEntries(frame.prefixEntries, frame.tailEntries, frame.stable, 0)
	m.finishTranscriptFrame(forceBottom)
}

// finishTranscriptFrame publishes the freshly built region to the reader.
func (m *Model) finishTranscriptFrame(forceBottom bool) {
	m.transcriptScroll.setSource(&m.transcript)
	if forceBottom {
		m.transcriptScroll.gotoBottom()
	}
	if m.fullscreen() && !m.transcript.hasLive() {
		// Inline mode releases its live frame when streamProjection settles; the
		// managed store has no live record once the draft or thinking block
		// settles, and the committed record has its own rows.
		m.retireLiveRenderState()
	}
	// Matches are row indexes, so a reflow, a prepended page or new output has to
	// re-resolve them; the reader's match is kept by identity. The store
	// fingerprints everything a scan reads, so an unchanged frame is skipped.
	if m.search.active && m.search.scanned != m.transcript.fingerprint() {
		m.refreshSearch()
	}
}

// syncTranscriptStore projects the active blocks into per-record rendered rows.
// Records are reused by identity, so an unchanged record is never re-rendered and
// a streaming delta costs one entry.
func (m *Model) syncTranscriptStore(blocks []timelineBlock, leading int) {
	entries := make([]transcriptEntry, 0, len(blocks))
	for index := range blocks {
		entries = append(entries, transcriptEntry{
			id:    m.blockIdentity(blocks[index]),
			live:  blockIsUnsettled(blocks[index]),
			block: &blocks[index],
		})
	}

	m.transcript.sync(
		m.width,
		m.theme,
		m.themeFingerprint(),
		m.options.NoColor,
		m.markdown,
		leading,
		entries,
		nil,
		-1,
	)
}

// syncTranscriptEntries refreshes the store from an already-split frame. stable is
// the number of leading entries whose records did not change, so the store keeps
// their rendered rows and rebuilds only the tail.
func (m *Model) syncTranscriptEntries(prefix, tail []transcriptEntry, stable, leading int) {
	m.transcript.sync(
		m.width,
		m.theme,
		m.themeFingerprint(),
		m.options.NoColor,
		m.markdown,
		leading,
		prefix,
		tail,
		stable,
	)
}

// blockIdentity names a block for anchoring and cache reuse.
//
// Durable blocks carry their own identity (a candidate key, a Tool call ID, or a
// plan-mode activity ID). Blocks the projection cannot name fall back to their
// conversation position, which is stable for everything already on screen: an
// earlier entry never changes position because a later one arrived. A block with
// neither is deliberately uncacheable.
func (m *Model) blockIdentity(block timelineBlock) string {
	switch {
	case block.id != "":
		return kindName(block.kind) + ":" + block.id
	case len(block.tools) > 0 && block.tools[0].id != "":
		return "tool:" + block.tools[0].id
	case block.kind == blockDraft:
		return "draft"
	case block.position > 0:
		return "at:" + itoa(block.position) + ":" + kindName(block.kind)
	default:
		return ""
	}
}

// kindName renders a block kind for identity strings.
func kindName(kind blockKind) string {
	return itoa(int(kind))
}

func splitTranscriptRows(rendered string) []string {
	if rendered == "" {
		return nil
	}

	return strings.Split(rendered, "\n")
}

func (m *Model) themeFingerprint() themeFingerprint {
	return themeFingerprint(m.theme.Fingerprint())
}

// transcriptBlocks selects the projection the transcript renders from.
//
// Inline mode renders only the uncommitted tail, because everything already
// stable has been printed to the terminal's own history. Fullscreen mode owns the
// whole conversation, so it renders the full projection and keeps the stable
// content in the store instead of writing it out.
func (m *Model) transcriptBlocks() []timelineBlock {
	if m.fullscreen() {
		return m.fullscreenTimelineBlocks()
	}

	return m.activeTimelineBlocks()
}

// updateTranscriptScrollKey applies the conversation scrolling keys. They are
// deliberately separate from the composer's up/down history browsing, which is
// claimed first.
func (m *Model) updateTranscriptScrollKey(message tea.KeyPressMsg) (tea.Cmd, bool) {
	region := &m.transcriptScroll
	page := max(1, region.height-layoutStatusRows)
	atTop := false

	switch message.String() {
	case keyPageUp:
		region.scrollBy(-page)
		atTop = true
	case keyPageDown:
		region.scrollBy(page)
	case keyCtrlU:
		region.scrollBy(-max(1, page/2))
		atTop = true
	case keyCtrlD:
		region.scrollBy(max(1, page/2))
	case keyHome:
		region.gotoTop()
		atTop = true
	case keyEnd, keyCtrlG:
		region.gotoBottom()
	default:
		return nil, false
	}

	// An ordinary scroll ends a rail focus: the reader moved the window themselves,
	// so the mark a step placed is no longer where they are.
	m.endTurnRailFocus()

	// Reaching the top asks for the page above it. The request is bounded and
	// idempotent, so a reader who keeps scrolling simply gets more history.
	if atTop && region.offset == 0 {
		return m.requestOlderHistory(), true
	}

	return nil, true
}

// transcriptWindow sizes the transcript region and returns its visible rows.
func (m *Model) transcriptWindow(height int) string {
	if height <= 0 {
		m.transcriptScroll.setHeight(0)

		return ""
	}

	m.transcriptScroll.setHeight(height)
	visible := m.transcriptScroll.visible()
	if m.selection.visible {
		visible = m.highlightSelectionWindow(visible)
	}
	if m.searchOwnsKeys() {
		visible = m.highlightSearchWindow(visible)
	}

	// A frame that owns the screen pads the region to its band, so the Composer
	// and status line sit on the bottom row of the container instead of floating
	// under a short conversation. A frame in the main buffer must stay
	// content-sized: padding it would scroll the terminal's own history away.
	if m.ownsScreen() {
		visible = padRows(visible, height)
	}

	return visible
}

// ownsScreen reports whether the frame is drawn in the alternate buffer, where
// the app owns every row and no native history has to stay visible above it.
func (m *Model) ownsScreen() bool {
	screen, policy, _, _ := m.resolvePresentation()
	if screen != ScreenFullscreen {
		return false
	}

	return m.useAltScreen(screen, policy)
}

// padRows grows a composed band to exactly height rows with blank rows below it.
func padRows(content string, height int) string {
	rows := lipgloss.Height(content)
	if rows >= height {
		return content
	}

	return content + strings.Repeat("\n", height-rows)
}

// clampFrameTailOffset reports how many leading rows clampFrameTail removed, so a
// cursor measured against the untrimmed composition can be shifted with it.
func clampFrameTailOffset(content string, height int) int {
	if height <= 0 {
		return 0
	}
	if rows := lipgloss.Height(content); rows > height {
		return rows - height
	}

	return 0
}

// managedTimelineNeedsLeadingGap owns the seam between terminal-native
// scrollback and Bubble Tea's mutable tail. Once stream rows have been
// promoted, the remaining draft is a continuation and must stay adjacent.
func (m *Model) managedTimelineNeedsLeadingGap(blocks []timelineBlock) bool {
	if !m.scrollbackOutput || len(blocks) == 0 {
		return false
	}
	if m.streaming.active && m.streaming.emitted > 0 && blocks[0].kind == blockDraft {
		return false
	}

	return timelineProjectionDrawsRows(blocks)
}

// timelineProjectionDrawsRows reports whether the projection would draw at least
// one row. It replaces the "render the conversation, then test the string for
// emptiness" seam test: that render had no reader, and the active window of a
// long inline session is where it cost the most.
//
// The predicate reads the projection instead of rendering it, so it stays
// truthful for every block whose body and title are its visible content, and it
// answers true for the kinds that always draw a row of their own.
func timelineProjectionDrawsRows(blocks []timelineBlock) bool {
	return slices.ContainsFunc(blocks, timelineBlockDrawsRows)
}

// timelineBlockDrawsRows reports whether one block contributes a visible row
// without rendering it.
func timelineBlockDrawsRows(block timelineBlock) bool {
	if block.title != "" || strings.TrimSpace(block.body) != "" {
		return true
	}
	if len(block.tools) > 0 || block.workspaceChanges != nil || block.subagent != nil {
		return true
	}

	switch block.kind {
	case blockUser, blockAssistant, blockDraft, blockDiagnostic:
		// These kinds draw the title and body tested above and nothing else.
		return false
	case blockThinking, blockIncomplete, blockCompaction, blockQuestion, blockCompletion,
		blockChange, blockTeam, blockTool, blockSubagent, blockPlan, blockError:
		return true
	}

	return false
}

func (m *Model) timelineBlocks() []timelineBlock {
	blocks := insertCompletionMarkers(m.projectTimeline(m.state, nil), m.completionMarkers)
	blocks = insertQuestionPreviews(blocks, m.optimisticQuestionBlocks())
	if m.streamErr != nil {
		blocks = append(blocks, timelineBlock{
			kind: blockError, title: "Operation", body: safeError(m.streamErr),
		})
	}

	return blocks
}

// refreshToolDetailRoute keeps an open detail route aligned with live State:
// while a Tool runs, its progress and result appear in place instead of only
// after the route is reopened.
func (m *Model) refreshToolDetailRoute() {
	if m.route.kind != routeToolDetail || m.route.toolDetail == nil ||
		len(m.route.toolDetail.callIDs) == 0 {
		return
	}

	for _, block := range projectTimeline(m.state) {
		if len(block.tools) == 0 {
			continue
		}
		var detail toolDetailView
		switch block.kind {
		case blockTool:
			detail = newToolDetailViewInWorkspace(block, m.options.Workspace)
		case blockQuestion:
			detail = newQuestionDetailView(block)
		default:
			continue
		}
		if !slices.Equal(detail.callIDs, m.route.toolDetail.callIDs) {
			continue
		}

		// Keep the reader's position, clamping it to the new physical height.
		offset := m.route.offset
		m.route.toolDetail = &detail
		m.route.offset = min(max(0, offset), m.toolDetailMaximumOffset())

		return
	}
}

func (m *Model) toggleLatestTool() tea.Cmd {
	if len(m.state.Tools) > 0 {
		latest := m.state.Tools[len(m.state.Tools)-1]
		if isSubagentToolName(latest.Call.Name) {
			childSessionID := subagentChildSessionID(latest, m.state.Subagents)
			if childSessionID != "" {
				return m.openSubagentRoute(childSessionID)
			}
		}
	}

	blocks := projectTimeline(m.state)
	for _, block := range slices.Backward(blocks) {
		switch block.kind {
		case blockTool, blockSubagent:
			if len(block.tools) > 0 && block.tools[0].childSessionID != "" {
				return m.openSubagentRoute(block.tools[0].childSessionID)
			}
			detail := newToolDetailViewInWorkspace(block, m.options.Workspace)

			return m.openToolDetailRoute(detail)
		case blockQuestion:
			if len(block.tools) > 0 {
				return m.openToolDetailRoute(newQuestionDetailView(block))
			}
		case blockUser, blockAssistant, blockDraft, blockPlan, blockDiagnostic,
			blockChange, blockError, blockCompletion, blockTeam, blockCompaction:
		}
	}
	if child, ok := m.latestTeamWorkerChild(); ok {
		return m.openChildRoute(child)
	}

	return nil
}

func truncateText(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}

	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}

	return value + "…"
}

// Only live increments can wait for the next frame. Durable commits, errors,
// approvals and route transitions retain their synchronous presentation boundary.
func deferredTranscriptEvent(event coding.Event) bool {
	return event.Type == coding.EventMessageDelta || event.Type == coding.EventToolUpdated
}

func (m *Model) requestRender() tea.Cmd {
	m.renderDirty = true
	if m.renderWait {
		return nil
	}

	m.renderWait = true

	return tea.Tick(renderFrame, func(time.Time) tea.Msg {
		return renderTickMsg{}
	})
}

// armLiveWork publishes the newest live version the event loop declined to
// render and bounds the one-shot catch-up. An exact live slot arms nothing, so an
// idle session runs no timer and no repeated repaint.
func (m *Model) armLiveWork() tea.Cmd {
	if m.markdown == nil {
		return nil
	}

	commands := make([]tea.Cmd, 0, 2)

	if miss, ok := m.markdown.takeLiveMiss(); ok {
		if m.liveRender == nil {
			m.liveRender = newLiveRenderWorker()
		}
		m.liveRender.submit(liveRenderRequest{epoch: m.liveRenderEpoch, miss: miss})
	}
	if m.liveRender.pending() && !m.liveRenderWait {
		m.liveRenderWait = true
		commands = append(commands, m.liveRender.wait())
	}

	// A background render in flight is already the repaint. Only a stale frame no
	// worker is correcting needs the fallback, which is why a stray timer cannot
	// block the event loop behind the same render the worker is doing.
	if m.liveRender.pending() || m.liveCatchUpWait {
		return tea.Batch(commands...)
	}
	if delay, ok := m.markdown.liveCatchUpDelay(); ok {
		m.liveCatchUpWait = true
		m.liveCatchUpGen++
		generation := m.liveCatchUpGen
		commands = append(commands, tea.Tick(delay, func(time.Time) tea.Msg {
			return liveCatchUpMsg{generation: generation}
		}))
	}

	return tea.Batch(commands...)
}

// acceptLiveRender installs one background result and repaints. A result from a
// replaced projection, or one that failed, is dropped; the newest version is
// still requested, so the final content is always rendered.
func (m *Model) acceptLiveRender(result liveRenderResult) tea.Cmd {
	if result.worker != nil && result.worker != m.liveRender {
		return nil
	}
	m.liveRenderWait = false
	if m.liveRender != nil {
		m.liveRender.delivered(result.sequence)
	}
	if result.err == nil && result.epoch == m.liveRenderEpoch &&
		m.markdown.installExactRows(result.miss, result.rows, result.cost) {
		m.rerenderTranscript(false)
	}

	return m.armLiveWork()
}

// retireLiveRenderState invalidates paint ownership without cancelling the
// worker's existing receiver. Even an obsolete result must be drained before
// newer submitted work can finish; only shutdown cancels that receive command.
func (m *Model) retireLiveRenderState() {
	m.liveRenderEpoch++
	m.liveCatchUpWait = false
	m.liveCatchUpGen++
	m.markdown.releaseLiveState()
}

// shutdownLiveRender stops the background renderer. A program that never streamed
// an expensive tail never started it.
func (m *Model) shutdownLiveRender() {
	m.liveRenderEpoch++
	m.liveRenderWait = false
	m.liveCatchUpWait = false
	m.liveCatchUpGen++
	if m.liveRender != nil {
		m.liveRender.shutdown()
		m.liveRender = nil
	}
}

// publishRefusal records an operator-facing refusal and publishes it in the same
// update. setLayout only resizes the Composer, and the refusal is drawn from the
// transcript, so a refusal that does not commit leaves the draft on screen with
// Enter doing nothing and nothing explaining why. Every refusal path returns
// through here; a nil error publishes nothing and keeps the draft.
func (m *Model) publishRefusal(err error) tea.Cmd {
	if err == nil {
		return nil
	}

	m.streamErr = err
	m.setLayout()

	return m.commitStableTimeline()
}

// selectAllDraft selects the whole draft and states the keys that act on it. The
// notice is not decoration: the composer draws the selection but cannot say what the
// next key will do to it.
func (m *Model) selectAllDraft() tea.Cmd {
	selected := m.composer.ToggleSelectAll()
	m.setLayout()
	if !selected {
		return nil
	}

	return m.setStatusNotice(composerSelectionNotice)
}

// dismissReadingGesture answers Escape for the two reading gestures the ready view
// owns: a drag selection in the transcript, and a whole-draft selection in the
// Composer. Escape ends the gesture the reader just made, so neither may reach the
// cancel/exit binding behind it. It reports whether it answered the key.
func (m *Model) dismissReadingGesture(key string) (tea.Cmd, bool) {
	if key != keyEscape {
		return nil, false
	}

	if m.selection.visible {
		m.clearSelection()

		return redrawFrame(), true
	}
	if m.composer.SelectionActive() {
		m.composer.CollapseSelection()
		m.setLayout()

		return redrawFrame(), true
	}

	return nil, false
}

// composerImageRefusal reports why an image held by the draft will not send, or
// nil when the active model accepts image input.
func (m *Model) composerImageRefusal() error {
	if m.controller.Capabilities().Vision {
		return nil
	}

	return errComposerVisionUnsupported
}

// reconcileComposerRefusal drops a refusal whose cause is gone. The refusal is
// about the draft, so it must not outlive it: once the draft no longer holds an
// image the active model cannot accept, the notice would tell the operator to fix
// something they already fixed. It returns the commit that takes the notice off
// screen, or nil when there was nothing to clear.
func (m *Model) reconcileComposerRefusal() tea.Cmd {
	if !errors.Is(m.streamErr, errComposerVisionUnsupported) {
		return nil
	}
	if composerSnapshotHasImages(m.composer.Snapshot()) && !m.controller.Capabilities().Vision {
		return nil
	}

	m.streamErr = nil
	// The inline path prints the notice once and remembers it; a later refusal has
	// to be able to print again.
	m.scrollback.streamError = ""

	return m.commitStableTimeline()
}

func actionContextFor(state coding.State) actionContext {
	switch state.Phase {
	case coding.PhaseRunning:
		return contextRunning
	case coding.PhasePaused:
		return contextPaused
	default:
		return contextIdle
	}
}

func (m *Model) actionContext() actionContext {
	if m.starting || m.bridge != nil || m.canceling {
		return contextRunning
	}

	return actionContextFor(m.state)
}

type renderTickMsg struct{}

type scrollbackWriteDoneMsg struct {
	sequence uint64
}

func (m *Model) submit(actionCtx actionContext) tea.Cmd {
	m.endTurnRailFocus()
	snapshot := m.composer.Snapshot()
	switch actionCtx {
	case contextIdle:
		if m.directAgent != nil {
			return m.submitDirectAgent(snapshot)
		}

		return m.prepareSubmission(submissionPrompt, snapshot)
	case contextRunning:
		if m.directAgent != nil && m.directAgent.running {
			return nil
		}

		return m.prepareSubmission(submissionQueued, snapshot)
	default:
		return nil
	}
}

func (m *Model) queueMessage() tea.Cmd {
	if m.directAgent != nil && m.directAgent.running {
		return nil
	}

	return m.prepareSubmission(submissionQueued, m.composer.Snapshot())
}

func (m *Model) prepareSubmission(
	kind submissionKind,
	snapshot composerSnapshot,
) tea.Cmd {
	vision := true
	if composerSnapshotHasImages(snapshot) {
		vision = m.controller.Capabilities().Vision
		if !vision {
			return m.publishRefusal(errComposerVisionUnsupported)
		}
	}
	if composerSnapshotHasFiles(snapshot) {
		m.composerResolveSeq++
		generation := m.composerResolveSeq
		resolveCtx, cancel := context.WithCancel(m.ctx)
		resolveFile := m.controller.ResolveWorkspaceFile
		m.composerCancel = cancel
		m.composerResolving = true
		m.streamErr = nil
		m.setLayout()

		return func() tea.Msg {
			message, err := resolveComposerSnapshot(
				resolveCtx,
				snapshot,
				vision,
				resolveFile,
			)

			return composerResolvedMsg{
				generation: generation,
				snapshot:   snapshot,
				kind:       kind,
				message:    message,
				err:        err,
			}
		}
	}

	message, err := resolveComposerSnapshot(m.ctx, snapshot, vision, nil)
	if err != nil {
		if strings.TrimSpace(m.composer.Value()) != "" {
			return m.publishRefusal(err)
		}

		return nil
	}

	return m.dispatchSubmission(kind, snapshot, message)
}

func (m *Model) dispatchSubmission(
	kind submissionKind,
	snapshot composerSnapshot,
	message ai.Message,
) tea.Cmd {
	if kind == submissionQueued || m.queued > 0 || m.canceling {
		return m.enqueueRequest(snapshot, message)
	}
	if err := m.composer.RecordHistory(snapshot); err != nil {
		return m.publishRefusal(err)
	}
	m.composer.Reset()
	m.setLayout()
	m.streamErr = nil
	m.requests.paused = false
	controller := m.controller

	return m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
		return controller.Prompt(ctx, message)
	})
}

func (m *Model) startStream(operation streamOperation) tea.Cmd {
	if m.bridge != nil || m.starting || m.canceling {
		return nil
	}
	m.activity.Reset()
	m.requests.operationFailed = false
	m.canceling = false
	m.starting = true
	m.streamSeq++
	generation := m.streamSeq
	parent := m.ctx
	m.setLayout()

	return func() tea.Msg {
		return bridgeStartedMsg{bridge: startBridge(parent, operation), generation: generation}
	}
}

func (m *Model) continueIfPaused() tea.Cmd {
	if m.state.Goal.Status == coding.GoalPaused || m.state.Goal.Status == coding.GoalInterrupted {
		return nil
	}
	if m.state.Phase != coding.PhasePaused || m.bridge != nil || m.starting {
		return nil
	}

	return m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
		return m.controller.Continue(ctx)
	})
}

func (m *Model) updateStream(message streamItemMsg) (tea.Model, tea.Cmd) {
	if message.bridge == nil || message.bridge != m.bridge {
		return m, nil
	}

	m.waiting = false
	m.streamDeliveries++
	if !message.ok {
		return m, m.finishStream()
	}
	m.observeRequestEvent(message.item.event)

	if m.subscriptionMode && message.item.err == nil {
		// The subscription is the parent projection's source and already received
		// this event, so the delivery only advances the wait. It renders nothing
		// new, so the composed view stays current and View reuses it instead of
		// re-styling the whole frame. A cache an unadopted invalidation took away
		// is composed again rather than resurrected.
		m.streamItems++
		m.viewStable = true
		m.waiting = true

		return m, m.bridge.wait()
	}

	batch := make([]streamItem, 0, streamBatchMax)
	batch = append(batch, message.item)
	batch = append(batch, message.bridge.drainBatch(streamBatchMax-1, streamBatchBudget)...)
	m.streamItems += len(batch)
	for _, item := range batch[1:] {
		m.observeRequestEvent(item.event)
	}

	advanced := m.queueStreamBatch(batch)
	if advanced {
		m.setLayout()
	}
	refresh := tea.Batch(m.streamBatchRefresh(batch)...)

	m.waiting = true
	wait := m.bridge.wait()
	var commit, render tea.Cmd
	if m.streamErr == nil && !advanced {
		// The update only accounted for deltas: the reader cannot see them yet, so
		// the frame that is already composed is still current.
		m.viewStable = true
		render = m.requestRender()
	} else {
		commit = m.commitStableTimeline()
	}

	return m, m.afterScrollback(commit, wait, render, refresh)
}

// queueStreamBatch accounts for one drained batch. Deltas ahead of the first
// durable event only mark the frame dirty and wait for the render tick; the
// durable part, and everything the producer queued behind it, is applied now so
// approvals, errors, commits and session transitions keep their synchronous
// boundary. It reports whether the parent state advanced during this update.
func (m *Model) queueStreamBatch(batch []streamItem) bool {
	m.streamBatches++

	split := 0
	for split < len(batch) && deferredTranscriptEvent(batch[split].event) {
		split++
	}

	m.pending = append(m.pending, batch[:split]...)
	if split == len(batch) {
		return false
	}

	m.flushPending()
	m.reduceStreamBatch(batch[split:])

	return true
}

// flushPending applies the deltas accumulated since the last frame as one batch,
// so a frame advances the parent state once however many deltas it carried.
//
// The buffer is released with its events: a drained coding.Event carries the
// delta payload, and truncating the slice alone would keep the last burst's
// payloads reachable until the next burst overwrote them.
func (m *Model) flushPending() {
	if len(m.pending) == 0 {
		return
	}

	items := m.pending
	m.pending = nil
	m.reduceStreamBatch(items)
	clear(items)
	if m.pending == nil {
		m.pending = items[:0]
	}
}

// dropPending discards deltas that belong to a projection the model is about to
// replace, so they cannot be applied to the replacement.
func (m *Model) dropPending() {
	if len(m.pending) == 0 {
		return
	}
	clear(m.pending)
	m.pending = m.pending[:0]
}

// reduceStreamBatch advances the TUI projection for one drained frame.
//
// The Runtime already applied these events in subscription mode, so the parent
// state is taken from the Runtime once per batch instead of running the
// transition table a second time (R2). Every other frontend responsibility -
// child Sessions, Team and subagent projections, stream errors, completion
// markers and approval prompts - still runs per event.
func (m *Model) reduceStreamBatch(items []streamItem) {
	if len(items) == 0 {
		return
	}

	valid, streamErr := streamItemsBeforeError(items)

	previousSessionID := m.state.SessionID
	previousMessages := len(m.state.Transcript)

	if !m.advanceParentState(valid) {
		return
	}
	m.rebaseScrollbackWindow()
	if len(m.state.Transcript) != previousMessages {
		m.rebaseCompletionMarkers()
	}

	if previousSessionID != m.state.SessionID {
		m.resetRequestQueue()
		m.stopTeamWorkerRouteSubscription()
		m.resetTeamProjection(m.state.SessionID)
		m.resetSubagentInteractions()
		m.resetTasksPane()
	}

	// The tasks pane observes the projection the batch just advanced, so a child
	// starting or stopping is what opens and closes it.
	m.syncTasksPane(time.Now())

	parent := false
	for _, item := range valid {
		if m.isParentSessionEvent(item.event) {
			parent = true
		}

		m.observeStreamEvent(item.event)
	}

	if streamErr != nil {
		m.failStream(streamErr)

		return
	}

	if parent {
		m.syncApprovalPrompt()
	}
}

// streamItemsBeforeError splits a drained batch at its first bridge error. The
// producer stops after an error, so an error is expected last.
func streamItemsBeforeError(items []streamItem) ([]streamItem, error) {
	for index, item := range items {
		if item.err != nil {
			return items[:index], item.err
		}
	}

	return items, nil
}

// streamBatchRefresh collects the per-event route refreshes for a frame.
// Deferred deltas never drive those refreshes, so they are skipped.
func (m *Model) streamBatchRefresh(batch []streamItem) []tea.Cmd {
	refresh := make([]tea.Cmd, 0, len(batch)+1)

	for _, item := range batch {
		if deferredTranscriptEvent(item.event) {
			continue
		}

		refresh = append(refresh,
			m.invalidateAgentDetail(item),
			m.invalidateTeamProjection(item.event),
			m.observeSubagentControl(item.event),
		)
	}
	// A completed interaction's split is written to the Session's sidecar here,
	// where the state it was projected from is already advanced.
	if command := m.usageProjectionWriteCommand(); command != nil {
		refresh = append(refresh, command)
	}

	return append(refresh, m.loadPlanViewIfNeeded())
}

// modelProgressObserved reports whether a frame carried anything that shows the
// model is working: a new interaction, run, or turn, a streaming delta, or a
// retry notice. Tool lifecycle events deliberately do not count, because a
// running tool owns the activity row on its own.
func modelProgressObserved(events []coding.Event) bool {
	for _, event := range events {
		switch event.Type {
		case coding.EventInteractionStarted, coding.EventRunStarted, coding.EventTurnStarted,
			coding.EventMessageDelta, coding.EventModelRetry:
			return true
		default:
			// Other events are either not model progress or own a row of their
			// own; new event types stay inert here.
		}
	}

	return false
}

// advanceParentState applies one frame of parent-Session events to m.state and
// reports whether the projection can still be rendered.
func (m *Model) advanceParentState(items []streamItem) bool {
	events := make([]coding.Event, 0, len(items))
	for _, item := range items {
		if m.isParentSessionEvent(item.event) {
			events = append(events, item.event)
		}
	}

	if len(events) == 0 {
		return true
	}

	if modelProgressObserved(events) {
		m.lastModelEventAt = time.Now()
	}

	if m.runtimeState && m.controller != nil {
		if err := m.checkParentSequence(events); err != nil {
			m.failStream(err)

			return false
		}

		m.frameAdvances++

		return m.adoptRuntimeState(events[len(events)-1])
	}

	next, err := coding.ReduceBatch(m.state, events)
	if err != nil {
		m.failStream(err)

		return false
	}

	m.frameAdvances++
	m.state = next
	m.observedSequence = next.Sequence

	return true
}

// checkParentSequence verifies that the parent-Session events of one frame
// continue the last delivered event, so an out-of-order or missing delivery is
// still detected when the Runtime owns the reduction. Child-Session events run
// on their own Session sequence and never appear here.
func (m *Model) checkParentSequence(events []coding.Event) error {
	previous := m.observedSequence

	for _, event := range events {
		if event.Sequence != previous+1 {
			return fmt.Errorf("coding tui: event sequence %d follows %d", event.Sequence, previous)
		}

		previous = event.Sequence
	}

	m.observedSequence = previous

	return nil
}

// readOnlySnapshotter is the optional Controller capability that hands the
// model a read-only projection without a defensive deep copy. The Runtime's
// reducer is copy-on-write, so a retained view keeps the projection it was
// taken from.
type readOnlySnapshotter interface {
	SnapshotReadOnly() coding.State
}

// projectionSnapshot takes the authoritative parent projection. A Controller
// that offers the read-only view is preferred: the streaming path calls this
// once per frame, and the deep Snapshot would copy the whole conversation each
// time. Controllers without the capability keep the defensive contract.
func (m *Model) projectionSnapshot() coding.State {
	if reader, ok := m.controller.(readOnlySnapshotter); ok {
		return reader.SnapshotReadOnly()
	}

	return m.controller.Snapshot()
}

// adoptRuntimeState takes the parent projection from the Runtime that already
// applied these events. The Runtime sequence must be at least as advanced as the
// last event observed here.
func (m *Model) adoptRuntimeState(last coding.Event) bool {
	snapshot := m.projectionSnapshot()
	if snapshot.Sequence < last.Sequence {
		m.failStream(fmt.Errorf(
			"coding tui: runtime snapshot at sequence %d lags event %d",
			snapshot.Sequence, last.Sequence,
		))

		return false
	}

	m.state = snapshot

	return true
}

// failStream records a stream failure and stops the running bridge.
func (m *Model) failStream(err error) {
	m.requests.operationFailed = true
	m.streamErr = err
	if m.bridge != nil {
		m.bridge.once.Do(m.bridge.cancel)
	}
}

// observeStreamEvent runs the per-event frontend bookkeeping for one drained
// event. Child-Session events only update their own projection.
func (m *Model) observeStreamEvent(event coding.Event) {
	if !m.isParentSessionEvent(event) {
		m.reduceChildEvent(event)

		return
	}

	m.updateSubagentRouteSummary(event)
	m.trackTeamLifecycleEvent(event)
	m.applyTeamInteractionEvent(event)
	m.trackInteractionRun(event)
	switch event.Type {
	case coding.EventSessionNavigated:
		m.resetRequestQueue()
		// A new conversation replaces what the main buffer holds, so the exit
		// handoff may write it again. Compaction keeps the flag: the rows the
		// escape hatch already printed are still on the screen.
		m.transcriptHanded = false
		m.resetNotices()
		m.resetHistory()
		m.resetScrollback()
	case coding.EventCompactionCompleted:
		m.resetScrollback()
	}

	m.recordCompletion(event)
}

func (m *Model) isParentSessionEvent(event coding.Event) bool {
	return m.state.SessionID == "" || event.SessionID == m.state.SessionID
}

func (m *Model) recordCompletion(event coding.Event) {
	switch event.Type {
	case coding.EventSessionNavigated, coding.EventCompactionCompleted:
		m.completionMarkers = nil
		m.questionPreviews = nil
		m.clearInteractionRuns()

		return
	case coding.EventInteractionCompleted:
	default:
		return
	}

	completed, ok := event.Payload.(coding.InteractionCompleted)
	if !ok || event.InteractionID == "" {
		return
	}
	for _, marker := range m.completionMarkers {
		if marker.interactionID == event.InteractionID {
			return
		}
	}

	marker := completionMarker{
		interactionID: event.InteractionID,
		// The tail is the fallback hint only; the run identity below decides when
		// the snapshot already holds a later interaction.
		afterMessages:  len(m.state.Transcript),
		afterRunID:     m.interactionRun(event.InteractionID),
		outcome:        completed.Outcome,
		stop:           completed.Stop,
		durationMillis: completed.DurationMillis,
		model:          m.modelLabel(),
	}
	m.forgetInteractionRun(event.InteractionID)

	// The anchor comes from the marker's own identity, not from the transcript
	// tail: the subscription path adopts a snapshot that may already hold a later
	// interaction, and its tail would place this row after that interaction.
	m.anchorCompletionMarker(&marker)

	m.completionMarkers = appendMarker(m.completionMarkers, marker)
	if len(m.completionMarkers) > maxCompletionMarkers {
		first := len(m.completionMarkers) - maxCompletionMarkers
		for _, dropped := range m.completionMarkers[:first] {
			delete(m.scrollback.completions, dropped.interactionID)
		}
		m.completionMarkers = append([]completionMarker(nil), m.completionMarkers[first:]...)
	}
}

// trackInteractionRun remembers which run an interaction is in. The Runtime
// delivers parent events contiguously, so this is the interaction's own run
// identity recorded at event time, independent of how far the snapshot has
// advanced.
func (m *Model) trackInteractionRun(event coding.Event) {
	if event.InteractionID == "" {
		return
	}

	switch event.Type {
	case coding.EventInteractionStarted:
		m.rememberInteractionRun(event.InteractionID, "")
	case coding.EventRunStarted, coding.EventMessageCommitted:
		if event.RunID != "" {
			m.rememberInteractionRun(event.InteractionID, event.RunID)
		}
	default:
		// Other events carry no new interaction-to-run ownership.
	}
}

func (m *Model) rememberInteractionRun(interactionID, runID string) {
	if m.interactionRuns == nil {
		m.interactionRuns = make(map[string]string, maxCompletionMarkers)
	}
	if _, exists := m.interactionRuns[interactionID]; !exists {
		m.interactionRunKeys = append(m.interactionRunKeys, interactionID)
		for len(m.interactionRunKeys) > maxCompletionMarkers {
			delete(m.interactionRuns, m.interactionRunKeys[0])
			m.interactionRunKeys = append([]string(nil), m.interactionRunKeys[1:]...)
		}
	}
	m.interactionRuns[interactionID] = runID
}

func (m *Model) interactionRun(interactionID string) string {
	return m.interactionRuns[interactionID]
}

func (m *Model) forgetInteractionRun(interactionID string) {
	if _, exists := m.interactionRuns[interactionID]; !exists {
		return
	}
	delete(m.interactionRuns, interactionID)
	for index, key := range m.interactionRunKeys {
		if key == interactionID {
			m.interactionRunKeys = append(m.interactionRunKeys[:index], m.interactionRunKeys[index+1:]...)

			break
		}
	}
}

func (m *Model) clearInteractionRuns() {
	clear(m.interactionRuns)
	m.interactionRuns = nil
	m.interactionRunKeys = nil
}

// completionAnchor resolves a marker's conversation position from the current
// window. It indexes the window once so several markers cost one scan.
type completionAnchor struct {
	runs    map[string]int
	digests map[uint64][]int
}

func (m *Model) newCompletionAnchor() completionAnchor {
	transcript := m.state.Transcript
	anchor := completionAnchor{
		runs:    make(map[string]int, len(transcript)),
		digests: make(map[uint64][]int, len(transcript)),
	}

	// The reducer keeps candidates parallel across durable transcript replacement.
	// Defend against external controller snapshots that predate that invariant.
	aligned := len(m.state.MessageCandidates) == len(transcript)

	for index, message := range transcript {
		position := index + 1
		if aligned {
			if runID := m.state.MessageCandidates[index].RunID; runID != "" {
				// The last message a run committed is the one the marker follows.
				anchor.runs[runID] = position
			}
		}

		digest := projectionMessageDigest(projectionHashSeed, message)
		anchor.digests[digest] = append(anchor.digests[digest], position)
	}

	return anchor
}

// position resolves where a marker belongs. The run identity is exact: a run
// commits one assistant message per turn, so it names the interaction's final
// answer and survives both a leading Runtime snapshot and a message inserted
// ahead of that answer. The content digest is the fallback for a transcript the
// durable path rebuilt without candidate identities; it resolves to the nearest
// occurrence from the marker's last position, so an identical later answer
// cannot capture an earlier marker.
func (anchor completionAnchor) position(marker completionMarker) int {
	if position, ok := anchor.runs[marker.afterRunID]; ok && marker.afterRunID != "" {
		return position
	}

	if marker.afterDigest != 0 {
		best, bestDistance := 0, 0
		for _, position := range anchor.digests[marker.afterDigest] {
			distance := position - marker.afterMessages
			if distance < 0 {
				distance = -distance
			}
			if best == 0 || distance < bestDistance {
				best, bestDistance = position, distance
			}
		}
		if best > 0 {
			return best
		}
	}

	// No identity resolved: the recorded position is the anchor. It is never
	// clamped down, because a count beyond the window renders at the tail exactly
	// like the window's own length would.
	return max(0, marker.afterMessages)
}

// anchorCompletionMarker resolves a marker and records the digest of the message
// it now follows, so the next re-anchor has a fallback when the identity is gone.
func (m *Model) anchorCompletionMarker(marker *completionMarker) {
	position := m.newCompletionAnchor().position(*marker)
	marker.afterMessages = position
	marker.afterDigest = 0
	if position > 0 && position <= len(m.state.Transcript) {
		marker.afterDigest = projectionMessageDigest(
			projectionHashSeed, m.state.Transcript[position-1],
		)
	}
}

// rebaseCompletionMarkers re-resolves each marker's anchor after the transcript
// changed. A question result can be inserted ahead of the final assistant
// message after the interaction completed, which shifts every later message and
// would otherwise place the completion row above the answer it follows.
func (m *Model) rebaseCompletionMarkers() {
	if len(m.completionMarkers) == 0 {
		return
	}

	anchor := m.newCompletionAnchor()
	changed := false

	for index := range m.completionMarkers {
		marker := &m.completionMarkers[index]
		position := anchor.position(*marker)
		if position != marker.afterMessages {
			marker.afterMessages = position
			changed = true
		}
		if position > 0 && position <= len(m.state.Transcript) {
			digest := projectionMessageDigest(projectionHashSeed, m.state.Transcript[position-1])
			if digest != marker.afterDigest {
				marker.afterDigest = digest
				changed = true
			}
		}
	}

	if changed {
		// A resolved position can reorder two markers; the projection and the
		// prefix split both require the list ordered by position.
		sort.SliceStable(m.completionMarkers, func(left, right int) bool {
			return m.completionMarkers[left].afterMessages <
				m.completionMarkers[right].afterMessages
		})
	}
}

// modelLabel is captured on each completion marker so a later model switch
// cannot rewrite the provider/model attribution of an earlier interaction.
func (m *Model) modelLabel() string {
	provider := string(m.state.Provider)
	switch {
	case provider == "":
		return m.state.ModelID
	case m.state.ModelID == "":
		return provider
	default:
		return provider + "/" + m.state.ModelID
	}
}

func (m *Model) finishStream() tea.Cmd {
	// The last deltas of the turn are part of its final projection, so they are
	// applied before the stream is closed and the snapshot is compared.
	m.flushPending()

	if !m.subscriptionMode {
		snapshot := m.controller.Snapshot()
		if m.streamErr == nil && snapshot.Sequence != m.state.Sequence {
			m.streamErr = fmt.Errorf(
				"coding tui: event stream stopped at sequence %d; runtime is at %d",
				m.state.Sequence,
				snapshot.Sequence,
			)
			m.requests.operationFailed = true
		}
		m.state = snapshot
	}
	m.syncApprovalPrompt()
	m.clearCompletedDirectAgent()
	m.bridge = nil
	m.starting = false
	m.waiting = false
	m.finishRequestOperation()
	m.setLayout()

	return m.afterScrollback(m.commitStableTimeline(), m.requestAdvanceCommand())
}

func (m *Model) cancelStream() tea.Cmd {
	if m.canceling {
		return nil
	}
	m.requests.paused = true
	bridge := m.bridge
	generation := m.streamSeq
	controller := m.controller
	parent := m.ctx
	beforeStart := bridge == nil && m.starting
	if beforeStart {
		m.cancelStart = true
	}
	m.canceling = true
	m.setLayout()

	return func() tea.Msg {
		if beforeStart {
			return cancelResultMsg{generation: generation, beforeStart: true}
		}
		err := controller.Cancel()
		if bridge != nil {
			stopCtx, cancel := context.WithTimeout(
				context.WithoutCancel(parent),
				controllerCloseTimeout,
			)
			err = errors.Join(err, bridge.stop(stopCtx))
			cancel()
		}

		return cancelResultMsg{bridge: bridge, generation: generation, beforeStart: beforeStart, err: err}
	}
}

func (m *Model) stopBridge(bridge *eventBridge) tea.Cmd {
	generation := m.streamSeq
	parent := m.ctx
	controller := m.controller
	return func() tea.Msg {
		err := controller.Cancel()
		stopCtx, cancel := context.WithTimeout(
			context.WithoutCancel(parent),
			controllerCloseTimeout,
		)
		defer cancel()

		return cancelResultMsg{bridge: bridge, generation: generation, err: errors.Join(err, bridge.stop(stopCtx))}
	}
}

func (m *Model) stopStream(ctx context.Context) error {
	if m == nil || m.bridge == nil {
		return nil
	}

	err := m.controller.Cancel()
	err = errors.Join(err, m.bridge.stop(ctx))
	m.bridge = nil
	m.starting = false
	m.waiting = false
	m.canceling = false
	m.setLayout()

	return err
}

func (m *Model) startSubscription() tea.Cmd {
	if m.controller == nil {
		return nil
	}

	m.subscriptionSeq++
	generation := m.subscriptionSeq
	controller := m.controller

	return func() tea.Msg {
		message := startSubscription(controller)
		message.generation = generation

		return message
	}
}

func (m *Model) updateSubscription(message subscriptionEventMsg) (tea.Model, tea.Cmd) {
	if message.bridge == nil || message.bridge != m.subscription {
		return m, nil
	}
	if !message.ok {
		m.subscription = nil
		if message.err != nil && !errors.Is(message.err, coding.ErrEventGap) {
			m.streamErr = message.err
		}
		if m.picker.controlling || m.route.controlling {
			return m, nil
		}

		return m, m.startSubscription()
	}

	m.subscriptionDeliveries++

	// Drain what is already queued so one MVU update advances a whole frame
	// instead of one delta at a time.
	batch := make([]streamItem, 0, streamBatchMax)
	batch = append(batch, streamItem{event: message.record.Event})
	for _, record := range message.bridge.drainBatch(streamBatchMax-1, streamBatchBudget) {
		batch = append(batch, streamItem{event: record.Event})
	}
	m.streamItems += len(batch)

	activityWasVisible := m.activityClockVisible()
	advanced := m.queueStreamBatch(batch)
	if advanced {
		m.setLayout()
	}
	refresh := tea.Batch(append(m.streamBatchRefresh(batch), m.startActivityClock(activityWasVisible))...)
	wait := message.bridge.wait()
	var commit, render tea.Cmd
	if m.streamErr == nil && !advanced {
		// The update only accounted for deltas: the reader cannot see them yet, so
		// the frame that is already composed is still current.
		m.viewStable = true
		render = m.requestRender()
	} else {
		commit = m.commitStableTimeline()
	}

	return m, m.afterScrollback(commit, wait, render, refresh, m.requestAdvanceCommand())
}

// reduceObservedEvent applies one already-observed event. Batched updates use
// reduceStreamBatch; this single-event entry point remains for callers that hold
// exactly one event, such as mode control waiting for its subscribed event.
func (m *Model) reduceObservedEvent(event coding.Event) {
	m.reduceStreamBatch([]streamItem{{event: event}})
}

// reduceChildEvent projects one child-Session event into the TUI's own child
// state. A parent subscription does not carry child Runtime States, so child
// projections keep using the shared transition table. The open child page's
// scroll region re-anchors when the fresh rows are installed, so appending content
// keeps the tail visible while the reader follows it.
func (m *Model) reduceChildEvent(event coding.Event) {
	state, known := m.childStates[event.SessionID]
	if !known {
		// A terminal child projection is released once its final status lands.
		// A late bookkeeping event cannot extend it; a fresh SessionOpened starts
		// the next execution that reuses the child Session.
		if event.Type != coding.EventSessionOpened || event.SessionID == "" {
			return
		}

		state = coding.State{}
	}
	// An authoritative snapshot adopted for an open page can already include
	// events still queued on the subscription: the snapshot is read after they
	// were published. Replaying one is a no-op, so its sequence is not a
	// protocol violation.
	if state.SessionID == event.SessionID && event.Sequence <= state.Sequence {
		return
	}

	next, err := coding.Reduce(state, event)
	if err != nil {
		m.streamErr = fmt.Errorf("coding tui: reduce child %s: %w", event.SessionID, err)

		return
	}

	m.childStates[event.SessionID] = next
	if m.route.kind == routeChild && m.route.childKind == childSubagent &&
		m.route.childSessionID == event.SessionID {
		child := next.Clone()
		m.route.childState = &child
	}
	if childStatusFinished(event) {
		delete(m.childStates, event.SessionID)
	}
}

// childProjectionFinished reports a child projection whose interaction has
// completed. The final idle status can still be queued at this point; it is
// applied to the open page before the map entry is released.
func childProjectionFinished(state coding.State) bool {
	return state.SessionID != "" && !state.Interaction.Active &&
		state.Interaction.Outcome != ""
}

// childStatusFinished reports the child event that closes a projection: the idle
// status finishChildProjection emits last, or an explicit SessionClosed.
func childStatusFinished(event coding.Event) bool {
	if event.Type == coding.EventSessionClosed {
		return true
	}
	if event.Type != coding.EventStatusChanged {
		return false
	}
	status, ok := event.Payload.(coding.StatusChanged)

	return ok && status.Phase == coding.PhaseIdle
}

// adoptChildState records the Runtime's authoritative child State as this model's
// own copy. A child that continued another child's conversation inherits its
// history, which the child's own event stream does not carry, so keeping the two
// copies the same is what lets later events extend the inherited transcript
// instead of replacing it with a shorter one. A terminal child is owned by its
// open page only; releasing it here is what keeps the map bounded by live
// children.
func (m *Model) adoptChildState(childSessionID string, state coding.State) {
	if childSessionID == "" || childProjectionFinished(state) {
		return
	}
	m.childStates[childSessionID] = state.Clone()
}

// liveChildStates takes ownership of an EventObservation's child projections.
// The observation is exclusive to this subscription and already holds a
// defensive clone, so the model keeps the map instead of cloning every child a
// second time. Terminal projections are dropped.
func liveChildStates(values map[string]coding.State) map[string]coding.State {
	live := make(map[string]coding.State, len(values))
	for childSessionID, state := range values {
		if childProjectionFinished(state) {
			continue
		}
		live[childSessionID] = state
	}

	return live
}

func (m *Model) stopSubscription() {
	if m == nil {
		return
	}

	// Invalidate observations already dispatched as well as a live bridge.
	// Clearing queued scrollback continuations alone cannot stop their replies.
	m.subscriptionSeq++
	m.subscription.stop()
	m.subscription = nil
}

func (m *Model) confirmTrust() (tea.Model, tea.Cmd) {
	m.lifecycle = lifecycleLoading
	m.err = nil

	return m, m.bootstrap(m.allow)
}

func (m *Model) bootstrap(trustProject bool) tea.Cmd {
	return func() tea.Msg {
		controller, err := m.options.Bootstrap(m.ctx, trustProject)

		return bootstrapResult{controller: controller, err: err}
	}
}

func safeError(err error) string {
	if err == nil {
		return "unknown startup error"
	}

	return strings.TrimSpace(err.Error())
}

func trustErrorMessage(err error) string {
	if err == nil {
		return ""
	}

	return "\n\nCould not save trust: " + safeError(err) + "\nRetry or choose Deny."
}

var _ tea.Model = (*Model)(nil)
