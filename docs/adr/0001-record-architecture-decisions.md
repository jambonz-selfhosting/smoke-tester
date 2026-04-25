# 1. Record architecture decisions

- **Status:** Accepted
- **Date:** 2026-04-18
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** process, meta

## Context

`smoke-tester` is a new repository. Even on a small project, architectural decisions accumulate quickly — language, test runner, SIP library, cleanup convention, CI scope — and six months from now the reasoning will be forgotten unless it is written down. The maintainer also wants to start practising "architecture as code" / ADR discipline on a small project before applying it to larger ones.

Without ADRs, the alternatives are (a) documenting decisions in PR descriptions (lost in git UIs, hard to index), (b) long-form docs that go stale, or (c) oral tradition (does not survive turnover).

## Decision

Significant architectural and design decisions for `smoke-tester` are recorded as Architecture Decision Records under `docs/adr/`, following the lightweight [Michael Nygard format](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions).

- One markdown file per decision, numbered sequentially with a four-digit zero-padded prefix: `NNNN-kebab-case-title.md`.
- Each record uses the sections: **Status**, **Context**, **Decision**, **Consequences**, **Alternatives considered**, **References**.
- `docs/adr/README.md` holds the index, status lifecycle, and authoring guidelines.
- Accepted ADRs are **append-only**: if a decision changes, a new ADR supersedes the old one. The old record stays in place with its status flipped to `Superseded by ADR-NNNN`.

## Consequences

- Positive: decisions are discoverable, reviewable, and preserved across contributor turnover. PR reviews get an explicit "does this change contradict an ADR?" checkpoint.
- Positive: the act of writing an ADR forces naming rejected alternatives, which improves decision quality.
- Negative: small overhead per decision (~15 minutes to draft). Risk of ADR fatigue if applied to trivial choices — mitigated by the "architecturally significant" bar in the README.
- Neutral: ADRs live under `docs/adr/` rather than in `README.md` to keep the project root lean.

## Alternatives considered

### Option A — Capture decisions only in PR descriptions
Rejected: PR descriptions are not indexed and are hard to navigate after the fact. They also do not lend themselves to the "status lifecycle" (superseded, deprecated).

### Option B — Single long-form `DESIGN.md`
Rejected: monolithic design docs rot quickly, do not preserve decision history, and make it hard to see *when* and *why* something changed.

### Option C — No formal decision log
Rejected: defeats one of the stated goals of this repo, which is to practise architecture-as-code hygiene on a small project.

## References

- Michael Nygard, *Documenting Architecture Decisions* (2011)
- [adr.github.io](https://adr.github.io/)
