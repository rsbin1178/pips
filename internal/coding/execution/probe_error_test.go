package execution

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProbeErrorContract(t *testing.T) {
	t.Parallel()

	cause := errors.New("private probe path and raw stderr")
	typed := newProbeError(
		ProbeFailureRuntimeTooOld,
		"bubblewrap",
		"0.4.0",
		"0.8.0",
		cause,
	)
	wrapped := fmt.Errorf("%w: %w", ErrSandboxUnavailable, typed)

	require.ErrorIs(t, wrapped, ErrSandboxUnavailable)
	require.ErrorIs(t, wrapped, cause)

	probeErr, ok := errors.AsType[*ProbeError](wrapped)
	require.True(t, ok)
	assert.Equal(t, ProbeFailureRuntimeTooOld, probeErr.Failure())
	assert.Equal(t, "runtime_too_old", probeErr.Code())
	assert.Equal(t, "bubblewrap", probeErr.Runtime())
	assert.Equal(t, "0.4.0", probeErr.Version())
	assert.Equal(t, "0.8.0", probeErr.MinimumVersion())
	assert.Contains(t, wrapped.Error(), "install Bubblewrap 0.8.0 or newer")
	assert.NotContains(t, wrapped.Error(), cause.Error())
}

func TestProbeErrorFailureCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		failure ProbeFailure
		code    string
	}{
		{name: "unknown", failure: ProbeFailureUnknown, code: "probe_failed"},
		{name: "launcher", failure: ProbeFailureLauncher, code: "launcher_unavailable"},
		{name: "version", failure: ProbeFailureRuntimeVersion, code: "runtime_version_invalid"},
		{name: "old runtime", failure: ProbeFailureRuntimeTooOld, code: "runtime_too_old"},
		{name: "isolation", failure: ProbeFailureIsolation, code: "isolation_unavailable"},
		{name: "deny", failure: ProbeFailureDeny, code: "deny_probe_failed"},
		{name: "allow", failure: ProbeFailureAllow, code: "allow_probe_failed"},
		{name: "invalid", failure: ProbeFailure(255), code: "probe_failed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			probeErr := newProbeError(test.failure, "bubblewrap", "0.8.0", "0.8.0", assert.AnError)
			assert.Equal(t, test.code, probeErr.Code())
			assert.NotContains(t, probeErr.Error(), assert.AnError.Error())

			if test.code == "probe_failed" {
				assert.Equal(t, ProbeFailureUnknown, probeErr.Failure())
				assert.Equal(t, "sandbox probe failed", probeErr.Error())
			} else {
				assert.Contains(t, probeErr.Error(), test.code)
			}
		})
	}

	var nilError *ProbeError
	assert.Equal(t, ProbeFailureUnknown, nilError.Failure())
	assert.Equal(t, "probe_failed", nilError.Code())
	assert.Equal(t, "sandbox probe failed", nilError.Error())
}
