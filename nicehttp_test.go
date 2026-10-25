package nicehttp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type MockEndpoint struct {
	CallCount int
	Start     time.Time
	Timings   []time.Duration
	Deadline  time.Time
	Handler   func(endpoint *MockEndpoint, callCount int, req *http.Request) (*http.Response, error)
}

type MockTransport struct {
	mutex           sync.Mutex
	MaxConnsPerHost int
	Endpoint        *MockEndpoint
}

func (mt *MockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	mt.mutex.Lock()
	if mt.Endpoint.CallCount == 0 {
		mt.Endpoint.Start = time.Now().UTC()
	}
	mt.Endpoint.CallCount++
	mt.Endpoint.Timings = append(mt.Endpoint.Timings, time.Now().UTC().Sub(mt.Endpoint.Start))
	callCount := mt.Endpoint.CallCount
	mt.mutex.Unlock()
	return mt.Endpoint.Handler(mt.Endpoint, callCount, req)
}

func checkGeneralRequestStuff(t *testing.T, req *http.Request, callCount int, method string, body string) {
	if req.URL.String() != "http://example.com" {
		t.Fatalf("call %d: expected req.URL %q, got %q", callCount, "http://example.com", req.URL.String())
	}
	if req.Method != method {
		t.Fatalf("call %d: expected req.Method %q, got %q", callCount, method, req.Method)
	}
	if body != "" {
		if req.Body == nil {
			t.Fatalf("call %d: expected non-nil req.Body", callCount)
		}
		bodyData, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("call %d: while trying to read req.Body: %s", callCount, err)
		}
		if string(bodyData) != body {
			t.Fatalf("call %d: expected req.Body %q, got %q", callCount, body, string(bodyData))
		}
	}
	if _, exists := req.Header["Empty-Header"]; exists {
		t.Fatalf("call %d: expected Empty-Header to not be present in headers", callCount)
	}
}

func checkDeadline(t *testing.T, req *http.Request, callCount int, deadline time.Time) {
	dl, set := req.Context().Deadline()
	if !set {
		t.Fatal("expected request has context deadline")
	}
	diff := dl.Sub(deadline)
	if diff < 0 {
		diff = -diff
	}
	diff /= time.Millisecond
	if diff > 1000 {
		t.Errorf("call %d: endpoint.Deadline and req.Context().Deadline() should be approximately equal", callCount)
		t.Fatalf("call %d: expected < 1000ms difference, got %d", callCount, diff)
	}
}

func createClientWithEndpoint(settings *NiceTransport, endpoint *MockEndpoint) *http.Client {
	if settings == nil {
		settings = &NiceTransport{}
	}
	settings.downstreamTransport = &MockTransport{
		Endpoint: endpoint,
	}

	transport := NewNiceTransportBuilder().Set(settings).Build()

	return &http.Client{
		Transport: transport,
	}
}

func TestRoundTripperFirstRun(t *testing.T) {
	// context with deadline set
	// limiter wait with context expires
	backoff := NewBackoff(100*time.Millisecond, 1*time.Second, DefaultBackoffCoefficients)
	settings := NiceTransport{
		defaultHeaders: http.Header{
			"Empty-Header": []string{}, // test headers with empty value list
		},
		limiter:     NewLimiter(backoff),
		maxAttempts: 5,
	}

	expectedBody := "This is the body"
	expectedCalls := 3
	deadlineDuration := 400 * time.Millisecond

	endpoint := &MockEndpoint{
		CallCount: 0,
		Start:     time.Time{},
		Timings:   nil,
		Deadline:  time.Time{},
		Handler: func(endpoint *MockEndpoint, callCount int, req *http.Request) (*http.Response, error) {
			checkGeneralRequestStuff(t, req, callCount, "POST", expectedBody)
			checkDeadline(t, req, callCount, endpoint.Deadline)
			switch callCount {
			case 1: // +0ms    test network error
				return nil, errors.New("simulated network error")
			case 2: // +151ms  test 429 error
				// A date in the past, so the wait is clamped to minWait. The
				// date form only has second resolution, so a date built from
				// now+100ms is in the past or up to a second in the future
				// depending on the clock phase.
				retryTime := time.Now().UTC().Add(-2 * time.Second)
				header := make(http.Header)
				header.Set("Retry-After", retryTime.Format(http.TimeFormat))
				return &http.Response{
					StatusCode: 429,
					Body:       io.NopCloser(strings.NewReader("Too Many Requests")),
					Header:     header,
				}, nil
			case 3: // +375ms test timeout
				select {
				case <-req.Context().Done(): // Context cancelled so don't sleep please
					return nil, req.Context().Err()
				case <-time.After(10 * time.Second): // Otherwise sleep before retry
				}
				t.Fatal("context timeout for request never occur")
			default:
				t.Fatalf("unhandled call %d", callCount)
			}
			return nil, errors.New("statement should not be reached")
		},
	}

	client := createClientWithEndpoint(&settings, endpoint)

	endpoint.Deadline = time.Now().UTC().Add(deadlineDuration)

	ctx, cancel := context.WithDeadline(context.Background(), endpoint.Deadline)
	defer cancel()

	fakeBody := newReadSeekCloser([]byte(expectedBody))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.com", fakeBody)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext failed: %s", err)
	}
	// A POST is only retried after a transport error when it is marked safe to
	// replay, which is what this header tells the server
	req.Header.Set("Idempotency-Key", "test-first-run")

	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected context deadline exceeded error but got response status %d", resp.StatusCode)
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded error but got error %s", err)
	}

	if endpoint.CallCount != expectedCalls {
		t.Fatalf("expected call count %d, got %d", expectedCalls, endpoint.CallCount)
	}

	t.Logf("calls: %d, timings in ms: %+v", endpoint.CallCount, endpoint.Timings)
}

