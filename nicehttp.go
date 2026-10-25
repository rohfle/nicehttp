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

// versionUnknown is the version reported for builds that carry none.
const versionUnknown = "unknown"

// nicehttpModulePath is this module, as it appears in build info.
const nicehttpModulePath = "github.com/rohfle/nicehttp"

// getVersion returns the nicehttp release version, or versionUnknown for local
// builds, which report no version or "(devel)".
func getVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return versionUnknown
	}
	return versionFrom(info)
}

// versionFrom reads the nicehttp version out of build info: the main module
// version when nicehttp was built as the main module, otherwise the version of
// the dependency, following a replacement.
func versionFrom(info *debug.BuildInfo) string {
	if info.Main.Path == nicehttpModulePath && isReleaseVersion(info.Main.Version) {
		return info.Main.Version
	}
	for _, module := range info.Deps {
		if module.Path != nicehttpModulePath {
			continue
		}
		if module.Replace != nil {
			module = module.Replace
		}
		if isReleaseVersion(module.Version) {
			return module.Version
		}
	}
	return versionUnknown
}

// isReleaseVersion reports whether a module version identifies a release,
// rather than the empty version or the "(devel)" of a local build.
func isReleaseVersion(version string) bool {
	return version != "" && version != "(devel)"
}

const (
	// NoTimeout means the request will wait forever to complete
	NoTimeout time.Duration = 0
	// TimeoutNotSet means the timeout is unset, so DefaultAttemptTimeout applies
	TimeoutNotSet time.Duration = -1
)

// Package defaults. They are read when a builder or per host config is
// created or built, so changing one affects only transports created
// afterwards, never existing ones. They are plain variables and are not
// synchronized: set them during program initialization, before any transport
// is created or used from another goroutine.
var (
	// DefaultUserAgent is the User-Agent header value added by the builder when
	// the default headers do not include one. It is "nicehttp/<version>".
	DefaultUserAgent = fmt.Sprintf("nicehttp/%s", getVersion())

	// DefaultAttemptTimeout is the timeout of each attempt when the builder
	// or a per host config does not set one. The default is NoTimeout, which
	// disables the per attempt deadline; requests are then bounded only by the
	// caller's context or the http.Client timeout.
	DefaultAttemptTimeout time.Duration = NoTimeout

	// DefaultMaxAttempts is the maximum number of attempts per request,
	// including the first, when the builder or a per host config does not set
	// one. The default is 10.
	DefaultMaxAttempts = 10
)

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

// NiceTransport is an http.RoundTripper that limits the request rate, retries failures and
// adds default headers. Create it with NiceTransportBuilder.
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
	// Not setting a value will use a clone of http.DefaultTransport, or the
	// global itself when it has been replaced with a custom RoundTripper
	downstreamTransport http.RoundTripper
	// limiter ensures a minimum interval between requests
	limiter *Limiter
}

// Clone returns a copy of the transport, with cloned headers, limiter and backoff.
func (rt *NiceTransport) Clone() *NiceTransport {
	cloned := *rt
	cloned.defaultHeaders = rt.defaultHeaders.Clone()
	// The limiter carries state of its own, so it is cloned rather than shared
	if rt.limiter != nil {
		cloned.limiter = rt.limiter.Clone()
	}
	return &cloned
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

// shouldRetryRequest reports whether the request may be sent again after an
// attempt failed or returned a retryable response.
//
// origReq is the request given by the caller, not the per attempt clone: the
// attempt timeout cancels the clone's context, and that timeout is exactly one
// of the cases that should be retried.
func shouldRetryRequest(origReq *http.Request, resp *http.Response, err error) bool {
	// The caller gave up, so a retry cannot succeed
	if origReq.Context().Err() != nil {
		return false
	}

	if err != nil {
		// A network error, such as connection timeout, dns resolution error or
		// connection reset, does not say whether the request reached the
		// server, so only a request that is safe to replay is retried
		return replayable(origReq)
	}

	if resp == nil {
		return false
	}

	switch resp.StatusCode {
	case 429, 503:
		// The server says it did not process the request
		return true
	case 504:
		// The upstream may have processed the request before the gateway
		// timed out
		return replayable(origReq)
	}

	return false
}

// replayable reports whether sending the request twice is harmless: an
// idempotent method, a request the server can deduplicate with an
// Idempotency-Key, or an explicit opt in from the caller.
func replayable(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodPut,
		http.MethodDelete, http.MethodOptions, http.MethodTrace:
		return true
	}
	return req.Header.Get("Idempotency-Key") != "" || GetRetryNonIdempotentFromContext(req.Context())
}

// RoundTrip is a custom implementation that handles backoff, error retries and done context.
//
// A failed attempt is retried up to maxAttempts. An attempt timeout is always
// retried, while a cancelled or expired caller context never is. After a
// transport error or a 504 Gateway Timeout, which do not say whether the
// request reached the server, only a request that is safe to replay is retried:
// an idempotent method, a request carrying an Idempotency-Key, or a request
// whose context comes from SetRetryNonIdempotentInContext. A 429 or 503 is
// retried for any method, since the server answered that it did not process the
// request.
//
// A response that arrives after the caller's context is done is returned as is,
// rather than retried, and the cancellation surfaces when its body is read.
func (rt *NiceTransport) RoundTrip(origReq *http.Request) (*http.Response, error) {
	if rt.limiter == nil {
		// A transport without a limiter cannot back off between attempts
		return nil, fmt.Errorf("nicehttp: transport has no limiter, create it with NewNiceTransportBuilder")
	}

	// in order to retry requests, we need io.Seeker to support rewinding the body
	var bodySeeker io.ReadSeekCloser
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
		attempt++

		resp, final, err := rt.roundTripAttempt(origCtx, origReq, attemptTimeout, attempt >= maxAttempts)
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
func (rt *NiceTransport) roundTripAttempt(origCtx context.Context, origReq *http.Request, attemptTimeout time.Duration, lastAttempt bool) (resp *http.Response, final bool, err error) {
	// Wait patiently for the limiter before sending the request up to the context deadline
	if err := rt.limiter.Wait(origCtx); err != nil {
		// The original context is cancelled or has exceeded deadline
		return nil, true, err
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
	needsRetry = shouldRetryRequest(origReq, resp, err)
	retryAfter = parseRetryAfterHeader(resp)

	if lastAttempt || !needsRetry {
		// Hand off context ownership to the response body wrapper if successful
		if cancelAttemptCtx != nil && resp != nil && resp.Body != nil {
			resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancelAttemptCtx}
			cancelAttemptCtx = nil
		}
		return resp, true, err
	}

	// Drain and close the discarded body to allow connection reuse for retries
	if resp != nil && resp.Body != nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, discardedBodyLimit))
		resp.Body.Close()
	}

	return resp, false, err
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
