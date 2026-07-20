package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/rsbin/pips/ai"
)

// ErrNotFound means no usable credential is available for a provider.
var ErrNotFound = errors.New("coding credential: credential not found")

// Store returns credentials at the point a provider model is constructed.
type Store interface {
	Get(context.Context, ai.Provider) (Credential, error)
}

// Credential contains one provider API key. Its formatting is always
// redacted; callers must explicitly request the key at the provider boundary.
type Credential struct {
	apiKey string
}

// APIKey returns the credential value for immediate provider construction.
func (c Credential) APIKey() string { return c.apiKey }

// Format redacts the credential for every fmt verb, including %#v.
func (Credential) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "<redacted>")
}

// MarshalJSON prevents reflective serialization from exposing internal
// credential representation.
func (Credential) MarshalJSON() ([]byte, error) {
	return json.Marshal("<redacted>")
}

// MissingError identifies the provider and non-secret environment variable
// that was checked.
type MissingError struct {
	Provider ai.Provider
	Variable string
}

// Error implements error.
func (e *MissingError) Error() string {
	return fmt.Sprintf("coding credential: %s: set %s", e.Provider, e.Variable)
}

// Unwrap makes MissingError match ErrNotFound.
func (*MissingError) Unwrap() error { return ErrNotFound }
