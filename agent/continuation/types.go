package continuation

import (
	"context"
	"slices"
	"time"

	"github.com/rsbin/pips/ai"
)

// ID identifies one continuation execution independently of its target.
type ID string

// AttemptID identifies one Worker invocation and its following decision.
type AttemptID string

// Revision is an optimistic concurrency version.
type Revision uint64

// HandlerRef durably identifies a compatible handler implementation.
type HandlerRef struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

// Target identifies host-owned work without prescribing its domain.
type Target struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Status is the durable lifecycle state of an Execution.
type Status string

// Execution statuses.
const (
	StatusReady           Status = "ready"
	StatusRunning         Status = "running"
	StatusWaiting         Status = "waiting"
	StatusPauseRequested  Status = "pause_requested"
	StatusPaused          Status = "paused"
	StatusBlocked         Status = "blocked"
	StatusInterrupted     Status = "interrupted"
	StatusCancelRequested Status = "cancel_requested"
	StatusCompleted       Status = "completed"
	StatusFailed          Status = "failed"
	StatusCancelled       Status = "cancelled"
	StatusLimited         Status = "limited"
)

// Phase identifies the next or active stage.
type Phase string

// Execution phases.
const (
	PhaseWork     Phase = "work"
	PhaseDecision Phase = "decision"
)

// Action is a Controller lifecycle decision.
type Action string

// Controller actions.
const (
	ActionContinue Action = "continue"
	ActionWait     Action = "wait"
	ActionBlock    Action = "block"
	ActionComplete Action = "complete"
	ActionFail     Action = "fail"
	ActionCancel   Action = "cancel"
)

// Progress classifies whether a completed stage made observable progress.
type Progress string

// Progress values.
const (
	ProgressUnknown   Progress = "unknown"
	ProgressChanged   Progress = "changed"
	ProgressUnchanged Progress = "unchanged"
)

// Cause identifies why a durable transition occurred.
type Cause string

// Transition causes.
const (
	CauseCreate           Cause = "create"
	CauseStageStart       Cause = "stage_start"
	CauseWorkComplete     Cause = "work_complete"
	CauseStageInterrupted Cause = "stage_interrupted"
	CauseControllerAction Cause = "controller_action"
	CausePauseRequested   Cause = "pause_requested"
	CausePause            Cause = "pause"
	CauseResume           Cause = "resume"
	CauseRetryWork        Cause = "retry_work"
	CauseRetryDecision    Cause = "retry_decision"
	CauseSignal           Cause = "signal"
	CauseTimeWake         Cause = "time_wake"
	CauseBlockResolved    Cause = "block_resolved"
	CauseCancelRequested  Cause = "cancel_requested"
	CauseCancel           Cause = "cancel"
	CauseFail             Cause = "fail"
	CauseLimit            Cause = "limit"
	CauseRecovery         Cause = "recovery"
)

// YieldReason says why Drive returned control to its host.
type YieldReason string

// Drive yield reasons.
const (
	YieldTerminal    YieldReason = "terminal"
	YieldWaiting     YieldReason = "waiting"
	YieldPaused      YieldReason = "paused"
	YieldBlocked     YieldReason = "blocked"
	YieldInterrupted YieldReason = "interrupted"
	YieldGate        YieldReason = "gate"
	YieldNoProgress  YieldReason = "no_progress"
	YieldQuantum     YieldReason = "quantum"
	YieldContext     YieldReason = "context"
	YieldError       YieldReason = "error"
)

// ActivationSource identifies the explicit event that made work runnable.
type ActivationSource string

// Activation sources.
const (
	ActivationInitial ActivationSource = "initial"
	ActivationSignal  ActivationSource = "signal"
	ActivationTime    ActivationSource = "time"
	ActivationBlock   ActivationSource = "block"
)

// Activation is durable evidence for why a new Work stage became runnable.
type Activation struct {
	Source   ActivationSource `json:"source"`
	At       time.Time        `json:"at"`
	SignalID string           `json:"signal_id,omitempty"`
	Payload  ai.JSON          `json:"payload,omitempty"`
}

// SignalSpec selects one exact, host-delivered signal key.
type SignalSpec struct {
	Key string `json:"key"`
}

// WaitCondition is satisfied by NotBefore or Signal when both are present.
type WaitCondition struct {
	NotBefore *time.Time  `json:"not_before,omitempty"`
	Signal    *SignalSpec `json:"signal,omitempty"`
}

// Signal is one idempotent external wakeup delivery.
type Signal struct {
	ID      string  `json:"id"`
	Key     string  `json:"key"`
	Payload ai.JSON `json:"payload,omitempty"`
}

// Block describes controller-required external input.
type Block struct {
	Kind string  `json:"kind"`
	Data ai.JSON `json:"data,omitempty"`
}

// Suspension captures the state restored by Resume.
type Suspension struct {
	Status        Status         `json:"status"`
	Phase         Phase          `json:"phase"`
	Wait          *WaitCondition `json:"wait,omitempty"`
	Block         *Block         `json:"block,omitempty"`
	RetryRequired bool           `json:"retry_required,omitempty"`
	Reason        string         `json:"reason,omitempty"`
}

