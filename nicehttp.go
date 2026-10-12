// Package nicehttp provides a custom HTTP client with built-in support
// for rate limiting, automatic retries, and exponential backoff.
package nicehttp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
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

var NoTimeout time.Duration = 0
var DefaultUserAgent = fmt.Sprintf("nicehttp/%s", getVersion())
var DefaultAttemptTimeout time.Duration = 0
var DefaultMaxAttempts = 10

// discardedBodyLimit is the maximum number of bytes read from the response body
// of a request attempt that is going to be retried. Draining the body allows
// the downstream roundtripper to reuse the connection; a body larger than this
// limit is closed early and its connection is dropped instead.
const discardedBodyLimit = 4096

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

// cancelOnCloseBody cancels the attempt context once the response body has been
// fully read or closed. The attempt context is tied to the response body by the
// downstream roundtripper, so cancelling it while the caller is still reading
// aborts the read.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		// The body is finished (or broken), so the attempt can be cancelled
		b.cancel()
	}
	return n, err
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

type NiceTransport struct {
	// Headers added to every request
	// Include headers like "User-Agent" and "Authorization" here
	defaultHeaders http.Header
	// Number of times to retry the request on failure
	maxAttempts int
	// Timeout for a single connection attempt, after which the request
	// will be retried. This is part of the total request time, which is
	// controlled by the http.Client timeout.
	// The attempt stays active until the response body has been read or
	// closed, so the timeout also limits how long the caller may spend
	// reading the body.
	// A value of 0 means no timeout.
	attemptTimeout time.Duration
	// downstreamTransport allows override of downstream roundtripper
	// Not specifying a value means http.DefaultTransport will be used
	downstreamTransport http.RoundTripper
	// RateLimter ensures a minimum rate limit between the start of requests
	limiter *Limiter
}

func (s *NiceTransport) Clone() *NiceTransport {
	var snew NiceTransport
	snew.defaultHeaders = s.defaultHeaders.Clone()
	snew.maxAttempts = s.maxAttempts
	snew.attemptTimeout = s.attemptTimeout
	snew.downstreamTransport = s.downstreamTransport
	if s.limiter != nil {
		snew.limiter = s.limiter.Clone()
	}
	return &snew
}

// downstream returns the transport that requests are sent to. Not specifying a
// downstream transport means http.DefaultTransport is used.
func (rt *NiceTransport) downstream() http.RoundTripper {
	if rt.downstreamTransport == nil {
		return http.DefaultTransport
	}
	return rt.downstreamTransport
}

// CloseIdleConnections closes idle connections in the downstream roundtripper.
// It is called by http.Client.CloseIdleConnections.
func (rt *NiceTransport) CloseIdleConnections() {
	if closer, ok := rt.downstream().(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func shouldRetryRequest(resp *http.Response, err error) bool {
	if err != nil {
		// Retry if a network error has occurred, such as
		// - connection timeouts,
		// - dns resolution errors,
		// - connection reset
		return true
	}

	if resp == nil {
		return false
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
	if rt.limiter == nil {
		// A transport without a limiter cannot back off between attempts
		return nil, fmt.Errorf("nicehttp: transport has no limiter, create it with NewNiceTransportBuilder")
	}

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

	origCtx := origReq.Context()

	// maxAttempts and attemptTimeout can be overridden per request through context
	maxAttempts := rt.maxAttempts
	if val, ok := GetMaxAttemptsFromContext(origCtx); ok {
		maxAttempts = val
	}
	attemptTimeout := rt.attemptTimeout
	if val, ok := GetAttemptTimeoutFromContext(origCtx); ok {
		attemptTimeout = val
	}

	attempt := 0
	for {
		// This loop will run up to maxAttempts times
		attempt += 1

		resp, err, final := rt.roundTripAttempt(origCtx, origReq, attemptTimeout, attempt >= maxAttempts)
		if final {
			return resp, err
		}

		if bodySeeker != nil {
			// Rewind the body to the start on retry
			bodySeeker.Seek(0, io.SeekStart)
		}
	}
}

// newAttemptContext returns a timeout-derived context and its cancel function,
// or the parent context and a nil cancel function if no timeout is specified.
func newAttemptContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout == NoTimeout {
		return parent, nil
	}
	return context.WithTimeout(parent, timeout)
}

// roundTripAttempt executes a single request attempt under the rate limiter,
// returning whether the response is final or eligible for a retry.
func (rt *NiceTransport) roundTripAttempt(origCtx context.Context, origReq *http.Request, attemptTimeout time.Duration, lastAttempt bool) (resp *http.Response, err error, final bool) {
	// Wait patiently for the limiter before sending the request up to the context deadline
	if err := rt.limiter.Wait(origCtx); err != nil {
		// The original context is cancelled or has exceeded deadline
		return nil, err, true
	}

	needsRetry := true
	var retryAfter time.Duration

	ctx, cancelAttemptCtx := newAttemptContext(origCtx, attemptTimeout)
	defer func() {
		rt.limiter.Done(needsRetry, retryAfter)
		if cancelAttemptCtx != nil {
			cancelAttemptCtx()
		}
	}()

	// Clone the request with the attempt-specific context
	req := origReq.Clone(ctx)

	// Set default headers (used for User-Agent and Authorization) on the
	// request sent downstream. The request given by the caller is left
	// untouched, because it may be reused or shared with other clients
	if len(rt.defaultHeaders) > 0 {
		if req.Header == nil {
			req.Header = make(http.Header)
		}
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

	resp, err = rt.downstream().RoundTrip(req)

	// Determine if the request should be retried
	needsRetry = shouldRetryRequest(resp, err)
	retryAfter = parseRetryAfterHeader(resp)

	if lastAttempt || !needsRetry {
		// Hand off context ownership to the response body wrapper if successful
		if cancelAttemptCtx != nil && resp != nil && resp.Body != nil {
			resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancelAttemptCtx}
			cancelAttemptCtx = nil
		}
		return resp, err, true
	}

	// Drain and close the discarded body to allow connection reuse for retries
	if resp != nil && resp.Body != nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, discardedBodyLimit))
		resp.Body.Close()
	}

	return resp, err, false
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
		} else {
			log.Printf("nicehttp: unexpected formatting of Retry-After value %q", val)
		}
	}
	return 0
}
