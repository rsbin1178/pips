package planmode

import "strings"

// Tool descriptions and model-facing text are fixed prompt contracts: their
// wording is deliberate and must stay stable across edits.

// EnterToolName is the model-facing entry gate.
const EnterToolName = "enter_plan_mode"

// ExitToolName is the model-facing exit gate.
const ExitToolName = "exit_plan_mode"

// EnterDescription is the enter_plan_mode tool description.
const EnterDescription = "Use this tool when a task has ambiguity about the right approach " +
	"or when the user asks you to write a plan. This tool enables a read-only plan mode " +
	"where you explore the codebase and create an implementation plan for the user."

// ExitDescription is the exit_plan_mode tool description.
const ExitDescription = "Exit plan mode and present your plan to the user.\n\n" +
	"Use this after you have finished writing your plan to the plan file in plan mode."

// ReminderNames are the tool names interpolated into the plan-mode reminder.
type ReminderNames struct {
	Edit string
	Ask  string
	Exit string
}

// PlanReminder renders the plan-mode reminder injected with each request while
// plan mode is active.
func PlanReminder(planPath string, hasContent bool, names ReminderNames) string {
	edit := orDefault(names.Edit, "edit")
	ask := orDefault(names.Ask, "ask_user")
	exit := orDefault(names.Exit, ExitToolName)

	var builder strings.Builder
	builder.WriteString("Plan mode is active. Do not make any edits or writes to the system.\n\n")
	builder.WriteString("## Plan File:\n")

	if hasContent {
		builder.WriteString("A plan file exists at " + planPath + ". You can read it and make edits " +
			"using the " + edit + " tool.\n")
	} else {
		builder.WriteString("No plan written yet. Write your plan to " + planPath + " using the " +
			edit + " tool.\n")
	}

	builder.WriteString("\nYou should build your plan by writing to or editing this file. " +
		"Note that this is the only file you are allowed to edit.\n")
	builder.WriteString("Your turn should only end with either " + ask + " to clarify requirements " +
		"or " + exit + " to present your plan to the user.")

	return builder.String()
}

// ReturningReminder renders the reminder used when plan mode is entered again
// while a plan file from the previous planning session still exists.
func ReturningReminder(planPath string, names ReminderNames) string {
	ask := orDefault(names.Ask, "ask_user")
	exit := orDefault(names.Exit, ExitToolName)

	return "## Returning to Plan Mode\n" +
		"You are entering plan mode again after having previously exited it. " +
		"A plan file exists at " + planPath + " from your previous planning session.\n\n" +
		"Your turn should only end with either " + ask + " to clarify requirements " +
		"or " + exit + " to present your plan to the user."
}

// IterationGuidance renders the plan-iteration-versus-execution guidance
// injected on user turns while plan mode is still active.
func IterationGuidance(ask string) string {
	question := orDefault(ask, "ask_user")

	return "Plan mode is still active.\n" +
		"- Understand the user's intent between plan iteration and execution: " +
		"Plan iteration happens when the user is providing feedback, iterating on what they want, " +
		"or requesting changes. Because we are still in plan mode, most actionable statements, " +
		"such as 'let's do this YYY way' or 'implement this feature using xxxx methodology' are " +
		"with the intention of **adding these items** to the plan (plan iteration), NOT execution. " +
		"The ONLY time execution happens is when the user's query is obviously referring to the " +
		"plan itself and telling you to execute it.\n" +
		"- If there is any ambiguity between plan iteration and execution, be conservative and " +
		"assume that the user is iterating on the plan.\n" +
		"- If iterating on the plan, always update the plan document accordingly without executing, " +
		"do NOT begin making edits or executing the plan.\n" +
		"- Any iterations and feedback MUST be reflected in the plan document until the plan has " +
		"been executed.\n" +
		"- To ask clarifying questions about the plan, use the " + question + " tool to present " +
		"them to the user. Do not ask questions as pure text in your final assistant message; " +
		"resolve any ambiguity with " + question + ".\n" +
		"# Examples\n" +
		"## When to execute the plan (user explicitly asks)\n" +
		"- \"go ahead and implement the plan\" - makes it clear that the user is asking you to execute the plan.\n" +
		"- \"execute the plan\" - the user is directly asking you to execute the plan.\n" +
		"- \"start implementing\" / \"ok, do it\" / \"ship it\" / \"let's execute\" - when this is the user's " +
		"only ask, it means they want you to execute the plan. If it is followed by implementation details, " +
		"it is plan iteration, not an execution request.\n" +
		"## When NOT to execute the plan (user is iterating - update the plan document instead)\n" +
		"- \"implement the cache using Redis\" - The user is describing what the plan should contain, " +
		"not asking you to go write code. Add this to the plan.\n" +
		"- \"okay make the poller loop over each shard\" - Action verbs like \"make\" here refer to how " +
		"the design should work, not a command to start coding. Update the plan.\n" +
		"- \"actually let's do this with a lock manager instead\" - The user is revising the approach. " +
		"This is plan refinement, not execution.\n" +
		"- \"what do you think?\" - The user is asking for your opinion on the plan. Respond with feedback, do not execute.\n" +
		"- \"let's do the following approach: we partition into 32 shards and ...\" - The user is " +
		"describing an implementation strategy. This is plan content, not a request to execute.\n" +
		"- \"add error handling for the timeout case\" - \"Add\" here means add it to the plan, not go write the code.\n" +
		"- \"use a queue instead of polling\" - The user is specifying a design decision to incorporate into the plan.\n" +
		"- \"handle the edge case where the lock expires\" - The user is describing a requirement for the plan to cover.\n" +
		"Remember: Unless the user has explicitly and unambiguously asked you to execute, " +
		"you MUST NOT make any edits or run any non-readonly tools."
}

