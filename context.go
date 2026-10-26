package nicehttp

import (
	"context"
	"time"
)

// Define key types to avoid collisions
type maxAttemptsKey struct{}
type attemptTimeoutKey struct{}
type retryNonIdempotentKey struct{}

// SetRetryNonIdempotentInContext returns a context that allows POST and PATCH
// requests to be retried after a transport error or a 504 Gateway Timeout.
// Without it, only methods that are safe to replay, and requests carrying an
// Idempotency-Key, are retried.
func SetRetryNonIdempotentInContext(parent context.Context) context.Context {
	return context.WithValue(parent, retryNonIdempotentKey{}, true)
}

// GetRetryNonIdempotentFromContext reports whether POST and PATCH requests may
// be retried, as set by SetRetryNonIdempotentInContext.
func GetRetryNonIdempotentFromContext(ctx context.Context) bool {
	val, _ := ctx.Value(retryNonIdempotentKey{}).(bool)
	return val
}

// SetAttemptTimeoutInContext returns a new context with the given timeout value.
// A negative value means DefaultAttemptTimeout applies, and NoTimeout disables
// the per attempt deadline.
func SetAttemptTimeoutInContext(parent context.Context, timeout time.Duration) context.Context {
	return context.WithValue(parent, attemptTimeoutKey{}, timeout)
}

// GetAttemptTimeoutFromContext retrieves the timeout value from the context.
func GetAttemptTimeoutFromContext(ctx context.Context) (time.Duration, bool) {
	val, ok := ctx.Value(attemptTimeoutKey{}).(time.Duration)
	return val, ok
}

// SetMaxAttemptsInContext returns a new context with the given max attempts
// value. The value is clamped to 1 through 100 when a request is made,
// matching the per host config.
func SetMaxAttemptsInContext(parent context.Context, attempts int) context.Context {
	return context.WithValue(parent, maxAttemptsKey{}, attempts)
}

// GetMaxAttemptsFromContext retrieves the max attempts value from the context.
func GetMaxAttemptsFromContext(ctx context.Context) (int, bool) {
	val, ok := ctx.Value(maxAttemptsKey{}).(int)
	return val, ok
}
