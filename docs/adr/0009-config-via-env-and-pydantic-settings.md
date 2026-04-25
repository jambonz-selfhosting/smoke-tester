# 9. Configuration via `.env` and a typed Go settings struct

- **Status:** Accepted
- **Date:** 2026-04-18
- **Updated:** 2026-04-18 (implementation restated in Go vocabulary after [ADR-0011](0011-go-modules-for-dependencies.md); `PUBLIC_IP` scope refined per [ADR-0014](0014-symmetric-rtp-media-latch.md))
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** configuration, tooling, go

## Context

The harness points traffic at different jambonz clusters (staging, local dev, pre-release canary) depending on who is running it. It also gates behaviour on environment capability (`BEHIND_NAT`, `PUBLIC_IP`). Configuration must be easy to override locally without editing code, easy to template for CI/EC2, and validated early so bad configs fail at session start, not mid-test.

## Decision

Configuration is loaded from a `.env` file and process environment into a typed Go `Settings` struct, with `.env.example` as the template.

- `internal/config/config.go` defines a single `Settings` struct with typed fields for every knob (strings, ints, `net.IP`, `bool`, `time.Duration`).
- A small, zero-dependency loader reads `.env` (if present) into `os.Setenv` unless a value is already set in the process env, then populates the struct. No third-party library is strictly required; [godotenv](https://github.com/joho/godotenv) or the stdlib-only loader used in the spike ([spikes/001-sipgo-diago/main.go](../../spikes/001-sipgo-diago/main.go)) is sufficient.
- Field validators reject obviously-bad values (e.g. non-URL for `JAMBONZ_API_URL`, non-IP for `PUBLIC_IP`).
- `TestMain` in the top-level integration package loads `Settings` once; a failure here aborts the whole process with a readable error.
- `.env` is `.gitignore`d; `.env.example` is committed and enumerates every supported variable with comments.
- Precedence: process env > `.env` > struct defaults. Tests never hardcode cluster endpoints.

**Supported variables** (see `.env.example` for full list):
- `JAMBONZ_API_URL`, `JAMBONZ_API_KEY`, `JAMBONZ_ACCOUNT_SID` — required.
- `JAMBONZ_SIP_DOMAIN`, `JAMBONZ_SIP_PROXY` — SIP targeting.
- `PUBLIC_IP` — **only required for `Carrier` and `Inbound` SIP modes** ([ADR-0014](0014-symmetric-rtp-media-latch.md)); `Client`-mode UAC tests work behind NAT without it.
- `BEHIND_NAT` — environment capability flag; gates `Inbound` mode ([ADR-0007](0007-three-sip-test-modes.md)).
- `NGROK_AUTHTOKEN`, `NGROK_DOMAIN` — tunnel config.
- `RUN_ID`, `LOG_LEVEL`, `ORPHAN_TTL_HOURS` — test-run knobs.

## Consequences

- Positive: fail-fast on config errors; typed fields prevent whole classes of bugs.
- Positive: same `.env.example` works as self-documenting config reference.
- Positive: secrets (`JAMBONZ_API_KEY`, `NGROK_AUTHTOKEN`) stay out of git by default.
- Positive: Go struct + validation adds zero runtime dependencies (stdlib only if we avoid godotenv).
- Positive: `PUBLIC_IP` no longer required for UAC/Client tests — developer onboarding drops a step.
- Negative: without a library like `pydantic-settings`, config validation is hand-written — ~50 lines. Trivial cost for the validation win.
- Follow-up: a `cmd/config-check` binary prints resolved config (with secrets masked) for debugging environment issues.

## Alternatives considered

### Option A — Raw `os.Getenv` reads scattered through code
Rejected: no validation, no central list of knobs, easy to drift.

### Option B — YAML config file + a Go YAML library
Rejected: the harness's knobs are overwhelmingly secrets and per-machine values, which belong in env vars, not committed files. `.env` is the right shape.

### Option C — A heavy config library (viper, koanf)
Rejected for v1: they solve multi-source merging and live reload, neither of which we need. `.env` + stdlib is enough. Revisit if we ever need profile-switching or hot reload.

### Option D — Go flags (`flag.StringVar`)
Rejected: `go test` is the entry point; flags would need plumbing through `testing.M.Run`. Env vars compose better with CI/CD secret stores.

## References

- [ADR-0011](0011-go-modules-for-dependencies.md) — Go toolchain this config lives in.
- [ADR-0007](0007-three-sip-test-modes.md) — uses `BEHIND_NAT` and `PUBLIC_IP` for mode gating.
- [ADR-0014](0014-symmetric-rtp-media-latch.md) — why `PUBLIC_IP` is conditional, not required.
