//nolint:wsl_v5 // The MVU state machine keeps transition effects adjacent.
package tui

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
)

const (
	defaultWidth     = 80
	defaultHeight    = 24
	composerMaxLines = 8
	renderFrame      = 33 * time.Millisecond
	exitConfirmTime  = 2 * time.Second
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

	controller  Controller
	state       coding.State
	viewport    viewport.Model
	composer    textarea.Model
	markdown    *markdownRenderer
	theme       colorTheme
	unseen      int
	renderWait  bool
	bridge      *eventBridge
	starting    bool
	cancelStart bool
	waiting     bool
	streamErr   error
	queued      int
	exitArmed   bool
	overlay     overlayState
	expanded    map[string]bool
}

func newModel(ctx context.Context, options Options) *Model {
	lifecycle := lifecycleTrust
	if options.Trusted {
		lifecycle = lifecycleLoading
	}

	transcript := viewport.New(
		viewport.WithWidth(defaultWidth),
		viewport.WithHeight(defaultHeight-5),
	)
	transcript.FillHeight = true
	transcript.SoftWrap = true
	transcript.MouseWheelEnabled = true
	transcript.MouseWheelDelta = 3

	composer := textarea.New()
	composer.Prompt = "› "
	composer.Placeholder = "Ask Pips to inspect, explain, or change the code…"
	composer.ShowLineNumbers = false
	composer.DynamicHeight = true
	composer.MinHeight = 1
	composer.MaxHeight = composerMaxLines
	composer.MaxContentHeight = 200
	composer.SetVirtualCursor(false)
	composer.SetWidth(defaultWidth)
	if options.NoColor {
		composer.SetStyles(textarea.Styles{})
	}

	model := &Model{
		ctx:       ctx,
		options:   options,
		lifecycle: lifecycle,
		width:     defaultWidth,
		height:    defaultHeight,
		viewport:  transcript,
		composer:  composer,
		markdown:  newMarkdownRenderer(markdownCacheCapacity),
		theme:     themeDark,
		expanded:  make(map[string]bool),
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
//nolint:funlen,gocyclo,gocritic // The sealed Tea message union stays visible in one dispatcher.
func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = message.Width
		m.height = message.Height
		m.setLayout()
		m.rerenderTranscript(false)

		return m, nil
	case tea.BackgroundColorMsg:
		if m.options.NoColor {
			m.composer.SetStyles(textarea.Styles{})
		} else if message.IsDark() {
			m.theme = themeDark
			m.composer.SetStyles(textarea.DefaultDarkStyles())
		} else {
			m.theme = themeLight
			m.composer.SetStyles(textarea.DefaultLightStyles())
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
		m.lifecycle = lifecycleReady
		m.syncApprovalOverlay()
		m.setLayout()
		m.rerenderTranscript(true)

		return m, tea.Batch(m.composer.Focus(), m.continueIfPaused())
	case bridgeStartedMsg:
		m.starting = false
		if m.bridge != nil {
			message.bridge.once.Do(message.bridge.cancel)

			return m, nil
		}

		m.bridge = message.bridge
		m.waiting = true
		m.streamErr = nil
		if m.cancelStart {
			m.cancelStart = false

			return m, m.stopBridge(message.bridge)
		}

		return m, message.bridge.wait()
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
		m.renderTranscript(false)

		return m, nil
	case cancelResultMsg:
		if message.beforeStart {
			m.streamErr = errors.Join(m.streamErr, message.err)
			m.state = m.controller.Snapshot()
			m.syncApprovalOverlay()
			m.renderTranscript(false)

			return m, nil
		}
		if m.bridge != nil && message.bridge != nil && message.bridge != m.bridge {
			return m, nil
		}
		m.bridge = nil
		m.waiting = false
		m.streamErr = errors.Join(m.streamErr, message.err)
		m.state = m.controller.Snapshot()
		m.syncApprovalOverlay()
		m.renderTranscript(false)

		return m, nil
	case overlayDataMsg:
		if message.kind != m.overlay.kind {
			return m, nil
		}
		m.overlay.loading = false
		m.overlay.err = message.err
		m.overlay.sessions = message.sessions
		m.overlay.tree = message.tree.Clone()
		m.overlay.preview = message.preview
		m.overlay.cursor = 0

		return m, nil
	case controlResultMsg:
		m.overlay.loading = false
		m.state = m.controller.Snapshot()
		if message.err != nil {
			m.overlay.err = message.err
			m.renderTranscript(false)

			return m, nil
		}
		m.overlay = overlayState{}
		m.streamErr = nil
		m.syncApprovalOverlay()
		m.renderTranscript(true)

		return m, m.continueIfPaused()
	case exitResetMsg:
		m.exitArmed = false

		return m, nil
	case tea.MouseWheelMsg:
		if m.lifecycle != lifecycleReady {
			return m, nil
		}

		m.viewport, _ = m.viewport.Update(message)
		m.updateScrollState()

		return m, nil
	case tea.MouseMsg:
		return m, nil
	case tea.KeyPressMsg:
		return m.updateKey(message)
	case renderTickMsg:
		m.renderWait = false
		m.renderTranscript(false)

		return m, nil
	default:
		if m.lifecycle == lifecycleReady {
			var command tea.Cmd
			m.composer, command = m.composer.Update(message)
			m.setLayout()

			return m, command
		}

		return m, nil
	}
}

// View renders the full-screen lifecycle shell or ready chat layout.
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
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
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
		case actionScrollUp:
			m.viewport.PageUp()
			m.updateScrollState()
		case actionScrollDown:
			m.viewport.PageDown()
			m.updateScrollState()
		case actionBottom:
			m.viewport.GotoBottom()
			m.unseen = 0
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
			m.toggleLatestTool()
		case actionCommand, actionHelp:
			if action == actionCommand {
				return m, m.openOverlay(overlayCommand)
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
	separator := strings.Repeat("─", max(1, m.width))
	if m.options.NoColor {
		separator = strings.Repeat("-", max(1, m.width))
	} else {
		separator = lipgloss.NewStyle().Foreground(lipgloss.Color("#4B5563")).Render(separator)
	}

	status := m.statusLine()
	help := actionHints(defaultActions, m.actionContext())
	if !m.options.NoColor {
		status = lipgloss.NewStyle().Foreground(lipgloss.Color("#8B949E")).Render(status)
		help = lipgloss.NewStyle().Foreground(lipgloss.Color("#6E7681")).Render(help)
	}

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		m.viewport.View(),
		separator,
		m.composer.View(),
		ansi.Truncate(status, max(1, m.width), "…"),
		ansi.Truncate(help, max(1, m.width), "…"),
	)
	view := tea.NewView(content)
	view.SetContent(m.renderOverlay(content))
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.WindowTitle = appTitle
	view.Cursor = m.composer.Cursor()
	if m.overlay.kind != overlayNone {
		view.Cursor = nil
	}
	if view.Cursor != nil {
		view.Cursor.Y += m.viewport.Height() + 1
	}

	return view
}