// Model-facing tool results and reminders.

// EnterResult is returned after an approved enter_plan_mode call.
const EnterResult = "You have entered plan mode. You should now focus on exploring the codebase " +
	"and creating an implementation plan."

// DeclineResult is returned when the user declines to enter plan mode.
const DeclineResult = "The user declined to enter plan mode. Continue in normal mode without it."

// ExitApprovedResult is returned when the user approves the plan.
const ExitApprovedResult = "Your plan has been approved. You can now start coding."

// ExitApprovedEmptyResult is returned when the user approves without a plan.
const ExitApprovedEmptyResult = "Plan mode exit approved. No plan content was found - you can proceed."

// ExitReviseResult is returned when the user requests changes instead of approving.
const ExitReviseResult = "The user does not want to exit plan mode. Continue planning and ask " +
	"the user what they would like to do."

// ExitQuitResult is returned when the user abandons the plan.
const ExitQuitResult = "The user chose to abandon the plan entirely (via the Abandon option in " +
	"the plan approval dialog). Plan mode has been disabled. Do not call " + ExitToolName +
	" again unless the user explicitly asks to re-enter plan mode."

// ApprovalCommentsPrefix introduces review comments attached to an approval.
const ApprovalCommentsPrefix = "The user approved the plan with the following review comments:"

// RevisionPrefix introduces freeform revision notes requested from the user.
const RevisionPrefix = "User revision notes:"

// ExitedReminder is injected on the first request after plan mode is turned off
// while a turn is still in flight.
const ExitedReminder = "You have exited plan mode. You can now make edits, run tools, and take actions."

// AgentModeReminder is injected when the session returns to agent mode.
const AgentModeReminder = "You are now in Agent mode. Continue with the task in the new mode."

// StillActiveReminder is the short reminder for follow-up requests.
const StillActiveReminder = "Plan mode is still active. Do not make any edits or writes to the " +
	"system except for the plan file."

// EditRejection is the model-facing rejection for edits outside the plan file.
func EditRejection(planPath string) string {
	return "Rejected: file edits are not allowed in plan mode - the only editable file is the " +
		"plan file (" + planPath + ")."
}

// ApprovalResult renders the approval tool result, including pending review
// comments when present.
func ApprovalResult(comments []string) string {
	if len(comments) == 0 {
		return ExitApprovedResult
	}

	return ExitApprovedResult + "\n\n" + ApprovalCommentsPrefix + "\n" +
		strings.Join(comments, "\n")
}

// RevisionResult renders the request-changes tool result with revision notes.
func RevisionResult(notes string) string {
	if strings.TrimSpace(notes) == "" {
		return ExitReviseResult
	}

	return ExitReviseResult + "\n\n" + RevisionPrefix + "\n" + notes
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}

	return value
}
