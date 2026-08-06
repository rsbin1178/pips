//go:build !darwin && !linux && !windows

package pluginsupervisor

import (
	"context"
	"errors"
	"os/exec"
)

type unsupportedProcessController struct{}

func newProcessController() processController                   { return &unsupportedProcessController{} }
func (*unsupportedProcessController) configure(*exec.Cmd) error { return nil }
func (*unsupportedProcessController) attach(*exec.Cmd) error    { return nil }
func (*unsupportedProcessController) close() error              { return nil }
func (*unsupportedProcessController) terminate(context.Context, *exec.Cmd, <-chan struct{}) (processTermination, error) {
	return processTermination{}, errors.Join(ErrCleanupUncertain, errors.New("pluginsupervisor: process-tree cleanup unsupported on this platform"))
}
