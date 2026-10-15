package nicehttp_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rohfle/nicehttp"
)

func ExampleNewNiceTransportBuilder() {
	// Mock HTTP server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello world")
	}))
	defer server.Close()

	headers := make(http.Header)
	headers.Set("Authorization", "Bearer xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	downstream := http.DefaultTransport.(*http.Transport).Clone()
	downstream.MaxConnsPerHost = 1
	// downstream.TLSClientConfig = &tls.Config{
	//     InsecureSkipVerify: true, // Skip certificate verification
	// }

	backoff := nicehttp.NewBackoff(1*time.Second, 120*time.Second, nicehttp.DefaultBackoffCoefficients)
	// backoff := nicehttp.NewBackoff(1*time.Second, 120*time.Second, nicehttp.BackoffCoefficients{
	// 	   Success: 0.5,
	// 	   Fail:    1.5,
	// 	   Jitter:  0.3,
	// })

	transport := nicehttp.NewNiceTransportBuilder().
		SetDefaultHeaders(headers).
		SetUserAgent("your-user-agent-here/0.1").
		SetMaxAttempts(10).
		SetAttemptTimeout(120 * time.Second).
		SetLimiterBackoff(backoff).
		SetDownstreamTransport(downstream).
		Build()

	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Minute,
		// CheckRedirect:
	}

	ctx := nicehttp.SetAttemptTimeoutInContext(context.Background(), 5*time.Second)
	ctx = nicehttp.SetMaxAttemptsInContext(ctx, 5)
	req, err := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	resp, err := client.Do(req)
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

func ExampleNewPerHostConfigManagerFromMap() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello world")
	}))
	defer server.Close()

	// The defaults apply to every host, a host entry overrides single keys
	manager, err := nicehttp.NewPerHostConfigManagerFromMap(map[string]any{
		"defaults": map[string]any{
			"max_attempts":    3,
			"attempt_timeout": "30s",
		},
		"hosts": map[string]any{
			// This host gets the inherited 3 attempts and no attempt timeout
			"slow.example.com": map[string]any{"attempt_timeout": "none"},
			// This host gets the inherited 30s timeout and one more attempt
			"api.example.com": map[string]any{"max_attempts": 5},
		},
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	// One transport per host is created on the first request to it
	transport, err := manager.CreateNicePerHostTransport()
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Minute,
	}
	defer client.CloseIdleConnections()

	// The host in the URL picks the config. The test server listens on 127.0.0.1,
	// which has no entry, so this request uses the defaults.
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

func ExampleSetRetryNonIdempotentInContext() {
	var servedFirst atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if servedFirst.CompareAndSwap(false, true) {
			// The server did not process the request, so a retry is safe
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		fmt.Fprint(w, "created")
	}))
	defer server.Close()

	backoff := nicehttp.NewBackoff(1*time.Millisecond, 1*time.Millisecond, nicehttp.DefaultBackoffCoefficients)
	transport := nicehttp.NewNiceTransportBuilder().
		SetMaxAttempts(2).
		SetLimiterBackoff(backoff).
		Build()

	client := &http.Client{Transport: transport}

	// A POST is not retried after a 504, since the client cannot know whether the
	// server processed it. This context opts in.
	ctx := nicehttp.SetRetryNonIdempotentInContext(context.Background())
	body := strings.NewReader("payload")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, body)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	resp, err := client.Do(req)
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
	fmt.Println("status:", resp.StatusCode, "body:", string(data))
	// Output: status: 200 body: created
}