// StageFailure is the bounded durable projection of a stage error.
type StageFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Limits are cumulative hard execution limits. Zero means unset except that
// MaxAttempts zero resolves to DefaultMaxAttempts; -1 means unlimited.
type Limits struct {
	MaxAttempts       int           `json:"max_attempts,omitempty"`
	MaxTurns          int           `json:"max_turns,omitempty"`
	MaxTokens         int           `json:"max_tokens,omitempty"`
	MaxActiveDuration time.Duration `json:"max_active_duration,omitempty"`
	Deadline          time.Time     `json:"deadline,omitzero"`
}

// Accounting is observed cumulative usage.
type Accounting struct {
	Attempts       int           `json:"attempts"`
	Turns          int           `json:"turns"`
	Usage          ai.Usage      `json:"usage"`
	ActiveDuration time.Duration `json:"active_duration"`
}

// Tokens returns cumulative input plus output tokens.
func (a Accounting) Tokens() int { return a.Usage.InputTokens + a.Usage.OutputTokens }

// Remaining reports cooperative budgets before a Worker invocation.
type Remaining struct {
	Attempts       int           `json:"attempts,omitempty"`
	Turns          int           `json:"turns,omitempty"`
	Tokens         int           `json:"tokens,omitempty"`
	ActiveDuration time.Duration `json:"active_duration,omitempty"`
	Deadline       time.Time     `json:"deadline,omitzero"`
}

// WorkRequest is the immutable input for one bounded Worker invocation.
type WorkRequest struct {
	ExecutionID ID          `json:"execution_id"`
	AttemptID   AttemptID   `json:"attempt_id"`
	Attempt     int         `json:"attempt"`
	Target      Target      `json:"target"`
	Input       ai.JSON     `json:"input,omitempty"`
	Activation  *Activation `json:"activation,omitempty"`
	Limits      Limits      `json:"limits"`
	Accounting  Accounting  `json:"accounting"`
	Remaining   Remaining   `json:"remaining"`
}

// WorkResult is durable Controller evidence and observed Worker accounting.
type WorkResult struct {
	Value    ai.JSON  `json:"value,omitempty"`
	Turns    int      `json:"turns"`
	Usage    ai.Usage `json:"usage"`
	Progress Progress `json:"progress"`
}

// DecisionRequest is the immutable input for post-Work evaluation.
type DecisionRequest struct {
	ExecutionID     ID          `json:"execution_id"`
	AttemptID       AttemptID   `json:"attempt_id"`
	Attempt         int         `json:"attempt"`
	Target          Target      `json:"target"`
	Work            WorkResult  `json:"work"`
	Activation      *Activation `json:"activation,omitempty"`
	ControllerState ai.JSON     `json:"controller_state,omitempty"`
	Limits          Limits      `json:"limits"`
	Accounting      Accounting  `json:"accounting"`
}

// Decision controls the durable state after a completed Work stage.
type Decision struct {
	Action    Action         `json:"action"`
	Reason    string         `json:"reason,omitempty"`
	State     ai.JSON        `json:"state,omitempty"`
	NextInput ai.JSON        `json:"next_input,omitempty"`
	Wait      *WaitCondition `json:"wait,omitempty"`
	Block     *Block         `json:"block,omitempty"`
	Output    ai.JSON        `json:"output,omitempty"`
	Progress  Progress       `json:"progress,omitempty"`
}

// Attempt records the durable Work/Decision boundary.
type Attempt struct {
	ID                AttemptID     `json:"id"`
	Number            int           `json:"number"`
	Phase             Phase         `json:"phase"`
	Activation        *Activation   `json:"activation,omitempty"`
	WorkStartedAt     time.Time     `json:"work_started_at"`
	WorkCompletedAt   time.Time     `json:"work_completed_at,omitzero"`
	DecisionStartedAt time.Time     `json:"decision_started_at,omitzero"`
	DecisionEndedAt   time.Time     `json:"decision_ended_at,omitzero"`
	Work              *WorkResult   `json:"work,omitempty"`
	Decision          *Decision     `json:"decision,omitempty"`
	Failure           *StageFailure `json:"failure,omitempty"`
	Interrupted       bool          `json:"interrupted,omitempty"`
}

