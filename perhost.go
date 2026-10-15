package nicehttp

import (
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultPerHostConfig is config used for hosts that have none: the package defaults
// (DefaultAttemptTimeout, DefaultMaxAttempts, the default User-Agent) and a clone of DefaultBackoff.
var DefaultPerHostConfig = PerHostConfig{
	Backoff:        DefaultBackoff.Clone(),
	AttemptTimeout: DefaultAttemptTimeout,
	MaxAttempts:    DefaultMaxAttempts,
	DefaultHeaders: http.Header{
		"User-Agent": []string{DefaultUserAgent},
	},
}

// PerHostConfig is the configuration of a single host. CreateNiceTransport applies it on
// top of builder defaults.
type PerHostConfig struct {
	DefaultHeaders http.Header
	Backoff        *Backoff
	TLS            *tls.Config

	// AttemptTimeout limits the time of a single attempt. 0 means no attempt
	// timeout (NoTimeout), and a negative value (TimeoutNotSet) means unset,
	// which uses DefaultAttemptTimeout. Config loading never produces a
	// negative value: an unset key is inherited from the defaults, and "none"
	// or 0 is off.
	AttemptTimeout time.Duration

	MaxAttempts int
}

// PerHostConfigManager holds a default config and the per host overrides.
type PerHostConfigManager struct {
	Defaults *PerHostConfig
	Hosts    map[string]*PerHostConfig
}

// transportEntry is a cached transport for one host. dynamic reports whether the
// host has no entry in the config, so it was created for a host that appeared
// dynamically. lastSeq is the seq value of the last lookup of the entry, so the
// smallest lastSeq is the least recently used entry.
type transportEntry struct {
	transport *NiceTransport
	dynamic   bool
	lastSeq   atomic.Uint64
}

// NicePerHostTransport is an http.RoundTripper that uses one transport per host, created on
// the first request to that host.
type NicePerHostTransport struct {
	mu         sync.RWMutex
	transports map[string]*transportEntry
	// seq stamps lastSeq: every lookup that hits a cached entry takes the next
	// value, so the smallest lastSeq is the least recently used entry.
	seq    atomic.Uint64
	config *PerHostConfigManager
}

// Clone returns a deep copy of the config.
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

// CreateNiceTransport builds the transport for this config, combined with builder defaults.
func (c *PerHostConfig) CreateNiceTransport() (*NiceTransport, error) {
	builder := NewNiceTransportBuilder()
	if len(c.DefaultHeaders) > 0 {
		builder = builder.SetDefaultHeaders(c.DefaultHeaders)
	}
	if c.MaxAttempts > 0 {
		builder = builder.SetMaxAttempts(c.MaxAttempts)
	}
	// A negative value means unset, so it follows the default; 0 (NoTimeout)
	// means no attempt timeout
	attemptTimeout := c.AttemptTimeout
	if attemptTimeout >= 0 {
		builder = builder.SetAttemptTimeout(attemptTimeout)
	}
	if c.Backoff != nil {
		builder = builder.SetLimiterBackoff(c.Backoff)
	}
	if c.TLS != nil {
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, errors.New("tls config requires http.DefaultTransport to be an *http.Transport")
		}
		downstream := base.Clone()
		downstream.TLSClientConfig = c.TLS.Clone()
		builder = builder.SetDownstreamTransport(downstream)
	}

	return builder.Build(), nil
}

// ForHost returns a copy of the config for host, or of the defaults when host is not configured.
func (m *PerHostConfigManager) ForHost(host string) (*PerHostConfig, error) {
	if m == nil || m.Defaults == nil {
		return nil, fmt.Errorf("no default per host config")
	}
	host = normalizeHost(host)
	if config, ok := m.Hosts[host]; ok && config != nil {
		return config.Clone(), nil
	}

	return m.Defaults.Clone(), nil
}

