package nicehttp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPerHostConfigAttemptTimeout checks that a config can turn the attempt
// timeout off with "none" or 0, that unset keys are inherited, and that a
// nonzero DefaultAttemptTimeout does not override an explicit NoTimeout.
func TestPerHostConfigAttemptTimeout(t *testing.T) {
	restore := DefaultAttemptTimeout
	DefaultAttemptTimeout = 5 * time.Second
	defer func() { DefaultAttemptTimeout = restore }()

	config, err := PerHostConfigFromMap(map[string]any{
		"defaults": map[string]any{"attempt_timeout": "30s"},
		"hosts": map[string]any{
			"none.example.com": map[string]any{"attempt_timeout": "none"},
			"zero.example.com": map[string]any{"attempt_timeout": float64(0)},
			"zero-s.example.com": map[string]any{
				"attempt_timeout": "0s",
			},
			"zero-string.example.com": map[string]any{"attempt_timeout": "0"},
			"zero-int.example.com":    map[string]any{"attempt_timeout": 0},
		},
	})
	if err != nil {
		t.Fatalf("while loading config: %s", err)
	}
	runner, err := NewNicePerHostTransport(config)
	if err != nil {
		t.Fatalf("while creating transport: %s", err)
	}

	want := map[string]time.Duration{
		"none.example.com":        NoTimeout,
		"zero.example.com":        NoTimeout,
		"zero-s.example.com":      NoTimeout,
		"zero-string.example.com": NoTimeout,
		"zero-int.example.com":    NoTimeout,
		"other.example.com":       30 * time.Second, // inherited from defaults
	}
	for host, wantTimeout := range want {
		transport, err := runner.ForHost(host)
		if err != nil {
			t.Fatalf("while getting transport for %s: %s", host, err)
		}
		if transport.attemptTimeout != wantTimeout {
			t.Errorf("%s: expected attempt timeout %v, got %v", host, wantTimeout, transport.attemptTimeout)
		}
	}
}

// TestPerHostConfigAttemptTimeoutUnset checks that a nil attempt timeout follows
// DefaultAttemptTimeout, while a pointer to NoTimeout turns it off.
func TestPerHostConfigAttemptTimeoutUnset(t *testing.T) {
	restore := DefaultAttemptTimeout
	DefaultAttemptTimeout = 5 * time.Second
	defer func() { DefaultAttemptTimeout = restore }()

	noTimeout := NoTimeout
	duration := 30 * time.Second
	tests := []struct {
		name    string
		timeout *time.Duration
		want    time.Duration
	}{
		{"unset", nil, 5 * time.Second},
		{"no timeout", &noTimeout, NoTimeout},
		{"duration", &duration, 30 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner, err := NewNicePerHostTransport(PerHostConfig{
				Defaults: &HostConfig{AttemptTimeout: test.timeout},
			})
			if err != nil {
				t.Fatalf("while creating transport: %s", err)
			}
			transport, err := runner.ForHost("example.com")
			if err != nil {
				t.Fatalf("while getting transport: %s", err)
			}
			if transport.attemptTimeout != test.want {
				t.Errorf("expected attempt timeout %v, got %v", test.want, transport.attemptTimeout)
			}
		})
	}
}

// TestPerHostConfigAttemptTimeoutInvalid checks that values which are not a
// duration, "none" or 0 are rejected, including the bool a yaml 1.1 decoder
// produces for "off".
func TestPerHostConfigAttemptTimeoutInvalid(t *testing.T) {
	for _, value := range []any{"off", false, true, "5", -1, -time.Second, []any{1}} {
		_, err := PerHostConfigFromMap(map[string]any{
			"defaults": map[string]any{"attempt_timeout": value},
		})
		if err == nil {
			t.Errorf("expected an error for attempt_timeout %#v", value)
			continue
		}
		t.Logf("attempt_timeout %#v -> %s", value, err)
	}
}

