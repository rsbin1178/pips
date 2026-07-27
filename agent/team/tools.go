package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/ai"
)

// ToolCommandContext identifies one model-originated Team command.
type ToolCommandContext struct {
	TeamID     ID       `json:"team_id"`
	MemberID   MemberID `json:"member_id"`
	ToolName   string   `json:"tool_name"`
	ToolCallID string   `json:"tool_call_id"`
}

// ToolCommandIDSource derives a durable command key for a bound tool call.
type ToolCommandIDSource func(ToolCommandContext) (CommandID, error)

// ToolsetOption configures bound Team tools.
type ToolsetOption func(*toolsetConfig) error

type toolsetConfig struct {
	commands ToolCommandIDSource
}

// WithToolCommandIDSource replaces the default ToolCall-derived command key.
func WithToolCommandIDSource(source ToolCommandIDSource) ToolsetOption {
	return func(config *toolsetConfig) error {
		if source == nil {
			return fmt.Errorf("%w: nil tool command ID source", ErrInvalid)
		}

		config.commands = source

		return nil
	}
}

// Toolset is a fixed collection of tools bound to one Team member identity.
type Toolset struct {
	tools []agent.Tool
}

// NewMemberToolset builds ordinary member tools scoped to one durable member.
func NewMemberToolset(
	engine *Engine,
	teamID ID,
	memberID MemberID,
	options ...ToolsetOption,
) (*Toolset, error) {
	return newToolset(engine, teamID, memberID, false, options...)
}

// NewLeadToolset builds member tools plus fixed-lead governance tools.
func NewLeadToolset(
	engine *Engine,
	teamID ID,
	leadMemberID MemberID,
	options ...ToolsetOption,
) (*Toolset, error) {
	return newToolset(engine, teamID, leadMemberID, true, options...)
}

// Tools returns a copy of the bound tool collection.
func (toolset *Toolset) Tools() []agent.Tool {
	if toolset == nil {
		return nil
	}

	return slices.Clone(toolset.tools)
}

type toolBinding struct {
	engine   *Engine
	teamID   ID
	memberID MemberID
	commands ToolCommandIDSource
}

func newToolset(
	engine *Engine,
	teamID ID,
	memberID MemberID,
	lead bool,
	options ...ToolsetOption,
) (*Toolset, error) {
	if engine == nil {
		return nil, fmt.Errorf("%w: nil Team engine", ErrInvalid)
	}

	if err := validateSafeID("Team id", string(teamID)); err != nil {
		return nil, err
	}

	if err := validateSafeID("member id", string(memberID)); err != nil {
		return nil, err
	}

	config := toolsetConfig{commands: defaultToolCommandID}

	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("%w: nil toolset option", ErrInvalid)
		}

		if err := option(&config); err != nil {
			return nil, err
		}
	}

	team, err := engine.Get(context.Background(), teamID)
	if err != nil {
		return nil, err
	}

	member, _, found := findMember(team, memberID)
	if !found || member.Status != MemberStatusActive {
		return nil, ErrUnauthorized
	}

	if lead && team.LeadMemberID != memberID {
		return nil, ErrUnauthorized
	}

	binding := &toolBinding{
		engine: engine, teamID: teamID, memberID: memberID,
		commands: config.commands,
	}

	tools := binding.memberTools()
	if lead {
		tools = append(tools, binding.leadTools()...)
	}

	return &Toolset{tools: tools}, nil
}

func (binding *toolBinding) metadata(
	ctx context.Context,
	call agent.ToolCall,
) (CommandMetadata, error) {
	if call.ID == "" {
		return CommandMetadata{}, fmt.Errorf("%w: empty tool call ID", ErrInvalid)
	}

	team, err := binding.engine.Get(ctx, binding.teamID)
	if err != nil {
		return CommandMetadata{}, err
	}

	commandID, err := binding.commands(ToolCommandContext{
		TeamID: binding.teamID, MemberID: binding.memberID,
		ToolName: call.Name, ToolCallID: call.ID,
	})
	if err != nil {
		return CommandMetadata{}, fmt.Errorf("team: derive tool command ID: %w", err)
	}

	if err := validateSafeID("command id", string(commandID)); err != nil {
		return CommandMetadata{}, err
	}

	return CommandMetadata{
		ID: commandID, ExpectedRevision: team.Revision,
		Actor: Actor{Kind: ActorKindMember, ID: string(binding.memberID)},
	}, nil
}

func defaultToolCommandID(input ToolCommandContext) (CommandID, error) {
	if input.ToolCallID == "" {
		return "", fmt.Errorf("%w: empty tool call ID", ErrInvalid)
	}

	data, err := json.Marshal(input)
	if err != nil {
		return "", err
	}

	digest := sha256.Sum256(data)

	return CommandID("tool-" + hex.EncodeToString(digest[:])), nil
}

