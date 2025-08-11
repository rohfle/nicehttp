package nicehttp

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/neilotoole/fifomu"
)

type WaitAdjuster func(last time.Time, wait time.Duration, fail bool) time.Duration

func DefaultWaitAdjuster(last time.Time, wait time.Duration, fail bool) time.Duration {
	const FailWaitMultiplier = 1.5
	const SuccessWaitMultiplier = 0.5
	jitter := rand.Float64()*0.3 + 0.85 // 85%–115%
	multiplier := SuccessWaitMultiplier
	if fail {
		multiplier = FailWaitMultiplier
	}

	return time.Duration(multiplier * jitter * float64(wait))
}

type Limiter struct {
	last    time.Time
	wait    time.Duration
	minWait time.Duration
	maxWait time.Duration
	mu      fifomu.Mutex

	AdjustWait WaitAdjuster
}

func NewLimiter(minWait time.Duration, maxWait time.Duration) *Limiter {
	return &Limiter{
		last:       time.Now().Add(-1 * minWait),
		minWait:    minWait,
		maxWait:    maxWait,
		wait:       minWait,
		AdjustWait: DefaultWaitAdjuster,
	}
}

func (rl *Limiter) Clone() *Limiter {
	limiter := NewLimiter(rl.minWait, rl.maxWait)
	limiter.AdjustWait = rl.AdjustWait
	return limiter
}

func (rl *Limiter) Wait(ctx context.Context) error {
	err := rl.mu.LockContext(ctx)
	if err != nil {
		return err
	}
	// Acquire lock for next request
	adjustedWait := time.Until(rl.last.Add(rl.wait))
	if adjustedWait < 0 {
		// Enough time has passed since last request - proceed
		return nil
	}

	select {
	case <-ctx.Done():
		// Context is cancelled or exceeded deadline
		rl.mu.Unlock()
		return ctx.Err()
	case <-time.After(adjustedWait):
		// Sleep until appropriate time
	}
	// Enough time has passed - proceed
	return nil
}

func (rl *Limiter) Done(retry bool, waitGiven time.Duration) error {
	now := time.Now()
	wait := waitGiven
	if wait == 0 {
		wait = rl.AdjustWait(rl.last, rl.wait, retry)
		wait = min(wait, rl.maxWait)
	}
	wait = max(rl.minWait, wait)

	rl.last = now
	rl.wait = wait
	rl.mu.Unlock()
	return nil
}