// TestPerHostConfigFromMapInheritance checks that a host inherits the keys it
// does not set, that an empty or nil host entry falls back to the defaults, and
// that inheritance is a snapshot: changing the defaults afterwards does not
// affect hosts that were already parsed.
func TestPerHostConfigFromMapInheritance(t *testing.T) {
	config, err := PerHostConfigFromMap(map[string]any{
		"defaults": map[string]any{
			"attempt_timeout": "30s",
			"max_attempts":    3,
			"default_headers": map[string]any{"X-Default": "yes"},
		},
		"hosts": map[string]any{
			"Sub.EXAMPLE.com.":  map[string]any{"max_attempts": 7},
			"empty.example.com": map[string]any{},
			"nil.example.com":   nil,
		},
	})
	if err != nil {
		t.Fatalf("while loading config: %s", err)
	}

	host := config.Hosts["sub.example.com"] // key is normalized
	if host == nil {
		t.Fatal("expected the config of sub.example.com")
	}
	if host.MaxAttempts != 7 || host.AttemptTimeout == nil || *host.AttemptTimeout != 30*time.Second {
		t.Errorf("expected 7 attempts and a 30s timeout, got %d and %v", host.MaxAttempts, host.AttemptTimeout)
	}
	if host.DefaultHeaders.Get("X-Default") != "yes" {
		t.Errorf("expected inherited headers, got %v", host.DefaultHeaders)
	}

	// Empty and nil entries are still entries, so they are resolved snapshots
	// rather than dynamic hosts.
	for _, name := range []string{"empty.example.com", "nil.example.com"} {
		host, ok := config.Hosts[name]
		if !ok {
			t.Errorf("%s: expected a configured host", name)
			continue
		}
		if host.MaxAttempts != 3 || *host.AttemptTimeout != 30*time.Second {
			t.Errorf("%s: expected the defaults, got %d attempts and %v", name, host.MaxAttempts, host.AttemptTimeout)
		}
	}

	// Inheritance is a snapshot: changing the defaults does not reach parsed
	// hosts, but it does reach hosts without an entry when the transport is built.
	config.Defaults.MaxAttempts = 99
	config.Defaults.DefaultHeaders.Set("X-Default", "changed")
	if host := config.Hosts["empty.example.com"]; host.MaxAttempts != 3 || host.DefaultHeaders.Get("X-Default") != "yes" {
		t.Errorf("expected the parsed snapshot, got %d attempts and %v", host.MaxAttempts, host.DefaultHeaders)
	}
	runner, err := NewNicePerHostTransport(config)
	if err != nil {
		t.Fatalf("while creating transport: %s", err)
	}
	dynamic, err := runner.ForHost("other.example.com")
	if err != nil {
		t.Fatalf("while getting transport: %s", err)
	}
	if dynamic.maxAttempts != 99 {
		t.Errorf("dynamic host should use the changed defaults, got %d attempts", dynamic.maxAttempts)
	}
}

// TestPerHostConfigFromMapBackoff checks that a backoff entry merges onto the
// inherited backoff.
func TestPerHostConfigFromMapBackoff(t *testing.T) {
	config, err := PerHostConfigFromMap(map[string]any{
		"defaults": map[string]any{
			"backoff": map[string]any{"min_wait": "2s", "max_wait": "30s"},
		},
		"hosts": map[string]any{
			"fast.example.com":       map[string]any{"backoff": map[string]any{"min_wait": "10s"}},
			"inheriting.example.com": map[string]any{},
		},
	})
	if err != nil {
		t.Fatalf("while loading config: %s", err)
	}

	backoff := config.Hosts["fast.example.com"].Backoff
	if backoff.MinWait != 10*time.Second || backoff.MaxWait != 30*time.Second {
		t.Errorf("expected min_wait 10s inherited with max_wait 30s, got %v..%v", backoff.MinWait, backoff.MaxWait)
	}
	if backoff.Coeff != config.Defaults.Backoff.Coeff {
		t.Errorf("expected inherited coefficients, got %+v", backoff.Coeff)
	}
}

