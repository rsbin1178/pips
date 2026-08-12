package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/continuation"
	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/agent/team"
	"github.com/rsbin1178/pips/ai"
)

type streamedHarnessResult struct {
	Text  string
	Stop  agent.StopReason
	Turns int
	Usage ai.Usage
}

type harnessStreamState struct {
	result         streamedHarnessResult
	finalAssistant *ai.AssistantMessage
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
		return state.result, errors.New("stream ended without a run_completed event")
	}

	state.result.Text = messageText(state.finalAssistant)

	return state.result, nil
}

func (state *harnessStreamState) handle(
	coordinator *teamCoordinator, label string,
	event agent.Event,
) error {
	switch payload := event.Payload().(type) {
	case agent.TurnStarted:
		return coordinator.writeStream("[%s] ", label)
	case agent.ModelStreamEvent:
		if payload.Event.Type == ai.StreamTextDelta {
			return coordinator.writeStream("%s", payload.Event.Text)
		}
	case agent.MessageCommitted:
		if message, ok := payload.Message.(ai.AssistantMessage); ok {
			state.finalAssistant = &message
		}
	case agent.ToolStarted:
		return coordinator.printf("[%s tool] %s\n", label, payload.Call.Name)
	case agent.TurnCompleted:
		return coordinator.writeStream("\n")
	case agent.RunCompleted:
		state.result.Stop = payload.Stop
		state.result.Turns = payload.Turns
		state.result.Usage = payload.Usage
		state.ended = true
	case agent.RunStarted, agent.CandidateDiscarded,
		agent.ToolUpdated, agent.ToolCompleted:
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

func messageText(message *ai.AssistantMessage) string {
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
