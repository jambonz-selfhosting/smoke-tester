# 8. Run-ID tagging and mandatory resource cleanup

- **Status:** Accepted
- **Date:** 2026-04-18
- **Updated:** 2026-04-18 (fixture mechanism restated in Go test vocabulary per [ADR-0012](0012-go-test-as-test-runner.md))
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** hygiene, safety, testing

## Context

The harness writes to a shared, externally-managed jambonz cluster (ADR-0002). Parallel CI runs, multiple developers, and crashed/interrupted test sessions will all try to provision resources simultaneously. Without a discipline, leaked Applications, PhoneNumbers, Carriers, and Users will accumulate, pollute the cluster, and cause flaky tests when resources collide by name.

## Decision

Every resource the harness provisions is tagged with a **run-id** and guaranteed to be cleaned up.

- **Run-id generation.** A short ULID-based `runID` is produced once per test process in `TestMain` and exposed via a package-level helper. It can be overridden via `RUN_ID=...` for debugging.
- **Name prefix convention.** Every provisioned resource's `name` (or closest equivalent field) starts with `it-<runID>-`. Example: `it-01HGJ-say-app`.
- **Per-test cleanup.** Resources are created via a `provision.Managed(t, ...)` helper in the provisioning package. The helper registers a `t.Cleanup(...)` that issues `DELETE`, swallowing 404s so cleanup is idempotent. `t.Cleanup` runs on both pass and fail — this is the runner contract we rely on.
- **Explicit cleanup fallback.** Any resource created outside `provision.Managed` must call `t.Cleanup(func() { ... })` directly. Code review enforces the "one or the other" rule; `provision.Managed` is the default path.
- **Orphan sweep on process start.** `TestMain` in the top-level integration package lists every supported resource type and deletes any whose name starts with `it-` and whose age exceeds `ORPHAN_TTL_HOURS` (default 2). This catches resources leaked by crashed prior runs without racing against concurrent runs.
- **No global "delete all `it-*`" button.** Too dangerous in a shared cluster; age-gating is required.

## Consequences

- Positive: parallel runs (`go test -parallel`, multiple developers, CI) cannot collide on resource names.
- Positive: crashed runs self-heal on the next invocation — no manual janitor work.
- Positive: the cluster operator can always see which resources come from the test harness at a glance (`it-` prefix).
- Positive: `t.Cleanup` + `provision.Managed` is simpler than pytest's nested fixture graph and gives the same correctness guarantee.
- Negative: every provisioning SDK method must respect the naming convention. Discipline has to be enforced in code review and, ideally, in a lint-style check (a simple `go vet`-compatible analyser is one option).
- Negative: `ORPHAN_TTL_HOURS` trades off collision risk (too short — might delete a concurrent run's resources) against leak tolerance (too long — clutter persists). Start at 2 hours; tune based on observed run durations.
- Follow-up: add a small CLI (`cmd/cleanup`) for ad-hoc orphan inspection with `--dry-run`.

## Alternatives considered

### Option A — No tagging; rely on per-test teardown only
Rejected: a single crashed run leaks resources permanently. No recovery path without manual intervention.

### Option B — Dedicated test account/namespace in jambonz
Would be cleaner if jambonz supported strong account-level isolation with per-account quotas, but the harness still needs a discipline for parallel runs inside that account. Tagging is required regardless.

### Option C — Tag with a timestamp, not a run-id
Rejected: timestamps collide under parallelism and make it hard to group all resources from a single run for diagnostics.

## References

- [ADR-0002](0002-scope-external-test-harness.md) — shared cluster is the root cause of this discipline.
- [ADR-0012](0012-go-test-as-test-runner.md) — `TestMain` + `t.Cleanup` are where this is enforced.