// TestPerHostConfigFromMapErrors checks that a malformed document is rejected
// with a message naming the offending key.
func TestPerHostConfigFromMapErrors(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
		want string
	}{
		{"unknown top level key", map[string]any{"nope": 1}, "nope: unknown key"},
		{"defaults not a map", map[string]any{"defaults": "x"}, "defaults: must be a map"},
		{"hosts not a map", map[string]any{"hosts": []any{}}, "hosts: must be a map"},
		{"host entry not a map", map[string]any{"hosts": map[string]any{"a.example.com": "x"}}, "hosts[a.example.com]: must be a map"},
		{"host with a port", map[string]any{"hosts": map[string]any{"a.example.com:8080": map[string]any{}}}, "must not contain a port"},
		{"host with no name", map[string]any{"hosts": map[string]any{"": map[string]any{}}}, "must not be empty"},
		{"duplicate host after normalization", map[string]any{"hosts": map[string]any{
			"Example.com":  map[string]any{},
			"example.com.": map[string]any{},
		}}, "collides with"},
		{"duplicate header after canonicalization", map[string]any{"defaults": map[string]any{
			"default_headers": map[string]any{"x-foo": "1", "X-Foo": "2"},
		}}, "duplicate header after canonicalization"},
		{"unknown config key", map[string]any{"defaults": map[string]any{"nope": 1}}, "defaults.nope: unknown key"},
		{"max_attempts not a number", map[string]any{"defaults": map[string]any{"max_attempts": "three"}}, "max_attempts: must be a whole number"},
		{"max_attempts zero", map[string]any{"defaults": map[string]any{"max_attempts": 0}}, "must be a whole number between 1 and"},
		{"max_attempts fractional", map[string]any{"defaults": map[string]any{"max_attempts": 1.5}}, "must be a whole number between 1 and"},
		{"max_attempts too large", map[string]any{"defaults": map[string]any{"max_attempts": maxAttemptsLimit + 1}}, "must be a whole number between 1 and"},
		{"default_headers not a map", map[string]any{"defaults": map[string]any{"default_headers": "x"}}, "default_headers: must be a map"},
		{"header value wrong type", map[string]any{"defaults": map[string]any{
			"default_headers": map[string]any{"X-Foo": 1},
		}}, "[X-Foo]: must be a string or a list of strings"},
		{"header list item wrong type", map[string]any{"defaults": map[string]any{
			"default_headers": map[string]any{"X-Foo": []any{1}},
		}}, "[X-Foo][0]: value must be a string"},
		{"invalid header name", map[string]any{"defaults": map[string]any{
			"default_headers": map[string]any{"Bad Header": "1"},
		}}, "[Bad Header]: invalid header name"},
		{"invalid header value", map[string]any{"defaults": map[string]any{
			"default_headers": map[string]any{"X-Foo": "a\r\nb"},
		}}, "[X-Foo]: invalid header value"},
		{"backoff not a map", map[string]any{"defaults": map[string]any{"backoff": "x"}}, "backoff: must be a map"},
		{"tls is no longer a config key", map[string]any{"defaults": map[string]any{"tls": map[string]any{"insecure_skip_verify": true}}}, "tls: unknown key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := PerHostConfigFromMap(test.data)
			if err == nil {
				t.Fatalf("expected an error for %#v", test.data)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("expected an error containing %q, got %q", test.want, err)
			}
		})
	}
}

// TestPerHostConfigCloneIsDeep checks that a clone shares no state with the
// config it was made from.
func TestPerHostConfigCloneIsDeep(t *testing.T) {
	timeout := 30 * time.Second
	config := HostConfig{
		DefaultHeaders: http.Header{"X-Foo": {"1"}},
		Backoff:        NewBackoff(time.Second, time.Minute, DefaultBackoffCoefficients),
		AttemptTimeout: &timeout,
		MaxAttempts:    3,
	}

	clone := config.Clone()
	config.DefaultHeaders.Set("X-Foo", "2")
	config.Backoff.MinWait = time.Hour
	config.MaxAttempts = 9
	timeout = time.Second

	if got := clone.DefaultHeaders.Get("X-Foo"); got != "1" {
		t.Errorf("expected the cloned headers, got %q", got)
	}
	if clone.Backoff.MinWait != time.Second {
		t.Errorf("expected the cloned backoff, got %v", clone.Backoff.MinWait)
	}
	if clone.MaxAttempts != 3 {
		t.Errorf("expected the cloned max attempts, got %d", clone.MaxAttempts)
	}
	if clone.Backoff == config.Backoff {
		t.Error("expected a cloned backoff, not a shared one")
	}
	if clone.AttemptTimeout == config.AttemptTimeout || *clone.AttemptTimeout != 30*time.Second {
		t.Error("expected a cloned attempt timeout")
	}
}

