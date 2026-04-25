# 13. sipgo + diago for SIP and RTP

- **Status:** Accepted
- **Date:** 2026-04-18
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** sip, rtp, go, dependencies
- **Supersedes:** [ADR-0005](0005-pjsua2-for-sip-rtp.md)

## Context

The harness must send and receive real SIP signaling (REGISTER, INVITE, re-INVITE, BYE, CANCEL, DTMF via RFC 2833 + SIP INFO) and real RTP media (play WAV out, capture WAV in, detect tones, send DTMF) across three operating modes: client / carrier / inbound ([ADR-0007](0007-three-sip-test-modes.md)). Required transports are UDP, TCP, and TLS. WebSocket is **not** required for v1.

Originally pjsua2 was chosen ([ADR-0005](0005-pjsua2-for-sip-rtp.md)). A 1-day spike ([spikes/001-sipgo-diago/](../../spikes/001-sipgo-diago/)) established that [sipgo](https://github.com/emiago/sipgo) (SIP stack) + [diago](https://github.com/emiago/diago) (media + dialog abstractions on top of sipgo) meet every requirement with materially better developer ergonomics.

## Decision

Use **[sipgo v1.3.0](https://github.com/emiago/sipgo)** for SIP signaling and **[diago v0.28.0](https://github.com/emiago/diago)** for dialog management and RTP media. Both are authored and maintained by the same developer; diago is explicitly positioned as *"fast and easy testable VoIP apps."*

- A thin wrapper package `internal/sip/` hides sipgo/diago boilerplate (UA construction, transport registration, NAT-aware SDP, audio pipeline wiring, logging) behind a test-friendly API.
- Three mode builders (`internal/sip/mode/{client,carrier,inbound}`) configure the UA for the three SIP test modes ([ADR-0007](0007-three-sip-test-modes.md)).
- Transports registered at UA construction: **UDP and TCP for v1**; TLS added when needed. `InviteOptions.Transport` selects per-call. WebSocket/WSS is deferred — jambonz webhooks support WS delivery, but the UA-side WS transport is not a v1 requirement.
- Default log level is INFO; a `LOG_LEVEL=debug` env var bumps sipgo+diago to debug for failure diagnostics.
- Library versions are pinned in `go.mod` and locked in `go.sum`. Upgrading is a conscious decision recorded in a future ADR.

### Spike evidence (2026-04-18)

Running the spike against `sip.jambonz.me` from a laptop behind a home-router NAT, dialing `sip:caller-uas@sip.jambonz.me` over TCP:

- Install: `go get github.com/emiago/sipgo github.com/emiago/diago` — seconds, no native compile.
- Build: `go build ./spike` — under 15 seconds cold, cached thereafter.
- Signaling: INVITE → digest auth → 200 OK in ~2.5 seconds. Custom `X-Test-Id` header was a one-line `sip.NewHeader(...)` in `InviteOptions.Headers`.
- Media: PCMU (G.711 µ-law) negotiated automatically. 160 kB of decoded PCM16 mono captured over 10 seconds — a complete, playable WAV. RMS energy confirmed real (non-silence) audio.
- NAT: the laptop's home router drops unsolicited inbound UDP, yet media still flowed back end-to-end. jambonz's symmetric-RTP / media-latch behaviour (documented in [ADR-0014](0014-symmetric-rtp-media-latch.md)) relies on the UA sending outbound RTP first; diago's `AudioWriter()` made that a one-goroutine `time.Ticker` loop.

## Consequences

- Positive: plain `go get` replaces pjsua2's `./configure && make && swig` dance. `rm -rf bin/` is a complete reset.
- Positive: parsed SIP messages are handed to handlers as typed structs. Assertions on received SIP are trivial (`req.GetHeader("X-Test-Id")`), unlike pjsua2's opaque callback prompts.
- Positive: goroutines + channels replace pjsua2's thread-registration + callback-on-pjsip-thread model. No `libRegisterThread` calls, no deadlock class removed entirely.
- Positive: NAT-aware SDP is a single `MediaExternalIP` field on the transport. Even without it, symmetric RTP works ([ADR-0014](0014-symmetric-rtp-media-latch.md)).
- Positive: static binary cross-compiles for Linux from macOS — same codebase runs on the laptop and the public-IP Debian box.
- Negative: diago is young (v0.28 at decision time) and has one primary maintainer. Mitigated by (a) MIT/BSD licenses allowing vendor/fork if needed, (b) a spike that exercised the critical paths we'll depend on, (c) thin wrapper limits blast radius of a future fork.
- Negative: no built-in Goertzel/tone-detect — ~30 lines of Go against the captured PCM when we need it. Not a dependency problem.
- Negative: Opus coverage is unverified in diago. Called out as "not required for v1" in the spike; revisit with a follow-up spike if the scope changes.
- Follow-up: the `internal/sip/` wrapper should be the only package that imports sipgo/diago directly, so a future library swap is contained.

## Alternatives considered

### Option A — pjsua2 (the previous decision, [ADR-0005](0005-pjsua2-for-sip-rtp.md))
Rejected after the spike. Capable but carries a permanent ergonomic tax the Go stack does not: SWIG-generated bindings, native build step tied to a specific Python ABI, thread-registration discipline. The spike took one day end-to-end; an equivalent pjsua2 spike would have spent most of that day on build/threading plumbing.

### Option B — drachtio-srf (Node.js, used by jambonz itself)
Rejected as primary: strong signaling stack but media is delegated to rtpengine/FreeSWITCH sidecars. Bolting on rtpengine to play a WAV and detect a tone is more operational surface than diago's in-process media. Keep as future option if the harness ever needs SIP-proxy conformance testing.

### Option C — go-sip-ua
Rejected: maintenance stalled (last release 2022-01). RTP handling is relay-only; no codec or WAV playback API.

### Option D — rsipstack (Rust)
Rejected: signaling only; media would need pion or similar bolted on. Steeper language learning curve for solo maintainer.

### Option E — SIPp as a subprocess
Rejected (same rationale as [ADR-0005](0005-pjsua2-for-sip-rtp.md)): XML scenarios are awkward for programmatic assertions, no WebSocket, correlation-via-header is inconvenient, and splitting signaling from media across tools adds integration cost.

## References

- [ADR-0005](0005-pjsua2-for-sip-rtp.md) — the superseded predecessor.
- [ADR-0007](0007-three-sip-test-modes.md) — the SIP modes this stack implements.
- [ADR-0011](0011-go-modules-for-dependencies.md) — Go toolchain this depends on.
- [ADR-0014](0014-symmetric-rtp-media-latch.md) — why UAC tests work behind NAT.
- [spikes/001-sipgo-diago/](../../spikes/001-sipgo-diago/) — the spike evidence.
- sipgo: <https://github.com/emiago/sipgo>
- diago: <https://github.com/emiago/diago>
