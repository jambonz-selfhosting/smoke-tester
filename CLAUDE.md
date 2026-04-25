# CLAUDE.md — orientation for future Claude sessions

This file is loaded automatically into every Claude Code session that runs in this directory. Read it first; it will route you to the right documents and save both of us from re-litigating decisions that are already settled.

## What this repo is

`smoke-tester` is an **external, black-box integration-test harness** for the open-source jambonz platform. It runs **before tagging a release** and drives real traffic at a pre-existing jambonz cluster to verify three surfaces end-to-end: REST API, verb/webhook execution, and SIP+RTP media.

- The jambonz cluster is **out of scope** — managed by a separate deployment tool. This repo only points traffic at it.
- The canonical API/verb/webhook specs live in a **sibling repo at `<jambonz-fern-config-checkout>/`**. That is authoritative — never guess at field names or paths; read the fern YAML.

## Read before doing anything

1. **[HANDOFF.md](HANDOFF.md)** — Where we are right now: what's done, what's in progress, what's next, open questions. Start here to avoid redoing work or re-asking questions already answered.
2. **[docs/adr/README.md](docs/adr/README.md)** — Architecture Decision Record index. Fifteen ADRs so far, all current. The reasoning for every significant choice (language, test runner, SIP library, NAT strategy, contract-testing policy) is captured there, including the *rejected* alternatives. Do not re-propose a rejected alternative without reading the ADR that rejected it and writing a new ADR that supersedes it.
3. **[docs/coverage-matrix.md](docs/coverage-matrix.md)** — What is tested, what is scheduled, what is explicitly out of scope. Tier 1 → Tier 7 is the **implementation order**; do not skip ahead. Each row has a Feature status and a Contract status, and updating these rows is part of every PR.
4. **[ARCHITECTURE.md](ARCHITECTURE.md)** — System shape, component responsibilities, repo layout, config surface.

**End-of-session protocol:** update [HANDOFF.md](HANDOFF.md) `Now` / `Next` / `Session log` before stopping. Claude sessions are stateless — if it isn't in HANDOFF, ADRs, or the coverage matrix, it's gone.

**Phase 2 is mid-flight (2026-04-19).** Webhook + ngrok infra is wired but the first test (`gather`) is blocked on an `X-Test-Id` correlation bug. See HANDOFF → "Known issues blocking Phase 2 gather test" + "Resume plan for Phase 2" before touching webhook code.

## Non-negotiable rules

These are captured in ADRs — not my preferences. If you disagree, the path is to write an ADR that supersedes the existing one, not to quietly violate it.

