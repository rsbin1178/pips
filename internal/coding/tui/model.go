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
	"unicode/utf8"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	codingclipboard "github.com/rsbin/pips/internal/coding/clipboard"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/runtimecontrol"
	"github.com/rsbin/pips/internal/coding/statusline"
)

var errStatusLinePersistenceUnavailable = errors.New("status-line persistence is unavailable")

const (
	defaultWidth          = 80
	defaultHeight         = 24
	statusHorizontalInset = 2
	composerMaxLines      = 8
	conversationGapHeight = 1
	renderFrame           = 33 * time.Millisecond
	exitConfirmTime       = 2 * time.Second
	maxCompletionMarkers  = 256
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
	ctx       context.Context //nolint:containedctx // Program context owns every asynchronous command.
	options   Options
	lifecycle lifecycle
	width     int
	height    int
	allow     bool
	err       error

	controller           Controller
	state                coding.State
	childStates          map[string]coding.State
	composer             composerState
	markdown             *markdownRenderer
	theme                colorTheme
	themeSelection       string
	themeRegistry        themeRegistry
	themeBackgroundKnown bool
	themeIsDark          bool
	themeDiagnostics     []themeDiagnostic
	timeline             string
	scrollback           scrollbackCursor
	scrollbackOutput     bool
	streaming            streamProjection
	renderWait           bool
	bridge               *eventBridge
	subscription         *subscriptionBridge
	subscriptionMode     bool
	starting             bool
	cancelStart          bool
	waiting              bool
	streamErr            error
	composerResolving    bool
	composerResolveSeq   uint64
	composerCancel       context.CancelFunc
	clipboardLoading     bool
	clipboardGeneration  uint64
	clipboardCancel      context.CancelFunc
	queued               int
	canceling            bool
	exitArmed            bool
	bannerPrinted        bool
	picker               pickerState
	pickerSeq            uint64
	route                routeState
	routeSeq             uint64
	teamProjection       teamProjectionState
	teamInteractions     teamInteractionQueueState
	teamPanel            teamPanelState
	presentation         presentationState
	prompt               promptState
	promptSeq            uint64
	completionMarkers    []completionMarker
	worktreeLoading      bool
	worktreeGeneration   uint64
	worktreeCancel       context.CancelFunc
	worktreeSummary      string
	activity             activityIndicator
	statusLineItems      []statusline.Item
	refreshCursor        bool
	cursorRefreshSeq     uint64
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
		ctx:             ctx,
		options:         options,
		lifecycle:       lifecycle,
		width:           defaultWidth,
		height:          defaultHeight,
		composer:        composer,
		markdown:        newMarkdownRenderer(markdownCacheCapacity),
		theme:           themeDark,
		themeSelection:  config.ThemeAuto,
		themeRegistry:   loadThemeRegistry(""),
		themeIsDark:     true,
		activity:        newActivityIndicator(),
		childStates:     make(map[string]coding.State),
		statusLineItems: slices.Clone(statusLineItems),
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
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = max(1, message.Width)
		m.height = message.Height
		m.setLayout()
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
		m.state = message.controller.Snapshot()
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
		commit := m.commitStartupOutput()
		activity := m.startActivityClock(false)

		return m, tea.Sequence(commit, tea.Batch(
			m.composer.Focus(), m.startSubscription(), m.loadPlanReviewIfNeeded(), activity,
		))
	case subscriptionStartedMsg:
		activityWasVisible := m.activityClockVisible()
		m.subscriptionMode = message.supported
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
		m.state = message.observation.State
		if previousSessionID != m.state.SessionID {
			m.stopTeamWorkerRouteSubscription()
			m.resetTeamProjection(m.state.SessionID)
		}
		m.childStates = cloneChildStates(message.observation.Children)
		m.syncApprovalPrompt()
		m.setLayout()
		commit := m.commitStableTimeline()
		wait := tea.Batch(
			message.bridge.wait(),
			m.continueIfPaused(),
			m.refreshTeamProjectionSnapshot(),
			m.loadPlanReviewIfNeeded(),
			m.startActivityClock(activityWasVisible),
		)
		if commit != nil {
			return m, tea.Sequence(commit, wait)
		}

		return m, wait
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
		if m.prompt.kind != promptPlanReview ||
			message.generation != m.prompt.generation ||
			message.requestID != m.prompt.planReview.request.ID {
			return m, nil
		}
		m.prompt.planReview.loading = false
		m.prompt.planReview.err = message.err
		if message.err == nil {
			if message.document.Revision != m.prompt.planReview.request.Revision ||
				message.document.Size != m.prompt.planReview.request.Size {
				m.prompt.planReview.err = errors.New("plan revision changed; resubmit the current Plan")
			} else {
				m.prompt.planReview.document = message.document
			}
		}
		m.setLayout()

		return m, nil
	case bridgeImageMsg:
		return m.updateBridgeImage(message)
	case cancelResultMsg:
		if message.beforeStart {
			m.streamErr = errors.Join(m.streamErr, message.err)
			if !m.subscriptionMode {
				m.state = m.controller.Snapshot()
			}
			m.syncApprovalPrompt()
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
		if !m.subscriptionMode {
			m.state = m.controller.Snapshot()
		}
		m.syncApprovalPrompt()
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
			m.stopTeamWorkerRouteSubscription()
			m.stopSubscription()
			m.state = m.controller.Snapshot()
		} else if !m.subscriptionMode {
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
			m.resetTeamProjection(m.state.SessionID)
			m.resetScrollback()
		case operationModel, operationReload, operationMode, operationPermissions:
		}
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
			if m.route.kind == routeSessions || m.route.kind == routeSkills {
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
			commands := []tea.Cmd{commit, m.startSubscription()}
			if message.operation == operationResume {
				commands = append(commands, m.probeTeamRecoveryAfterResume())
			}

			return m, tea.Sequence(commands...)
		}

		return m, tea.Sequence(commit, m.continueIfPaused())
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
		if message.err != nil {
			body := strings.TrimPrefix(m.statusContent(), "Status\n\n") +
				"\nRepository: unavailable (Git status: " + safeError(message.err) + ")"

			return m, m.printInspection("Status", body)
		}
		m.worktreeSummary = compactWorktreeSummary(message.status)

		return m, m.printInspection(
			"Status",
			strings.TrimPrefix(m.statusContent(), "Status\n\n"),
		)
	case exitResetMsg:
		m.exitArmed = false

		return m, nil
	case tea.MouseWheelMsg:
		// Mouse reporting stays disabled. Native terminal selection and
		// scrollback consume drag and wheel gestures before they reach the model.
		return m, nil
	case tea.MouseMsg:
		return m, nil
	case tea.PasteMsg:
		if m.lifecycle != lifecycleReady {
			return m, nil
		}
		if m.route.kind == routeSessions || m.route.kind == routeSkills {
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
		m.renderTranscript(false)

		return m, nil
	case scrollbackRenderReadyMsg:
		return m, nil
	case scrollbackWriteDoneMsg:
		return m, m.finishScrollbackWrite(message.sequence)
	case scrollbackCursorRefreshMsg:
		if message.sequence != m.cursorRefreshSeq {
			return m, nil
		}
		m.refreshCursor = true

		return m, tea.Tick(renderFrame, func(time.Time) tea.Msg {
			return scrollbackCursorRefreshDoneMsg(message)
		})
	case scrollbackCursorRefreshDoneMsg:
		if message.sequence != m.cursorRefreshSeq {
			return m, nil
		}
		m.refreshCursor = false

		return m, nil
	case activityTickMsg:
		if !m.activityClockVisible() {
			return m, nil
		}

		return m, m.activity.Update(message)
	default:
		if m.lifecycle == lifecycleReady {
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

// View renders the inline lifecycle shell or ready chat layout.
func (m *Model) View() tea.View {
	var content string

	switch m.lifecycle {
	case lifecycleTrust:
		content = m.trustView()
	case lifecycleLoading:
		content = "Starting Pips…"
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
	inlineTeamRoute := m.teamRouteIsInline()
	if m.route.kind != routeNone && !inlineTeamRoute {
		return m.updateRouteKey(message)
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
	key := message.String()
	if key == "up" && m.composer.AtFirstVisualRow() && m.composer.HistoryUp() {
		m.setLayout()

		return m, nil
	}
	if key == keyDown && m.composer.AtLastVisualRow() && m.composer.HistoryDown() {
		m.setLayout()

		return m, nil
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
			if context == contextRunning {
				return m, m.cancelStream()
			}
			if m.composer.Value() != "" {
				m.composer.Reset()
				m.setLayout()
				m.exitArmed = false
			} else if m.exitArmed {
				return m, tea.Quit
			} else {
				m.exitArmed = true

				return m, tea.Tick(exitConfirmTime, func(time.Time) tea.Msg {
					return exitResetMsg{}
				})
			}
		case actionSubmit:
			return m, m.submit(context)
		case actionFollowUp:
			return m, m.queueMessage(commandFollowUp)
		case actionToggleTool:
			return m, m.toggleLatestTool()
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

//nolint:gocyclo // Footer composition follows the explicit route/prompt/picker state machine.
func (m *Model) readyView() tea.View {
	if m.route.kind != routeNone && !m.teamRouteIsInline() {
		return m.routeView()
	}

	footer := make([]string, 0, 5)
	promptFooterIndex := -1
	promptContent := ""
	if activity := m.activityLine(); activity != "" {
		for range conversationGapHeight {
			footer = append(footer, "")
		}
		footer = append(footer, activity)
	}
	if prompt := m.promptView(); prompt != "" {
		promptFooterIndex = len(footer)
		promptContent = prompt
		footer = append(footer, prompt)
	}
	if inline := m.inlineTeamRouteView(); inline != "" {
		footer = append(footer, inline)
	}
	for range conversationGapHeight {
		footer = append(footer, "")
	}
	composerFooterIndex := len(footer)
	footer = append(footer, m.composerBox())
	if m.picker.kind != pickerNone {
		usedHeight := lipgloss.Height(lipgloss.JoinVertical(lipgloss.Left, footer...))
		availableRows := max(1, m.height-usedHeight)
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
	} else {
		footer = append(footer, m.statusLineView())
		if panel := m.teamPanelView(); panel != "" {
			footer = append(footer, panel)
		}
	}

	parts := make([]string, 0, len(footer)+1)
	timelineHeight := max(
		0,
		m.height-lipgloss.Height(lipgloss.JoinVertical(lipgloss.Left, footer...)),
	)
	if timeline := truncateTailHeight(m.timeline, timelineHeight); timeline != "" {
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
	view := tea.NewView(content)
	view.SetContent(content)
	view.AltScreen = false
	view.MouseMode = tea.MouseModeNone
	view.WindowTitle = appTitle
	view.Cursor = m.composer.Cursor()
	if m.prompt.kind != promptNone || m.pickerHidesComposerCursor() || m.teamPanel.isFocused ||
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
	if m.prompt.kind == promptPlanReview && m.prompt.planReview.editing && promptIndex >= 0 {
		if cursorX, cursorY, ok := editorOffset(promptContent, m.prompt.planReview.editor.View()); ok {
			view.Cursor = m.prompt.planReview.editor.Cursor()
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
	if view.Cursor != nil {
		cursorX, cursorY := m.composerBoxCursorOffset()
		view.Cursor.X += cursorX
		view.Cursor.Y += composerOffset + cursorY
		if m.refreshCursor {
			view.Cursor.Blink = !view.Cursor.Blink
		}
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
		value = fmt.Sprintf("%s/%s", m.state.Provider, m.state.ModelID)
		style = style.Foreground(palette.model)
	case statusline.ContextUsed:
		window := m.state.ContextWindow
		if window > 0 {
			percent := min(100, max(0, int(
				(float64(m.state.ContextTokens)/float64(window))*100,
			)))
			value = fmt.Sprintf("Context %d%% used", percent)
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
	extras := make([]string, 0, 3)
	if m.queued > 0 {
		extras = append(extras, fmt.Sprintf("%d queued", m.queued))
	}
	if m.exitArmed {
		extras = append(extras, "press Ctrl+C again to quit")
	}
	if m.worktreeLoading {
		extras = append(extras, "inspecting Git")
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
	composerHeight := max(1, min(composerMaxLines, m.composer.Height()))
	m.composer.SetHeight(composerHeight)
	if m.prompt.kind == promptQuestion {
		m.prompt.question.editor.SetWidth(max(1, width-4))
	}
	if m.prompt.kind == promptPlanReview {
		m.prompt.planReview.editor.SetWidth(max(1, width-4))
	}
	if m.route.kind == routeSessions || m.route.kind == routeSkills {
		m.route.search.SetWidth(routeSearchInputWidth(width))
	}
}

func (m *Model) composerBox() string {
	return m.composerBoxContent(m.composer.View())
}

func (m *Model) composerBoxContent(content string) string {
	if !m.hasComposerBox() {
		return content
	}

	style := lipgloss.NewStyle().
		Width(max(1, m.width)).
		Padding(0, 1).
		Border(lipgloss.RoundedBorder(), true)
	if m.options.NoColor {
		style = style.Border(lipgloss.NormalBorder(), true)
	} else {
		style = style.BorderForeground(paletteFor(m.theme).separator)
	}

	return style.Render(content)
}

func (m *Model) hasComposerBox() bool {
	return m.width >= 24
}

func composerEditorWidth(width int) int {
	if width >= 24 {
		return max(1, width-4)
	}

	return max(1, width)
}

func (m *Model) composerBoxCursorOffset() (int, int) {
	if !m.hasComposerBox() {
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

	return ansi.Truncate(
		m.activity.View(status, m.theme, m.options.NoColor),
		max(1, m.width),
		"…",
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
	m.renderTranscriptContent(forceBottom, true)
}

func (m *Model) rerenderTranscript(forceBottom bool) {
	m.renderTranscriptContent(forceBottom, false)
}

func (m *Model) renderTranscriptContent(forceBottom, newContent bool) {
	_ = forceBottom
	_ = newContent
	blocks := m.activeTimelineBlocks()
	m.timeline = renderTimelineContent(
		blocks,
		m.markdown,
		m.width,
		m.theme,
		m.options.NoColor,
	)
	if m.managedTimelineNeedsLeadingGap(blocks) {
		m.timeline = strings.Repeat("\n", conversationGapHeight) + m.timeline
	}
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
	blocks := insertCompletionMarkers(projectTimeline(m.state), m.completionMarkers)
	if m.streamErr != nil {
		blocks = append(blocks, timelineBlock{
			kind: blockError, title: "Operation", body: safeError(m.streamErr),
		})
	}

	return blocks
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
		case blockUser, blockAssistant, blockDraft, blockQuestion, blockDiagnostic,
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

func (m *Model) requestRender() tea.Cmd {
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

type scrollbackCursorRefreshMsg struct {
	sequence uint64
}

type scrollbackWriteDoneMsg struct {
	sequence uint64
}

type scrollbackRenderReadyMsg struct{}

type scrollbackCursorRefreshDoneMsg struct {
	sequence uint64
}

func (m *Model) submit(actionCtx actionContext) tea.Cmd {
	snapshot := m.composer.Snapshot()
	switch actionCtx {
	case contextIdle:
		return m.prepareSubmission(submissionPrompt, snapshot)
	case contextRunning:
		return m.prepareSubmission(submissionSteer, snapshot)
	default:
		return nil
	}
}

func (m *Model) queueMessage(kind controllerCommand) tea.Cmd {
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
	if !message.ok {
		return m, m.finishStream()
	}

	if m.subscriptionMode && message.item.err == nil {
		m.waiting = true

		return m, m.bridge.wait()
	}

	m.reduceStreamItem(message.item)
	m.setLayout()
	refresh := tea.Batch(
		m.invalidateAgentDetail(message.item),
		m.invalidateTeamProjection(message.item.event),
		m.loadPlanReviewIfNeeded(),
	)

	m.waiting = true
	wait := m.bridge.wait()
	commit := m.commitStableTimeline()
	if commit != nil {
		return m, tea.Sequence(commit, tea.Batch(wait, refresh))
	}

	return m, tea.Batch(wait, m.requestRender(), refresh)
}

func (m *Model) reduceStreamItem(item streamItem) {
	if item.err != nil {
		m.streamErr = item.err

		return
	}

	next, err := coding.Reduce(m.state, item.event)
	if err != nil {
		m.streamErr = err
		if m.bridge != nil {
			m.bridge.once.Do(m.bridge.cancel)
		}

		return
	}

	previousSessionID := m.state.SessionID
	m.state = next
	if previousSessionID != m.state.SessionID {
		m.stopTeamWorkerRouteSubscription()
		m.resetTeamProjection(m.state.SessionID)
	}
	m.trackTeamLifecycleEvent(item.event)
	m.applyTeamInteractionEvent(item.event)
	if item.event.Type == coding.EventSessionNavigated ||
		item.event.Type == coding.EventCompactionCompleted {
		m.resetScrollback()
	}
	m.recordCompletion(item.event)
	m.syncApprovalPrompt()
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

	m.completionMarkers = append(m.completionMarkers, completionMarker{
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

	return func() tea.Msg {
		return startSubscription(m.controller)
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

	activityWasVisible := m.activityClockVisible()
	m.reduceObservedEvent(message.record.Event)
	m.setLayout()
	refresh := tea.Batch(
		m.invalidateAgentDetail(streamItem{event: message.record.Event}),
		m.invalidateTeamProjection(message.record.Event),
		m.loadPlanReviewIfNeeded(),
		m.startActivityClock(activityWasVisible),
	)
	wait := message.bridge.wait()
	commit := m.commitStableTimeline()
	if commit != nil {
		return m, tea.Sequence(commit, tea.Batch(wait, refresh))
	}

	return m, tea.Batch(wait, m.requestRender(), refresh)
}

func (m *Model) reduceObservedEvent(event coding.Event) {
	if m.state.SessionID == "" || event.SessionID == m.state.SessionID {
		m.reduceStreamItem(streamItem{event: event})
		m.updateSubagentRouteSummary(event)

		return
	}

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
	if m == nil || m.subscription == nil {
		return
	}

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
