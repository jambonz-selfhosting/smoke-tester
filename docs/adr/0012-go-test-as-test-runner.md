# 12. `go test` as the test runner

- **Status:** Accepted
- **Date:** 2026-04-18
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** testing, tooling, go
- **Supersedes:** [ADR-0004](0004-pytest-as-test-runner.md)

## Context

With the switch to Go ([ADR-0011](0011-go-modules-for-dependencies.md)), pytest is no longer the test runner. We need a runner that covers:

- Per-test setup/teardown with guaranteed cleanup
- Gating tests on environment capability (e.g. `BEHIND_NAT`, `PUBLIC_IP` presence) — see [ADR-0007](0007-three-sip-test-modes.md)
- Concurrent execution for faster release-gate runs, with isolation via run-ID tagging ([ADR-0008](0008-run-id-tagging-and-cleanup.md))
- Rich on-failure diagnostics (webhook events, RTP capture files, SIP message traces)

Go's standard `testing` package covers this natively, without adding third-party runner dependencies.

## Decision

Use Go's built-in `go test` as the sole test runner.

- Tests live next to the code they cover (`_test.go` files) plus an `integration/` package for end-to-end scenarios that need the webhook app and SIP UA up.
- **Setup/teardown:** `t.Cleanup(func() { ... })` replaces pytest finalizers. It runs on both pass and fail, which is the contract we need for ADR-0008 cleanup.
- **Environment gating:** a small helper in `internal/testenv` inspects config and calls `t.Skip(...)` with a clear reason when a test requires capability the current environment lacks. Examples: `testenv.RequirePublicIP(t)`, `testenv.RequireNotBehindNAT(t)`.
- **Shared fixtures:** `TestMain(m *testing.M)` in each package starts session-scoped dependencies (webhook app, ngrok tunnel, orphan sweep) once before tests run and tears down after. Within a test, `t.Helper()` + constructor functions like `newCallContext(t, cfg)` replace pytest's nested fixture graph.
- **Parallelism:** tests opt in with `t.Parallel()` at the top. Isolation is enforced by the run-ID tagging convention ([ADR-0008](0008-run-id-tagging-and-cleanup.md)) plus a per-test `testID` passed as `X-Test-Id` on every INVITE. `go test -parallel N` controls fan-out.
- **Failure diagnostics:** tests attach context via `t.Logf` and `t.Cleanup` dumpers that run only on failure (checked via `t.Failed()`). Dumped artifacts: webhook events for the failing `testID`, path to the captured RTP WAV, the matching RecentCalls record from the jambonz REST API.
- **Assertions:** the standard library is enough for most checks. We adopt [testify/require](https://pkg.go.dev/github.com/stretchr/testify) only where it materially improves readability (e.g. `require.NoError` + deep-equal structs). No testify imports in hot paths.
- **No BDD layer, no code generation.** The harness is authored by engineers, for engineers.

## Consequences

- Positive: zero-dependency test runner, always works with `go test ./...`, plays well with IDE tooling and CI.
- Positive: `TestMain` + `t.Cleanup` + `t.Skip` map cleanly to the pytest patterns they replace, without bringing pytest's fixture-scope complexity.
- Positive: `go test -race` catches concurrency bugs for free — valuable with goroutine-per-call tests.
- Positive: `-run` regex filtering and `-count=N` repetition are first-class for debugging flakes.
- Negative: Go's test output is plainer than pytest's. Mitigation: a thin reporter helper can print a one-line-per-test summary on session end.
- Negative: no equivalent of `pytest.mark.parametrize` — Go uses table-driven tests instead. Different shape, same effect, slightly more verbose.
- Follow-up: wrap the "provision → call → assert → cleanup" pattern in a small in-repo helper so individual tests stay short and uniform.

## Alternatives considered

### Option A — `ginkgo` / `gomega` (BDD-style)
Rejected: adds a second vocabulary on top of `testing`, and BDD's natural-language layer has no payoff when tests aren't read by non-engineers. Keeps the project idiomatic.

### Option B — `testify/suite` (xUnit-style suite structs)
Rejected for the same reason testify is kept narrow: we don't need suite-setup beyond `TestMain`, and the struct-based sugar adds weight without solving a concrete problem.

### Option C — A hand-rolled scenario runner outside `go test`
Rejected: reinvents everything `go test` already provides (parallelism, filtering, timing, race detector, IDE integration).

## References

- [ADR-0011](0011-go-modules-for-dependencies.md) — Go toolchain choice.
- [ADR-0007](0007-three-sip-test-modes.md) — defines the capability gates tests use.
- [ADR-0008](0008-run-id-tagging-and-cleanup.md) — the cleanup discipline that `t.Cleanup` enforces.
- Go testing docs: <https://pkg.go.dev/testing>.
