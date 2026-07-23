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
	"github.com/rsbin/pips/internal/coding/subagent"
)

const (
	defaultWidth          = 80
	defaultHeight         = 24
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
	kind controllerCommand
	text string
	err  error
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

	controller        Controller
	state             coding.State
	composer          textarea.Model
	markdown          *markdownRenderer
	theme             colorTheme
	timeline          string
	scrollback        scrollbackCursor
	scrollbackOutput  bool
	streaming         streamProjection
	renderWait        bool
	bridge            *eventBridge
	starting          bool
	cancelStart       bool
	waiting           bool
	streamErr         error
	queued            int
	canceling         bool
	exitArmed         bool
	bannerPrinted     bool
	commandPicker     commandPickerState
	sessionPicker     sessionPickerState
	sessionPickerSeq  uint64
	overlay           overlayState
	overlaySeq        uint64
	completionMarkers []completionMarker
	activity          activityIndicator
	refreshCursor     bool
	cursorRefreshSeq  uint64
}

func newModel(ctx context.Context, options Options) *Model {
	lifecycle := lifecycleTrust
	if options.Trusted {
		lifecycle = lifecycleLoading
	}

	composer := textarea.New()
	composer.Prompt = ""
	composer.SetPromptFunc(inputPromptWidth, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return inputArrow + " "
		}

		return ""
	})
	composer.Placeholder = ""
	composer.ShowLineNumbers = false
	composer.DynamicHeight = true
	composer.MinHeight = 1
	composer.MaxHeight = composerMaxLines
	composer.MaxContentHeight = 200
	composer.SetVirtualCursor(false)
	composer.SetWidth(defaultWidth)
	composer.SetStyles(composerStyles(themeDark, options.NoColor))

	model := &Model{
		ctx:       ctx,
		options:   options,
		lifecycle: lifecycle,
		width:     defaultWidth,
		height:    defaultHeight,
		composer:  composer,
		markdown:  newMarkdownRenderer(markdownCacheCapacity),
		theme:     themeDark,
		activity:  newActivityIndicator(),
	}
	model.setLayout()

	return model
}

// Init starts bootstrap immediately for an already trusted Workspace.
func (m *Model) Init() tea.Cmd {
	commands := make([]tea.Cmd, 0, 2)
	if !m.options.NoColor {
		commands = append(commands, tea.RequestBackgroundColor)
	}
	if m.lifecycle == lifecycleLoading {
		commands = append(commands, m.bootstrap(true))
	}

	return tea.Batch(commands...)
}

