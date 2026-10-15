# Changelog

Notable changes per tagged release, newest first. 

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

[v0.3.0]: https://github.com/rohfle/nicehttp/releases/tag/v0.3.0
