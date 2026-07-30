package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/agent/harness"
	"github.com/rsbin/pips/agent/team"
	"github.com/rsbin/pips/ai"
)

type streamedHarnessResult struct {
	Text  string
	Stop  agent.StopReason
	Turns int
	Usage ai.Usage
}

type harnessStreamState struct {
	result         streamedHarnessResult
	finalAssistant *ai.Message
	ended          bool
}

// streamingMemberWorker 在一次 Continuation Work 中消费 Harness 事件流。
// TextDelta 立即写到终端，完整 assistant message 则用于持久化任务结果。
type streamingMemberWorker struct {
	coordinator *teamCoordinator
	harness     *harness.Harness
	taskID      team.TaskID
	label       string
}

func (worker *streamingMemberWorker) Run(
	ctx context.Context,
	request continuation.WorkRequest,
) (continuation.WorkResult, error) {
	result := continuation.WorkResult{Progress: continuation.ProgressUnknown}
	before := worker.harness.Session().LeafID()

	streamed, streamErr := worker.coordinator.streamHarness(
		ctx,
		worker.label,
		worker.harness,
		ai.UserText(memberDispatchPrompt(request.Input)),
	)

	after := worker.harness.Session().LeafID()
	if before == after {
		result.Progress = continuation.ProgressUnchanged
	} else {
		result.Progress = continuation.ProgressChanged
	}

	result.Turns = streamed.Turns
	result.Usage = streamed.Usage

	if streamErr != nil {
		return result, streamErr
	}

	text, err := boundedStreamText(streamed.Text, streamed.Stop)
	if err != nil {
		return result, err
	}

	result.Value, err = jsonValue(taskResultPayload{TaskID: worker.taskID, Text: text})

	return result, err
}

func (coordinator *teamCoordinator) streamHarness(
	ctx context.Context,
	label string,
	memberHarness *harness.Harness,
	messages ...ai.Message,
) (streamedHarnessResult, error) {
	state := harnessStreamState{}

	for event, streamErr := range memberHarness.PromptMessagesStream(ctx, messages...) {
		if streamErr != nil {
			return state.result, streamErr
		}

		if err := state.handle(coordinator, label, event); err != nil {
			return state.result, err
		}
	}

	if !state.ended {
		return state.result, errors.New("stream ended without a run_end event")
	}

	state.result.Text = messageText(state.finalAssistant)

	return state.result, nil
}

func (state *harnessStreamState) handle(
	coordinator *teamCoordinator, label string,
	event agent.Event,
) error {
	switch event.Type {
	case agent.EventTurnStart:
		return coordinator.writeStream("[%s] ", label)
	case agent.EventDelta:
		if event.Delta.Type == ai.StreamTextDelta {
			return coordinator.writeStream("%s", event.Delta.Text)
		}
	case agent.EventMessage:
		if event.Message != nil && event.Message.Role == ai.RoleAssistant {
			message := *event.Message
			state.finalAssistant = &message
		}
	case agent.EventToolStart:
		if event.Call != nil {
			return coordinator.printf("[%s tool] %s\n", label, event.Call.Name)
		}
	case agent.EventTurnEnd:
		return coordinator.writeStream("\n")
	case agent.EventRunEnd:
		state.result.Stop = event.Stop
		state.result.Turns = event.Turn
		state.result.Usage = event.Usage
		state.ended = true
	case agent.EventRunStart, agent.EventCandidateDiscard,
		agent.EventToolUpdate, agent.EventToolEnd:
	}

	return nil
}

func boundedStreamText(text string, stop agent.StopReason) (string, error) {
	if stop != agent.StopEndTurn {
		return "", fmt.Errorf("model run stopped with %s instead of %s", stop, agent.StopEndTurn)
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("model stream returned empty final text")
	}

	if len(text) > maxModelResultBytes {
		text = strings.ToValidUTF8(text[:maxModelResultBytes], "")
	}

	return text, nil
}

func messageText(message *ai.Message) string {
	if message == nil {
		return ""
	}

	var text strings.Builder

	for _, part := range message.Parts {
		if part, ok := part.(ai.TextPart); ok {
			text.WriteString(part.Text)
		}
	}

	return text.String()
}

func (coordinator *teamCoordinator) writeStream(format string, arguments ...any) error {
	if _, err := fmt.Fprintf(coordinator.stream, format, arguments...); err != nil {
		return fmt.Errorf("write model stream: %w", err)
	}

	return nil
}

var _ continuation.Worker = (*streamingMemberWorker)(nil)
