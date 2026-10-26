package nicehttp

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"time"
)

// Backoff is the exponential wait between request attempts.
type Backoff struct {
	last time.Time
	wait time.Duration

	MinWait time.Duration       `json:"min_wait"`
	MaxWait time.Duration       `json:"max_wait"`
	Coeff   BackoffCoefficients `json:"coefficients"`
}

// BackoffCoefficients scale a Backoff's wait: Success and Fail are the multipliers applied
// after a success or a failure, and Jitter is how much the wait is randomised.
type BackoffCoefficients struct {
	Success float64 `json:"success"`
	Fail    float64 `json:"fail"`
	Jitter  float64 `json:"jitter"`
}

// DefaultBackoffCoefficients are the backoff coefficients that will be used by default
var DefaultBackoffCoefficients = BackoffCoefficients{
	Success: 0.5,
	Fail:    1.5,
	Jitter:  0.3,
}

// DefaultBackoff is the default backoff - 1s growing to 120s with default coefficients.
var DefaultBackoff = NewBackoff(1*time.Second, 120*time.Second, DefaultBackoffCoefficients)

// maxRetryAfterWait caps a server supplied Retry-After wait, so a request for
// days cannot stall the host.
const maxRetryAfterWait = 24 * time.Hour

// NewBackoffFromMap builds a Backoff from a config map. Keys that are absent
// are taken from base, or from DefaultBackoff when base is nil.
//
// Accepted keys: min_wait and max_wait (duration strings), and coefficients,
// a map accepting success, fail and jitter (numbers).
func NewBackoffFromMap(data map[string]any, base *Backoff) (*Backoff, error) {
	if base == nil {
		base = DefaultBackoff
	}
	minWait, maxWait, coeff := base.MinWait, base.MaxWait, base.Coeff

	for _, key := range slices.Sorted(maps.Keys(data)) {
		switch key {
		case "min_wait":
			d, err := durationFromValue(data[key], key)
			if err != nil {
				return nil, err
			}
			minWait = d
		case "max_wait":
			d, err := durationFromValue(data[key], key)
			if err != nil {
				return nil, err
			}
			maxWait = d
		case "coefficients":
			coeffData, ok := data[key].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s: must be a map", key)
			}
			for _, ck := range slices.Sorted(maps.Keys(coeffData)) {
				p := keyPath(key, ck)
				f, err := floatFromValue(coeffData[ck], p)
				if err != nil {
					return nil, err
				}
				switch ck {
				case "success":
					coeff.Success = f
				case "fail":
					coeff.Fail = f
				case "jitter":
					coeff.Jitter = f
				default:
					return nil, fmt.Errorf("%s: unknown key", p)
				}
			}
		default:
			return nil, fmt.Errorf("%s: unknown key", key)
		}
	}

	if minWait <= 0 {
		return nil, errors.New("min_wait: must be positive")
	}
	if maxWait < minWait {
		return nil, errors.New("max_wait: must not be less than min_wait")
	}
	if coeff.Success <= 0 || coeff.Fail <= 0 {
		return nil, errors.New("coefficients: success and fail must be positive")
	}
	if coeff.Jitter < 0 || coeff.Jitter > 1 {
		return nil, errors.New("coefficients.jitter: must be between 0 and 1")
	}
	return NewBackoff(minWait, maxWait, coeff), nil
}

// floatFromValue reads a finite number. Yaml may decode "2" as int.
func floatFromValue(value any, path string) (float64, error) {
	var f float64
	switch v := value.(type) {
	case float64:
		f = v
	case int:
		f = float64(v)
	case int64:
		f = float64(v)
	default:
		return 0, fmt.Errorf("%s: must be a number", path)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%s: must be finite", path)
	}
	return f, nil
}