func TestRoundTripperSecondRun(t *testing.T) {
	// PUT
	// xsend with body on retries
	// xbody is non-seekable
	// reaches maxretries
	// backoff max reached limiting
	settings := NiceTransport{
		limiter: &Limiter{
			backoff: NewBackoff(10*time.Millisecond, 30*time.Millisecond, DefaultBackoffCoefficients),
		},
		maxAttempts: 5,
	}

	expectedBody := "This is the body"
	expectedCalls := 5
	// The retry loop ends on the attempt limit, so the deadline only has to be
	// long enough that it cannot cut a sequence of 10-30ms waits short
	deadlineDuration := 5 * time.Second

	endpoint := &MockEndpoint{
		CallCount: 0,
		Start:     time.Time{},
		Timings:   nil,
		Deadline:  time.Time{},
		Handler: func(endpoint *MockEndpoint, callCount int, req *http.Request) (*http.Response, error) {
			checkGeneralRequestStuff(t, req, callCount, "PUT", expectedBody)
			checkDeadline(t, req, callCount, endpoint.Deadline)

			if callCount == expectedCalls {
				return &http.Response{
					StatusCode: 504,
					Body:       io.NopCloser(strings.NewReader("Gateway Timeout")),
					Header:     make(http.Header),
				}, nil
			}

			return &http.Response{
				StatusCode: 503,
				Body:       io.NopCloser(strings.NewReader("Service Unavailable")),
				Header:     make(http.Header),
			}, nil
		},
	}

	client := createClientWithEndpoint(&settings, endpoint)

	endpoint.Deadline = time.Now().UTC().Add(deadlineDuration)

	ctx, cancel := context.WithDeadline(context.Background(), endpoint.Deadline)
	defer cancel()

	fakeBody := io.NopCloser(bytes.NewReader([]byte(expectedBody)))

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://example.com", fakeBody)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext failed: %s", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("expected 504 response, got unexpected error", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 504 {
		t.Fatalf("expected 504 response but got status %d", resp.StatusCode)
	}
	if endpoint.CallCount != expectedCalls {
		t.Fatalf("expected call count %d, got %d", expectedCalls, endpoint.CallCount)
	}

	t.Logf("calls: %d, timings in ms: %+v", endpoint.CallCount, endpoint.Timings)
}

func TestRoundTripperThirdRun(t *testing.T) {
	// deadline around 0.5 * RequestInterval
	backoff := NewBackoff(500*time.Millisecond, 500*time.Millisecond, DefaultBackoffCoefficients)
	settings := NewNiceTransportBuilder().
		SetLimiterBackoff(backoff).
		SetMaxAttempts(5).Build()

	expectedCalls := 1
	deadlineDuration := 250 * time.Millisecond

	endpoint := &MockEndpoint{
		CallCount: 0,
		Start:     time.Time{},
		Timings:   nil,
		Deadline:  time.Time{},
		Handler: func(endpoint *MockEndpoint, callCount int, req *http.Request) (*http.Response, error) {
			checkDeadline(t, req, callCount, endpoint.Deadline)

			switch callCount {
			case 1:
				return &http.Response{
					StatusCode: 504,
					Body:       io.NopCloser(strings.NewReader("Gateway Timeout")),
					Header:     make(http.Header),
				}, nil
			default:
				t.Fatalf("unhandled call %d", callCount)
			}
			return nil, errors.New("statement should not be reached")
		},
	}

	client := createClientWithEndpoint(settings, endpoint)

	endpoint.Deadline = time.Now().UTC().Add(deadlineDuration)

	ctx, cancel := context.WithDeadline(context.Background(), endpoint.Deadline)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext failed: %s", err)
	}

	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected context deadline exceeded error but got response status %d", resp.StatusCode)
	}

	if endpoint.CallCount != expectedCalls {
		t.Fatalf("expected call count %d, got %d", expectedCalls, endpoint.CallCount)
	}

	t.Logf("calls: %d, timings in ms: %+v", endpoint.CallCount, endpoint.Timings)
}