// TestPerHostConfigHeaderRemoval checks that an empty list or null removes an
// inherited header, that an empty string is sent as an empty value, and that
// other names keep the inherited value.
func TestPerHostConfigHeaderRemoval(t *testing.T) {
	config, err := PerHostConfigFromMap(map[string]any{
		"defaults": map[string]any{"default_headers": map[string]any{
			"X-Keep":  "kept",
			"X-Drop":  "dropped",
			"X-Null":  "nulled",
			"X-Empty": "empty",
		}},
		"hosts": map[string]any{"a.example.com": map[string]any{"default_headers": map[string]any{
			"X-Drop":  []any{},
			"X-Null":  nil,
			"X-Empty": "",
		}}},
	})
	if err != nil {
		t.Fatalf("while loading config: %s", err)
	}
	runner, err := NewNicePerHostTransport(config)
	if err != nil {
		t.Fatalf("while creating transport: %s", err)
	}

	transport, err := runner.ForHost("a.example.com")
	if err != nil {
		t.Fatalf("while getting transport: %s", err)
	}
	headers := transport.defaultHeaders
	if _, ok := headers["X-Drop"]; ok {
		t.Errorf("expected an empty list to remove X-Drop, got %v", headers["X-Drop"])
	}
	if _, ok := headers["X-Null"]; ok {
		t.Errorf("expected null to remove X-Null, got %v", headers["X-Null"])
	}
	if values, ok := headers["X-Empty"]; !ok || len(values) != 1 || values[0] != "" {
		t.Errorf("expected one empty value for X-Empty, got %#v", values)
	}
	if got := headers.Get("X-Keep"); got != "kept" {
		t.Errorf("expected the inherited X-Keep, got %q", got)
	}

	// Another host keeps every inherited header
	other, err := runner.ForHost("b.example.com")
	if err != nil {
		t.Fatalf("while getting transport: %s", err)
	}
	if got := other.defaultHeaders.Get("X-Drop"); got != "dropped" {
		t.Errorf("expected X-Drop on a host that does not remove it, got %q", got)
	}
}

// TestPerHostConfigHostValidation checks that a hand built config gets
// the same host name validation and normalization as config loading.
func TestPerHostConfigHostValidation(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		wantErr string
	}{
		{name: "plain name", host: "example.com"},
		{name: "upper case and spaces", host: "  Example.COM  "},
		{name: "trailing dot", host: "example.com."},
		{name: "IPv6 literal", host: "::1"},
		{name: "bracketed IPv6 literal", host: "[::1]"},
		{name: "name with a port", host: "example.com:8080", wantErr: "must not contain a port"},
		{name: "IPv6 literal with a port", host: "[::1]:8080", wantErr: "must not contain a port"},
		{name: "URL", host: "http://example.com", wantErr: "without scheme, path or spaces"},
		{name: "double dot", host: "example.com..", wantErr: "more than one trailing dot"},
		{name: "empty name", host: "", wantErr: "must not be empty"},
		{name: "only spaces", host: "   ", wantErr: "must not be empty"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner, err := NewNicePerHostTransport(PerHostConfig{
				Hosts: map[string]*HostConfig{test.host: {MaxAttempts: 7}},
			})
			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q", test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("expected an error containing %q, got %q", test.wantErr, err)
				}
				if !strings.Contains(err.Error(), "hosts[") {
					t.Errorf("expected the error to name the host, got %q", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}

			// The host config must be found by the name requests are matched on
			transport, err := runner.ForHost("example.com")
			if err != nil {
				t.Fatalf("while getting transport: %s", err)
			}
			if transport == nil {
				t.Fatal("expected a transport")
			}
		})
	}
}

