# 2. Scope: external integration-test harness only

- **Status:** Accepted
- **Date:** 2026-04-18
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** scope, architecture

## Context

jambonz is released frequently and every release must be validated against three surfaces — REST API, verb/webhook execution, and SIP+RTP. A jambonz cluster is deployed and managed by a *separate* tool (not this repo). The goal is to have a suite that points traffic at an already-running cluster and asserts correctness end-to-end, before a release is tagged.

## Decision

`smoke-tester` is **strictly an external, black-box test harness**. It:

- Targets an existing, externally-managed jambonz deployment addressed purely by configuration (`JAMBONZ_API_URL`, `JAMBONZ_SIP_DOMAIN`, API key).
- Performs **no** deployment, configuration management, image build, database migration, or cluster lifecycle action.
- Exercises jambonz **only over public interfaces**: the REST API, SIP signaling, RTP media, and webhook callbacks.
- Is a **release gate**: it runs before tagging, not on every PR to jambonz.

Anything that requires inspecting or manipulating the cluster from the inside (logs, DB rows, internal services) is out of scope.

## Consequences

- Positive: the harness is portable — same suite validates staging, a local dev cluster, or a customer-facing canary.
- Positive: forces tests to reflect real customer-observable behaviour, not implementation details.
- Negative: some failure modes (internal race conditions, DB inconsistencies) are only visible indirectly through their external symptoms.
- Negative: cluster must be externally provisioned before tests can run — no one-command "boot + test" loop.
- Follow-up: every provisioned resource must be tagged and cleaned up, since we're writing to shared infrastructure (see ADR-0008).

## Alternatives considered

### Option A — Bundle a docker-compose jambonz stack for self-contained testing
Rejected: duplicates work done by the separate deployment tool, couples the test suite to a specific deployment shape, and does not validate the real target (which may be a VM, k8s, etc.).

### Option B — In-cluster test agent with access to logs and DB
Rejected: violates the black-box principle, makes the harness non-portable across deployment topologies, and tests implementation instead of behaviour.

## References

- ADR-0008 (run-id tagging and cleanup) — direct consequence of the "shared cluster" constraint.
- ADR-0010 (release-gate scope) — frames *when* the suite runs.
