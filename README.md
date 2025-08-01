# nicehttp

A nice HTTP client written in golang with rate limit, backup and max retries. Respects 429 Retry-After header.

[![Go Report Card](https://goreportcard.com/badge/github.com/rohfle/nicehttp)](https://goreportcard.com/report/github.com/rohfle/nicehttp)
[![Test](https://github.com/rohfle/nicehttp/actions/workflows/test.yml/badge.svg)](https://github.com/rohfle/nicehttp/actions/workflows/test.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/rohfle/nicehttp.svg)](https://pkg.go.dev/github.com/rohfle/nicehttp)

## Usage

```go
package main

import (
    "fmt"
    "io"
    "net/http"

    "github.com/rohfle/nicehttp"
)

func main() {
    headers := make(http.Header)
    headers.Set("User-Agent", "your-user-agent-here/0.1")

    settings := &nicehttp.Settings{
        DefaultHeaders: headers,
        RequestInterval: 1 * time.Second,
        Backoff:         1 * time.Second,
        MaxBackoff:      120 * time.Second,
        MaxTries:        10,
        MaxConnsPerHost: 1,
    }

    client := nicehttp.NewClient(settings)

    resp, err := client.Get(server.URL)
    if err != nil {
        fmt.Println("error:", err)
    }
    defer resp.Body.Close()
    data, err := io.ReadAll(resp.Body)
    if err != nil {
        fmt.Println("error:", err)
    }
    fmt.Println("got resp:", string(data))
    // Output: got resp: hello world
}

```

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
