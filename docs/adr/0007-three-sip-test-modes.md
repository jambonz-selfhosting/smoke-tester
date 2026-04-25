# 7. Three SIP test modes: client, carrier, inbound

- **Status:** Accepted
- **Date:** 2026-04-18
- **Updated:** 2026-04-18 (NAT gating refined after spike confirmed symmetric-RTP makes UAC tests laptop-friendly — see [ADR-0014](0014-symmetric-rtp-media-latch.md); marker mechanism restated in Go test vocabulary per [ADR-0012](0012-go-test-as-test-runner.md))
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** sip, testing

## Context

jambonz integrates with SIP peers in several distinct roles, each with a different auth/routing model: registered SIP users (softphones/devices), IP-authenticated carriers (upstream trunks), and outbound calls that jambonz places into the PSTN or another SIP endpoint. A release gate that validates only one of these gives a false sense of safety. But not every test environment can host every mode: a laptop behind NAT cannot accept inbound SIP from the cluster.

## Decision

The SIP test surface is split into **three modes**. Each test declares which mode(s) it needs by calling a helper at the top of the test (e.g. `testenv.RequireCarrier(t)`); the helper calls `t.Skip(...)` with a readable reason when the current environment cannot support the mode.

| Mode | Helper | Test-box role | Jambonz-side provisioning | NAT requirement |
|---|---|---|---|---|
| Client | `testenv.RequireClientMode(t)` | UA (sipgo+diago) registers as a jambonz SIP user, places calls through jambonz | `User` with SIP creds | **Works behind NAT** via symmetric RTP ([ADR-0014](0014-symmetric-rtp-media-latch.md)) |
| Carrier | `testenv.RequireCarrierMode(t)` | UA sends INVITEs from `PUBLIC_IP` as an IP-authenticated trunk | `Carrier` + `SipGateway` whitelisting `PUBLIC_IP` | Requires `PUBLIC_IP` set (outbound only) |
| Inbound | `testenv.RequireInboundMode(t)` | UA listens on `PUBLIC_IP:5060`; jambonz dials it via a `dial` verb | Outbound `Carrier` → `SipGateway` pointing at test box | Requires public, reachable UDP/TCP port — skipped if `BEHIND_NAT=true` |

- Environment gating: `BEHIND_NAT=true` → `Inbound` tests are skipped.
- Environment gating: `PUBLIC_IP` unset → `Carrier` and `Inbound` tests are skipped.
- **UAC / Client mode no longer requires `PUBLIC_IP`.** jambonz's symmetric-RTP behaviour delivers return media to the UA's NAT-mapped source regardless of advertised SDP. The UA wrapper must begin sending outbound RTP (silence is fine) immediately on dialog answer — this is enforced by default in the wrapper and documented in [ADR-0014](0014-symmetric-rtp-media-latch.md).
- Every outbound INVITE carries an `X-Test-Id` header for correlation with the webhook app ([ADR-0006](0006-fastapi-webhook-via-ngrok.md)).

## Consequences

- Positive: one suite validates all SIP-peer roles jambonz supports.
- Positive: the same codebase runs on a laptop and on a public Debian/EC2 box; differing capability is handled by skips, not by branching the suite.
- Positive: `Client` mode runs on any developer's laptop without port-forwards, STUN, or TURN — bar to running the suite locally is minimal.
- Positive: explicit `testenv.Require*` helpers make it obvious (at the top of each test file) which environment a test needs — easier than pytest-style markers to grep for.
- Negative: the Debian/EC2 box is still required for **full** coverage — a maintainer on a laptop alone has a gap (carrier + inbound).
- Negative: provisioning logic per mode is non-trivial; each mode needs its own helper(s). Mitigation: the provisioning SDK's `managed(...)`-style resource helpers ([ADR-0008](0008-run-id-tagging-and-cleanup.md)) reduce this to a few lines of setup per test.
- Follow-up: a pre-flight check (run once per test process in `TestMain`) verifies that the configured environment can reach the cluster and, if `PUBLIC_IP` is set, that inbound SIP on the test port is actually reachable from the cluster — fail fast rather than through cryptic test timeouts.

## Alternatives considered

### Option A — Only test `sip_client` mode (register as a user)
Rejected: ignores carrier integration, which is how most production jambonz traffic enters/leaves. Biggest category of real-world bugs would be uncovered.

### Option B — Only test outbound directions (skip `sip_inbound` entirely)
Rejected: jambonz's `dial` verb targeting a SIP endpoint is a core feature; shipping without exercising it is risky.

### Option C — Mock jambonz-side SIP with a simulated peer
Rejected: violates the black-box principle (ADR-0002) and does not catch real SIP interop issues.

## References

- [ADR-0002](0002-scope-external-test-harness.md) — black-box principle.
- [ADR-0013](0013-sipgo-diago-for-sip-rtp.md) — the UA (sipgo+diago) that implements all three modes.
- [ADR-0006](0006-fastapi-webhook-via-ngrok.md) — correlation via `X-Test-Id`.
- [ADR-0014](0014-symmetric-rtp-media-latch.md) — why `Client` mode works behind NAT.
- [ADR-0012](0012-go-test-as-test-runner.md) — how `testenv.Require*` helpers integrate with the runner.
