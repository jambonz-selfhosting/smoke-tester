# 15. Contract-test every jambonz response against the canonical spec

- **Status:** Accepted
- **Date:** 2026-04-18
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** testing, contracts, api, webhooks

## Context

A jambonz release can break customer integrations in two distinct ways:

1. **Behaviour breaks** — a verb stops working, a REST call returns the wrong result.
2. **Contract breaks** — a field is renamed, a type changes, a required field goes missing. The feature "works" but every downstream consumer silently breaks.

The original design of `smoke-tester` ([ARCHITECTURE.md](../../ARCHITECTURE.md), ADRs 0001–0014) focused on class (1). Class (2) is equally important for a release gate: customers of the open-source project integrate against documented APIs and webhooks; an accidental rename in a point release is exactly the kind of regression a release gate exists to catch.

The canonical contract lives in two upstream repos (discovered on 2026-04-18, after this ADR's first draft — which referenced the fern bundle). These supersede the fern bundle as the primary source:

- **REST API:** `<api-server-checkout>/lib/swagger/swagger.yaml` — OpenAPI 3.0, the live spec served by `api-server` at `/v1/swagger.json`. Complete coverage of every REST endpoint in one file.
- **Verbs + callbacks + components:** `<jambonz-schema-checkout>/` — the [`@jambonz/schema`](https://github.com/jambonz/schema) NPM package. JSON Schema draft 2020-12. 33 verb schemas (`verbs/`), 32 callback payload schemas (`callbacks/`), 42 shared component schemas (`components/`), plus the root `jambonz-app.schema.json`.
- **WebSocket API:** `<jambonz-fern-config-checkout>/fern/apis/async/call.yml` — AsyncAPI 3.0.

The fern bundle at `<jambonz-fern-config-checkout>/` is the docs-site source and has gaps (`verbs.yaml` only covers `say`+`play`; most callback shapes are MDX-only). It is kept as a secondary cross-check, not the primary contract.

## Decision

Every jambonz response observed by the harness — REST body, webhook payload, WebSocket message — is **validated against a JSON Schema** before being consumed by a test. A schema violation is a test failure, not a warning, regardless of whether the feature-level assertion would have passed.

### Scope of validation

- **REST responses (jambonz → us):** validated on every call, against the schema derived from `platform.yaml` or `calls.yaml`. Schema load happens once in `TestMain`.
- **Webhook bodies (jambonz → us):** validated on receipt by the webhook server. Violations are recorded per `X-Test-Id` and cause the originating test to fail at assertion time.
- **Verb arrays (us → jambonz):** validated before we return them in a webhook response. Guards against bugs in *our* test scripts masquerading as jambonz bugs.
- **WebSocket messages:** in scope when the WebSocket API tier (Tier 7 in [coverage-matrix.md](../coverage-matrix.md)) is built. Same validator, different schema source.

### Spec-source hierarchy

When choosing a schema for a given interaction:

1. **Prefer the fern YAML** (`jambonz-fern-config`). Generated from the same source that powers <https://docs.jambonz.org>; authoritative.
2. **Fall back to a hand-rolled schema** in `smoke-tester/schemas/` when the YAML has a known gap (most verb and action-hook payloads). Each local schema file starts with a `# TODO: upstream to jambonz-fern-config` marker and a link to the MDX page it was authored from.
3. **Never silently skip validation.** If no schema exists for an interaction, the test fails with `no-schema: <interaction>` rather than returning undefined-behaviour "pass." Missing schemas are a backlog item, not an escape hatch.

As hand-rolled schemas are upstreamed to `jambonz-fern-config` and released, the local copy is deleted and the generator switches to the YAML path. This is the convergence strategy, not permanent duplication.

### Library

**[santhosh-tekuri/jsonschema](https://github.com/santhosh-tekuri/jsonschema)** (v5+, MIT, draft 2020-12). Chosen for: OpenAPI 3.1 compatibility (which uses JSON Schema 2020-12), precise error messages with JSON pointers, no cgo, actively maintained. The older `xeipuuv/gojsonschema` was considered and rejected — draft 7 only, less precise errors.

### Strictness policy

- **`additionalProperties: true`** by default. The goal is to catch **renames, removals, and type changes**, not to forbid jambonz from *adding* optional fields in a minor release. A new field appearing is not a break; an existing field vanishing is.
- **Required fields are enforced strictly.** If the schema says `sid` is required and the response omits it, the test fails.
- **Type changes are enforced strictly.** `string` → `integer` on an existing field fails.
- When a release-notes entry announces a breaking field rename or removal, the corresponding schema is updated in the same PR that bumps the harness to that release. This is a conscious choice, not automated.

### Violations are structured

A validation failure reports:

- JSON pointer to the offending field (e.g. `/data/0/call_sid`)
- Expected type / required-ness
- Actual value snippet
- Schema source (file path + line in `platform.yaml` or local file)
- The test's `X-Test-Id` (so failures correlate with webhook logs and jambonz RecentCalls)

Output is dumped via `t.Errorf` so `go test -v` shows the full diff without truncation.

### Handling the "no schema" case

When a response has no schema (first time a new resource/verb is exercised, or an upstream gap not yet filled), the test fails with a specific error type `contract.ErrNoSchema` and a link to `docs/coverage-matrix.md` explaining the policy. This turns gaps into visible backlog, not silent pass-throughs.

## Consequences

- Positive: a jambonz PR that accidentally renames `call_sid` → `callSid` is caught by the release gate with a precise diff, even if no test feature-asserts on that field.
- Positive: the `schemas/` directory + coverage matrix becomes a de-facto spec-completeness tracker — each upstream merge deletes a local file, visible in git log.
- Positive: the same validator covers REST, webhooks, and eventually WebSocket — one mechanism, three surfaces.
- Negative: authoring hand-rolled schemas for verbs/hooks is real work. Mitigated by doing it tier-by-tier, only for the hooks a tier exercises.
- Negative: "no schema" failures may feel obstructive during early development. Mitigated by authoring the schema as part of adding the test — not a separate chore.
- Negative: `additionalProperties: true` means we will **miss** the class of bug where jambonz starts emitting an undocumented field that customers then depend on. This is a deliberate tradeoff — we'd rather be lenient than block every minor release on unannounced additions.
- Follow-up: if the `additionalProperties: true` choice ever silently admits a real regression, that motivates tightening per-resource and a new ADR.

## Alternatives considered

### Option A — Feature-assert only; no contract validation
Rejected: the whole premise of this repo is to catch what customers would hit in production. Customers depend on field shapes, not just "does this endpoint return 200."

### Option B — Hand-written Go structs as the contract (no JSON Schema)
Rejected: the struct only declares what *our* code consumes. Fields we ignore would drift without notice. The schema says what jambonz promises, independent of our consumption.

### Option C — Contract-recording / snapshot testing (capture first response, pin thereafter)
Rejected as primary: codifies whatever jambonz happens to return today — which might itself be wrong. Useful as a *temporary* tool when authoring a new schema, not as the canonical contract.

### Option D — File PRs to `jambonz-fern-config` first, then consume only the upstream YAML
Right long-term, wrong short-term. Would block `smoke-tester` on upstream review cycles. Adopt as a parallel workstream: hand-roll locally today, PR upstream in parallel, delete the local copy when the PR merges.

### Option E — `gojsonschema` (xeipuuv) instead of santhosh-tekuri
Rejected: draft 7 only. OpenAPI 3.1 needs 2020-12.

## References

- [ADR-0002](0002-scope-external-test-harness.md) — black-box principle; contract is only what jambonz exposes.
- [ADR-0012](0012-go-test-as-test-runner.md) — `TestMain` loads the schemas once per process.
- [coverage-matrix.md](../coverage-matrix.md) — what is and isn't schema-covered, tier-by-tier.
- [jambonz-fern-config](https://github.com/jambonz/jambonz-fern-config) — the authoritative spec source.
- [santhosh-tekuri/jsonschema](https://github.com/santhosh-tekuri/jsonschema).
