# 5. pjsua2 for SIP and RTP

- **Status:** Superseded by [ADR-0013](0013-sipgo-diago-for-sip-rtp.md)
- **Date:** 2026-04-18
- **Superseded:** 2026-04-18
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** sip, rtp, python, dependencies, superseded

> **Superseded on 2026-04-18** following a 1-day spike with sipgo+diago
> (Go). The spike proved full UAC capability end-to-end (TCP signaling,
> digest auth, custom headers, PCMU media, symmetric-RTP NAT traversal,
> clean BYE) in a single-file program, with no native build, no SWIG, and
> no venv/Python-ABI coupling. The pjsua2 pain points listed below were
> judged too high to carry for the life of the project. See
> [ADR-0013](0013-sipgo-diago-for-sip-rtp.md) for the replacement and
> [spikes/001-sipgo-diago/](../../spikes/001-sipgo-diago/) for the evidence.
> Record below is kept for history.

## Context

The harness must send and receive real SIP signaling (REGISTER, INVITE, re-INVITE, BYE, DTMF via SIP INFO / RFC 2833) and real RTP media (play WAV out, capture WAV in, detect tones, send DTMF) across three operating modes (client / carrier / inbound). Python does not have a pure-Python SIP+RTP stack of production quality. The viable options are thin wrappers over the PJSIP C library.

## Decision

Use `pjsua2` — the C++/Python object-oriented API on top of PJSIP — as the SIP+RTP engine.

- PJSIP is built from source via `scripts/build_pjsua2.sh` targeting the repo's `.venv`. This keeps the binding aligned with the venv's Python ABI and means `rm -rf .venv` is still a clean reset (the build script is re-run on `make install`).
- A thin wrapper module `src/smoke-tester/sip/ua.py` hides pjsua2 boilerplate (account config, media format negotiation, WAV play/record, DTMF, logging) behind a test-friendly API.
- Three builder functions in `src/smoke-tester/sip/modes.py` configure the UA for `client`, `carrier`, or `inbound` mode (ADR-0007).
- pjsip log level defaults to 2 (warnings); `LOG_LEVEL=DEBUG` bumps it to 4 for on-failure diagnostics.

## Consequences

- Positive: PJSIP is the most mature open-source SIP stack and covers every codec, transport, and SIP edge case we'll realistically encounter.
- Positive: object-oriented `pjsua2` API is more readable than the C `pjsua` API.
- Negative: native build step complicates install — `build-essential`, `swig`, and PJSIP's own `./configure` prerequisites must be present on Debian/macOS.
- Negative: pjsua2's Python binding is SWIG-generated; tracebacks through it can be opaque. Mitigated by the thin wrapper.
- Follow-up: `scripts/build_pjsua2.sh` must pin a specific PJSIP release tag for reproducibility; upgrading PJSIP is a conscious decision recorded in a future ADR.

## Alternatives considered

### Option A — `sipvicious` / `sippy` / pure-Python SIP libs
Rejected: hobby-grade, incomplete, no RTP media handling worth using. Cannot validate audio/DTMF.

### Option B — `baresip` as a subprocess
Rejected: inter-process control surface is clumsy for a test harness; driving it from Python means scraping its text UI or writing a custom bridge.

### Option C — `sipp` as a subprocess for SIP + separate tool for RTP
Rejected: `sipp` is excellent for signaling but splits the harness into two moving parts. Combining them is more integration work than wrapping pjsua2.

### Option D — Go with `pion`, binding to Python via `cgo`
Rejected: adds a second language to the repo, and `pion` is WebRTC-first — SIP support is incomplete.

## References

- [PJSIP pjsua2 docs](https://docs.pjsip.org/en/latest/pjsua2/intro.html)
- ADR-0003 (venv) — native build targets the venv's Python.
- ADR-0007 (three SIP modes) — what the UA is configured to do.
