package cli

import (
	"context"
	"errors"

	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/approval"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/credential"
	"github.com/rsbin/pips/internal/coding/execution"
	"github.com/rsbin/pips/internal/coding/generation"
	"github.com/rsbin/pips/internal/coding/model"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
	"github.com/rsbin/pips/internal/coding/paths"
	"github.com/rsbin/pips/internal/coding/session"
	"github.com/rsbin/pips/internal/coding/workspace"
)

// Stable process exit codes for the Pips CLI.
const (
	ExitSuccess     = 0
	ExitFailure     = 1
	ExitUsage       = 2
	ExitInput       = 3
	ExitApproval    = ExitInput
	ExitSecurity    = 4
	ExitInterrupted = 130
	ExitTerminated  = 143
)

// ErrUsage means command-line input or local configuration is invalid.
var ErrUsage = errors.New("coding cli: invalid usage")

// ExitCode classifies one error tree without relying on display text.
func ExitCode(err error) int {
	if err == nil {
		return ExitSuccess
	}

	if errors.Is(err, context.Canceled) {
		return ExitInterrupted
	}

	if isApprovalError(err) {
		return ExitApproval
	}

	if isSecurityError(err) {
		return ExitSecurity
	}

	if isUsageError(err) {
		return ExitUsage
	}

	return ExitFailure
}

func isApprovalError(err error) bool {
	return matchesAny(err,
		coding.ErrInputRequired,
		coding.ErrTeamInteractionRequired,
		approval.ErrApprovalRequired,
		approval.ErrOutcomeUnknown,
		approval.ErrInvalidResolution,
		approval.ErrJournalCorrupt,
		approval.ErrDenied,
	)
}

func isSecurityError(err error) bool {
	return matchesAny(err,
		execution.ErrInvalidOperation,
		execution.ErrInvalidFingerprint,
		execution.ErrInvalidPolicy,
		execution.ErrUnauthorized,
		execution.ErrUnsupportedPlatform,
		execution.ErrSandboxUnavailable,
		workspace.ErrOutsideRoot,
		workspace.ErrSymlink,
		workspace.ErrChanged,
		workspace.ErrInsecurePermissions,
		workspace.ErrUnsupportedStoreSchema,
		workspace.ErrWorkspaceUnknown,
		workspace.ErrUnsupportedPlatform,
		session.ErrWorkspaceMismatch,
		session.ErrUnsupportedPlatform,
	)
}

func isUsageError(err error) bool {
	return matchesAny(err,
		ErrUsage,
		coding.ErrInvalidPrompt,
		coding.ErrRuntimeInvalid,
		config.ErrInvalid,
		config.ErrFile,
		config.ErrDecode,
		config.ErrMigration,
		credential.ErrNotFound,
		generation.ErrInvalid,
		model.ErrInvalid,
		modelcatalog.ErrInvalid,
		paths.ErrInvalid,
		session.ErrInvalid,
		workspace.ErrInvalid,
		workspace.ErrInvalidPath,
		workspace.ErrUnsupportedType,
	)
}

func matchesAny(err error, targets ...error) bool {
	for _, target := range targets {
		if errors.Is(err, target) {
			return true
		}
	}

	return false
}
