# Changelog

Notable changes per tagged release, newest first.

## [v0.5.0] - 2026-10-26

### Added

- **Retry fallback**: when the next backoff wait cannot start before the caller's deadline, the last response is returned instead of only a deadline error. Its body is kept when it reached EOF within 4 KB; a larger or broken body is returned empty, with `ContentLength`, `Content-Length`, `TransferEncoding` and trailers corrected.
- **Config fuzzing**: `FuzzPerHostConfigFromMap` and a 20s fuzz job in CI.

### Changed

- **Attempt timeout**: it is retried under the same replay-safety rule as a transport error, not always; the documentation now says so.
- **Attempt limits**: builder and context attempt counts are clamped to 1..100. A negative or `TimeoutNotSet` context attempt timeout resolves to `DefaultAttemptTimeout` instead of an already expired deadline.
- **Request bodies**: unseekable bodies are read under the caller's context and sent unbuffered when a single attempt cannot retry. `RoundTrip` no longer modifies the caller's request, and a failed rewind aborts the retry instead of resending a partial body.
- **Retry-After**: a malformed value is ignored silently instead of written to the global logger.

### Fixed

- **Cancellation**: a wait that ends because the caller's context is done returns the context error, and a retryable transport error no longer overwrites the saved response with nil.

### CI

- Tests run with `-race -shuffle=on -count=2`, a 92% coverage gate, and a `GOMAXPROCS=1` job.
- `golangci-lint` v2 with `revive`, `govulncheck`, a `go mod tidy` check, and a 20s fuzz job.

## [v0.4.1] - 2026-10-25

### Changed

- **Dependencies**: Updated `github.com/neilotoole/fifomu` to v0.2.0, which brings `golang.org/x/sync` v0.20.0. The `fifomu.Mutex` API used by `Limiter` is unchanged.

## [v0.4.0] - 2026-10-25

### Changed

- **Per-Host API**: `PerHostConfig` (defaults, hosts and the shared downstream) plus `HostConfig` (one host) replace `PerHostConfigManager`. Parse with `PerHostConfigFromMap` and build with `NewNicePerHostTransport(config)`. Hand built configs are validated like parsed ones.
- **Shared Downstream**: Every per-host transport uses `PerHostConfig.Downstream`, so one connection pool serves all hosts. `nil` uses `http.DefaultTransport`; `CloseIdleConnections` delegates to it.
- **Transport Defaults**: A builder without a downstream now shares `http.DefaultTransport` instead of cloning it, so connections pool across clients.
- **Validation**: `max_attempts` is limited to 1 to 100; host names reject ports, schemes, paths, spaces, empty labels and malformed brackets; header names and values are checked at parse time; `json.Number` is accepted.
- **TLS**: Removed the `tls` config key and `PerHostConfig.TLS`; configure TLS on the shared downstream instead.

### Removed

- `PerHostConfigManager`, `DefaultPerHostConfig`, `NewPerHostConfigManagerFromMap`, `CreateNicePerHostTransportFromMap`, `PerHostConfig.CreateNiceTransport`, and `PerHostConfigManager.ForHost`.

## [v0.3.0] - 2026-10-16

### Added

- **Per-Host Configuration**: Added `NewPerHostConfigManagerFromMap` supporting defaults, host overrides, name normalization, and LRU caching for dynamic hosts (capped at 256).
- **Timeouts & Contexts**: Added `TimeoutNotSet`, support for disabling timeouts via `"none"`/`"0s"`/`0`, and context helpers for non-idempotent retries.
- **Transport Utilities**: Added `NewBackoffFromMap`, `SetDownstreamTransport`, and `CloseIdleConnections`.
- **Documentation**: Added comprehensive doc comments to every exported declaration.
- **Documentation**: Documented `DefaultBackoffCoefficients`, and cleaned up receivers, parameter names and the internal return order.

### Changed

- The module requires Go 1.26, and CI tests 1.26 and 1.27.
- **Backoffs & Retries**: Renamed `ExponentialBackoff` to `Backoff` (removed the `Backoff` interface) and refined retry rules for errors, `504`, `429`, and `503`.
- **Headers & User-Agent**: Updated header inheritance (replacing per name, with `null` or empty lists to remove) and set default User-Agent to `nicehttp/<version>`.
- **API Cleanups**: `PerHostConfigManager.ForHost` now reports default usage; `Build()` and `Done()` no longer return redundant errors.

### Fixed

- Fixed bugs involving context timeouts, response body cleanup during retries, and `Clone` state.
- Fixed host normalization issues with ports, stray spaces, and IPv6 brackets.
- Resolved various concurrency edge cases, header mutation bugs, and zero-value handling.

[v0.5.0]: https://github.com/rohfle/nicehttp/releases/tag/v0.5.0
[v0.4.1]: https://github.com/rohfle/nicehttp/releases/tag/v0.4.1
[v0.4.0]: https://github.com/rohfle/nicehttp/releases/tag/v0.4.0
[v0.3.0]: https://github.com/rohfle/nicehttp/releases/tag/v0.3.0
