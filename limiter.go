package nicehttp

import (
	"context"
	"time"

	"github.com/neilotoole/fifomu"
)

type Limiter struct {
	mu fifomu.Mutex

	backoff Backoff
}

func NewLimiter(backoff Backoff) *Limiter {
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

	wait := rl.backoff.Next()
	if wait <= 0 {
		// Enough time has passed since last request - proceed
		return nil
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

func (rl *Limiter) Done(retry bool, waitOverride time.Duration) error {
	rl.backoff.Update(retry, waitOverride)
	rl.mu.Unlock()
	return nil
}
