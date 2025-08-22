package nicehttp_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	headers.Set("Authentication", "Bearer xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	downstream := http.DefaultTransport.(*http.Transport).Clone()
	downstream.MaxConnsPerHost = 1
	// downstream.TLSClientConfig = &tls.Config{
	//     InsecureSkipVerify: true, // Skip certificate verification
	// }

	backoff := nicehttp.NewExponentialBackoff(1*time.Second, 120*time.Second, nicehttp.DefaultExponentialBackoffCoefficients)
	// backoff := nicehttp.NewExponentialBackoff(1*time.Second, 120*time.Second, nicehttp.ExponentialBackoffCoefficients{
	// 	   Success: 0.5,
	// 	   Fail:    1.5,
	// 	   Jitter:  0.3,
	// })

	transport, err := nicehttp.NewNiceTransportBuilder().
		SetDefaultHeaders(headers).
		SetUserAgent("your-user-agent-here/0.1").
		SetMaxAttempts(10).
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
