package execution

import (
	"context"
	"io"
	"os"
)

// Capabilities describes platform sandbox guarantees verified by Probe.
type Capabilities struct {
	Platform         string
	WorkspaceWrite   bool
	NetworkIsolation bool
	ProcessIsolation bool
}

type backend interface {
	probe(context.Context, probeRequest) (Capabilities, error)
	compile(context.Context, compileRequest) (launchSpec, []io.Closer, error)
}

type probeRequest struct {
	workspaceRoot string
	tempRoot      string
}

type compileRequest struct {
	operation     Operation
	workspaceRoot string
	privateDir    string
	environment   []string
}

type launchSpec struct {
	executable  string
	args        []string
	cwd         string
	environment []string
	stdin       []byte
	extraFiles  []*os.File
}

type unavailableBackend struct{}

func (unavailableBackend) probe(context.Context, probeRequest) (Capabilities, error) {
	return Capabilities{}, ErrUnsupportedPlatform
}

func (unavailableBackend) compile(context.Context, compileRequest) (launchSpec, []io.Closer, error) {
	return launchSpec{}, nil, ErrUnsupportedPlatform
}

func platformBackend() backend { return unavailableBackend{} }
