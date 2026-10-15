package nicehttp

import (
	"net/http"
	"time"
)

// NiceTransportBuilder is a builder used to create a NiceTransport
type NiceTransportBuilder struct {
	defaultHeaders      http.Header
	attemptTimeout      time.Duration // TimeoutNotSet until SetAttemptTimeout is called
	maxAttempts         int
	downstreamTransport http.RoundTripper
	limiter             *Limiter
}

// NewNiceTransportBuilder creates a NewNiceTransportBuilder
func NewNiceTransportBuilder() *NiceTransportBuilder {
	return &NiceTransportBuilder{
		defaultHeaders: map[string][]string{"User-Agent": {DefaultUserAgent}},
		attemptTimeout: TimeoutNotSet,
	}
}

// Set sets values of the builder from an existing NiceTransport
func (b *NiceTransportBuilder) Set(settings *NiceTransport) *NiceTransportBuilder {
	b.SetDefaultHeaders(settings.defaultHeaders)
	b.SetMaxAttempts(settings.maxAttempts)
	b.SetDownstreamTransport(settings.downstreamTransport)
	b.SetAttemptTimeout(settings.attemptTimeout)
	b.limiter = nil
	if settings.limiter != nil {
		b.limiter = settings.limiter.Clone()
	}
	return b
}

// SetDefaultHeaders sets headers that will be by default included
// in each request. If this is called multiple times, then existing
// keys will be overwritten.
func (b *NiceTransportBuilder) SetDefaultHeaders(headers http.Header) *NiceTransportBuilder {
	if b.defaultHeaders == nil {
		b.defaultHeaders = make(http.Header)
	}
	for key, values := range headers {
		b.defaultHeaders.Del(key)
		if len(values) == 0 {
			continue
		}
		for _, value := range values {
			b.defaultHeaders.Add(key, value)
		}
	}
	return b
}

// SetLimiterBackoff sets the backoff for the limiter.
// If this is not set, or set to nil, the DefaultBackoff will be used.
func (b *NiceTransportBuilder) SetLimiterBackoff(backoff *Backoff) *NiceTransportBuilder {
	if backoff == nil {
		// A nil backoff means the default backoff is used
		b.limiter = nil
		return b
	}
	b.limiter = &Limiter{
		backoff: backoff.Clone(),
	}
	return b
}

// SetAttemptTimeout sets timeout for a single connection attempt,
// after which the request will be retried. This is part of the total
// request time, which is controlled by the http.Client timeout.
// A value of NoTimeout means no timeout, and TimeoutNotSet (or any negative
// value) means the attempt timeout is not set, so DefaultAttemptTimeout is
// used.
func (b *NiceTransportBuilder) SetAttemptTimeout(timeout time.Duration) *NiceTransportBuilder {
	b.attemptTimeout = timeout
	return b
}

// SetUserAgent sets the User-Agent in default headers
func (b *NiceTransportBuilder) SetUserAgent(ua string) *NiceTransportBuilder {
	if b.defaultHeaders == nil {
		b.defaultHeaders = make(http.Header)
	}
	b.defaultHeaders.Set("User-Agent", ua)
	return b
}

// SetMaxAttempts sets the maximum number of request attempts that will be made
func (b *NiceTransportBuilder) SetMaxAttempts(n int) *NiceTransportBuilder {
	b.maxAttempts = n
	return b
}

// SetDownstreamTransport sets the downstream transport to be called by RoundTrip
func (b *NiceTransportBuilder) SetDownstreamTransport(rt http.RoundTripper) *NiceTransportBuilder {
	b.downstreamTransport = rt
	return b
}

// Build creates a NiceTransport with settings given previously combined with defaults.
func (b *NiceTransportBuilder) Build() *NiceTransport {
	// Set defaults
	if b.defaultHeaders == nil {
		// The builder was not created by NewNiceTransportBuilder, so apply the
		// default User-Agent here
		b.defaultHeaders = http.Header{"User-Agent": {DefaultUserAgent}}
	}
	if b.maxAttempts <= 0 {
		b.maxAttempts = DefaultMaxAttempts
	}
	if b.attemptTimeout < 0 {
		b.attemptTimeout = DefaultAttemptTimeout
	}
	if b.downstreamTransport == nil {
		b.downstreamTransport = cloneDefaultTransport()
	}
	if b.limiter == nil {
		b.limiter = &Limiter{
			backoff: DefaultBackoff.Clone(),
		}
	}

	return &NiceTransport{
		defaultHeaders:      b.defaultHeaders,
		maxAttempts:         b.maxAttempts,
		attemptTimeout:      b.attemptTimeout,
		limiter:             b.limiter,
		downstreamTransport: b.downstreamTransport,
	}
}
