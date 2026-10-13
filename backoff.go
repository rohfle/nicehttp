package nicehttp

import (
	"fmt"
	"math/rand/v2"
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

func NewBackoffFromMap(data map[string]any) (*Backoff, error) {
	minWaitStr, ok := data["minWait"].(string)
	if !ok {
		return nil, fmt.Errorf("minWait must be a string")
	}

	maxWaitStr, ok := data["maxWait"].(string)
	if !ok {
		return nil, fmt.Errorf("maxWait must be a string")
	}

	minWait, err := time.ParseDuration(minWaitStr)
	if err != nil {
		return nil, fmt.Errorf("invalid minWait duration: %w", err)
	}

	maxWait, err := time.ParseDuration(maxWaitStr)
	if err != nil {
		return nil, fmt.Errorf("invalid maxWait duration: %w", err)
	}

	var coeff BackoffCoefficients
	if coeffData, ok := data["coeff"].(map[string]any); !ok {
		coeff = DefaultBackoffCoefficients
	} else {
		if success, ok := coeffData["Success"].(float64); ok {
			coeff.Success = success
		} else {
			coeff.Success = DefaultBackoffCoefficients.Success
		}

		if fail, ok := coeffData["Fail"].(float64); ok {
			coeff.Fail = fail
		} else {
			coeff.Fail = DefaultBackoffCoefficients.Fail
		}

		if jitter, ok := coeffData["Jitter"].(float64); ok {
			coeff.Jitter = jitter
		} else {
			coeff.Jitter = DefaultBackoffCoefficients.Jitter
		}
	}

	return NewBackoff(minWait, maxWait, coeff), nil
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
