package execution

import (
	"errors"
	"fmt"
)

// ProbeFailure identifies the capability-probe stage that failed.
type ProbeFailure uint8

const (
	// ProbeFailureUnknown represents an unclassified fail-closed probe error.
	ProbeFailureUnknown ProbeFailure = iota
	// ProbeFailureLauncher means the fixed runtime launcher is unavailable.
	ProbeFailureLauncher
	// ProbeFailureRuntimeVersion means runtime version metadata is invalid.
	ProbeFailureRuntimeVersion
	// ProbeFailureRuntimeTooOld means the runtime is below the supported baseline.
	ProbeFailureRuntimeTooOld
	// ProbeFailureIsolation means the core namespace or mount preflight failed.
	ProbeFailureIsolation
	// ProbeFailureDeny means the restrictive full capability probe failed.
	ProbeFailureDeny
	// ProbeFailureAllow means the expanded-network full capability probe failed.
	ProbeFailureAllow
)

const unknownProbeFailureCode = "probe_failed"

// ProbeError reports a stable sandbox capability failure without exposing
// untrusted process output or probe paths in its message.
type ProbeError struct {
	failure        ProbeFailure
	runtime        string
	version        string
	minimumVersion string
	cause          error
}

func newProbeError(
	failure ProbeFailure,
	runtime string,
	version string,
	minimumVersion string,
	cause error,
) *ProbeError {
	if probeFailureCode(failure) == unknownProbeFailureCode {
		failure = ProbeFailureUnknown
	}

	return &ProbeError{
		failure:        failure,
		runtime:        runtime,
		version:        version,
		minimumVersion: minimumVersion,
		cause:          cause,
	}
}

// Error returns a stable, low-sensitivity diagnostic.
func (e *ProbeError) Error() string {
	if e == nil {
		return "sandbox probe failed"
	}

	switch e.failure {
	case ProbeFailureLauncher:
		return fmt.Sprintf(
			"sandbox probe %s: install Bubblewrap %s or newer at /usr/bin/bwrap or /bin/bwrap",
			e.Code(),
			e.minimumVersion,
		)
	case ProbeFailureRuntimeVersion:
		return fmt.Sprintf(
			"sandbox probe %s: Bubblewrap version could not be verified; install Bubblewrap %s or newer",
			e.Code(),
			e.minimumVersion,
		)
	case ProbeFailureRuntimeTooOld:
		return fmt.Sprintf(
			"sandbox probe %s: Bubblewrap %s is below required %s; install Bubblewrap %s or newer",
			e.Code(),
			e.version,
			e.minimumVersion,
			e.minimumVersion,
		)
	case ProbeFailureIsolation:
		return "sandbox probe isolation_unavailable: Bubblewrap isolation could not be established; " +
			"check outer-container user, mount, PID, network namespace and /proc mount permissions"
	case ProbeFailureDeny:
		return "sandbox probe deny_probe_failed: Bubblewrap deny capability verification failed; " +
			"check host user-namespace and security policy"
	case ProbeFailureAllow:
		return "sandbox probe allow_probe_failed: Bubblewrap allow capability verification failed; " +
			"check host user-namespace and security policy"
	default:
		return "sandbox probe failed"
	}
}

// Unwrap retains the underlying local diagnostic for errors.Is and errors.As.
func (e *ProbeError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.cause
}

// Failure returns the failed probe stage.
func (e *ProbeError) Failure() ProbeFailure {
	if e == nil {
		return ProbeFailureUnknown
	}

	return e.failure
}

// Code returns a stable machine-readable failure code.
func (e *ProbeError) Code() string {
	if e == nil {
		return unknownProbeFailureCode
	}

	return probeFailureCode(e.failure)
}

// Runtime returns the canonical sandbox runtime name, when known.
func (e *ProbeError) Runtime() string {
	if e == nil {
		return ""
	}

	return e.runtime
}

// Version returns the canonical detected runtime version, when known.
func (e *ProbeError) Version() string {
	if e == nil {
		return ""
	}

	return e.version
}

// MinimumVersion returns the minimum supported runtime version, when relevant.
func (e *ProbeError) MinimumVersion() string {
	if e == nil {
		return ""
	}

	return e.minimumVersion
}

func probeFailureCode(failure ProbeFailure) string {
	switch failure {
	case ProbeFailureLauncher:
		return "launcher_unavailable"
	case ProbeFailureRuntimeVersion:
		return "runtime_version_invalid"
	case ProbeFailureRuntimeTooOld:
		return "runtime_too_old"
	case ProbeFailureIsolation:
		return "isolation_unavailable"
	case ProbeFailureDeny:
		return "deny_probe_failed"
	case ProbeFailureAllow:
		return "allow_probe_failed"
	default:
		return unknownProbeFailureCode
	}
}

func joinProbeErrorCause(probeErr, cause error) error {
	if cause == nil {
		return probeErr
	}

	typed, ok := errors.AsType[*ProbeError](probeErr)
	if !ok {
		return newProbeError(ProbeFailureUnknown, "", "", "", errors.Join(probeErr, cause))
	}

	return newProbeError(
		typed.failure,
		typed.runtime,
		typed.version,
		typed.minimumVersion,
		errors.Join(typed.cause, cause),
	)
}
