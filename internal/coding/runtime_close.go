//nolint:wsl_v5 // Close follows reverse resource ownership order.
package coding

import (
	"context"
	"errors"
)

// Close starts the Runtime's unique cleanup owner and waits for it. If the
// caller deadline expires, cleanup continues in the background and a later
// Close waits for the same result. It is safe for concurrent and repeated
// calls.
func (r *Runtime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	if r.closed {
		err := r.closeErr
		r.mu.Unlock()

		return err
	}
	if !r.cleanupRunning {
		r.closing = true
		r.cleanupRunning = true
		cleanupCtx := context.WithoutCancel(ctx)
		go r.runCloseCleanup(cleanupCtx)
	}
	done := r.closeDone
	r.mu.Unlock()

	select {
	case <-done:
		r.mu.Lock()
		err := r.closeErr
		r.mu.Unlock()

		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) runCloseCleanup(ctx context.Context) {
	for {
		r.mu.Lock()
		active := r.active
		if active == nil {
			current := r.interaction
			r.mu.Unlock()

			err := r.closeResources(ctx, current)
			r.publisher.close()

			r.mu.Lock()
			r.closeErr = err
			r.closed = true
			r.cleanupRunning = false
			close(r.closeDone)
			r.mu.Unlock()

			return
		}
		active.cancel()
		done := active.done
		r.mu.Unlock()
		<-done
	}
}

func (r *Runtime) closeResources(ctx context.Context, current *interaction) error {
	emitter := newEventEmitter(ctx, r, func(Event, error) bool { return true }, true)

	errs := make([]error, 0, 8)
	if current != nil {
		if err := r.finishInteraction(
			context.WithoutCancel(ctx),
			current,
			InteractionCanceled,
			emitter,
		); err != nil {
			errs = append(errs, err)
		}
	}

	if err := emitter.emit("", "", EventStatusChanged, StatusChanged{Phase: PhaseClosing}); err != nil {
		errs = append(errs, err)
	}
	if err := r.stopNotificationCoordinator(ctx); err != nil {
		errs = append(errs, err)
	}
	r.mu.Lock()
	coordinator := r.team
	r.mu.Unlock()
	if coordinator != nil {
		r.teamGuard.beginClose(coordinator.id)
		if err := coordinator.close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	// Children stop while the parent Session is still open so their terminal
	// lifecycle and durable completion notification can be committed safely.
	if err := r.subagents.Close(ctx); err != nil {
		errs = append(errs, err)
	}
	r.runSessionEnd(ctx, SessionClosedNormally, emitter)

	if err := emitter.emit("", "", EventSessionClosed, SessionClosed{Reason: SessionClosedNormally}); err != nil {
		errs = append(errs, err)
	}

	resources := &cleanupStack{}
	// Scratch is the outermost runtime resource and must be removed only after
	// all child transports, inspectors, and sessions have stopped using it.
	if r.tempRoot != nil {
		resources.add(func(context.Context) error { return r.tempRoot.Close() })
	}
	resources.add(func(context.Context) error { return r.tree.Close() })
	resources.add(func(context.Context) error { return r.handle.Close() })
	resources.add(func(context.Context) error { return r.inspector.Close() })
	resources.add(r.extensions.Shutdown)
	resources.add(r.integration.retire)
	if err := resources.close(ctx); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}
