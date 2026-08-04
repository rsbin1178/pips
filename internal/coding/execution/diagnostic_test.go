package execution

import (
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSandboxDiagnosticFromErrorClassifiesPermissionPath(t *testing.T) {
	t.Parallel()

	diagnostic, ok := SandboxDiagnosticFromError(
		&os.PathError{Op: "mkdir", Path: "/private/var/folders/tsx-501", Err: syscall.EPERM},
		"darwin-seatbelt",
		"plan-environment",
	)
	require.True(t, ok)
	assert.Equal(t, SandboxDiagnostic{
		Errno:     "EPERM",
		Operation: "mkdir",
		Path:      "/private/var/folders/tsx-501",
		Backend:   "darwin-seatbelt",
		Phase:     "plan-environment",
	}, diagnostic)
}

func TestSandboxDiagnosticFromErrorIgnoresNonPermissionErrors(t *testing.T) {
	t.Parallel()

	_, ok := SandboxDiagnosticFromError(
		&os.PathError{Op: "open", Path: "/tmp/file", Err: errors.New("other")},
		"backend",
		"phase",
	)
	assert.False(t, ok)
}

func TestSandboxDiagnosticFromOutputParsesNodeStyleDenial(t *testing.T) {
	t.Parallel()

	diagnostic, ok := SandboxDiagnosticFromOutput(
		"Error: EPERM: operation not permitted, mkdir '/Users/test/.pips/tmp/tsx-501'",
		"child-process",
		"stderr",
	)
	require.True(t, ok)
	assert.Equal(t, SandboxDiagnostic{
		Errno:     "EPERM",
		Operation: "mkdir",
		Path:      "/Users/test/.pips/tmp/tsx-501",
		Backend:   "child-process",
		Phase:     "stderr",
	}, diagnostic)
}
