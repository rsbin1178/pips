//nolint:wsl_v5 // The MVU state machine keeps transition effects adjacent.
package tui

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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

type controllerCommand string

const (
	commandSteer    controllerCommand = "steer"
	commandFollowUp controllerCommand = "follow_up"
)

type controllerCommandMsg struct {
	snapshot composerSnapshot
	err      error
}

type submissionKind uint8

const (
	submissionPrompt submissionKind = iota
	submissionSteer
	submissionFollowUp
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
	timeline             string
	transcript           transcriptStore
	transcriptScroll     scrollRegion
	selection            selectionState
	search               searchState
	mouseCaptureOff      bool
	transcriptMode       transcriptModeState
	transcriptHanded     bool // Escape hatch already wrote the rows to the main buffer.
	frameHit             frameHitMap
	history              historyState
	notices              []noticeEntry
	noticeSequence       uint64
	scrollback           scrollbackCursor
	scrollbackOutput     bool
	streaming            streamProjection
	renderWait           bool
	renderDirty          bool
	// viewStable marks an update that did not change what the frame renders: a
	// deferred delta the reader cannot see yet, or a delivery the subscription
	// already owns and the operation iterator only repeats. View() then reuses the
	// last composition instead of re-styling the whole frame for it.
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
	// frameCache reuses the committed prefix of the managed timeline between
	// frames, and frameEntries is the buffer the volatile tail is named in.
	// toolActivities is the activity list that prefix was projected from, so the
	// volatile tail can be rebuilt without rescanning the transcript.
	frameCache     timelineFrameCache
	frameEntries   []transcriptEntry
	toolActivities []toolActivity
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
	observedSequence    uint64
	starting            bool
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
	canceling           bool
	exitArmed           bool
	bannerPrinted       bool
	// permissionMode caches the effective sandbox mode the Composer's permission
	// row shows. Reading it while rendering would clone the Controller's whole
	// configuration, so it is refreshed when a control result can change it.
	permissionMode       config.SandboxMode
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
	planNoticeSeq        uint64
	worktreeLoading      bool
	worktreeGeneration   uint64
	worktreeCancel       context.CancelFunc
	worktreeSummary      string
	activity             activityIndicator
	statusLineItems      []statusline.Item
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
	commands := make([]tea.Cmd, 0, 3)
	if !m.options.NoColor {
		commands = append(commands, tea.RequestBackgroundColor)
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
		if m.themeSelection == config.ThemeAuto {
			m.applyTheme(m.themeForSelection(config.ThemeAuto))
		}

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
		if previousSessionID != m.state.SessionID {
			m.stopTeamWorkerRouteSubscription()
			m.resetTeamProjection(m.state.SessionID)
		}
		m.childStates = cloneChildStates(message.observation.Children)
		m.resetSubagentInteractions()
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
		)
		return m, m.afterScrollback(commit, wait)
	case subscriptionEventMsg:
		return m.updateSubscription(message)
	case bridgeStartedMsg:
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

			return m, tea.Batch(m.stopBridge(message.bridge), m.activity.Tick())
		}

		return m, tea.Batch(message.bridge.wait(), m.activity.Tick())
	case streamItemMsg:
		return m.updateStream(message)
	case controllerCommandMsg:
		if message.err != nil {
			m.streamErr = message.err
			if m.composer.Value() == "" {
				m.streamErr = errors.Join(m.streamErr, m.composer.Restore(message.snapshot))
			}
		} else {
			m.queued++
			if err := m.composer.RecordHistory(message.snapshot); err != nil {
				m.streamErr = errors.Join(m.streamErr, err)
			}
		}
		m.setLayout()
		return m, m.commitStableTimeline()
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

			return m, m.commitStableTimeline()
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
			m.streamErr = message.err
			m.setLayout()

			return m, nil
		}
		m.streamErr = m.composer.InsertImage(message.image)
		m.setLayout()

		return m, nil
	case planDocumentMsg:
		return m.applyPlanDocument(message)
	case bridgeImageMsg:
		return m.updateBridgeImage(message)
	case goalControlResultMsg:
		return m.applyGoalControl(message)
	case cancelResultMsg:
		if message.beforeStart {
			m.streamErr = errors.Join(m.streamErr, message.err)
			if !m.subscriptionMode {
				m.state = m.controller.Snapshot()
			}
			m.syncApprovalPrompt()
			m.clearCompletedDirectAgent()
			m.setLayout()
			return m, m.commitStableTimeline()
		}
		if m.bridge != nil && message.bridge != nil && message.bridge != m.bridge {
			return m, nil
		}
		m.bridge = nil
		m.waiting = false
		m.canceling = false
		m.streamErr = errors.Join(m.streamErr, message.err)
		m.flushPending()
		if !m.subscriptionMode {
			m.state = m.controller.Snapshot()
		}
		m.syncApprovalPrompt()
		m.clearCompletedDirectAgent()
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
			m.presentation.pendingRoute = routeOpenRequest{}
			m.stopTeamWorkerRouteSubscription()
			m.stopSubscription()
			m.dropPending()
			m.state = m.controller.Snapshot()
		} else if !m.subscriptionMode {
			m.dropPending()
			m.state = m.controller.Snapshot()
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
		// refreshed with it instead of on every frame.
		m.refreshPermissionMode()
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
		var commit tea.Cmd
		switch message.operation {
		case operationNew:
			commit = m.commitNewSessionOutput()
		default:
			commit = m.commitStableTimeline()
		}

		if replacementControl {
			commands := []tea.Cmd{m.startSubscription()}
			if message.operation == operationResume {
				commands = append(commands, m.probeTeamRecoveryAfterResume())
			}

			return m, m.afterScrollback(commit, commands...)
		}

		return m, m.afterScrollback(commit, m.continueIfPaused())
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
		// A copy confirmation and the selection it describes expire together, so
		// the highlight lives exactly as long as the notice that explains it.
		m.clearSelection()
		m.setLayout()

		return m, nil
	case selectionRedrawMsg:
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
			m.streamErr = err
			m.setLayout()

			return m, nil
		}
		if m.route.kind == routeChild && m.route.childKind == childTeamWorker {
			_, err := m.composer.InsertPaste(message.Content)
			m.streamErr = err
			m.setLayout()

			return m, nil
		}
		if m.route.kind != routeNone || m.prompt.kind != promptNone || m.composerResolving ||
			m.clipboardLoading ||
			m.picker.kind != pickerNone {
			return m, nil
		}

		_, err := m.composer.InsertPaste(message.Content)
		m.streamErr = err
		m.setLayout()

		return m, nil
	case tea.KeyPressMsg:
		return m.updateKey(message)
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

		return m, commit
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

		return m, m.activity.Update(message)
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
	// The find box owns the ready view's keys while it is open: it is a reading
	// surface, so the composer, its history and the command picker stay untouched
	// until it closes.
	if m.searchOwnsKeys() {
		return m.updateSearchKey(message)
	}
	key := message.String()
	// A drag selection answers Escape first, the way the find box does: the key
	// closes the gesture the reader just made instead of canceling the turn behind
	// it.
	if key == keyEscape && m.selection.visible {
		m.clearSelection()

		return m, redrawSelection()
	}
	if key == keyEscape && m.directAgent != nil && !m.directAgent.running &&
		m.composer.Value() == "" {
		m.directAgent = nil
		m.setLayout()

		return m, nil
	}
	if key == "up" && m.composer.AtFirstVisualRow() && m.composer.HistoryUp() {
		m.setLayout()

		return m, nil
	}
	if key == keyDown && m.composer.AtLastVisualRow() && m.composer.HistoryDown() {
		m.setLayout()

		return m, nil
	}
	if command, handled := m.updateTranscriptScrollKey(message); handled {
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
		case actionSubmit:
			return m, m.submit(context)
		case actionFollowUp:
			return m, m.queueMessage(commandFollowUp)
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
		case actionCommand, actionHelp:
			if action == actionCommand {
				m.openCommandPicker()

				return m, nil
			}

			return m, m.printHelp()
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

//nolint:gocyclo // Footer composition follows the explicit route/prompt/picker state machine.
func (m *Model) readyView() tea.View {
	if m.transcriptMode.active {
		return m.transcriptModeView()
	}
	if m.route.kind != routeNone && !m.teamRouteIsInline() {
		return m.routeView()
	}

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
	if prompt := m.promptView(); prompt != "" && !m.goalCommandPickerActive() {
		// The band shares the Composer's text column, so its accent gutter lines
		// up with the transcript's accented blocks instead of the frame edge.
		prompt = insetRows(prompt, timelineInset(m.width))
		if caps.promptRows > 0 {
			prompt = truncateTailHeight(prompt, caps.promptRows)
		}
		promptFooterIndex = len(footer)
		promptContent = prompt
		footer = append(footer, prompt)
	}
	if inline := m.inlineTeamRouteView(); inline != "" {
		footer = append(footer, truncateHeight(inline, max(1, caps.panelRows)))
	}
	if notice := m.directAgentNotice(); notice != "" {
		footer = append(footer, truncateHeight(notice, 1))
	}
	for range conversationGapHeight {
		footer = append(footer, "")
	}
	composerFooterIndex := len(footer)
	footer = append(footer, m.composerBand())
	if m.picker.kind != pickerNone {
		usedHeight := lipgloss.Height(lipgloss.JoinVertical(lipgloss.Left, footer...))
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
	view := m.presentationView(clampFrameTail(content, m.height))
	m.frameHit = frameHitMap{
		painted:    m.sizeReady,
		transcript: max(0, transcriptRows-min(dropped, transcriptRows)),
		topDropped: min(dropped, transcriptRows),
	}
	// presentationView installs the pointer handler for every frame, so the ready
	// view only has to record where its transcript band sat.
	view.Cursor = m.composer.Cursor()
	if m.searchOwnsKeys() {
		view.Cursor = m.search.input.Cursor()
	}
	if (m.prompt.kind != promptNone && !m.goalCommandPickerActive()) || m.pickerHidesComposerCursor() || m.teamPanel.isFocused ||
		(m.teamRouteIsInline() && (m.route.loading || !teamRouteInputStage(m.route.team.stage))) {
		view.Cursor = nil
	}
	if m.prompt.kind == promptQuestion &&
		m.prompt.question.editing != questionEditNone && promptIndex >= 0 {
		if cursorX, cursorY, ok := m.questionEditorOffset(promptContent); ok {
			view.Cursor = m.prompt.question.editor.Cursor()
			if view.Cursor != nil {
				promptOffset := lipgloss.Height(lipgloss.JoinVertical(
					lipgloss.Left,
					parts[:promptIndex]...,
				))
				view.Cursor.X += cursorX
				view.Cursor.Y += promptOffset + cursorY
			}
		}
	}
	m.placePlanReviewCursor(&view, parts, promptContent, promptIndex)
	if view.Cursor != nil {
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

func (m *Model) questionEditorOffset(prompt string) (int, int, bool) {
	return editorOffset(prompt, m.prompt.question.editor.View())
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
		value = modelDisplayName(m.state.Provider, m.state.ModelID)
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
	case statusline.TaskProgress:
		if m.state.Tasks.Total > 0 {
			value = fmt.Sprintf("Tasks %d/%d", m.state.Tasks.Completed, m.state.Tasks.Total)
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
	// A paused reader needs to know that the newest output is off screen and how
	// to get back to it; without this the viewport looks stuck.
	if !m.transcriptScroll.follow && m.transcriptScroll.maxOffset() > 0 {
		extras = append(extras, "scrolled · End for latest")
	}
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
		isStarting:                 m.starting,
		hasBridge:                  m.bridge != nil,
		isCanceling:                m.canceling,
		isResolvingAllowedApproval: m.mainApprovalExecutionStarting(),
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

func (m *Model) renderTranscriptContent(forceBottom bool) {
	m.renderDirty = false
	m.frameRenders++
	// The transcript region changed, so a composed view is stale whatever the last
	// update was, and this invalidation outlives the update until View adopts it.
	m.viewStable = false
	m.viewStale = true
	if m.fullscreen() {
		m.renderManagedTranscript(forceBottom)

		return
	}

	blocks := m.transcriptBlocks()
	m.timeline = renderTimelineContent(
		blocks,
		m.markdown,
		m.width,
		m.theme,
		m.options.NoColor,
	)
	// The seam between terminal-native history and the mutable region is part of
	// the managed content, so the region's rows must include it too.
	gap := m.managedTimelineNeedsLeadingGap(blocks)
	if gap {
		m.timeline = strings.Repeat("\n", conversationGapHeight) + m.timeline
	}

	leading := 0
	if gap {
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
		m.timeline = ""
		m.syncTranscriptStore(blocks, 0)
		m.finishTranscriptFrame(forceBottom)

		return
	}

	frame := m.viewportProjection()
	m.timeline = ""
	m.syncTranscriptEntries(frame.prefixEntries, frame.tailEntries, frame.stable, 0)
	m.finishTranscriptFrame(forceBottom)
}

// finishTranscriptFrame publishes the freshly built region to the reader.
func (m *Model) finishTranscriptFrame(forceBottom bool) {
	m.transcriptScroll.setSource(&m.transcript)
	if forceBottom {
		m.transcriptScroll.gotoBottom()
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
	for _, block := range blocks {
		entries = append(entries, transcriptEntry{
			id:    m.blockIdentity(block),
			live:  blockIsUnsettled(block),
			block: block,
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
	if !m.scrollbackOutput || len(blocks) == 0 || m.timeline == "" {
		return false
	}
	if m.streaming.active && m.streaming.emitted > 0 && blocks[0].kind == blockDraft {
		return false
	}

	return true
}

func (m *Model) timelineBlocks() []timelineBlock {
	blocks := insertCompletionMarkers(m.projectTimeline(m.state, nil), m.completionMarkers)
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
		if block.kind != blockTool || len(block.tools) == 0 {
			continue
		}
		detail := newToolDetailView(block)
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
		case blockTool:
			if len(block.tools) > 0 && block.tools[0].childSessionID != "" {
				return m.openSubagentRoute(block.tools[0].childSessionID)
			}
			detail := newToolDetailView(block)

			return m.openToolDetailRoute(detail)
		case blockUser, blockAssistant, blockDraft, blockPlan, blockQuestion, blockDiagnostic,
			blockChange, blockError, blockCompletion, blockTeam:
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
	if m.starting || m.bridge != nil {
		return contextRunning
	}

	return actionContextFor(m.state)
}

type renderTickMsg struct{}

type scrollbackWriteDoneMsg struct {
	sequence uint64
}

func (m *Model) submit(actionCtx actionContext) tea.Cmd {
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

		return m.prepareSubmission(submissionSteer, snapshot)
	default:
		return nil
	}
}

func (m *Model) queueMessage(kind controllerCommand) tea.Cmd {
	if m.directAgent != nil && m.directAgent.running {
		return nil
	}

	snapshot := m.composer.Snapshot()
	submission := submissionSteer
	if kind == commandFollowUp {
		submission = submissionFollowUp
	}

	return m.prepareSubmission(submission, snapshot)
}

func (m *Model) prepareSubmission(
	kind submissionKind,
	snapshot composerSnapshot,
) tea.Cmd {
	vision := true
	if composerSnapshotHasImages(snapshot) {
		vision = m.controller.Capabilities().Vision
		if !vision {
			m.streamErr = errComposerVisionUnsupported
			m.setLayout()

			return nil
		}
	}
	if composerSnapshotHasFiles(snapshot) {
		m.composerResolveSeq++
		generation := m.composerResolveSeq
		resolveCtx, cancel := context.WithCancel(m.ctx)
		m.composerCancel = cancel
		m.composerResolving = true
		m.streamErr = nil
		m.setLayout()

		return func() tea.Msg {
			message, err := resolveComposerSnapshot(
				resolveCtx,
				snapshot,
				vision,
				m.controller.ResolveWorkspaceFile,
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
			m.streamErr = err
			m.setLayout()
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
	if kind == submissionPrompt {
		if err := m.composer.RecordHistory(snapshot); err != nil {
			m.streamErr = err

			return nil
		}
		m.composer.Reset()
		m.setLayout()
		m.streamErr = nil

		return m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
			return m.controller.Prompt(ctx, message)
		})
	}

	m.composer.Reset()
	m.setLayout()

	return func() tea.Msg {
		var commandErr error
		if kind == submissionFollowUp {
			commandErr = m.controller.FollowUp(message)
		} else {
			commandErr = m.controller.Steer(message)
		}

		return controllerCommandMsg{
			snapshot: snapshot, err: commandErr,
		}
	}
}

func (m *Model) startStream(operation streamOperation) tea.Cmd {
	if m.bridge != nil || m.starting {
		return nil
	}
	m.activity.Reset()
	m.canceling = false
	m.starting = true
	m.setLayout()

	return func() tea.Msg {
		return bridgeStartedMsg{bridge: startBridge(m.ctx, operation)}
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
func (m *Model) flushPending() {
	if len(m.pending) == 0 {
		return
	}

	items := m.pending
	m.pending = m.pending[:0]
	m.reduceStreamBatch(items)
}

// dropPending discards deltas that belong to a projection the model is about to
// replace, so they cannot be applied to the replacement.
func (m *Model) dropPending() {
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

	if !m.advanceParentState(valid) {
		return
	}

	if previousSessionID != m.state.SessionID {
		m.stopTeamWorkerRouteSubscription()
		m.resetTeamProjection(m.state.SessionID)
		m.resetSubagentInteractions()
	}

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

// adoptRuntimeState takes the parent projection from the Runtime that already
// applied these events. The snapshot is a defensive copy, and the Runtime
// sequence must be at least as advanced as the last event observed here.
func (m *Model) adoptRuntimeState(last coding.Event) bool {
	snapshot := m.controller.Snapshot()
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
	switch event.Type {
	case coding.EventSessionNavigated:
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

	m.completionMarkers = appendMarker(m.completionMarkers, completionMarker{
		interactionID:  event.InteractionID,
		afterMessages:  len(m.state.Transcript),
		outcome:        completed.Outcome,
		stop:           completed.Stop,
		durationMillis: completed.DurationMillis,
		model:          m.modelLabel(),
	})
	if len(m.completionMarkers) > maxCompletionMarkers {
		first := len(m.completionMarkers) - maxCompletionMarkers
		m.completionMarkers = append([]completionMarker(nil), m.completionMarkers[first:]...)
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
		}
		m.state = snapshot
	}
	m.syncApprovalPrompt()
	m.clearCompletedDirectAgent()
	m.bridge = nil
	m.starting = false
	m.waiting = false
	m.queued = 0
	m.setLayout()

	return m.commitStableTimeline()
}

func (m *Model) cancelStream() tea.Cmd {
	bridge := m.bridge
	beforeStart := bridge == nil && m.starting
	if beforeStart {
		m.cancelStart = true
	}
	m.canceling = true
	m.setLayout()

	return func() tea.Msg {
		err := m.controller.Cancel()
		if bridge != nil {
			stopCtx, cancel := context.WithTimeout(
				context.WithoutCancel(m.ctx),
				controllerCloseTimeout,
			)
			err = errors.Join(err, bridge.stop(stopCtx))
			cancel()
		}

		return cancelResultMsg{bridge: bridge, beforeStart: beforeStart, err: err}
	}
}

func (m *Model) stopBridge(bridge *eventBridge) tea.Cmd {
	return func() tea.Msg {
		stopCtx, cancel := context.WithTimeout(
			context.WithoutCancel(m.ctx),
			controllerCloseTimeout,
		)
		defer cancel()

		return cancelResultMsg{bridge: bridge, err: bridge.stop(stopCtx)}
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

	return m, m.afterScrollback(commit, wait, render, refresh)
}

// reduceObservedEvent applies one already-observed event. Batched updates use
// reduceStreamBatch; this single-event entry point remains for callers that hold
// exactly one event, such as mode control waiting for its subscribed event.
func (m *Model) reduceObservedEvent(event coding.Event) {
	m.reduceStreamBatch([]streamItem{{event: event}})
}

// reduceChildEvent projects one child-Session event into the TUI's own child
// state. A parent subscription does not carry child Runtime States, so child
// projections keep using the shared transition table.
func (m *Model) reduceChildEvent(event coding.Event) {
	state := m.childStates[event.SessionID]
	wasAtBottom := m.route.kind == routeChild && m.route.childKind == childSubagent &&
		m.route.childSessionID == event.SessionID &&
		m.route.offset >= m.subagentRouteMaximumOffset()
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
		if wasAtBottom {
			m.route.offset = m.subagentRouteMaximumOffset()
		} else {
			m.route.offset = min(m.route.offset, m.subagentRouteMaximumOffset())
		}
	}
}

func cloneChildStates(values map[string]coding.State) map[string]coding.State {
	cloned := make(map[string]coding.State, len(values))
	for childSessionID, state := range values {
		cloned[childSessionID] = state.Clone()
	}

	return cloned
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