// Update applies terminal input and asynchronous bootstrap results.
//
//nolint:funlen,gocyclo // The sealed Tea message union stays visible in one dispatcher.
func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = message.Width
		m.height = message.Height
		m.setLayout()
		m.rerenderTranscript(false)

		return m, nil
	case tea.BackgroundColorMsg:
		if message.IsDark() {
			m.theme = themeDark
		} else {
			m.theme = themeLight
		}
		m.composer.SetStyles(composerStyles(m.theme, m.options.NoColor))
		if m.sessionPicker.open {
			m.sessionPicker.search.SetStyles(sessionSearchStyles(m.theme, m.options.NoColor))
		}
		m.rerenderTranscript(false)

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
		m.resetScrollback()
		m.lifecycle = lifecycleReady
		m.syncApprovalOverlay()
		m.setLayout()
		commit := m.commitStartupOutput()

		return m, tea.Sequence(commit, tea.Batch(m.composer.Focus(), m.continueIfPaused()))
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
				m.composer.SetValue(message.text)
			}
		} else {
			m.queued++
		}
		m.setLayout()
		return m, m.commitStableTimeline()
	case cancelResultMsg:
		if message.beforeStart {
			m.streamErr = errors.Join(m.streamErr, message.err)
			m.state = m.controller.Snapshot()
			m.syncApprovalOverlay()
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
		m.state = m.controller.Snapshot()
		m.syncApprovalOverlay()
		m.setLayout()
		return m, m.commitStableTimeline()
	case overlayDataMsg:
		if message.kind != m.overlay.kind ||
			message.generation != m.overlay.generation ||
			(message.childSessionID != "" &&
				message.childSessionID != m.overlay.childSessionID) {
			return m, nil
		}
		if message.background {
			return m.applyAgentDetailRefresh(message)
		}
		m.overlay.loading = false
		m.overlay.err = message.err
		m.overlay.tree = message.tree.Clone()
		m.overlay.preview = message.preview
		if message.hasAgents {
			m.overlay.agents = message.agents
			m.overlay.cursor = 0
		}
		if message.hasDetail {
			detail := message.detail
			m.overlay.agentDetail = &detail
		}
		m.overlay.cursor = 0
		if m.overlay.refreshPending && m.overlay.agentDetail != nil {
			m.overlay.refreshPending = false

			return m, m.refreshAgentDetail()
		}

		return m, nil
	case sessionPickerDataMsg:
		if !m.sessionPicker.open || message.generation != m.sessionPicker.generation {
			return m, nil
		}
		m.sessionPicker.loading = false
		m.sessionPicker.err = message.err
		m.sessionPicker.sessions = message.sessions
		m.sessionPicker.cursor = 0

		return m, nil
	case controlResultMsg:
		commandControl := m.commandPicker.open && m.commandPicker.controlling
		sessionControl := m.sessionPicker.open && m.sessionPicker.controlling
		switch {
		case commandControl:
			m.commandPicker.loading = false
			m.commandPicker.controlling = false
		case sessionControl:
			m.sessionPicker.loading = false
			m.sessionPicker.controlling = false
		default:
			m.overlay.loading = false
		}
		m.state = m.controller.Snapshot()
		if message.err != nil {
			switch {
			case commandControl:
				m.commandPicker.err = message.err
			case sessionControl:
				m.sessionPicker.err = message.err
			default:
				m.overlay.err = message.err
			}
			m.renderTranscript(false)

			return m, nil
		}
		switch message.operation {
		case operationNew, operationResume, operationFork:
			m.completionMarkers = nil
			m.resetScrollback()
		case operationModel, operationReload:
		}
		switch {
		case commandControl:
			m.closeCommandPicker(false)
		case sessionControl:
			m.closeSessionPicker(false)
		default:
			m.commandPicker = commandPickerState{}
		}
		m.overlay = overlayState{}
		m.streamErr = nil
		m.syncApprovalOverlay()
		var commit tea.Cmd
		switch message.operation {
		case operationNew:
			commit = m.commitNewSessionOutput()
		default:
			commit = m.commitStableTimeline()
		}

		return m, tea.Sequence(commit, m.continueIfPaused())
	case exitResetMsg:
		m.exitArmed = false

		return m, nil
	case tea.MouseWheelMsg:
		// Mouse reporting stays disabled. Native terminal selection and
		// scrollback consume drag and wheel gestures before they reach the model.
		return m, nil
	case tea.MouseMsg:
		return m, nil
	case tea.KeyPressMsg:
		return m.updateKey(message)
	case renderTickMsg:
		m.renderWait = false
		m.renderTranscript(false)

		return m, nil
	case scrollbackRenderReadyMsg:
		return m, nil
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
		if _, visible := m.activityStatus(); !visible {
			return m, nil
		}

		return m, m.activity.Update(message)
	default:
		if m.lifecycle == lifecycleReady {
			if m.sessionPicker.open {
				var command tea.Cmd
				m.sessionPicker.search, command = m.sessionPicker.search.Update(message)

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
	case "left", "right", keyTab:
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
		accent := lipgloss.Color("#5FAFFF")
		muted := lipgloss.Color("#8B949E")
		if m.theme == themeLight {
			accent = lipgloss.Color("#0969DA")
			muted = lipgloss.Color("#57606A")
		}
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

	color := lipgloss.Color("#5FAFFF")
	if !selected {
		color = lipgloss.Color("#8B949E")
	}
	if m.theme == themeLight {
		color = lipgloss.Color("#0969DA")
		if !selected {
			color = lipgloss.Color("#57606A")
		}
	}

	return lipgloss.NewStyle().Bold(true).Foreground(color).Render(line)
}

//nolint:gocyclo,nestif,gocritic // Contextual input precedence is an explicit product state machine.
func (m *Model) updateReadyKey(message tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.overlay.kind != overlayNone {
		return m.updateOverlayKey(message)
	}
	if m.sessionPicker.open {
		return m.updateSessionPickerKey(message)
	}
	if m.commandPicker.open {
		return m.updateCommandPickerKey(message)
	}

	key := message.String()
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
		case actionCommand, actionHelp:
			if action == actionCommand {
				m.openCommandPicker()

				return m, nil
			}

			return m, m.openOverlay(overlayHelp)
		}

		return m, nil
	}

	var command tea.Cmd
	m.composer, command = m.composer.Update(message)
	m.setLayout()

	return m, command
}

