package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/approval"
	"github.com/rsbin1178/pips/internal/coding/config"
	"github.com/rsbin1178/pips/internal/coding/credential"
	"github.com/rsbin1178/pips/internal/coding/execution"
	"github.com/rsbin1178/pips/internal/coding/generation"
	"github.com/rsbin1178/pips/internal/coding/model"
	"github.com/rsbin1178/pips/internal/coding/modelcatalog"
	"github.com/rsbin1178/pips/internal/coding/paths"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		args  []string
		stdin string
		want  string
		err   error
	}{
		{name: "argument", args: []string{"keep whitespace\n"}, want: "keep whitespace\n"},
		{name: "implicit stdin", stdin: "from pipe\n", want: "from pipe\n"},
		{name: "explicit stdin", args: []string{"-"}, stdin: "from pipe", want: "from pipe"},
		{name: "empty nonterminal stdin", err: ErrUsage},
		{name: "empty explicit stdin", args: []string{"-"}, err: ErrUsage},
		{name: "too many arguments", args: []string{"one", "two"}, err: ErrUsage},
		{name: "input conflict", args: []string{"argument"}, stdin: "pipe", err: ErrUsage},
		{name: "invalid prompt", args: []string{"bad\x00prompt"}, err: ErrUsage},
		{name: "exact limit", args: []string{strings.Repeat("a", coding.MaxPromptTextBytes)}, want: strings.Repeat("a", coding.MaxPromptTextBytes)},
		{name: "over limit", args: []string{"-"}, stdin: strings.Repeat("a", coding.MaxPromptTextBytes+1), err: ErrUsage},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := readPrompt(t.Context(), strings.NewReader(test.stdin), test.args)
			if test.err != nil {
				require.ErrorIs(t, err, test.err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestReadPromptCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := readPrompt(ctx, bytes.NewBufferString("prompt"), nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestReadPromptPipeUnblocksOnCancellation(t *testing.T) {
	t.Parallel()

	readEnd, writeEnd, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = readEnd.Close()
		_ = writeEnd.Close()
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		_, err := readPrompt(ctx, readEnd, []string{"-"})
		done <- err
	}()

	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("stdin read did not stop after cancellation")
	}
}

func TestReadPromptCharacterDeviceRequiresExplicitInput(t *testing.T) {
	t.Parallel()

	input, err := os.Open(os.DevNull)
	require.NoError(t, err)
	t.Cleanup(func() { _ = input.Close() })

	_, err = readPrompt(t.Context(), input, nil)
	require.ErrorIs(t, err, ErrUsage)

	value, err := readPrompt(t.Context(), input, []string{"argument"})
	require.NoError(t, err)
	assert.Equal(t, "argument", value)
}

func FuzzReadPromptArgument(f *testing.F) {
	f.Add("prompt")
	f.Add(" \n\t ")
	f.Add("bad\x00prompt")
	f.Add(string([]byte{0xff}))

	f.Fuzz(func(t *testing.T, value string) {
		prompt, err := readPrompt(t.Context(), strings.NewReader(""), []string{value})
		if err != nil {
			require.ErrorIs(t, err, ErrUsage)

			return
		}

		assert.Equal(t, value, prompt)
	})
}

func TestParseOutputMode(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"plain", "jsonl"} {
		mode, err := parseOutputMode(value)
		require.NoError(t, err)
		assert.Equal(t, outputMode(value), mode)
	}

	_, err := parseOutputMode("json")
	require.ErrorIs(t, err, ErrUsage)
}

func TestExitCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "success", want: ExitSuccess},
		{name: "failure", err: errors.New("boom"), want: ExitFailure},
		{name: "provider", err: ai.ErrAuth, want: ExitFailure},
		{name: "session locked", err: session.ErrLocked, want: ExitFailure},
		{name: "usage", err: fmt.Errorf("outer: %w", ErrUsage), want: ExitUsage},
		{name: "prompt usage", err: coding.ErrInvalidPrompt, want: ExitUsage},
		{name: "config usage", err: config.ErrInvalid, want: ExitUsage},
		{name: "config migration", err: config.ErrMigration, want: ExitUsage},
		{name: "credential usage", err: credential.ErrNotFound, want: ExitUsage},
		{name: "generation usage", err: generation.ErrInvalid, want: ExitUsage},
		{name: "model usage", err: model.ErrInvalid, want: ExitUsage},
		{name: "catalog usage", err: modelcatalog.ErrInvalid, want: ExitUsage},
		{name: "paths usage", err: paths.ErrInvalid, want: ExitUsage},
		{name: "session usage", err: session.ErrInvalid, want: ExitUsage},
		{name: "workspace usage", err: workspace.ErrInvalid, want: ExitUsage},
		{name: "approval", err: approval.ErrApprovalRequired, want: ExitApproval},
		{name: "structured input", err: coding.ErrInputRequired, want: ExitInput},
		{name: "Plan review", err: coding.ErrPlanReviewRequired, want: ExitInput},
		{name: "Team interaction", err: coding.ErrTeamInteractionRequired, want: ExitInput},
		{name: "unknown approval", err: approval.ErrOutcomeUnknown, want: ExitApproval},
		{name: "denied approval", err: approval.ErrDenied, want: ExitApproval},
		{name: "security", err: execution.ErrSandboxUnavailable, want: ExitSecurity},
		{name: "policy security", err: execution.ErrInvalidPolicy, want: ExitSecurity},
		{name: "workspace security", err: workspace.ErrChanged, want: ExitSecurity},
		{name: "session security", err: session.ErrWorkspaceMismatch, want: ExitSecurity},
		{name: "canceled", err: context.Canceled, want: ExitInterrupted},
		{name: "deadline", err: context.DeadlineExceeded, want: ExitFailure},
		{name: "joined specificity", err: errors.Join(ErrUsage, approval.ErrOutcomeUnknown), want: ExitApproval},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.want, ExitCode(test.err))
		})
	}
}