func (m *Model) statusLine() string {
	workspaceName := filepath.Base(m.options.Workspace)
	if workspaceName == "." || workspaceName == string(filepath.Separator) {
		workspaceName = m.options.Workspace
	}

	status := fmt.Sprintf(
		"%s  ·  %s  ·  %s/%s  ·  %s",
		workspaceName,
		m.state.SessionID,
		m.state.Provider,
		m.state.ModelID,
		m.state.Phase,
	)
	if m.unseen > 0 {
		status += fmt.Sprintf("  ·  %d new", m.unseen)
	}
	if m.queued > 0 {
		status += fmt.Sprintf("  ·  %d queued", m.queued)
	}
	if m.exitArmed {
		status += "  ·  press Ctrl+C again to quit"
	}

	return status
}

func (m *Model) setLayout() {
	width := max(1, m.width)
	height := max(1, m.height)
	m.composer.SetWidth(width)
	composerHeight := max(1, min(composerMaxLines, m.composer.Height()))
	m.composer.SetHeight(composerHeight)
	viewportHeight := max(1, height-composerHeight-3)
	m.viewport.SetWidth(width)
	m.viewport.SetHeight(viewportHeight)
}

func (m *Model) renderTranscript(forceBottom bool) {
	m.renderTranscriptContent(forceBottom, true)
}

func (m *Model) rerenderTranscript(forceBottom bool) {
	m.renderTranscriptContent(forceBottom, false)
}

func (m *Model) renderTranscriptContent(forceBottom, newContent bool) {
	wasAtBottom := m.viewport.AtBottom()
	content := renderTimeline(
		m.timelineBlocks(),
		m.markdown,
		m.viewport.Width(),
		m.theme,
		m.options.NoColor,
	)
	m.viewport.SetContent(content)
	if forceBottom || wasAtBottom {
		m.viewport.GotoBottom()
		m.unseen = 0
	} else if newContent {
		m.unseen++
	}
}

func (m *Model) timelineBlocks() []timelineBlock {
	blocks := projectTimeline(m.state)
	for index := range blocks {
		block := &blocks[index]
		if block.kind != blockTool || !m.expanded[block.id] {
			continue
		}
		for _, tool := range m.state.Tools {
			if tool.Call.ID != block.id {
				continue
			}

			detail := "Arguments: " + strings.TrimSpace(string(tool.Call.Arguments))
			if update := visibleToolMessage(tool.Update); update != "" {
				detail += "\nProgress: " + update
			}
			if result := visibleToolMessage(tool.Result); result != "" {
				detail += "\nResult: " + result
			}
			block.body = truncateText(detail, 8<<10)
			block.status += " · expanded"

			break
		}
	}
	if m.streamErr != nil {
		blocks = append(blocks, timelineBlock{
			kind: blockError, title: "Operation", body: safeError(m.streamErr),
		})
	}

	return blocks
}

func (m *Model) toggleLatestTool() {
	if len(m.state.Tools) == 0 {
		return
	}

	id := m.state.Tools[len(m.state.Tools)-1].Call.ID
	m.expanded[id] = !m.expanded[id]
	m.rerenderTranscript(false)
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

func (m *Model) updateScrollState() {
	if m.viewport.AtBottom() {
		m.unseen = 0
	}
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
	m.starting = true

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
		m.finishStream()

		return m, nil
	}

	if message.item.err != nil {
		m.streamErr = message.item.err
	} else {
		next, err := coding.Reduce(m.state, message.item.event)
		if err != nil {
			m.streamErr = err
			m.bridge.once.Do(m.bridge.cancel)
		} else {
			m.state = next
			m.syncApprovalOverlay()
		}
	}

	m.waiting = true
	wait := m.bridge.wait()
	if terminalRenderEvent(message.item.event) || message.item.err != nil {
		m.renderTranscript(false)

		return m, wait
	}

	return m, tea.Batch(wait, m.requestRender())
}

func (m *Model) finishStream() {
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
	m.renderTranscript(false)
}

func (m *Model) cancelStream() tea.Cmd {
	bridge := m.bridge
	beforeStart := bridge == nil && m.starting
	if beforeStart {
		m.cancelStart = true
	}

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

	return err
}

func terminalRenderEvent(event coding.Event) bool {
	switch event.Type {
	case coding.EventInteractionCompleted,
		coding.EventApprovalRequired,
		coding.EventApprovalUnknown,
		coding.EventWorkspaceChanged,
		coding.EventError:
		return true
	default:
		return false
	}
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
