# nicehttp

A nice HTTP client written in golang with rate limiting, backoff and max retries. Respects the 429 Retry-After header.

[![Go Report Card](https://goreportcard.com/badge/github.com/rohfle/nicehttp)](https://goreportcard.com/report/github.com/rohfle/nicehttp)
[![Test](https://github.com/rohfle/nicehttp/actions/workflows/test.yml/badge.svg)](https://github.com/rohfle/nicehttp/actions/workflows/test.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/rohfle/nicehttp.svg)](https://pkg.go.dev/github.com/rohfle/nicehttp)

## Usage

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rohfle/nicehttp"
)

func main() {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	downstream := http.DefaultTransport.(*http.Transport).Clone()
	downstream.MaxConnsPerHost = 1

	backoff := nicehttp.NewBackoff(1*time.Second, 120*time.Second, nicehttp.DefaultBackoffCoefficients)
	// Or set the coefficients directly:
	// backoff = nicehttp.NewBackoff(1*time.Second, 120*time.Second, nicehttp.BackoffCoefficients{
	// 	Success: 0.5,
	// 	Fail:    1.5,
	// 	Jitter:  0.3,
	// })

	transport := nicehttp.NewNiceTransportBuilder().
		SetDefaultHeaders(headers).
		SetUserAgent("your-user-agent-here/0.1").
		SetMaxAttempts(10).
		SetAttemptTimeout(120*time.Second).
		SetLimiterBackoff(backoff).
		SetDownstreamTransport(downstream).
		Build()

	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Minute,
	}

	// The attempt timeout and the number of attempts can be overridden per
	// request through the context
	ctx := nicehttp.SetAttemptTimeoutInContext(context.Background(), 5*time.Second)
	ctx = nicehttp.SetMaxAttemptsInContext(ctx, 5)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
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
}
```

A runnable version of this example is in [`example_test.go`](./example_test.go).

## Per host configuration

`NewPerHostConfigManagerFromMap` takes a `map[string]any` based config
and returns a factory that builds one `NiceTransport` per host on first use:

```json
{
  "defaults": {
    "attempt_timeout": "30s",
    "max_attempts": 2,
    "default_headers": { "Authorization": "Bearer xxxx" },
    "backoff": {
      "min_wait": "2s",
      "max_wait": "30s",
      "coefficients": { "success": 0.5, "fail": 1.5, "jitter": 0.3 }
    }
  },
  "hosts": {
    "api.example.com": { "max_attempts": 5 },
    "api.internal.example.com": { "tls": { "insecure_skip_verify": true } },
    "slow.example.com": { "attempt_timeout": "none" }
  }
}
```

```go
// data is the document above decoded, for example with encoding/json
manager, err := nicehttp.NewPerHostConfigManagerFromMap(data)
if err != nil {
	return err
}
transport, err := manager.CreateNicePerHostTransport()
if err != nil {
	return err
}
client := &http.Client{Transport: transport}
```

A runnable version is in [`example_test.go`](./example_test.go) (`ExampleNewPerHostConfigManagerFromMap`).

- **Inheritance**: Each host entry inherits from defaults. Unset keys are inherited, and a missing or null host entry falls back entirely to defaults.
- **Attempt Timeout** (`attempt_timeout`): Accepts a duration string, "`none`", "`0s`", or `0` for no timeout. (In Go, `TimeoutNotSet` inherits from `DefaultAttemptTimeout`, whereas `0` explicitly disables it).
- **Max Attempts** (`max_attempts`): Accepts an integer, at least 1.
- **Default Headers** (`default_headers`): Replaces the inherited value per header name; it does not append. An empty list or `null` removes a header, and an empty string is sent as an empty value.
- **Backoff** (`backoff`): Accepts `min_wait`, `max_wait`, and `coefficients`, and is merged onto the inherited backoff config.
- **TLS** (`tls`): Supports `insecure_skip_verify` only. Unlike other fields, this replaces the inherited TLS configuration entirely rather than merging.
- **Host Matching**: Host names are lowercased with any trailing dots ignored, and must not contain a port (requests match against `URL.Hostname()`). Unknown keys or invalid value types will result in an error.
- **Transport Cache**: Transports are created lazily on first use and cached. Defined hosts remain cached permanently, while dynamic hosts (those absent from config that fallback to defaults) are capped at 256 using LRU eviction. Evicted hosts reset with a fresh limiter and backoff.


## Retries

An attempt is retried up to `maxAttempts`. An attempt timeout is always retried, and a cancelled or expired
caller context never is.

After a transport error or a `504 Gateway Timeout` the client cannot know whether the request reached the
server, so only a request that is safe to replay is retried:

- safe methods: `GET`, `HEAD`, `PUT`, `DELETE`, `OPTIONS`, `TRACE`
- a `POST` or `PATCH` carrying an `Idempotency-Key` header
- a `POST` or `PATCH` whose context comes from `SetRetryNonIdempotentInContext`

A `429 Too Many Requests`, honouring `Retry-After`, or a `503 Service Unavailable` is retried for any method,
since the server answered that it did not process the request.

A request that is not safe to replay can opt in to retrying using context:

```go
// A POST after a transport error or a 504 is only retried when the context says so
ctx := nicehttp.SetRetryNonIdempotentInContext(context.Background())
req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
```

Runnable in [`example_test.go`](./example_test.go) (`ExampleSetRetryNonIdempotentInContext`).

## License

MIT License

Copyright (c) 2025 Rohan Fletcher

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