// ForHost returns the transport for host, creating and caching it on first use.
func (t *NicePerHostTransport) ForHost(host string) (*NiceTransport, error) {
	if t.config == nil {
		return nil, errors.New("per host transport has no config, create it with CreateNicePerHostTransport")
	}
	host = normalizeHost(host)

	t.mu.RLock()
	entry, ok := t.transports[host]
	t.mu.RUnlock()
	if ok {
		entry.lastSeq.Store(t.seq.Add(1))
		return entry.transport, nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	// Re-check: another goroutine may have created it while we waited
	if entry, ok := t.transports[host]; ok {
		entry.lastSeq.Store(t.seq.Add(1))
		return entry.transport, nil
	}

	config, err := t.config.ForHost(host)
	if err != nil {
		return nil, err
	}
	transport, err := config.CreateNiceTransport()
	if err != nil {
		return nil, fmt.Errorf("while creating transport for host %q: %w", host, err)
	}
	_, configured := t.config.Hosts[host]
	entry = &transportEntry{transport: transport, dynamic: !configured}
	entry.lastSeq.Store(t.seq.Add(1))
	t.transports[host] = entry
	return transport, nil
}

// CloseIdleConnections closes idle connections in all the transports created
// for the hosts requested so far. It is called by
// http.Client.CloseIdleConnections.
func (t *NicePerHostTransport) CloseIdleConnections() {
	t.mu.RLock()
	entries := slices.Collect(maps.Values(t.transports))
	t.mu.RUnlock()

	for _, entry := range entries {
		entry.transport.CloseIdleConnections()
	}
}

// RoundTrip dispatches the request to the transport of its host.
func (t *NicePerHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || req.URL.Hostname() == "" {
		if req != nil && req.Body != nil {
			req.Body.Close()
		}
		return nil, errors.New("invalid request URL: missing hostname")
	}
	transport, err := t.ForHost(req.URL.Hostname())
	if err != nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, fmt.Errorf("NicePerHostTransport.RoundTrip: %w", err)
	}
	return transport.RoundTrip(req)
}

// CreateNicePerHostTransport returns a NicePerHostTransport using a deep copy
// of the manager, so later changes to the manager do not affect it. Defaults
// must be set. Host names are validated and normalized the same way as config
// loading: a name with a port or an empty name is an error, case, surrounding
// spaces, a trailing dot and the brackets of an IPv6 literal are ignored, and
// names that collide after normalization are an error. Nil host entries use the
// defaults.
//
// Transports are created lazily on the first request to each host and then
// cached. The result must come from this method, since the zero value has no
// config.
func (m *PerHostConfigManager) CreateNicePerHostTransport() (*NicePerHostTransport, error) {
	if m.Defaults == nil {
		return nil, errors.New("no default per host config")
	}

	// Deep copy so later changes by the caller cannot race with ForHost
	cloned := PerHostConfigManager{
		Defaults: m.Defaults.Clone(),
		Hosts:    make(map[string]*PerHostConfig, len(m.Hosts)),
	}
	seen := make(map[string]struct{}, len(m.Hosts))
	for _, host := range slices.Sorted(maps.Keys(m.Hosts)) {
		if err := validateHostName(host); err != nil {
			return nil, fmt.Errorf("hosts[%s]: %w", host, err)
		}
		key := normalizeHost(host)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("hosts[%s]: duplicate host after normalization", host)
		}
		seen[key] = struct{}{}
		if cfg := m.Hosts[host]; cfg != nil {
			cloned.Hosts[key] = cfg.Clone()
		}
	}

	return &NicePerHostTransport{
		config:     &cloned,
		transports: make(map[string]*transportEntry),
	}, nil
}

