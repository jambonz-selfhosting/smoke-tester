# 0016. Per-leg call recording as playable WAV, env-gated

- **Status:** Accepted
- **Date:** 2026-07-03
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** sip, testing, tooling, media

## Context

When a verb/SIP test fails — or when a new feature is being developed — the
fastest way to understand what jambonz actually did is to *hear* the audio each
call leg received. Today the harness does record inbound RTP, but:

- Recordings are written as **raw headerless PCM** (`.pcm`) to `t.TempDir()`,
  which Go deletes at the end of the run. There is no way to play them back
  after the fact.
- The transcript-verification path (`internal/stt`) depends on that raw PCM,
  so we cannot simply change `StartRecording` to emit WAV.
- Files are tagged only with an ad-hoc short string (`config`, `dial-caller`,
  `conference-listener`); there is no per-test / per-leg organisation.
- There is no on/off switch — recording is hard-wired into every test.

The developer wants, when developing or debugging, to open a folder and hear
every leg of every call, with filenames that make it obvious which test and
which leg each file is. When *not* debugging (the release-gate default) no
recordings should be produced. Disk is constrained: only the **latest** run of
any given test needs to be kept, not a history of every run.

## Decision

Add per-leg **WAV** recording, controlled by an env flag, written to a stable,
human-navigable location, produced as a *side-artifact* of the existing PCM
capture so the transcript pipeline is untouched.

- **Format is WAV (8 kHz / mono / 16-bit LPCM), not MP3.** The RTP audio is
  already decoded to raw LPCM in `Call.pumpAudio`; WAV is that byte stream plus
  a 44-byte RIFF header — zero CPU, zero new dependencies. MP3 would require an
  encoder (cgo LAME or a pure-Go encoder), burning CPU per leg for every call
  and adding a dependency, in exchange for a size win that is irrelevant at
  telephony bitrates (~1 MB/minute). See "Alternatives considered".

- **Control via a single env var `RECORD_LEGS`.** Unset / `false` / `0` / `off`
  ⇒ no WAV artifacts (release-gate default, matching ADR-0010). `true` / `1` /
  `on` ⇒ every recorded leg is also archived as WAV. Base directory is
  `RECORD_DIR`, default `./recordings` (gitignored).

- **Layout is `<RECORD_DIR>/<test-name>/<leg>.wav`.** One subfolder per test,
  one file per leg, named by the leg's role (`dial-caller`,
  `conference-listener`, `agent-turn-1-reply`, …). Filenames are **stable
  across runs** (no run-id, no call-id): re-running a test overwrites that
  test's own files, so the folder always holds the latest run of each test and
  disk stays bounded to one run's worth per test. A test's subfolder is cleared
  the first time that test archives a leg in a run, so a re-run of a single
  test never mixes old and new legs. Legs whose names collide within a test
  disambiguate with a numeric suffix (`caller-1`, `caller-2`).

- **Identity is inherited, not wired per test.** Two facts make the archive
  fully automatic:
  1. *Test name* — every test has its **own private SIP stack** (per-test
     isolation via `claimUAS`), so `claimUAS` stamps `t.Name()` into
     `sip.Config.Owner` once, and every `*Call` born on that stack (inbound
     via dispatch, outbound via `Invite`) inherits it at construction.
  2. *Leg role* — every `StartRecording(path)` call already names its file
     meaningfully (`dial-caller.pcm`, `conference-listener.pcm`, …); the role
     is the recording file's basename. No role argument exists anywhere.

  On recording finalize (explicit `StopRecording` or auto-stop at call end)
  the `sip` package invokes a package-level archive hook (installed once in
  `TestMain` when `RECORD_LEGS` is on) with `(owner, basename, pcmPath)`; the
  hook wraps the captured PCM into the WAV at the computed path. Test bodies
  and per-site recording code need **zero** changes. `internal/sip` gains no
  import of `internal/config` or `internal/recording` — the hook keeps the
  layering one-directional.

## Consequences

- Positive: `RECORD_LEGS=true go test ./tests/verbs/...` then `open recordings/`
  and every leg of every call is a double-clickable WAV, named by test + leg.
- Positive: release-gate runs are unaffected (flag off by default; no artifacts,
  no extra work in the hot path).
- Positive: no new dependency; the WAV writer is ~30 lines reusing the existing
  RIFF layout from `internal/tts`.
- Positive: bounded disk — one run's worth per test, overwritten in place.
- Negative: the leg name is only as good as the `.pcm` filename the recording
  site chose. In practice every site already names its file meaningfully; a
  site that records to `x.pcm` gets an `x.wav` archive and the fix is to name
  the file better at that site (which also improves its diagnostics).
- Negative: calls placed on a stack without `Owner` set (e.g. `cmd/probe`,
  future ad-hoc stacks) are never archived. Intentional — archiving is a
  test-suite concern; the probe has no test identity.
- Neutral / follow-up: MP3 (or Opus) export can be layered later behind the same
  flag if archival size ever matters; the PCM→WAV seam is the natural hook.

## Alternatives considered

### Option A — MP3 output
Rejected. Requires an encoder dependency (cgo LAME or a pure-Go encoder) and
per-leg CPU during calls, for a size reduction that does not matter at 8 kHz
mono (~16 KB/s). WAV of already-decoded LPCM is free. The artifacts are
listen-and-delete debug aids, not an archive.

### Option B — Change `StartRecording` to write WAV directly
Rejected. The STT transcript path reads the raw PCM produced by
`StartRecording`; switching its output to WAV would break every transcript
assertion or force RIFF-stripping there. Producing WAV as an additive
finalize-time artifact keeps the transcript pipeline untouched.

### Option C — Per-run subfolders `<RECORD_DIR>/<run-id>/...`
Rejected on the user's explicit disk constraint. Keeping every run accumulates
audio without bound. Stable per-test filenames that overwrite in place keep only
the latest run of each test.

### Option D — Derive test-name/role by reverse-engineering `t.TempDir()`
Rejected. `t.TempDir()` returns `.../<TestName><counter>/001`; the trailing
counter and sanitisation make the test name fragile to recover. Stamping
`t.Name()` into the per-test stack's `Owner` is unambiguous and costs one
line in `claimUAS`.

### Option E — Explicit `Call.SetArchiveMeta(test, role)` at every recording site
Rejected after a first implementation pass. It worked, but required a
boilerplate line at all 13 `StartRecording` sites (and every future one),
re-stating information the harness already has: the test name is knowable
from the per-test stack, and the role is already encoded in the recording
filename. Inheritance via `Config.Owner` + basename-derived roles achieves
the same result with zero per-site code and can't be forgotten at new sites.

## References

- ADR-0010 — release-gate scope (why the default is off).
- ADR-0014 — symmetric-RTP media latch (why inbound RTP exists to record).
- `internal/sip/call.go` — `StartRecording` / `pumpAudio` (PCM capture).
- `internal/tts/deepgram.go` — `writeTelephonyWAV` (RIFF layout reused).
