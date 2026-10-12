package nicehttp

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// TODO: function to load perhostconfigmanager from yaml and json

var DefaultPerHostConfig = PerHostConfig{
	UserAgent:      DefaultUserAgent,
	Backoff:        DefaultExponentialBackoff.Clone(),
	AttemptTimeout: DefaultAttemptTimeout,
	MaxAttempts:    DefaultMaxAttempts,
}

type PerHostConfig struct {
	UserAgent      string
	DefaultHeaders http.Header
	Backoff        Backoff
	TLS            *tls.Config
	AttemptTimeout time.Duration
	MaxAttempts    int
}

type PerHostConfigManager struct {
	Defaults *PerHostConfig
	Hosts    map[string]*PerHostConfig
}

type NicePerHostTransport struct {
	mu         sync.Mutex
	transports map[string]*NiceTransport
	config     PerHostConfigManager
}

func (c *PerHostConfig) Clone() *PerHostConfig {
	cfg := *c
	if cfg.Backoff != nil {
		cfg.Backoff = cfg.Backoff.Clone()
	}
	if cfg.TLS != nil {
		cfg.TLS = cfg.TLS.Clone()
	}
	if cfg.DefaultHeaders != nil {
		cfg.DefaultHeaders = cfg.DefaultHeaders.Clone()
	}
	return &cfg
}

func (c *PerHostConfig) CreateNiceTransport() (*NiceTransport, error) {
	builder := NewNiceTransportBuilder()
	if len(c.DefaultHeaders) > 0 {
		builder = builder.SetDefaultHeaders(c.DefaultHeaders)
	}
	if c.UserAgent != "" {
		builder = builder.SetUserAgent(c.UserAgent)
	}
	if c.MaxAttempts > 0 {
		builder = builder.SetMaxAttempts(c.MaxAttempts)
	}
	if c.AttemptTimeout > 0 {
		builder = builder.SetAttemptTimeout(c.AttemptTimeout)
	}
	if c.Backoff != nil {
		builder = builder.SetLimiterBackoff(c.Backoff)
	}
	if c.TLS != nil {
		downstream := http.DefaultTransport.(*http.Transport).Clone()
		downstream.TLSClientConfig = c.TLS.Clone()
		builder = builder.SetDownstreamTransport(downstream)
	}

	return builder.Build()
}

func (m *PerHostConfigManager) ForHost(host string) (*PerHostConfig, error) {
	config, exists := m.Hosts[host]
	if exists {
		return config.Clone(), nil
	}

	return m.Defaults.Clone(), nil
}

func (t *NicePerHostTransport) ForHost(host string) (*NiceTransport, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if transport, exists := t.transports[host]; exists {
		return transport, nil
	}

	config, err := t.config.ForHost(host)
	if err != nil {
		return nil, err
	}
	transport, err := config.CreateNiceTransport()
	if err != nil {
		return nil, fmt.Errorf("while creating transport for host %q: %w", host, err)
	}
	t.transports[host] = transport
	return transport, nil
}

// CloseIdleConnections closes idle connections in all the transports created
// for the hosts requested so far. It is called by
// http.Client.CloseIdleConnections.
func (t *NicePerHostTransport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, transport := range t.transports {
		transport.CloseIdleConnections()
	}
}

func (t *NicePerHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || req.URL.Hostname() == "" {
		return nil, fmt.Errorf("invalid request URL: missing hostname")
	}
	transport, err := t.ForHost(req.URL.Hostname())
	if err != nil {
		return nil, fmt.Errorf("NicePerHostTransport.RoundTrip: %w", err)
	}
	return transport.RoundTrip(req)
}