// normalizeHost makes host names comparable: dns names are case insensitive,
// and "example.com." is the same host as "example.com".
func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	// An IPv6 literal may be written the way a URL spells it, in brackets,
	// while URL.Hostname() returns it without them
	host = strings.Trim(host, "[]")
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// NewPerHostConfigManagerFromMap creates a PerHostConfigManager from a decoded
// config map, for example the result of decoding yaml or json into
// map[string]any.
//
// Top level keys:
//   - defaults: starts from DefaultPerHostConfig and applies the keys it sets
//   - hosts: map of host name to config. Each host starts from the resulting
//     defaults and applies the keys it sets. A null entry means defaults.
//
// Config keys:
//   - max_attempts: whole number, at least 1
//   - attempt_timeout: positive duration string, for example "30s", or
//     "none" (or "0s", or the number 0) for no attempt timeout
//   - default_headers: map of header name to a string or list of strings.
//     A name replaces the inherited header of the same name; an empty list or
//     null removes it. An empty string is sent as an empty value.
//   - backoff: map accepted by NewBackoffFromMap
//   - tls: map accepting insecure_skip_verify (bool). It replaces the
//     inherited tls config rather than merging with it.
//
// Host names are lower cased, a trailing dot is ignored, and they must not
// contain a port, because requests are matched on URL.Hostname(). Names that
// collide after normalization are an error. Unknown keys and values of the
// wrong type are errors, and errors are reported in sorted key order.
//
// Inheritance is a snapshot taken while parsing: changing Defaults in code
// afterwards does not affect hosts that were already parsed.
func NewPerHostConfigManagerFromMap(data map[string]any) (*PerHostConfigManager, error) {
	for _, key := range slices.Sorted(maps.Keys(data)) {
		if key != "defaults" && key != "hosts" {
			return nil, fmt.Errorf("%s: unknown key", key)
		}
	}

	manager := &PerHostConfigManager{
		Defaults: DefaultPerHostConfig.Clone(),
		Hosts:    make(map[string]*PerHostConfig),
	}

	if value := data["defaults"]; value != nil {
		defaultsData, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("defaults: must be a map")
		}
		defaults, err := perHostConfigFromEntry(defaultsData, manager.Defaults, "defaults")
		if err != nil {
			return nil, err
		}
		manager.Defaults = defaults
	}

	if value := data["hosts"]; value != nil {
		hostsData, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("hosts: must be a map")
		}
		for _, host := range slices.Sorted(maps.Keys(hostsData)) {
			if err := validateHostName(host); err != nil {
				return nil, fmt.Errorf("hosts[%s]: %w", host, err)
			}
			key := normalizeHost(host)
			if _, dup := manager.Hosts[key]; dup {
				return nil, fmt.Errorf("hosts[%s]: duplicate of another host after normalization", host)
			}

			var entry map[string]any // nil entry: host gets the defaults
			if raw := hostsData[host]; raw != nil {
				if entry, ok = raw.(map[string]any); !ok {
					return nil, fmt.Errorf("hosts[%s]: must be a map", host)
				}
			}
			config, err := perHostConfigFromEntry(entry, manager.Defaults, fmt.Sprintf("hosts[%s]", key))
			if err != nil {
				return nil, err
			}
			manager.Hosts[key] = config
		}
	}
	return manager, nil
}

// perHostConfigFromEntry returns a copy of defaults with the keys in entry
// applied, so unset keys are inherited. A nil defaults means the zero value.
// path prefixes error messages, for example "hosts[api.example.com]". Keys are
// processed in sorted order so the first reported error is reproducible.
func perHostConfigFromEntry(entry map[string]any, defaults *PerHostConfig, path string) (*PerHostConfig, error) {
	config := &PerHostConfig{
		AttemptTimeout: TimeoutNotSet,
	}
	if defaults != nil {
		config = defaults.Clone()
	}

	for _, key := range slices.Sorted(maps.Keys(entry)) {
		value := entry[key]
		switch key {
		case "max_attempts":
			maxAttempts, err := maxAttemptsFromValue(value, keyPath(path, key))
			if err != nil {
				return nil, err
			}
			config.MaxAttempts = maxAttempts
		case "attempt_timeout":
			attemptTimeout, err := attemptTimeoutFromValue(value, keyPath(path, key))
			if err != nil {
				return nil, err
			}
			config.AttemptTimeout = attemptTimeout
		case "default_headers":
			headers, err := headersFromValue(value, keyPath(path, key))
			if err != nil {
				return nil, err
			}
			if config.DefaultHeaders == nil {
				config.DefaultHeaders = make(http.Header)
			}
			// Headers replace the inherited value per name. A removal is kept
			// as an empty list, which also removes the header the builder
			// would otherwise add
			for _, name := range slices.Sorted(maps.Keys(headers)) {
				config.DefaultHeaders.Del(name)
				if len(headers[name]) == 0 {
					config.DefaultHeaders[name] = []string{}
					continue
				}
				for _, v := range headers[name] {
					config.DefaultHeaders.Add(name, v)
				}
			}
		case "backoff":
			backoffData, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s: must be a map", keyPath(path, key))
			}
			backoff, err := NewBackoffFromMap(backoffData, config.Backoff)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", keyPath(path, key), err)
			}
			config.Backoff = backoff
		case "tls":
			if value == nil {
				config.TLS = nil // drop any inherited tls config
				continue
			}
			tlsConfig, err := tlsFromValue(value, keyPath(path, key))
			if err != nil {
				return nil, err
			}
			config.TLS = tlsConfig
		default:
			return nil, fmt.Errorf("%s: unknown key", keyPath(path, key))
		}
	}

	return config, nil
}

