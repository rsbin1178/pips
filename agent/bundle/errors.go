package bundle

import "errors"

var (
	// ErrInvalid reports malformed configuration, manifests, filters, or paths.
	ErrInvalid = errors.New("bundle: invalid")
	// ErrUnsupportedSchema reports a manifest schema this version cannot load.
	ErrUnsupportedSchema = errors.New("bundle: unsupported schema")
	// ErrUntrusted reports a bundle rejected by the selected trust policy.
	ErrUntrusted = errors.New("bundle: untrusted")
	// ErrLimitExceeded reports a manifest, resource, or bundle over its bound.
	ErrLimitExceeded = errors.New("bundle: limit exceeded")
	// ErrDuplicate reports duplicate bundle or component identities.
	ErrDuplicate = errors.New("bundle: duplicate")
	// ErrClosed reports use of a disk Loader after Close.
	ErrClosed = errors.New("bundle: loader closed")
)
