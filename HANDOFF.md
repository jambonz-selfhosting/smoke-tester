# HANDOFF — smoke-tester

> **What this is:** a living log of what's done, in progress, and next. Updated at the end of every session and whenever work changes direction. Designed so any Claude session — or any human — can pick the work up cold without re-asking questions.
>
> **How to use it:**
>
> - Read this file **after** [CLAUDE.md](CLAUDE.md), [docs/adr/README.md](docs/adr/README.md), and [docs/coverage-matrix.md](docs/coverage-matrix.md). Those tell you *what's true*; this tells you *where we are*.
> - Update the **Session log**, **Now**, and **Next** sections at the end of each session. Keep entries terse — one line per item unless a nuance matters.
> - Move items between sections as they progress: `Next` → `Now` → `Session log`.
> - Don't treat this as a design doc. Architectural decisions go in ADRs; tier/coverage status goes in `docs/coverage-matrix.md`; commit-level history goes in git. This file is the *narrative* layer.

---

## State as of 2026-07-07

> **Orientation:** for the harness ↔ jambonz component diagram + traffic
> breakdown, see [docs/architecture/components.md](docs/architecture/components.md).

### Now (in progress)

- **SDP direction negotiation (Five9 a=sendonly interop) — 3 new drachtio
  tests, all RED against jambonz.me (2026-08-12).**
  `tests/drachtio/sdp_direction_test.go`: `TestDrachtio_Sendonly_InitialOffer`
  (a=sendonly initial offer → answer must be recvonly/inactive per RFC 3264
  §6.1), `_ReinviteLoop` (Five9's corrective a=sendonly re-INVITE → 200 +
  complement + dialog survives), `_HoldResume` (sendrecv call → sendonly
  hold → sendrecv resume). **Live finding:** the current jambonz.me build
  (mediajam — answer o= says "Jambonz Media Server") answers `a=sendrecv`
  to a sendonly offer on BOTH the initial INVITE and re-INVITEs. The
  re-INVITEs do get 200 (not 488) and the dialog survives, so the hard
  endpoint-update breakage isn't visible on this path — but the wrong
  direction complement is exactly the trigger that makes Five9 loop
  corrective a=sendonly re-INVITEs.
  **FIXED + DEPLOYED same day (2026-08-12):** the believed-existing
  mediajam fix did NOT exist — `buildLocalSDP` hardcoded `a=sendrecv`
  and `Endpoint.Modify` returned the answer SDP cached at creation.
  Fixed on mediajam branch `fix/sdp-answer-direction` (pushed to
  github.com/jambonz/mediajam, commit b68cc20, no PR opened yet):
  answers now carry the RFC 3264 §6.1 complement, Modify re-renders the
  cached answer on direction change (o= sess-id kept, sess-version
  bumped), offers stay sendrecv, and an answer to our OWN offer never
  rewrites our local SDP. RTP send behaviour deliberately untouched
  (signalling-only fix). Deployed to jambonz.me by cherry-picking the
  commit onto the box's build tree (`/usr/local/src/mediajam` is on
  `feat/dialogflow_ces_tool_calls`, 5 commits AHEAD of origin — do NOT
  overwrite it with main; the patch was applied in a copy at
  `~/build/mediajam-sendonly` and built with the krisp flags from the
  box's own `build.sh`). Pre-fix binary saved at
  `/usr/local/bin/mediajam.bak-sendonly-fix` on the box. All 3 sendonly
  tests now GREEN against jambonz.me
  (`make test-drachtio RUN=TestDrachtio_Sendonly`).
  Unrelated pre-existing red: `TestDrachtio_SessionTimer_UASRefresher`
  fails ("200 OK has no Session-Expires") — verified identical against
  the pre-fix binary via A/B binary swap, so it's drachtio
  session-timer config drift on the cluster, not the mediajam change.
  Investigate drachtio.conf.xml default-refresher config separately.
  New infra: `jsip.InviteOptions.SDPMode` (internal/sip/uac.go) — sets the
  initial offer's direction attribute via diago's NewDialog → set
  `MediaSession().Mode` → Invite → Ack sequence (diago's one-shot
  `Diago.Invite` offers no pre-offer hook).

- **Tier 4 AMD (answering machine detection) on the `dial` verb (2026-07-07).**
  New `tests/verbs/amd_test.go` (coverage-matrix row 4.15). AMD is not a
  standalone verb — it's the `amd` object on `dial`, runs on the dialed
  (B) leg, and is **STT-driven** (feature-server `lib/utils/amd-utils.js`
  word-count heuristics). Our callee UAS drives each outcome.
  - **Green on jambonz.me (2 tests):** `TestVerb_Dial_AMD_NoSpeechDetected`
    (silent callee → `amd_no_speech_detected`+`amd_stopped`) and
    `TestVerb_Dial_AMD_DecisionTimeout` (silent callee +
    `timers{noSpeechTimeoutMs:60000, decisionTimeoutMs:3000}` →
    `amd_decision_timeout`+`amd_stopped`). These prove AMD runs and the
    `amd.actionHook`, X-Test-Id correlation (via `SessionURL(sess,"amd")`),
    and `callbacks/amd` contract validation all work. AMD's timers are
    pure `setTimeout`, so they fire regardless of STT.
  - **Green on jambonz.me (2 tests) — after a feature-server fix (2026-07-07):**
    `TestVerb_Dial_AMD_HumanDetected` (short greeting → `amd_human_detected`
    reason "short greeting" + `amd_stopped`) and `_MachineDetected` (long
    greeting → `amd_machine_detected` reason "long greeting" +
    `amd_machine_stopped_speaking`). **This test found a real feature-server
    bug and we fixed it.** Initially neither fired: the STT-driven detections
    produced NO transcripts while the timer events did. Root-caused live via
    `ssh bastion` (mediajam log + `pm2 logs 49` + control-frame log): mediajam
    (the media server — Go, NOT FreeSWITCH) transcribed the greeting fine and
    sent `stt.transcription` (bugname `amd_bug`) to feature-server, but AMD's
    handler never ran. Cause: the FreeSWITCH→mediajam migration renamed
    transcription events to a normalized vocab (`stt.transcription`) and
    migrated gather/transcribe/listen via `sttEvents()`
    (`lib/utils/media-events.js`), but **`amd-utils.js` was never migrated** —
    it still registered the legacy `deepgram_transcribe::transcription`
    listener, which mediajam never emits (`media-events.js` even flagged it:
    `// … AmdEvents follow as their adapters land`). AMD's `setTimeout` timers
    fired regardless, which is why no-speech/decision-timeout "worked" while
    human/machine never did. **Fix:** route AMD's listener registration
    through `sttEvents()` in `feature-server/lib/utils/amd-utils.js` (also
    fixed the paired teardown to `removeCustomEventListener`, and a latent
    `${this.vendor}` → `${vendor}` bug). Deployed as a hotfix to jambonz.me
    and verified all four pass. `amd_stopped` is intentionally NOT asserted on
    the machine path (AMD keeps avmd running for a beep post-machine, so it's
    deferred past the drain window).
  - **Skip-stubs (2):** `TestVerb_Dial_AMD_ToneDetected` — the beep path is
    acoustic and independent of the STT gap. mediajam DOES have a Go
    mod_avmd port (`internal/audiofx/avmd.go`, DESA-2 beep detector), so
    it's implementable, but a probe playing a clean 1000Hz sine (8kHz mono,
    silence-tone-silence) on the callee leg did NOT fire `amd_tone_detected`
    — likely because the beep is sent as G.711 µ-law and companding noise
    raises the amplitude variance the detector keys on. Needs a µ-law-robust
    beep fixture tuned against mediajam's avmd thresholds. `TestVerb_Dial_AMD_Error`
    — **feature-server bug:** `amd-utils.js` emits the error via
    `task.emit(AmdEvents.Error, err)` (event name `"amd_error"`) but
    `dial.js` only wires `this.on('amd', …)`; every delivered event uses
    `task.emit('amd', {type:…})`, so `amd_error` never reaches the
    actionHook. The credential-missing path also just throws in the `Amd`
    constructor and is caught+logged in `dial.js:946`. Needs an upstream fix.
  - Schema `schemas/callbacks/amd.schema.json` was already vendored; the
    webhook server auto-validates every `/action/amd` payload against it.
    New `.gitignore` allowlist `tests/verbs/testdata/amd/*.wav` (TTS
    greetings, same policy as agent/listen).
  - **Operational follow-ups (feature-server / cluster):**
    (a) The AMD fix is a **hotfix on the deployed tree** (`/home/admin/apps/feature-server`,
    which `jambonz-feature-server` symlinks to) — mirror it in the
    feature-server git repo so it survives a redeploy. Local copy with the
    change: `~/private_jambonz/feature-server/lib/utils/amd-utils.js`;
    backup of the original on the box at `/tmp/amd-utils.bak.js`.
    (b) **pm2 launch caveat (caused a brief outage this session):** pm2 id 49
    must run under **nvm Node v22.22.1** (the app's `undici` needs Node 21+'s
    `webidl.util.markAsUncloneable`). `pm2 restart 49 --update-env` from a
    shell without that node on PATH relaunches it under system Node 20 →
    crash-loop. It's currently running with an explicit
    `--interpreter ~/.nvm/versions/node/v22.22.1/bin/node`; run `pm2 save` to
    persist, and avoid `--update-env` unless nvm node22 is on PATH.
    (c) `amd_tone_detected` (mediajam avmd): a µ-law-transmitted clean sine
    didn't trip it — need a codec-robust beep fixture, or confirm avmd
    thresholds. (d) `amd_error` wrong-event-channel bug (`amd-utils.js` emits
    on `amd_error`, `dial.js` listens on `amd`) still needs its own fix.

- **Tool-calling depth for `agent` + `llm` verbs (2026-07-05).** Two new
  tests, both PASS live against `jambonz.me`. `TestVerb_Agent_ToolHook_Arguments`
  (`tests/verbs/agent_test.go`) extends the existing parameterless-tool
  coverage (`TestVerb_Agent_ToolHook`) to a `get_weather(location)` tool
  — proves the LLM populates `arguments.location` from real user speech
  ("What is the weather in Chicago?" → `arguments.location=="Chicago"`),
  corroborated by `turn_end.tool_calls` naming `get_weather` and the
  agent speaking the tool result. `TestVerb_LLM_Deepgram_ToolHook`
  (`tests/verbs/llm_test.go`) is the **first tool-calling test for the
  `llm` verb** (Deepgram Voice Agent): declares `get_weather` under
  `Settings.agent.think.functions`, wires a verb-level `toolHook`,
  asserts the Voice Agent emits a function call (`args.location=="Chicago"`
  — note the `llm` verb uses field `args`, not `arguments`) and that a
  dynamic `FunctionCallResponse` envelope round-trips to a spoken reply.
  New infra: `webhook.Session.ScriptActionHookBodyFunc` (per-request
  dynamic response body — needed because the `llm` verb's tool result
  must echo the live `tool_call_id`, which a static body can't do). New
  local schemas `schemas/callbacks/agent-tool.schema.json` (requires
  `arguments`) and `schemas/callbacks/llm-tool.schema.json` (requires
  `args`), both `TODO: upstream`. **Still out of scope:** `mcpServers`
  discovery, the WS `sendToolOutput` path, `toolFiller`, and
  multi-tool/multi-round tool chains — none of those are covered by
  this change. See `docs/coverage-matrix.md` rows 5.0/5.1.

- **Per-leg call recording as playable WAV (2026-07-03,
  [ADR-0016](docs/adr/0016-per-leg-call-recording.md)).** `RECORD_LEGS=true`
  archives every recorded call leg as `recordings/<test-name>/<leg>.wav`
  (8 kHz mono 16-bit; playable in Finder/browser) so a developer can hear
  back each leg after a run. Off by default (release gate unaffected).
  Zero per-test wiring: the owning test comes from `sip.Config.Owner`
  (stamped once in `claimUAS` with `t.Name()`), the leg name from the
  recording file's basename (`dial-caller.pcm` → `dial-caller.wav`).
  Filenames are stable across runs — re-running a test overwrites that
  test's own subfolder (wiped on the test's first archive of a run), so
  disk stays bounded to the latest run. New: `internal/recording`
  (archiver + unit tests), `sip.SetArchiveHook` (installed in verbs
  TestMain when the flag is on; `internal/sip` imports neither config nor
  recording). Also fixed a latent hole this surfaced: on **remote BYE**
  nothing ever closed the recording file — `setState(StateEnded)` now
  fires `go stopMedia()`, so recordings finalize on peer-initiated
  teardown too. Relative `RECORD_DIR` is anchored at the repo root (found
  via go.mod walk-up), since `go test` sets CWD to the package dir.

- **`listen` verb `mark` feature green (2026-06-30): 2 tests.**
  `TestVerb_Listen_Mark_Playout` and `TestVerb_Listen_Mark_Cleared` in
  `tests/verbs/listen_mark_test.go`. The mark protocol is a
  bidirectional-audio synchronization mechanism handled entirely in
  **mod_audio_fork** (`lws_glue.cpp`), NOT feature-server JS — the JS
  only handles non-streaming `playAudio`/`killAudio`; mark/playout/
  cleared live in mod_audio_fork's *streaming* playout buffer path
  (`bidirectionalAudio.streaming: true`). Flow: our WS server sends raw
  linear16 PCM as **binary** frames → buffered in the playout buffer;
  sends `{type:"mark",data:{name}}` → next binary frame inserts an
  `AUDIO_MARKER` sentinel at the buffer tail; as the buffer drains to
  the caller and hits the sentinel, mod_audio_fork sends
  `{type:"mark",data:{name,event:"playout"}}` back. `killAudio`/
  `clearMarks` discard buffered audio → pending marks return
  `event:"cleared"`. The Playout test additionally records the caller
  leg and asserts real audio (rms≈17700, 6.26s) reached the caller —
  the mark fires because audio played, not by accident. The Cleared
  test bursts a long (~12s) audio block unpaced so `killAudio` arrives
  while the marked position is still deep in the undrained buffer.
  New infra: `webhook.Session.WSConnected(ctx)` (block until jambonz
  dials /ws/<id>), `webhook.Session.WaitWSMark(ctx, name)` +
  `webhook.WSMark{Name,Event}`. Playback audio synthesized via
  `tts.EnsureWAV` (Deepgram), raw PCM extracted by stripping the
  44-byte RIFF header; fixtures cached under
  `tests/verbs/testdata/listen/*.wav` (gitignore-allowlisted, same
  policy as agent). Stable across 3 consecutive runs; no regression in
  the listen/stream suite.

- **Self-provisioning ephemeral suite accounts (2026-05-01).** TestMain
  no longer touches any pre-existing account on the cluster. It uses
  the SP key to (a) sweep stale `it-*` accounts from prior runs, (b)
  create `it-<runID>-{verbs,rest}` under our SP, (c) mint an
  account-scope token for it via POST /ApiKeys, (d) set a synthetic
  sip_realm `<account-name>.smoke.test` via POST /SipRealms, (e)
  provision a Deepgram credential under that account, (f) bring up a
  webhook Application bound to the ngrok tunnel. Suite teardown
  deletes the clients of that account first (upstream FK constraint
  workaround), then the account. **There is no long-lived
  account-scope identity in env any more** — `JAMBONZ_API_KEY` and
  `JAMBONZ_ACCOUNT_SID` are gone. Required env: `JAMBONZ_API_URL`,
  `JAMBONZ_SP_API_KEY`, `JAMBONZ_SP_SID`, `JAMBONZ_SBC_PUBLIC_IP`,
  `NGROK_AUTHTOKEN`, `DEEPGRAM_API_KEY`, `DEEPSEEK_API_KEY`. See
  `internal/provision/suite.go`.
- **Synthetic SIP realm + custom DNS resolver.** The synthetic
  `*.smoke.test` realm has no real DNS; sipgo's transport layer would
  NXDOMAIN. We install a `*net.Resolver` driven by a tiny in-process
  UDP DNS server that answers queries for the realm with
  `JAMBONZ_SBC_PUBLIC_IP` and forwards everything else to the system
  resolver. See `internal/sip/resolver.go`. The `Stack.Config.Resolver`
  field plumbs it into sipgo via `WithUserAgentDNSResolver`.
- **`anchorMedia: true` on `dial` (2026-05-01).** Without it the
  cluster sometimes negotiates a peer-to-peer SDP between the two
  legs using each leg's private NAT'd RTP address (10.x.x.x), which
  neither side can reach — bridge "completes" SIP-wise but no audio
  crosses. Anchored media keeps every packet inside the cluster's
  data plane, reachable via the SBC public IP. Documented inline in
  `tests/verbs/dial_test.go`.
- **Test ergonomics overhaul (2026-05-01).** Major helper landings to
  cut per-test boilerplate ~50% on audio-roundtrip tests:
  - `claimSession(t)` collapses the 5-line "register-webhook-session"
    boilerplate to one line. Used by ~25 tests.
  - `SessionURL(sess, verbSlug)` builds an action-hook URL pre-baked
    with the `?X-Test-Id=<sessionID>` query param so callbacks
    without customerData (eventHook, toolHook, transcribe's
    transcriptionHook, tag verb) route to the per-test session
    automatically. No more manual `q := "?" + ... +
    url.QueryEscape(testID)` footgun.
  - `SessionAckEmpty(sess, verbs...)` variadic empty-action-hook ack.
  - `RunAudioRoundtrip(t, ctx, call, opts)` collapses the 7-step
    answer/record/silence/wait-stt/wav/wait-reply sequence into one
    call. Used by 9 audio tests.
  - `WaitFor(t, name, dur)` for "wait-for-stt" / "wait-for-llm-reply"
    style steps; logs a Step + sleeps + closes.
  - `HangupAndWaitEnded(t, ctx, call)` for the 5-line tail every
    recording-bearing test had.
  - Named timing constants: `RecognizerArmDelay` (1500ms),
    `LLMReplyWindow` (12s), `BridgeSettleDelay` (1500ms),
    `EndedDrainTimeout` (5s), and the existing `WarmupPause` (1s).
  - `ScriptAgent(sess, opts, extra...)` — full agent-verb script
    registration + ack hooks + correlation-aware URLs in one call.
    9 agent tests collapsed.
  - `provisionWebhookApp(t, ctx, suffix)` — the 14-field Application
    builder used by UAC tests (`answer`, `sip:decline`).
  - `helperFatalf(t, step, fmt, args)` — the canonical "setup helper
    fatal" entry point. All `t.Fatalf` from helpers (claimUAS,
    submitAndAwaitOn, resolveFixture, RunAudioRoundtrip, etc) now go
    through it so failures land in the FAILURE SUMMARY block under
    `-parallel`.
  - `Callback.{String,Int,Bool,NestedString,NestedAny,CustomerData}`
    payload accessors on `webhook.Callback` — eliminate 3 hand-rolled
    `extractTranscript`-style functions and tightens diagnostics
    (`cb.NestedString("speech.alternatives.0.transcript")`).
  - `Call.MethodsReceived()` shortcut for the "did we see BYE / INFO?"
    pattern.
