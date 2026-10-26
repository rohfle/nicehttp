package nicehttp

import (
	"encoding/json"
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

// HostConfig is the configuration of a single host. The zero value means the
// package defaults: DefaultMaxAttempts, DefaultAttemptTimeout, DefaultBackoff
// and the builder's default User-Agent.
type HostConfig struct {
	// DefaultHeaders are added to every request. A nil header inherits the
	// defaults; a name with an empty value list removes an inherited header.
	DefaultHeaders http.Header
	// Backoff is the limiter backoff. nil means DefaultBackoff.
	Backoff *Backoff
	// AttemptTimeout limits a single attempt. nil means DefaultAttemptTimeout;
	// a pointer to NoTimeout disables the attempt timeout.
	AttemptTimeout *time.Duration
	// MaxAttempts is the maximum number of attempts, from 1 to 100. 0 means
	// DefaultMaxAttempts.
	MaxAttempts int
}

// Clone returns a deep copy of the config.
func (c *HostConfig) Clone() *HostConfig {
	if c == nil {
		return &HostConfig{}
	}
	cfg := *c
	if cfg.Backoff != nil {
		cfg.Backoff = cfg.Backoff.Clone()
	}
	if cfg.DefaultHeaders != nil {
		cfg.DefaultHeaders = cfg.DefaultHeaders.Clone()
	}
	if cfg.AttemptTimeout != nil {
		attemptTimeout := *cfg.AttemptTimeout
		cfg.AttemptTimeout = &attemptTimeout
	}
	return &cfg
}

// PerHostConfig is a complete per-host configuration: the defaults, the host
// overrides, and the downstream transport shared by every host.
type PerHostConfig struct {
	// Defaults supplies the fields a host does not set. nil means the package
	// defaults. It is not modified by the transport.
	Defaults *HostConfig
	// Hosts maps host names to configs. Unset fields inherit from Defaults when
	// the transport is built, so entries may be partial. Entries returned by
	// PerHostConfigFromMap are already resolved snapshots. A nil value defines
	// the host with only the defaults, which keeps it out of the dynamic-host
	// cache bound.
	Hosts map[string]*HostConfig
	// Downstream is the roundtripper shared by every host, so one connection
	// pool serves all of them. nil means http.DefaultTransport.
	Downstream http.RoundTripper
}

// PerHostConfigFromMap parses a decoded config map. Decode with encoding/json
// into map[string]any (json.Number is accepted when Decoder.UseNumber is set)
// or with yaml.v3; yaml.v2's map[interface{}]interface{} is not supported. An
// empty or nil map yields the package defaults and no configured hosts.
//
// Top-level keys:
//   - defaults: Overrides the package defaults.
//   - hosts: Map of host names to overrides. Each host inherits the defaults;
//     a null entry defines the host with only the defaults.
//
// Host keys:
//   - max_attempts: Whole number from 1 to 100.
//   - attempt_timeout: Positive duration string (e.g., "30s"), "none", "0s",
//     or 0 for no attempt timeout.
//   - default_headers: Map of header names to strings or string lists. Replaces
//     the inherited value per name; an empty list or null removes the header.
//   - backoff: Map accepted by NewBackoffFromMap, merged onto the inherited
//     backoff.
//
// Host names are validated and normalized: case, surrounding spaces, trailing
// dots, and IPv6 brackets are ignored. Names with ports, empty names, or
// collisions after normalization are rejected. Unknown keys and type
// mismatches are rejected with sorted, path-prefixed errors.
//
// The downstream transport cannot be set from the map; assign it on the
// returned config instead.
//
// The returned Hosts entries are resolved snapshots: each is the parsed
// defaults with its overrides applied. Changing Defaults after parsing does not
// affect them, but it does affect hosts without an entry once the config is
// passed to NewNicePerHostTransport.
func PerHostConfigFromMap(settings map[string]any) (PerHostConfig, error) {
	for _, key := range slices.Sorted(maps.Keys(settings)) {
		if key != "defaults" && key != "hosts" {
			return PerHostConfig{}, fmt.Errorf("%s: unknown key", key)
		}
	}

	config := PerHostConfig{
		Defaults: &HostConfig{},
		Hosts:    make(map[string]*HostConfig),
	}

	if value := settings["defaults"]; value != nil {
		defaultsData, ok := value.(map[string]any)
		if !ok {
			return PerHostConfig{}, errors.New("defaults: must be a map")
		}
		defaults, err := parseHostConfig(defaultsData, config.Defaults, "defaults")
		if err != nil {
			return PerHostConfig{}, err
		}
		config.Defaults = defaults
	}

	if value := settings["hosts"]; value != nil {
		hostsData, ok := value.(map[string]any)
		if !ok {
			return PerHostConfig{}, errors.New("hosts: must be a map")
		}
		seen := make(map[string]string, len(hostsData))
		for _, host := range slices.Sorted(maps.Keys(hostsData)) {
			if err := validateHostName(host); err != nil {
				return PerHostConfig{}, fmt.Errorf("hosts[%s]: %w", host, err)
			}
			key := normalizeHost(host)
			if previous, dup := seen[key]; dup {
				return PerHostConfig{}, fmt.Errorf("hosts[%s]: collides with %q after normalization", host, previous)
			}
			seen[key] = host

			var entry map[string]any // nil entry: host gets the defaults
			if raw := hostsData[host]; raw != nil {
				if entry, ok = raw.(map[string]any); !ok {
					return PerHostConfig{}, fmt.Errorf("hosts[%s]: must be a map", host)
				}
			}
			hostConfig, err := parseHostConfig(entry, config.Defaults, fmt.Sprintf("hosts[%s]", key))
			if err != nil {
				return PerHostConfig{}, err
			}
			config.Hosts[key] = hostConfig
		}
	}

	return config, nil
}

// maxAttemptsLimit bounds max_attempts, whether parsed from config, set on the
// builder or overridden through the context, so a typo is reported or clamped
// instead of turning into a multi-minute retry loop.
const maxAttemptsLimit = 100

// maxDynamicHosts bounds transports for hosts absent from the config, so an
// endless stream of names cannot grow the cache. Defined hosts are never
// evicted.
const maxDynamicHosts = 256

// hostEntry caches the transport of one host. lastSeq records the sequence
// number when the entry was last used. An entry is dynamic when it is present
// in NicePerHostTransport.dynamic.
type hostEntry struct {
	transport *NiceTransport
	lastSeq   atomic.Uint64
}

// NicePerHostTransport is an http.RoundTripper that uses one transport per
// host, created on the first request to that host. Create it with
// NewNicePerHostTransport; the zero value has no config.
type NicePerHostTransport struct {
	mu         sync.RWMutex
	transports map[string]*hostEntry
	// seq stamps lastSeq: every lookup that hits a cached entry takes the next
	// value, so the smallest lastSeq is the least recently used entry.
	seq      atomic.Uint64
	defaults *HostConfig
	// hosts holds the resolved config of every defined host. A host is defined
	// by presence, even when its config equals the defaults.
	hosts map[string]*HostConfig
	// dynamic aliases the entries in transports that came from hosts without a
	// config, so eviction scans the bound instead of every defined host.
	dynamic    map[string]*hostEntry
	downstream http.RoundTripper
}

// NewNicePerHostTransport builds a transport that dispatches to one
// NiceTransport per host. The config is copied, so later changes to it do not
// affect the transport. Defaults may be nil and Hosts may be nil or contain nil
// entries; hosts absent from the config use the defaults.
//
// Host names are validated and normalized: case, surrounding spaces, trailing
// dots, and IPv6 brackets are ignored. Names with ports, empty names, or
// collisions after normalization return an error. Hand built configs are
// checked like parsed ones: negative MaxAttempts, MaxAttempts above 100,
// negative AttemptTimeout, duplicate headers after canonicalization, and
// invalid header names or values are rejected.
//
// Every host transport shares one downstream roundtripper: config.Downstream,
// or http.DefaultTransport when nil. Defined hosts stay cached permanently,
// while dynamic hosts are bounded by maxDynamicHosts using LRU eviction.
func NewNicePerHostTransport(config PerHostConfig) (*NicePerHostTransport, error) {
	if err := validateHostConfig(config.Defaults, "defaults"); err != nil {
		return nil, err
	}
	defaults := mergeHostConfig(&HostConfig{}, config.Defaults)
	hosts, err := resolveHosts(defaults, config.Hosts)
	if err != nil {
		return nil, err
	}

	downstream := config.Downstream
	if downstream == nil {
		downstream = http.DefaultTransport
	}

	return &NicePerHostTransport{
		transports: make(map[string]*hostEntry),
		dynamic:    make(map[string]*hostEntry),
		defaults:   defaults,
		hosts:      hosts,
		downstream: downstream,
	}, nil
}

// ForHost returns the transport for host, creating and caching it on first use.
func (t *NicePerHostTransport) ForHost(host string) (*NiceTransport, error) {
	if t == nil || t.transports == nil {
		return nil, errors.New("per host transport has no config, create it with NewNicePerHostTransport")
	}
	host = normalizeHost(host)

	// Cache hits need only the read lock; lastSeq and seq are atomic.
	t.mu.RLock()
	cached, ok := t.transports[host]
	t.mu.RUnlock()
	if ok {
		cached.lastSeq.Store(t.seq.Add(1))
		return cached.transport, nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Another goroutine may have created the entry while this one waited.
	if entry, ok := t.transports[host]; ok {
		entry.lastSeq.Store(t.seq.Add(1))
		return entry.transport, nil
	}

	if err := validateHostName(host); err != nil {
		return nil, err
	}

	config, usedDefaults := t.configFor(host)
	transport := config.newNiceTransport(t.downstream)
	entry := &hostEntry{transport: transport}
	entry.lastSeq.Store(t.seq.Add(1))
	t.transports[host] = entry
	if usedDefaults {
		t.dynamic[host] = entry
		t.evictLocked()
	}
	return transport, nil
}

// configFor returns the resolved config for a normalized host, and whether the
// defaults were used. The maps are immutable after construction, so no lock is
// needed.
func (t *NicePerHostTransport) configFor(host string) (*HostConfig, bool) {
	if config, ok := t.hosts[host]; ok {
		return config, false
	}
	return t.defaults, true
}

// evictLocked evicts the least recently used dynamic transport when the bound
// is exceeded. The evicted transport is not closed because active requests may
// still use it, and its downstream is shared anyway. A host that comes back
// starts with a fresh limiter and backoff, so a Retry-After wait can be
// forgotten by churn; a request already using the old transport keeps it until
// it finishes.
// t.mu must be held.
func (t *NicePerHostTransport) evictLocked() {
	if len(t.dynamic) <= maxDynamicHosts {
		return
	}
	var victim string
	var victimSeq uint64
	for host, entry := range t.dynamic {
		if seq := entry.lastSeq.Load(); victim == "" || seq < victimSeq {
			victim, victimSeq = host, seq
		}
	}
	delete(t.transports, victim)
	delete(t.dynamic, victim)
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

// CloseIdleConnections closes idle connections in the shared downstream. It is
// called by http.Client.CloseIdleConnections. With the default downstream this
// also closes idle connections of other clients using http.DefaultTransport.
func (t *NicePerHostTransport) CloseIdleConnections() {
	if t == nil || t.downstream == nil {
		return
	}
	if closer, ok := t.downstream.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// resolveHosts validates, normalizes and resolves the configured hosts. Names
// that collide after normalization report both spellings.
func resolveHosts(defaults *HostConfig, hosts map[string]*HostConfig) (map[string]*HostConfig, error) {
	resolved := make(map[string]*HostConfig, len(hosts))
	seen := make(map[string]string, len(hosts))
	for _, host := range slices.Sorted(maps.Keys(hosts)) {
		if err := validateHostName(host); err != nil {
			return nil, fmt.Errorf("hosts[%s]: %w", host, err)
		}
		key := normalizeHost(host)
		if previous, dup := seen[key]; dup {
			return nil, fmt.Errorf("hosts[%s]: collides with %q after normalization", host, previous)
		}
		seen[key] = host
		if err := validateHostConfig(hosts[host], fmt.Sprintf("hosts[%s]", key)); err != nil {
			return nil, err
		}
		resolved[key] = mergeHostConfig(defaults, hosts[host])
	}
	return resolved, nil
}

// mergeHostConfig returns a copy of base overlaid by override. nil override
// means base only; nil base means the package defaults.
func mergeHostConfig(base, override *HostConfig) *HostConfig {
	config := base.Clone()
	if override == nil {
		return config
	}
	if override.MaxAttempts > 0 {
		config.MaxAttempts = override.MaxAttempts
	}
	if override.AttemptTimeout != nil {
		attemptTimeout := *override.AttemptTimeout
		config.AttemptTimeout = &attemptTimeout
	}
	if override.Backoff != nil {
		config.Backoff = override.Backoff.Clone()
	}
	for _, name := range slices.Sorted(maps.Keys(override.DefaultHeaders)) {
		canonical := http.CanonicalHeaderKey(name)
		if config.DefaultHeaders == nil {
			config.DefaultHeaders = make(http.Header)
		}
		values := override.DefaultHeaders[name]
		config.DefaultHeaders.Del(canonical)
		if len(values) == 0 {
			config.DefaultHeaders[canonical] = []string{}
			continue
		}
		config.DefaultHeaders[canonical] = slices.Clone(values)
	}
	return config
}

// newNiceTransport builds the transport for this config, combined with builder
// defaults. downstream is shared by every host transport.
func (c *HostConfig) newNiceTransport(downstream http.RoundTripper) *NiceTransport {
	builder := NewNiceTransportBuilder()
	// Dynamic hosts share one resolved defaults config, so copy the mutable
	// parts before handing them to the builder.
	if len(c.DefaultHeaders) > 0 {
		builder = builder.SetDefaultHeaders(c.DefaultHeaders.Clone())
	}
	if c.MaxAttempts > 0 {
		builder = builder.SetMaxAttempts(c.MaxAttempts)
	}
	if c.AttemptTimeout != nil {
		builder = builder.SetAttemptTimeout(*c.AttemptTimeout)
	}
	if c.Backoff != nil {
		builder = builder.SetLimiterBackoff(c.Backoff.Clone())
	}
	if downstream != nil {
		builder = builder.SetDownstreamTransport(downstream)
	}
	return builder.Build()
}

// parseHostConfig returns a copy of defaults with the keys in entry applied, so
// unset keys are inherited. A nil defaults means the package defaults. path
// prefixes error messages, for example "hosts[api.example.com]".
func parseHostConfig(entry map[string]any, defaults *HostConfig, path string) (*HostConfig, error) {
	config := defaults.Clone()

	for _, key := range slices.Sorted(maps.Keys(entry)) {
		value := entry[key]
		switch key {
		case "max_attempts":
			maxAttempts, err := parseMaxAttempts(value, keyPath(path, key))
			if err != nil {
				return nil, err
			}
			config.MaxAttempts = maxAttempts
		case "attempt_timeout":
			attemptTimeout, err := parseAttemptTimeout(value, keyPath(path, key))
			if err != nil {
				return nil, err
			}
			config.AttemptTimeout = &attemptTimeout
		case "default_headers":
			headers, err := parseHeaders(value, keyPath(path, key))
			if err != nil {
				return nil, err
			}
			if config.DefaultHeaders == nil {
				config.DefaultHeaders = make(http.Header)
			}
			// Headers replace the inherited value per name. A removal is kept
			// as an empty list, which also removes the header the builder
			// would otherwise add.
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
		default:
			return nil, fmt.Errorf("%s: unknown key", keyPath(path, key))
		}
	}

	return config, nil
}

// normalizeHost makes host names comparable: dns names are case insensitive,
// and "example.com." is the same host as "example.com". Brackets are removed
// only when they wrap a valid IP, so malformed names stay malformed and are
// rejected by validateHostName.
func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		if _, err := netip.ParseAddr(host[1 : len(host)-1]); err == nil {
			host = host[1 : len(host)-1]
		}
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// validateHostName rejects names that cannot be a host in a URL authority.
// It accepts IPv4 and IPv6 literals, and DNS-style names with letters, digits,
// '-', '_' and non-ASCII (IDN) characters. Ports, schemes, paths and spaces are
// rejected.
func validateHostName(host string) error {
	normalized := normalizeHost(host)
	if normalized == "" {
		return errors.New("must not be empty")
	}
	if _, err := netip.ParseAddr(normalized); err == nil {
		return nil
	}
	if strings.ContainsAny(normalized, "/?#@") || strings.ContainsAny(normalized, " \t") {
		return errors.New("must be a hostname, without scheme, path or spaces")
	}
	if strings.Contains(normalized, ":") {
		return errors.New("must not contain a port or scheme")
	}
	if strings.HasSuffix(normalized, ".") {
		return errors.New("must not have more than one trailing dot")
	}
	if len(normalized) > 253 {
		return errors.New("is too long")
	}
	for _, label := range strings.Split(normalized, ".") {
		if label == "" {
			return errors.New("must not contain empty labels")
		}
		if len(label) > 63 {
			return errors.New("label is too long")
		}
		for i := range label {
			c := label[i]
			switch {
			case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_':
			case c >= 0x80:
				// Internationalized name; leave the encoding to the transport.
			default:
				return fmt.Errorf("contains invalid character %q", c)
			}
		}
	}
	return nil
}

// validateHostConfig rejects values that would build a broken transport. It
// runs for hand built configs, which do not pass through the map parser.
func validateHostConfig(config *HostConfig, path string) error {
	if config == nil {
		return nil
	}
	if config.MaxAttempts < 0 {
		return fmt.Errorf("%s: max attempts must not be negative", path)
	}
	if config.MaxAttempts > maxAttemptsLimit {
		return fmt.Errorf("%s: max attempts must be between 1 and %d", path, maxAttemptsLimit)
	}
	if config.AttemptTimeout != nil && *config.AttemptTimeout < 0 {
		return fmt.Errorf("%s: attempt timeout must not be negative (use nil to inherit)", path)
	}

	seen := make(map[string]string, len(config.DefaultHeaders))
	for _, name := range slices.Sorted(maps.Keys(config.DefaultHeaders)) {
		if !validHeaderName(name) {
			return fmt.Errorf("%s: invalid header name %q", path, name)
		}
		canonical := http.CanonicalHeaderKey(name)
		if previous, dup := seen[canonical]; dup {
			return fmt.Errorf("%s: header %q duplicates %q after canonicalization", path, name, previous)
		}
		seen[canonical] = name
		for _, value := range config.DefaultHeaders[name] {
			if !validHeaderValue(value) {
				return fmt.Errorf("%s: invalid value for header %q", path, name)
			}
		}
	}
	return nil
}

// validHeaderName reports whether name is an RFC 7230 token.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := range name {
		c := name[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// validHeaderValue reports whether value contains no control characters other
// than tab, which is what an HTTP field value allows.
func validHeaderValue(value string) bool {
	for i := range value {
		c := value[i]
		if c == '\t' {
			continue
		}
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// keyPath joins a key path prefix and a key for use in error messages.
func keyPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// int64FromValue returns value as int64 when it is an integer kind, including
// json.Number. Floats are not accepted here because they need rounding checks.
func int64FromValue(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int8:
		return int64(v), true
	case int16:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		if uint64(v) > math.MaxInt64 {
			return 0, false
		}
		return int64(v), true
	case uint8:
		return int64(v), true
	case uint16:
		return int64(v), true
	case uint32:
		return int64(v), true
	case uint64:
		if v > math.MaxInt64 {
			return 0, false
		}
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	}
	return 0, false
}

// isZeroNumber reports whether value is a numeric zero of any supported kind.
func isZeroNumber(value any) bool {
	if n, ok := int64FromValue(value); ok {
		return n == 0
	}
	switch v := value.(type) {
	case float32:
		return v == 0
	case float64:
		return v == 0
	case json.Number:
		f, err := v.Float64()
		return err == nil && f == 0
	}
	return false
}

// parseAttemptTimeout reads an attempt timeout: a positive duration, or "none",
// "0s" or 0 for no attempt timeout.
func parseAttemptTimeout(value any, path string) (time.Duration, error) {
	if isZeroNumber(value) {
		return NoTimeout, nil
	}
	text, ok := value.(string)
	if !ok {
		return 0, fmt.Errorf("%s: must be a duration string, %q, or 0 for no timeout", path, "none")
	}
	text = strings.TrimSpace(text)
	if strings.EqualFold(text, "none") {
		return NoTimeout, nil
	}
	duration, err := time.ParseDuration(text)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q", path, text)
	}
	if duration < 0 {
		return 0, fmt.Errorf("%s: must be positive, or 0 for no timeout", path)
	}
	return duration, nil
}

// durationFromValue parses a positive duration string, for example "30s".
func durationFromValue(value any, path string) (time.Duration, error) {
	valueString, ok := value.(string)
	if !ok {
		return 0, fmt.Errorf("%s: must be a duration string", path)
	}
	valueString = strings.TrimSpace(valueString)
	duration, err := time.ParseDuration(valueString)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q", path, valueString)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s: must be positive", path)
	}
	return duration, nil
}

// parseMaxAttempts reads a whole number of attempts from 1 to maxAttemptsLimit.
// Yaml decodes numbers as int, JSON as float64 or json.Number.
func parseMaxAttempts(value any, path string) (int, error) {
	var n int64
	if integer, ok := int64FromValue(value); ok {
		n = integer
	} else {
		var f float64
		switch v := value.(type) {
		case float32:
			f = float64(v)
		case float64:
			f = v
		case json.Number:
			parsed, err := v.Float64()
			if err != nil {
				return 0, fmt.Errorf("%s: must be a whole number", path)
			}
			f = parsed
		default:
			return 0, fmt.Errorf("%s: must be a whole number", path)
		}
		// NaN fails the Trunc comparison, Inf fails the range check below.
		if f != math.Trunc(f) || f < 1 || f > maxAttemptsLimit {
			return 0, fmt.Errorf("%s: must be a whole number between 1 and %d", path, maxAttemptsLimit)
		}
		n = int64(f)
	}
	if n < 1 || n > maxAttemptsLimit {
		return 0, fmt.Errorf("%s: must be a whole number between 1 and %d", path, maxAttemptsLimit)
	}
	return int(n), nil
}

// parseHeaders reads default headers, where each value is a string or a list of
// strings. An empty list or null means the header is removed.
func parseHeaders(value any, path string) (map[string][]string, error) {
	headersData, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: must be a map", path)
	}

	headers := make(map[string][]string, len(headersData))
	for _, name := range slices.Sorted(maps.Keys(headersData)) {
		if !validHeaderName(name) {
			return nil, fmt.Errorf("%s[%s]: invalid header name", path, name)
		}
		canonical := http.CanonicalHeaderKey(name)
		if _, dup := headers[canonical]; dup {
			return nil, fmt.Errorf("%s[%s]: duplicate header after canonicalization (%s)", path, name, canonical)
		}

		switch v := headersData[name].(type) {
		case nil:
			// A null header, and an empty list below, both remove it.
			headers[canonical] = []string{}
		case string:
			headers[canonical] = []string{v}
		case []string:
			headers[canonical] = slices.Clone(v)
		case []any:
			values := make([]string, 0, len(v))
			for i, item := range v {
				s, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("%s[%s][%d]: value must be a string", path, name, i)
				}
				values = append(values, s)
			}
			headers[canonical] = values
		default:
			return nil, fmt.Errorf("%s[%s]: must be a string or a list of strings", path, name)
		}
		for _, value := range headers[canonical] {
			if !validHeaderValue(value) {
				return nil, fmt.Errorf("%s[%s]: invalid header value", path, name)
			}
		}
	}
	return headers, nil
}