// NewBackoff returns a Backoff that waits between minWait and maxWait.
func NewBackoff(minWait time.Duration, maxWait time.Duration, coeff BackoffCoefficients) *Backoff {
	return &Backoff{
		last:    time.Now().Add(-minWait),
		wait:    minWait,
		MinWait: minWait,
		MaxWait: maxWait,
		Coeff:   coeff,
	}
}

// Update records the outcome of an attempt, schedules the next one, and
// returns the wait from now until it may happen. It also stamps the current
// time, so Next and UntilNext are measured from this call.
//
// With newWait == 0 the wait follows the exponential schedule: the previous
// wait is multiplied by Coeff.Fail if fail is true, or Coeff.Success
// otherwise, then scaled by a random jitter factor in the range
// 1 ± Coeff.Jitter/2. The result is clamped to MinWait..MaxWait.
//
// With newWait != 0 the schedule is overridden, which is how a Retry-After
// header is honoured. fail is ignored and no jitter is applied. The wait is
// clamped to MinWait..24h, not MinWait..MaxWait, so a server's request can
// exceed MaxWait but a request for days cannot stall the host indefinitely.
// Callers should still set a Timeout on the http.Client (10 minutes, for
// example) as a backstop. Because the override becomes the current wait, the
// next scheduled update starts from it, and is then clamped to MaxWait, so
// the wait drops back into the normal range after one scheduled update.
//
// Update modifies the Backoff and is not safe for concurrent use; callers
// must serialize access.
func (adj *Backoff) Update(fail bool, newWait time.Duration) time.Duration {
	maxWait := adj.MaxWait

	if newWait == 0 {
		// Normal backoff
		// Update wait value based on success or fail and current wait time.
		jitter := (rand.Float64()-0.5)*adj.Coeff.Jitter + 1
		multiplier := adj.Coeff.Success
		if fail {
			multiplier = adj.Coeff.Fail
		}
		newWait = time.Duration(multiplier * jitter * float64(adj.wait))
	} else {
		// Override, probably from a Retry-After header. The upper bound is
		// fixed at 24h instead of MaxWait, because the server knows best
		// when it will be ready, but an absurd value must not block forever.
		// The lower bound below still applies, so a Retry-After shorter than
		// MinWait is raised to MinWait.
		maxWait = maxRetryAfterWait
	}

	adj.wait = clampWait(newWait, adj.MinWait, maxWait)
	adj.last = time.Now()
	return adj.wait
}

// previewWait returns the longest wait Update may set for the same outcome,
// without modifying the backoff or stamping the time. The normal schedule uses
// the upper end of the jitter range, so a wait that fits is certain to fit.
func (adj *Backoff) previewWait(fail bool, newWait time.Duration) time.Duration {
	maxWait := adj.MaxWait

	if newWait == 0 {
		multiplier := adj.Coeff.Success
		if fail {
			multiplier = adj.Coeff.Fail
		}
		newWait = time.Duration(multiplier * (1 + adj.Coeff.Jitter/2) * float64(adj.wait))
	} else {
		maxWait = maxRetryAfterWait
	}
	return clampWait(newWait, adj.MinWait, maxWait)
}

// clampWait bounds a wait to minWait..maxWait.
func clampWait(wait time.Duration, minWait time.Duration, maxWait time.Duration) time.Duration {
	if wait < minWait {
		return minWait
	}
	if wait > maxWait {
		return maxWait
	}
	return wait
}

// UntilNext returns how long to wait from now until the next attempt.
func (adj *Backoff) UntilNext() time.Duration {
	return time.Until(adj.last) + adj.wait
}

// Next returns the time of the next attempt.
func (adj *Backoff) Next() time.Time {
	return adj.last.Add(adj.wait)
}

// Clone returns a copy of the backoff, reset to its minimum wait.
func (adj *Backoff) Clone() *Backoff {
	cloned := *adj
	cloned.last = time.Now().Add(-cloned.MinWait)
	cloned.wait = cloned.MinWait
	return &cloned
}
