// Package nicehttp provides a custom HTTP client with built-in support
// for rate limiting, automatic retries, and exponential backoff.
package nicehttp

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

func getVersion() string {
	info, ok := debug.ReadBuildInfo()
	if ok {
		for _, mod := range info.Deps {
			if mod.Path != "github.com/rohfle/nicehttp" {
				continue
			}
			if mod.Replace != nil {
				mod = mod.Replace
			}
			if mod.Version != "" && mod.Version != "(devel)" {
				return mod.Version
			}
		}
	}
	return "unknown"
}

var DefaultUserAgent = fmt.Sprintf("nicehttp/%s", getVersion())

type readSeekCloser struct {
	*bytes.Reader
}

func (rsc readSeekCloser) Close() error {
	// No resources to free, so just return no error
	return nil
}

func newReadSeekCloser(b []byte) io.ReadSeekCloser {
	return readSeekCloser{bytes.NewReader(b)}
}

type NiceTransport struct {
	// Headers added to every request
	// Include headers like "User-Agent" and "Authorization" here
	defaultHeaders http.Header
	// Amount of time to wait between errors
	// Doubled after every successive fail
	backoff time.Duration
	// Maximum amount of time to wait between errors
	// Backoff will be clamped to this value
	maxBackoff time.Duration
	// Number of times to retry the request on failure
	maxTries int
	// downstreamTransport allows override of downstream roundtripper
	// Not specifying a value means http.DefaultTransport will be used
	downstreamTransport http.RoundTripper
	// RateLimter ensures a minimum rate limit between the start of requests
	rateLimiter *rate.Limiter
}

func (s *NiceTransport) Clone() *NiceTransport {
	var snew NiceTransport
	snew.defaultHeaders = s.defaultHeaders.Clone()
	snew.backoff = s.backoff
	snew.maxBackoff = s.maxBackoff
	snew.maxTries = s.maxTries
	snew.downstreamTransport = s.downstreamTransport
	if s.rateLimiter != nil {
		snew.rateLimiter = rate.NewLimiter(s.rateLimiter.Limit(), s.rateLimiter.Burst())
	}
	return &snew
}

// calculateWaitAfterError calculates a wait duration based on headers and current backoff
func (rt *NiceTransport) calculateWaitAfterError(resp *http.Response, backoff time.Duration) time.Duration {
	waitTime := backoff
	// retry-after header sometimes set on 429 too many requests
	val := resp.Header.Get("Retry-After")
	if secs, err := strconv.Atoi(val); err == nil {
		waitTime = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(val); err == nil {
		waitTime = t.Sub(time.Now().UTC())
	}

	// wait at least backoff even if Retry-After encourages to try again sooner
	if waitTime < backoff {
		waitTime = backoff
	}

	return waitTime
}

// RoundTrip is a custom implementation that handles backoff, error retries and done context
func (rt *NiceTransport) RoundTrip(origReq *http.Request) (*http.Response, error) {
	// Set the initial backoff
	backoff := rt.backoff

	// in order to retry requests, we need io.Seeker to support rewinding the body
	var bodySeeker io.ReadSeekCloser = nil
	// the body might be nil for some methods (eg GET / HEAD)
	if origReq.Body != nil {
		var hasSeek bool
		bodySeeker, hasSeek = origReq.Body.(io.ReadSeekCloser)
		if hasSeek {
			// The body is seekable so we can stream the original
			// Defer the close of the original request body
			defer origReq.Body.Close()
			// Prevent downstream roundtripper from closing the body
			origReq.Body = io.NopCloser(bodySeeker)
		} else {
			// The body is not seekable so we have to read it fully into memory
			data, err := io.ReadAll(origReq.Body)
			// Close body immediately after read, regardless if there was an error or not
			origReq.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("while reading request body: %w", err)
			}
			// With the read data, create a new bytes.Reader with noop Close()
			// No need to defer closing the bytes.Reader
			bodySeeker = newReadSeekCloser(data)
			origReq.Body = bodySeeker
		}
	}

	tries := 0
	for {
		// This loop will run up to rt.maxTries times
		tries += 1

		// Check if context is done on the original request before attempting anything
		select {
		case <-origReq.Context().Done():
			// The original request has timed out, so return its error with no result
			return nil, origReq.Context().Err()
		default:
		}

		// Rewind the body to the start on retries when body is not nil
		if bodySeeker != nil && tries > 0 {
			bodySeeker.Seek(0, io.SeekStart)
		}

		// The request must be cloned each time it is sent
		req := origReq.Clone(origReq.Context())

		// Set default headers (used for User-Agent and Authorization)
		if len(rt.defaultHeaders) > 0 {
			for key, values := range rt.defaultHeaders {
				if len(req.Header[key]) > 0 {
					// Header already exists with at least one value, do not replace with defaults
					continue
				}
				// Support header keys with multiple values
				for _, val := range values {
					req.Header.Add(key, val)
				}
			}
		}

		// Wait patiently for the limiter before sending the request
		if err := rt.rateLimiter.Wait(req.Context()); err != nil {
			// Context is cancelled or has exceeded deadline
			return nil, err
		}

		// Send the request using the downstream roundtripper
		// This call tries to close req.Body, which we don't want because we rewind
		// but we handle this with the io.NopCloser wrapper above
		resp, err := rt.downstreamTransport.RoundTrip(req)

		// On the last retry, return the response and error no matter what
		if tries >= rt.maxTries {
			return resp, err
		}

		// Check if there is a response error
		if err != nil {
			// A network error has occurred, such as
			// - connection timeouts,
			// - dns resolution errors,
			// - connection reset
			// Retry respecting request interval but without additional backoff
			// No need to close the resp.Body on error
			// And try again!
			continue
		}

		// No error, check the returned status code
		switch resp.StatusCode {
		case 429, 503, 504:
			// The request can be retried with backoff if its status is one of:
			// - 429 Too Many Requests
			// - 503 Service Unavailable
			// - 504 Gateway Timeout
			waitTime := rt.calculateWaitAfterError(resp, backoff)
			// Close the response body before retry or memory gonna leak
			if resp.Body != nil {
				resp.Body.Close()
			}

			select {
			case <-origReq.Context().Done():
				// Context is cancelled or has exceeded deadline
				return nil, origReq.Context().Err()
			case <-time.After(waitTime):
				// Sleep before retry zzz
			}

			// Double backoff each retry
			backoff = min(backoff*2, rt.maxBackoff)
			// And try again!
			continue
		}

		// The request either succeeded or failed with an error that cannot be retried
		// Either way, return the result to the caller
		return resp, err
	}
}