// Execution is the latest full continuation snapshot.
type Execution struct {
	ID              ID             `json:"id"`
	Revision        Revision       `json:"revision"`
	Status          Status         `json:"status"`
	Phase           Phase          `json:"phase"`
	Target          Target         `json:"target"`
	Worker          HandlerRef     `json:"worker"`
	Controller      HandlerRef     `json:"controller"`
	ControllerState ai.JSON        `json:"controller_state,omitempty"`
	NextInput       ai.JSON        `json:"next_input,omitempty"`
	Activation      *Activation    `json:"activation,omitempty"`
	Wait            *WaitCondition `json:"wait,omitempty"`
	Block           *Block         `json:"block,omitempty"`
	Suspension      *Suspension    `json:"suspension,omitempty"`
	CurrentAttempt  *Attempt       `json:"current_attempt,omitempty"`
	LastAttempt     *Attempt       `json:"last_attempt,omitempty"`
	Limits          Limits         `json:"limits"`
	Accounting      Accounting     `json:"accounting"`
	Reason          string         `json:"reason,omitempty"`
	Output          ai.JSON        `json:"output,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// Worker performs one bounded unit of host-defined work.
type Worker interface {
	Run(context.Context, WorkRequest) (WorkResult, error)
}

// Controller evaluates a durable Work result without rerunning it.
type Controller interface {
	Decide(context.Context, DecisionRequest) (Decision, error)
}

// Handlers binds runtime implementations to persisted references.
type Handlers struct {
	WorkerRef     HandlerRef
	Worker        Worker
	ControllerRef HandlerRef
	Controller    Controller
}

// CreateRequest configures a new Execution.
type CreateRequest struct {
	ID              ID
	Target          Target
	Worker          HandlerRef
	Controller      HandlerRef
	ControllerState ai.JSON
	Input           ai.JSON
	Limits          Limits
}

// Transition is operational audit data for one revision.
type Transition struct {
	Revision  Revision  `json:"revision"`
	At        time.Time `json:"at"`
	From      Status    `json:"from,omitempty"`
	To        Status    `json:"to"`
	Phase     Phase     `json:"phase"`
	Cause     Cause     `json:"cause"`
	AttemptID AttemptID `json:"attempt_id,omitempty"`
	SignalID  string    `json:"signal_id,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// Record is a full post-transition snapshot.
type Record struct {
	Execution  Execution  `json:"execution"`
	Transition Transition `json:"transition"`
}

// ListOptions bounds one lexicographically ordered store page.
type ListOptions struct {
	Limit  int
	Cursor string
}

// ListPage is a bounded page of current Execution snapshots.
type ListPage struct {
	Executions []Execution
	NextCursor string
}

// Gate lets a host yield before an Advance without mutating state.
type Gate func(context.Context, Execution) (bool, error)

// DriveOptions bounds synchronous advancement.
type DriveOptions struct {
	MaxAdvances int
	Gate        Gate
}

// DriveResult is the latest state and why Drive yielded.
type DriveResult struct {
	Execution Execution
	Advances  int
	Yield     YieldReason
}

// Terminal reports whether the status cannot be resumed or retried.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusLimited:
		return true
	default:
		return false
	}
}

func cloneJSON(value ai.JSON) ai.JSON { return slices.Clone(value) }

func cloneExecution(in Execution) Execution {
	out := in
	out.ControllerState = cloneJSON(in.ControllerState)
	out.NextInput = cloneJSON(in.NextInput)
	out.Output = cloneJSON(in.Output)
	out.Activation = cloneActivation(in.Activation)
	out.Wait = cloneWait(in.Wait)
	out.Block = cloneBlock(in.Block)
	out.Suspension = cloneSuspension(in.Suspension)
	out.CurrentAttempt = cloneAttempt(in.CurrentAttempt)
	out.LastAttempt = cloneAttempt(in.LastAttempt)

	return out
}

func cloneAttempt(in *Attempt) *Attempt {
	if in == nil {
		return nil
	}

	out := *in

	out.Activation = cloneActivation(in.Activation)
	if in.Work != nil {
		work := *in.Work
		work.Value = cloneJSON(in.Work.Value)
		out.Work = &work
	}

	if in.Decision != nil {
		decision := cloneDecision(*in.Decision)
		out.Decision = &decision
	}

	if in.Failure != nil {
		failure := *in.Failure
		out.Failure = &failure
	}

	return &out
}

func cloneDecision(in Decision) Decision {
	out := in
	out.State = cloneJSON(in.State)
	out.NextInput = cloneJSON(in.NextInput)
	out.Output = cloneJSON(in.Output)
	out.Wait = cloneWait(in.Wait)
	out.Block = cloneBlock(in.Block)

	return out
}

func cloneActivation(in *Activation) *Activation {
	if in == nil {
		return nil
	}

	out := *in
	out.Payload = cloneJSON(in.Payload)

	return &out
}

func cloneWait(in *WaitCondition) *WaitCondition {
	if in == nil {
		return nil
	}

	out := *in
	if in.NotBefore != nil {
		notBefore := *in.NotBefore
		out.NotBefore = &notBefore
	}

	if in.Signal != nil {
		signal := *in.Signal
		out.Signal = &signal
	}

	return &out
}

func cloneBlock(in *Block) *Block {
	if in == nil {
		return nil
	}

	out := *in
	out.Data = cloneJSON(in.Data)

	return &out
}

func cloneSuspension(in *Suspension) *Suspension {
	if in == nil {
		return nil
	}

	out := *in
	out.Wait = cloneWait(in.Wait)
	out.Block = cloneBlock(in.Block)

	return &out
}

func cloneRecord(in Record) Record {
	out := in
	out.Execution = cloneExecution(in.Execution)

	return out
}
