# 11. Go with modules for dependency management

- **Status:** Accepted
- **Date:** 2026-04-18
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** tooling, go
- **Supersedes:** [ADR-0003](0003-python-venv-for-dependencies.md)

## Context

The harness needs to act as a full SIP UA and UAS with fine-grained control over signaling and media. Python was chosen originally **because of** pjsua2 ([ADR-0005](0005-pjsua2-for-sip-rtp.md)) — the only mature Python SIP+RTP option. A spike run on 2026-04-18 against `sip.jambonz.me` ([spikes/001-sipgo-diago/](../../spikes/001-sipgo-diago/)) showed that [sipgo](https://github.com/emiago/sipgo) + [diago](https://github.com/emiago/diago) (both Go) deliver the same capabilities with dramatically better developer ergonomics:

- No SWIG / `./configure` / native build step
- `go get` + `go build` in under 15 seconds
- Plain Go stack traces (no generated binding layer to debug through)
- Goroutines + channels instead of pjsua2's thread-registration + callback-on-pjsip-thread model
- First-class SIP message access in handlers (easier assertions)
- The author positions diago as *"fast and easy testable VoIP apps"* — i.e. exactly this project

Since Python's selection was purely downstream of pjsua2, removing pjsua2 removes the reason to use Python.

## Decision

The harness is written in **Go**, with the standard Go toolchain and `go.mod` / `go.sum` for dependency management.

- Minimum Go version: **1.22** (recorded in `go.mod` via the `go` directive).
- Dependencies are declared in `go.mod`; versions are locked in `go.sum`. Both files are committed. This gives us reproducible builds out of the box, without a separate lockfile tool.
- Build outputs go to `./bin/` (gitignored). Binaries are throwaway — anyone can reproduce them with `go build`.
- No dependency-management wrapper (no `dep`, no `vgo`, no vendor directory by default). If a vendored build is ever required for air-gapped CI, `go mod vendor` is the escape hatch — recorded in a future ADR if used.
- Full environment reset is: `rm -rf bin/ && go clean -modcache && go build ./...`. The Go module cache lives in `$GOMODCACHE` (user-global) and can be wiped without touching the project.

## Consequences

- Positive: one tool (`go`) installs dependencies, builds, tests, formats, lints (via `go vet`) — zero separate ecosystem.
- Positive: single static binary output per entrypoint — trivially portable to the Debian/EC2 release-gate box (`GOOS=linux GOARCH=amd64 go build`).
- Positive: `go.sum` provides bit-for-bit reproducibility out of the box — no extra lockfile decision to make.
- Positive: native cross-compile means the same developer laptop can build Linux binaries for the public-IP box.
- Negative: the team has zero prior Go experience at project start. Mitigated by Go's deliberately small surface area (the language spec is ~90 pages; idiomatic Go is learnable in days, not months). Documented in the README so new contributors know what to read first.
- Negative: loses the existing `jambonz-python-sdk` verb-builder library — no Go equivalent exists. We will hand-roll a small verb-JSON builder against the public JSON schemas when needed. Scoped as ~a day of work and non-blocking for v1.
- Neutral: Go's standard `testing` package replaces pytest; see [ADR-0012](0012-go-test-as-test-runner.md).

## Alternatives considered

### Option A — Stay on Python with pjsua2
Rejected after the spike. pjsua2 is capable but carries permanent ergonomic tax: SWIG-generated tracebacks, native build step that must be redone whenever the venv is rebuilt, threading model where callbacks run on pjsip threads (requires manual `libRegisterThread` on every Python thread that touches the binding), and opaque C-side crashes. Every release cycle re-pays this cost.

### Option B — Python with a different SIP stack (aiosip, sipsimple, pyVoIP)
Rejected: surveyed and none are production-viable for a release gate that needs UAC+UAS, both UDP+TCP, digest auth, real media with codec control, and NAT-aware SDP. See ADR-0013 alternatives section.

### Option C — Node.js with drachtio-srf (the framework jambonz itself uses)
Strong ecosystem-reuse appeal, but drachtio-srf is signaling-only and expects rtpengine or FreeSWITCH as a media sidecar. For a test harness that wants to play a WAV and detect tones in-process, bolting on rtpengine is more operational surface than Go's in-process media. Revisit if the harness ever needs B2BUA / proxy conformance testing.

### Option D — Rust (rsipstack)
Rejected: rsipstack is signaling-only, media stack would be hand-rolled on top of pion or similar, and the language learning curve is steeper than Go's for a single-maintainer project.

## References

- [ADR-0013](0013-sipgo-diago-for-sip-rtp.md) — the SIP/RTP stack that made this switch worthwhile.
- [ADR-0012](0012-go-test-as-test-runner.md) — test runner decision.
- [spikes/001-sipgo-diago/](../../spikes/001-sipgo-diago/) — the spike results that unblocked this decision.
- Go Modules reference: <https://go.dev/ref/mod>.
