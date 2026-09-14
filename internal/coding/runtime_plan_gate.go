//nolint:wsl_v5 // Plan gate, reminder, and transition helpers stay adjacent.
package coding

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/generation"
	"github.com/rsbin1178/pips/internal/coding/planmode"
	"github.com/rsbin1178/pips/internal/coding/planreview"
	"github.com/rsbin1178/pips/internal/coding/question"
	"github.com/rsbin1178/pips/internal/coding/tools"
	patchdoc "github.com/rsbin1178/pips/internal/coding/tools/patch"
)

// planEditGate rejects file edits outside the plan file while plan mode is
// armed. Plan-file edits are admitted and never reach permission prompts.
func (r *Runtime) planEditGate() func(context.Context, agent.ToolCallInfo) agent.ToolDecision {
	return func(_ context.Context, info agent.ToolCallInfo) agent.ToolDecision {
		if info.Name != tools.ApplyPatchName || !r.PlanState().GateArmed() {
			return agent.ToolDecision{}
		}

		store := r.planStoreSnapshot()
		if store == nil {
			return agent.DenyTool("plan file is unavailable")
		}

		patchText, err := decodePlanPatchArguments(info.Args)
		if err != nil {
			// Let the patch tool report its own argument error.
			return agent.ToolDecision{}
		}

		limits := tools.DefaultLimits()
		document, err := patchdoc.Parse(patchText, patchdoc.Limits{
			Bytes: limits.PatchBytes, Files: limits.PatchFiles, PlanPath: store.Path(),
		})
		if err != nil {
			// Let the patch tool report its own grammar error.
			return agent.ToolDecision{}
		}

		for _, change := range document.Changes {
			if change.Path != store.Path() {
				return agent.DenyTool(planmode.EditRejection(store.Path()))
			}
		}

		return agent.ToolDecision{}
	}
}

// planReminderInjector prepends the plan-mode reminder to every model request
// while plan mode is armed. Unlike a prepare-turn update, a request hook also
// reaches the first request of a run.
func (r *Runtime) planReminderInjector(current *interaction) func(*ai.Request) {
	return func(request *ai.Request) {
		reminder := r.planReminderText(current)
		if reminder == "" {
			return
		}

		prefix := 0
		for prefix < len(request.Messages) {
			if _, ok := request.Messages[prefix].(ai.SystemMessage); !ok {
				break
			}

			prefix++
		}

		suffix := reminder
		if prefix > 0 {
			suffix = "\n" + suffix
		}

		request.Messages = slices.Insert(request.Messages, prefix, ai.Message(ai.SystemText(suffix)))
	}
}

// planReminderText renders the reminder for the next request, consuming the
// one-shot transition notices.
func (r *Runtime) planReminderText(current *interaction) string {
	if !r.PlanState().GateArmed() {
		if r.takeExitedReminder() {
			return planmode.ExitedReminder + "\n\n" + planmode.AgentModeReminder
		}

		return ""
	}

	store := r.planStoreSnapshot()
	if store == nil {
		return ""
	}

	names := planmode.ReminderNames{
		Edit: tools.ApplyPatchName, Ask: question.ToolName, Exit: planmode.ExitToolName,
	}

	if current != nil && current.source != InteractionSourceUser {
		// Runtime-generated continuations already carry the plan context;
		// they only need the short boundary reminder.
		return planmode.StillActiveReminder
	}

	hasContent := false
	if document, err := store.Read(context.Background()); err == nil {
		hasContent = strings.TrimSpace(document.Content) != ""
	}

	reminder := planmode.PlanReminder(store.Path(), hasContent, names)

	if r.takeReturningReminder() {
		reminder = planmode.ReturningReminder(store.Path(), names) + "\n\n" + reminder
	}

	return reminder + "\n\n" + planmode.IterationGuidance(question.ToolName)
}

// composePlanReminder chains the plan reminder after the application request
// policy so both hooks observe every request.
func composePlanReminder(
	policy generation.Policy,
	inject func(*ai.Request),
) func(*ai.Request) {
	return func(request *ai.Request) {
		if policy != nil {
			policy(request)
		}
		if inject != nil {
			inject(request)
		}
	}
}

// applyPlanOutcome applies the plan-mode state transition of one resolved
// decision. Revision decisions keep the current state.
func (r *Runtime) applyPlanOutcome(
	ctx context.Context,
	current *interaction,
	emitter *eventEmitter,
	outcome planreview.Outcome,
) error {
	interactionID := ""
	if current != nil {
		interactionID = current.id
	}

	switch outcome.Kind {
	case planreview.KindEnter:
		if outcome.Decision != planreview.DecisionApprove {
			return nil
		}

		return r.transitionPlanMode(ctx, planmode.StateActive, interactionID, emitter)
	case planreview.KindExit:
		if outcome.Decision == planreview.DecisionRevise {
			return nil
		}

		return r.transitionPlanMode(ctx, planmode.StateInactive, interactionID, emitter)
	default:
		return nil
	}
}

// settlePlanMode completes a deferred plan-mode exit once its turn finishes.
func (r *Runtime) settlePlanMode(
	ctx context.Context,
	current *interaction,
	emitter *eventEmitter,
) error {
	state := r.PlanState()
	if state != planmode.StateExitPending {
		return nil
	}

	next, err := state.TurnComplete()
	if err != nil {
		return err
	}

	interactionID := ""
	if current != nil {
		interactionID = current.id
	}

	return r.transitionPlanMode(ctx, next, interactionID, emitter)
}

func (r *Runtime) planStoreSnapshot() *planmode.Store {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.planStore
}

// planFileBoundary exposes the plan-mode file to the workspace patch tool.
func (r *Runtime) planFileBoundary() *tools.PlanFile {
	store := r.planStoreSnapshot()
	if store == nil {
		return nil
	}

	return &tools.PlanFile{
		Path:     store.Path,
		Admitted: func() bool { return r.PlanState().GateArmed() },
		Read: func(ctx context.Context) (string, error) {
			document, err := store.Read(ctx)

			return document.Content, err
		},
		Write: func(ctx context.Context, content string) error {
			_, err := store.Replace(ctx, content)

			return err
		},
	}
}

func (r *Runtime) takeExitedReminder() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	taken := r.planExited
	r.planExited = false

	return taken
}

func (r *Runtime) takeReturningReminder() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	taken := r.planReturning
	r.planReturning = false

	return taken
}

func decodePlanPatchArguments(data []byte) (string, error) {
	var args struct {
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal(data, &args); err != nil {
		return "", err
	}

	return args.Patch, nil
}
