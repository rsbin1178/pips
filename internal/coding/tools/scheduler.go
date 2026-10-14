package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/scheduler"
	"github.com/rsbin1178/pips/internal/jsonx"
)

// The three model-facing scheduler Tool names.
const (
	// SchedulerCreateName schedules a prompt on an interval, or updates one in
	// place by task_id.
	SchedulerCreateName = "scheduler_create"
	// SchedulerDeleteName cancels a scheduled task by id.
	SchedulerDeleteName = "scheduler_delete"
	// SchedulerListName lists what is scheduled.
	SchedulerListName = "scheduler_list"
)

// schedulerListPromptBytes bounds one listed prompt, matching the reference's
// summary truncation.
const schedulerListPromptBytes = 80

// SchedulerSurface is the injected store boundary the scheduler Tools read and
// write through. A catalog built without one offers no scheduler Tool.
type SchedulerSurface interface {
	Create(context.Context, scheduler.CreateRequest) (scheduler.Task, error)
	Update(context.Context, string, scheduler.UpdateRequest) (scheduler.Task, error)
	Delete(context.Context, string) (bool, error)
	List(context.Context) ([]scheduler.Task, error)
}

// NewSchedulerCatalog returns the Agent-mode application catalog of scheduled
// task Tools. It is offered exactly where update_plan is offered, and a nil
// store offers none of them.
func NewSchedulerCatalog(store SchedulerSurface) (*catalog.Catalog, error) {
	if store == nil {
		return catalog.New()
	}

	return catalog.New(
		localEntry(NewSchedulerCreateTool(store), catalog.RiskWrite, "planning", "schedule"),
		localEntry(NewSchedulerDeleteTool(store), catalog.RiskWrite, "planning", "schedule"),
		localEntry(NewSchedulerListTool(store), catalog.RiskRead, "planning", "schedule"),
	)
}

// schedulerCreateArgs is the create Tool's input. Every field is optional in the
// schema: interval and prompt are required to create and optional with task_id.
type schedulerCreateArgs struct {
	TaskID          string `json:"task_id" description:"Id of an existing task to update in place: provided fields replace old values, omitted ones are unchanged, the schedule keeps its phase, and an unknown id errors. Omit to create a task."`
	Interval        string `json:"interval" description:"Interval between executions, e.g. \"5m\", \"2h\", \"1d\". Required to create; optional with task_id."`
	Prompt          string `json:"prompt" description:"The prompt text to execute on each scheduled fire. Required to create; optional with task_id."`
	Durable         *bool  `json:"durable" description:"Whether the task persists across sessions. Default: false. Create-only: ignored with task_id."`
	FireImmediately *bool  `json:"fire_immediately" description:"Whether to fire immediately on creation (true) or wait for the first interval (false). Default: false. Create-only: ignored with task_id."`
}

// NewSchedulerCreateTool builds the model-facing create Tool. It creates a
// recurring scheduled task or updates an existing one in place.
func NewSchedulerCreateTool(store SchedulerSurface) agent.Tool {
	schema, err := ai.SchemaFor[schedulerCreateArgs]()
	if err != nil {
		panic(fmt.Sprintf("coding tools: scheduler create schema: %v", err))
	}
	// Nothing is required: interval and prompt are required only to create, and
	// the store refuses a create that omits either.
	schema.Required = nil

	return &schedulerCreateTool{store: store, decl: ai.Tool{
		Name: SchedulerCreateName,
		Description: "Create a scheduled task that runs a prompt on a recurring interval, " +
			"or update an existing one in place.\n\n" +
			"Use this tool when a user asks you to loop, repeat, or schedule a prompt or a task.\n\n" +
			"Set fire_immediately: true to also fire once on creation; by default the first " +
			"run waits for the interval. Set durable: true to keep the task across sessions.\n\n" +
			"To change an existing task, pass its task_id: provided fields replace old values, " +
			"omitted ones are unchanged, and the schedule keeps its phase. An unknown id errors.\n\n" +
			"Usage notes:\n" +
			"- Interval format: \"5m\" (minutes), \"2h\" (hours), \"1d\" (days), \"60s\" (seconds, min 60)\n" +
			"- A scheduled task keeps firing until it is deleted\n" +
			"- Each fire starts a background subagent, and its completion notifies you",
		InputSchema: schema,
	}}
}

type schedulerCreateTool struct {
	store SchedulerSurface
	decl  ai.Tool
}

func (t *schedulerCreateTool) Decl() ai.Tool { return t.decl }

