package nicehttp

import (
	"context"
	"time"

	"github.com/neilotoole/fifomu"
)

type Limiter struct {
	mu fifomu.Mutex

	backoff *Backoff
}

func NewLimiter(backoff *Backoff) *Limiter {
	if backoff == nil {
		// A nil backoff means the default backoff is used
		backoff = DefaultBackoff.Clone()
	}
	return &Limiter{
		backoff: backoff,
	}
}

func (rl *Limiter) Clone() *Limiter {
	return &Limiter{
		backoff: rl.backoff.Clone(),
	}
}

// Wait ensures sequential access via a fifo lock, then waits for a period
// determined by a backoff strategy, while respecting the cancellation and
// deadline of the context. Requires call to Done() to unlock the fifo lock
func (rl *Limiter) Wait(ctx context.Context) error {
	err := rl.mu.LockContext(ctx)
	if err != nil {
		return err
	}

	wait := rl.backoff.UntilNext()
	if wait <= 0 {
		// Enough time has passed since last request - proceed
		return nil
	}

	// If the wait is longer than the remaining deadline of the ctx,
	// it better to simulate an DeadlineExceeded now.
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(rl.backoff.Next()) {
		rl.mu.Unlock()
		return context.DeadlineExceeded
	}

	select {
	case <-ctx.Done():
		// Context is cancelled or exceeded deadline
		rl.mu.Unlock()
		return ctx.Err()
	case <-time.After(wait):
		// Sleep until appropriate time
	}
	// Enough time has passed - proceed
	return nil
}

// Done records the outcome of an attempt and releases the host's turn, which
// Wait acquired. The outcome updates the backoff (see Backoff.Update), so
// it determines how long the next caller waits.
//
// Every Wait that returns without an error must be followed by exactly one
// Done, on every path, including errors and cancellation. The lock is handed
// to waiters in FIFO order, so a missing Done blocks all later requests to
// the host, and a second Done [panics / releases another caller's turn].
// Calling it with defer right after a successful Wait is the safest pattern.
//
// If Wait returns an error, it did not take the lock and Done must not be
// called.
func (rl *Limiter) Done(retry bool, waitOverride time.Duration) {
	rl.backoff.Update(retry, waitOverride)
	rl.mu.Unlock()
}
