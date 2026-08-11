package workflow

import (
	"context"
	"fmt"
	"slices"
)

// CheckpointStore persists one opaque, versioned Workflow checkpoint by Run ID.
// Implementations must replace a key atomically and detach returned buffers.
type CheckpointStore interface {
	Get(context.Context, string) ([]byte, bool, error)
	Set(context.Context, string, []byte) error
}

// NodeAddress identifies one node invocation inside its composite scope.
type NodeAddress struct {
	NodeID NodeID
	Scope  []ScopeFrame
}

// InterruptContext describes one targetable dynamic interruption.
type InterruptContext struct {
	ID      string
	Address NodeAddress
	Info    Value
}

// InterruptInfo aggregates every interruption observed at the settled
// scheduler frontier. Only Contexts are targetable with ResumeTarget.
type InterruptInfo struct {
	Contexts    []InterruptContext
	BeforeNodes []NodeAddress
	AfterNodes  []NodeAddress
	RerunNodes  []NodeAddress
}

// ResumeTarget supplies optional data to one dynamic interruption ID.
type ResumeTarget struct {
	InterruptID string
	Data        Value
}

// InterruptError reports a successfully checkpointed, resumable Run.
type InterruptError struct {
	RunID string
	Info  InterruptInfo
}

// Error implements error.
func (e *InterruptError) Error() string {
	return fmt.Sprintf("%v: run %q", ErrInterrupted, e.RunID)
}

// Unwrap exposes ErrInterrupted for errors.Is.
func (e *InterruptError) Unwrap() error {
	return ErrInterrupted
}

func cloneNodeAddress(address NodeAddress) NodeAddress {
	address.Scope = slices.Clone(address.Scope)

	return address
}

func cloneInterruptInfo(info InterruptInfo) InterruptInfo {
	cloned := InterruptInfo{
		Contexts:    make([]InterruptContext, len(info.Contexts)),
		BeforeNodes: make([]NodeAddress, len(info.BeforeNodes)),
		AfterNodes:  make([]NodeAddress, len(info.AfterNodes)),
		RerunNodes:  make([]NodeAddress, len(info.RerunNodes)),
	}

	for index, context := range info.Contexts {
		cloned.Contexts[index] = InterruptContext{
			ID: context.ID, Address: cloneNodeAddress(context.Address), Info: context.Info,
		}
	}

	for index, address := range info.BeforeNodes {
		cloned.BeforeNodes[index] = cloneNodeAddress(address)
	}

	for index, address := range info.AfterNodes {
		cloned.AfterNodes[index] = cloneNodeAddress(address)
	}

	for index, address := range info.RerunNodes {
		cloned.RerunNodes[index] = cloneNodeAddress(address)
	}

	return cloned
}

func interruptInfoEmpty(info InterruptInfo) bool {
	return len(info.Contexts) == 0 && len(info.BeforeNodes) == 0 &&
		len(info.AfterNodes) == 0 && len(info.RerunNodes) == 0
}