func (t *schedulerCreateTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	var args schedulerCreateArgs

	if err := jsonx.Decode(call.Args, &args); err != nil {
		return nil, failure(SchedulerCreateName, fmt.Errorf(
			"%w: arguments must be one object with interval and prompt, or task_id and a patch",
			errInvalidArgument,
		))
	}

	if t.store == nil {
		return nil, failure(SchedulerCreateName, fmt.Errorf("%w: scheduler is unavailable", errInvalidArgument))
	}

	interval, err := schedulerInterval(args.Interval)
	if err != nil {
		return nil, failure(SchedulerCreateName, err)
	}

	if strings.TrimSpace(args.TaskID) != "" {
		return t.update(ctx, args, interval)
	}

	return t.create(ctx, args, interval)
}

// update applies one in-place patch. A call that names no field to change is
// refused rather than silently rewriting the task.
func (t *schedulerCreateTool) update(
	ctx context.Context,
	args schedulerCreateArgs,
	interval *int64,
) ([]ai.Part, error) {
	prompt := strings.TrimSpace(args.Prompt)

	if interval == nil && prompt == "" {
		return nil, failure(SchedulerCreateName, fmt.Errorf(
			"%w: nothing to update: provide interval and/or prompt alongside task_id",
			errInvalidArgument,
		))
	}

	patch := scheduler.UpdateRequest{IntervalSeconds: interval}
	if prompt != "" {
		patch.Prompt = &args.Prompt
	}

	task, err := t.store.Update(ctx, strings.TrimSpace(args.TaskID), patch)
	if err != nil {
		return nil, failure(SchedulerCreateName, err)
	}

	return schedulerCreatedResult(task, true)
}

// create adds one recurring task.
func (t *schedulerCreateTool) create(
	ctx context.Context,
	args schedulerCreateArgs,
	interval *int64,
) ([]ai.Part, error) {
	if interval == nil {
		return nil, failure(SchedulerCreateName, fmt.Errorf(
			"%w: interval is required when creating a task", errInvalidArgument,
		))
	}

	if strings.TrimSpace(args.Prompt) == "" {
		return nil, failure(SchedulerCreateName, fmt.Errorf(
			"%w: prompt is required when creating a task", errInvalidArgument,
		))
	}

	task, err := t.store.Create(ctx, scheduler.CreateRequest{
		Prompt:          args.Prompt,
		IntervalSeconds: *interval,
		Recurring:       true,
		Durable:         args.Durable != nil && *args.Durable,
		FireImmediately: args.FireImmediately != nil && *args.FireImmediately,
	})
	if err != nil {
		return nil, failure(SchedulerCreateName, err)
	}

	return schedulerCreatedResult(task, false)
}

// schedulerInterval parses an optional interval argument. An absent value is
// nil rather than zero, so an update can tell "omitted" from "changed".
func schedulerInterval(value string) (*int64, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}

	parsed, err := scheduler.ParseInterval(value)
	if err != nil {
		return nil, err
	}

	return &parsed, nil
}

// schedulerCreatedResult renders the reference's create output.
func schedulerCreatedResult(task scheduler.Task, updated bool) ([]ai.Part, error) {
	data, err := json.Marshal(struct {
		ID            string `json:"id"`
		HumanSchedule string `json:"humanSchedule"`
		Updated       bool   `json:"updated"`
	}{ID: task.ID, HumanSchedule: scheduler.IntervalHuman(task.IntervalSecs), Updated: updated})
	if err != nil {
		return nil, failure(SchedulerCreateName, fmt.Errorf("coding tools: encode scheduler result: %w", err))
	}

	return agent.TextResult(string(data)), nil
}

// schedulerDeleteArgs is the delete Tool's input.
type schedulerDeleteArgs struct {
	ID string `json:"id" description:"The task ID to cancel (from scheduler_create output)."`
}

// NewSchedulerDeleteTool builds the model-facing cancel Tool. An id that does
// not exist reports success: false rather than failing the call.
func NewSchedulerDeleteTool(store SchedulerSurface) agent.Tool {
	schema, err := ai.SchemaFor[schedulerDeleteArgs]()
	if err != nil {
		panic(fmt.Sprintf("coding tools: scheduler delete schema: %v", err))
	}

	return &schedulerDeleteTool{store: store, decl: ai.Tool{
		Name: SchedulerDeleteName,
		Description: "Cancel a scheduled task by ID.\n\n" +
			"Returns success: true if the task was found and removed, " +
			"false if no task with that ID exists.",
		InputSchema: schema,
	}}
}

