package nicehttp

import (
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

// Builder struct holds raw inputs
type NiceTransportBuilder struct {
	defaultHeaders      http.Header
	rateLimiter         *rate.Limiter
	backoff             time.Duration
	maxBackoff          time.Duration
	interval            time.Duration
	burst               int
	maxTries            int
	downstreamTransport http.RoundTripper
}

// Constructor
func NewNiceTransportBuilder() *NiceTransportBuilder {
	return &NiceTransportBuilder{
		defaultHeaders: map[string][]string{"User-Agent": {DefaultUserAgent}},
	}
}

// Chained setters
func (b *NiceTransportBuilder) Set(settings *NiceTransport) *NiceTransportBuilder {
	b.SetDefaultHeaders(settings.DefaultHeaders)
	b.SetBackoff(settings.Backoff, settings.MaxBackoff)
	b.SetMaxTries(settings.MaxTries)
	b.SetDownstreamTransport(settings.DownstreamTransport)
	b.rateLimiter = nil
	if settings.RateLimiter != nil {
		rl := settings.RateLimiter
		b.rateLimiter = rate.NewLimiter(rl.Limit(), rl.Burst())
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

func (b *NiceTransportBuilder) SetRateLimit(interval time.Duration, burst int) *NiceTransportBuilder {
	b.interval = interval
	b.burst = burst
	return b
}

func (b *NiceTransportBuilder) SetBackoff(backoff time.Duration, maxBackoff time.Duration) *NiceTransportBuilder {
	b.backoff = backoff
	b.maxBackoff = maxBackoff
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
	if b.backoff <= 0 {
		b.backoff = 1 * time.Second
	}
	if b.maxBackoff <= 0 {
		b.maxBackoff = 120 * time.Second
	}
	if b.maxTries <= 0 {
		b.maxTries = 10
	}
	if b.downstreamTransport == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxConnsPerHost = 1
		b.downstreamTransport = transport
	}

	if b.rateLimiter == nil {
		if b.interval <= 0 {
			b.interval = 1 * time.Second
		}
		if b.burst <= 0 {
			b.burst = 1
		}
		b.rateLimiter = rate.NewLimiter(rate.Every(b.interval), b.burst)
	}

	return &NiceTransport{
		DefaultHeaders:      b.defaultHeaders,
		Backoff:             b.backoff,
		MaxBackoff:          b.maxBackoff,
		MaxTries:            b.maxTries,
		RateLimiter:         b.rateLimiter,
		DownstreamTransport: b.downstreamTransport,
	}, nil
}
