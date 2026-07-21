//nolint:wsl_v5 // Close follows reverse resource ownership order.
package coding

import (
	"context"
	"errors"
)

// Close cancels and waits for an active operation, terminally closes a paused
// interaction, then releases resources in reverse acquisition order. It is
// safe for concurrent and repeated calls.
func (r *Runtime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}

	for {
		r.mu.Lock()
		if r.closed {
			err := r.closeErr
			r.mu.Unlock()

			return err
		}

		if !r.closing {
			r.closing = true
		}

		if r.cleanupRunning {
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

		active := r.active
		if active != nil {
			active.cancel()
			done := active.done
			r.mu.Unlock()

			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		r.cleanupRunning = true
		current := r.interaction
		r.mu.Unlock()

		err := r.closeResources(ctx, current)

		r.mu.Lock()
		r.closeErr = err
		r.closed = true
		r.cleanupRunning = false
		close(r.closeDone)
		r.mu.Unlock()

		return err
	}
}

func (r *Runtime) closeResources(ctx context.Context, current *interaction) error {
	emitter := &eventEmitter{
		runtime: r,
		yield:   func(Event, error) bool { return true },
		alive:   true,
	}

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

	if err := emitter.emit("", "", EventSessionClosed, SessionClosed{Reason: SessionClosedNormally}); err != nil {
		errs = append(errs, err)
	}

	if err := r.extensions.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}

	if err := r.connections.Close(); err != nil {
		errs = append(errs, err)
	}

	if err := r.inspector.Close(); err != nil {
		errs = append(errs, err)
	}

	if err := r.handle.Close(); err != nil {
		errs = append(errs, err)
	}

	if err := r.tree.Close(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}
