package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
)

type execPresenter interface {
	Opened(coding.State) error
	Event(coding.Event) error
	Final(coding.State, int) error
}

func newExecPresenter(mode outputMode, stdout, stderr io.Writer, resumed bool) execPresenter {
	switch mode {
	case outputJSONL:
		return &jsonlPresenter{encoder: json.NewEncoder(stdout)}
	default:
		return &plainPresenter{stdout: stdout, stderr: stderr, resumed: resumed}
	}
}

type plainPresenter struct {
	stdout  io.Writer
	stderr  io.Writer
	resumed bool
}

func (p *plainPresenter) Opened(state coding.State) error {
	status := "opened"
	if p.resumed {
		status = "resumed"
	}

	return writeFormatted(p.stderr, "session %s %s\n", state.SessionID, status)
}

func (p *plainPresenter) Event(event coding.Event) error {
	projected, err := coding.Project(event, coding.DisclosureSafe)
	if err != nil {
		return err
	}

	safe := projected.Event()
	switch payload := safe.Payload.(type) {
	case coding.ToolStarted:
		err = writeFormatted(p.stderr, "tool %s started\n", payload.Call.Name)
	case coding.ToolCompleted:
		err = writeFormatted(p.stderr, "tool %s completed\n", payload.Call.Name)
	case coding.WorkspaceChanged:
		suffix := ""
		if payload.Truncated {
			suffix = " (truncated)"
		}

		err = writeFormatted(
			p.stderr,
			"workspace changed: %d path(s)%s\n",
			len(payload.Entries),
			suffix,
		)
	case coding.IntegrationDiagnostic:
		err = writeFormatted(p.stderr, "integration %s/%s\n", payload.Component, payload.Code)
	case coding.ApprovalRequired:
		err = writeFormatted(p.stderr, "approval required: %s\n", payload.Tool)
	case coding.ApprovalUnknown:
		err = writeFormatted(p.stderr, "approval outcome unknown: %s\n", payload.Tool)
	case coding.RuntimeError:
		err = writeFormatted(p.stderr, "runtime error: %s\n", payload.Code)
	}

	return err
}

func (p *plainPresenter) Final(state coding.State, baseline int) error {
	if baseline < 0 || baseline > len(state.Transcript) {
		return errors.New("coding cli: invalid transcript baseline")
	}

	for index := len(state.Transcript) - 1; index >= baseline; index-- {
		message := state.Transcript[index]
		if message.Role != ai.RoleAssistant {
			continue
		}

		var builder strings.Builder

		for _, part := range message.Parts {
			if text, ok := part.(ai.TextPart); ok {
				builder.WriteString(text.Text)
			}
		}

		value := builder.String()
		if value == "" {
			return nil
		}

		if !strings.HasSuffix(value, "\n") {
			value += "\n"
		}

		return writeString(p.stdout, value)
	}

	return nil
}

type jsonlPresenter struct {
	encoder *json.Encoder
}

func (*jsonlPresenter) Opened(coding.State) error { return nil }

func (p *jsonlPresenter) Event(event coding.Event) error {
	projected, err := coding.Project(event, coding.DisclosureSafe)
	if err != nil {
		return err
	}

	return p.encoder.Encode(projected)
}

func (*jsonlPresenter) Final(coding.State, int) error { return nil }

func writeFormatted(writer io.Writer, format string, values ...any) error {
	return writeString(writer, fmt.Sprintf(format, values...))
}

func writeString(writer io.Writer, value string) error {
	written, err := io.WriteString(writer, value)
	if written != len(value) && err == nil {
		return io.ErrShortWrite
	}

	return err
}
