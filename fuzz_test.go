package nicehttp

import (
	"encoding/json"
	"testing"
)

// FuzzPerHostConfigFromMap checks that arbitrary decoded config never panics
// and that every accepted config stays within the documented bounds.
func FuzzPerHostConfigFromMap(f *testing.F) {
	for _, seed := range []string{
		`{}`,
		`null`,
		`{"defaults":null}`,
		`{"defaults":{"max_attempts":3,"attempt_timeout":"30s"},"hosts":{"example.com":{"max_attempts":5}}}`,
		`{"hosts":{"[::1]":null}}`,
		`{"hosts":{"example.com":{"default_headers":{"X-Foo":["a","b"]}}}}`,
		`{"hosts":{"bad host":{}}}`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data string) {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(data), &decoded); err != nil {
			return
		}
		config, err := PerHostConfigFromMap(decoded)
		if err != nil {
			return
		}

		check := func(path string, cfg *HostConfig) {
			if cfg == nil {
				t.Fatalf("%s: nil host config", path)
			}
			if cfg.MaxAttempts < 0 || cfg.MaxAttempts > maxAttemptsLimit {
				t.Fatalf("%s: MaxAttempts %d out of range", path, cfg.MaxAttempts)
			}
			if cfg.AttemptTimeout != nil && *cfg.AttemptTimeout < 0 {
				t.Fatalf("%s: negative AttemptTimeout", path)
			}
		}
		if config.Defaults != nil {
			check("defaults", config.Defaults)
		}
		for host, cfg := range config.Hosts {
			check(host, cfg)
		}
	})
}
