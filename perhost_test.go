package nicehttp

import (
	"crypto/tls"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestPerHostConfigAttemptTimeout checks that a config can turn the attempt
// timeout off with "none" or 0, that unlset keys are inherited, and that a
// nonzero DefaultAttemptTimeout does not override an explicit NoTimeout.
func TestPerHostConfigAttemptTimeout(t *testing.T) {
	restore := DefaultAttemptTimeout
	DefaultAttemptTimeout = 5 * time.Second
	defer func() { DefaultAttemptTimeout = restore }()

	manager, err := NewPerHostConfigManagerFromMap(map[string]any{
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

	want := map[string]time.Duration{
		"none.example.com":        NoTimeout,
		"zero.example.com":        NoTimeout,
		"zero-s.example.com":      NoTimeout,
		"zero-string.example.com": NoTimeout,
		"zero-int.example.com":    NoTimeout,
		"other.example.com":       30 * time.Second, // inherited from defaults
	}
	for host, wantTimeout := range want {
		config, _, err := manager.ForHost(host)
		if err != nil {
			t.Fatalf("while getting config for %s: %s", host, err)
		}
		transport, err := config.CreateNiceTransport()
		if err != nil {
			t.Fatalf("while creating transport for %s: %s", host, err)
		}
		if transport.attemptTimeout != wantTimeout {
			t.Errorf("%s: expected attempt timeout %v, got %v", host, wantTimeout, transport.attemptTimeout)
		}
	}
}

// TestPerHostConfigAttemptTimeoutUnset checks that a negative attempt timeout
// means unset, so it follows DefaultAttemptTimeout, while 0 (NoTimeout) turns
// the attempt timeout off.
func TestPerHostConfigAttemptTimeoutUnset(t *testing.T) {
	restore := DefaultAttemptTimeout
	DefaultAttemptTimeout = 5 * time.Second
	defer func() { DefaultAttemptTimeout = restore }()

	tests := []struct {
		name  string
		value time.Duration
		want  time.Duration
	}{
		{"unset", -1, 5 * time.Second},
		{"no timeout", NoTimeout, NoTimeout},
		{"duration", 30 * time.Second, 30 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := PerHostConfig{AttemptTimeout: test.value}
			transport, err := config.CreateNiceTransport()
			if err != nil {
				t.Fatalf("while creating transport: %s", err)
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
		_, err := NewPerHostConfigManagerFromMap(map[string]any{
			"defaults": map[string]any{"attempt_timeout": value},
		})
		if err == nil {
			t.Errorf("expected an error for attempt_timeout %#v", value)
			continue
		}
		t.Logf("attempt_timeout %#v -> %s", value, err)
	}
}

// TestPerHostConfigFromMapInheritance checks that a host inherits the keys it does
// not set, that an empty or nil host entry falls back to the defaults, and that the
// inheritance is a snapshot: changing the defaults afterwards does not affect hosts
// that were already parsed.
func TestPerHostConfigFromMapInheritance(t *testing.T) {
	manager, err := NewPerHostConfigManagerFromMap(map[string]any{
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

	host, usedDefaults, err := manager.ForHost("sub.example.com") // key is normalized
	if err != nil {
		t.Fatalf("while getting config: %s", err)
	}
	if usedDefaults {
		t.Error("expected the config of sub.example.com, not the defaults")
	}
	if host.MaxAttempts != 7 || host.AttemptTimeout != 30*time.Second {
		t.Errorf("expected 7 attempts and a 30s timeout, got %d and %v", host.MaxAttempts, host.AttemptTimeout)
	}
	if host.DefaultHeaders.Get("X-Default") != "yes" || host.DefaultHeaders.Get("User-Agent") != DefaultUserAgent {
		t.Errorf("expected inherited headers, got %v", host.DefaultHeaders)
	}

	// An empty entry, a nil entry and an unconfigured host all follow the defaults
	for _, name := range []string{"empty.example.com", "nil.example.com", "other.example.com"} {
		config, usedDefaults, err := manager.ForHost(name)
		if err != nil {
			t.Fatalf("while getting config for %s: %s", name, err)
		}
		if config.MaxAttempts != 3 || config.AttemptTimeout != 30*time.Second {
			t.Errorf("%s: expected the defaults, got %d attempts and %v", name, config.MaxAttempts, config.AttemptTimeout)
		}
		// An empty or nil entry is still an entry for that host, so only a host
		// that has none reports that the defaults were used.
		if want := name == "other.example.com"; usedDefaults != want {
			t.Errorf("%s: usedDefaults = %v, want %v", name, usedDefaults, want)
		}
	}

	// Inheritance is a snapshot: changing the defaults does not reach parsed hosts
	manager.Defaults.MaxAttempts = 99
	manager.Defaults.DefaultHeaders.Set("X-Default", "changed")
	config, _, err := manager.ForHost("empty.example.com")
	if err != nil {
		t.Fatalf("while getting config: %s", err)
	}
	if config.MaxAttempts != 3 || config.DefaultHeaders.Get("X-Default") != "yes" {
		t.Errorf("expected the parsed snapshot, got %d attempts and %v", config.MaxAttempts, config.DefaultHeaders)
	}
}

// TestPerHostConfigFromMapTLSAndBackoff checks that tls: null drops the inherited
// tls config, and that a backoff entry merges onto the inherited backoff.
func TestPerHostConfigFromMapTLSAndBackoff(t *testing.T) {
	manager, err := NewPerHostConfigManagerFromMap(map[string]any{
		"defaults": map[string]any{
			"tls":     map[string]any{"insecure_skip_verify": true},
			"backoff": map[string]any{"min_wait": "2s", "max_wait": "30s"},
		},
		"hosts": map[string]any{
			"plain.example.com":      map[string]any{"tls": nil},
			"fast.example.com":       map[string]any{"backoff": map[string]any{"min_wait": "10s"}},
			"inheriting.example.com": map[string]any{},
		},
	})
	if err != nil {
		t.Fatalf("while loading config: %s", err)
	}

	if !manager.Defaults.TLS.InsecureSkipVerify {
		t.Error("expected the defaults to skip verification")
	}
	if got := manager.Hosts["plain.example.com"].TLS; got != nil {
		t.Errorf("expected tls: null to drop the inherited tls config, got %v", got)
	}
	if got := manager.Hosts["inheriting.example.com"].TLS; got == nil || !got.InsecureSkipVerify {
		t.Errorf("expected the inherited tls config, got %v", got)
	}

	backoff := manager.Hosts["fast.example.com"].Backoff
	if backoff.MinWait != 10*time.Second || backoff.MaxWait != 30*time.Second {
		t.Errorf("expected min_wait 10s inherited with max_wait 30s, got %v..%v", backoff.MinWait, backoff.MaxWait)
	}
	if backoff.Coeff != manager.Defaults.Backoff.Coeff {
		t.Errorf("expected inherited coefficients, got %+v", backoff.Coeff)
	}
}

// TestPerHostConfigFromMapErrors checks that a malformed document is rejected with
// a message naming the offending key.
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
		{"host with a port", map[string]any{"hosts": map[string]any{"a.example.com:8080": map[string]any{}}}, "hostname must not contain a port"},
		{"host with no name", map[string]any{"hosts": map[string]any{"": map[string]any{}}}, "hostname must not be empty"},
		{"duplicate host after normalization", map[string]any{"hosts": map[string]any{
			"Example.com":  map[string]any{},
			"example.com.": map[string]any{},
		}}, "duplicate of another host after normalization"},
		{"duplicate header after canonicalization", map[string]any{"defaults": map[string]any{
			"default_headers": map[string]any{"x-foo": "1", "X-Foo": "2"},
		}}, "duplicate header after canonicalization"},
		{"unknown config key", map[string]any{"defaults": map[string]any{"nope": 1}}, "defaults.nope: unknown key"},
		{"max_attempts not a number", map[string]any{"defaults": map[string]any{"max_attempts": "three"}}, "max_attempts: must be a whole number"},
		{"max_attempts zero", map[string]any{"defaults": map[string]any{"max_attempts": 0}}, "must be a whole number between 1 and"},
		{"max_attempts fractional", map[string]any{"defaults": map[string]any{"max_attempts": 1.5}}, "must be a whole number between 1 and"},
		{"default_headers not a map", map[string]any{"defaults": map[string]any{"default_headers": "x"}}, "default_headers: must be a map"},
		{"header value wrong type", map[string]any{"defaults": map[string]any{
			"default_headers": map[string]any{"X-Foo": 1},
		}}, "[X-Foo]: must be a string or a list of strings"},
		{"header list item wrong type", map[string]any{"defaults": map[string]any{
			"default_headers": map[string]any{"X-Foo": []any{1}},
		}}, "[X-Foo]: value must be a string"},
		{"backoff not a map", map[string]any{"defaults": map[string]any{"backoff": "x"}}, "backoff: must be a map"},
		{"tls not a map", map[string]any{"defaults": map[string]any{"tls": "x"}}, "tls: must be a map"},
		{"tls unknown key", map[string]any{"defaults": map[string]any{"tls": map[string]any{"ca_file": "x"}}}, "tls.ca_file: unknown key"},
		{"tls wrong type", map[string]any{"defaults": map[string]any{"tls": map[string]any{"insecure_skip_verify": "yes"}}}, "tls.insecure_skip_verify: must be a boolean"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewPerHostConfigManagerFromMap(test.data)
			if err == nil {
				t.Fatalf("expected an error for %#v", test.data)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("expected an error containing %q, got %q", test.want, err)
			}
		})
	}
}

// TestPerHostConfigCloneIsDeep checks that a clone shares no state with the config
// it was made from.
func TestPerHostConfigCloneIsDeep(t *testing.T) {
	config := PerHostConfig{
		DefaultHeaders: http.Header{"X-Foo": {"1"}},
		Backoff:        NewBackoff(time.Second, time.Minute, DefaultBackoffCoefficients),
		TLS:            &tls.Config{},
		MaxAttempts:    3,
	}

	clone := config.Clone()
	config.DefaultHeaders.Set("X-Foo", "2")
	config.Backoff.MinWait = time.Hour
	config.TLS.InsecureSkipVerify = true
	config.MaxAttempts = 9

	if got := clone.DefaultHeaders.Get("X-Foo"); got != "1" {
		t.Errorf("expected the cloned headers, got %q", got)
	}
	if clone.Backoff.MinWait != time.Second {
		t.Errorf("expected the cloned backoff, got %v", clone.Backoff.MinWait)
	}
	if clone.TLS.InsecureSkipVerify {
		t.Error("expected the cloned tls config")
	}
	if clone.MaxAttempts != 3 {
		t.Errorf("expected the cloned max attempts, got %d", clone.MaxAttempts)
	}
	if clone.Backoff == config.Backoff {
		t.Error("expected a cloned backoff, not a shared one")
	}
}

// TestPerHostConfigHeaderRemoval checks that an empty list or null removes an
// inherited header, that an empty string is sent as an empty value, and that
// other names keep the inherited value.
func TestPerHostConfigHeaderRemoval(t *testing.T) {
	manager, err := NewPerHostConfigManagerFromMap(map[string]any{
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

	config, _, err := manager.ForHost("a.example.com")
	if err != nil {
		t.Fatalf("while getting config: %s", err)
	}
	transport, err := config.CreateNiceTransport()
	if err != nil {
		t.Fatalf("while creating transport: %s", err)
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
	other, _, err := manager.ForHost("b.example.com")
	if err != nil {
		t.Fatalf("while getting config: %s", err)
	}
	if got := other.DefaultHeaders.Get("X-Drop"); got != "dropped" {
		t.Errorf("expected X-Drop on a host that does not remove it, got %q", got)
	}
}

// TestPerHostConfigManagerHostValidation checks that a hand built manager, whose
// Hosts map is exported, gets the same host name validation and normalization as
// config loading.
func TestPerHostConfigManagerHostValidation(t *testing.T) {
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
		{name: "empty name", host: "", wantErr: "must not be empty"},
		{name: "only spaces", host: "   ", wantErr: "must not be empty"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := &PerHostConfigManager{
				Defaults: DefaultPerHostConfig.Clone(),
				Hosts:    map[string]*PerHostConfig{test.host: {MaxAttempts: 7}},
			}
			runner, err := manager.CreateNicePerHostTransport()
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
			host, err := runner.ForHost("example.com")
			if err != nil {
				t.Fatalf("while getting transport: %s", err)
			}
			if host == nil {
				t.Fatal("expected a transport")
			}
		})
	}
}

// TestPerHostConfigManagerHostMatching checks that a hand built host key is found
// by the names a request can carry, so a key with stray spaces or brackets does
// not silently miss.
func TestPerHostConfigManagerHostMatching(t *testing.T) {
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
			manager := &PerHostConfigManager{
				Defaults: DefaultPerHostConfig.Clone(),
				Hosts:    map[string]*PerHostConfig{test.key: {MaxAttempts: 7}},
			}
			runner, err := manager.CreateNicePerHostTransport()
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
			if transport.maxAttempts != DefaultPerHostConfig.MaxAttempts {
				t.Errorf("expected the defaults for an unconfigured host, got maxAttempts %d", transport.maxAttempts)
			}
		})
	}
}

// TestPerHostConfigManagerHostDuplicates checks that two keys that collide after
// normalization are rejected, with the host named.
func TestPerHostConfigManagerHostDuplicates(t *testing.T) {
	manager := &PerHostConfigManager{
		Defaults: DefaultPerHostConfig.Clone(),
		Hosts: map[string]*PerHostConfig{
			"example.com":      {MaxAttempts: 3},
			"  EXAMPLE.com.  ": {MaxAttempts: 4},
		},
	}
	_, err := manager.CreateNicePerHostTransport()
	if err == nil {
		t.Fatal("expected an error for hosts that collide after normalization")
	}
	if !strings.Contains(err.Error(), "duplicate host after normalization") {
		t.Errorf("expected a duplicate error, got %q", err)
	}
}
