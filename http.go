package nicehttpclient

import (
	"context"
	"errors"
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

type NiceHTTPClientSettings struct {
	// Headers added to every request
	// Include headers like "User-Agent" and "Authorization" here
	DefaultHeaders http.Header
	// Minimum time between successful requests
	RequestInterval time.Duration
	// Amount of time to wait between errors
	// Doubled after every successive fail
	Backoff time.Duration
	// Maximum amount of time to wait between errors
	// Backoff will be clamped to this value
	MaxBackoff time.Duration
	// Number of times to retry the request on failure
	MaxRetries int
	// Maximum number of connections per host
	MaxConnsPerHost int
}

// Create a nice HTTP client with custom transport that implements rate limit, backoff and max retries
func NiceHTTPClient(settings *NiceHTTPClientSettings) *http.Client {
	limiter := rate.NewLimiter(rate.Every(settings.RequestInterval), 1)

	// Provide some sensible default values
	maxRetries := settings.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 5
	}

	backoff := settings.Backoff
	if backoff <= 0 {
		backoff = 1 * time.Second
	}

	maxBackoff := settings.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = settings.MaxConnsPerHost

	return &http.Client{
		Transport: &niceRoundTripper{
			roundTripper:   transport,
			limiter:        limiter,
			defaultHeaders: settings.DefaultHeaders,
			backoff:        backoff,
			maxBackoff:     maxBackoff,
			maxRetries:     maxRetries,
		},
	}
}

// niceRoundTripper is a custom transport that enforces a rate limit
type niceRoundTripper struct {
	roundTripper   http.RoundTripper // underlying RoundTripper to use
	limiter        *rate.Limiter
	defaultHeaders http.Header
	backoff        time.Duration
	maxBackoff     time.Duration
	maxRetries     int
}

// Calculate a wait duration from Retry-After response header and current backoff
func (rt *niceRoundTripper) calculateWaitAfterError(resp *http.Response, backoff time.Duration) time.Duration {
	waitTime := backoff
	// retry-after header sometimes set on 429 too many requests
	retryAfterHeader := resp.Header.Get("Retry-After")
	retryAfterDuration, err := time.ParseDuration(retryAfterHeader + "s")
	if err == nil {
		waitTime = time.Duration(retryAfterDuration)
	}

	if waitTime < backoff {
		waitTime = backoff
	}

	return waitTime
}

// Custom RoundTrip function that implements retries, backoff, handles error retries and done context
func (rt *niceRoundTripper) RoundTrip(origReq *http.Request) (*http.Response, error) {
	// Set the initial backoff
	backoff := rt.backoff
	deadline, deadlineSet := origReq.Context().Deadline()

	for retries := 0; retries <= rt.maxRetries; retries++ {
		// Check if context is done on the original request before sending
		select {
		case <-origReq.Context().Done():
			return nil, origReq.Context().Err()
		default:
		}

		// The request must be cloned each time it is sent so the body is reset
		var req *http.Request
		if deadlineSet {
			ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
			defer cancel()
			req = origReq.Clone(ctx)
		} else {
			req = origReq.Clone(context.Background())
		}

		// Wait for the limiter before sending the request
		if err := rt.limiter.Wait(req.Context()); err != nil {
			return nil, err
		}

		// Set default headers (used for User-Agent and Authorization)
		if len(rt.defaultHeaders) > 0 {
			for key, values := range rt.defaultHeaders {
				if len(values) == 0 {
					continue
				}
				// Support header keys with multiple values
				req.Header.Del(key)
				for _, val := range values {
					req.Header.Add(key, val)
				}
			}
		}

		// Send the request
		resp, err := rt.roundTripper.RoundTrip(req)

		// On the last retry, return the result no matter what the result
		if retries >= rt.maxRetries {
			return resp, err
		}

		// Check if there is a response error
		if err == nil {
			// No error, check the returned status code
			switch resp.StatusCode {
			case 429, 503, 504:
				// 429 Too Many Requests
				// 503 Service Unavailable
				// 504 Gateway Timeout
				// the request can be retried, with backoff cause we're nice :)
			default:
				// The request was either successful, or had an error that cannot be retried
				// Either way, return the result
				return resp, err
			}
		} else {
			// A network error has occurred, retry without additional backoff
			// Things like connection timeouts, dns resolution errors, connection reset
			// Request interval still applies so we are still being reasonably nice
			continue
		}

		waitTime := rt.calculateWaitAfterError(resp, backoff)
		resp.Body.Close()

		select {
		case <-req.Context().Done(): // context cancelled so don't sleep
		case <-time.After(waitTime): // sleep before retry
		}

		// Double backoff each retry
		backoff = backoff * 2
		if backoff > rt.maxBackoff {
			backoff = rt.maxBackoff
		}
	}

	// Execution never gets here but we apologise anyway
	return nil, errors.New("too many retries sorry")
}
