package sshclient

import (
	"encoding/base64"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rsbin/pips/internal/coding/imagebridge"
)

const (
	maxDestinationBytes = 255
	maxWorkspaceBytes   = 4096
	maxVersionBytes     = 128
)

// Request is one validated remote Pips invocation.
type Request struct {
	Destination string
	Workspace   string
	Version     string
	Nonce       string
}

// ValidateDestination accepts a bounded ASCII SSH destination/config alias
// but no option-shaped or shell-bearing text.
func ValidateDestination(value string) error {
	if value == "" || len(value) > maxDestinationBytes || !utf8.ValidString(value) ||
		value[0] == '-' || strings.Count(value, "@") > 1 {
		return fmt.Errorf("%w: invalid SSH destination", ErrInvalid)
	}

	for _, character := range value {
		if !validDestinationCharacter(character) {
			return fmt.Errorf("%w: invalid SSH destination", ErrInvalid)
		}
	}

	if strings.HasPrefix(value, "@") || strings.HasSuffix(value, "@") ||
		strings.Count(value, "[") != strings.Count(value, "]") ||
		strings.Count(value, "[") > 1 {
		return fmt.Errorf("%w: invalid SSH destination", ErrInvalid)
	}

	return nil
}

// ValidateWorkspace accepts only a canonical dot or absolute POSIX remote path.
func ValidateWorkspace(value string) error {
	if value == "" || len(value) > maxWorkspaceBytes || !utf8.ValidString(value) ||
		value != "." && !strings.HasPrefix(value, "/") || value != path.Clean(value) ||
		strings.Contains(value, "\\") {
		return fmt.Errorf("%w: invalid remote Workspace", ErrInvalid)
	}

	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return fmt.Errorf("%w: invalid remote Workspace", ErrInvalid)
		}
	}

	return nil
}

// EncodeWorkspace returns the canonical base64url token used in remote argv.
func EncodeWorkspace(value string) (string, error) {
	if err := ValidateWorkspace(value); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString([]byte(value)), nil
}

// DecodeWorkspace validates a canonical base64url remote Workspace token.
func DecodeWorkspace(value string) (string, error) {
	decoded, err := decodeRemoteValue(value, maxWorkspaceBytes)
	if err != nil {
		return "", err
	}

	if err := ValidateWorkspace(decoded); err != nil {
		return "", err
	}

	return decoded, nil
}

// EncodeVersion returns the canonical base64url token used in remote argv.
func EncodeVersion(value string) (string, error) {
	if err := validateVersion(value); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString([]byte(value)), nil
}

// DecodeVersion validates a canonical base64url build-version token.
func DecodeVersion(value string) (string, error) {
	decoded, err := decodeRemoteValue(value, maxVersionBytes)
	if err != nil {
		return "", err
	}

	if err := validateVersion(decoded); err != nil {
		return "", err
	}

	return decoded, nil
}

// ValidateRequest checks all values before any filesystem or process access.
func ValidateRequest(request Request) error {
	if err := ValidateDestination(request.Destination); err != nil {
		return err
	}

	if err := ValidateWorkspace(request.Workspace); err != nil {
		return err
	}

	if err := validateVersion(request.Version); err != nil {
		return err
	}

	if err := imagebridge.ValidateNonce(request.Nonce); err != nil {
		return fmt.Errorf("%w: invalid bridge nonce", ErrInvalid)
	}

	return nil
}

func validateVersion(value string) error {
	if value == "" || len(value) > maxVersionBytes || !utf8.ValidString(value) {
		return fmt.Errorf("%w: invalid Pips version", ErrInvalid)
	}

	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return fmt.Errorf("%w: invalid Pips version", ErrInvalid)
		}
	}

	return nil
}

func decodeRemoteValue(value string, maximum int) (string, error) {
	if value == "" || len(value) > base64.RawURLEncoding.EncodedLen(maximum) {
		return "", fmt.Errorf("%w: invalid remote token", ErrInvalid)
	}

	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) > maximum || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return "", fmt.Errorf("%w: invalid remote token", ErrInvalid)
	}

	return string(decoded), nil
}

func validDestinationCharacter(character rune) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' || strings.ContainsRune("._-@%+:[]", character)
}
