package team

import "fmt"

const (
	defaultMaxMembers             = 32
	defaultMaxTasks               = 1_024
	defaultMaxDependenciesPerTask = 64
	defaultMaxMessages            = 4_096
	defaultMaxActiveTasks         = 32
	defaultMaxAttemptsPerTask     = 16
	defaultMaxArtifactsPerResult  = 64
	defaultMaxJSONBytes           = 256 << 10

	hardMaxMembers             = 1_024
	hardMaxTasks               = 100_000
	hardMaxDependenciesPerTask = 1_024
	hardMaxMessages            = 100_000
	hardMaxActiveTasks         = 1_024
	hardMaxAttemptsPerTask     = 1_000
	hardMaxArtifactsPerResult  = 1_024
	hardMaxJSONBytes           = 4 << 20
)

func resolveLimits(limits Limits) (Limits, error) {
	values := []*int{
		&limits.MaxMembers,
		&limits.MaxTasks,
		&limits.MaxDependenciesPerTask,
		&limits.MaxMessages,
		&limits.MaxActiveTasks,
		&limits.MaxAttemptsPerTask,
		&limits.MaxArtifactsPerResult,
		&limits.MaxJSONBytes,
	}
	defaults := []int{
		defaultMaxMembers,
		defaultMaxTasks,
		defaultMaxDependenciesPerTask,
		defaultMaxMessages,
		defaultMaxActiveTasks,
		defaultMaxAttemptsPerTask,
		defaultMaxArtifactsPerResult,
		defaultMaxJSONBytes,
	}
	hard := []int{
		hardMaxMembers,
		hardMaxTasks,
		hardMaxDependenciesPerTask,
		hardMaxMessages,
		hardMaxActiveTasks,
		hardMaxAttemptsPerTask,
		hardMaxArtifactsPerResult,
		hardMaxJSONBytes,
	}

	for i, value := range values {
		if *value == 0 {
			*value = defaults[i]
		}

		if *value < 1 || *value > hard[i] {
			return Limits{}, fmt.Errorf("%w: Team limit must be between 1 and %d", ErrInvalid, hard[i])
		}
	}

	if limits.MaxActiveTasks > limits.MaxMembers {
		return Limits{}, fmt.Errorf("%w: active tasks cannot exceed members", ErrInvalid)
	}

	return limits, nil
}
