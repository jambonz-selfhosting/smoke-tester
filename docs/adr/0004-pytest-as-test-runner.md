# 4. pytest as the test runner

- **Status:** Superseded by [ADR-0012](0012-go-test-as-test-runner.md)
- **Date:** 2026-04-18
- **Superseded:** 2026-04-18
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** testing, tooling, superseded

> **Superseded on 2026-04-18.** The project switched from Python to Go; the
> corresponding test runner decision is now [ADR-0012](0012-go-test-as-test-runner.md).
> Record below is kept for history.

## Context

The harness is structurally a test suite: many small scenarios, each with setup, exercise, assert, teardown. It needs fixtures (ngrok tunnel, FastAPI app, run_id, cleanup), markers (to gate SIP modes by NAT environment), parallelism (for faster release-gate runs), and good failure output.

## Decision

Use `pytest` as the sole test runner.

- Session-scoped fixtures for: ngrok tunnel lifecycle, FastAPI webhook app lifecycle, orphan-sweep at session start, `run_id` generation.
- Function-scoped fixtures for: per-test cleanup of provisioned resources (guaranteed by `yield` + finalizer).
- Markers for SIP mode gating: `sip_client`, `sip_carrier`, `sip_inbound`. A conftest hook auto-skips modes not supported by the current `BEHIND_NAT` environment.
- `pytest-xdist` for parallel execution; isolation guaranteed by the `run_id` tagging convention (ADR-0008).
- `pytest-asyncio` for async test support where needed (httpx clients, FastAPI interactions).
- Failure output includes: webhook event log for the failing `X-Test-Id`, pjsip trace, and the matching `RecentCalls` record from jambonz. Implemented via a conftest `pytest_runtest_makereport` hook.

## Consequences

- Positive: battle-tested fixture model maps cleanly onto the "setup/teardown per test" requirement.
- Positive: markers + conftest let the *same* suite run on laptop (behind NAT) and Debian box (public IP) without branching.
- Positive: rich ecosystem (xdist, asyncio, html-report) available when needed.
- Negative: pytest fixture scoping is powerful but has sharp edges (session vs. module vs. function); contributors need a minimum level of pytest literacy.
- Follow-up: document the fixture graph in `ARCHITECTURE.md` or a dedicated testing guide once the shape stabilises.

## Alternatives considered

### Option A — `unittest` (stdlib)
Rejected: weaker fixture model, no marker system, no parallel runner out of the box.

### Option B — A hand-rolled script runner
Rejected: reinvents pytest badly. We'd end up re-implementing fixtures, markers, and reporting.

### Option C — Behave / pytest-bdd (Gherkin-style)
Rejected: the natural-language layer adds cost without a matching benefit for a single-team, developer-authored suite. Tests here are not executable specifications for non-engineers.

## References

- ADR-0007 (three SIP test modes) — defines the markers.
- ADR-0008 (run-id tagging) — enables safe parallelism under xdist.
