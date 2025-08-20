package nicehttp

import (
	"context"
	"time"
)

// Define key types to avoid collisions
type maxAttemptsKey struct{}
type attemptTimeoutKey struct{}

// SetAttemptTimeoutInContext returns a new context with the given timeout value.
func SetAttemptTimeoutInContext(parent context.Context, timeout time.Duration) context.Context {
	return context.WithValue(parent, attemptTimeoutKey{}, timeout)
}

// GetAttemptTimeoutFromContext retrieves the timeout value from the context.
func GetAttemptTimeoutFromContext(ctx context.Context) (time.Duration, bool) {
	val, ok := ctx.Value(attemptTimeoutKey{}).(time.Duration)
	return val, ok
}

// SetMaxAttemptsInContext returns a new context with the given max attempts value.
func SetMaxAttemptsInContext(parent context.Context, attempts int) context.Context {
	return context.WithValue(parent, maxAttemptsKey{}, attempts)
}

// GetMaxAttemptsFromContext retrieves the max attempts value from the context.
func GetMaxAttemptsFromContext(ctx context.Context) (int, bool) {
	val, ok := ctx.Value(maxAttemptsKey{}).(int)
	return val, ok
}
