package imagebridge

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

const (
	nonceBytes  = 32
	nonceLength = 43
)

// NewNonce returns a fresh 256-bit base64url bridge nonce.
func NewNonce() (string, error) {
	return newNonce(rand.Reader)
}

func newNonce(random io.Reader) (string, error) {
	if random == nil {
		return "", fmt.Errorf("%w: missing randomness", ErrProtocol)
	}

	value := make([]byte, nonceBytes)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", fmt.Errorf("coding image bridge: generate nonce: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(value), nil
}

// ValidateNonce accepts only the canonical 256-bit base64url representation.
func ValidateNonce(value string) error {
	if len(value) != nonceLength {
		return fmt.Errorf("%w: invalid nonce", ErrProtocol)
	}

	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != nonceBytes ||
		base64.RawURLEncoding.EncodeToString(decoded) != value {
		return fmt.Errorf("%w: invalid nonce", ErrProtocol)
	}

	return nil
}
