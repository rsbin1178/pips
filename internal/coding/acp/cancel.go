package acp

import "context"

// deliverCancelOnDone arranges for deliver to be called once when ctx is done and
// returns the function that disarms it. The returned function guarantees the
// delivery has happened by the time it returns whenever ctx is already done:
// context.AfterFunc runs its callback on its own goroutine, so a prompt that
// returns on its own cancellation can otherwise prevent the delivery and leave the
// controller running with no cancel.
func deliverCancelOnDone(ctx context.Context, deliver func()) func() {
	delivered := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(delivered)

		deliver()
	})

	return func() {
		switch {
		case !stop():
			// The callback is on its goroutine: let it finish before the caller moves on.
			<-delivered
		case ctx.Err() != nil:
			// The callback was prevented and the context is done anyway, which is what
			// happens when the prompt returns on its own cancellation first.
			deliver()
		}
	}
}
