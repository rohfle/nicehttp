package nicehttp

import (
	"math"
	"strings"
	"testing"
	"time"
)

// TestNewBackoffFromMap checks the keys that are accepted, including the values
// that are taken from the base backoff when they are absent.
func TestNewBackoffFromMap(t *testing.T) {
	base := NewBackoff(2*time.Second, 30*time.Second, BackoffCoefficients{Success: 0.8, Fail: 1.2, Jitter: 0.1})

	tests := []struct {
		name string
		data map[string]any
		base *Backoff
		want Backoff
	}{
		{
			name: "no data, no base",
			want: *DefaultBackoff,
		},
		{
			name: "no data follows the base",
			data: map[string]any{},
			base: base,
			want: *base,
		},
		{
			name: "every key",
			data: map[string]any{
				"min_wait":     "1s",
				"max_wait":     "2s",
				"coefficients": map[string]any{"success": 0.6, "fail": 1.4, "jitter": 0.2},
			},
			want: Backoff{MinWait: time.Second, MaxWait: 2 * time.Second, Coeff: BackoffCoefficients{0.6, 1.4, 0.2}},
		},
		{
			name: "partial coefficients",
			data: map[string]any{"coefficients": map[string]any{"fail": 2.0}},
			base: base,
			want: Backoff{MinWait: 2 * time.Second, MaxWait: 30 * time.Second, Coeff: BackoffCoefficients{0.8, 2.0, 0.1}},
		},
		{
			name: "min_wait only",
			data: map[string]any{"min_wait": "5s"},
			base: base,
			want: Backoff{MinWait: 5 * time.Second, MaxWait: 30 * time.Second, Coeff: base.Coeff},
		},
		{
			name: "yaml int coefficient and zero jitter",
			data: map[string]any{"coefficients": map[string]any{"success": 1, "jitter": 0}},
			want: Backoff{MinWait: time.Second, MaxWait: 120 * time.Second, Coeff: BackoffCoefficients{1, 1.5, 0}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NewBackoffFromMap(test.data, test.base)
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if got.MinWait != test.want.MinWait || got.MaxWait != test.want.MaxWait || got.Coeff != test.want.Coeff {
				t.Errorf("expected %+v, got %+v", test.want, *got)
			}
		})
	}
}

// TestNewBackoffFromMapErrors checks that bad keys and values are rejected with
// a message naming the offending key.
func TestNewBackoffFromMapErrors(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
		want string
	}{
		{"unknown key", map[string]any{"nope": "1s"}, "nope: unknown key"},
		{"min_wait not a string", map[string]any{"min_wait": 5}, "min_wait: must be a duration string"},
		{"min_wait invalid", map[string]any{"min_wait": "1 fortnight"}, "min_wait: invalid duration"},
		{"min_wait zero", map[string]any{"min_wait": "0s"}, "min_wait: must be positive"},
		{"max_wait below min_wait", map[string]any{"max_wait": "500ms"}, "max_wait: must not be less than min_wait"},
		{"coefficients not a map", map[string]any{"coefficients": "x"}, "coefficients: must be a map"},
		{"unknown coefficient", map[string]any{"coefficients": map[string]any{"multiplier": 2.0}}, "coefficients.multiplier: unknown key"},
		{"success not a number", map[string]any{"coefficients": map[string]any{"success": "0.5"}}, "coefficients.success: must be a number"},
		{"success not positive", map[string]any{"coefficients": map[string]any{"success": 0.0}}, "success and fail must be positive"},
		{"fail negative", map[string]any{"coefficients": map[string]any{"fail": -1.0}}, "success and fail must be positive"},
		{"fail not finite", map[string]any{"coefficients": map[string]any{"fail": math.Inf(1)}}, "coefficients.fail: must be finite"},
		{"jitter out of range", map[string]any{"coefficients": map[string]any{"jitter": 1.5}}, "coefficients.jitter: must be between 0 and 1"},
		{"jitter negative", map[string]any{"coefficients": map[string]any{"jitter": -0.1}}, "coefficients.jitter: must be between 0 and 1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewBackoffFromMap(test.data, nil)
			if err == nil {
				t.Fatalf("expected an error for %#v", test.data)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("expected an error containing %q, got %q", test.want, err)
			}
		})
	}
}

// TestBackoffPreviewWaitBoundsUpdate checks that previewWait is never shorter
// than the wait Update actually sets, because the retry feasibility check
// refuses to wait only when it cannot fit before the deadline.
func TestBackoffPreviewWaitBoundsUpdate(t *testing.T) {
	for _, fail := range []bool{false, true} {
		for range 100 {
			backoff := NewBackoff(time.Second, 2*time.Second, DefaultBackoffCoefficients)
			preview := backoff.previewWait(fail, 0)
			actual := backoff.Update(fail, 0)
			if actual > preview {
				t.Fatalf("fail=%v: Update set %s, above the previewed %s", fail, actual, preview)
			}
		}
	}

	// A Retry-After override is deterministic, so the preview matches exactly
	backoff := NewBackoff(time.Millisecond, time.Millisecond, DefaultBackoffCoefficients)
	preview := backoff.previewWait(true, 90*time.Second)
	if actual := backoff.Update(true, 90*time.Second); actual != preview {
		t.Fatalf("override: Update set %s, previewed %s", actual, preview)
	}
}

// TestFloatFromValue checks the numeric kinds accepted by NewBackoffFromMap,
// including the finite check.
func TestFloatFromValue(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		want    float64
		wantErr string
	}{
		{name: "float64", value: 1.5, want: 1.5},
		{name: "int", value: 2, want: 2},
		{name: "int64", value: int64(3), want: 3},
		{name: "nan", value: math.NaN(), wantErr: "must be finite"},
		{name: "positive infinity", value: math.Inf(1), wantErr: "must be finite"},
		{name: "negative infinity", value: math.Inf(-1), wantErr: "must be finite"},
		{name: "string", value: "1", wantErr: "must be a number"},
		{name: "nil", value: nil, wantErr: "must be a number"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := floatFromValue(test.value, "coefficients.jitter")
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("expected an error containing %q, got %v", test.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if got != test.want {
				t.Errorf("expected %v, got %v", test.want, got)
			}
		})
	}
}

// TestBackoffRetryAfterClamps checks that negative and past-date Retry-After
// values fall back to MinWait, and a huge value is capped.
func TestBackoffRetryAfterClamps(t *testing.T) {
	const (
		minWait = time.Second
		maxWait = time.Minute
	)
	tests := []struct {
		name string
		wait time.Duration
		want time.Duration
	}{
		{name: "negative", wait: -5 * time.Second, want: minWait},
		{name: "past date", wait: time.Until(time.Now().Add(-2 * time.Hour)), want: minWait},
		{name: "huge", wait: 100000 * time.Hour, want: maxRetryAfterWait},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backoff := NewBackoff(minWait, maxWait, DefaultBackoffCoefficients)
			if got := backoff.Update(true, test.wait); got != test.want {
				t.Errorf("Update: expected %s, got %s", test.want, got)
			}
			preview := NewBackoff(minWait, maxWait, DefaultBackoffCoefficients)
			if got := preview.previewWait(true, test.wait); got != test.want {
				t.Errorf("previewWait: expected %s, got %s", test.want, got)
			}
		})
	}
}