- **Suite numbers (2026-05-01):** verbs 47 tests pass parallel
  ~85s; rest 23 tests pass ~27s. Together ~112s for the full release
  gate.

- **Tier 5 `agent` verb green (2026-05-01): 11 tests parallel ~28s wall-clock when run alone.** Self-hosted webhook + ngrok serves the `agent` verb response (no external deploy needed). Inline LLM auth (`agent.llm.auth.apiKey`) bypasses /LlmCredentials provisioning — bring DEEPSEEK_API_KEY in `.env`. STT + TTS use the in-jambonz Deepgram credential we provision at TestMain; offline reply transcripts verified by re-uploading to Deepgram. Per-test eventHook/toolHook routing via `?X-Test-Id=<testID>` query param (server's `extractTestID` was already wired) — no `_anon` contention under parallel.
- Coverage: round-trip echo, eventHook (`user_transcript` / `llm_response` / `turn_end`), `greeting:true`, `actionHook` on end (callInfo + completion_reason + customerData correlation round-trip), `toolHook` round-trip (LLM function call → server replies JSON body → LLM speaks the secret word), `bargeIn` + `user_interruption`, `noResponseTimeout` re-prompt, `turnDetection:"krisp"`, `noiseIsolation` 3 variants. See `tests/verbs/agent_test.go`.
- New infra: `internal/tts/deepgram.go` (cached Deepgram /v1/speak helper for pre-generated user-side WAVs), `webhook.Session.ScriptActionHookBody` (raw JSON body responder, needed for toolHook).
- Drift in `schemas/callbacks/agent-turn.schema.json`: feature-server emits `latency.{stt_ms, eot_ms, llm_ms, tts_ms, tool_ms}` (upstream uses `*_latency` names) and adds `turn_end.{confidence, tool_calls}` not declared upstream. Both kept in the local schema with `DRIFT (TODO upstream)` markers — file a PR upstream when convenient.

- **Tier 3 verb coverage: 23/34 verbs tested, 47 passing + 13 skip-stubs, parallel runtime ~85s.** Audio-bearing tests all verified content-level via Deepgram. Multi-leg tests (`dial`, `conference`, `enqueue/dequeue/leave`) provision two dynamic UASes per test and assert bridged audio passes through. `listen`/`stream` exercised via a generic WS endpoint in `internal/webhook/ws.go`.
- **Per-test SIP isolation via dynamic /Clients provisioning.** Every test now calls `claimUAS(t, ctx)` which: (1) POSTs `/Clients` to provision a fresh `it-<runID>-uas-<hash>` SIP user, (2) brings up a private sipgo+diago stack registered with those credentials, (3) returns a `*UAS{SID, Username, Password, Stack, Inbound}` whose Inbound channel is private to the test. No more shared `currentCall` / `currentCalleeCall` singletons → tests run safely in parallel.
- **`t.Parallel()` everywhere + `-parallel 8`.** Stable across 3+ consecutive runs at ~77-84s. JAMBONZ_SIP_USER / JAMBONZ_SIP_CALLEE_USER env vars no longer required (still consulted for legacy compat but unused in practice).
- **All Phase-1 calls now route through the ngrok webhook** for `call_status_hook` delivery — no more `getaddrinfo ENOTFOUND example.invalid` noise on feature-server side. Tests can opt into asserting on call-lifecycle events via `statusCallbacks(t, within)`.
- **Call-sid → session routing in `internal/webhook/registry.go`** fixes the `_anon` race under parallel: when a webhook arrives with `x_test_id`, we record `call_sid → testID`. Subsequent hooks for the same call (incl. ones jambonz strips correlation from like `tag` verb's customerData replacement and `transcribe`'s transcriptionHook) route by call_sid rather than landing in the shared `_anon` bag.
- **Full SIP observability landed.** `call.Received()` / `call.Sent()` capture every in-dialog request + response via the sipgo middleware chain — no diago/sipgo fork. Tests assert on BYE headers, INFO headers, REFER Refer-To, etc.
- **Generic WebSocket utility** — `internal/webhook/ws.go` exposes a session-routed `/ws/<id>` endpoint that's not tied to audio. Ready for future AsyncAPI / `llm` / `agent` tests.
- **Shared helpers in `tests/verbs/helpers_test.go`** — `V(verb, kv...)` kills map-literal noise, `claimUAS(t, ctx)` for per-test SIP isolation, `placeCallTo` / `placeWebhookCallTo` / `placeWebhookCallToNoWait` for routing calls to a specific UAS, `AnswerRecordAndWaitEnded` consolidates the lifecycle dance, `AssertAudioDuration/Bytes/TranscriptContains` + `RequireRecvMethods/SentStatus` + `WithWarmupScript` cover common patterns.
- **Transcript verification via Deepgram** — content-level assertions on every audio-bearing test. `DEEPGRAM_API_KEY` gated — tests log-skip when absent. See `internal/stt/`.

### Tier 3 Phase 1 snapshot

- `internal/sip/` — unified `Call` for UAC + UAS. Methods: `Trying`, `Ringing`, `Answer`, `Reject`, `Hangup`, `StartRecording`/`StopRecording`, `SendSilence`, `SendDTMF`, `WaitState`, `Done`, `Sent`, `Received`, `Header`, `From`, `To`, `Codec`, `RMS`, `PCMBytesIn`, `AudioDuration`, `ReceivedDTMF`, etc.
- Two-transport sipgo Stack (TCP + UDP listeners), TCP used for REGISTER. `setupWebhook` in `TestMain` optionally provisions a webhook Application bound to the ngrok tunnel.
- 16 verb tests green (see `tests/verbs/*_test.go`).

### Tier 3 Phase 2 snapshot (partial)

**What's working:**
- `internal/webhook/` — `{types,registry,correlation,server,tunnel}.go`. Server binds to loopback; ngrok tunnel forwards https traffic via `webhook.StartNgrok`. Routes: `GET|POST /hook`, `POST /status`, `POST /action/<verb>`, `/health`.
- `Registry` + `Session` — per-test script registration (`ScriptCallHook`, `ScriptActionHook`) and callback capture (`WaitCallback`, `WaitCallbackFor`).
- TestMain bring-up: spins tunnel, provisions a dedicated Application pointing at ngrok URL, deletes on teardown. Phase-2 tests skip cleanly if `NGROK_AUTHTOKEN` unset.
- First test authored: `tests/verbs/gather_test.go` — places a call via `POST /Calls` with `application_sid=webhookApp`, expects `action/gather` callback with digits.
- Schemas vendored from `@jambonz/schema` into `schemas/{verbs,callbacks,components}/` + `schemas/jambonz-app.schema.json`.

### Tier 1 snapshot

- 12 tests green, 10 resource types covered with hand-authored JSON Schemas:
  Applications · Accounts (SP + account scope) · ApiKeys · VoipCarriers · SipGateways · PhoneNumbers · MsTeamsTenants · Lcrs · ServiceProviders (read) · Sbcs (read) · Availability (read).
- **Contract architecture.** Every response validated against `schemas/` (hand-authored, committed, not sourced live from api-server). Loader: `internal/contract`. Client: `internal/provision`. Runtime deps: `santhosh-tekuri/jsonschema/v5` only.
- **Scope model.** Two clients side-by-side: account-scope and SP-scope (one is the default service provider, not the red-herring SID returned by `/ServiceProviders` for the account). Tests mark which scope they need; SP-only tests skip cleanly when SP creds absent.
- **Orphan sweeper.** Generic `Sweeper` interface — per-resource list+filter+delete. Runs at `TestMain` for Applications, VoipCarriers, Lcrs, Accounts (SP). Other resources auto-clean via `t.Cleanup` and don't need sweepers until we exercise stress/parallel scenarios.
- **Drift findings** surfaced by the contract/test layer and folded into local schemas + Go structs:
  1. `Webhook.method` — live returns uppercase, swagger enum lowercase only.
  2. Optional fields across all resources return `null`; swagger types them as plain strings.
  3. `VoipCarrier.register_status` — live returns object, swagger says string.
  4. `SipGateway.inbound/outbound` — live returns `0/1`, swagger says boolean.
  5. `PhoneNumber.number` normalised (leading `+` stripped).
  6. `Lcr` create rejects `description` column.
  7. MsTeamsTenant swagger has typo `account` (should be `account_sid`).
  8. `LcrRoutes` POST endpoint inconsistent between swagger and live — deferred to Tier 2.

### Critical safety rule (post-mortem 2026-05-01)

A previous session destroyed user data by client-side-filtering a
`GET /Clients` response that the upstream cluster does NOT actually
filter server-side. The harness now treats the whole jambonz
`/Clients` endpoint as **list-all-cross-account, even with an
account-scope or SP-scope token**. Hard rule baked into
`internal/provision/sip_clients.go`: never iterate `ListSIPClients`
result and DELETE — only `ListSIPClientsForAccount(ctx, sid)` filters
by AccountSID client-side, AND every delete site re-checks
`cl.AccountSID == ourSID` before issuing DELETE. The
`AccountSweeper` documents the same rule.

The harness will only ever delete:
1. Resources whose name starts with `it-` (verified by reading the
   `name` field in the response body — never trusting the server-side
   filter).
2. Resources whose `account_sid` matches an ephemeral suite account
   we just created.

If you add a new sweeper, follow this pattern. If something looks
broken in the cluster after a test run, the first hypothesis to rule
out is "did the sweeper hit something it didn't own". Audit the
incident before assuming flake.

### Known issues

0. **~~`SIPClient.is_active` drift.~~** ✅ **RESOLVED 2026-05-01.** Live `GET /Clients` returns `is_active` as a number (0/1), not a bool. Fixed by changing `provision.SIPClient.IsActive` to use the existing `IntField` type (same drift pattern as `SipGateway.inbound/outbound`).

1. **~~`X-Test-Id` correlation not reaching the webhook server.~~** ✅ **RESOLVED 2026-04-20.** Root cause was api-server's `validateCreateCall` (`<api-server-checkout>/lib/routes/api/accounts.js:415-434`), which clobbers the caller's `call_hook` with the Application's fixed URL when `application_sid` is set. Fix: use the POST /Calls top-level `tag` field — feature-server surfaces it as `customerData` on every webhook payload. `internal/webhook/correlation.go` defines `CorrelationKey = "x_test_id"` and reads `customerData[CorrelationKey]`.
2. **~~DTMF digit-shift on `SendDTMF`.~~** ✅ **RESOLVED 2026-04-20.** Two layered bugs in diago's DTMF sender:
   - `RTPDtmfWriter.writeDTMF` passes `timestamp_increment=0` to `WriteSamples` for every packet, including across digits — so `"1234"` all shared RTP timestamp 0 and receivers dedup them as one stuck event.
   - freeswitch (jambonz's media layer) treats each of RFC 4733's 3 recommended end-of-event retransmissions as a **separate completed event**, so sending `1 end end end 2` becomes `1 1 1 2`.
   **Fix:** bypass diago's `AudioWriterDTMF` and drive `RTPPacketWriter.WriteSamples` directly (`internal/sip/call.go:SendDTMFWithDuration`). Per-digit layout is N interim packets (250ms @ 20ms ptime = 12 packets) at timestamp T, then **one** end-of-event packet at timestamp T, then advance to T + duration + 40ms silence before the next digit. Feature-server logs confirmed four discrete `TaskGather:_onDtmf` events for `"1234"`. `SendDTMF(digits)` defaults to 250ms/tone; `SendDTMFWithDuration` for callers that want to vary it.
3. **Vendored @jambonz/schema uses absolute URL `$ref`s** (e.g. `https://jambonz.org/schema/callbacks/base`). santhosh-tekuri's jsonschema can't resolve those from disk without a custom Loader. Mitigated in `validateInbound` by detecting `"no Loader found"` substring and logging Debug instead of Error, but this means callback contract validation is **effectively disabled** for any schema that uses these refs. Fix: (a) write a Loader that maps `https://jambonz.org/schema/...` → local `schemas/...`, or (b) rewrite the vendored schemas at copy-time to use relative file refs.

### Deferred to Tier 2

- **LcrRoutes + LcrCarrierSetEntries** — swagger/live mismatch, needs investigation of `/Lcrs/{sid}/Routes` compound endpoint.
- **GoogleCustomVoices** — depends on SpeechCredentials.
- **SpeechCredentials** — needs real vendor creds to exercise the create path cleanly (jambonz validates the key on create); skipping keeps Tier 1 credential-free.
- **Users create/delete** — swagger POST returns 204 with no body, no way to capture SID in response; `GET /Users/me` is the only useful read path. Revisit in Tier 2.
- **Clients (SIP users)** — not in the swagger.
- **Webhooks fetch-only** — skipped; not actionable without creating Webhooks via their parent resources first.

### Resume plan

**Next session, start here (post 2026-05-01 refactor):**

1. **Pre-flight cluster verification.** Run `go run ./cmd/probe` on
   the target cluster to confirm: (a) SP can create accounts, (b)
   ApiKey minting works, (c) SipRealm endpoint accepts a synthetic
   realm (it'll return 500 from the DNS layer but that's fine), (d)
   resolver-routed REGISTER + POST /Calls roundtrip works. If any
   step fails, the error message tells you which jambonz layer needs
   investigation. Then `go test ./tests/...` should be green.

2. **Two persistent items left from earlier sessions:**
   - **Schema URL `$ref` loader** for vendored `@jambonz/schema` —
     santhosh-tekuri's jsonschema can't resolve absolute URL refs
     from disk. Currently mitigated by detecting "no Loader found"
     and logging Debug. Real fix is a `jsonschema.Loader` that maps
     `https://jambonz.org/schema/<path>` → local `schemas/<path>`.
   - **Upstream PR for diago DTMF** — the `RTPDtmfWriter.writeDTMF`
     timestamp-0-for-every-packet bug. Worth filing against
     `emiago/diago` so we can drop our own `SendDTMFWithDuration`
     workaround at some point.

3. **Tier 4 / 6 / 7 backlog** (per [docs/coverage-matrix.md](docs/coverage-matrix.md)):
   - Tier 4 — advanced verbs (most are vendor-gated)
   - Tier 6 — `PUT /Calls/{sid}` matrix
   - Tier 7 — WebSocket API tests over the existing `/ws` endpoint
   - Tier 5 tool-calling depth (2026-07-05 added arguments-extraction for
     `agent` + first `llm` toolHook test) — remaining gaps still open:
     `mcpServers` discovery, WS `sendToolOutput` path, `toolFiller`,
     multi-tool/multi-round tool chains. See coverage-matrix rows 5.0/5.1.

3. **Decide on schema URL-ref strategy (issue #3).** Either:
   - Write a `jsonschema.Loader` that maps `https://jambonz.org/schema/<path>` → `file://<repo>/schemas/<path>`, or
   - Rewrite vendored schema files to use relative refs (one-time script).
   Recommend the Loader approach — preserves upstream schemas unchanged, allows `jambonz-app.schema.json` to validate full verb arrays.

3. **Then run each verb test over WS transport** as well (per user request: "run both"). WS handler is scaffolded in types only — not yet wired in `server.go`. Needs an `/ws` route with `gorilla/websocket` (dep to add) that parses the AsyncAPI `call.yml` message shapes.

4. **Upstream fix for diago DTMF.** The timestamp-0-for-every-packet bug in `RTPDtmfWriter.writeDTMF` affects anyone using diago for outbound DTMF. Worth filing a PR against `emiago/diago` with a fix (advance `sampleRateTimestamp` on the first packet of each event and on the end-of-event packet).

### Tier 2 landed (2026-04-19)

8 new tests added on top of Tier 1:

- **PUT** — Applications, VoipCarriers, Accounts. Each uses a dedicated `*Update` struct to avoid sending immutable primary-key fields (real drift finding: PUT rejects those with 400).
- **Read-only paginated** — RecentCalls, Alerts. Shared `Paginated` envelope. Drift finding: `total`/`page`/`batch` come back as **strings**, not integers (swagger says integer); `batch` may be omitted entirely. `IntField` unmarshaller + relaxed schema accept both shapes.
- **Read-only simple** — WebhookSecret (get, not regenerate), RegisteredSipUsers.
- **Error cases** — 404 on missing SID surfaced as structured `*APIError`; cascade-policy observation test for account-with-app delete (finding: `jambonz.me` cascades; swagger suggested 422).

**Full suite: 20 tests, 19.6s, all green.**

### Remaining Tier 2 items (deferred)

- PUT on PhoneNumber / SipGateway — same pattern as landed PUTs, low release-gate value.
- Bulk create (LcrRoutes, LcrCarrierSetEntries, GoogleCustomVoices) — swagger/live mismatch on LcrRoutes POST; needs `/Lcrs/{sid}/Routes` compound endpoint investigation.
- TtsCache/Synthesize — requires real SpeechCredential.
- SipRealms, AppEnv — low priority, not a release-gate signal.

### Next (pick one to start)

In priority order:

1. **Tier 1 — REST platform CRUD scaffolding.** Land repo scaffold (`go.mod`, `.gitignore`, `.env.example`, `Makefile`) + `internal/config` + `internal/provision` client + contract-validator layer + `tests/testmain_test.go` + **Applications CRUD as the pilot resource**. Exit criteria: `go test ./tests/rest/applications_test.go` creates, GETs, contract-validates, and deletes a real Application on `jambonz.me`. Once Applications is green, the pattern repeats mechanically for the other ~15 resources in Tier 1.
2. **Delete the spike** at [spikes/001-sipgo-diago/](spikes/001-sipgo-diago/) — only after the first SIP test (Tier 3) is green and the spike's role as archived evidence is fulfilled. Not before.
3. **Resolve Tier 1 open questions** (see below) before Tier 2.

### Open questions (resolve before they block progress)

- **Upstream swagger feedback (not blocking).** Two items found during Applications work that upstream should ideally fix:
  - `Webhook.method` enum case — swagger has `["get","post"]`, live returns `"POST"`. Our local schema accepts both. File swagger PR when convenient.
  - Optional fields return `null` in live responses but swagger declares them as `{type: string}`. Our local schemas declare them `["string","null"]`. Idiomatic fix upstream is `nullable: true` (OpenAPI 3.0) on each optional field, or omit nulls from responses.
- **MsTeamsTenants required field typo.** Fern YAML says required field is `account`; verify against live API when Tier 1 row 1.9 runs.
- **ngrok auth token.** Needed for Tier 3 onward. Not needed for Tier 1/2. Defer.
- **SP-scope is live and connected** (token + SID configured via `JAMBONZ_SP_API_KEY` / `JAMBONZ_SP_SID`). SP tests create their own accounts under this test SP — no risk to prod data.

### Blockers

None.

---

## Session log (reverse-chronological)

### 2026-08-26 — a WebSocket app parked the caller after `dial`; fixed + 2 tests

When a verb list empties, feature-server ends the call for an HTTP application
but parks a WebSocket one in `CallSession._awaitCommandsOrHangup()` waiting for
verbs to be pushed over the socket. That is right when the app was asked for
verbs and is thinking; it is wrong after a `dial` the app was never asked about:

- `dial` with **no `actionHook`** — nothing was sent, nothing is coming back.
- `dial` whose **actionHook failed** — over WS the only failure mode is the app
  never acking, which `WsRequestor` abandons after
  `JAMBONES_WS_API_MSG_RESPONSE_TIMEOUT` (5s).

Either way the B leg is gone and the caller sits in dead air until `timeLimit`
or a media timeout. Measured on jambonz.me before the fix: no BYE at all within
a 10s/15s budget of the B-leg hangup.

Fixed in feature-server (private, `lib/tasks/dial.js` +
`lib/session/call-session.js`): `TaskDial._endSessionUnlessHandedOff` calls the
new `CallSession.expectNoFurtherVerbs`, which suppresses the await for that one
task iteration so the loop falls out and tears the call down normally. Skipped
when the app already took the call elsewhere — an LCC redirect that replaced the
dial task (`KillReason.Replaced`) or a completed REFER (`KillReason.ReferComplete`)
— since those have follow-on verbs or another session in play. **Not in OSS**:
`jambonz/jambonz-feature-server@origin/main` (db18b55) has the identical
un-fixed code, so there was nothing to clone.

New tests, `tests/verbs/dial_no_action_hook_test.go` (coverage matrix row 3.5a):
`TestVerb_Dial_WS_NoActionHook_EndsCall` and
`TestVerb_Dial_WS_ActionHookNoAck_EndsCall`. Both place against `wsApp`, script a
lone `dial` with **no trailing `hangup`** (a `hangup` would make them pass either
way), let the callee hang up the B leg, and assert the caller's BYE arrives within
a budget measured from the B-leg BYE. `timeLimit` is 90s so a regression can't be
rescued by the max-duration timer. Verified red → green across a deploy: 0.5s (no
hook) and 5.5s (5s ack timeout + teardown).

Two things worth remembering:

- **A hook can only be failed over WS by withholding the ack.** New harness knob
  `Session.ScriptActionHookNoAck(verb)` + `HookOutcome.NoAck`; the appws read loop
  captures the frame and simply doesn't reply. HTTP ignores it (the handler must
  always write a response).
- **The actionHook must be a RELATIVE path to reach the socket at all.**
  `ws-requestor.js` (~line 97) short-circuits an absolute `http(s)` hook to a plain
  HTTP webhook. The first draft used `SessionURL(sess, "dial")` and the hook went
  out over ngrok, so the withheld ack never happened and the test failed even with
  the fix deployed. There is now an `assert-ws-got-dial-hook` step that fails loudly
  if the hook doesn't arrive on the socket, so this can't silently regress to
  vacuous.

Pre-existing and unrelated: `TestVerb_Transfer_WarmThreeWay` fails on
`assert-bridge-audio-transcript` (transcript is "briefing the agent now", missing
"shining" from the target's WAV). Reproduced identically with the fix reverted on
the box, so it is not from this work — the target's reference audio isn't reaching
the caller's recording in three-way mode.


### 2026-08-19 — permitted_marks was losing the comma; smoke test added

`punctuation_overrides.permitted_marks` travelled to the media server as a
comma-separated channel variable, so a list that CONTAINS a comma lost it:
`[".", ",", "?", "!"]` became `".,,,?,!"` and split back to `[".", "?", "!"]`.

Not cosmetic. With commas disallowed the engine substitutes full stops and
marks them `is_eos`, so an application splitting sentences on `is_eos` sees one
sentence as two. Measured on the same audio, changing only the mark list:

    comma permitted    8 is_eos, 2 commas   "...para hoy, por favor."
    comma dropped     10 is_eos, 0 commas   "...para hoy. Por favor."

Total punctuation stayed 10 in both; two commas simply became full stops, which
flipped their `is_eos` false -> true.

Fixed by encoding the list as JSON (feature-server `fix/speechmatics`,
mediajam `fix/speechmatics-permitted-marks`, which still accepts the old CSV so
deploy order does not matter). `max_delay` was NOT a factor: `is_eos` stays 10
at 0.7, 2.0 and 4 — that knob only moved the stray space-before-period
artifacts, a separate defect fixed earlier.

**New: `TestVerb_Speechmatics_PermittedMarks`.** Uses `[",", "?", "!"]` —
commas permitted, full stops not — so one call catches both failure modes:

    marks arrive intact     2 commas, 0 full stops   pass
    comma lost in transit   0 commas, 0 full stops   fails on commas
    marks never sent        2 commas, 8 full stops   fails on full stops

Verified RED before the fix (`map[!:4 ?:5]`) and GREEN after
(`map[!:4 ,:2 ?:4]`). The two older tests in the file could not catch this:
their recognizer carries no `speechmaticsOptions`, so nothing exercised the
option encoding. Worth remembering when adding vendor-option coverage.

Same latent bug still exists for `SPEECHMATICS_SPEECH_HINTS` (also `join(',')`)
and for deepgram keywords — a hint containing a comma would be split. Not hit
yet, not fixed.

**Also new: `TestVerb_Speechmatics_EndOfUtterance`.** The vendor's EndOfUtterance
had no coverage at all — no test touched `interim`, `speech_event` or
`EndOfUtterance`, even though two bugs had already been found and fixed there
(transcribe had no handler, so events were dropped; and the event was posted
unnamed, so `_resolve` put neither `speech` nor `speech_event` in the body). The
test pins both: at least one `speech_event` with `type: EndOfUtterance` arrives,
no hook arrives carrying neither key, and transcript payloads keep flowing
alongside. Trigger pinned at 0.4s — the mid-utterance gap is 0.60s on word
timings, so 0.5 sits 0.1s from the edge and fires only sometimes.


### 2026-08-18 — Speechmatics results/transcript pass-through pinned

Two new tests in `tests/verbs/speechmatics_passthrough_test.go` (fixture
`testdata/es_reservation.wav`, a 14.8s Spanish restaurant-reservation clip at
mono/8k/16-bit), covering a report that Spanish transcripts arrived
re-punctuated and re-cased and that Speechmatics' `results[]` was "lost".

**What the payloads actually show.** jambonz does not touch the text — the
returned transcript is byte-identical to the vendor's `metadata.transcript`
strings concatenated, and all 44 `results[]` entries arrive complete with
`start_time` / `end_time` / `confidence` / `type` (+ `attaches_to` /
`is_eos` on punctuation). Nothing is lost.

**The real finding is that `speech.vendor.evt` has two shapes.** A turn from
a single `AddTranscript` yields the raw vendor object (`vendor.evt.results`
works); a consolidated turn yields an array of jambonz-normalized objects
and the raw payload moves one level deeper, to
`vendor.evt[i].vendor.evt.results`. With the `max_delay` jambonz used to send
the engine finalized roughly per word, so the array shape is what
applications see essentially always — a 1.3s "the sun is shining" already
arrives as 4 segments. The tests assert data completeness in EITHER shape
and deliberately do NOT demand the raw object: flattening it would break
every application reading the array today.

**The punctuation complaint traced to config, not text handling.** Same
audio, same engine, only `transcription_config` differing: engine defaults
give `"...a la lactosa. A nombre de Sergi Prieto."`, while the `max_delay:
0.7` jambonz forced gives `"...a la lactosa . A nombre de Sergi Prieto ."`
— punctuation lands in its own segment because the punctuator has almost no
right context. Fixed in feature-server (stop forcing the default) and
verified on jambonz.me: the stray artifacts are gone and accents come back
(`"Si, correcto"` -> `"Sí, correcto"`).

**Turn boundaries, measured.** With `asrTimeout` raised to 10-20s so the
silence timer could not explain it, `gather` still posted ~1.45s after the
audio ended, and the timing tracked `end_of_utterance_silence_trigger` —
so `gather` genuinely ends turns on the vendor's `EndOfUtterance`.
`transcribe` under the identical config posted at +21.554s, i.e. it ignored
`EndOfUtterance` entirely and only the timer ended the turn. Fixed in
feature-server; after deploying, the same probe measured +1.502s.

**Probe variants not kept.** Five assertion-free timing probes
(`max_delay` override, EOU trigger 0.5/2.0, clamped/unclamped, `asrTimeout:
0`) were written to isolate the above and then dropped rather than pushed —
they each cost a live call in the gate and asserted nothing. Their numbers
are in this entry. Note trigger 2.0 produced no `EndOfUtterance` at all
across three runs; 0.5-1.5 is the band that behaved.

### 2026-08-13 — SDP-direction tests promoted into the default gate + offer-mode coverage

Two coverage gaps closed after verifying the Five9 fix live.

**1. The regression guard now runs by default.** The three sendonly tests
were in `tests/drachtio`, behind the `drachtio` build tag — so `make test`,
`make test-report` and the release gate never ran them; only an explicit
`make test-drachtio` did. A customer-facing carrier-interop guard that
nobody runs rots. Moved to `tests/verbs/sdp_direction_test.go` (renamed
`TestSDP_Sendonly_*`), rewritten to the CLAUDE.md failure-fast pattern
(`WithTimeout` + `Step` + `s.Fatalf`, so failures reach the FAILURE SUMMARY
— which the drachtio package structurally cannot do), and added to
`RELEASE_GATE_VERBS`. Whole file costs ~20s. The drachtio package keeps only
its genuinely slow session-timer tests.

**2. New: `TestSDP_OfferMode_EarlyAnswerDirectionChange`** — the side of the
fix nothing exercised over SIP. Everything else drives the leg where jambonz
ANSWERS; this drives the leg where jambonz OFFERS (`dial` verb B leg), where
the far end's SDP arrives as an answer, possibly twice (183 then 200). A
media server must not re-render its own offer as an answer there — mediajam's
first cut of the fix did exactly that for the second answer. The callee
answers twice with DIFFERENT directions (183 `a=recvonly`, then 200
sendrecv) and the test asserts the dial completes AND real audio bridges
afterwards, which it cannot if the B-leg SDP was rewritten mid-sequence.
Needed one harness addition: `jsip.EarlyMediaSDPWithDirection` (the old
`EarlyMediaSDP` hardcoded `a=sendrecv`; it now delegates, so no caller
changed).

Gotcha found in the first run: all three sendonly tests shared one
Application name and 422'd on `applications_idx_name` under `-parallel`.
`sdpDirectionApp` now derives the name from `t.Name()`.

**mediajam side (branch `fix/sdp-answer-direction`, commit 5ac62d3).** An
independent verification review confirmed the four earlier findings are
genuinely fixed (each mutation-tested) and turned up three more, now fixed:
`Endpoint.LocalSDP()` became a data race once `Modify` started re-rendering
`localSDP` mid-call (unreachable today — control dispatch is serial — but
the write-once invariant it relied on is gone, so it takes `e.mu`);
`offerPending` was left write-only with a misleading doc comment (deleted);
and `TestModifyDynamicPTRemap` asserted only the SDP, so deleting the
`SwapCodec` remap left the suite green — added `media.Session.PayloadType()`
plus an assertion, verified by mutation. Two limitations are now recorded in
`docs/control-protocol.md` rather than left implicit: an offer-mode endpoint
still answers a genuine hold re-INVITE with its verbatim offer (the control
protocol has no answer-vs-offer discriminator on `endpoint.modify` — the SIP
layer knows, the protocol can't say), and a mid-call telephone-event PT
remap reaches the SDP but not the session's DTMF PT.

**Cluster state:** the DB migration to 11.1.1 landed (schema_version 11.1.1,
40 → 50 tables), deepgramflux both green, release gate green, and the
deployed mediajam binary carries both fix commits (verified by finding the
`answer SDP re-rendered on modify` log string in it).

### 2026-08-12 (night) — "make test-report fails" debugged: 4 stacked causes, none the suspected merge

User suspected merge 4796ced (release-gate hardening) broke the suite.
Verified innocent — the same tree was green at 13:45. The actual causes,
peeled in order:

1. **ngrok monthly HTTP quota exhausted (ERR_NGROK_727).** Every hook
   fetch got 403 HTML from the ngrok edge → feature-server "error
   retrieving or parsing application" → 480 on every webhook call.
   Proven by curling the tunnel URL from the bastion itself. NOTE: the
   fresh token the user made went into `.env.jambonz.me`, which NOTHING
   reads — config loads `.env` only. Moved the token; fixed.
2. **Concurrent suite runs fight over ngrok** (free tier = 1 live
   endpoint). A backgrounded run held the tunnel; the user's own run
   died in verbs TestMain with "endpoint already online" → verbs
   reported 0 tests. Also: after killing an agent hard, ngrok takes
   ~1-2min to release the session — immediate reruns hit the same error.
3. **test-report's hardcoded 300s go-test timeout** (predates the
   merge) vs a suite that has grown to 154 tests (~10min at
   -parallel 4). The timeout fired mid-run and killed 79 in-flight
   tests — the "mass failure" look. Fixed: `TIMEOUT_REPORT ?= 900s`.
4. **REGISTER Timer_B cascade under churn.** drachtio log trace: our
   own FIN arrives 0.2ms behind the REGISTER (sipgo "TCP ref went
   negative" refcount bug closes the conn right after the write), the
   401 challenge has no path back, claimUAS dies after 32s, failures
   accelerate churn → cascade. Fixed: bounded retry (3 attempts) in
   `Stack.register()`; SIP-level rejections (`*diago.
   RegisterResponseError`) still fail immediately. Upstream sipgo issue
   worth filing.

**Post-fix full run: 154 tests — 136 pass / 5 fail / 13 skip, zero
cut-off.** The 5 remaining are cluster-side casualties of today's
redeploy/mediajam-revert, exactly what the gate exists to catch:
3× handoff (feature-server warm-transfer/handoff hotfixes from
2026-07-13/14 rolled back by the 11.0.4 redeploy — they were never
mirrored upstream, as HANDOFF warned), Dial_EarlyMedia_CodecChange
(sbc-outbound empty-m= regression back in the deployed build), and
Say_Stream_DeepgramFlux (rms=0 — `DEEPGRAMFLUX_TTS_URI` env likely
missing from `/etc/default/mediajam` after the revert).

Fixes on branch `fix/test-report-timeout-register-retry` (pushed).
Related: the sendonly test work + mediajam fix sit on
`test/sdp-sendonly-direction` and mediajam `fix/sdp-answer-direction`;
the mediajam revert means those drachtio tests are RED again until the
mediajam PR merges + deploys.
### 2026-08-12 (evening) — adversarial code review of the sendonly work; all findings fixed

**Scope:** /code-review (high) over both change sets. 10 findings survived
adversarial verification; all fixed except the one explicitly deferred.

**mediajam (branch `fix/sdp-answer-direction`, second commit):**
- **CONFIRMED regression fixed:** only the FIRST answer to our own offer was
  protected (`offerPending`); a second answer (multi-183 → 200 sequences)
  was misread as a re-offer and rewrote the offer-mode endpoint's local SDP
  into a single-codec "answer". New persistent `Endpoint.offerMode` field
  gates the refresh; offer-mode endpoints never rewrite localSDP in Modify.
- **CONFIRMED gap fixed:** Modify's third exit (`rekeyCodec` — off-geometry
  codec change on an isolated leg) returned the creation-time SDP verbatim,
  so a direction+codec-changing re-INVITE still got the stale sendrecv
  answer (the Five9 bug on that path).
- **Staleness fixed structurally:** the three Modify exits now share one
  helper, `currentLocalSDPLocked()`, which re-renders the cached answer
  whenever the current negotiation state (direction, codec, payload types)
  no longer matches it byte-for-byte — keeping o= sess-id, bumping only
  sess-version. This subsumes the direction-only `refreshAnswerSDPLocked`
  (deleted, along with the now-redundant `localDir` field).
- **PT-remap fixed:** Modify's same-codec path now mirrors a re-offer's
  dynamic payload-type change (e.g. Opus 96→111) into both the media
  session (`SwapCodec`, same geometry so it cannot fail on it) and the
  rendered answer.
- 4 new regression tests: multi-answer offer-mode, rekey+direction,
  swap-with-unchanged-direction codec freshness, dynamic PT remap.

**smoke-tester:**
- The initial-offer plumbing guard now asserts on the TRANSMITTED INVITE
  body via new `Call.InviteOfferSDP()` (internal/sip/call.go) instead of
  `LocalSDP()`, which diago mutates during negotiation — an RFC-valid
  `a=inactive` answer would have false-REDed the harness.
- `Stack.Invite` collapsed to a single NewDialog→Invite→Ack path (verified
  byte-equivalent to diago's `Diago.Invite`); `SDPMode` is validated against
  the four RFC 4566 direction constants before any SIP traffic, with an
  ADR-0014 caveat documented (recvonly/inactive silently disable diago's
  RTP writer).
- `tests/drachtio/sdp_direction_test.go`: dead `res == nil` guards removed
  (SendReinviteWithSDP never returns nil,nil), the thrice-pasted re-INVITE
  block folded into `reinviteDirection(t, ctx, call, mode, label)`.
- **Deferred (finding 10):** the drachtio package's raw `t.Fatalf` usage
  contra CLAUDE.md's failure-fast pattern — package-wide pre-existing
  convention (~69 raw calls); the mandated Step/WithTimeout helpers are
  unexported `_test.go` symbols in tests/verbs and structurally unavailable
  here. Real fix is porting the failure-summary helpers into tests/drachtio
  (or a shared package) — its own work item, not patched per-file.

**Verification:** mediajam endpoint suite green (incl. 4 new tests) on mac
and with krisp tags; smoke-tester build/vet/race green; all 3 sendonly
drachtio tests green LIVE against jambonz.me. NOTE while verifying: the
whole jambonz.me pm2 stack restarted ~08:50 UTC and `/usr/local/bin/mediajam`
was replaced at 08:08 UTC by a root-owned build (user's deployment in
motion — sendonly tests pass against it, so it carries the fix).
`TestVerb_Answer_Basic` began 480ing after that restart — A/B-verified NOT
caused by the smoke-tester changes (fails identically with them stashed);
re-check once the user's deployment settles.

### 2026-08-12 (later) — mediajam fix authored, deployed to jambonz.me, sendonly tests GREEN

**Scope:** fix the a=sendonly answer-direction bug the morning's tests
reproduced, deploy to jambonz.me, verify.

**Fix (mediajam branch `fix/sdp-answer-direction`, commit b68cc20, pushed;
no PR yet):** `internal/endpoint/sdp.go` hardcoded `a=sendrecv` in every
rendered SDP, and `Endpoint.Modify` returned the answer SDP cached at
endpoint creation, so re-INVITE direction changes were never re-answered.
Changes: `negotiateRemote` parses the offer's direction attribute
(session-level, media-level wins); answers carry the RFC 3264 §6.1
complement; `Modify` re-renders the cached answer when the re-offer needs a
different direction, keeping o= sess-id and bumping sess-version (§8);
unchanged direction returns the cached SDP byte-for-byte; an answer to our
OWN offer (offer-mode endpoint, `offerPending` at entry) never rewrites our
local SDP. RTP send behaviour deliberately untouched. 9 new unit tests in
`internal/endpoint/sdp_direction_test.go` incl. a Modify-level hold/resume
sequence.

**Deploy (jambonz.me):** the box's build tree `/usr/local/src/mediajam` is
on `feat/dialogflow_ces_tool_calls` **5 commits ahead of origin** (unpushed
work — the deployed binary is built from it; do NOT deploy main over it).
So: `git format-patch` the fix → copied tree to `~/build/mediajam-sendonly`
(krisp SDK staging preserved) → `git apply` → build with the box's own
`build.sh` flags (`-tags "nolicense krisp"`, CGO krisp paths) → backed up
old binary to `/usr/local/bin/mediajam.bak-sendonly-fix` → install +
`systemctl restart mediajam`. Box `.git` has root-owned objects (past sudo
git run) that block fetches as admin — the patch route avoids that.

**Verification:** all 3 `TestDrachtio_Sendonly_*` GREEN against jambonz.me
(initial offer answered recvonly; corrective re-INVITE 200 + recvonly with
o= version bump; hold recvonly → resume sendrecv). Full drachtio suite:
only pre-existing red is `TestDrachtio_SessionTimer_UASRefresher` ("200 OK
has no Session-Expires") — **A/B verified against the pre-fix binary:
fails identically**, so it's drachtio session-timer config drift, not the
mediajam change. Full verbs suite at `-parallel 8` hit the 600s timeout
(known agent-family flake pattern, see 2026-07-03 entry); the
deterministic release-gate subset was run instead — see result below.

**Follow-ups:** (a) open the mediajam PR for `fix/sdp-answer-direction`;
(b) merge/rebase story for the box's 5-unpushed-commit
`feat/dialogflow_ces_tool_calls` tree; (c) drachtio session-timer config
drift (UASRefresher red); (d) decide whether recvonly answers should also
mute mediajam's RTP sender (deliberately out of scope here).

### 2026-08-12 — Five9 a=sendonly SDP-direction tests (RED: repro on live)

**Scope:** Five9 (as a carrier) sends its initial INVITE with a one-way
media offer (`a=sendonly`). jambonz answered 200 OK `a=sendrecv` instead of
the RFC 3264 §6.1 complement (`recvonly`/`inactive`), so Five9 re-INVITEs
with `a=sendonly` to correct it — and that surprise renegotiation broke the
freeswitch endpoint update. User asked for tests pinning how jambonz
behaves on `a=sendonly` (fix believed to exist in mediajam).

**Done — 3 tests in `tests/drachtio/sdp_direction_test.go` (drachtio tag):**

- `TestDrachtio_Sendonly_InitialOffer` — a=sendonly initial offer; asserts
  the 200 OK SDP direction is recvonly (inactive tolerated + logged).
  Includes a plumbing guard: negotiated local mode must be sendonly, else
  the offer never carried the attribute and the test is meaningless.
- `TestDrachtio_Sendonly_ReinviteLoop` — Five9's second step: after the
  sendonly INVITE (answer direction logged, not asserted), send the
  corrective in-dialog re-INVITE re-asserting a=sendonly; asserts 200 (488
  = endpoint renegotiation broke), recvonly/inactive answer, dialog alive.
- `TestDrachtio_Sendonly_HoldResume` — generalized surface: sendrecv call,
  re-INVITE to sendonly (hold), re-INVITE back to sendrecv (resume);
  asserts 200 + correct direction complement at each step — resume pins
  that endpoint updates still work AFTER a sendonly renegotiation.

**New infra:** `jsip.InviteOptions.SDPMode` (`internal/sip/uac.go`). diago's
`Diago.Invite` has no hook between dialog creation and offer generation, so
when SDPMode is set the harness replicates its NewDialog → Invite → Ack
sequence and sets `MediaSession().Mode` in between (`MediaSession()` is an
exported accessor; the offer body is generated from the session inside
`dialog.Invite`). diago's `negotiateMediaDirection` keeps local mode
sendonly whatever the far end answers, which the plumbing guard leans on.
Re-INVITE offers reuse `call.LocalSDP()` (fresh o= version each call — diago
increments sessionVersion per LocalSDP generation) with the direction
attribute rewritten by the test-file helper `setSDPDirection`.

**Live result (jambonz.me, 2026-08-12): all 3 RED — bug reproduced.** The
answer's o= line says "Jambonz Media Server" (mediajam), and it answers
`a=sendrecv` to sendonly offers on the initial INVITE AND on re-INVITEs.
Re-INVITEs return 200 and the dialog survives (no 488), so the freeswitch
hard-breakage isn't visible — but the wrong complement is precisely the
Five9 loop trigger. Either the mediajam fix isn't deployed to jambonz.me,
or it fixed endpoint-update robustness without fixing the answer's
direction complement. Re-run after deploying the fixed build:
`make test-drachtio RUN=TestDrachtio_Sendonly`.

**Files:** new `tests/drachtio/sdp_direction_test.go`; modified
`internal/sip/uac.go` (SDPMode), `HANDOFF.md`.

### 2026-08-07 — call-leak audit: guaranteed dialog teardown

**Symptom:** after a full suite run (verbs + `-tags drachtio`), jambonz kept
live calls around. Root cause was teardown-by-happy-path: hangups sat at the
tail of test bodies, so every `s.Fatalf` / watchdog-timeout path skipped them,
and `sip.Stack.Stop()` deregistered + closed the UA with no knowledge of live
dialogs — so a BYE we owed never went out, and jambonz's inbound BYE arrived at
a closed UA (no 200).

**Fix — one choke point plus a REST backstop:**

- `internal/sip/drain.go` (new): `drainCalls(calls, perCall, total)` hangs every
  call up concurrently, waits for terminal state, and is bounded by an overall
  deadline (the hangup itself included — `Call.Hangup` has its own 10s ctx and
  would otherwise blow the budget). Panic-safe, positional results.
  `internal/sip/drain_test.go` pins the contract (12 tests, race-clean).
- `internal/sip/uas.go` / `uac.go`: `Stack` now tracks every `*Call` born on it
  (`track()` in both `Invite` and `dispatchInbound`); `Stop()` drains live calls
  **before** the deregister → cancel → `ua.Close()` sequence, and `slog.Warn`s
  any that didn't end cleanly. Since every test gets its stack from `claimUAS`,
  whose `t.Cleanup` calls `Stop()`, teardown is now guaranteed on *every* exit
  path with zero per-test wiring. Also deleted a dead ctx-watcher goroutine.
- `tests/verbs/helpers_test.go`: `claimUAS`'s inbound handler selects on the
  stack ctx as well as `call.Done()`, so `dispatchInbound`'s safety-net hangup
  becomes reachable for abandoned legs; `placeWebhookCallToNoWait` now registers
  a `t.Cleanup` DELETE (hand-rolled rather than `provision.ManagedCall`, which
  raises a raw `t.Fatalf` that would bypass the FAILURE SUMMARY).
- `tests/verbs/builtin_hangup_test.go` (`inviteInbound`) and `answer_test.go`:
  `t.Cleanup(call.Hangup)` registered immediately after `Invite`, before the
  first assertion that can `Fatalf`. `inviteInbound`'s app deliberately has no
  trailing hangup verb, so nothing else would ever have ended those calls.
- `tests/drachtio/main_test.go`: suite `app_json` gains a trailing
  `{"verb":"hangup"}` so an abandoned dialog dies at pause end. The 150s pause
  stays (session-timer tests need `sessionInterval + 30` = 120s).
- `internal/provision/calls.go` + `suite.go`: `ListLiveCalls()` (GET
  `/Accounts/{sid}/Calls`, contract-validated against the new
  `schemas/rest/calls/listCalls.response.200.json`), `CallSummary.IsLive()`,
  `HangupCall()`, and a sweep at the head of `SuiteAccount.Teardown`.
  `internal/provision/calls_list_test.go` covers it (8 tests, httptest).

**⚠ The most important thing learned this session — `DeleteCall` does not hang
up a call.** `DELETE /Accounts/{sid}/Calls/{callSid}`
(`api-server/lib/routes/api/accounts.js:1052`) resolves to
`@jambonz/realtimedb-helpers/lib/delete-call.js`, which is only
`redis.multi().del(key).zrem(CALL_SET, key)`. It never signals the
feature-server. It returns 204 while the SIP dialog stays up — and it deletes
the Redis record that made the leak discoverable, so it makes a leak *invisible*
rather than fixing it. The only REST path that actually ends a call is
`POST /Accounts/{sid}/Calls/{callSid}` with `{"call_status":"completed"}` →
`${call.serviceUrl}/v1/updateCall/{sid}` → feature-server `_lccCallStatus` →
`_jambonzHangup()`, acked 202. That is now wrapped as `Client.HangupCall`, and
every cleanup path calls it *before* `DeleteCall`. `provision.ManagedCall`'s doc
("hangs it up if still active") has been wrong since it was written.

Related: `GET /Accounts/{sid}/Calls` is **not** filtered to live calls.
`list-calls.js` filters by `callStatus` only when the caller passes that query
param, and `update-call-status.js` leaves completed calls in `CALL_SET` for
`MAX_CALL_LIFETIME_AFTER_COMPLETED` (default 3600s). A whole run's completed
calls come back — hence `CallSummary.IsLive()`, which callers MUST apply.

**Also fixed (found in review):**

- `Call.Hangup()` ran `stopMedia()` *before* the BYE, and `stopMedia` waited
  unbounded on the audio pump — which blocks in an RTP read with **no read
  deadline**. A far end that stopped sending RTP (exactly the stalled-call case)
  wedged `Hangup` forever and the BYE never went out. Now bounded by
  `recordingStopGrace` (2s), after which the recording is abandoned (file
  deliberately NOT closed — the pump goroutine is still writing to it). The
  order was left alone on purpose: BYE-first would surface a "use of closed
  connection" error into `Call.Err()` and change what tests observe.
- The `hctx` fix was initially applied to only one of four inbound handlers.
  `tests/verbs/dial_srtp_test.go`, `dial_nat_tls_ack_test.go` and
  `cmd/probe/main.go` had the same unconditional `<-call.Done()`.
- Both probe tests registered `t.Cleanup(probe.Stop)` *before*
  `t.Cleanup(closeTun)`, so LIFO tore the ngrok tunnel down first and the
  drain's BYE had no transport to travel on. A second (idempotent) `probe.Stop`
  registration after `closeTun` fixes the order.
- `tests/verbs/lcc_transfer_test.go:50` was the only asserting goroutine in
  `tests/verbs` with no `t.Cleanup` join; on an early Goexit the drain could
  race it into a spurious `[target:answer] FAILED` summary line.
- The drachtio trailing `hangup` verb turns out to be **inert** — the
  feature-server's `_clearResources` already BYEs at end-of-application. Kept
  for explicitness; the comment now says so rather than claiming it bounds
  anything.

**Verified:** `go build ./...`, `go vet ./...`, `go test -race ./internal/...`
all green; `tests/verbs` and `tests/drachtio` compile. Two adversarial review
passes; verdict **assertion-neutral** — no passing test's meaning changed.
**Not** run against the live cluster — next full run should show zero live calls
at teardown, with any straggler surfacing as a
`sip: call did not end cleanly at stack stop` warning.

**Known residuals:** `tport_reconnect_test.go`'s post-reconnect deregister 403s
(acknowledged in-code), so that registration lingers until the 300s expiry —
registrations, not calls. Pre-existing and untouched: the `WithTimeout` watchdog
can `t.Errorf` after the test completes (`helpers_test.go:1599`, `Timer.Stop()`
doesn't wait for a firing callback), and `claimUAS2` deadlocks if `claimUAS`
Goexits in the child goroutine (`helpers_test.go:159`).

### 2026-07-14 — onHoldHook + transfer-kill coverage (feature-server transfer fixes)

**Scope:** the feature-server gained (a) onHoldHook execution on warm/parked
transfers (previously schema-only; parked callers heard silence), (b)
spread-based handoff→transfer field forwarding (headers/callerName/
anchorMedia/referredBy were silently dropped), and (c) a kill-gap fix
(TaskTransfer.kill now aborts a running warm strategy via _abortStrategy —
previously a LCC redirect mid-transfer left the hold loop + ringing human leg
alive until ring timeout). Three test changes pin these; all compile + vet
clean but are UNRUN — `jambonz.me` was unreachable at session end (likely
being redeployed with the new build). Run the transfer/handoff/LCC suites
when it's back.

- **`TestVerb_Transfer_WarmParked_OnHoldHook`** (`transfer_test.go`, new) —
  warm/parked transfer with `onHoldHook` served by `/action/onhold` returning
  a say; target delays answer 4s so the announcement has a window. Asserts
  hook invoked with `event_type=="transfer.on-hold"`, caller transcript
  contains "transfer" (hold say reached the parked caller), and bridged/
  completed.
- **`TestVerb_Agent_OpenAI_Handoff_WarmCallerID`** (extended) — handoff block
  now also carries `onHoldHook` + `headers:{X-Handoff-Test}`; new steps
  assert the custom header arrives on the target INVITE (headers forwarding)
  and `/action/onhold` was invoked (onHoldHook forwarding). Hook-invocation
  only — audible-hold audio is the transfer test's job.
- **`TestLCC_Redirect_DuringWarmTransfer`** (`lcc_transfer_test.go`, new) —
  kill-gap regression: warm/parked transfer (timeout 30) ringing a
  never-answering target with the hold loop live; re-script the call hook and
  inject `updateCall{call_hook}` → assert the caller hears the replacement
  say and the call ends <20s after updateCall (pre-fix: stalled the full 30s
  ring timeout), and the target leg is CANCELed without a 200.

### 2026-07-13 — Warm-transfer caller-ID regression coverage

**Scope:** a live call on `eu.jambonz.io` exposed a feature-server bug: the
`transfer` verb drops the configured `callerId` (task never copies
`data.callerId`) AND its fallback reads the non-existent
`cs.callInfo.callingNumber` (real property is `.from`), so the warm-transfer
outdial INVITE goes out with an empty From user. sbc-outbound substitutes
"anonymous" and PSTN carriers reject the leg (Twilio 403 / error 32204).
All three tests were verified red against the buggy build, then the 2-line
feature-server fix (copy `data.callerId` in transfer.js; fallback
`cs.callInfo.from` in warm-common.js) was deployed to `jambonz.me` and the
full transfer + handoff suites now PASS live.

- **`TestVerb_Transfer_WarmCallerID`** (`tests/verbs/transfer_test.go`, new)
  — warm/parked transfer with explicit `callerId`; asserts the target UAS's
  received INVITE From contains the configured number (and not "anonymous"),
  plus the usual bridged/completed actionHook. No audio/STT — caller-ID
  contract only.
- **`TestVerb_Transfer_WarmParked`** — added step
  `assert-target-caller-id-fallback`: with no `callerId` configured, the
  target INVITE From must fall back to the parent caller's number
  (`441514533212`, the REST-created leg's From), never anonymous/empty.
- **`TestVerb_Agent_OpenAI_Handoff_WarmCallerID`**
  (`tests/verbs/handoff_test.go`, new) — same contract through the agent
  verb's Layer-1 handoff (the exact production path): warm handoff with
  `callerId` + `brief:"none"`, LLM force-calls `transfer_to_human`, asserts
  the human-leg INVITE From carries the callerId (not anonymous) and the
  agent actionHook reports `completion_reason=="transferred"`. Verified red
  live against `jambonz.me` (From arrives as anonymous; blind/dial handoff is
  unaffected — `blind.js` reads `data.callerId` directly, warm path doesn't).

### 2026-07-05 — Tool-calling depth: agent argument extraction + first llm-verb toolHook test

**Scope:** the existing `TestVerb_Agent_ToolHook` only proved a
PARAMETERLESS tool call round-trip; it didn't prove the LLM extracts
real argument values from user speech. Separately, the `llm` verb
(row 5.1) had zero tool-calling coverage. Close both gaps.

**Done — 2 new tests, both PASS live against `jambonz.me`:**

- **`TestVerb_Agent_ToolHook_Arguments`** (`tests/verbs/agent_test.go`)
  — a `get_weather(location)` tool. Proves the LLM populates
  `arguments.location` from real user speech ("What is the weather in
  Chicago?" → `arguments.location=="Chicago"`), corroborated by
  `turn_end.tool_calls` naming `get_weather` and the agent speaking the
  returned tool result. Extends `TestVerb_Agent_ToolHook` (parameterless
  tool) to prove argument extraction — tool-calling working together
  with the verb's NL-understanding feature.
- **`TestVerb_LLM_Deepgram_ToolHook`** (`tests/verbs/llm_test.go`) —
  first tool-calling coverage for the `llm` verb (Deepgram Voice
  Agent). Declares `get_weather` under `Settings.agent.think.functions`,
  wires a verb-level `toolHook`. Proves the Voice Agent emits a
  function call (`args.location=="Chicago"` — the `llm` verb uses field
  `args`, NOT `arguments`) and that a `FunctionCallResponse` envelope
  returned via a dynamic per-request responder round-trips to a spoken
  reply.

**New infra:**

- **`webhook.Session.ScriptActionHookBodyFunc`** (`internal/webhook`) —
  per-request dynamic response body. Needed because the `llm` verb's
  tool result must echo the live `tool_call_id` back in the vendor
  envelope; a static body (the existing `ScriptActionHookBody`) can't
  do that.
- **New local request schemas**: `schemas/callbacks/agent-tool.schema.json`
  (requires `arguments`) and `schemas/callbacks/llm-tool.schema.json`
  (requires `args`), both marked `$comment: TODO: upstream`.

**Drift found:** the `agent` verb's toolHook payload uses field
`arguments`; the `llm` verb's toolHook payload uses field `args` for
the same concept (function-call parameters). Candidate for an upstream
doc/schema clarification — noted on coverage-matrix rows 5.0/5.1.

**Still out of scope (not touched by this change):** `mcpServers`
discovery, the WS `sendToolOutput` path, `toolFiller`, and
multi-tool/multi-round tool chains.

**Files touched:**
- `tests/verbs/agent_test.go` — `TestVerb_Agent_ToolHook_Arguments`
- `tests/verbs/llm_test.go` — `TestVerb_LLM_Deepgram_ToolHook`
- `internal/webhook/registry.go` — `ScriptActionHookBodyFunc`
- `schemas/callbacks/agent-tool.schema.json`, `schemas/callbacks/llm-tool.schema.json` — new
- `docs/coverage-matrix.md` — rows 5.0, 5.1
- `HANDOFF.md` — this entry

---

### 2026-07-03 — Per-leg call recording as playable WAV (ADR-0016)

**Scope:** user wants to *hear back* each leg of each call after a test
run when developing/debugging, controlled by an env var, organised so
the test + leg are obvious from the path. WAV over MP3 (already-decoded
LPCM + 44-byte RIFF header = zero CPU, zero deps; MP3 needs an encoder
for an irrelevant size win at 8 kHz mono).

**Done:**
- `RECORD_LEGS=true` (+ optional `RECORD_DIR`, default `recordings/`
  at repo root) archives every recorded leg as
  `recordings/<test-name>/<leg>.wav`. Off by default; release gate
  unaffected. `.env.example` documents it; `recordings/` gitignored.
- **Zero per-test wiring** (v1 used an explicit
  `Call.SetArchiveMeta(test, role)` at all 13 StartRecording sites;
  user pushed back — rightly — and it was replaced): test name inherits
  from `sip.Config.Owner` (stamped once in each `claimUAS` with
  `t.Name()`; every Call born on the per-test stack carries it), leg
  name derives from the recording file's basename
  (`dial-caller.pcm` → `dial-caller.wav`). Rejected alternatives are
  ADR-0016 options A–E.
- New `internal/recording` package (Archiver: stable paths, wipe test
  dir on first archive of a run, collision suffixes, RIFF wrap;
  5 unit tests). Wired via package-level `sip.SetArchiveHook` in verbs
  TestMain — `internal/sip` imports neither config nor recording.
- **Latent bug found & fixed:** on remote BYE nothing ever closed the
  recording file (`stopMedia` was only called from `Hangup()`), so
  recordings were never flushed/finalized for peer-ended calls.
  `setState(StateEnded)` now fires `go stopMedia()`.
- Relative `RECORD_DIR` anchored at repo root via go.mod walk-up
  (`go test` sets CWD to the package dir — first e2e run scattered
  recordings under `tests/verbs/`).

**Verified:** unit tests green (`internal/recording`, `-race` incl.
sip); e2e single tests archive on the remote-BYE path; full verbs suite
with the flag produced 36 valid WAVs (`afinfo`: 8 kHz mono PCM) across
say/play/dial/conference/enqueue/transfer/llm/listen/dub/dtmf/agent.
Full-suite run had 11 agent-family failures — **exonerated**: identical
subset passes with flag on (44s) and off (43s); failures are
LLM-under-parallel-load flakiness on this wip branch (see below), not
the recording change.

**Watch out:** full `-parallel 8` suite currently flakes on agent-family
tests (Deepseek/OpenAI latency + the new handoff/OpenAI tests on
`feat/handoff-test-finalize`) — failures like "SendWAV invalid in state
ended", greeting 0 PCM, action-hook deadline, and 480s on
`sip:app-...` INVITEs. Pre-existing; investigate separately before
using the full parallel run as a release gate.

**Files:** new `internal/recording/{archive.go,archive_test.go}`,
`docs/adr/0016-per-leg-call-recording.md`; modified
`internal/config/config.go` (RecordLegs/RecordDir + findRepoRoot),
`internal/sip/{call,uas,uac}.go` (Owner, ArchiveHook, finalize,
stopMedia-on-ended), `tests/verbs/{helpers,verbsmain}_test.go`,
`tests/drachtio/helpers_test.go` (Owner stamp), `.env.example`,
`.gitignore`, `docs/adr/README.md`.

---

### 2026-06-30 — `listen` verb `mark` feature (bidirectional playout sync)

**Scope:** implement tests for the `mark` feature of the `listen` verb
(https://docs.jambonz.org/verbs/verbs/listen#mark).

**Investigation:** the docs are thin; the authoritative protocol was
read from `mod_audio_fork/lws_glue.cpp`. Key finding: `mark`/`clearMarks`
and the `playout`/`cleared` events are **not** in feature-server JS at
all (`listen.js` only wires `playAudio`/`killAudio`). They operate on
mod_audio_fork's *streaming* playout buffer, which only exists when
`bidirectionalAudio.streaming: true` and the WS server sends raw audio
as **binary** frames (not the `playAudio` JSON-with-base64 path, which
goes through `ep.play()` and bypasses the buffer/marks entirely).

**Done — 2 tests (`tests/verbs/listen_mark_test.go`):**

- **`TestVerb_Listen_Mark_Playout`** — listen with
  `bidirectionalAudio{enabled,streaming,sampleRate:8000}`; after WS
  connect, stream paced linear16 PCM binary frames, send
  `{type:mark,name}`, flush a trailing burst. Asserts the returned
  mark has `event:"playout"` AND the caller-leg recording is real
  audio (rms≈17700, 6.26s) — proving the marked audio actually reached
  the caller.
- **`TestVerb_Listen_Mark_Cleared`** — bursts a long (~12s) audio block
  unpaced, sends `{type:mark,name}` + one frame, then `{type:killAudio}`
  immediately. Asserts the returned mark has `event:"cleared"`. The
  burst keeps the marked position deep in the undrained buffer so the
  kill wins the race; an early version that paced the stream got
  `playout` because the audio drained before the kill arrived.

**New infra:**
- `webhook.Session.WSConnected(ctx)` — block until jambonz dials our
  `/ws/<id>` (bidi sends race call setup otherwise).
- `webhook.Session.WaitWSMark(ctx, name)` + `webhook.WSMark{Name,Event}`
  + `parseMark` — drain text frames for a matching `{type:mark}`.
- Test helpers `marksRawPCM` (EnsureWAV → strip 44-byte RIFF header),
  `marksStreamPCM(sess, pcm, paced)`, `marksMarkMsg`,
  `marksKillAudioMsg`, `marksAnswerRecord`.
- Distinct playback text per test so `EnsureWAV` cache keys differ
  (parallel tests sharing one cache file caused a 0-byte partial-read
  race in the first run).
- `.gitignore` allowlist `tests/verbs/testdata/listen/*.wav`.

**Surprises:**
- First run: the two tests shared `markPlayAudioText` → same EnsureWAV
  cache path → parallel read-during-write yielded a 0-byte WAV. Fixed
  by giving each test distinct text (distinct sha1 cache key).
- First run cleared test got `playout` not `cleared`: paced streaming
  let the marked audio drain to the caller before `killAudio` landed.
  Fixed by bursting a long undrained block before the kill.
- `clearMarks` and `killAudio` both produce `event:"cleared"`; this
  test exercises the `killAudio` path (also clears the buffer).
  `clearMarks` (no buffer flush) is left for a future depth pass.

**Verified runs:** mark pair ~15s parallel, 3 consecutive green; full
`TestVerb_(Listen|Stream)` suite ~15s green (no regression).

**Files touched:**
- new: `tests/verbs/listen_mark_test.go`,
  `tests/verbs/testdata/listen/{d252370b21acbcb8,f5f010e7f47f7f40}.wav`
- `internal/webhook/ws.go` — `WSConnected`, `WaitWSMark`, `WSMark`,
  `parseMark`
- `.gitignore`, `docs/coverage-matrix.md` (row 4.2)

---

### 2026-05-01 — Self-provisioning ephemeral suite accounts + `dial` bridge fix + ergonomics overhaul

**Scope:** the harness used to depend on a long-lived account-scope
identity (`JAMBONZ_API_KEY` + `JAMBONZ_ACCOUNT_SID`) that pointed at
a real account on the cluster. User asked to remove that dependency
and have every test run create + tear down its own account under the
SP — so the harness can be pointed at any cluster where you have an
SP token, and never touches anything else under that SP.

This session also fixed the long-standing `TestVerb_Dial_User_Bridge`
flake and absorbed all eight "Top wins" + most "Smaller polish" items
from the DX audit (general agent review).

**Done — self-provisioning model:**

- **`internal/provision/suite.go`** — new `SetupSuiteAccount(...)`
  helper used by both TestMain implementations:
  1. SP creates account `it-<runID>-{verbs,rest}` under the configured SP
  2. SP mints account-scope ApiKey via POST /ApiKeys
  3. account-scope POST /Accounts/<sid>/SipRealms/<synthetic-realm>
     where realm is `<account-name>.smoke.test` — verified via
     follow-up GetAccount because the cluster's DNS provider returns
     500 even after the DB UPDATE has committed (so we tolerate 500
     in `provision.SetSipRealm` and assert correctness via GET)
  4. returns a `*SuiteAccount` with `{AccountSID, AccountName,
     APIKeySID, Token, SIPRealm, AccountClient (account-scope), SPClient}`
  5. `Teardown()` deletes the suite account's clients first
     (upstream FK constraint workaround) then the account itself
- **`internal/sip/resolver.go`** — new `StaticResolver` runs an
  in-process UDP DNS server on 127.0.0.1, answers A queries for the
  synthetic realm with the SBC public IP, forwards everything else to
  the system resolver. Hooked into sipgo via
  `Stack.Config.Resolver` → `WithUserAgentDNSResolver`. Without this,
  sipgo's transport layer NXDOMAINs on the synthetic realm and
  REGISTER never fires.
- **`internal/provision/accounts.go`** gained `SetSipRealm` —
  POST `/Accounts/<sid>/SipRealms/<realm>`. Tolerates 500 from
  cluster's broken DNS provider integration since the DB UPDATE
  always succeeds before the DNS hop.
- **`internal/provision/sweeper_accounts.go`** rewritten with
  hardened safety rules (see "Critical safety rule" section above).
  Three other sweepers (Application, VoipCarrier, Lcr, SIPClient)
  deleted — sub-resources cascade when their parent suite account is
  deleted, so per-resource sweepers are unnecessary and risky.
- **`internal/config/config.go`** — removed `APIKey`, `AccountSID`,
  `SIPDomain`, `SIPProxy`, `BehindNAT`, `PublicIP`. Added required
  `SBCPublicIP`, optional `SIPRealmZone` (defaults to `smoke.test`).
  `JAMBONZ_SP_API_KEY`, `JAMBONZ_SP_SID`, `JAMBONZ_SBC_PUBLIC_IP`,
  `NGROK_AUTHTOKEN`, `DEEPGRAM_API_KEY`, `DEEPSEEK_API_KEY` are now
  all required.
- **`tests/{verbs,rest}/<*>main_test.go`** — TestMain rewrites that
  call `SetupSuiteAccount`, install the resolver, provision Deepgram
  + webhook Application under the new account, run the suite, tear
  the account down. Per-test code references `suite.SIPRealm` /
  `suite.AccountSID` instead of `cfg.SIPDomain` / `cfg.AccountSID`.
- **`cmd/probe/main.go`** — standalone Go program that exercises the
  whole stand-up sequence end-to-end (account → ApiKey → SipRealm →
  Client → REGISTER → POST /Calls → INVITE → Answer → Hangup) for
  one-shot debugging. Run via `go run ./cmd/probe`. Useful when the
  refactor hits a new cluster and you want to isolate where it
  rejects.

**Done — `dial` bridge fix:**

- **Root cause:** by default jambonz brokers a peer-to-peer SDP
  exchange between the two legs of a `dial` verb. Each leg ends up
  with the OTHER leg's NAT'd RTP address (10.x.x.x in EC2 internal
  networking). Neither side can reach those — the bridge "completes"
  SIP-wise (`dial_call_status: completed, dial_sip_status: 200`) but
  no audio crosses. The caller's recording was 1.74s of silence
  (`rms=0.0`).
- **Fix:** add `anchorMedia: true` to the dial verb. Forces
  FreeSWITCH to relay every RTP packet through the cluster's data
  plane (which we can reach via the SBC public IP). After the fix:
  caller records 5.46s of audio at `rms=21160` and Deepgram
  transcribes "the sun is shining" cleanly.

**Done — ergonomics (8 helpers + payload accessors):**

- `claimSession`, `SessionURL`, `SessionAckEmpty`, `RunAudioRoundtrip`,
  `WaitFor`, `HangupAndWaitEnded`, `ScriptAgent`, `provisionWebhookApp`,
  `helperFatalf` — all in `tests/verbs/helpers_test.go`.
- `webhook.Callback.{String,Int,Bool,NestedString,NestedAny,CustomerData}`
  payload accessors in `internal/webhook/types.go`.
- `internal/sip/Call.MethodsReceived()` shortcut.
- Named timing constants (`RecognizerArmDelay`, `LLMReplyWindow`, etc).
- Migrated 16 verb test files + agent test (9 sub-functions), every
  webhook URL site, every audio-roundtrip site. Suite per-test
  bodies are visibly shorter and uniform.

**Surprises:**

- **The `GET /Clients` endpoint ignores its `account_sid` query
  parameter** — confirmed by curl. Earlier in the session a
  client-side delete loop (filtered by `?account_sid=...`) actually
  iterated **every client visible to the SP token**, deleting 12
  clients across 2 unrelated accounts. Restored by the user from
  another session; we now treat /Clients as cross-account by default
  and filter exclusively client-side via `ListSIPClientsForAccount`.
  The `AccountSweeper` re-checks `name` prefix per-record before
  every DELETE. Documented in the new "Critical safety rule"
  section.
- **The cluster's POST /SipRealms returns HTTP 500** when its DNS
  provider is half-configured (`DME_API_KEY` set but the integration
  is broken — `createDnsRecords is not a function`). The upstream
  handler runs the DB UPDATE BEFORE the DNS hop, so the realm is
  persisted regardless. `SetSipRealm` swallows the 500; the caller
  verifies via GET.
- **`anchorMedia` was the obvious-in-hindsight fix** for dial — the
  test had been flagged as a known issue for ~10 days. Without
  anchored media the dial test was passing only when the cluster
  happened to advertise public IPs in SDP, which was nondeterministic.
- **Probe binary was the right move.** Building `cmd/probe` to
  validate the whole synthetic-realm pipeline before refactoring
  TestMain saved hours of "is it the resolver, the realm, the
  cluster, or the test?" debugging during the migration.

**Verified runs:**

| Suite | Wall-clock | Tests | Status |
|---|---|---|---|
| `tests/rest` | ~27s | 23 | all pass |
| `tests/verbs` | ~85s | 47 | all pass (incl. dial) |
| Combined | ~112s | 70 | all pass |

The `it-*` accounts seen on the cluster after a successful run are
zero — verified by curl. The two persistent accounts on the cluster
(`default account`, `ic-test-account`) are never touched.

**Files touched (notable):**
- new: `internal/sip/resolver.go`, `internal/provision/suite.go`,
  `cmd/probe/main.go`
- removed: `internal/provision/sweeper_{applications,lcrs,sip_clients,voip_carriers}.go`
- rewritten: `internal/config/config.go`, `tests/{verbs,rest}/*main_test.go`,
  `internal/provision/sweeper_accounts.go`,
  `internal/provision/sip_clients.go` (added `ListSIPClientsForAccount`,
  switched `IsActive` to `IntField`)
- migrated: 16 verb test files, 11 rest test files

---

### 2026-05-01 — Tier 5 `agent` verb: full coverage, self-hosted, contract-validated

**Scope:** stand up the `agent` verb test surface using only the
smoke-tester's existing webhook + ngrok infrastructure (no external deploy
of `jambonz-test-agent` to EC2). Use Deepseek as the LLM (user has the
key; OpenAI key not available) and the in-jambonz Deepgram credential we
already provision at TestMain for STT + TTS. End-to-end audio round-trips
verified by re-uploading recordings to Deepgram.

**What landed (11 tests, all PASS parallel ~28s):**

- **`TestVerb_Agent_Echo`** — round-trip STT → Deepseek → TTS → STT,
  asserts ≥3/4 keywords ("alpha bravo charlie delta") survive the loop.
- **`TestVerb_Agent_EventHook`** — drains the per-test session for the
  3 turn-level events (`user_transcript`, `llm_response`, `turn_end`).
  Asserts each fires at least once with a content-bearing payload.
  `turn_end` validated against `schemas/callbacks/agent-turn.schema.json`
  (this is where most schema drift surfaced — see "Drift" below).
- **`TestVerb_Agent_Greeting`** — `greeting: true` ⇒ agent emits TTS
  before the user speaks. Asserts ~96 KB inbound PCM in the first 6s
  (greeting window) before any user audio is sent.
- **`TestVerb_Agent_ActionHookOnEnd`** — actionHook fires on agent.kill
  (call BYE → call-session teardown → `notifyTaskDone` →
  `performAction`). Asserts payload has `call_sid`, `completion_reason`,
  and that `customerData.x_test_id` round-trips back to us — proving
  correlation works for actionHook (unlike eventHook).
- **`TestVerb_Agent_ToolHook`** — declares `get_secret_word` in
  `llmOptions.tools`, system prompt forces Deepseek to call it, server
  replies with raw JSON body `{"word":"kingfisher"}`. Asserts callback
  payload has `tool_call_id` + `name` + `arguments`, and that the LLM
  speaks "kingfisher" in the second turn (verified offline via Deepgram).
  Required new `webhook.Session.ScriptActionHookBody` to return a raw
  JSON body instead of a verb array (toolHook expects an object, not a
  verb sequence).
- **`TestVerb_Agent_BargeIn`** — `greeting: true, bargeIn: true`. Sends
  user WAV ~3s into the agent's greeting and asserts the eventHook
  emits `user_interruption`.
- **`TestVerb_Agent_NoResponseTimeout`** — sets `noResponseTimeout: 4`,
  stays silent through one greeting + one re-prompt window, asserts ≥2
  `llm_response` events landed in the eventHook stream. Re-prompt fired:
  *"Are you still there, or is there something I can help you with?"*
- **`TestVerb_Agent_KrispTurnDetection`** — agent runs with
  `turnDetection: "krisp"`. Confirms the verb param is accepted and
  there's inbound RTP. Krisp is internal to jambonz (mod_krisp) — no
  client-side handle — so the test scope is "did the verb run", not
  "did Krisp emit EOT".
- **`TestVerb_Agent_NoiseIsolation`** — exercises the
  `{mode, level, direction}` object form (the most expressive shape; if
  it parses, the shorthand strings do too — same validator). Same
  "param accepted, RTP flowed" smoke level as Krisp: noiseIsolation is
  media-server-internal (FreeSWITCH mod_krisp; mediajam's own path) with
  no client-side handle, so we don't assert the LLM echoed the prompt —
  on a pass-through media server (no Krisp) that flakes under load
  without signalling a defect. Runs on every cluster (no skip);
  audio-path round-trip is covered by Agent_Echo.

**Self-hosted architecture (no external deploy needed):**

```
test → POST /Calls (application_sid=webhookApp, tag.x_test_id=<testID>)
     ↓
jambonz fetches verbs from ngrok tunnel /hook
     ↓
webhook server returns [answer, pause, agent {stt:dg, tts:dg,
                                              llm: {vendor:deepseek,
                                                    auth:{apiKey:cfg.DeepseekAPIKey},
                                                    ...},
                                              eventHook, actionHook,
                                              toolHook}]
     ↓
agent runs → eventHook/toolHook callbacks back to ngrok
     ↓
test asserts on captured callbacks + recorded reply audio
```

`agent.llm.auth.apiKey` inline → feature-server skips DB credential
lookup (`lib/tasks/agent/index.js:446`), so no `/LlmCredentials`
provisioning is needed.

**Per-test routing for hooks without customerData:**

eventHook + toolHook payloads don't carry our `tag.x_test_id`
correlation key (feature-server's `_sendEventHook` / agent tool-call
path only forward `{type, ...}` / `{tool_call_id, name, arguments}` —
not `callInfo`). Without intervention every event from every parallel
agent test would land in the shared `_anon` session and races would
make `WaitCallbackFor` non-deterministic.

Workaround: append `?X-Test-Id=<testID>` to the eventHook + toolHook
URLs the test gives jambonz. The webhook server's `extractTestID`
(internal/webhook/correlation.go) already resolves the test ID from
URL query, so the callback routes to the per-test session. No
`_anon` contention. Verified by running all 11 tests parallel
without flakes across multiple runs.

**Drift findings (filed as TODO upstream, applied to local schema):**

`schemas/callbacks/agent-turn.schema.json` `turn_end` variant:
- Upstream declares `latency.{transcriber_latency,
  turn_detection_latency, model_latency, voice_latency, preflight}`
  with `additionalProperties: false`.
- Feature-server emits `latency.{stt_ms, eot_ms, llm_ms, tts_ms,
  tool_ms}` instead. Local schema now accepts both.
- Feature-server also adds `turn_end.confidence` (STT recognizer
  confidence) and `turn_end.tool_calls` (array of tool-call summaries)
  — neither declared upstream. Local schema accepts both.
All marked `DRIFT (TODO upstream)` in the schema descriptions for a
future schema-repo PR.

**New infra:**

- **`internal/tts/deepgram.go`** — `EnsureWAV(ctx, dir, text, opts)`
  hits Deepgram's `/v1/speak` REST API (8 kHz / mono / linear16),
  wraps the raw LPCM in a RIFF/WAVE header, caches by sha1(model|text)
  under `tests/verbs/testdata/agent/<sha>.wav`. Re-runs are free; the
  WAVs are checked in (`.gitignore` allowlists
  `tests/verbs/testdata/agent/*.wav`).
- **`webhook.Session.ScriptActionHookBody(verbName, body)`** — raw
  JSON body responder. Needed because toolHook expects an object
  result, not a verb array — `ScriptActionHook(...)` JSON-encodes the
  Verbs slice as `[]` by default. New helper sets `HookOutcome.Body`
  directly with `Status: 200` so `writeOutcome` short-circuits the
  verb-array path.
- **`agentVerbOpts` builder** in `agent_test.go` — centralised
  parameterised verb construction (system prompt, hook URLs,
  greeting/bargeIn/turnDetection/noiseIsolation/etc.), so every test
  expresses only the diff from the default echo configuration.
- **`AssertTranscriptHasMost(s, ctx, recording, minHits, wants...)`** —
  relaxed sibling of `AssertTranscriptContains`. Tolerates LLM word
  drops/substitutions; asserts ≥minHits keywords landed.

**Surprises:**

- Deepseek frequently dropped one word (e.g. "delta" → "dulsett") on
  the STT round-trip. Echo test uses `AssertTranscriptHasMost(... 3, ...)`
  to tolerate it; otherwise we'd have a flaky test for a non-bug.
- `greeting: false` is essential when you want the user to speak first.
  Without it the agent emits "Begin the conversation." before our WAV
  arrives and the recording becomes a two-turn jumble. Documented in
  the agentVerbOpts default and used in 8/11 tests.
- toolHook callback initially landed in `_anon` and our per-test
  `ScriptActionHookBody("agent-tool", ...)` was ignored — the server
  routed to `_anon.outcomeForActionHook("agent-tool")` which had no
  registered script and replied `[]`. Net effect: LLM got an empty
  string back from the tool and started saying "the secret word is
  empty". Fixed by routing the callback to the per-test session via
  `?X-Test-Id=<testID>`; once the routing was correct, the test went
  green on the first re-run.
- Agent verb's `actionHook` only fires from
  `awaitTaskDone()` → `performAction()`, which only resolves at
  call-session teardown. No `noResponseTimeout` path or LLM-finish
  shortcut ends the agent task — call BYE is the only signal. Test
  hangs up proactively after a brief settle.

**Verified runs:**

| Mode | Wall-clock | Status |
|---|---|---|
| `-parallel 8`, agent suite alone (11 tests) | ~28s | all green |

No regressions in the rest of the verbs suite.

**Files touched:**
- `internal/config/config.go` — `DeepseekAPIKey` + `HasDeepseek()`
- `internal/tts/deepgram.go` — new
- `internal/webhook/registry.go` — `ScriptActionHookBody`
- `tests/verbs/agent_test.go` — new (11 tests)
- `tests/verbs/helpers_test.go` — `AssertTranscriptHasMost`
- `tests/verbs/ai_skips_test.go` — drop `TestVerb_Agent_Basic` skip-stub
- `tests/verbs/testdata/agent/b45558c6b28216eb.wav` — pre-gen prompt
- `schemas/callbacks/agent-turn.schema.json` — drift fixes
- `.env.example` — document `DEEPSEEK_API_KEY`
- `.gitignore` — allowlist `tests/verbs/testdata/agent/*.wav`
- `docs/coverage-matrix.md` — Tier 5 row 5.0 added

---

### 2026-04-25 — Failure-fast pattern + answer verb via UAC origination + response-code helpers

**Scope:** the user noticed that under `-parallel`, when a test fails the
output is hard to triage — `go test` without `-v` doesn't print the
failing assertion in a discoverable place, and even `-v` interleaves
output across concurrent tests. Make failure attribution instant. Also:
make `answer` verb actually testable (was a skip-stub), and give
REST+SIP tests a clean way to assert on response codes.

**Done — failure-fast harness:**

- **End-of-run `=== FAILURE SUMMARY ===` block** in both
  `tests/verbs/TestMain` and `tests/rest/TestMain`. Every `s.Errorf` /
  `s.Fatalf` / `s.Fatal` / watchdog timeout records `(testName, step,
  message)`; after `m.Run()` we print them all on stderr in a
  one-line-per-failure block. Operators see exactly which test, which
  step, why — without grepping through interleaved log noise.
- **`recordFailure(t, step, msg)`** is the entry point. Wired into
  `StepCtx.{Errorf,Fatalf,Fatal}` and `WithTimeout`'s watchdog. Mirror
  helpers exist in both packages (`tests/verbs/helpers_test.go` and
  `tests/rest/helpers_test.go`).
- **`GoroutineFailf(t, label, format, args)`** — for callee/listener
  goroutines that don't have a `*StepCtx` in scope. Replaced raw
  `t.Errorf("[callee:X] FAILED:")` patterns in `dial_test.go`. Setup
  helpers (`resolveFixture`, `mustSchemasRoot`) now call
  `recordFailure(...)` immediately before their `t.Fatalf`.
- **CLAUDE.md** has a new "Test-design rules (failure-fast pattern)"
  section documenting:
  - `Step(t, "...") + s.Errorf/Fatalf` over raw `t.Errorf/Fatalf`
  - `GoroutineFailf` for goroutine code
  - `recordFailure` for setup helpers
  - `provision.AsAPIError` / `provision.StatusOf` for REST status
    assertions; `Call.AnsweredStatus` / `Call.ReceivedByStatus` for SIP

**Done — `answer` verb test:**

- **`TestVerb_Answer_Basic` rewritten** to use UAC origination via
  `sip:app-<application_sid>@<domain>`. Same shape as `sip:decline`.
  Test provisions an Application whose call_hook returns
  `[answer, pause 1s, hangup]`, UAC INVITEs the auto-routed app URI,
  jambonz runs `answer` (explicit 200 OK) → pause → hangup (BYE).
  Asserts on `call.AnsweredStatus() == 200`, end reason
  `remote-bye`, BYE in `Received()`. **Was a skip-stub; now PASSES at
  ~4s wall-clock.**

**Done — response-code helpers:**

- **`provision.AsAPIError(err)`** + **`provision.StatusOf(err)`** in
  `internal/provision/client.go`. Tests can do
  `if provision.StatusOf(err) == 404 { ... }` or
  `apiErr, ok := provision.AsAPIError(err)` instead of the
  `errors.As(&apiErr)` ceremony.
- **`Call.AnsweredStatus() int`** + **`Call.ReceivedByStatus(code) []Message`**
  in `internal/sip/call.go`. UAC outbound calls capture the final 2xx
  on `call.recv` (added at `Stack.Invite` success path); UAS inbound
  calls already had `SentByStatus`. `AnsweredStatus` returns the
  answer code regardless of direction.
- **`Stack.Invite`** now records `dialog.InviteResponse` on the
  outbound `*Call` when the dialog establishes, so 200 OK shows up in
  `call.Received()` for assertion.

**Verified:** verbs suite still ~80-87s parallel, all green. Probe
test confirmed the summary fires:
`FAIL TestProbe_FailureSummary [step:intentional-fail] this is a deliberate failure: x=42`

**Surprises:**

- Go's `-parallel` output interleaving combined with stderr buffering
  means even with `-v`, scanning back to find a failure name + step is
  painful. The summary-after-`m.Run()` pattern is the only reliable
  fix; it's a tiny harness investment that pays off every flake.
- `dialog.InviteResponse` is exposed publicly on
  `diago.DialogClientSession` — easy capture, no fork needed.

---

### 2026-04-25 — Deepgram TTS + STT, Speech-credential provisioning, .env trimmed

**Scope:** stop relying on whatever speech credentials happen to be
provisioned on the test account out-of-band. Provision the Deepgram
credential ourselves at TestMain, label it, and have every verb test
reference that label so the pipeline is self-contained and reproducible.
While we're there, drop the now-dead `JAMBONZ_SIP_USER` /
`JAMBONZ_SIP_CALLEE_USER` env vars (claimUAS provisions dynamically).

**Done:**

- **`.env` slimmed** to what's actually used:
  - account scope (verb tests + /Clients + /SpeechCredentials)
  - SP scope (rest tests)
  - ngrok (Phase-2 + Phase-1 status callbacks)
  - Deepgram key (in-jambonz speech credential + offline transcript STT)
  Dropped: `JAMBONZ_SIP_USER`, `JAMBONZ_SIP_PASS`,
  `JAMBONZ_SIP_CALLEE_USER`, `JAMBONZ_SIP_CALLEE_PASS`. config.go's
  `SIPUser`/`SIPPass`/`SIPCalleeUser`/`SIPCalleePass` fields and
  `HasSIPUser`/`HasSIPCallee` helpers gone with them.
  `.env.example` rewritten to match.
- **`internal/provision/speech_credentials.go`**:
  `CreateAccountSpeechCredential`, `DeleteAccountSpeechCredential`,
  `ManagedAccountSpeechCredential`. Body shape: `{vendor, label,
  api_key, use_for_tts, use_for_stt}`. Per-resource schema
  `schemas/rest/speech_credentials/createSpeechCredential.response.201.json`
  wraps the shared `successful_add.json`.
- **TestMain (verbs) provisions a Deepgram credential** under the test
  account labelled `it-deepgram-<runID>`. Per-run label dodges the
  jambonz unique-(account,vendor,label) constraint when concurrent CI
  runs use the same account. Cleanup deletes the row at suite end. SID
  + label exposed via package-level `deepgramLabel`/`deepgramSID`/
  `deepgramVoice` (default `aura-asteria-en`). When
  `DEEPGRAM_API_KEY` is unset, provisioning is skipped and any test
  that references the empty label fails at jambonz-side credential
  lookup — by design, no silent fallback.
- **All verb tests use Deepgram via the label**. `placeCallTo` /
  webhook Application both set
  `speech_synthesis_vendor=deepgram, speech_synthesis_label=<runID
  label>` plus the recognizer counterpart. Inline overrides in
  `config_test.go` (synthesizer=aura-luna-en),
  `say_test.go::TestVerb_Say_SynthesizerOverride` (aura-orion-en), and
  `transcribe_test.go` (recognizer.vendor=deepgram + label) updated to
  match.
- **`provision.CallCreate` + `provision.ApplicationCreate`** gained
  `speech_synthesis_label` / `speech_recognizer_label` fields. Not
  documented in swagger, but feature-server reads them via the merged
  `{...application, ...req.body}` (verified in
  `feature-server/lib/middleware.js:425`).

**Verified:**

| Run | Wall-clock | Status |
|---|---|---|
| -parallel 8, run A | 73.9s | all green |
| -parallel 8, run B | 84.3s | all green |
| -parallel 8, run C | 88.2s | all green |

Deepgram STT actually faster than Google for our short test phrases —
the gather_speech callback fires at ~488ms (vs ~600-800ms with
Google), aura TTS lands at ~3.6s for "Hello from jambonz integration
tests" (Google was ~4s). Suite avg trends slightly faster.

**Surprises:**

- The Application's `speech_synthesis_label` field exists in
  feature-server's consumer code but isn't enumerated in api-server's
  swagger schema. Live API accepts it (no 400, no silent drop) — it
  flows from POST body through the DB row to feature-server. Treating
  this as upstream swagger drift; our local schema needs no change
  because we're additive on the request side. File a swagger PR when
  we surface a Tier-2 PUT for these fields.
- api-server's `validateCreateCall` (accounts.js) has an explicit
  `Object.assign` allowlist when `application_sid` is set — it
  propagates vendor/voice/language but NOT `*_label`. Verified harmless
  because feature-server ALSO does `{...application, ...req.body}` and
  fetches the full Application row by SID, so the labels still arrive.
  Worth noting if anyone tries to override label per-call on top of an
  application_sid call — that body field is dropped server-side.
- aura voices are Deepgram's only TTS option (no `language` family
  like Google's en-US-Standard-X). voice naming convention is
  `aura-<character>-<lang>`; we default to `aura-asteria-en` for
  feminine American English.

---

### 2026-04-25 — Parallelisation landed: 252s → 82s (3× faster, all green)

**Scope:** finish what the spike (earlier same day) opened. Land the
`claimUAS` helper, migrate every verb test to it, drop the singleton
routing in `verbsmain_test.go`, add `t.Parallel()` everywhere, and
validate. User asked explicitly for <100s wall-clock.

**Done:**

- **`claimUAS(t, ctx) *UAS`** in `tests/verbs/helpers_test.go`.
  Per-test: POSTs `/Clients` (~250ms), starts a sipgo+diago stack
  (~800ms REGISTER), returns `{SID, Username, Password, Stack, Inbound}`.
  `Inbound` is a buffered chan (cap 4) private to the test. Cleanup runs
  via `t.Cleanup`: `stack.Stop()` BEFORE the /Clients delete (cleanup is
  LIFO). Drops the cosmetic "Failed to unregister: transaction canceled"
  noise the spike noted, but a different cosmetic warning ("403
  Forbidden" on the post-delete unregister) appears instead — still
  cosmetic, suite continues.
- **`placeCallTo` / `placeWebhookCallTo` / `placeWebhookCallToNoWait`** —
  replacement helpers that route to a specific UAS. The `*NoWait`
  variant is for the side of a multi-leg test that picks up its inbound
  call off the channel in a goroutine instead of blocking inline.
- **Phase-1 calls route through the real ngrok webhook.** Was: every
  Phase-1 test sent placeholder `https://example.invalid/hook` for both
  call_hook and call_status_hook. jambonz successfully ignored
  call_hook (app_json wins) but tried to POST status updates to the
  invalid URL → flooded feature-server logs with `getaddrinfo ENOTFOUND
  example.invalid`. New `callbackURLs(t)` helper points both at the
  ngrok tunnel when it's up, registers a session under `t.Name()` so
  status callbacks have somewhere to land. Sessions are NOT released on
  test exit because the final `completed` status callback fires ~1s
  after our BYE (often after the test has returned); TestMain teardown
  reaps the registry instead.
- **Migrated all 47 tests** (single-leg, multi-leg, Phase-2 webhook,
  sip:decline UAC). Multi-leg tests provision two UASes via
  `claimUAS` twice. `sip:decline` switched from `cfg.SIPUser`/`cfg.SIPPass`
  + the package-level `stack` to `uas.Stack` + `uas.Username`/`uas.Password`.
- **Deleted `tests/verbs/spike_dynamic_client_test.go`** — its job was
  to prove the path, and now every test exercises it.
- **Deleted singletons in `verbsmain_test.go`:** `currentCall`,
  `currentCalleeCall`, `stack`, `stackCallee`, `routeHandler`,
  `calleeRouteHandler`, `claimCalleeCall`. ~150 lines gone. The new
  TestMain only owns the webhook tunnel + Application — no SIP stacks.
- **`t.Parallel()` on every test** via a one-shot perl pass.
- **Call-sid → session routing in `internal/webhook/registry.go`.**
  When a webhook arrives WITH `x_test_id`, record
  `Registry.callSidIndex[call_sid] = testID`. When a webhook arrives
  WITHOUT `x_test_id` (jambonz's `tag` verb replaces customerData;
  `transcribe`'s transcriptionHook drops it), look up by `call_sid`
  instead. Without this, `_anon` becomes a shared bag under parallel
  and tag/transcribe tests race-drain each other's callbacks.
  `sessionOrFallback` → `sessionFor(testID, callSid)`.

**Verified:**

| Mode | Wall-clock | Status |
|---|---|---|
| Sequential (baseline) | 252s | all green |
| `-parallel 8` run A | 82s | all green |
| `-parallel 8` run B | 77s | all green |
| `-parallel 8` run C | 84s | all green |

**3.0–3.3× speedup, stable across 3 consecutive runs.** Well under the
user's 100s target. Theoretical floor is ~63s (252/4 effective
concurrency); we're at ~80s because:
- ngrok free tier rate-limits parallel HTTP requests
- POST /Clients + REGISTER per test pays ~1s overhead
- Some tests have built-in wall-clock floors (Conference: 25s for
  speaker-listener-bridge-settle-WAV-Deepgram dance)

**Surprises:**

- Cleanup-time DEREGISTER returns `403 Forbidden` because our /Clients
  cleanup deletes the row first, then the stack tries to deregister a
  user that no longer exists. Cosmetic; jambonz cluster doesn't care.
  Could be fixed by ordering Stack.Stop fully BEFORE
  ManagedSIPClient's t.Cleanup runs — would need to register the stack
  cleanup with t.Cleanup BEFORE calling ManagedSIPClient (LIFO order).
  Not worth the complexity now.
- `SIPClient.is_active` drift (see Known issues #0). Caught by the
  sweeper at TestMain, doesn't break anything.
- The `_anon` race only manifested under parallel. Sequential always
  worked because tests drain `_anon` while no other test is filling
  it. The fix (call_sid index) is robust independently of parallel —
  also correct for the sequential case where late-arriving callbacks
  cross test boundaries.

---

### 2026-04-25 — Spike: dynamic SIP client provisioning (parallelism foundation)

**Scope:** prove that the harness can create+delete SIP user credentials
on the fly via the live `/Clients` REST endpoint, then register a UAS with
those credentials and accept calls. Foundation for parallelising the verbs
suite (each test owns its own SIP user → no `currentCall` singleton →
`t.Parallel()` viable).

**Done:**

- **Verified `/Clients` API exists.** The Tier-1 HANDOFF entry (~line 75)
  said "Clients (SIP users) — not in the swagger." The swagger we
  vendored is incomplete; live docs at
  https://docs.jambonz.org/reference/rest-platform-management/clients/
  show full CRUD on `/Clients`. POST body: `account_sid`, `username`,
  `password` (required) + `is_active` etc. (optional). Returns `{sid}`.
- **`internal/provision/sip_clients.go`** — SDK methods `CreateSIPClient`,
  `ListSIPClients`, `DeleteSIPClient`, `ManagedSIPClient` (the
  test-friendly variant: returns `(sid, username, password)` and
  registers `t.Cleanup` to delete on exit). Username pattern
  `it-<runID>-<role>-<hex8>`; password 32-hex random. Per ADR-0008.
- **`schemas/rest/clients/createClient.response.201.json`** — wraps the
  shared `successful_add.json`, so 201 bodies are contract-validated like
  every other create endpoint.
- **`internal/provision/sweeper_sip_clients.go`** — `SIPClientSweeper`
  follows the existing `Sweeper` interface; deletes any
  `it-<otherRunID>-*` Client. Wired into `tests/rest/restmain_test.go`
  and `tests/verbs/verbsmain_test.go` orphan sweeps.
- **`tests/verbs/spike_dynamic_client_test.go`** — `TestSpike_DynamicClientLifecycle`.
  Steps: create-sip-client → start-stack-and-register → place-self-call →
  answer-and-wait-end → assert-sip-methods. **Runs end-to-end against
  jambonz.me in 3.5s.** Skipped under `-short`.

**Verified live timings (single test):**

| Step | Time |
|---|---|
| `POST /Clients` | 246ms |
| sipgo+diago Register | 803ms |
| place call → INVITE arrival | <300ms |
| answer + BYE | ~1.7s |
| **total** | **3.5s** |

**Surprises:**

- diago emits `ERROR Failed to unregister: transaction canceled` when the
  parent ctx is cancelled before the unregister round-trip completes.
  Cosmetic; doesn't fail the test, but worth fixing later by ordering
  `stack.Stop()` before the parent-ctx cancel.
- `WARN TCP ref went negative` from sipgo on shutdown — looks like an
  upstream tracking bug in connection ref-counting. Not ours.

**Deferred — full parallelisation refactor.** Spike proved the API works,
but rolling it out across 30 verb tests + multi-leg tests + Phase-2
webhook tests is a 3-4 hour edit + 3+ validation runs. Deliberately not
landed in this session to avoid half-done state. Blueprint:

1. **`claimUAS(t, ctx) *UAS` helper** in helpers_test.go: bundles
   `ManagedSIPClient` → `jsip.Start` → returns `{username, password,
   stack, inboundCh}` with `t.Cleanup(stack.Stop)`.
2. **Replace singletons.** Drop `currentCall`/`currentCalleeCall` from
   `verbsmain_test.go`. Each per-test stack has its own private inbound
   channel.
3. **`placeCallTo(ctx, t, uas, verbs, ...)`** — new core helper that
   posts /Calls with `to=<uas.username>@<domain>` and reads from
   `uas.inboundCh`. `placeCall` becomes a thin wrapper that calls
   `claimUAS` first.
4. **Multi-leg tests** call `claimUAS` twice (caller + callee). Replaces
   `claimCalleeCall` + the parallel callee stack.
5. **Phase-2 tests:** add `placeWebhookCallTo(ctx, t, uas, sess, ...)`
   variant with the same pattern.
6. **`t.Parallel()`** at the top of every verb test.
7. **Cap parallelism** at the `go test -parallel` flag — start with
   `-parallel 4`. ngrok free tier has tunnel/req limits; jambonz cluster
   may rate-limit too. Can crank up after observing.
8. **Validate with 3+ runs** before declaring done.

Best-case win: **210s → 80-100s** based on the 4× parallel ceiling. Worst
case (ngrok throttling): we accept `-parallel 2` and still save ~50%.

---

### 2026-04-23 — Suite wall-clock pass (265s → target ~170s)

**Scope:** suite was taking 269s end-to-end. Audited where the time actually
goes and trimmed the biggest offenders. Real-call tests are I/O-bound on
jambonz-side wall-clock (TTS speech duration, RFC 2833 tone emission,
bridge settle), so there's a hard floor — but several knobs were set too
generously.

**Done:**

- **DTMF trailing pauses**, `tests/verbs/dtmf_test.go`. Every DTMF test used
  `pause 8s` regardless of digit count. Right-sized per test: SingleDigit
  `8→2s`, Symbols `8→3s`, MultiDigit `8→5s`. Sizing formula is
  `(tones × 500ms) + (w-separators × 500ms) + 1s slack`. Savings: ~14s.
- **Tag callback drain window**, `tests/verbs/tag_test.go`. `DrainCallbacks`
  is bounded-wait — the budget is only paid on the last empty-wait. 5s→2s;
  anon fallback 1s→500ms. Savings: ~3s.
- **Conference sleeps** — *attempted then reverted*. Trimming pre-speaker
  settle 2s→1s caused the WAV prefix to clip ("the sun is shining" became
  "is shining" in the listener recording). Reverted to 2s. The
  speaker-side bridge-settle was also reverted to 1.5s defensively (one
  flake is enough; the savings here were negligible). Net change: 0s.
- **Fixed Conference regression**, `tests/verbs/conference_test.go`. Previous
  run was 62.75s (vs ~30s in earlier junit). Root cause: listener leg's
  `TimeLimit: 60` meant if `DeleteCall` propagation lagged, the listener
  sat in-room until its own TimeLimit expired. Dropped `placeCalleeCall`
  TimeLimit to 20s. Speaker speech is only ~5s so 20s is plenty; conference
  + enqueue_dequeue both share this helper. Savings: up to ~30s on the
  conference test alone.
- **`-short` gating for two slow tests**:
  - `TestVerb_Say_LongText` (15s TTS wall-clock) — shorter say tests cover
    the same code path.
  - `TestVerb_Stream_Basic` (5s; alias of Listen_Basic under a different
    verb name) — full release gate runs both.
  Inner-loop devs: `go test -short`. Release gate: no flag, runs everything.
  Savings when `-short`: ~20s.

**Measured:** verbs **264.5s → 210.8s = -53.7s (-20%)** on a clean run.
Full-suite (rest+verbs): **286s → 232s**. With `-short`: roughly 215s.

**One pre-existing flake surfaced and fixed:** `TestVerb_Say_ArrayRandom`
sometimes picks "Welcome back." which Google TTS renders in 4.46s; test
had `maxDur: 4s`. Bumped to 5s. Not caused by this perf pass — it had
been one bad random draw away from failing on any run.

**Deliberately not done this session — parallelisation.** Biggest remaining
lever (~100-130s saving), but requires a correlation refactor:

1. Replace `currentCall` / `currentCalleeCall` singleton channels in
   `verbsmain_test.go` with per-test-ID routing.
2. Have each test place calls with a unique `From` number (e.g.
   `441514500000+testIdx`); route inbound INVITEs by `From.User` →
   per-test channel.
3. Add `t.Parallel()` at the top of every test.
4. Handle the Phase-2 webhookApp — multiple concurrent tests hitting the
   same Application is fine (scripts are per-session) but we'd want to
   verify the ngrok tunnel's concurrent request handling.
5. Validate with 3+ full-suite runs to catch race conditions.

Deferred because a half-done parallelisation loses more to flake debugging
than it saves. Tracked as a follow-up in "Next" — don't quietly roll it in
without the validation runs.

**Surprises:**

- Conference's 62s run was mostly the listener leg waiting out its full
  TimeLimit after the speaker hung up. The jambonz-side conference room
  doesn't auto-evict members when one hangs up — each leg independently
  waits for BYE or TimeLimit.
- `DrainCallbacks`'s loop returns as soon as the next `WaitCallback` times
  out on an empty queue — so the supplied `within` duration is an upper
  bound, not a fixed cost. Only the LAST WaitCallback pays the tail
  timeout. Trimming the budget only saves that tail.

---

### 2026-04-23 — Step-named logs + per-test watchdog timeout

**Scope:** make failing tests cheap to triage. Every step in every test now
logs its entry, exit, and — on failure — a single line that names both the
step and the reason. Every test has a hard deadline enforced by a watchdog
that marks the test FAIL the moment its budget is exceeded, independent of
go-test's 10-minute binary alarm.

**Done:**

- `StepCtx` helper (`tests/verbs/helpers_test.go`, `tests/rest/helpers_test.go`).
  Per-step API: `s := Step(t, "name")` at the top, `s.Done()` at the bottom,
  `s.Fatalf` / `s.Errorf` / `s.Fatal` for failures, `s.Logf` for info logs
  that don't carry the step prefix. Failures emit
  `[step:<name>] FAILED: <reason>` on a single line. Passing steps emit
  `[step:<name>] start` → `[step:<name>] ok (Xms)`. Failures suppress the
  `ok` so no misleading pass follows a FAIL in the same step.
- `WithTimeout(t, d) context.Context` — one-line replacement for
  `context.WithTimeout(context.Background(), d); defer cancel()`. Arms a
  watchdog goroutine that fires at `d + 2s` (safety margin for
  context-aware code to unwind at its own deadline). When the watchdog
  fires it calls `t.Errorf("[test-timeout] FAILED: exceeded %s (last step: %s)")`
  so the operator sees both the budget and exactly which named step was
  stuck. Cannot force-unwind a truly wedged goroutine (Go has no such
  primitive) — but guarantees the reason is in the log at `d+2s` rather
  than at the 10-minute binary alarm.
- Rolled step logging through every test: **all 56 tests** (39 files) now
  have `Steps:` blocks in their top comments matching the named
  `Step(...)` calls in the test body. Operators reading a failure log can
  find the failing step name in the comment without opening the source.
- Rolled `WithTimeout(t, d)` through every test that previously used
  `context.WithTimeout(context.Background(), ...)` + `defer cancel()`. The
  two-line dance is now one line. Inner ctxs with shorter deadlines (e.g.
  `WaitCallbackFor` windows, cleanup contexts) kept their existing shape
  since they're intentional sub-budgets.
- Assertion helpers updated to take `*StepCtx` instead of `*testing.T` so
  their failures also name the step: `AnswerRecordAndWaitEnded`,
  `AssertAudioDuration`, `AssertAudioBytes`, `AssertTranscriptContains`,
  `RequireRecvMethods`, `RequireSentStatus`.

**Verified:** `go vet ./... && go build ./...` clean. REST suite (no SIP,
20 tests) passes end-to-end in 22.6s with step logs visible for every
assertion. Watchdog proven by a demo test that sleeps past its 2s budget
— it FAILs at 4s with the named step, not at 10 minutes.

**Surprises / design notes:**

- `runtime.Goexit()` inside the watchdog only unwinds the **watchdog
  goroutine**, not the stuck test goroutine. Go doesn't expose a way to
  interrupt one goroutine from another. So the watchdog's real value is
  *naming the failure* at budget+safety rather than *killing the
  goroutine*. If a test really is wedged in a non-context-aware syscall,
  go-test's own `-timeout` eventually kills the binary — but operators
  at least see `[test-timeout] FAILED: exceeded 30s (last step: place-call)`
  the instant the budget is blown, not 10 minutes later.
- Failing `Errorf`-style assertions now mark the step `ended=true` so
  Done() no-ops after the FAILED line — no misleading `[step:x] ok`
  trailing a `[step:x] FAILED`.
- Function names beginning with `Test` must match the `func(*testing.T)`
  signature. That's why the helper is `WithTimeout` not `TestTimeout`.

---

### 2026-04-20 — Remaining verb coverage + generic WS utility

**Scope:** close out every testable verb we can reach without vendor
credentials, and build the general-purpose WS infrastructure future
tests will need for AsyncAPI / LLM / agent work.

**Done:**

- **`conference`** — two POST /Calls into a uniquely-named room. Speaker streams `test_audio.wav`; listener records through jambonz's mix; Deepgram confirms `"the sun is shining"` came across. Real cross-leg audio verified.
- **`enqueue` + `dequeue`** — enqueuer waits in a named queue, agent dequeues, they bridge. Same audio passthrough + Deepgram assertion as `dial`. Needed 4s settle time (vs 2s for direct `dial`) — queue matching + bridge setup is slower.
- **`leave`** — caller enqueues with a waitHook returning `[leave]`; the verb pops the caller back to the main script, which `say`s a distinctive phrase we then Deepgram-verify in the caller's recording.
- **`transcribe`** — explicit `{vendor:google, language:en-US, singleUtterance:true}` recognizer config. Hook payload confirmed via the pinned transcript.
- **Generic WebSocket utility** in `internal/webhook/ws.go`. Not audio-specific — one `/ws/<session-id>` endpoint that upgrades, routes by session, and exposes `WaitWSMessage`/`CollectWS`/`SendWSText`/`SendWSBinary`/`WSMetadata`/`WSClosed` on `Session`. Captures both text (including opening JSON metadata) and binary frames. Will back future tests for the AsyncAPI ("jambonz WebSocket API") and bidirectional verbs.
- **`listen`** — streams audio to our WS endpoint; test asserts non-trivial binary received (distinct-byte-count sanity) and logs the JSON metadata header jambonz sends.
- **`stream`** — schema says it's an alias for `listen`; runs the same assertions under the different verb name.
- **Skip-stubs** for Tier-5 verbs that need vendor credentials we don't have on the test account: `llm`, `s2s`, `agent`, `dialogflow`, `openai_s2s`, `deepgram_s2s`, `elevenlabs_s2s`, `google_s2s`, `ultravox_s2s`, plus `rest_dial` (internal verb, origination already covered via POST /Calls). Each skip documents what would run and what credential is needed.

**Drift findings:**

- **`transcribe` drops our `customerData.x_test_id` correlation.** The transcriptionHook payload lands in the anon session. Same issue we saw with `tag` and `dub` — documented in the test's drain-both-sessions path.
- **jambonz emits `customerdata` (lowercase d) in some payloads** — noticed in the transcribe payload. Our extractor happened to work via anon-fallback; worth tightening extractors to be case-insensitive at some point.
- **Dial's actionHook doesn't include `dial_call_duration`** on live jambonz.me (documented earlier). Confirmed.

**Full verb suite: 47 tests, 34 PASS, 13 SKIP, 0 FAIL, 261s.**

Verb coverage: **23 of 34 verbs tested**. Remaining 11 are all either vendor-gated (10) or explicitly internal (`rest_dial`).

---

### 2026-04-20 — Transcript verification via Deepgram

**Scope:** stop accepting "audio of plausible duration arrived" as a strong say-test assertion. Upload the captured recording to Deepgram, read back the transcript, assert the expected words are there.

**Done:**

- New package `internal/stt/` wrapping the Deepgram Go SDK (v3.5.0). Single entry point `Transcribe(ctx, pcmPath) (string, error)` + `HasKey()` + `Normalize(s)` helper. Env var: `DEEPGRAM_API_KEY`. Skipped (with a log) when unset.
- **Encoding finding:** diago's `StartRecording` writes linear PCM16 (8 kHz mono little-endian), not µ-law — it decodes incoming PCMU/PCMA via `audio.NewPCMDecoderReader` before writing. Deepgram's `encoding` param must be `linear16`, not `mulaw`. Documented in `internal/stt/deepgram.go`.
- **SmartFormat:** intentionally off. Deepgram's SmartFormat rewrites spoken numbers as numerals (`"one two three"` → `"1 2 3"`), breaking word-substring asserts. Also enabled `Keyterm: ["jambonz"]` to bias the model toward our proper noun.
- Updated all 6 `say` tests with transcript expectations via new `AssertTranscriptContains(t, ctx, wavPath, wants...)` helper. `AnswerRecordAndWaitEnded` now returns the recording path so tests can hand it to the assertion.
- **Leading-word clipping:** the first ~200ms of TTS can be lost to RTP pinhole warmup. Tests assert on mid-phrase tokens instead of first words. Documented in the test comments.
- **Gather stability tune:** bumped the post-SendSilence sleep from 500ms to 1500ms. 500ms occasionally missed every digit on cold-start cycles; 1500ms has been green across 3 consecutive runs + the full suite.

**Full verb suite with Deepgram on: 27 tests, 24 PASS, 3 SKIP (alert, sip:decline, message), 135s.**

---

### 2026-04-20 — Phase 2 verb coverage: +10 verbs in one push

**Scope:** write every verb test we can reasonably author without multi-leg orchestration or vendor credentials. Finish the Phase-2 surface.

**Done:**

- **answer** — basic `[answer, pause, hangup]` flow. Asserts INVITE/BYE in `Received()` + 200 in `Sent()`.
- **tag** — webhook flow. Asserts `customerData.foo=bar` appears on a subsequent callback. **Drift finding**: the `tag` verb REPLACES `customerData` rather than merging, so our correlation key `x_test_id` gets dropped on the next hook — that callback lands in the anon session. Test reads from both sessions. Worth noting when porting future `tag`-dependent tests.
- **redirect** — webhook flow. Asserts a second `/action/redirect` hook fires after the redirect verb.
- **config** — webhook flow. Sets session-level synthesizer and proves a subsequent `say` without inline synth params still produces audio.
- **dub** — webhook flow. `addTrack + playOnTrack` with a public WAV; asserts enough PCM bytes arrive to confirm the track is mixing into our RTP.
- **sip:request** — webhook flow. Sends INFO with `X-Test: hi` and custom body. Asserts the INFO lands in `call.Received()` with the header present.
- **sip:refer** — webhook flow. Sends REFER to `sip:transfer-target@example.invalid`. Asserts REFER arrives at our UAS with the correct `Refer-To`. Doesn't complete the transfer (would need third-party UAS); a 202 response from us is enough.
- **message** — webhook flow, opt-in via `MESSAGE_CARRIER_TEST_TO` / `MESSAGE_CARRIER_TEST_FROM`. Skipped on clusters without an SMS carrier; flow is correct but live SMS send is gated by carrier cost/authz.

**Skipped (documented in the test files):**

- **alert** + **sip:decline** — both target the leg where jambonz is the *callee*, not the caller. Our Phase-1 shape has jambonz-as-caller (via POST /Calls), so these verbs never fire against our UAS. Requires UAC origination — we have the infra from the spike; just not wired into `tests/verbs/` yet. Track as a follow-up.

**Deferred (require multi-leg):**

- `dial`, `conference`, `enqueue`/`dequeue`/`leave` — need 2 concurrent calls orchestrated against the same conference/queue. The harness can do it, but it's meaningfully more plumbing. Separate session.

**Deferred (Tier 5, credential-gated):**

- `transcribe`, `listen`, `stream`, `llm`/`s2s` + all vendor-specific `*_s2s` variants, `dialogflow`, `rest_dial`. Each needs real vendor creds.

**Surprises:**

- **Gather flakes on cold-start.** First call through a fresh ngrok tunnel occasionally misses the first RFC 2833 event; subsequent runs pass. Added a 500ms `time.Sleep` after `SendSilence` to let jambonz's DTMF detector arm before our 2833 stream starts. Test now reliable on both cold and warm tunnels.
- **`tag` replaces rather than merges** (see above). If other verb tests later rely on reading `customerData.x_test_id` after a `tag` verb runs, they'll need the same anon-session fallback pattern.

**Full verb suite: 27 tests, 24 PASS, 3 SKIP (alert + sip:decline + message), 128s.**

---

### 2026-04-20 — Tier 3 SIP API gap closure: full in-dialog observability

**Scope:** close the `call.Received()` gap for in-dialog requests (BYE / ACK / INFO / REFER / NOTIFY / re-INVITE) and land the missing response-capture / media-accessor / send-REFER-INFO-MESSAGE surface so future verb tests can be written without running into missing capabilities.

**Done:**

- Added `internal/sip/observer.go` — middleware registered via `diago.WithServerRequestMiddleware`. On every inbound in-dialog request it looks up the owning `Call` by Call-ID in a shared registry, records the request on `Received()`, and replaces `sip.ServerTransaction` with an `observedTx` whose `Respond(res)` records the outbound response on `Sent()` before passing through. Zero diago/sipgo fork — uses only the sipgo middleware chain that diago already forwards to us (`diago.go:226-241`).
- `newInboundCall` / `newOutboundCall` register the Call in the registry; `setState(StateEnded)` unregisters. In-dialog requests captured end-to-end: hangup test's received list is now `[INVITE, ACK, BYE]` and sent is `[200]`.
- Tightened `TestVerb_Hangup_WithHeaders` to assert `X-Custom-A=foo` / `X-Custom-B=bar` on the received BYE — the Tier-3 TODO that prompted this whole session is now a real assertion.
- Added media accessors on `Call`: `LocalRTPAddr()`, `RemoteRTPAddr()`, `LocalSDP()` (pass-throughs over `DialogMedia.MediaSession()`).
- Added in-dialog send helpers: `SendInfo(ctx, contentType, body, extra...)`, `SendMessage(ctx, contentType, body, extra...)`, `SendRefer(ctx, target)`. INFO/MESSAGE build a `sip.NewRequest` and go through `dialog.Do`; REFER uses diago's built-in `d.Refer`. Both sides of the exchange (sent request + received response) land in the message history.

**Rejected (for now):**

- **Wiring `AnswerOptions.OnMediaUpdate`** to capture re-INVITE as a callback. Re-INVITE requests are already captured by the observer middleware (diago dispatches them through `OnInvite` → the middleware wraps that handler). `OnMediaUpdate` only adds a timing signal; no Phase-2 test needs it yet. Reopen if `dial`/`listen`/`transcribe` assertions need to block on renegotiation completing.
- **Fork of diago/sipgo** to capture *outbound* in-dialog requests diago auto-generates from its own goroutines (the NOTIFY for an incoming REFER, `dialog_session.go:202`; auto-ACK on 200 OK for a re-INVITE; BYE generated by diago's Hangup). The existing `SendInfo`/`SendMessage`/`SendRefer` already record anything we initiate. The auto-sent stuff could matter later for asserting on full REFER subscription flows; keep it as a known limitation, revisit if a Tier-4+ test needs it.

**Full verb suite: 17 tests, 101s, all green.** No regressions.

---

### 2026-04-20 — Phase 2 pilot (gather) green, hand-rolled RFC 2833 sender

**Scope:** turn the earlier session's `2223` near-miss into a clean `1234` detection. Continuation of correlation work earlier same day.

**Done:**

- Replaced `SendDTMF` with a hand-rolled RFC 2833 packetizer that drives `DialogMedia.RTPPacketWriter.WriteSamples` directly. Layout per digit at 20ms ptime: 12 interim event packets (duration 160, 320, …, 1920 samples — i.e. 250ms total tone) all sharing one RTP timestamp; then one end-of-event packet at the same timestamp; then advance `nextTimestamp` by `duration + 40ms` silence before the next digit.
- Key finding from feature-server logs (user captured live): freeswitch treats each RFC 4733 recommended end-of-event retransmission as a **separate** completed DTMF event. Sending 3 end packets turned `"1"` into `"1 1 1"`. One end packet is enough — RFC 4733 recommends 3 for loss resilience, but we'll accept the reliability trade-off on a LAN-y testbed where we control both ends. Document clearly in the code comment so future-me doesn't "fix" it.
- Added `SendDTMFWithDuration(digits, perTone)` for callers that need a different tone length. `SendDTMF` defaults to 250ms/tone.
- Verified end-to-end: gather test passes with `digits:"1234"`, `reason:"dtmfDetected"`. Feature-server logs show four discrete `TaskGather:_onDtmf` events 200-260ms apart. Phase-1 `TestVerb_Dtmf_*` (us as receiver) unaffected.

**Surprises:**

- The earlier `2223` reading wasn't partial DTMF detection — it was jambonz's inband detector false-positiving on our `SendSilence`'s constant-value PCMU frames *and* ALSO misdecoding shared-timestamp events as the same digit. Two unrelated bugs stacked. Removing either one separately would have kept the test broken for a different reason. This made the bisection awkward — "it looks worse now" was actually "the first bug is fixed and now you can see the second one."
- User's ability to tail feature-server logs live cut debugging time roughly in half. Worth noting: when debugging end-to-end stuff with jambonz, ask for the feature-server log stream before trying to reverse-engineer from payloads alone.

**Left on the table:**

- Schema URL `$ref` loader — still deferred; contract validation for inbound callbacks is currently best-effort.
- Upstream PR against emiago/diago for the timestamp bug.

---

### 2026-04-20 — Phase 2 correlation unblocked

**Scope:** diagnose and fix the `X-Test-Id` correlation bug blocking the gather test.

**Done:**

- Traced the root cause through api-server and feature-server source. Bug is not ours, not ngrok's — `validateCreateCall` in `api-server/lib/routes/api/accounts.js:415-434` overwrites the caller's `call_hook` with the Application's when `application_sid` is present, so the per-call URL override (with our query param) never reaches feature-server. Ironically feature-server's merge order (`{...application, ...req.body}`, `create-call.js:248-251`) would have honored it.
- Switched the harness to the `tag` field. `provision.CallCreate.Tag` is already `map[string]any`; added `webhook.CorrelationKey = "x_test_id"` and moved `placeWebhookCall` to send `Tag: {x_test_id: session.ID()}` instead of URL/header stuffing. `extractTestID` now reads `customerData[CorrelationKey]` as the primary path.
- Ran `TestVerb_Gather_Digits`: action/gather callback arrives with `"customerData":{"x_test_id":"TestVerb_Gather_Digits"}` — correlation verified end-to-end.
- Discovered a downstream DTMF bug: `SendDTMF("1234")` makes jambonz observe `"2223"`. Inspected diago's `RTPDtmfWriter.writeDTMF` — writes all 7 packets for a digit with RTP timestamp 0 and no inter-digit gap. Added a 60ms (then 500ms) inter-digit `time.Sleep`; did not fix, 500ms tripped jambonz's hangup-after-gather. Phase-1 DTMF tests (jambonz → us) unaffected. Full analysis + options in Known issues #3.

**Surprises:**

- I spent the first pass assuming ngrok or our extractor was the issue, per HANDOFF's hypothesis list. Reading api-server source directly flipped the priority order — the "unlikely" hypothesis was wrong; the "our override is being ignored" hypothesis was right, but the mechanism wasn't the one HANDOFF proposed (sub-paths / SIP headers wouldn't have helped either — api-server clobbers the entire `call_hook` object). Lesson: when the correlation is on URLs and the downstream is a multi-tier pipeline, read the upstream handler before prototyping workarounds.

**Left on the table:**

- DTMF digit-shift — real regression in our Phase-2 send path. Next session's first item.
- Schema URL `$ref` loader — still deferred; contract validation for inbound callbacks is currently best-effort.

---

### 2026-04-18 — Session 1

**Scope:** first-pass design, stack selection, contract-testing decision, session-continuity setup.

**Done:**

- Wrote [ARCHITECTURE.md](ARCHITECTURE.md) v0.1 (Python/pjsua2/pytest/FastAPI) → revised to v0.2 (Go/sipgo+diago/`go test`/`net/http`) after stack switch.
- Authored 14 initial ADRs: 0001 (meta), 0002 (scope), 0003 (venv — later superseded), 0004 (pytest — later superseded), 0005 (pjsua2 — later superseded), 0006 (webhook via ngrok), 0007 (three SIP modes), 0008 (run-id cleanup), 0009 (config), 0010 (release-gate scope).
- Ran spike [spikes/001-sipgo-diago/](spikes/001-sipgo-diago/) against `sip.jambonz.me` from a NAT'd laptop. Confirmed: Go+sipgo+diago installs via `go get` (no SWIG/native build), SIP signaling over TCP works, digest auth works, custom `X-Test-Id` header works, codec negotiation (PCMU) works, **symmetric RTP / media latch delivers inbound audio behind NAT even without `PUBLIC_IP` advertised** — 159,680 PCM bytes / 9.98s / RMS 434 of real audio captured.
- Switched project from Python to Go. Superseded ADR-0003 / 0004 / 0005. Added ADR-0011 (Go + modules), 0012 (`go test`), 0013 (sipgo+diago), 0014 (symmetric RTP / no `PUBLIC_IP` required for UAC). Updated 0006, 0007, 0008, 0009 in place to match Go vocabulary.
- Inventoried the authoritative fern specs at `<jambonz-fern-config-checkout>/`. Result: ~25 REST platform resources, ~10 call-control endpoints, ~30 verbs (only say+play in YAML; rest are MDX-only), ~25 webhook/action-hook types (partial YAML), plus a full WebSocket API.
- Raised contract testing as a first-class requirement. Wrote **ADR-0015 (contract testing)**: hand-rolled hybrid schema strategy, `santhosh-tekuri/jsonschema`, `additionalProperties: true`, violations = failures, `ErrNoSchema` on gaps.
- Wrote [docs/coverage-matrix.md](docs/coverage-matrix.md) with the full fern inventory laid out as Tier 1–7 implementation plan. Each row has Feature + Contract status + schema source.
- Wrote [CLAUDE.md](CLAUDE.md) at repo root — auto-loaded by future Claude Code sessions; routes them to ADRs/coverage/architecture; lists non-negotiable rules.
- Updated auto-memory: fixed stale Python entry, added `project_adr_driven.md` pointer, updated user role to reflect Go.

**Decisions taken in this session** (all captured in ADRs 0011–0015 — this is the pointer, not the record):

- Go + modules (ADR-0011)
- `go test` stdlib runner (ADR-0012)
- sipgo + diago (ADR-0013)
- Symmetric RTP implicit; `PUBLIC_IP` conditional (ADR-0014)
- Contract-validate every response (ADR-0015)

**Left on the table:**

- Tier 1 hasn't started. All code so far is the spike (in `spikes/001-sipgo-diago/`, to be deleted later).
- No `go.mod` at repo root yet.
- **Spike-era SIP password rotated** before the public commit. No long-lived SIP credentials live in the repo; per-test users are provisioned dynamically via `/Clients` (see `claimUAS` in `tests/verbs/helpers_test.go`).

---

## Maintenance notes

- **When a tier completes:** update the tier row in [docs/coverage-matrix.md](docs/coverage-matrix.md) to ☑ and add a one-line session log entry here pointing to the completing commit/PR.
- **When an ADR is superseded:** flip the old ADR's status, add the new ADR, update the index at [docs/adr/README.md](docs/adr/README.md), and add a session log entry here naming both ADRs.
- **When an open question is answered:** remove it from the Open questions list and, if the answer is architectural, write or update an ADR.
- **When something surprising happens:** add a `Surprises` subsection to the session entry. Future-you will thank you.
