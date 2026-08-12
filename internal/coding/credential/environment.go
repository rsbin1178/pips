package credential

import (
	"context"
	"errors"
	"strings"

	"github.com/rsbin1178/pips/ai"
)

// APIKeyEnv is the provider-neutral API key variable used by the application.
const APIKeyEnv = "API_KEY"

// LookupEnv is the environment lookup contract used by EnvironmentStore.
type LookupEnv func(string) (string, bool)

// EnvironmentStore reads provider API keys from an injected environment.
type EnvironmentStore struct {
	lookup LookupEnv
}

// NewEnvironmentStore returns an environment-backed Store.
func NewEnvironmentStore(lookup LookupEnv) (*EnvironmentStore, error) {
	if lookup == nil {
		return nil, errors.New("coding credential: nil environment lookup")
	}

	return &EnvironmentStore{lookup: lookup}, nil
}

// Get implements Store.
func (s *EnvironmentStore) Get(ctx context.Context, provider ai.Provider) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}

	if s == nil || s.lookup == nil {
		return Credential{}, errors.New("coding credential: nil environment store")
	}

	if provider == "" {
		return Credential{}, errors.New("coding credential: empty provider")
	}

	value, ok := s.lookup(APIKeyEnv)
	if ok && strings.TrimSpace(value) != "" {
		return Credential{apiKey: value}, nil
	}

	return Credential{}, &MissingError{Provider: provider, Variable: APIKeyEnv}
}
