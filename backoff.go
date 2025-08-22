package nicehttp

import (
	"math/rand/v2"
	"time"
)

type Backoff interface {
	Update(fail bool, waitGiven time.Duration) time.Duration
	UntilNext() time.Duration
	Next() time.Time
	Clone() Backoff
}

type ExponentialBackoff struct {
	last time.Time
	wait time.Duration

	minWait time.Duration
	maxWait time.Duration
	coeff   ExponentialBackoffCoefficients
}

type ExponentialBackoffCoefficients struct {
	Success float64
	Fail    float64
	Jitter  float64
}

var DefaultExponentialBackoffCoefficients = ExponentialBackoffCoefficients{
	Success: 0.5,
	Fail:    1.5,
	Jitter:  0.3,
}

var DefaultExponentialBackoff = NewExponentialBackoff(1*time.Second, 120*time.Second, DefaultExponentialBackoffCoefficients)

func NewExponentialBackoff(minWait time.Duration, maxWait time.Duration, coeff ExponentialBackoffCoefficients) *ExponentialBackoff {
	return &ExponentialBackoff{
		last:    time.Now().Add(-minWait),
		wait:    minWait,
		minWait: minWait,
		maxWait: maxWait,
		coeff:   coeff,
	}
}

func (adj *ExponentialBackoff) Update(fail bool, newWait time.Duration) time.Duration {
	maxWait := adj.maxWait

	if newWait == 0 {
		// Normal backoff
		// Update wait value based on success or fail and current wait time.
		jitter := (rand.Float64()-0.5)*adj.coeff.Jitter + 1
		multiplier := adj.coeff.Success
		if fail {
			multiplier = adj.coeff.Fail
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
	if newWait < adj.minWait {
		newWait = adj.minWait
	} else if newWait > maxWait {
		newWait = maxWait
	}
	adj.wait = newWait
	adj.last = time.Now()
	return adj.wait
}

func (adj *ExponentialBackoff) UntilNext() time.Duration {
	return time.Until(adj.last) + adj.wait
}

func (adj *ExponentialBackoff) Next() time.Time {
	return adj.last.Add(adj.wait)
}

func (adj *ExponentialBackoff) Clone() Backoff {
	copy := *adj
	copy.last = time.Now().Add(-copy.minWait)
	copy.wait = copy.minWait
	return &copy
}