- **Stack is Go** ([ADR-0011](docs/adr/0011-go-modules-for-dependencies.md)). Python was the original plan and was rejected after a spike; the rejection is permanent absent a new ADR.
- **SIP stack is [sipgo](https://github.com/emiago/sipgo) + [diago](https://github.com/emiago/diago)** ([ADR-0013](docs/adr/0013-sipgo-diago-for-sip-rtp.md)). Not pjsua2. Not sip.js. Not drachtio-srf.
- **Test runner is `go test`** ([ADR-0012](docs/adr/0012-go-test-as-test-runner.md)). Use `TestMain`, `t.Cleanup`, and `testenv.Require*` helpers.
- **Every provisioned resource is name-prefixed `it-<runID>-` and has guaranteed cleanup** ([ADR-0008](docs/adr/0008-run-id-tagging-and-cleanup.md)). Use `provision.Managed(t, ...)`; never create a resource outside a `t.Cleanup`.
- **Every response from jambonz is contract-validated against the canonical JSON Schema** ([ADR-0015](docs/adr/0015-contract-testing.md)). REST body, webhook payload, WS message — all of them. A schema violation fails the test, regardless of the feature-level assertion. Missing schemas fail the test with `contract.ErrNoSchema` — never silently skip validation.
- **UAC tests begin sending outbound RTP (silence is fine) on dialog answer** ([ADR-0014](docs/adr/0014-symmetric-rtp-media-latch.md)). This opens the NAT pinhole and lets jambonz's symmetric-RTP latch work. The UA wrapper enforces this by default; do not disable it.
- **Scope of work is a release gate, not per-PR CI** ([ADR-0010](docs/adr/0010-release-gate-scope.md)).

## Test-design rules (failure-fast pattern)

When a test fails the first question is always *which test, which step, why?*. Under `-parallel`, output from concurrent tests interleaves and a raw `t.Errorf` line is easy to miss. Every test in this repo MUST follow the pattern below so failures surface in the end-of-run `=== FAILURE SUMMARY ===` block at the bottom of stderr.

**The pattern:**

1. Open the test with `ctx := WithTimeout(t, <budget>)`. The watchdog records a timeout failure naming the last step that was running.
2. Wrap each phase in `s := Step(t, "kebab-case-name") ... s.Done()`. The name MUST match a bullet in the test's `Steps:` doc comment so operators can read the failure log without opening the source.
3. Report failures via `s.Errorf` / `s.Fatalf` / `s.Fatal` — **never raw `t.Errorf` / `t.Fatalf`** in the main test goroutine. The `StepCtx` methods both fail the test AND record the failure (test, step, message) for the summary.
4. Code that runs in a goroutine spawned by the test (multi-leg callee handlers, listener/agent goroutines) uses `GoroutineFailf(t, "callee:NAME", "fmt", args...)`. Same summary participation, just without the `[step:*]` start/ok markers.
5. For setup helpers that must `t.Fatalf` directly (no `*StepCtx` in scope, e.g. `resolveFixture`), call `recordFailure(t, "step-name", msg)` immediately before the `t.Fatalf` so the failure still reaches the summary.
6. REST + SIP response-code assertions use `provision.AsAPIError(err)` / `provision.StatusOf(err)` for HTTP, and `call.AnsweredStatus()` / `call.ReceivedByStatus(code)` for SIP. Don't unmarshal `*APIError` by hand.

**Why this matters:** without the summary line, debugging a `-parallel` flake means scrolling through 1000+ lines of interleaved log — minutes per investigation. With the summary, the test name + step + reason are visible in the last 3 lines of output.

If you find yourself reaching for raw `t.Errorf` / `t.Fatalf` in test code (not helpers), you're breaking the pattern — wrap the phase in a `Step` first.

## Canonical schema sources

Two upstream repos publish the authoritative contracts. Read these directly; never guess at field names or paths.

- **REST API (OpenAPI 3.0):** `<api-server-checkout>/lib/swagger/swagger.yaml` — the live spec served by the `api-server` at `GET /v1/swagger.json`. Single file (~5200 lines), covers every REST endpoint. Base server is `/v1`.
- **Verbs + callbacks + components (JSON Schema 2020-12):** `<jambonz-schema-checkout>/` — the [`@jambonz/schema`](https://github.com/jambonz/schema) package. 33 verb schemas (`verbs/`), 32 callback schemas (`callbacks/`), 42 component schemas (`components/`), plus the root `jambonz-app.schema.json`. Every verb and action-hook shape we need.
- **WebSocket API (AsyncAPI 3.0):** `<jambonz-fern-config-checkout>/fern/apis/async/call.yml` — used in Tier 7 only.

The fern spec at `<jambonz-fern-config-checkout>/` is the docs-site bundle (it's partially derived and has gaps — verbs.yaml only covers `say` + `play`). Treat it as secondary; prefer `api-server/swagger.yaml` + `@jambonz/schema` for contract validation. See [ADR-0015](docs/adr/0015-contract-testing.md).

Gaps (anything not in the two canonical sources above) are filled by hand-rolled JSON Schemas in `smoke-tester/internal/schemas/local/`, each with a `TODO: upstream` comment. The directory shrinking over time is the convergence signal.

## Implementation order

From the [coverage matrix](docs/coverage-matrix.md):

1. **Tier 1** — REST platform CRUD, every resource, minimal fields. No SIP.
2. **Tier 2** — REST depth: PUT, bulk, read-only endpoints, error cases.
3. **Tier 3** — Core call-flow verbs (say/play/pause/gather/dial/hangup/answer/redirect/tag).
4. **Tier 4** — Advanced verbs.
5. **Tier 5** — AI verbs (credential-gated).
6. **Tier 6** — `PUT /Calls/{sid}` matrix.
7. **Tier 7** — WebSocket API.

Land a tier fully (feature + contract for every row) before starting the next one. The harness must be useful at every checkpoint.

## Working style preferences

From prior sessions:

- **Use ADRs aggressively.** Every significant choice gets an ADR. Append-only: if a decision changes, supersede the old ADR with a new one; do not edit the old.
- **Design first, code second.** User prefers architecture-as-code and is building this project partly to practise ADR discipline on a small repo.
- **Tiered, reviewable diffs.** Do not drop 50 files in one turn without milestones. Announce the slice you're about to build and stop at its boundary for review.
- **Spike before committing** to a library or language choice. Spikes live in `spikes/NNN-<name>/` and are deletable.
- **Honest pushback.** When the user proposes something that conflicts with an ADR or a sensible boundary (e.g. scope creep, skipping Tier 1), say so.

## Quick context

- User is `hoan.h.luu@jambonz.org`, a jambonz maintainer with frequent releases to cut.
- Target cluster for tests: `https://jambonz.me/api/v1`, SIP at `sip.jambonz.me`. Credentials live in `.env` (gitignored). **The SIP password that appears in git/session history from the spike must be rotated** if it hasn't been already.
- Developer machine: macOS (Homebrew Go 1.26+). Debian/EC2 public-IP box is planned for Tier 4+ modes that need inbound SIP.
