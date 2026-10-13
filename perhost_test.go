package nicehttp

import (
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
		config, err := manager.ForHost(host)
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
