package cli

import (
	"slices"

	"github.com/rsbin1178/pips/internal/coding"
	"github.com/rsbin1178/pips/internal/coding/credential"
)

func newRuntimeOpenOptions(
	dependencies Dependencies,
	state commandState,
	sessionID string,
) (coding.OpenOptions, error) {
	if err := state.config.Config.ValidateRuntime(); err != nil {
		return coding.OpenOptions{}, err
	}

	credentials, err := credential.NewEnvironmentStore(dependencies.LookupEnv)
	if err != nil {
		return coding.OpenOptions{}, err
	}

	return coding.OpenOptions{
		Workspace:   state.workspace.workspace,
		Trusted:     state.workspace.isTrusted,
		Config:      state.config.Config,
		Paths:       dependencies.Paths,
		Session:     coding.SessionTarget{ID: sessionID},
		Credentials: credentials,
		Execution: coding.ExecutionOptions{
			Environment:      dependencies.LookupEnv,
			HooksEnvironment: slices.Clone(dependencies.Environment),
		},
	}, nil
}
