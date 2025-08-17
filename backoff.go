package nicehttp

import (
	"math/rand/v2"
	"time"
)

type Backoff interface {
	Update(fail bool, waitGiven time.Duration) time.Duration
	Next() time.Duration
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
	if newWait == 0 {
		// if no override is given, update wait time based on success or fail
		jitter := (rand.Float64()-0.5)*adj.coeff.Jitter + 1
		multiplier := adj.coeff.Success
		if fail {
			multiplier = adj.coeff.Fail
		}
		newWait = time.Duration(multiplier * jitter * float64(adj.wait))
		// TODO: should retry-after be bound by this also?
		newWait = min(newWait, adj.maxWait)
	}

	// always wait at least minWait
	adj.wait = max(adj.minWait, newWait)
	adj.last = time.Now()
	return adj.wait
}

func (adj *ExponentialBackoff) Next() time.Duration {
	return time.Until(adj.last) + adj.wait
}

func (adj *ExponentialBackoff) Clone() Backoff {
	copy := *adj
	copy.last = time.Now().Add(-copy.minWait)
	copy.wait = copy.minWait
	return &copy
}
