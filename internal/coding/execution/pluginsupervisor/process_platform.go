package pluginsupervisor

import (
	"context"
	"os/exec"
)

type processTermination struct {
	forced       bool
	treeQuiesced bool
}

type processController interface {
	configure(*exec.Cmd) error
	attach(*exec.Cmd) error
	terminate(context.Context, *exec.Cmd, <-chan struct{}) (processTermination, error)
	close() error
}