// TestPerHostConfigHostMatching checks that a hand built host key is
// found by the names a request can carry, so a key with stray spaces or
// brackets does not silently miss.
func TestPerHostConfigHostMatching(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		lookups []string
	}{
		{name: "spaces and case", key: " Example.COM ", lookups: []string{"example.com", "  EXAMPLE.com.  "}},
		{name: "trailing dot", key: "example.com.", lookups: []string{"Example.com"}},
		{name: "bracketed IPv6", key: "[::1]", lookups: []string{"::1"}},
		{name: "plain IPv6", key: "::1", lookups: []string{"[::1]"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner, err := NewNicePerHostTransport(PerHostConfig{
				Hosts: map[string]*HostConfig{test.key: {MaxAttempts: 7}},
			})
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}

			// The lookup goes through the transport, which is where matching happens
			matching, err := runner.ForHost(test.lookups[0])
			if err != nil {
				t.Fatalf("while getting transport: %s", err)
			}
			if matching == nil {
				t.Fatal("expected a transport")
			}
			if matching.maxAttempts != 7 {
				t.Errorf("expected the config of %q to be used, got maxAttempts %d", test.key, matching.maxAttempts)
			}

			transport, err := runner.ForHost("other.example.com")
			if err != nil {
				t.Fatalf("while getting transport: %s", err)
			}
			if transport == matching {
				t.Error("expected a different transport for a host without config")
			}
			if transport.maxAttempts != DefaultMaxAttempts {
				t.Errorf("expected the defaults for an unconfigured host, got maxAttempts %d", transport.maxAttempts)
			}
		})
	}
}

// TestPerHostConfigHostDuplicates checks that two keys that collide after
// normalization are rejected, with both spellings named.
func TestPerHostConfigHostDuplicates(t *testing.T) {
	_, err := NewNicePerHostTransport(PerHostConfig{
		Hosts: map[string]*HostConfig{
			"example.com":      {MaxAttempts: 3},
			"  EXAMPLE.com.  ": {MaxAttempts: 4},
		},
	})
	if err == nil {
		t.Fatal("expected an error for hosts that collide after normalization")
	}
	if !strings.Contains(err.Error(), "collides with") {
		t.Errorf("expected a collision error, got %q", err)
	}
	for _, name := range []string{"example.com", "  EXAMPLE.com.  "} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("expected the error to name %q, got %q", name, err)
		}
	}
}

// TestNewNicePerHostTransportSharesDownstream checks that every host transport,
// configured or dynamic, uses the downstream set on the config.
func TestNewNicePerHostTransportSharesDownstream(t *testing.T) {
	downstream := &http.Transport{}
	config, err := PerHostConfigFromMap(map[string]any{
		"hosts": map[string]any{
			"a.example.com": map[string]any{"max_attempts": 2},
			"b.example.com": map[string]any{"max_attempts": 3},
		},
	})
	if err != nil {
		t.Fatalf("while loading config: %s", err)
	}
	config.Downstream = downstream
	transport, err := NewNicePerHostTransport(config)
	if err != nil {
		t.Fatalf("while creating transport: %s", err)
	}

	for _, host := range []string{"a.example.com", "b.example.com", "dynamic.example.com"} {
		hostTransport, err := transport.ForHost(host)
		if err != nil {
			t.Fatalf("while getting transport for %s: %s", host, err)
		}
		if got := hostTransport.downstream(); got != downstream {
			t.Errorf("%s: expected the shared downstream transport, got %v", host, got)
		}
	}
}

// TestNewNicePerHostTransportDefaultDownstream checks that a nil downstream
// falls back to http.DefaultTransport.
func TestNewNicePerHostTransportDefaultDownstream(t *testing.T) {
	transport, err := NewNicePerHostTransport(PerHostConfig{})
	if err != nil {
		t.Fatalf("while creating transport: %s", err)
	}

	hostTransport, err := transport.ForHost("example.com")
	if err != nil {
		t.Fatalf("while getting transport: %s", err)
	}
	if got := hostTransport.downstream(); got != http.DefaultTransport {
		t.Errorf("expected http.DefaultTransport, got %v", got)
	}
}

