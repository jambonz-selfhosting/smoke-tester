# jambonz TTS latency — 10 vendors, measured 2026-09-04

Reproduction of the measurement behind
[Text-to-speech latency: the jambonz leaderboard](https://jambonz.org/blog/text-to-speech-latency-the-jambonz-leaderboard),
run against the live `jambonz.me` cluster with the credentials provisioned on it.

Harness: `tests/verbs/tts_leaderboard_test.go` (build tag `leaderboard`; not part
of the release-gate suite). Raw per-utterance data: [`data/tts-latency-samples-2026-09-04.json`](data/tts-latency-samples-2026-09-04.json)
(all 190 utterances, with `call_sid` on each, so any figure here can be traced
back to its line in the feature-server log).

---

## The metric

**Time to first byte (TTFB)** — from the moment jambonz issues the synthesis
request to the vendor, to the first byte of audio coming back. This is the
vendor's own latency and nothing else.

It is read from the media server's own instrumentation, not inferred: the
feature-server log records `variable_tts_time_to_first_byte_ms` on the playback
event for every utterance, keyed by `call_sid`. Each row below is that number,
scraped over ssh for the exact calls this run placed.

Deliberately **not** reported: any end-to-end figure. Time-to-first-audio at the
caller would add mediajam's vendor dial, RTP packetisation and a network hop
back to the measuring host — that is a property of the test rig's location, not
of the vendor.

All ten vendors returned true *first*-byte numbers on this build. Where a vendor
synthesizes node-side instead, only time-to-*last*-byte exists; the harness
labels those rows separately so the two are never mixed. No such row occurred in
this run.

---

## Cold vs warm

Each call speaks **five prompts back to back**. The split matters because the
vendor connection is established once and then reused.

- **cold** — the first prompt of the call. Includes establishing the vendor
  connection: DNS, TCP, TLS, and for streaming vendors the WebSocket handshake.
  This is what a caller hears on the **first thing you say to them**, and it is
  the closest match to the original blog's methodology, which dialled fresh each
  time.
- **warm** — the **median of the remaining four**. The connection already exists
  and is reused (visible in the feature-server log as
  `TtsStreamingBuffer:start already started, skipping`). This is what the caller
  hears on **every subsequent turn** — the number that matters for a
  conversational or `agent` flow, where the greeting is one turn out of twenty.

Read them together. `cold` is one sample and therefore noisy; `warm` is a median
of four and more stable. All four warm samples are printed so you can see the
spread rather than trust a single statistic — elevenlabs in particular has a
long tail (one 3433 ms outlier on short, one 2282 ms on long) that the median
hides.

Every utterance ran with `disableTtsCache: true`, so nothing was served from the
cluster's TTS cache. Prompts follow the blog's two classes: five customer-service
phrases of 14-20 words (short) and five IVR-style prompts of 40-60 words (long).

---

## Short prompts (14-20 words)

| # | vendor | voice | cold ms | warm median ms | warm samples ms | blog ms |
|--:|---|---|--:|--:|---|--:|
| 1 | inworld | `Olivia` | 70 | **52** | 47, 49, 56, 63 | — |
| 2 | deepgram | `aura-2-asteria-en` | 270 | **98** | 46, 66, 131, 136 | 341 |
| 3 | cartesia | `f014dce5-df0e-4cfa-98e1-bd4bb73bb0b1` | 151 | **138** | 116, 120, 156, 159 | — |
| 4 | deepgramflux | `flux-alexis-en` | 101 | **219** | 173, 215, 223, 245 | — |
| 5 | microsoft | `en-US-AvaMultilingualNeural` | 264 | **225** | 205, 207, 243, 310 | 302 |
| 6 | xai | `eve` | 303 | **273** | 249, 251, 295, 323 | — |
| 7 | google | `en-US-Wavenet-C` | 269 | **379** | 335, 363, 396, 690 | 201 |
| 8 | elevenlabs | `hpp4J3VqNfWAUOO0d1Us` | 691 | **497** | 450, 455, 539, 3433 | 532 |
| 9 | murf | `en-US-alina` | 549 | **647** | 598, 633, 662, 697 | — |
| 10 | rimelabs | `adeline` | 1235 | **1414** | 1180, 1317, 1511, 1589 | 242 |

## Long prompts (40-60 words)

| # | vendor | voice | cold ms | warm median ms | warm samples ms | blog ms |
|--:|---|---|--:|--:|---|--:|
| 1 | inworld | `Olivia` | 49 | **51** | 48, 50, 53, 54 | — |
| 2 | cartesia | `f014dce5-df0e-4cfa-98e1-bd4bb73bb0b1` | 159 | **151** | 121, 126, 176, 180 | — |
| 3 | deepgramflux | `flux-alexis-en` | 107 | **180** | 58, 172, 188, 287 | — |
| 4 | deepgram | `aura-2-asteria-en` | 137 | **215** | 60, 174, 256, 298 | 417 |
| 5 | xai | `eve` | 291 | **267** | 258, 258, 276, 281 | — |
| 6 | microsoft | `en-US-AvaMultilingualNeural` | 409 | **275** | 226, 243, 307, 310 | 353 |
| 7 | elevenlabs | `hpp4J3VqNfWAUOO0d1Us` | 526 | **534** | 498, 499, 569, 2282 | 906 |
| 8 | google | `en-US-Wavenet-C` | 1431 | **741** | 354, 739, 744, 897 | 408 |
| 9 | murf | `en-US-alina` | 2269 | **1989** | 1699, 1900, 2079, 2374 | — |
| 10 | rimelabs | `adeline` | 4701 | **4265** | 4133, 4256, 4275, 4811 | 386 |

---

## Reading the results

**Versus the blog.** Compare against `cold`, not `warm` — the blog dialled fresh
for every sample, so its numbers include connection setup the way `cold` does.
On that basis: deepgram improved (341 → 270 ms short, 417 → 137 ms long) and
elevenlabs improved on long (906 → 526 ms) while getting worse on short
(532 → 691 ms). microsoft is flat (302 → 264, 353 → 409 ms). google went
backwards on both (201 → 269, 408 → 1431 ms). **rimelabs regressed severely** —
242 → 1235 ms short and 386 → 4701 ms long — but see the caveat below before
drawing a conclusion from it.

**inworld is the fastest vendor on the cluster** by a wide margin, at ~50 ms warm
on both classes, and is almost perfectly flat between them.

**Prompt length separates the vendors.** cartesia, inworld, xai and microsoft are
effectively length-independent — they start returning audio before the full text
is processed. google, murf and rimelabs scale with input length, which suggests
they do more work up front. Compare inworld (51 vs 52 ms) against rimelabs
(1414 vs 4265 ms).

**Cold cost varies more than warm.** google pays 1431 ms on the first long
utterance versus a 741 ms warm median; deepgram pays 270 ms cold versus 98 ms
warm on short. Budget for the cold number in your greeting, the warm one
everywhere else.

---

## Caveats

**`say` with `stream: true` is absent, and cannot be added under this metric.**
There is no server-side TTFB for streaming anywhere in the current build:
feature-server hands text to the TTS stream and mediajam plays back whatever
arrives, with no playback-start event carrying a timing figure. mediajam's own
log records only `tts streaming started` → `tts connected` → `tts session ended`,
nothing per utterance. Measuring streaming would require either new
instrumentation upstream, or accepting an end-to-end number — which this report
excludes by design.

**elevenlabs streaming TTS is broken on this cluster.** The WebSocket handshake
returns HTTP 403 (`expected handshake response status code 101 but got 403`,
mediajam.log). The same credential synthesizes correctly over HTTP, so the key
is valid but has no streaming-WS permission on that elevenlabs account. Any
`say` with `stream: true` on elevenlabs plays silence for the full call
duration. Unrelated to the numbers above, which are all non-streaming.

**rimelabs' voice is not the blog's.** The blog used Mist / "Abby", which is no
longer in the vendor's catalogue; the harness auto-selected `adeline`. Part of
the regression may be voice choice rather than vendor latency. Pin a comparable
voice and rerun before publishing that figure.

**aws was excluded** — its speech credential on this cluster has
`tts_tested_ok = 0`, so feature-server would refuse it. Fix the credential to
include Polly. **`custom:telin`** is skipped unless named explicitly, since a
custom vendor needs a per-deployment URL and dialect.

**One run, five samples per cell.** Enough to rank vendors that differ by an
order of magnitude; not enough to separate ones within ~50 ms of each other.
Vendors ran concurrently (`-parallel 5`) with each vendor's own cells
sequential, so no vendor account ever saw two calls from this run at once.

---

## Reproducing

```
JAMBONZ_IT_SIP_BIND_HOST=<tunnel-ip-if-on-vpn> \
go test -count=1 -tags leaderboard -run TestTTSLeaderboard \
  -parallel 5 -timeout 90m -v ./tests/verbs
```

Knobs: `TTS_LB_VENDORS`, `TTS_LB_MODES`, `TTS_LB_CLASSES`, `TTS_LB_PROMPTS`,
`TTS_LB_SSH` (host holding the feature-server log), `TTS_LB_OUT`.
Full sweep: 38 cells, 190 utterances, ~15 minutes.
