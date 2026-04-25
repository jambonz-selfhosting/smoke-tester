# 10. Release-gate scope, not CI-per-PR

- **Status:** Accepted
- **Date:** 2026-04-18
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** scope, process

## Context

Integration test suites can be run at different cadences: on every PR, on every merge to main, nightly, or only before release. Each cadence makes different tradeoffs around speed, flakiness tolerance, infrastructure cost, and test depth. jambonz is a multi-repo open-source project with frequent releases; the test harness targets a full running cluster, involves real SIP media and an external ngrok tunnel, and has a non-trivial runtime.

## Decision

`smoke-tester` is a **pre-release gate**. It runs against a candidate jambonz cluster **before a release is tagged** — not on every PR and not in the per-repo CI pipelines of individual jambonz components.

- Primary invocation: a human or release automation runs `make it` (or `pytest`) against a configured cluster.
- Design budget: a full run should complete within a single-digit number of minutes, but **failure detection beats fast feedback**. It is acceptable for the suite to take longer than typical unit CI.
- Test depth: the suite covers representative call flows for every major verb and every SIP mode, plus REST CRUD. It does **not** aim for exhaustive combinatorial coverage.
- Flakiness policy: real-network tests can flake. A test that fails must produce enough diagnostics (webhook log, pjsip trace, RecentCalls) to triage in one pass. Chronically-flaky tests are quarantined with a skip marker and a linked issue until fixed — never left flaky and green.

## Consequences

- Positive: freedom to use real SIP, real RTP, real ngrok without the infrastructure/flakiness cost of running on every PR.
- Positive: concentrated investment in failure diagnostics rather than in speed optimisation.
- Negative: regressions are caught later than in a per-PR model — integration bugs can sit in main until the next release candidate.
- Negative: humans must remember to run it. Mitigation: document the step in the release runbook; optionally wire a GitHub Action that runs on a `v*-rc*` tag push.
- Follow-up: a future ADR may add a per-PR "smoke" subset (REST CRUD only, no SIP) if early-signal is needed without the full cluster dependency.

## Alternatives considered

### Option A — Run the full suite on every jambonz PR
Rejected: requires every jambonz repo's CI to stand up a cluster and configure ngrok. Too much infra per run; flakiness would stall all PRs.

### Option B — Nightly run against staging
A reasonable complement, not a replacement. Could be added later without changing this ADR's shape.

### Option C — No automated pre-release testing; manual verification only
Rejected: the whole point of this repo is to make pre-release verification reproducible and cheap-to-rerun.

## References

- ADR-0002 (scope) — black-box external harness.
- ADR-0008 (cleanup) — makes repeated release-candidate runs against the same cluster safe.
