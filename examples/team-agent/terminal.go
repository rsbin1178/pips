package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/rsbin/pips/agent/team"
)

const maxTerminalInputBytes = 32 << 10

type terminalApp struct {
	coordinator *teamCoordinator
	input       io.Reader
	diagnostics io.Writer
}

type conversationTurn struct {
	User      string `json:"user"`
	Assistant string `json:"assistant"`
}

type terminalLine struct {
	text string
	err  error
	eof  bool
}

type terminalState struct {
	group      team.Team
	turn       int
	transcript []conversationTurn
}

func runTerminal(
	ctx context.Context,
	config modelConfig,
	input io.Reader,
	output io.Writer,
	diagnostics io.Writer,
) error {
	coordinator, err := newTeamCoordinator(config, diagnostics, output)
	if err != nil {
		return err
	}

	app := terminalApp{
		coordinator: coordinator,
		input:       input,
		diagnostics: diagnostics,
	}

	return app.Run(ctx)
}

// Run 保持一个 Team 和三名成员的 Session，直到 EOF、/exit 或进程取消。
func (app *terminalApp) Run(ctx context.Context) error {
	group, err := app.coordinator.Start(ctx)
	if err != nil {
		return err
	}

	if err := app.writeDiagnostics(
		"Team Agent 终端已启动。输入问题后按回车；/status 查看状态；/help 查看命令；/exit 退出。\n",
	); err != nil {
		return err
	}

	lines := scanTerminalLines(ctx, app.input)
	state := terminalState{group: group, transcript: make([]conversationTurn, 0)}

	for {
		if err := app.writeDiagnostics("你> "); err != nil {
			return err
		}

		line, cancelled := nextTerminalLine(ctx, lines)
		if cancelled {
			_ = app.writeDiagnostics("\n终端已取消。\n")

			return nil
		}

		if line.err != nil {
			return fmt.Errorf("read terminal input: %w", line.err)
		}

		if line.eof {
			return app.finish(ctx, state.group.ID, state.transcript)
		}

		done, err := app.handleRequest(ctx, &state, strings.TrimSpace(line.text))
		if err != nil {
			return err
		}

		if done {
			return nil
		}
	}
}

func (app *terminalApp) handleRequest(
	ctx context.Context,
	state *terminalState,
	request string,
) (bool, error) {
	switch request {
	case "":
		return false, nil
	case "/exit", "/quit":
		return true, app.finish(ctx, state.group.ID, state.transcript)
	case "/help":
		err := app.writeDiagnostics("命令：/status 查看 Team；/exit 或 /quit 完成 Team 并退出。\n")

		return false, err
	case "/status":
		group, err := app.coordinator.teams.Get(ctx, state.group.ID)
		if err != nil {
			return false, err
		}

		state.group = group

		return false, app.printStatus(group, state.turn)
	default:
		return false, app.runTurn(ctx, state, request)
	}
}

func (app *terminalApp) runTurn(
	ctx context.Context,
	state *terminalState,
	request string,
) error {
	state.turn++
	if err := app.writeDiagnostics("[第 %d 轮：Team 正在执行]\n", state.turn); err != nil {
		return err
	}

	result, group, err := app.coordinator.RunTurn(ctx, state.group.ID, state.turn, request)
	if err != nil {
		return err
	}

	state.group = group
	state.transcript = append(
		state.transcript,
		conversationTurn{User: request, Assistant: result.Answer},
	)

	return app.writeDiagnostics("[第 %d 轮完成]\n", state.turn)
}

func (app *terminalApp) finish(
	ctx context.Context,
	teamID team.ID,
	transcript []conversationTurn,
) error {
	// Team Output 保持有界；完整多轮上下文由各成员 Harness Session 保存。
	var lastTurn *conversationTurn

	if len(transcript) > 0 {
		last := transcript[len(transcript)-1]
		lastTurn = &last
	}

	output, err := jsonValue(struct {
		TurnCount int               `json:"turn_count"`
		LastTurn  *conversationTurn `json:"last_turn,omitempty"`
	}{TurnCount: len(transcript), LastTurn: lastTurn})
	if err != nil {
		return err
	}

	group, err := app.coordinator.Complete(ctx, teamID, output, "terminal conversation closed")
	if err != nil {
		return err
	}

	if err := app.printStatus(group, len(transcript)); err != nil {
		return err
	}

	return app.writeDiagnostics("终端会话已结束。\n")
}

func (app *terminalApp) printStatus(group team.Team, turns int) error {
	completed := 0

	for _, task := range group.Tasks {
		if task.Status == team.TaskStatusCompleted {
			completed++
		}
	}

	return app.writeDiagnostics(
		"Team=%s status=%s revision=%d turns=%d tasks=%d/%d messages=%d\n",
		group.ID,
		group.Status,
		group.Revision,
		turns,
		completed,
		len(group.Tasks),
		len(group.Messages),
	)
}

func nextTerminalLine(ctx context.Context, lines <-chan terminalLine) (terminalLine, bool) {
	select {
	case <-ctx.Done():
		return terminalLine{}, true
	case line := <-lines:
		return line, false
	}
}

func (app *terminalApp) writeDiagnostics(format string, arguments ...any) error {
	if _, err := fmt.Fprintf(app.diagnostics, format, arguments...); err != nil {
		return fmt.Errorf("write terminal diagnostics: %w", err)
	}

	return nil
}

func scanTerminalLines(ctx context.Context, input io.Reader) <-chan terminalLine {
	lines := make(chan terminalLine, 1)

	go func() {
		defer close(lines)

		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 64<<10), maxTerminalInputBytes)

		for scanner.Scan() {
			select {
			case lines <- terminalLine{text: scanner.Text()}:
			case <-ctx.Done():
				return
			}
		}

		line := terminalLine{err: scanner.Err(), eof: scanner.Err() == nil}
		select {
		case lines <- line:
		case <-ctx.Done():
		}
	}()

	return lines
}
