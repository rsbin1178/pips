package cli

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/rsbin1178/pips/internal/coding/workspace"
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
	goal           bool
	goalBudget     int
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

			var goalRequest *coding.GoalRequest

			if cmd.Flags().Changed("goal-budget") && (!flags.goal || flags.goalBudget <= 0) {
				return fmt.Errorf("%w: --goal-budget requires --goal and a positive token count", ErrUsage)
			}

			if flags.goal {
				goalRequest = &coding.GoalRequest{Condition: prompt, MaxTokens: flags.goalBudget}
				if err := goalRequest.Validate(); err != nil {
					return fmt.Errorf("%w: invalid goal: %w", ErrUsage, err)
				}
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

			options, err := newRuntimeOpenOptions(dependencies, state, flags.session)
			if err != nil {
				return err
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

			return runExecRequest(cmd.Context(), runtime, presenter, prompt, goalRequest)
		},
	}

	command.Flags().BoolVar(&flags.goal, "goal", false, "work toward the prompt until independently verified")
	command.Flags().IntVar(&flags.goalBudget, "goal-budget", 0, "cumulative goal token budget (requires --goal)")
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
) error {
	return runExecRequest(ctx, runtime, presenter, prompt, nil)
}

func runExecRequest(
	ctx context.Context,
	runtime execRuntime,
	presenter execPresenter,
	prompt string,
	goal *coding.GoalRequest,
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

	finalState, baseline, returnErr = runExecOperation(ctx, runtime, presenter, prompt, goal)
	finalReady = returnErr == nil

	return returnErr
}

func runExecOperation(
	ctx context.Context,
	runtime execRuntime,
	presenter execPresenter,
	prompt string,
	goal *coding.GoalRequest,
) (coding.State, int, error) {
	state := runtime.Snapshot()
	if err := presenter.Opened(state); err != nil {
		return coding.State{}, 0, err
	}

	state, err := settleOpenedRuntime(ctx, runtime, presenter, state)
	if err != nil {
		return coding.State{}, 0, err
	}

	if err := pendingExecError(state); err != nil {
		return coding.State{}, 0, err
	}

	if state.Phase != coding.PhaseIdle || state.Interaction.Active {
		return coding.State{}, 0, errors.New("coding cli: runtime did not settle before prompt")
	}

	baseline := len(state.Transcript)

	var events iter.Seq2[coding.Event, error]

	if goal != nil {
		starter, ok := runtime.(interface {
			StartGoal(context.Context, coding.GoalRequest) iter.Seq2[coding.Event, error]
		})
		if !ok {
			return coding.State{}, 0, errors.New("coding cli: runtime does not support goals")
		}

		events = starter.StartGoal(ctx, *goal)
	} else {
		events = runtime.Prompt(ctx, ai.UserText(prompt))
	}

	if err := consumeEvents(events, presenter); err != nil {
		return coding.State{}, 0, err
	}

	finalState := runtime.Snapshot()
	if err := pendingExecError(finalState); err != nil {
		return coding.State{}, 0, err
	}

	if goal != nil && !finalState.Goal.Completed() {
		return coding.State{}, 0, fmt.Errorf("coding cli: goal did not complete (status %q); inspect or resume it interactively", finalState.Goal.Status)
	}

	if err := validateSuccessfulInteraction(finalState); err != nil {
		return coding.State{}, 0, err
	}

	return finalState, baseline, nil
}

func pendingExecError(state coding.State) error {
	if err := state.Approval.NonInteractiveError(); err != nil {
		return err
	}

	if err := state.Question.NonInteractiveError(); err != nil {
		return err
	}

	return state.PlanReview.NonInteractiveError()
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
			"coding cli: interaction ended without success (phase=%s outcome=%s stop=%s active=%t)",
			state.Phase,
			state.Interaction.Outcome,
			state.Interaction.Stop,
			state.Interaction.Active,
		)
	}

	return nil
}

func validateSessionFlag(value string) error {
	if value == "" {
		return nil
	}

	return validateSessionArgument(value)
}

func validateSessionArgument(value string) error {
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: session ID has surrounding whitespace", ErrUsage)
	}

	if err := session.ValidateID(value); err != nil {
		return fmt.Errorf("%w: session ID: %w", ErrUsage, err)
	}

	return nil
}
