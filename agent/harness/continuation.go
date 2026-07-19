package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rsbin/pips/agent"
	"github.com/rsbin/pips/agent/continuation"
	"github.com/rsbin/pips/ai"
)

const (
	defaultContinuationTextBytes = 32 << 10
	maxContinuationEvidenceBytes = 256 << 10
)

// ContinuationPrompt maps neutral Work input to one Harness prompt.
type ContinuationPrompt func(context.Context, continuation.WorkRequest) ([]ai.Message, error)

// ContinuationResultMapper projects one Harness run into bounded Controller evidence.
type ContinuationResultMapper func(
	context.Context,
	continuation.WorkRequest,
	*agent.RunResult,
	string,
	string,
) (ai.JSON, error)

// ContinuationWorkerOption configures a ContinuationWorker.
type ContinuationWorkerOption func(*continuationWorkerConfig) error

type continuationWorkerConfig struct {
	maxTextBytes int
	mapper       ContinuationResultMapper
}

// WithContinuationTextLimit bounds final response text in default evidence.
func WithContinuationTextLimit(maxBytes int) ContinuationWorkerOption {
	return func(config *continuationWorkerConfig) error {
		if maxBytes <= 0 || maxBytes > maxContinuationEvidenceBytes {
			return fmt.Errorf("harness: invalid continuation text limit %d", maxBytes)
		}

		config.maxTextBytes = maxBytes

		return nil
	}
}

// WithContinuationResultMapper replaces the default evidence projection.
func WithContinuationResultMapper(mapper ContinuationResultMapper) ContinuationWorkerOption {
	return func(config *continuationWorkerConfig) error {
		if mapper == nil {
			return errors.New("harness: nil continuation result mapper")
		}

		config.mapper = mapper

		return nil
	}
}

// ContinuationWorker adapts exactly one Harness prompt to continuation.Worker.
type ContinuationWorker struct {
	harness *Harness
	prompt  ContinuationPrompt
	config  continuationWorkerConfig
}

// NewContinuationWorker creates a first-party Harness Worker adapter.
func NewContinuationWorker(
	harness *Harness,
	prompt ContinuationPrompt,
	options ...ContinuationWorkerOption,
) (*ContinuationWorker, error) {
	if harness == nil {
		return nil, errors.New("harness: nil continuation harness")
	}

	if prompt == nil {
		return nil, errors.New("harness: nil continuation prompt")
	}

	config := continuationWorkerConfig{maxTextBytes: defaultContinuationTextBytes}

	for _, option := range options {
		if option == nil {
			return nil, errors.New("harness: nil continuation worker option")
		}

		if err := option(&config); err != nil {
			return nil, err
		}
	}

	return &ContinuationWorker{harness: harness, prompt: prompt, config: config}, nil
}

// Run implements continuation.Worker with one complete PromptMessages call.
func (worker *ContinuationWorker) Run(
	ctx context.Context,
	request continuation.WorkRequest,
) (continuation.WorkResult, error) {
	result := continuation.WorkResult{Progress: continuation.ProgressUnknown}

	messages, err := worker.prompt(ctx, request)
	if err != nil {
		return result, err
	}

	if len(messages) == 0 {
		return result, errors.New("harness: continuation prompt returned no messages")
	}

	before := worker.harness.Session().LeafID()
	runResult, runErr := worker.harness.PromptMessages(ctx, messages...)

	after := worker.harness.Session().LeafID()
	if before != after {
		result.Progress = continuation.ProgressChanged
	} else {
		result.Progress = continuation.ProgressUnchanged
	}

	if runResult != nil {
		result.Turns = runResult.Turns
		result.Usage = runResult.Usage
	}

	var mapErr error
	if worker.config.mapper != nil {
		result.Value, mapErr = worker.config.mapper(ctx, request, runResult, before, after)
	} else {
		result.Value, mapErr = defaultContinuationEvidence(runResult, before, after, worker.config.maxTextBytes)
	}

	if mapErr == nil && (len(result.Value) > maxContinuationEvidenceBytes || !json.Valid(result.Value)) {
		mapErr = errors.New("harness: continuation evidence is invalid or too large")
	}

	return result, errors.Join(runErr, mapErr)
}

type continuationEvidence struct {
	RunID         string           `json:"run_id,omitempty"`
	ParentRunID   string           `json:"parent_run_id,omitempty"`
	Agent         string           `json:"agent,omitempty"`
	Stop          agent.StopReason `json:"stop,omitempty"`
	Turns         int              `json:"turns"`
	Usage         ai.Usage         `json:"usage"`
	Text          string           `json:"text,omitempty"`
	TextTruncated bool             `json:"text_truncated,omitempty"`
	PendingCalls  int              `json:"pending_calls,omitempty"`
	LeafBefore    string           `json:"leaf_before,omitempty"`
	LeafAfter     string           `json:"leaf_after,omitempty"`
}

func defaultContinuationEvidence(
	result *agent.RunResult,
	before string,
	after string,
	maxTextBytes int,
) (ai.JSON, error) {
	evidence := continuationEvidence{LeafBefore: before, LeafAfter: after}
	if result != nil {
		evidence.RunID = result.RunID
		evidence.ParentRunID = result.ParentRunID
		evidence.Agent = result.Agent
		evidence.Stop = result.Stop
		evidence.Turns = result.Turns
		evidence.Usage = result.Usage
		evidence.PendingCalls = len(result.Pending)
		evidence.Text, evidence.TextTruncated = truncateContinuationText(result.Text(), maxTextBytes)
	}

	data, err := json.Marshal(evidence)
	if err != nil {
		return nil, fmt.Errorf("harness: encode continuation evidence: %w", err)
	}

	return data, nil
}

func truncateContinuationText(text string, maxBytes int) (string, bool) {
	if len(text) <= maxBytes {
		return text, false
	}

	return strings.ToValidUTF8(text[:maxBytes], ""), true
}

var _ continuation.Worker = (*ContinuationWorker)(nil)
