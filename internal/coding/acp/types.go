// Package acp exposes the Coding Runtime through Agent Client Protocol v1.
package acp

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/mcp"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/planreview"
	"github.com/rsbin/pips/internal/coding/question"
)

var (
	// ErrInvalid means ACP input is malformed or unsupported.
	ErrInvalid = errors.New("coding acp: invalid input")
	// ErrSessionNotFound means no active ACP session owns the supplied ID.
	ErrSessionNotFound = errors.New("coding acp: session not found")
	// ErrSessionBusy means a prompt is already active for the session.
	ErrSessionBusy = errors.New("coding acp: session busy")
	// ErrClosed means the ACP server or session has been closed.
	ErrClosed              = errors.New("coding acp: closed")
	errInitializationState = errors.New("coding acp: invalid initialization state")
)

// Implementation is the protocol-neutral identity advertised by the ACP
// adapter. The SDK representation stays private to this package.
type Implementation struct {
	Name    string
	Title   string
	Version string
}

// Limits bound one ACP process and its untrusted protocol input.
type Limits struct {
	MaxSessions     int
	MaxMCPServers   int
	MaxPromptBlocks int
	MaxPromptBytes  int
	MaxBinaryBytes  int
	ListPageSize    int
}

// DefaultLimits returns conservative process and request bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxSessions: 32, MaxMCPServers: 64, MaxPromptBlocks: 128,
		MaxPromptBytes: 1 << 20, MaxBinaryBytes: 8 << 20, ListPageSize: 100,
	}
}

// Controller is the narrow Runtime surface consumed by ACP sessions.
type Controller interface {
	Prompt(context.Context, ...ai.Message) iter.Seq2[coding.Event, error]
	Continue(context.Context) iter.Seq2[coding.Event, error]
	Resolve(context.Context, approval.Resolution) iter.Seq2[coding.Event, error]
	ResolveQuestion(context.Context, question.Resolution) iter.Seq2[coding.Event, error]
	ResolvePlanReview(context.Context, planreview.Resolution) iter.Seq2[coding.Event, error]
	RejectQuestion(context.Context, string, string) iter.Seq2[coding.Event, error]
	Cancel() error
	Snapshot() coding.State
	SetMode(context.Context, coding.OperatingMode) error
	Models() []modelcatalog.Entry
	SwitchSessionModel(context.Context, modelcatalog.Selection) error
	Close(context.Context) error
}

// SessionSpec identifies one Runtime to create or reopen.
type SessionSpec struct {
	ID             string
	CWD            string
	MCPDefinitions mcp.Definitions
}

// SessionMetadata is the durable subset returned by session/list.
type SessionMetadata struct {
	ID        string
	CWD       string
	Title     string
	UpdatedAt time.Time
}

// SessionFactory owns Runtime construction and durable metadata discovery.
type SessionFactory interface {
	Open(context.Context, SessionSpec) (Controller, error)
	List(context.Context) ([]SessionMetadata, error)
	Delete(context.Context, string) error
}

// Config supplies process-level ACP dependencies.
type Config struct {
	Factory   SessionFactory
	AgentInfo Implementation
	Limits    Limits
	Logger    *slog.Logger
}

func validateConfig(config Config) error {
	limits := config.Limits
	if config.Factory == nil || config.AgentInfo.Name == "" || config.AgentInfo.Version == "" ||
		limits.MaxSessions <= 0 || limits.MaxMCPServers <= 0 || limits.MaxPromptBlocks <= 0 ||
		limits.MaxPromptBytes <= 0 || limits.MaxBinaryBytes <= 0 || limits.ListPageSize <= 0 {
		return ErrInvalid
	}

	return nil
}
