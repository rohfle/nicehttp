package nicehttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	mt.Endpoint.CallCount += 1
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

	transport, err := NewNiceTransportBuilder().Set(settings).Build()
	if err != nil {
		panic(err)
	}

	return &http.Client{
		Transport: transport,
	}
}

func TestRoundTripperFirstRun(t *testing.T) {
	// context with deadline set
	// limiter wait with context expires
	backoff := NewExponentialBackoff(100*time.Millisecond, 1*time.Second, DefaultExponentialBackoffCoefficients)
	settings := NiceTransport{
		defaultHeaders: http.Header{
			"Empty-Header": []string{}, // test headers with empty value list
		},
		limiter:  NewLimiter(backoff),
		maxTries: 5,
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
				retryTime := time.Now().UTC().Add(100 * time.Millisecond)
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
			backoff: NewExponentialBackoff(10*time.Millisecond, 30*time.Millisecond, DefaultExponentialBackoffCoefficients),
		},
		maxTries: 5,
	}

	expectedBody := "This is the body"
	expectedCalls := 5
	deadlineDuration := 200 * time.Millisecond

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
	backoff := NewExponentialBackoff(500*time.Millisecond, 500*time.Millisecond, DefaultExponentialBackoffCoefficients)
	settings, err := NewNiceTransportBuilder().
		SetLimiterBackoff(backoff).
		SetMaxTries(5).Build()
	if err != nil {
		t.Fatalf("while building settings: %s", err)
	}

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
			backoff: NewExponentialBackoff(1*time.Millisecond, 1*time.Millisecond, DefaultExponentialBackoffCoefficients),
		},
		maxTries:       5,
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

			time.Sleep(500 * time.Millisecond)

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
			backoff: NewExponentialBackoff(10*time.Millisecond, 100*time.Millisecond, DefaultExponentialBackoffCoefficients),
		},
		maxTries: 5,
	}

	numberOfThreads := 10

	endpoint := &MockEndpoint{
		CallCount: 0,
		Start:     time.Time{},
		Timings:   nil,
		Deadline:  time.Time{},
		Handler: func(endpoint *MockEndpoint, callCount int, req *http.Request) (*http.Response, error) {
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
		Handler: func(endpoint *MockEndpoint, callCount int, req *http.Request) (*http.Response, error) {
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
			backoff: NewExponentialBackoff(1*time.Millisecond, 1*time.Millisecond, DefaultExponentialBackoffCoefficients),
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
func ExampleNiceTransportBuilder() {
	// Mock HTTP server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello world")
	}))
	defer server.Close()

	headers := make(http.Header)
	headers.Set("Authentication", "Bearer xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	downstream := http.DefaultTransport.(*http.Transport).Clone()
	downstream.MaxConnsPerHost = 1
	// downstream.TLSClientConfig = &tls.Config{
	//     InsecureSkipVerify: true, // Skip certificate verification
	// }

	backoff := DefaultExponentialBackoff
	// backoff = NewExponentialBackoff(1*time.Second, 120*time.Second, DefaultExponentialBackoffCoefficients)
	// backoff = NewExponentialBackoff(1*time.Second, 120*time.Second, ExponentialBackoffCoefficients{
	// 	   Success: 0.5,
	// 	   Fail:    1.5,
	// 	   Jitter:  0.3,
	// })

	transport, err := NewNiceTransportBuilder().
		SetDefaultHeaders(headers).
		SetUserAgent("your-user-agent-here/0.1").
		SetMaxTries(10).
		SetAttemptTimeout(120 * time.Second).
		SetLimiterBackoff(backoff).
		SetDownstreamTransport(downstream).
		Build()
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	client := &http.Client{
		Transport: transport,
		// CheckRedirect:
		// Timeout:
	}

	resp, err := client.Get(server.URL)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("got resp:", string(data))
	// Output: got resp: hello world
}
