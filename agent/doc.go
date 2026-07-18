// Package agent provides the runtime core for autonomous LLM agents: a loop
// that calls a language model, executes the tools it requests, feeds results
// back, and repeats until the model finishes or a stop condition fires.
//
// It builds on the sibling [github.com/rsbin/pips/ai] package — any
// [ai.LanguageModel] (with whatever middleware stack) drives an agent, and
// conversations are ordinary [ai.Message] histories.
//
// The core abstraction is [Agent], an immutable bundle of model, system
// prompt, tools, and loop policy. Runs mutate a [Session]:
//
//	calc := agent.NewTool("add", "Add two integers.",
//	    func(ctx context.Context, args struct {
//	        A int `json:"a"`
//	        B int `json:"b"`
//	    }) (string, error) {
//	        return strconv.Itoa(args.A + args.B), nil
//	    })
//
//	a, err := agent.New(model, agent.WithTools(calc))
//	sess := agent.NewSession()
//	result, err := a.Run(ctx, sess, ai.UserText("What is 2+3?"))
//
// [Agent.Stream] exposes the same loop as an event sequence (model deltas,
// tool lifecycle, turn boundaries); breaking out of the range loop cancels
// the run.
//
// Tool failures — errors, panics, timeouts, undecodable arguments, calls to
// unknown tools — never abort a run: each becomes an error tool result the
// model can react to. Every tool call the model issues is answered before
// the next model call, even on cancellation, so sessions stay resumable.
//
// A [WithBeforeTool] gate intercepts calls before execution: deny with a
// reason the model sees, or pause the run for out-of-band approval and
// resume it later via [Session.ResolvePending].
//
// The package has no third-party runtime dependencies.
package agent
