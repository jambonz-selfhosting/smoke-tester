# 14. Rely on symmetric RTP / media latch; do not require `PUBLIC_IP` for UAC tests

- **Status:** Accepted
- **Date:** 2026-04-18
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** sip, rtp, nat, networking

## Context

jambonz's SBC implements **symmetric RTP** (a.k.a. "media latch" / "comedia"): after answering an INVITE, it ignores the RTP IP/port advertised in the caller's SDP and instead sends return media to the source IP/port of the first inbound RTP packet it receives. This is the standard SBC behaviour for NAT traversal and is how most softphones work.

This was verified empirically in the spike ([spikes/001-sipgo-diago/](../../spikes/001-sipgo-diago/)) on 2026-04-18 from a laptop behind a home-router NAT:

- With `PUBLIC_IP` set (14.186.26.178 advertised in SDP): 163,520 PCM bytes received over 10.22s, RMS 438.8. ✅
- With `PUBLIC_IP` deliberately unset (LAN IP 192.168.1.x advertised in SDP): 159,680 PCM bytes received over 9.98s, RMS 434.1. ✅ — **virtually identical.**

The laptop still had to send outbound RTP first to open the NAT pinhole; the UA does this as soon as the call is answered (20 ms PCMU silence frames on a ticker from `AudioWriter()`).

## Decision

The harness relies on jambonz's symmetric RTP for UAC-mode NAT traversal. Specifically:

- **UAC tests (`sip_client` mode where the harness dials jambonz)** do **not** require `PUBLIC_IP` to be configured. The UA may advertise its LAN IP in SDP; symmetric RTP will still return media correctly.
- The UA wrapper **must** begin sending outbound RTP immediately on dialog answer, even if the test has no audio to play. A 20 ms PCMU silence frame at 50 Hz is the canonical pattern; it opens the NAT pinhole and keeps it open for the life of the call.
- `PUBLIC_IP` remains **required** for:
  - **`sip_carrier`** mode — jambonz IP-authenticates the peer by source address; the harness must present from a stable, whitelisted IP.
  - **`sip_inbound`** mode — jambonz originates the INVITE toward the harness; there is no prior outbound flow to latch onto, so a reachable public address plus port-forward (or equivalent) is needed.
- `ARCHITECTURE.md` §6 and [ADR-0007](0007-three-sip-test-modes.md) reflect this: UAC is laptop-friendly; carrier and inbound modes still require the Debian/EC2 public-IP host.

## Consequences

- Positive: a developer can run the **full UAC test suite from any laptop behind any NAT**, without knowing their public IP, configuring a router port-forward, or running a TURN server. This materially lowers the barrier to running the release gate locally.
- Positive: the harness's `PUBLIC_IP` env var ([ADR-0009](0009-config-via-env-and-pydantic-settings.md)) downgrades from "conditionally required" to "only required for carrier/inbound modes." Config failures for UAC tests become impossible.
- Positive: no TURN server, no STUN discovery, no ICE — the UA stack stays small.
- Negative: the "send silence on answer" pattern must be correctly implemented in the UA wrapper and never forgotten. If a test forgets to start outbound RTP, inbound RTP will silently never arrive. Mitigated by making the silence-sender a default behaviour of the wrapper, opt-out rather than opt-in, with a log line on start so regressions are visible.
- Negative: this decision couples correctness to a specific jambonz-side behaviour (symmetric RTP). If a future jambonz release disables or changes it, UAC tests behind NAT break. Mitigation: we would detect this immediately in the release gate (the whole point of this repo), and the fallback is to require `PUBLIC_IP` + port-forward as before — an operational workaround, not a code change.
- Follow-up: document the "silence-on-answer" contract prominently in the UA wrapper's package doc, including a pointer to this ADR.

## Alternatives considered

### Option A — Always require `PUBLIC_IP` for any RTP-bearing test
Rejected: the spike proved it is not necessary for UAC tests, and requiring it penalises every developer who wants to run the suite locally. Adds config friction for no correctness benefit.

### Option B — Require a TURN server (coturn) for NAT traversal
Rejected: adds a service to operate, configure, and secure. Does not match the real-world deployment topology of jambonz (which relies on symmetric RTP from carriers and UAs). Hides, rather than tests, the media path.

### Option C — Use STUN to discover the NAT-mapped address and advertise it in SDP
Rejected for the same reason as Option B — adds complexity without a correctness benefit, because symmetric RTP already solves the problem at the jambonz side. And it would not help if the NAT is a full-cone / symmetric-NAT variant that assigns different mappings per destination.

## References

- [ADR-0007](0007-three-sip-test-modes.md) — the three SIP test modes whose NAT requirements this ADR refines.
- [ADR-0009](0009-config-via-env-and-pydantic-settings.md) — where the `PUBLIC_IP` env var's new "conditional" scope is documented.
- [ADR-0013](0013-sipgo-diago-for-sip-rtp.md) — the SIP/RTP stack that implements the silence-on-answer pattern.
- [spikes/001-sipgo-diago/](../../spikes/001-sipgo-diago/) — empirical evidence.
- RFC 4961 (Symmetric RTP / RTCP)
