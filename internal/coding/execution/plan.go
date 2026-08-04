package execution

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	planReady uint32 = iota
	planUsed
	planClosed
)

// Plan owns one validated, single-use command launch and its private resources.
type Plan struct {
	state      atomic.Uint32
	closeOnce  sync.Once
	closeErr   error
	tempRoot   fileObject
	privateDir string
	private    fileObject
	resources  []io.Closer
	operation  Operation
	launch     launchSpec
	launcher   fileObject
}

// Close releases plan-owned files and its exact private directory. It is idempotent.
func (p *Plan) Close() error {
	if p == nil {
		return nil
	}

	p.closeOnce.Do(func() {
		p.state.CompareAndSwap(planReady, planClosed)
		p.closeErr = p.closeResources()
	})

	return p.closeErr
}

func (p *Plan) consume() error {
	if p == nil || !p.state.CompareAndSwap(planReady, planUsed) {
		return ErrPlanUsed
	}

	return nil
}

func (p *Plan) closeResources() error {
	errorsToJoin := make([]error, 0, len(p.resources)+1)
	for _, v := range slices.Backward(p.resources) {
		if err := v.Close(); err != nil {
			errorsToJoin = append(errorsToJoin, fmt.Errorf("coding execution: close plan resource: %w", err))
		}
	}

	if err := removePrivatePlanDir(p.tempRoot, p.private); err != nil {
		errorsToJoin = append(errorsToJoin, err)
	}

	return errors.Join(errorsToJoin...)
}

func removePrivatePlanDir(tempRoot, privateDir fileObject) error {
	if tempRoot.path == "" || privateDir.path == "" || filepath.Dir(privateDir.path) != tempRoot.path ||
		!strings.HasPrefix(filepath.Base(privateDir.path), "plan-") {
		return errors.New("coding execution: refuse unsafe private directory cleanup")
	}

	if err := revalidatePrivateDirectory(tempRoot); err != nil {
		return fmt.Errorf("coding execution: refuse cleanup after temp root change: %w", err)
	}

	if err := removeOwnedChild(tempRoot, privateDir); err != nil {
		return fmt.Errorf("coding execution: remove private directory: %w", err)
	}

	return nil
}
