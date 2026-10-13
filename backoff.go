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

type Backoff struct {
	last time.Time
	wait time.Duration

	MinWait time.Duration       `json:"min_wait"`
	MaxWait time.Duration       `json:"max_wait"`
	Coeff   BackoffCoefficients `json:"coefficients"`
}

type BackoffCoefficients struct {
	Success float64 `json:"success"`
	Fail    float64 `json:"fail"`
	Jitter  float64 `json:"jitter"`
}

var DefaultBackoffCoefficients = BackoffCoefficients{
	Success: 0.5,
	Fail:    1.5,
	Jitter:  0.3,
}

var DefaultBackoff = NewBackoff(1*time.Second, 120*time.Second, DefaultBackoffCoefficients)

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

func NewBackoff(minWait time.Duration, maxWait time.Duration, coeff BackoffCoefficients) *Backoff {
	return &Backoff{
		last:    time.Now().Add(-minWait),
		wait:    minWait,
		MinWait: minWait,
		MaxWait: maxWait,
		Coeff:   coeff,
	}
}

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
		// Override backoff
		// A wait has been given, probably by the Retry-After header
		// and this could deadlock if set to a large delay (eg days).

		// It is recommended to set Timeout in http.Client to a
		// reasonable value such as 10 minutes to handle this.

		// Wait at least minWait, even if Retry-After encourages retry sooner.
		// Additionally, a maximum wait of 24 hours is set as a sane bound.
		maxWait = 24 * time.Hour
	}

	// Clamp minWait <= newWait <= maxWait
	if newWait < adj.MinWait {
		newWait = adj.MinWait
	} else if newWait > maxWait {
		newWait = maxWait
	}
	adj.wait = newWait
	adj.last = time.Now()
	return adj.wait
}

func (adj *Backoff) UntilNext() time.Duration {
	return time.Until(adj.last) + adj.wait
}

func (adj *Backoff) Next() time.Time {
	return adj.last.Add(adj.wait)
}

func (adj *Backoff) Clone() *Backoff {
	copy := *adj
	copy.last = time.Now().Add(-copy.MinWait)
	copy.wait = copy.MinWait
	return &copy
}