func derivedToolID(prefix string, commandID CommandID) string {
	digest := sha256.Sum256([]byte(prefix + ":" + string(commandID)))

	return prefix + "-" + hex.EncodeToString(digest[:16])
}

type boundTool struct {
	declaration ai.Tool
	execute     func(context.Context, agent.ToolCall) (any, error)
}

func (tool *boundTool) Decl() ai.Tool { return tool.declaration }

func (tool *boundTool) Exec(ctx context.Context, call agent.ToolCall) ([]ai.Part, error) {
	if call.Name != tool.declaration.Name {
		return nil, fmt.Errorf("%w: tool call name mismatch", ErrInvalid)
	}

	result, err := tool.execute(ctx, call)
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("team: encode tool result: %w", err)
	}

	return agent.TextResult(string(data)), nil
}

func newBoundTool[Arguments any](
	name string,
	description string,
	execute func(context.Context, agent.ToolCall, Arguments) (any, error),
) agent.Tool {
	declaration := ai.Tool{Name: name, Description: description}
	typeOfArguments := reflect.TypeFor[Arguments]()

	noArguments := typeOfArguments.Kind() == reflect.Struct && typeOfArguments.NumField() == 0
	if !noArguments {
		schema, err := ai.SchemaFor[Arguments]()
		if err != nil {
			panic(fmt.Sprintf("team: define tool %q: %v", name, err))
		}

		declaration.InputSchema = schema
	}

	return &boundTool{
		declaration: declaration,
		execute: func(ctx context.Context, call agent.ToolCall) (any, error) {
			var arguments Arguments
			if !noArguments && len(call.Args) > 0 {
				if err := json.Unmarshal(call.Args, &arguments); err != nil {
					return nil, fmt.Errorf("invalid arguments: %w", err)
				}
			}

			return execute(ctx, call, arguments)
		},
	}
}

type teamStatusView struct {
	ID           ID       `json:"id"`
	Revision     Revision `json:"revision"`
	Status       Status   `json:"status"`
	Objective    string   `json:"objective"`
	LeadMemberID MemberID `json:"lead_member_id"`
	Members      int      `json:"members"`
	Tasks        int      `json:"tasks"`
	Reason       string   `json:"reason,omitempty"`
}

type taskView struct {
	ID               TaskID     `json:"id"`
	Title            string     `json:"title"`
	Status           TaskStatus `json:"status"`
	DependencyIDs    []TaskID   `json:"dependency_ids,omitempty"`
	AssignedMemberID MemberID   `json:"assigned_member_id,omitempty"`
	ClaimedMemberID  MemberID   `json:"claimed_member_id,omitempty"`
	Attempts         int        `json:"attempts"`
	AttemptLimit     int        `json:"attempt_limit"`
	Reason           string     `json:"reason,omitempty"`
}

type taskMutationView struct {
	TeamRevision Revision `json:"team_revision"`
	Task         taskView `json:"task"`
}

func statusView(team Team) teamStatusView {
	return teamStatusView{
		ID: team.ID, Revision: team.Revision, Status: team.Status,
		Objective: team.Objective, LeadMemberID: team.LeadMemberID,
		Members: len(team.Members), Tasks: len(team.Tasks), Reason: team.Reason,
	}
}

func newTaskView(task Task) taskView {
	return taskView{
		ID: task.ID, Title: task.Title, Status: task.Status,
		DependencyIDs:    slices.Clone(task.DependencyIDs),
		AssignedMemberID: task.AssignedMemberID, ClaimedMemberID: task.ClaimedMemberID,
		Attempts: len(task.Attempts), AttemptLimit: task.AttemptLimit, Reason: task.Reason,
	}
}

func taskResult(team Team, taskID TaskID) (taskMutationView, error) {
	task, _, found := findTask(team, taskID)
	if !found {
		return taskMutationView{}, ErrNotFound
	}

	return taskMutationView{TeamRevision: team.Revision, Task: newTaskView(task)}, nil
}

func newTaskMutationBoundTool[Arguments any](
	binding *toolBinding,
	name string,
	description string,
	mutate func(context.Context, CommandMetadata, Arguments) (Team, TaskID, error),
) agent.Tool {
	return newBoundTool(
		name,
		description,
		func(ctx context.Context, call agent.ToolCall, arguments Arguments) (any, error) {
			command, err := binding.metadata(ctx, call)
			if err != nil {
				return nil, err
			}

			team, taskID, err := mutate(ctx, command, arguments)
			if err != nil {
				return nil, err
			}

			return taskResult(team, taskID)
		},
	)
}