func (m *Model) readyView() tea.View {
	if m.sessionPicker.open {
		return m.sessionPickerView()
	}
	if m.overlay.kind == overlayAgents {
		return m.agentsView()
	}

	separator := strings.Repeat("─", max(1, m.width))
	if m.options.NoColor {
		separator = strings.Repeat("-", max(1, m.width))
	} else {
		separator = lipgloss.NewStyle().Foreground(paletteFor(m.theme).separator).Render(separator)
	}

	footer := make([]string, 0, 7)
	if activity := m.activityLine(); activity != "" {
		for range conversationGapHeight {
			footer = append(footer, "")
		}
		footer = append(footer, activity)
	}
	for range conversationGapHeight {
		footer = append(footer, "")
	}
	footer = append(footer, separator)
	composerFooterIndex := len(footer)
	footer = append(footer, m.composer.View(), separator)
	if m.commandPicker.open {
		usedHeight := lipgloss.Height(lipgloss.JoinVertical(lipgloss.Left, footer...))
		availableRows := max(1, m.height-usedHeight)
		footer = append(footer, m.commandPickerView(availableRows))
	} else {
		footer = append(footer, ansi.Truncate(m.statusLine(), max(1, m.width), "…"))
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
	parts = append(parts, footer...)
	composerOffset := lipgloss.Height(lipgloss.JoinVertical(
		lipgloss.Left,
		parts[:composerIndex]...,
	))
	content := lipgloss.JoinVertical(lipgloss.Left, parts...)
	view := tea.NewView(content)
	view.SetContent(m.renderOverlay(content))
	view.AltScreen = false
	view.MouseMode = tea.MouseModeNone
	view.WindowTitle = appTitle
	view.Cursor = m.composer.Cursor()
	if m.overlay.kind != overlayNone {
		view.Cursor = nil
	}
	if view.Cursor != nil {
		view.Cursor.Y += composerOffset
		if m.refreshCursor {
			view.Cursor.Blink = !view.Cursor.Blink
		}
	}

	return view
}

func (m *Model) statusLine() string {
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
	if m.state.LastError != nil && m.state.Interaction.Outcome != coding.InteractionCanceled {
		phaseLabel = "error"
	}
	values := []string{
		workspaceName,
		sessionLabel,
		fmt.Sprintf("%s/%s", m.state.Provider, m.state.ModelID),
		phaseLabel,
	}
	if !m.options.NoColor {
		palette := paletteFor(m.theme)
		values[0] = lipgloss.NewStyle().Bold(true).Foreground(palette.workspace).Render(values[0])
		values[1] = lipgloss.NewStyle().Bold(true).Foreground(palette.session).Render(values[1])
		values[2] = lipgloss.NewStyle().Foreground(palette.model).Render(values[2])
		values[3] = lipgloss.NewStyle().Bold(true).Foreground(
			phaseColor(m.state, phase, palette),
		).Render(values[3])
	}
	separator := "  ·  "
	if !m.options.NoColor {
		separator = lipgloss.NewStyle().Foreground(paletteFor(m.theme).muted).Render(separator)
	}
	status := strings.Join(values, separator)
	extras := m.statusExtras()
	if len(extras) > 0 {
		status += separator + strings.Join(extras, separator)
	}

	return status
}

func (m *Model) statusExtras() []string {
	extras := make([]string, 0, 3)
	if m.queued > 0 {
		extras = append(extras, fmt.Sprintf("%d queued", m.queued))
	}
	if m.exitArmed {
		extras = append(extras, "press Ctrl+C again to quit")
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
	m.composer.SetWidth(width)
	composerHeight := max(1, min(composerMaxLines, m.composer.Height()))
	m.composer.SetHeight(composerHeight)
	if m.sessionPicker.open {
		innerWidth := max(1, width-4)
		m.sessionPicker.search.SetWidth(max(
			1,
			innerWidth-ansi.StringWidth(sessionPickerSearchPrompt),
		))
	}
}

func (m *Model) activityStatus() (activityStatus, bool) {
	return resolveActivity(activityContext{
		state:       m.state,
		isStarting:  m.starting,
		hasBridge:   m.bridge != nil,
		isCanceling: m.canceling,
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
	if m.state.Phase != coding.PhasePaused && (m.starting || m.bridge != nil || m.canceling) {
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
		if latest.Call.Name == subagent.ToolName {
			childSessionID := subagentChildSessionID(latest, m.state.Subagents)
			if childSessionID != "" {
				return m.openAgentDetail(childSessionID)
			}
		}
	}

	blocks := projectTimeline(m.state)
	for _, block := range slices.Backward(blocks) {
		switch block.kind {
		case blockSubagent:
			if block.id != "" {
				return m.openAgentDetail(block.id)
			}
			if len(block.tools) > 0 {
				detail := newToolDetailView(block)
				m.overlay = overlayState{
					kind: overlayToolDetail, toolDetail: &detail,
				}
			}

			return nil
		case blockTool:
			detail := newToolDetailView(block)
			m.overlay = overlayState{
				kind: overlayToolDetail, toolDetail: &detail,
			}

			return nil
		case blockUser, blockAssistant, blockDraft, blockDiagnostic,
			blockChange, blockError, blockCompletion:
		}
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

type scrollbackRenderReadyMsg struct{}

type scrollbackCursorRefreshDoneMsg struct {
	sequence uint64
}

func (m *Model) submit(actionCtx actionContext) tea.Cmd {
	text := strings.TrimSpace(m.composer.Value())
	if text == "" {
		return nil
	}

	switch actionCtx {
	case contextIdle:
		m.composer.Reset()
		m.setLayout()
		m.streamErr = nil

		return m.startStream(func(ctx context.Context) iter.Seq2[coding.Event, error] {
			return m.controller.Prompt(ctx, ai.UserText(text))
		})
	case contextRunning:
		return m.queueMessage(commandSteer)
	default:
		return nil
	}
}

func (m *Model) queueMessage(kind controllerCommand) tea.Cmd {
	text := strings.TrimSpace(m.composer.Value())
	if text == "" {
		return nil
	}

	m.composer.Reset()
	m.setLayout()

	return func() tea.Msg {
		message := ai.UserText(text)
		var err error
		if kind == commandFollowUp {
			err = m.controller.FollowUp(message)
		} else {
			err = m.controller.Steer(message)
		}

		return controllerCommandMsg{kind: kind, text: text, err: err}
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

	m.reduceStreamItem(message.item)
	m.setLayout()
	refresh := m.invalidateAgentDetail(message.item)

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
		m.bridge.once.Do(m.bridge.cancel)

		return
	}

	m.state = next
	if item.event.Type == coding.EventSessionNavigated ||
		item.event.Type == coding.EventCompactionCompleted {
		m.resetScrollback()
	}
	m.recordCompletion(item.event)
	m.syncApprovalOverlay()
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
		durationMillis: completed.DurationMillis,
	})
	if len(m.completionMarkers) > maxCompletionMarkers {
		first := len(m.completionMarkers) - maxCompletionMarkers
		m.completionMarkers = append([]completionMarker(nil), m.completionMarkers[first:]...)
	}
}

func (m *Model) finishStream() tea.Cmd {
	snapshot := m.controller.Snapshot()
	if m.streamErr == nil && snapshot.Sequence != m.state.Sequence {
		m.streamErr = fmt.Errorf(
			"coding tui: event stream stopped at sequence %d; runtime is at %d",
			m.state.Sequence,
			snapshot.Sequence,
		)
	}
	m.state = snapshot
	m.syncApprovalOverlay()
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