func TestRoundTripperFourthRun(t *testing.T) {
	// delay during roundtripper, return retriable
	// 	select {
	// 	case <-origReq.Context().Done():
	// 		return nil, origReq.Context().Err()
	// 	case <-time.After(waitTime):
	// 	}

	settings := NiceTransport{
		limiter: &Limiter{
			backoff: NewBackoff(1*time.Millisecond, 1*time.Millisecond, DefaultBackoffCoefficients),
		},
		maxAttempts:    5,
		defaultHeaders: map[string][]string{"Some-Header": {}},
	}

	expectedCalls := 1
	deadlineDuration := 250 * time.Millisecond

	endpoint := &MockEndpoint{
		CallCount: 0,
		Start:     time.Time{},
		Timings:   nil,
		Deadline:  time.Time{},
		Handler: func(endpoint *MockEndpoint, callCount int, req *http.Request) (*http.Response, error) {
			if _, exists := req.Header["Some-Header"]; exists {
				return nil, errors.New("expected header Some-Header with no values to not be sent in request")
			}
			checkDeadline(t, req, callCount, endpoint.Deadline)

			// Block until the deadline cuts the attempt, then report the
			// cancellation the way a real transport would, rather than
			// handing back a response for a dead context. Waiting for the
			// context rather than sleeping keeps this independent of the clock.
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(10 * time.Second): // the deadline is 250ms
				t.Fatal("context timeout for request never occur")
			}
			return nil, errors.New("statement should not be reached")
		},
	}

	client := createClientWithEndpoint(&settings, endpoint)

	endpoint.Deadline = time.Now().UTC().Add(deadlineDuration)

	ctx, cancel := context.WithDeadline(context.Background(), endpoint.Deadline)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext failed: %s", err)
	}

	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected context deadline exceeded error but got response status %d", resp.StatusCode)
	}

	if endpoint.CallCount != expectedCalls {
		t.Fatalf("expected call count %d, got %d", expectedCalls, endpoint.CallCount)
	}

	t.Logf("calls: %d, timings in ms: %+v", endpoint.CallCount, endpoint.Timings)
}

func TestRoundTripperMultithread(t *testing.T) {
	settings := NiceTransport{
		limiter: &Limiter{
			backoff: NewBackoff(10*time.Millisecond, 100*time.Millisecond, DefaultBackoffCoefficients),
		},
		maxAttempts: 5,
	}

	numberOfThreads := 10

	endpoint := &MockEndpoint{
		CallCount: 0,
		Start:     time.Time{},
		Timings:   nil,
		Deadline:  time.Time{},
		Handler: func(_ *MockEndpoint, callCount int, _ *http.Request) (*http.Response, error) {
			switch callCount % 2 {
			case 1:
				return &http.Response{
					StatusCode: 504,
					Body:       io.NopCloser(strings.NewReader("Gateway Timeout")),
					Header:     make(http.Header),
				}, nil
			case 0:
				return &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader("OK")),
					Header:     make(http.Header),
				}, nil
			}
			return nil, errors.New("statement should not be reached")
		},
	}

	client := createClientWithEndpoint(&settings, endpoint)

	var wg sync.WaitGroup

	wg.Add(numberOfThreads)
	for i := 0; i < numberOfThreads; i++ {
		go func() {
			defer wg.Done()
			resp, err := client.Get("http://example.com")
			if err != nil {
				t.Errorf("client.Get failed: %v", err)
				return
			}
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Errorf("reading body failed: %v", err)
			}
		}()
	}

	wg.Wait()

	t.Logf("calls: %d, timings in ms: %+v", endpoint.CallCount, endpoint.Timings)
}

func TestRoundTripperDefaults(t *testing.T) {
	expectedCalls := 1

	endpoint := &MockEndpoint{
		CallCount: 0,
		Start:     time.Time{},
		Timings:   nil,
		Deadline:  time.Time{},
		Handler: func(_ *MockEndpoint, callCount int, req *http.Request) (*http.Response, error) {
			checkGeneralRequestStuff(t, req, callCount, "GET", "")
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("OK")),
				Header:     make(http.Header),
			}, nil
		},
	}

	client := createClientWithEndpoint(nil, endpoint)
	resp, err := client.Get("http://example.com")
	if err != nil {
		t.Fatal("expected 200 response, got unexpected error", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 response but got status %d", resp.StatusCode)
	}
	if endpoint.CallCount != expectedCalls {
		t.Fatalf("expected call count %d, got %d", expectedCalls, endpoint.CallCount)
	}
}

