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
		r.publisher.close()

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
	// Children stop while the parent Session is still open so their terminal
	// lifecycle and durable completion notification can be committed safely.
	if err := r.subagents.Close(ctx); err != nil {
		errs = append(errs, err)
	}

	if err := emitter.emit("", "", EventSessionClosed, SessionClosed{Reason: SessionClosedNormally}); err != nil {
		errs = append(errs, err)
	}

	resources := &cleanupStack{}
	resources.add(func(context.Context) error { return r.tree.Close() })
	resources.add(func(context.Context) error { return r.handle.Close() })
	resources.add(func(context.Context) error { return r.inspector.Close() })
	resources.add(func(context.Context) error { return r.connections.Close() })
	resources.add(r.extensions.Shutdown)
	if err := resources.close(ctx); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}
