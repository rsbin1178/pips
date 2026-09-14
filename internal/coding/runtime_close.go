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

// isClosing reports whether Close owns the runtime.
func (r *Runtime) isClosing() bool {
	if r == nil {
		return true
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.closing || r.closed
}

// hasParkedDecision reports whether a user-facing decision is still displayed
// when Close starts. Such an interaction is preserved for the next open.
func (r *Runtime) hasParkedDecision() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.state.Question.Required != nil || r.state.PlanReview.Required != nil
}

// retireParkedInteraction releases the runtime-owned resources of an
// interaction that stays parked for a resumed run. Unlike finishInteraction it
// writes no terminal journal entry and emits no terminal events, because the
// pending decision — and the interaction that displays it — must remain
// durable.
func (r *Runtime) retireParkedInteraction(ctx context.Context, current *interaction) error {
	if current == nil {
		return nil
	}

	for _, runID := range current.runIDs {
		current.search.Forget(runID)
	}
	r.clearHookToolContext()
	r.pending.clear()
	r.resolver.set(nil)

	r.mu.Lock()
	if r.interaction == current {
		r.interaction = nil
	}
	r.recovery.PendingID = ""
	r.mu.Unlock()

	return current.integration.release(ctx)
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
	r.mu.Lock()
	r.agentDrafts = nil
	r.mu.Unlock()

	errs := make([]error, 0, 8)
	// A parked question or Plan review outlives this process: its interaction
	// journal stays open so the next open re-parks the same decision instead of
	// failing the Session on unresolved pending calls.
	parked := r.hasParkedDecision()
	if current != nil {
		var err error
		if parked {
			err = r.retireParkedInteraction(context.WithoutCancel(ctx), current)
		} else {
			err = r.finishInteraction(
				context.WithoutCancel(ctx),
				current,
				InteractionCanceled,
				emitter,
			)
		}
		if err != nil {
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

	// The reducer keeps the Session open while a parked decision is displayed
	// for the resumed run.
	if !parked {
		if err := emitter.emit("", "", EventSessionClosed, SessionClosed{Reason: SessionClosedNormally}); err != nil {
			errs = append(errs, err)
		}
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