func TestRoundTripperRetryAfterSeconds(t *testing.T) {
	expectedCalls := 2

	endpoint := &MockEndpoint{
		CallCount: 0,
		Start:     time.Time{},
		Timings:   nil,
		Deadline:  time.Time{},
		Handler: func(endpoint *MockEndpoint, callCount int, req *http.Request) (*http.Response, error) {
			checkGeneralRequestStuff(t, req, callCount, "GET", "")
			if userAgent := req.Header.Get("User-Agent"); userAgent != "something different" {
				t.Fatalf("unexpected header User-Agent %q", userAgent)
			}
			switch endpoint.CallCount {
			case 1:
				header := make(http.Header)
				header.Set("Retry-After", "1")
				return &http.Response{
					StatusCode: 429,
					Body:       io.NopCloser(strings.NewReader("OK")),
					Header:     header,
				}, nil
			case 2:
				return &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader("OK")),
					Header:     make(http.Header),
				}, nil
			default:
				t.Fatalf("unhandled call %d", endpoint.CallCount)
			}
			return nil, errors.New("statement should not be reached")
		},
	}

	settings := &NiceTransport{
		limiter: &Limiter{
			backoff: NewBackoff(1*time.Millisecond, 1*time.Millisecond, DefaultBackoffCoefficients),
		},
	}

	// test Clone while we are at it
	settings = settings.Clone()

	client := createClientWithEndpoint(settings, endpoint)
	req, err := http.NewRequest("GET", "http://example.com", nil)
	if err != nil {
		t.Fatal("unexpected error while making request:", err)
	}

	req.Header.Set("User-Agent", "something different")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("expected 200 response, got unexpected error", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 response but got status %d", resp.StatusCode)
	}
	if endpoint.CallCount != expectedCalls {
		t.Fatalf("expected call count %d, got %d", expectedCalls, endpoint.CallCount)
	}
}

func TestRoundTripperAttemptTimeoutOverride(t *testing.T) {
	// Deadline observed by the endpoint on each call
	deadlines := make(chan time.Duration, 16)

	endpoint := &MockEndpoint{
		CallCount: 0,
		Start:     time.Time{},
		Timings:   nil,
		Deadline:  time.Time{},
		Handler: func(_ *MockEndpoint, _ int, req *http.Request) (*http.Response, error) {
			if deadline, hasDeadline := req.Context().Deadline(); hasDeadline {
				deadlines <- time.Until(deadline)
			} else {
				deadlines <- 0
			}
			// Behave like a real transport and abort the attempt once the context is done
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			default:
			}
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("OK")),
				Header:     make(http.Header),
			}, nil
		},
	}

	backoff := NewBackoff(1*time.Millisecond, 1*time.Millisecond, DefaultBackoffCoefficients)

	// No attempt timeout in the transport, so the context value must be used
	settings := NiceTransport{
		limiter:     NewLimiter(backoff),
		maxAttempts: 2,
	}
	client := createClientWithEndpoint(&settings, endpoint)

	ctx := SetAttemptTimeoutInContext(context.Background(), 2*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext failed: %s", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected 200 response using attempt timeout from context, got error: %s", err)
	}
	resp.Body.Close()

	if observed := <-deadlines; observed < time.Second || observed > 3*time.Second {
		t.Fatalf("expected attempt deadline of about 2s from context override, got %s", observed)
	}

	// Attempt timeout in the transport is longer than the context value, which must win
	settings.attemptTimeout = 30 * time.Second
	client = createClientWithEndpoint(&settings, endpoint)

	ctx = SetAttemptTimeoutInContext(context.Background(), 100*time.Millisecond)
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext failed: %s", err)
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("expected 200 response using attempt timeout from context, got error: %s", err)
	}
	resp.Body.Close()

	if observed := <-deadlines; observed < 50*time.Millisecond || observed > time.Second {
		t.Fatalf("expected attempt deadline of about 100ms from context override, got %s", observed)
	}
}

func TestRoundTripperAttemptTimeoutBodyRead(t *testing.T) {
	// The body arrives only after the response headers, like any streamed or
	// large response. Reading it must not be aborted by the attempt timeout.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "5")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		w.Write([]byte("hello"))
	}))
	defer server.Close()

	// A 5 second attempt timeout is long enough that it never fires here, so a
	// failure is caused by the attempt being cancelled as soon as the response
	// headers have been received.
	transport := NewNiceTransportBuilder().
		SetAttemptTimeout(5 * time.Second).
		Build()
	client := &http.Client{Transport: transport}

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("unexpected error while getting response headers: %s", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("expected body to be readable after the response headers, got error: %s", err)
	}
	if string(data) != "hello" {
		t.Fatalf("expected body %q, got %q", "hello", string(data))
	}
}

func TestRoundTripperRetryWithConnectionLimit(t *testing.T) {
	// The body of a retriable response must be released before retrying,
	// because it holds on to its connection. With the connection limit of one
	// per host recommended in the readme, a body that is left open means the
	// retry waits for a connection that is never released.
	expectedCalls := 2

	var mutex sync.Mutex
	callCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mutex.Lock()
		callCount++
		currentCall := callCount
		mutex.Unlock()

		if currentCall == 1 {
			w.WriteHeader(503)
			io.WriteString(w, "Service Unavailable") //nolint:errcheck
			return
		}
		io.WriteString(w, "OK") //nolint:errcheck
	}))
	defer server.Close()

	downstream := http.DefaultTransport.(*http.Transport).Clone()
	downstream.MaxConnsPerHost = 1

	transport := NewNiceTransportBuilder().
		SetDownstreamTransport(downstream).
		SetLimiterBackoff(NewBackoff(1*time.Millisecond, 1*time.Millisecond, DefaultBackoffCoefficients)).
		SetMaxAttempts(3).
		Build()

	// The deadline only guards against the retry waiting for a connection forever
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext failed: %s", err)
	}

	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected 200 response, got unexpected error: %s", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 response but got status %d", resp.StatusCode)
	}

	mutex.Lock()
	defer mutex.Unlock()
	if callCount != expectedCalls {
		t.Fatalf("expected call count %d, got %d", expectedCalls, callCount)
	}
}

