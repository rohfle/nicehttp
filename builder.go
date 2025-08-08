package nicehttp

import (
	"net/http"
	"time"
)

// Builder struct holds raw inputs
type NiceTransportBuilder struct {
	defaultHeaders      http.Header
	minWait             time.Duration
	maxWait             time.Duration
	maxTries            int
	downstreamTransport http.RoundTripper
	limiter             *Limiter
}

// Constructor
func NewNiceTransportBuilder() *NiceTransportBuilder {
	return &NiceTransportBuilder{
		defaultHeaders: map[string][]string{"User-Agent": {DefaultUserAgent}},
	}
}

// Chained setters
func (b *NiceTransportBuilder) Set(settings *NiceTransport) *NiceTransportBuilder {
	b.SetDefaultHeaders(settings.defaultHeaders)
	b.SetMaxTries(settings.maxTries)
	b.SetDownstreamTransport(settings.downstreamTransport)
	b.SetRequestInterval(b.minWait, b.maxWait)
	b.limiter = nil
	if settings.limiter != nil {
		b.limiter = settings.limiter.Clone()
	}
	return b
}

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

func (b *NiceTransportBuilder) SetUserAgent(ua string) *NiceTransportBuilder {
	b.defaultHeaders.Set("User-Agent", ua)
	return b
}

func (b *NiceTransportBuilder) SetRequestInterval(minWait time.Duration, maxWait time.Duration) *NiceTransportBuilder {
	b.minWait = minWait
	b.maxWait = maxWait
	return b
}

func (b *NiceTransportBuilder) SetMaxTries(n int) *NiceTransportBuilder {
	b.maxTries = n
	return b
}

func (b *NiceTransportBuilder) SetDownstreamTransport(rt http.RoundTripper) *NiceTransportBuilder {
	b.downstreamTransport = rt
	return b
}

// Finalize and return
func (b *NiceTransportBuilder) Build() (*NiceTransport, error) {
	// Set defaults
	if b.maxTries <= 0 {
		b.maxTries = 10
	}
	if b.downstreamTransport == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		b.downstreamTransport = transport
	}

	if b.limiter == nil {
		if b.minWait <= 0 {
			b.minWait = 1 * time.Second
		}
		if b.maxWait <= 0 {
			b.maxWait = 120 * time.Second
		}
		b.limiter = NewLimiter(b.minWait, b.maxWait)
	}

	return &NiceTransport{
		defaultHeaders:      b.defaultHeaders,
		maxTries:            b.maxTries,
		limiter:             b.limiter,
		downstreamTransport: b.downstreamTransport,
	}, nil
}