// keyPath joins a key path prefix and a key for use in error messages
func keyPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// attemptTimeoutFromValue reads an attempt timeout: a positive duration, or
// "none", "0s" or 0 for no attempt timeout.
func attemptTimeoutFromValue(value any, path string) (time.Duration, error) {
	switch v := value.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			return NoTimeout, nil
		}
		if duration, err := time.ParseDuration(v); err == nil && duration == NoTimeout {
			return NoTimeout, nil
		}
	case int:
		if v == 0 {
			return NoTimeout, nil // yaml decodes numbers as int
		}
	case int64:
		if v == 0 {
			return NoTimeout, nil
		}
	case float64:
		if v == 0 {
			return NoTimeout, nil // json decodes numbers as float64
		}
	}

	timeout, err := durationFromValue(value, path)
	if err != nil {
		return 0, fmt.Errorf("%w (use %q for no attempt timeout)", err, "none")
	}
	return timeout, nil
}

// durationFromValue parses a duration string, for example "30s"
func durationFromValue(value any, path string) (time.Duration, error) {
	valueString, ok := value.(string)
	if !ok {
		return 0, fmt.Errorf("%s: must be a duration string", path)
	}
	duration, err := time.ParseDuration(valueString)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q", path, valueString)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s: must be positive", path)
	}
	return duration, nil
}

// maxAttemptsFromValue reads a whole number of attempts. Yaml decodes numbers as
// int and json as float64, so both are accepted
func maxAttemptsFromValue(value any, path string) (int, error) {
	errRange := fmt.Errorf("%s: must be a whole number between 1 and %d", path, math.MaxInt32)
	var n int64
	switch v := value.(type) {
	case int:
		n = int64(v)
	case int32:
		n = int64(v)
	case int64:
		n = v
	case uint64:
		if v > math.MaxInt32 {
			return 0, errRange
		}
		n = int64(v)
	case float64:
		// NaN fails the Trunc comparison, Inf fails the range check below
		if v != math.Trunc(v) || v < 1 || v > math.MaxInt32 {
			return 0, errRange
		}
		n = int64(v)
	default:
		return 0, fmt.Errorf("%s: must be a whole number", path)
	}
	if n < 1 || n > math.MaxInt32 {
		return 0, errRange
	}
	return int(n), nil
}

// headersFromValue reads default headers, where each value is a string or a list
// of strings. An empty list means the header is removed
func headersFromValue(value any, path string) (map[string][]string, error) {
	headersData, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: must be a map", path)
	}

	headers := make(map[string][]string, len(headersData))
	for _, name := range slices.Sorted(maps.Keys(headersData)) {
		canonical := http.CanonicalHeaderKey(name)
		if _, dup := headers[canonical]; dup {
			return nil, fmt.Errorf("%s[%s]: duplicate header after canonicalization (%s)", path, name, canonical)
		}

		switch v := headersData[name].(type) {
		case nil:
			// A null header, and an empty list below, both remove it
			headers[canonical] = []string{}
		case string:
			headers[canonical] = []string{v}
		case []string:
			headers[canonical] = slices.Clone(v)
		case []any:
			values := make([]string, 0, len(v))
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("%s[%s]: value must be a string", path, name)
				}
				values = append(values, s)
			}
			headers[canonical] = values
		default:
			return nil, fmt.Errorf("%s[%s]: must be a string or a list of strings", path, name)
		}
	}
	return headers, nil
}

// tlsFromValue reads a tls config. Only insecure_skip_verify is supported;
// certificate files and inline certificates are not
func tlsFromValue(value any, path string) (*tls.Config, error) {
	tlsData, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: must be a map", path)
	}

	tlsConfig := &tls.Config{}
	for _, key := range slices.Sorted(maps.Keys(tlsData)) {
		value := tlsData[key]
		switch key {
		case "insecure_skip_verify":
			insecureSkipVerify, ok := value.(bool)
			if !ok {
				return nil, fmt.Errorf("%s: must be a boolean", keyPath(path, key))
			}
			tlsConfig.InsecureSkipVerify = insecureSkipVerify
		default:
			return nil, fmt.Errorf("%s: unknown key", keyPath(path, key))
		}
	}

	return tlsConfig, nil
}

func validateHostName(host string) error {
	// The normalized name is what requests are matched on, so validate that
	normalized := normalizeHost(host)
	if normalized == "" {
		return errors.New("hostname must not be empty")
	}
	if _, err := netip.ParseAddr(normalized); err != nil && strings.Contains(normalized, ":") {
		return errors.New("hostname must not contain a port")
	}
	return nil
}