func TestRoundTripperCloneKeepsAttemptTimeout(t *testing.T) {
	// Cloning settings and passing them to the builder must keep the attempt
	// timeout, otherwise retries lose their per-attempt deadline
	deadlines := make(chan time.Duration, 4)

	endpoint := &MockEndpoint{
		CallCount: 0,
		Start:     time.Time{},
		Timings:   nil,
		Deadline:  time.Time{},
		Handler: func(_ *MockEndpoint, _ int, req *http.Request) (*http.Response, error) {
			if deadline, hasDeadline := req.Context().Deadline(); hasDeadline {
				deadlines <- time.Until(deadline)
			} else {
				deadlines <- 0
			}
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("OK")),
				Header:     make(http.Header),
			}, nil
		},
	}

	settings := NiceTransport{
		limiter: &Limiter{
			backoff: NewBackoff(1*time.Millisecond, 1*time.Millisecond, DefaultBackoffCoefficients),
		},
		maxAttempts:    1,
		attemptTimeout: 500 * time.Millisecond,
	}

	client := createClientWithEndpoint(settings.Clone(), endpoint)

	resp, err := client.Get("http://example.com")
	if err != nil {
		t.Fatal("expected 200 response, got unexpected error", err)
	}
	defer resp.Body.Close()

	if observed := <-deadlines; observed < 250*time.Millisecond || observed > time.Second {
		t.Fatalf("expected the clone to keep the 500ms attempt timeout, deadline observed was %s", observed)
	}
}

func TestRoundTripperLimiterReleasedAfterPanic(t *testing.T) {
	// The limiter is locked for the duration of an attempt, so a panic in the
	// downstream roundtripper must not leave it locked. Otherwise every later
	// request on this transport waits for a lock that is never released.
	expectedCalls := 2

	endpoint := &MockEndpoint{
		CallCount: 0,
		Start:     time.Time{},
		Timings:   nil,
		Deadline:  time.Time{},
		Handler: func(_ *MockEndpoint, callCount int, _ *http.Request) (*http.Response, error) {
			if callCount == 1 {
				panic("simulated panic in downstream roundtripper")
			}
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("OK")),
				Header:     make(http.Header),
			}, nil
		},
	}

	settings := NiceTransport{
		limiter: &Limiter{
			backoff: NewBackoff(1*time.Millisecond, 1*time.Millisecond, DefaultBackoffCoefficients),
		},
		maxAttempts: 1,
	}
	client := createClientWithEndpoint(&settings, endpoint)

	// The first request takes the limiter, then panics while it is held
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("expected the downstream roundtripper to panic")
			}
		}()
		resp, err := client.Get("http://example.com")
		if err == nil {
			resp.Body.Close()
		}
	}()

	// The second request must not wait for the panic leaked lock
	done := make(chan error, 1)
	go func() {
		resp, err := client.Get("http://example.com")
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected 200 response on the request after the panic, got error: %s", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected the request after the panic to complete, the limiter is still locked")
	}

	if endpoint.CallCount != expectedCalls {
		t.Fatalf("expected call count %d, got %d", expectedCalls, endpoint.CallCount)
	}
}

func TestRoundTripperDefaultHeadersKeepRequestClean(t *testing.T) {
	// Default headers are added to the request that is sent downstream, not to
	// the request given by the caller, which may be reused or shared with
	// other clients
	var sentHeader http.Header

	endpoint := &MockEndpoint{
		CallCount: 0,
		Start:     time.Time{},
		Timings:   nil,
		Deadline:  time.Time{},
		Handler: func(_ *MockEndpoint, _ int, req *http.Request) (*http.Response, error) {
			sentHeader = req.Header
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("OK")),
				Header:     make(http.Header),
			}, nil
		},
	}

	settings := NiceTransport{
		defaultHeaders: http.Header{"X-Default": {"default-value"}},
		limiter: &Limiter{
			backoff: NewBackoff(1*time.Millisecond, 1*time.Millisecond, DefaultBackoffCoefficients),
		},
		maxAttempts: 1,
	}
	client := createClientWithEndpoint(&settings, endpoint)

	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("http.NewRequest failed: %s", err)
	}
	req.Header.Set("X-Caller", "caller-value")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("expected 200 response, got unexpected error", err)
	}
	resp.Body.Close()

	if sent := sentHeader.Get("X-Default"); sent != "default-value" {
		t.Fatalf("expected default header X-Default %q on the request sent downstream, got %q", "default-value", sent)
	}
	if sent := sentHeader.Get("User-Agent"); sent == "" {
		t.Fatal("expected default User-Agent on the request sent downstream")
	}
	if sent := req.Header.Get("X-Default"); sent != "" {
		t.Fatalf("expected default header X-Default to not be added to the caller's request, got %q", sent)
	}
	if sent := req.Header.Get("User-Agent"); sent != "" {
		t.Fatalf("expected default User-Agent to not be added to the caller's request, got %q", sent)
	}
	if sent := req.Header.Get("X-Caller"); sent != "caller-value" {
		t.Fatalf("expected the caller's own header to be kept on the caller's request, got %q", sent)
	}
}

