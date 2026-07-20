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
)
