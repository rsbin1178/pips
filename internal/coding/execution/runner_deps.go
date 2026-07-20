package execution

import (
	"os"
	"os/exec"
	"time"
)

type runnerDependencies struct {
	pipe    func() (*os.File, *os.File, error)
	command func(string, ...string) *exec.Cmd
	now     func() time.Time
}

func systemRunnerDependencies() runnerDependencies {
	return runnerDependencies{pipe: os.Pipe, command: exec.Command, now: time.Now}
}