func TestRoundTripperCloseIdleConnections(t *testing.T) {
	// http.Client.CloseIdleConnections must reach the downstream transport,
	// otherwise its idle connections are never closed
	closed := make(chan struct{}, 4)

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "OK") //nolint:errcheck
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateClosed, http.StateHijacked:
			closed <- struct{}{}
		}
	}
	server.Start()
	defer server.Close()

	transport := NewNiceTransportBuilder().
		SetLimiterBackoff(NewBackoff(1*time.Millisecond, 1*time.Millisecond, DefaultBackoffCoefficients)).
		Build()
	client := &http.Client{Transport: transport}

	// Complete a request so that the connection becomes idle and reusable
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("unexpected error while making request: %s", err)
	}
	io.ReadAll(resp.Body) //nolint:errcheck
	resp.Body.Close()

	// The connection is idle, so nothing should have closed it yet
	select {
	case <-closed:
		t.Fatal("expected the idle connection to stay open before CloseIdleConnections is called")
	default:
	}

	client.CloseIdleConnections()

	select {
	case <-closed:
		// The connection was closed, so the call reached the downstream transport
	case <-time.After(2 * time.Second):
		t.Fatal("expected CloseIdleConnections to close the idle connection of the downstream transport")
	}
}

func TestZeroValueBuilder(t *testing.T) {
	// A builder that was not created by NewNiceTransportBuilder must still work
	var builder NiceTransportBuilder

	var sentHeader http.Header
	endpoint := &MockEndpoint{
		CallCount: 0,
		Start:     time.Time{},
		Timings:   nil,
		Deadline:  time.Time{},
		Handler: func(_ *MockEndpoint, _ int, req *http.Request) (*http.Response, error) {
			sentHeader = req.Header
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("OK")),
				Header:     make(http.Header),
			}, nil
		},
	}

	transport := builder.
		SetUserAgent("zero-value-agent").
		SetDefaultHeaders(http.Header{"X-Zero": {"zero"}}).
		SetLimiterBackoff(nil).
		SetMaxAttempts(1).
		SetDownstreamTransport(&MockTransport{Endpoint: endpoint}).
		Build()

	resp, err := (&http.Client{Transport: transport}).Get("http://example.com")
	if err != nil {
		t.Fatal("expected 200 response, got unexpected error", err)
	}
	resp.Body.Close()

	if userAgent := sentHeader.Get("User-Agent"); userAgent != "zero-value-agent" {
		t.Fatalf("expected User-Agent %q, got %q", "zero-value-agent", userAgent)
	}
	if sent := sentHeader.Get("X-Zero"); sent != "zero" {
		t.Fatalf("expected default header X-Zero %q, got %q", "zero", sent)
	}

	// A builder with no headers configured gets the default User-Agent
	transport = (&NiceTransportBuilder{}).
		SetMaxAttempts(1).
		SetDownstreamTransport(&MockTransport{Endpoint: endpoint}).
		Build()

	resp, err = (&http.Client{Transport: transport}).Get("http://example.com")
	if err != nil {
		t.Fatal("expected 200 response, got unexpected error", err)
	}
	resp.Body.Close()

	if userAgent := sentHeader.Get("User-Agent"); userAgent != DefaultUserAgent {
		t.Fatalf("expected default User-Agent %q, got %q", DefaultUserAgent, userAgent)
	}
}

// TestBuilderAttemptTimeout checks that an unset attempt timeout follows
// DefaultAttemptTimeout, and that NoTimeout still means no timeout.
func TestBuilderAttemptTimeout(t *testing.T) {
	restore := DefaultAttemptTimeout
	DefaultAttemptTimeout = 5 * time.Second
	defer func() { DefaultAttemptTimeout = restore }()

	transport := NewNiceTransportBuilder().Build()
	if transport.attemptTimeout != 5*time.Second {
		t.Errorf("expected the default attempt timeout, got %v", transport.attemptTimeout)
	}

	transport = NewNiceTransportBuilder().SetAttemptTimeout(NoTimeout).Build()
	if transport.attemptTimeout != NoTimeout {
		t.Errorf("expected no attempt timeout, got %v", transport.attemptTimeout)
	}

	transport = NewNiceTransportBuilder().SetAttemptTimeout(30 * time.Second).Build()
	if transport.attemptTimeout != 30*time.Second {
		t.Errorf("expected a 30s attempt timeout, got %v", transport.attemptTimeout)
	}

	// Set copies the timeout of an existing transport, which is already resolved
	transport = NewNiceTransportBuilder().Set(transport).Build()
	if transport.attemptTimeout != 30*time.Second {
		t.Errorf("expected a 30s attempt timeout, got %v", transport.attemptTimeout)
	}
}

