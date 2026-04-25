# Architecture Decision Records

This directory captures significant architectural and design decisions for `smoke-tester` using the lightweight ADR (Architecture Decision Record) format popularised by Michael Nygard.

## Why ADRs

- **Preserve the "why."** Code shows *what*; commit messages show *what changed*. ADRs show *why a choice was made over the alternatives considered.*
- **Onboarding.** New contributors read the ADR log and understand the shape of the system without archaeology.
- **Honest tradeoffs.** Writing down the rejected options forces clear thinking and prevents re-litigating settled decisions.
- **Cheap to maintain.** One short markdown file per decision. No tooling required.

## When to write an ADR

Write an ADR whenever a decision is **architecturally significant** — meaning it would be expensive or painful to reverse later. Examples:

- Choosing a language, framework, or major library (e.g. pjsua2 vs. alternatives)
- Choosing a pattern that shapes many files (e.g. resource tagging convention)
- A policy that touches every contributor (e.g. cleanup contract, env-var convention)
- Deliberately *not* doing something (negative decisions are valid ADRs)

Do **not** write an ADR for trivial, local, or easily-reversed choices. Prefer a comment in code or a README note.

## How to write one

1. Copy `template.md` to `NNNN-kebab-case-title.md` (next sequential number, zero-padded to 4).
2. Fill in the sections — keep it tight, a single screen is plenty.
3. Set **Status** to `Proposed` while under discussion; flip to `Accepted` on merge.
4. Add a one-line entry to the index below.
5. Commit with the ADR in its own commit so the log is easy to scan.

## Status lifecycle

```
Proposed  →  Accepted  →  Deprecated  →  Superseded by ADR-NNNN
```

- **Proposed** — under discussion, not binding.
- **Accepted** — the decision of record. Code should conform.
- **Deprecated** — no longer applies, but no replacement written yet.
- **Superseded** — replaced by a specific later ADR. Link to it. Never delete the old one.

Once an ADR is Accepted, **do not edit its decision body.** If the decision changes, write a new ADR that supersedes it. This preserves history.

## Index

| #    | Title                                                   | Status   |
| ---- | ------------------------------------------------------- | -------- |
| 0001 | [Record architecture decisions](0001-record-architecture-decisions.md)         | Accepted |
| 0002 | [Scope: external integration-test harness only](0002-scope-external-test-harness.md) | Accepted |
| 0003 | [Python with plain venv for dependency management](0003-python-venv-for-dependencies.md) | Superseded by 0011 |
| 0004 | [pytest as the test runner](0004-pytest-as-test-runner.md) | Superseded by 0012 |
| 0005 | [pjsua2 for SIP and RTP](0005-pjsua2-for-sip-rtp.md) | Superseded by 0013 |
| 0006 | [Go `net/http` webhook app exposed via ngrok](0006-fastapi-webhook-via-ngrok.md) | Accepted |
| 0007 | [Three SIP test modes: client, carrier, inbound](0007-three-sip-test-modes.md) | Accepted |
| 0008 | [Run-ID tagging and mandatory resource cleanup](0008-run-id-tagging-and-cleanup.md) | Accepted |
| 0009 | [Configuration via `.env` and a typed Go settings struct](0009-config-via-env-and-pydantic-settings.md) | Accepted |
| 0010 | [Release-gate scope, not CI-per-PR](0010-release-gate-scope.md) | Accepted |
| 0011 | [Go with modules for dependency management](0011-go-modules-for-dependencies.md) | Accepted |
| 0012 | [`go test` as the test runner](0012-go-test-as-test-runner.md) | Accepted |
| 0013 | [sipgo + diago for SIP and RTP](0013-sipgo-diago-for-sip-rtp.md) | Accepted |
| 0014 | [Symmetric RTP / media latch — no `PUBLIC_IP` required for UAC](0014-symmetric-rtp-media-latch.md) | Accepted |
| 0015 | [Contract-test every jambonz response against the canonical spec](0015-contract-testing.md) | Accepted |

## References

- Michael Nygard, [Documenting Architecture Decisions](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions) (2011)
- [adr.github.io](https://adr.github.io/) — community index of ADR formats and tools
