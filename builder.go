package nicehttp

import (
	"net/http"
	"time"
)

// NiceTransportBuilder is a builder used to create a NiceTransport
type NiceTransportBuilder struct {
	defaultHeaders      http.Header
	attemptTimeout      time.Duration
	maxAttempts         int
	downstreamTransport http.RoundTripper
	limiter             *Limiter
}

// NewNiceTransportBuilder creates a NewNiceTransportBuilder
func NewNiceTransportBuilder() *NiceTransportBuilder {
	return &NiceTransportBuilder{
		defaultHeaders: map[string][]string{"User-Agent": {DefaultUserAgent}},
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

// If this is not set, the DefaultExponentialBackoff will be used
func (b *NiceTransportBuilder) SetLimiterBackoff(backoff Backoff) *NiceTransportBuilder {
	b.limiter = &Limiter{
		backoff: backoff.Clone(),
	}
	return b
}

// SetAttemptTimeout sets timeout for a single connection attempt,
// after which the request will be retried. This is part of the total
// request time, which is controlled by the http.Client timeout.

// A value of 0 means no timeout.
func (b *NiceTransportBuilder) SetAttemptTimeout(timeout time.Duration) *NiceTransportBuilder {
	b.attemptTimeout = timeout
	return b
}

// SetUserAgent sets the User-Agent in default headers
func (b *NiceTransportBuilder) SetUserAgent(ua string) *NiceTransportBuilder {
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
func (b *NiceTransportBuilder) Build() (*NiceTransport, error) {
	// Set defaults
	if b.maxAttempts <= 0 {
		b.maxAttempts = 10
	}
	if b.downstreamTransport == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		b.downstreamTransport = transport
	}
	if b.limiter == nil {
		b.limiter = &Limiter{
			backoff: DefaultExponentialBackoff.Clone(),
		}
	}

	return &NiceTransport{
		defaultHeaders:      b.defaultHeaders,
		maxAttempts:         b.maxAttempts,
		attemptTimeout:      b.attemptTimeout,
		limiter:             b.limiter,
		downstreamTransport: b.downstreamTransport,
	}, nil
}