func TestZeroValueTransport(t *testing.T) {
	// An unconfigured transport must report an error instead of panicking
	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("http.NewRequest failed: %s", err)
	}
	if _, err := (&NiceTransport{}).RoundTrip(req); err == nil {
		t.Fatal("expected an error from a transport without a limiter")
	}

	// Not specifying a downstream transport means http.DefaultTransport is used
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "OK") //nolint:errcheck
	}))
	defer server.Close()

	transport := NiceTransport{
		limiter:     NewLimiter(nil), // a nil backoff means the default backoff is used
		maxAttempts: 1,
	}
	resp, err := (&http.Client{Transport: &transport}).Get(server.URL)
	if err != nil {
		t.Fatal("expected 200 response, got unexpected error", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 response but got status %d", resp.StatusCode)
	}

	// A per host transport without config must report an error instead of panicking
	if _, err := (&NicePerHostTransport{}).RoundTrip(req); err == nil {
		t.Fatal("expected an error from a per host transport without config")
	}

	var manager PerHostConfigManager
	if _, _, err := manager.ForHost("example.com"); err == nil {
		t.Fatal("expected an error from a per host config manager without defaults")
	}
}

// TestBuildDefaultTransport checks how the downstream is resolved when the
// builder has none: the shared http.DefaultTransport, including a replaced
// global.
func TestBuildDefaultTransport(t *testing.T) {
	restore := http.DefaultTransport
	defer func() { http.DefaultTransport = restore }()

	response := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader("custom")),
		Header:     make(http.Header),
	}
	http.DefaultTransport = &MockTransport{Endpoint: &MockEndpoint{
		Handler: func(*MockEndpoint, int, *http.Request) (*http.Response, error) {
			return response, nil
		},
	}}

	transport := NewNiceTransportBuilder().Build()
	resp, err := (&http.Client{Transport: transport}).Get("http://example.com")
	if err != nil {
		t.Fatalf("expected the replacement global to be used, got error: %s", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("while reading body: %s", err)
	}
	if string(body) != "custom" {
		t.Errorf("expected the response of the replacement global, got %q", body)
	}

	http.DefaultTransport = restore

	// Without a replacement, builds share the global transport and its pool
	first := NewNiceTransportBuilder().Build()
	second := NewNiceTransportBuilder().Build()
	if first.downstreamTransport != restore || second.downstreamTransport != restore {
		t.Error("expected both transports to share http.DefaultTransport")
	}
}

// TestRoundTripperNonIdempotent checks which methods a transport error is
// retried for, and the two ways a POST can opt in to being replayed.
func TestRoundTripperNonIdempotent(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		headers   http.Header
		optIn     bool
		wantCalls int
	}{
		{name: "GET", method: http.MethodGet, wantCalls: 2},
		{name: "HEAD", method: http.MethodHead, wantCalls: 2},
		{name: "PUT", method: http.MethodPut, wantCalls: 2},
		{name: "DELETE", method: http.MethodDelete, wantCalls: 2},
		{name: "OPTIONS", method: http.MethodOptions, wantCalls: 2},
		{name: "POST", method: http.MethodPost, wantCalls: 1},
		{name: "PATCH", method: http.MethodPatch, wantCalls: 1},
		{
			name:      "POST with Idempotency-Key",
			method:    http.MethodPost,
			headers:   http.Header{"Idempotency-Key": {"abc"}},
			wantCalls: 2,
		},
		{name: "POST with context opt in", method: http.MethodPost, optIn: true, wantCalls: 2},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint := &MockEndpoint{
				Handler: func(_ *MockEndpoint, callCount int, _ *http.Request) (*http.Response, error) {
					if callCount == 1 {
						return nil, errors.New("simulated network error")
					}
					return &http.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader("OK")),
						Header:     make(http.Header),
					}, nil
				},
			}
			settings := &NiceTransport{
				limiter:     &Limiter{backoff: NewBackoff(time.Millisecond, time.Millisecond, DefaultBackoffCoefficients)},
				maxAttempts: 3,
			}
			client := createClientWithEndpoint(settings, endpoint)

			ctx := context.Background()
			if test.optIn {
				ctx = SetRetryNonIdempotentInContext(ctx)
			}
			req, err := http.NewRequestWithContext(ctx, test.method, "http://example.com", nil)
			if err != nil {
				t.Fatalf("http.NewRequestWithContext failed: %s", err)
			}
			req.Header = test.headers.Clone()
			if req.Header == nil {
				req.Header = make(http.Header)
			}

			resp, err := client.Do(req)
			if test.wantCalls == 2 {
				if err != nil {
					t.Fatalf("expected the retry to succeed, got error: %s", err)
				}
				resp.Body.Close()
			} else {
				if err == nil {
					resp.Body.Close()
					t.Fatal("expected the transport error to be final")
				}
			}
			if endpoint.CallCount != test.wantCalls {
				t.Fatalf("expected %d calls, got %d", test.wantCalls, endpoint.CallCount)
			}
		})
	}
}

