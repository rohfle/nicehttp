package nicehttp

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newSeededLimiter returns a limiter whose next wait is about wait from now,
// so a Wait has to sleep unless the caller's deadline is checked first.
func newSeededLimiter(wait time.Duration) *Limiter {
	limiter := NewLimiter(NewBackoff(wait, wait, DefaultBackoffCoefficients))
	limiter.backoff.last = time.Now()
	return limiter
}

// TestLimiterWaitAlreadyCancelled checks that a cancelled context returns its
// error and leaves the turn free.
func TestLimiterWaitAlreadyCancelled(t *testing.T) {
	limiter := newSeededLimiter(time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := limiter.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// The failed Wait must not hold the turn: clear the wait and acquire.
	limiter.backoff.wait = 0
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatalf("expected to acquire the limiter after a failed Wait, got %v", err)
	}
	limiter.Done(false, 0)
}

// TestLimiterWaitCancelledWhileQueued checks that a waiter whose context ends
// before the turn reaches it returns the context error and never acquires.
func TestLimiterWaitCancelledWhileQueued(t *testing.T) {
	limiter := newSeededLimiter(time.Hour)

	// Hold the turn directly so Wait has to queue instead of acquiring.
	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := limiter.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

// TestLimiterWaitCancelledWhileSleeping checks that cancellation during the
// backoff sleep releases the turn.
func TestLimiterWaitCancelledWhileSleeping(t *testing.T) {
	limiter := newSeededLimiter(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if err := limiter.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected cancellation to stop the wait quickly, took %s", elapsed)
	}

	limiter.backoff.wait = 0
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatalf("expected to acquire the limiter after cancellation, got %v", err)
	}
	limiter.Done(false, 0)
}

// TestLimiterWaitDeadlineTooClose checks that a wait that cannot finish before
// the deadline is refused up front without sleeping, and releases the turn.
func TestLimiterWaitDeadlineTooClose(t *testing.T) {
	limiter := newSeededLimiter(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := limiter.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("expected the wait to be refused immediately, took %s", elapsed)
	}

	limiter.backoff.wait = 0
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatalf("expected to acquire the limiter after a refused wait, got %v", err)
	}
	limiter.Done(false, 0)
}