type schedulerDeleteTool struct {
	store SchedulerSurface
	decl  ai.Tool
}

func (t *schedulerDeleteTool) Decl() ai.Tool { return t.decl }

func (t *schedulerDeleteTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	var args schedulerDeleteArgs

	if err := jsonx.Decode(call.Args, &args); err != nil {
		return nil, failure(SchedulerDeleteName, fmt.Errorf(
			"%w: arguments must be one object with id", errInvalidArgument,
		))
	}

	id := strings.TrimSpace(args.ID)
	if id == "" {
		return nil, failure(SchedulerDeleteName, fmt.Errorf("%w: id is required", errInvalidArgument))
	}

	if t.store == nil {
		return nil, failure(SchedulerDeleteName, fmt.Errorf("%w: scheduler is unavailable", errInvalidArgument))
	}

	removed, err := t.store.Delete(ctx, id)
	if err != nil {
		return nil, failure(SchedulerDeleteName, err)
	}

	message := "No scheduled task with ID " + id + " found. Use " + SchedulerListName + " to see active tasks."
	if removed {
		message = "Scheduled task " + id + " cancelled."
	}

	data, err := json.Marshal(struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}{Success: removed, Message: message})
	if err != nil {
		return nil, failure(SchedulerDeleteName, fmt.Errorf("coding tools: encode scheduler result: %w", err))
	}

	return agent.TextResult(string(data)), nil
}

// schedulerListArgs is the list Tool's empty input.
type schedulerListArgs struct{}

// schedulerTaskSummary is one listed task, matching the reference's summary
// fields.
type schedulerTaskSummary struct {
	ID            string `json:"id"`
	Prompt        string `json:"prompt"`
	IntervalHuman string `json:"intervalHuman"`
	NextFireAt    string `json:"nextFireAt"`
	CreatedAt     string `json:"createdAt"`
	Recurring     bool   `json:"recurring"`
}

// NewSchedulerListTool builds the model-facing list Tool.
func NewSchedulerListTool(store SchedulerSurface) agent.Tool {
	schema, err := ai.SchemaFor[schedulerListArgs]()
	if err != nil {
		panic(fmt.Sprintf("coding tools: scheduler list schema: %v", err))
	}

	return &schedulerListTool{store: store, decl: ai.Tool{
		Name: SchedulerListName,
		Description: "List all active scheduled tasks with their IDs, prompts, " +
			"intervals, and next fire times.",
		InputSchema: schema,
	}}
}

type schedulerListTool struct {
	store SchedulerSurface
	decl  ai.Tool
}

func (t *schedulerListTool) Decl() ai.Tool { return t.decl }

func (t *schedulerListTool) Exec(ctx context.Context, _ agent.ToolCall) ([]ai.Part, error) {
	if t.store == nil {
		return nil, failure(SchedulerListName, fmt.Errorf("%w: scheduler is unavailable", errInvalidArgument))
	}

	tasks, err := t.store.List(ctx)
	if err != nil {
		return nil, failure(SchedulerListName, err)
	}

	summaries := make([]schedulerTaskSummary, 0, len(tasks))
	for _, task := range tasks {
		summaries = append(summaries, schedulerTaskSummary{
			ID:            task.ID,
			Prompt:        truncateSchedulerPrompt(task.Prompt),
			IntervalHuman: scheduler.IntervalHuman(task.IntervalSecs),
			NextFireAt:    task.NextFireAt.UTC().Format(time.RFC3339),
			CreatedAt:     task.CreatedAt.UTC().Format(time.RFC3339),
			Recurring:     task.Recurring,
		})
	}

	data, err := json.Marshal(struct {
		Tasks []schedulerTaskSummary `json:"tasks"`
	}{Tasks: summaries})
	if err != nil {
		return nil, failure(SchedulerListName, fmt.Errorf("coding tools: encode scheduler result: %w", err))
	}

	return agent.TextResult(string(data)), nil
}

// truncateSchedulerPrompt bounds one listed prompt at the reference's cut,
// appending an ellipsis when it was shortened.
func truncateSchedulerPrompt(prompt string) string {
	if len(prompt) <= schedulerListPromptBytes {
		return prompt
	}

	runes := []rune(prompt)
	if len(runes) <= schedulerListPromptBytes {
		return prompt
	}

	return string(runes[:schedulerListPromptBytes]) + "..."
}