// TestRoundTripperNonIdempotentStatus checks that a 429 or 503 is retried for
// any method, while a 504 Gateway Timeout is only retried when a replay is safe.
func TestRoundTripperNonIdempotentStatus(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		status    int
		wantCalls int
	}{
		{name: "POST 429", method: http.MethodPost, status: 429, wantCalls: 2},
		{name: "POST 503", method: http.MethodPost, status: 503, wantCalls: 2},
		{name: "POST 504", method: http.MethodPost, status: 504, wantCalls: 1},
		{name: "PUT 504", method: http.MethodPut, status: 504, wantCalls: 2},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint := &MockEndpoint{
				Handler: func(_ *MockEndpoint, callCount int, _ *http.Request) (*http.Response, error) {
					status := 200
					if callCount == 1 {
						status = test.status
					}
					return &http.Response{
						StatusCode: status,
						Body:       io.NopCloser(strings.NewReader("body")),
						Header:     make(http.Header),
					}, nil
				},
			}
			settings := &NiceTransport{
				limiter:     &Limiter{backoff: NewBackoff(time.Millisecond, time.Millisecond, DefaultBackoffCoefficients)},
				maxAttempts: 3,
			}
			client := createClientWithEndpoint(settings, endpoint)

			req, err := http.NewRequest(test.method, "http://example.com", nil)
			if err != nil {
				t.Fatalf("http.NewRequest failed: %s", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			defer resp.Body.Close()
			if endpoint.CallCount != test.wantCalls {
				t.Fatalf("expected %d calls, got %d", test.wantCalls, endpoint.CallCount)
			}
			if test.wantCalls == 1 && resp.StatusCode != test.status {
				t.Errorf("expected the final response to be %d, got %d", test.status, resp.StatusCode)
			}
		})
	}
}

// TestRoundTripperNoRetryAfterCancel checks that a caller who cancels mid attempt
// gets no retry, since the retry could not succeed either.
func TestRoundTripperNoRetryAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint := &MockEndpoint{
		Handler: func(_ *MockEndpoint, _ int, req *http.Request) (*http.Response, error) {
			cancel() // the caller gives up while this attempt is in flight
			return nil, req.Context().Err()
		},
	}
	settings := &NiceTransport{
		limiter:     &Limiter{backoff: NewBackoff(time.Millisecond, time.Millisecond, DefaultBackoffCoefficients)},
		maxAttempts: 3,
	}
	client := createClientWithEndpoint(settings, endpoint)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("http.NewRequestWithContext failed: %s", err)
	}

	if _, err := client.Do(req); err == nil {
		t.Fatal("expected an error from the cancelled request")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %s", err)
	}
	if endpoint.CallCount != 1 {
		t.Fatalf("expected 1 call, got %d", endpoint.CallCount)
	}
}

// TestShouldRetryRequest pins the retry rule table directly, since the limiter
// also refuses a request whose context is done, which would mask the guard in an
// end to end test.
func TestShouldRetryRequest(t *testing.T) {
	request := func(method string, opts ...func(*http.Request)) *http.Request {
		req, err := http.NewRequest(method, "http://example.com", nil)
		if err != nil {
			t.Fatalf("http.NewRequest failed: %s", err)
		}
		for _, opt := range opts {
			opt(req)
		}
		return req
	}
	cancelled := func(req *http.Request) {
		ctx, cancel := context.WithCancel(req.Context())
		cancel()
		*req = *req.WithContext(ctx)
	}
	optedIn := func(req *http.Request) {
		*req = *req.WithContext(SetRetryNonIdempotentInContext(req.Context()))
	}
	withKey := func(req *http.Request) { req.Header.Set("Idempotency-Key", "abc") }

	response := func(status int) *http.Response {
		return &http.Response{StatusCode: status, Header: make(http.Header)}
	}
	networkError := errors.New("simulated network error")

	tests := []struct {
		name string
		req  *http.Request
		resp *http.Response
		err  error
		want bool
	}{
		{"network error, GET", request(http.MethodGet), nil, networkError, true},
		{"network error, PUT", request(http.MethodPut), nil, networkError, true},
		{"network error, OPTIONS", request(http.MethodOptions), nil, networkError, true},
		{"network error, POST", request(http.MethodPost), nil, networkError, false},
		{"network error, PATCH", request(http.MethodPatch), nil, networkError, false},
		{"network error, POST with key", request(http.MethodPost, withKey), nil, networkError, true},
		{"network error, POST opted in", request(http.MethodPost, optedIn), nil, networkError, true},
		{"network error after cancel", request(http.MethodGet, cancelled), nil, networkError, false},
		{"429 after cancel", request(http.MethodGet, cancelled), response(429), nil, false},
		{"429, POST", request(http.MethodPost), response(429), nil, true},
		{"503, POST", request(http.MethodPost), response(503), nil, true},
		{"504, PUT", request(http.MethodPut), response(504), nil, true},
		{"504, POST", request(http.MethodPost), response(504), nil, false},
		{"504, POST with key", request(http.MethodPost, withKey), response(504), nil, true},
		{"200, GET", request(http.MethodGet), response(200), nil, false},
		{"no response and no error", request(http.MethodGet), nil, nil, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldRetryRequest(test.req, test.resp, test.err); got != test.want {
				t.Errorf("expected %v, got %v", test.want, got)
			}
		})
	}
}
