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

## State as of 2026-05-01

> **Orientation:** for the harness ↔ jambonz component diagram + traffic
> breakdown, see [docs/architecture/components.md](docs/architecture/components.md).

### Now (in progress)

- **Tier 5 `agent` verb green: 11 tests parallel ~28s wall-clock.** Self-hosted webhook + ngrok serves the `agent` verb response (no external deploy needed). Inline LLM auth (`agent.llm.auth.apiKey`) bypasses /LlmCredentials provisioning — bring DEEPSEEK_API_KEY in `.env`. STT + TTS use the in-jambonz Deepgram credential we provision at TestMain; offline reply transcripts verified by re-uploading to Deepgram. Per-test eventHook/toolHook routing via `?X-Test-Id=<testID>` query param (server's `extractTestID` was already wired) — no `_anon` contention under parallel.
- Coverage: round-trip echo, eventHook (`user_transcript` / `llm_response` / `turn_end`), `greeting:true`, `actionHook` on end (callInfo + completion_reason + customerData correlation round-trip), `toolHook` round-trip (LLM function call → server replies JSON body → LLM speaks the secret word), `bargeIn` + `user_interruption`, `noResponseTimeout` re-prompt, `turnDetection:"krisp"`, `noiseIsolation` 3 variants. See `tests/verbs/agent_test.go`.
- New infra: `internal/tts/deepgram.go` (cached Deepgram /v1/speak helper for pre-generated user-side WAVs), `webhook.Session.ScriptActionHookBody` (raw JSON body responder, needed for toolHook).
- Drift in `schemas/callbacks/agent-turn.schema.json`: feature-server emits `latency.{stt_ms, eot_ms, llm_ms, tts_ms, tool_ms}` (upstream uses `*_latency` names) and adds `turn_end.{confidence, tool_calls}` not declared upstream. Both kept in the local schema with `DRIFT (TODO upstream)` markers — file a PR upstream when convenient.

- **Tier 3 verb coverage: 23/34 verbs tested, 34 passing + 13 skip-stubs, parallel runtime ~80s (down from 252s sequential).** Audio-bearing tests all verified content-level via Deepgram. Multi-leg tests (`dial`, `conference`, `enqueue/dequeue/leave`) provision two dynamic UASes per test and assert bridged audio passes through. `listen`/`stream` exercised via a generic WS endpoint in `internal/webhook/ws.go`.
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

### Known issues

0. **`SIPClient.is_active` drift.** Live `GET /Clients` returns `is_active` as a number (0/1), not a bool. `provision.SIPClient.IsActive bool` panics on decode → the orphan sweeper logs `cannot unmarshal number into Go struct field SIPClient.is_active of type bool` at every TestMain. Sweep is non-fatal so tests continue, but stale `it-*` Clients accumulate over time. Fix: change the field to `int` or a custom IntField type. Same drift pattern as `SipGateway.inbound/outbound` (issue noted in Tier-1 drift findings).

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

**Next session, start here:**

1. **Wire UAC origination into `tests/verbs/`.** The harness already has diago's client side (the spike proved it). What's missing is a `placeCallAsUAC(ctx, t, target)` helper that lets a test INVITE a jambonz-served URI instead of calling `POST /Calls`. That unlocks:
   - `alert` — assert 180 Ringing + Alert-Info on our UAC.
   - `sip:decline` — assert the 4xx/5xx/6xx from jambonz when it rejects our INVITE.
   - Symmetric verb testing from both directions.

2. **Multi-leg orchestration.** `dial`, `conference`, `enqueue`/`dequeue`/`leave` need two concurrent calls against the same jambonz application. Plan: a `placeTwoLegCall` helper that runs two `placeCall` goroutines against a shared webhook script.

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
- **`TestVerb_Agent_NoiseIsolation/{krisp_shorthand,rnnoise_shorthand,krisp_object_form}`**
  — three sub-tests covering shorthand strings + the
  `{mode, level, direction}` object form. Same "param accepted, RTP
  flowed" smoke level as Krisp.

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