// TestNicePerHostTransportEvictsLeastRecentlyUsed checks that the cache stops at
// maxDynamicHosts, and that the drop is the host looked up longest ago.
func TestNicePerHostTransportEvictsLeastRecentlyUsed(t *testing.T) {
	runner, err := NewNicePerHostTransport(PerHostConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	first, err := runner.ForHost("h0.example.com")
	if err != nil {
		t.Fatalf("while getting transport: %s", err)
	}
	var second *NiceTransport
	// h0 plus this loop is exactly the bound, so nothing is evicted yet.
	for i := 1; i < maxDynamicHosts; i++ {
		transport, err := runner.ForHost(fmt.Sprintf("h%d.example.com", i))
		if err != nil {
			t.Fatalf("while getting transport for h%d: %s", i, err)
		}
		if i == 1 {
			second = transport
		}
	}
	if got := len(runner.dynamic); got != maxDynamicHosts {
		t.Fatalf("cached %d dynamic hosts, want %d", got, maxDynamicHosts)
	}

	// h0 is the most recently used now, so h1 is the entry to drop.
	if _, err := runner.ForHost("h0.example.com"); err != nil {
		t.Fatalf("while getting transport: %s", err)
	}
	if _, err := runner.ForHost("h-extra.example.com"); err != nil {
		t.Fatalf("while getting transport: %s", err)
	}

	if _, ok := runner.transports["h1.example.com"]; ok {
		t.Error("expected the least recently used host to be evicted")
	}
	if _, ok := runner.dynamic["h1.example.com"]; ok {
		t.Error("expected the evicted host to leave the dynamic index")
	}
	entry, ok := runner.transports["h0.example.com"]
	if !ok {
		t.Fatal("expected the recently used host to stay cached")
	}
	if entry.transport != first {
		t.Error("expected the recently used host to keep its transport")
	}
	if _, ok := runner.transports["h-extra.example.com"]; !ok {
		t.Error("expected the new host to be cached")
	}
	if got := len(runner.dynamic); got != maxDynamicHosts {
		t.Errorf("cached %d dynamic hosts, want %d", got, maxDynamicHosts)
	}

	// An evicted host gets a new transport on its next request.
	fresh, err := runner.ForHost("h1.example.com")
	if err != nil {
		t.Fatalf("while getting transport: %s", err)
	}
	if fresh == second {
		t.Error("expected a new transport for an evicted host")
	}
}

// TestNicePerHostTransportNeverEvictsConfiguredHosts checks that the bound only
// counts and drops hosts without a config entry.
func TestNicePerHostTransportNeverEvictsConfiguredHosts(t *testing.T) {
	runner, err := NewNicePerHostTransport(PerHostConfig{
		Hosts: map[string]*HostConfig{"configured.example.com": {MaxAttempts: 7}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	// The configured host is looked up first, so it is the oldest entry and would
	// be the first drop if it were counted.
	configured, err := runner.ForHost("configured.example.com")
	if err != nil {
		t.Fatalf("while getting transport: %s", err)
	}
	for i := range maxDynamicHosts + 1 {
		if _, err := runner.ForHost(fmt.Sprintf("h%d.example.com", i)); err != nil {
			t.Fatalf("while getting transport for h%d: %s", i, err)
		}
	}

	entry, ok := runner.transports["configured.example.com"]
	if !ok {
		t.Fatal("expected the configured host to stay cached")
	}
	if entry.transport != configured {
		t.Error("expected the configured host to keep its transport")
	}
	if _, ok := runner.dynamic["configured.example.com"]; ok {
		t.Error("expected the configured host to stay out of the dynamic index")
	}
	if got := len(runner.dynamic); got != maxDynamicHosts {
		t.Errorf("cached %d dynamic transports, want %d", got, maxDynamicHosts)
	}
}

// TestNicePerHostTransportConcurrentEviction checks that the bound holds when
// several goroutines see new hosts at once.
func TestNicePerHostTransportConcurrentEviction(t *testing.T) {
	runner, err := NewNicePerHostTransport(PerHostConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				host := fmt.Sprintf("w%d-h%d.example.com", worker, i)
				if _, err := runner.ForHost(host); err != nil {
					t.Errorf("while getting transport for %s: %s", host, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := len(runner.dynamic); got > maxDynamicHosts {
		t.Errorf("cached %d dynamic transports, want at most %d", got, maxDynamicHosts)
	}
}

// TestPerHostConfigFromMapNumbers checks numeric kinds, json.Number, and
// whitespace around duration strings.
func TestPerHostConfigFromMapNumbers(t *testing.T) {
	config, err := PerHostConfigFromMap(map[string]any{
		"defaults": map[string]any{
			"max_attempts":    json.Number("3"),
			"attempt_timeout": " 30s ",
		},
	})
	if err != nil {
		t.Fatalf("while parsing: %s", err)
	}
	if config.Defaults.MaxAttempts != 3 {
		t.Errorf("max attempts = %d, want 3", config.Defaults.MaxAttempts)
	}
	if config.Defaults.AttemptTimeout == nil || *config.Defaults.AttemptTimeout != 30*time.Second {
		t.Errorf("attempt timeout = %v, want 30s", config.Defaults.AttemptTimeout)
	}

	if _, err := PerHostConfigFromMap(map[string]any{
		"defaults": map[string]any{"max_attempts": json.Number("3.0")},
	}); err != nil {
		t.Errorf("json.Number 3.0 should be accepted: %s", err)
	}

	for name, value := range map[string]any{
		"int32 zero":       int32(0),
		"uint64 zero":      uint64(0),
		"json.Number zero": json.Number("0"),
		"json float zero":  json.Number("0.0"),
	} {
		config, err := PerHostConfigFromMap(map[string]any{
			"defaults": map[string]any{"attempt_timeout": value},
		})
		if err != nil {
			t.Errorf("%s: unexpected error: %s", name, err)
			continue
		}
		if config.Defaults.AttemptTimeout == nil || *config.Defaults.AttemptTimeout != NoTimeout {
			t.Errorf("%s: expected NoTimeout, got %v", name, config.Defaults.AttemptTimeout)
		}
	}
}

// TestPerHostConfigTypedValidation checks that hand built configs are rejected
// like parsed ones.
func TestPerHostConfigTypedValidation(t *testing.T) {
	negativeTimeout := -time.Second
	tests := []struct {
		name   string
		config PerHostConfig
		want   string
	}{
		{"negative attempts", PerHostConfig{Defaults: &HostConfig{MaxAttempts: -1}}, "must not be negative"},
		{"attempts above limit", PerHostConfig{Defaults: &HostConfig{MaxAttempts: maxAttemptsLimit + 1}}, "between 1 and"},
		{"negative timeout", PerHostConfig{Defaults: &HostConfig{AttemptTimeout: &negativeTimeout}}, "must not be negative"},
		{"duplicate headers", PerHostConfig{Hosts: map[string]*HostConfig{
			"a.example.com": {DefaultHeaders: http.Header{"x-foo": {"1"}, "X-Foo": {"2"}}},
		}}, "duplicates"},
		{"invalid header name", PerHostConfig{Defaults: &HostConfig{
			DefaultHeaders: http.Header{"Bad Header": {"1"}},
		}}, "invalid header name"},
		{"invalid header value", PerHostConfig{Defaults: &HostConfig{
			DefaultHeaders: http.Header{"X-A": {"a\r\nb"}},
		}}, "invalid value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewNicePerHostTransport(test.config)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("expected an error containing %q, got %q", test.want, err)
			}
		})
	}
}

type closeSpy struct {
	http.RoundTripper
	closed bool
}

func (c *closeSpy) CloseIdleConnections() { c.closed = true }

// TestNicePerHostTransportCloseIdleConnections checks that closing idle
// connections reaches the shared downstream.
func TestNicePerHostTransportCloseIdleConnections(t *testing.T) {
	spy := &closeSpy{RoundTripper: http.DefaultTransport}
	runner, err := NewNicePerHostTransport(PerHostConfig{Downstream: spy})
	if err != nil {
		t.Fatalf("while creating transport: %s", err)
	}
	runner.CloseIdleConnections()
	if !spy.closed {
		t.Error("expected CloseIdleConnections to reach the downstream")
	}

	// The zero value must not panic.
	(&NicePerHostTransport{}).CloseIdleConnections()
}

// TestNicePerHostTransportRejectsInvalidHosts checks that ForHost validates
// before caching.
func TestNicePerHostTransportRejectsInvalidHosts(t *testing.T) {
	runner, err := NewNicePerHostTransport(PerHostConfig{})
	if err != nil {
		t.Fatalf("while creating transport: %s", err)
	}
	for _, host := range []string{"", "http://example.com", "a.example.com:80", "[example.com"} {
		if _, err := runner.ForHost(host); err == nil {
			t.Errorf("ForHost(%q) should fail", host)
		}
	}
	if len(runner.transports) != 0 || len(runner.dynamic) != 0 {
		t.Errorf("invalid hosts were cached: %d transports, %d dynamic", len(runner.transports), len(runner.dynamic))
	}
}
