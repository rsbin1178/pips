package execution

import "errors"

var (
	// ErrInvalidOperation means an operation request cannot be represented safely.
	ErrInvalidOperation = errors.New("coding execution: invalid operation")
	// ErrInvalidFingerprint means a persisted operation fingerprint is malformed.
	ErrInvalidFingerprint = errors.New("coding execution: invalid fingerprint")
	// ErrInvalidPolicy means a policy configuration cannot establish a safe ceiling.
	ErrInvalidPolicy = errors.New("coding execution: invalid policy")
	// ErrUnauthorized means an operation has no valid authorization under policy.
	ErrUnauthorized = errors.New("coding execution: unauthorized")
	// ErrUnsupportedPlatform means the host cannot provide a required execution boundary.
	ErrUnsupportedPlatform = errors.New("coding execution: unsupported platform")
	// ErrSandboxUnavailable means the configured platform boundary failed its capability check.
	ErrSandboxUnavailable = errors.New("coding execution: sandbox unavailable")
	// ErrPlanUsed means a plan was already run or closed.
	ErrPlanUsed = errors.New("coding execution: plan already consumed")
	// ErrStart means the operating system did not start a validated plan.
	ErrStart = errors.New("coding execution: process start failed")
	// ErrOutputLimit means observed process output exceeded the operation hard limit.
	ErrOutputLimit = errors.New("coding execution: output limit exceeded")
)
