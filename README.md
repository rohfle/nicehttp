# nicehttp

A nice HTTP client written in golang with rate limiting, backoff and max retries. Respects the 429 Retry-After header.

Requires Go 1.26 or newer.

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

`PerHostConfigFromMap` parses a `map[string]any` document (decoded with
`encoding/json`, including `json.Number`, or `yaml.v3`), and
`NewNicePerHostTransport` builds `NiceTransport` per host on first use:

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
    "slow.example.com": { "attempt_timeout": "none" }
  }
}
```

```go
// data is the document above decoded, for example with encoding/json.
config, err := nicehttp.PerHostConfigFromMap(data)
if err != nil {
	return err
}
// Shared by every host; nil means http.DefaultTransport.
config.Downstream = downstream
transport, err := nicehttp.NewNicePerHostTransport(config)
if err != nil {
	return err
}
client := &http.Client{Transport: transport}
```

The same config can be built in Go, where unset fields inherit from `Defaults`:

```go
config := nicehttp.PerHostConfig{
	Defaults: &nicehttp.HostConfig{MaxAttempts: 3},
	Hosts: map[string]*nicehttp.HostConfig{
		"api.example.com": {MaxAttempts: 5},
	},
}
transport, err := nicehttp.NewNicePerHostTransport(config)
```

A runnable version is in [`example_test.go`](./example_test.go) (`ExampleNewNicePerHostTransport`).

- **Inheritance**: Each host entry inherits from defaults. Unset keys are inherited, and a missing or null host entry falls back entirely to defaults.
- **Attempt Timeout** (`attempt_timeout`): Accepts a duration string, "`none`", "`0s`", or `0` for no timeout. In Go, a nil `HostConfig.AttemptTimeout` inherits `DefaultAttemptTimeout`, while a pointer to `NoTimeout` disables it.
- **Max Attempts** (`max_attempts`): Accepts a whole number from 1 to 100.
- **Default Headers** (`default_headers`): Replaces the inherited value per header name; it does not append. An empty list or `null` removes a header, and an empty string is sent as an empty value. Names and values must be valid HTTP header tokens.
- **Backoff** (`backoff`): Accepts `min_wait`, `max_wait`, and `coefficients`, and is merged onto the inherited backoff config.
- **Host Matching**: Host names may be IP literals or DNS-style names (letters, digits, `-`, `_`, IDN characters). Case, surrounding spaces, one trailing dot and IPv6 brackets are normalized; ports, schemes, paths, empty labels, spaces and multiple trailing dots are rejected.
- **Shared Downstream**: Every host transport sends requests through `PerHostConfig.Downstream`, so one connection pool serves all hosts. TLS, proxies and other transport tuning belong there, not in the per-host config.
- **Transport Cache**: Transports are created lazily on first use and cached. Defined hosts remain cached permanently, while dynamic hosts (those absent from config that fallback to defaults) are capped at 256 using LRU eviction. Evicted hosts reset with a fresh limiter and backoff.


## Retries

An attempt is retried up to `maxAttempts`, and a cancelled or expired caller context is never retried.

`DefaultAttemptTimeout` is `NoTimeout`, so do not rely on an implicit per-attempt deadline: bound requests with
`http.Client.Timeout` or a context deadline, as the examples above do with 10 minutes. When the next backoff wait
cannot start before the caller's deadline, the client stops retrying and returns the last response instead of
waiting for the deadline to expire. That response keeps its body when it is at most 4 KB; a larger body is dropped
rather than returned truncated. This early return needs a deadline on the request context: `http.Client.Timeout`
is not visible to the transport, so a request bounded only by it is not cut short, and fails when the timeout fires.

After an attempt timeout, a transport error or a `504 Gateway Timeout` the client cannot know whether the
request reached the server, so only a request that is safe to replay is retried:

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

Copyright (c) 2026 Rohan Fletcher

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
