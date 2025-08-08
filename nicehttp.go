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
	// Number of times to retry the request on failure
	maxTries int
	// downstreamTransport allows override of downstream roundtripper
	// Not specifying a value means http.DefaultTransport will be used
	downstreamTransport http.RoundTripper
	// RateLimter ensures a minimum rate limit between the start of requests
	limiter *Limiter
}

func (s *NiceTransport) Clone() *NiceTransport {
	var snew NiceTransport
	snew.defaultHeaders = s.defaultHeaders.Clone()
	snew.maxTries = s.maxTries
	snew.downstreamTransport = s.downstreamTransport
	if s.limiter != nil {
		snew.limiter = s.limiter.Clone()
	}
	return &snew
}

func shouldRetryRequest(resp *http.Response, err error) bool {
	if err != nil {
		// Retry if a network error has occurred, such as
		// - connection timeouts,
		// - dns resolution errors,
		// - connection reset
		return true
	}

	switch resp.StatusCode {
	case 429, 503, 504:
		// The request can be retried with backoff if its status is one of:
		// - 429 Too Many Requests
		// - 503 Service Unavailable
		// - 504 Gateway Timeout
		return true
	}

	return false
}

// RoundTrip is a custom implementation that handles backoff, error retries and done context
func (rt *NiceTransport) RoundTrip(origReq *http.Request) (*http.Response, error) {
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

	// Set default headers (used for User-Agent and Authorization)
	if len(rt.defaultHeaders) > 0 {
		for key, values := range rt.defaultHeaders {
			if len(origReq.Header[key]) > 0 {
				// Header already exists with at least one value, do not replace with defaults
				continue
			}
			// Support header keys with multiple values
			for _, val := range values {
				origReq.Header.Add(key, val)
			}
		}
	}

	tries := 0
	for {
		// This loop will run up to rt.maxTries times
		tries += 1

		// The request must be cloned each time it is sent
		req := origReq.Clone(origReq.Context())

		// Wait patiently for the limiter before sending the request
		if err := rt.limiter.Wait(req.Context()); err != nil {
			// Context is cancelled or has exceeded deadline
			return nil, err
		}

		// Sending request using downstream roundtripper tries to close req.Body
		// but this is handled by the io.NopCloser wrapper so retrying is possible
		resp, err := rt.downstreamTransport.RoundTrip(req)

		needsRetry := shouldRetryRequest(resp, err)
		retryAfter := parseRetryAfterHeader(resp)
		rt.limiter.Done(needsRetry, retryAfter)

		if tries >= rt.maxTries || !needsRetry {
			// The request either succeeded or failed with an http status that cannot be retried
			// Or the retry limit has been reached so return the response and error no matter what
			// Return the response and error to the caller
			return resp, err
		}

		// Rewind the body to the start on retries when body is not nil
		if bodySeeker != nil {
			bodySeeker.Seek(0, io.SeekStart)
		}
	}
}

func parseRetryAfterHeader(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	val := resp.Header.Get("Retry-After")
	if val != "" {
		if secs, err := strconv.Atoi(val); err == nil {
			return time.Duration(secs) * time.Second
		} else if t, err := http.ParseTime(val); err == nil {
			return time.Until(t)
		} // TODO: log unexpected
	}
	return 0
}
