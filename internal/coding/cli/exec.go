package cli

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/credential"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/workspace"
	"github.com/spf13/cobra"
)

const runtimeCloseTimeout = 10 * time.Second

type execRuntime interface {
	Prompt(context.Context, ...ai.Message) iter.Seq2[coding.Event, error]
	Continue(context.Context) iter.Seq2[coding.Event, error]
	Snapshot() coding.State
	Close(context.Context) error
}

type runtimeOpener func(context.Context, coding.OpenOptions) (execRuntime, error)

type execFlags struct {
	output         string
	session        string
	trustWorkspace bool
}

func newExecCommand(
	dependencies Dependencies,
	root *rootFlags,
	opener runtimeOpener,
) *cobra.Command {
	flags := &execFlags{}
	command := &cobra.Command{
		Use:   "exec [prompt]",
		Short: "Run one non-interactive coding request",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := parseOutputMode(flags.output)
			if err != nil {
				return err
			}

			if err := validateSessionFlag(flags.session); err != nil {
				return err
			}

			prompt, err := readPrompt(cmd.Context(), cmd.InOrStdin(), args)
			if err != nil {
				return err
			}

			resolved, err := resolveWorkspaceState(cmd.Context(), dependencies, root)
			if err != nil {
				return err
			}

			if flags.trustWorkspace && !resolved.isTrusted {
				store := workspace.NewStore(dependencies.Paths.WorkspacesFile())
				if err := store.Trust(resolved.workspace.Identity()); err != nil {
					return err
				}

				resolved.isTrusted = true
				if _, err := fmt.Fprintf(
					cmd.ErrOrStderr(),
					"workspace trusted: %q; project resources enabled\n",
					resolved.workspace.Root(),
				); err != nil {
					return err
				}
			}

			state, err := loadResolvedCommandState(cmd, dependencies, root, resolved)
			if err != nil {
				return err
			}

			if err := state.config.Config.ValidateRuntime(); err != nil {
				return err
			}

			credentials, err := credential.NewEnvironmentStore(dependencies.LookupEnv)
			if err != nil {
				return err
			}

			options := coding.OpenOptions{
				Workspace:   state.workspace.workspace,
				Trusted:     state.workspace.isTrusted,
				Config:      state.config.Config,
				Paths:       dependencies.Paths,
				Session:     coding.SessionTarget{ID: flags.session},
				Credentials: credentials,
				Execution: coding.ExecutionOptions{
					Environment: dependencies.LookupEnv,
				},
			}

			runtime, err := opener(cmd.Context(), options)
			if err != nil {
				return err
			}

			presenter := newExecPresenter(
				mode,
				cmd.OutOrStdout(),
				cmd.ErrOrStderr(),
				flags.session != "",
			)

			return runExecRuntime(cmd.Context(), runtime, presenter, prompt)
		},
	}

	command.Flags().StringVar(&flags.output, "output", string(outputPlain), "output mode: plain or jsonl")
	command.Flags().StringVar(&flags.session, "session", "", "resume a session by ID")
	command.Flags().BoolVar(
		&flags.trustWorkspace,
		"trust-workspace",
		false,
		"trust this workspace and enable project resources",
	)

	return command
}

func runExecRuntime(
	ctx context.Context,
	runtime execRuntime,
	presenter execPresenter,
	prompt string,
) (returnErr error) {
	if runtime == nil || presenter == nil {
		return errors.New("coding cli: incomplete exec runtime")
	}

	var (
		finalState coding.State
		baseline   int
	)

	finalReady := false

	defer func() {
		returnErr = errors.Join(returnErr, closeExecRuntime(ctx, runtime))
		if returnErr == nil && finalReady {
			returnErr = presenter.Final(finalState, baseline)
		}
	}()

	finalState, baseline, returnErr = runExecOperation(ctx, runtime, presenter, prompt)
	finalReady = returnErr == nil

	return returnErr
}

func runExecOperation(
	ctx context.Context,
	runtime execRuntime,
	presenter execPresenter,
	prompt string,
) (coding.State, int, error) {
	state := runtime.Snapshot()
	if err := presenter.Opened(state); err != nil {
		return coding.State{}, 0, err
	}

	state, err := settleOpenedRuntime(ctx, runtime, presenter, state)
	if err != nil {
		return coding.State{}, 0, err
	}

	if err := state.Approval.NonInteractiveError(); err != nil {
		return coding.State{}, 0, err
	}

	if state.Phase != coding.PhaseIdle || state.Interaction.Active {
		return coding.State{}, 0, errors.New("coding cli: runtime did not settle before prompt")
	}

	baseline := len(state.Transcript)

	if err := consumeEvents(runtime.Prompt(ctx, ai.UserText(prompt)), presenter); err != nil {
		return coding.State{}, 0, err
	}

	finalState := runtime.Snapshot()
	if err := finalState.Approval.NonInteractiveError(); err != nil {
		return coding.State{}, 0, err
	}

	if err := validateSuccessfulInteraction(finalState); err != nil {
		return coding.State{}, 0, err
	}

	return finalState, baseline, nil
}

func settleOpenedRuntime(
	ctx context.Context,
	runtime execRuntime,
	presenter execPresenter,
	state coding.State,
) (coding.State, error) {
	switch state.Phase {
	case coding.PhasePaused:
		if err := consumeEvents(runtime.Continue(ctx), presenter); err != nil {
			return coding.State{}, err
		}

		return runtime.Snapshot(), nil
	case coding.PhaseIdle:
		return state, nil
	default:
		return coding.State{}, fmt.Errorf(
			"coding cli: runtime opened in unsupported phase %q",
			state.Phase,
		)
	}
}

func closeExecRuntime(ctx context.Context, runtime execRuntime) error {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeCloseTimeout)
	defer cancel()

	return runtime.Close(closeCtx)
}

func consumeEvents(events iter.Seq2[coding.Event, error], presenter execPresenter) error {
	for event, eventErr := range events {
		if eventErr != nil {
			return eventErr
		}

		if err := presenter.Event(event); err != nil {
			return err
		}
	}

	return nil
}

func validateSuccessfulInteraction(state coding.State) error {
	if state.Interaction.Active || state.Phase != coding.PhaseIdle ||
		state.Interaction.Outcome != coding.InteractionSucceeded {
		return fmt.Errorf(
			"coding cli: interaction ended without success (phase=%s outcome=%s active=%t)",
			state.Phase,
			state.Interaction.Outcome,
			state.Interaction.Active,
		)
	}

	return nil
}

func validateSessionFlag(value string) error {
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: --session has surrounding whitespace", ErrUsage)
	}

	if value == "" {
		return nil
	}

	if err := session.ValidateID(value); err != nil {
		return fmt.Errorf("%w: --session: %w", ErrUsage, err)
	}

	return nil
}
