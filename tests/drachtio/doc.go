// Package drachtio holds RFC 4028 session-timer tests that exercise a live
// sbc-inbound (drachtio) deployment directly over SIP.
//
// These tests are long-running: individual cases wait on session-timer
// refreshes and expiries, with waits in the ~45-100s range. Because of that
// cost, the whole package is gated behind the `drachtio` build tag — plain
// `go test ./...`, `make test`, and `make test-report` compile this package
// (this file has no build tag, so the package always compiles to something)
// but every test file carries `//go:build drachtio`, so none of the actual
// tests are included in a default build. The result is an empty, harmless
// `[no test files]` package in ordinary runs.
//
// Run this suite explicitly via `make test-drachtio`.
package drachtio
