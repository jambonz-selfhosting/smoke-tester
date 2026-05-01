# smoke-tester — Coverage Matrix

> Living document. This is the canonical answer to *"what does the release gate cover, and what's still TODO?"* Every PR that adds a test updates the relevant row. Every new jambonz release prompts a review of the inventory (new resources, new verbs, new hooks) before the release is tagged.

## How this document works

- **Tier 1 → Tier 7** organises work from broad-but-shallow to narrow-but-deep.
- Each row has two statuses: **Feature** (does the harness exercise it?) and **Contract** (do we validate its JSON Schema? see [ADR-0015](adr/0015-contract-testing.md)).
- Status values: ☐ not started · ◐ partial · ☑ done · ⊘ deliberately out of scope for v1.
- **Schema source** indicates where the JSON Schema comes from: `fern` (authoritative, in `jambonz-fern-config`), `local` (hand-rolled in `smoke-tester/schemas/`, with `TODO: upstream` marker), or `missing` (gap; blocks the row from contract ☑).

## Implementation order (tiers)

Tiers are **stages of the build**, not a priority ranking. Each tier fully lands (feature + contract for every row in the tier) before the next one starts. This keeps the harness useful at every checkpoint — even after Tier 1 it's a valid REST-CRUD release gate.

| Tier | Scope | Status |
|---|---|---|
| **1** | REST platform CRUD — every resource: create with minimal fields, GET, DELETE. Auth, pagination shape, error shape validated. No SIP. | ◐ — 12 tests green, core 10 resource types covered. SpeechCredentials, LcrRoutes+CarrierSetEntries, GoogleCustomVoices deferred to Tier 2. |
| **2** | REST depth — PUT/update on everything that supports it. Bulk creates (Lcrs etc.). Read-only endpoints. Error cases (422 in-use, 404). | ◐ — 8 new tests: PUTs (Applications, VoipCarriers, Accounts), read-only (RecentCalls, Alerts, WebhookSecret, RegisteredSipUsers), errors (404, cascade-observed-on-delete). LcrRoutes/CarrierSetEntries, TtsCache/Synthesize, SipRealms, AppEnv, PhoneNumber PUT, SipGateway PUT deferred. |
| **3** | Core call-flow verbs — `say`, `play`, `pause`, `gather` (digits + speech), `dial` (simring + answerOnBridge + actionHook), `hangup`, `answer`, `redirect`, `tag`. One call per verb. | ☐ |
| **4** | Advanced verbs — `transcribe`, `listen`, `conference`, `enqueue`/`dequeue`/`leave`, `dtmf`, `config`, `dub`, `alert`, `message`, `sip:refer`, `sip:request`, `sip:decline`. | ☐ |
| **5** | AI verbs — `llm`, `lex`, `dialogflow`, `rasa`. Gated by vendor credentials; skipped with reason when creds absent. | ☐ |
| **6** | Call-control PUT matrix — redirect, whisper, mute, hold, listen-status, record start/stop/pause/resume, DTMF inject, SIP in-dialog request, conference-participant ops. | ☐ |
| **7** | WebSocket API — alternative to webhooks + `PUT /Calls/{sid}`. Separate test package. | ☐ |

**Out of v1:** SMPP gateways, SIPREC external server, recording + pcap media download, SP-scoped admin ops beyond CRUD, WebRTC/client-SDK auth flow, Signup/Lookup/Prompts/Password-settings (no YAML schema, no high-customer-impact stability risk for v1).

---

## Tier 1 — REST platform CRUD

Spec source: `fern/apis/platform/platform.yaml`. Every response body is contract-validated against the referenced schema in `platform.yaml`.

| # | Resource | Path | Feature | Contract | Schema source | Notes |
|---|---|---|---|---|---|---|
| 1.1 | Accounts | `/Accounts` | ☑ | ☑ | local | 2 tests: SP-scope CRUD + account-scope self-read. |
| 1.2 | Applications | `/Applications` | ☑ | ☑ | local | Full CRUD. |
| 1.3 | PhoneNumbers | `/PhoneNumbers` | ☑ | ☑ | local | Full CRUD. Depends on VoipCarrier. **Drift**: number is normalised (leading `+` stripped). |
| 1.4 | VoipCarriers | `/VoipCarriers` | ☑ | ☑ | local | Full CRUD. **Drift**: `register_status` returns object, not string. |
| 1.5 | SipGateways | `/SipGateways` | ☑ | ☑ | local | Full CRUD. Depends on VoipCarrier. **Drift**: `inbound`/`outbound` return integers, not booleans. |
| 1.6 | Users | `/Users` | ⊘ | ⊘ | — | POST returns 204 with no body — no SID to capture. Revisit in Tier 2 via `/Users/me`. |
| 1.7 | Clients (SIP) | `/Clients` | ⊘ | ⊘ | — | Not in swagger. Covered via SIP REGISTER in Tier 3 client mode instead. |
| 1.8 | ApiKeys | `/ApiKeys` | ☑ | ☑ | local | Create+delete. One-time token captured. |
| 1.9 | MsTeamsTenants | `/MicrosoftTeamsTenants` | ☑ | ☑ | local | Create+list+delete. **Drift**: swagger required field is `account` — typo for `account_sid`. |
| 1.10 | SpeechCredentials (Account-scope) | `/Accounts/{sid}/SpeechCredentials` | ⊘ | ⊘ | — | Deferred to Tier 2 — needs real vendor creds to exercise the create path cleanly (jambonz validates on create). |
| 1.11 | Lcrs | `/Lcrs` | ☑ | ☑ | local | Create+get+list+delete. **Drift**: server rejects `description` field despite swagger adjacency. |
| 1.12 | LcrRoutes | `/LcrRoutes` | ⊘ | ⊘ | — | Deferred to Tier 2 — swagger/live mismatch on lcr_sid location (body vs query); use `/Lcrs/{sid}/Routes` compound endpoint instead. |
| 1.13 | LcrCarrierSetEntries | `/LcrCarrierSetEntries` | ⊘ | ⊘ | — | Deferred to Tier 2 with LcrRoutes. |
| 1.14 | GoogleCustomVoices | `/GoogleCustomVoices` | ⊘ | ⊘ | — | Deferred to Tier 2 — depends on SpeechCredentials. |
| 1.15 | ServiceProviders | `/ServiceProviders` | ☑ | ☑ | local | Read-only (list + get). Creating/deleting real SPs on a shared cluster is destructive. |
| 1.16 | Webhooks (fetch-only) | `/Webhooks/{sid}` | ⊘ | ⊘ | — | Webhooks are embedded in parent resources; not useful as standalone CRUD. |
| 1.17 | Sbcs | `/Sbcs` | ☑ | ☑ | local | Read-only probe. |
| 1.18 | Availability | `/Availability` | ☑ | ☑ | local | Read-only probe. |

### Tier 1 cross-cutting

- **Auth**: `Authorization: Bearer <token>` header validated on every request.
- **Base URL**: `{JAMBONZ_API_URL}/v1` (env-configured).
- **Error shape**: `{msg: string}` — validated on every 4xx/5xx.
- **`it-<runID>-` prefix**: enforced by `provision.Managed(t, ...)` helper, asserted in `provision` unit tests.
- **Orphan sweep**: `TestMain` deletes any `it-*` resource older than `ORPHAN_TTL_HOURS` (default 2) across every resource in this tier.

---

## Tier 2 — REST depth

| # | Area | Path | Feature | Contract | Schema source | Notes |
|---|---|---|---|---|---|---|
| 2.1 | PUT — Application, VoipCarrier, Account | (per-resource) | ☑ | ☑ | local | Uses dedicated `*Update` structs (no primary-key in body). **Drift**: PUT rejects immutable primary-key fields with 400. |
| 2.1b | PUT — PhoneNumber, SipGateway | — | ⊘ | ⊘ | — | Deferred — same pattern; lower release-gate value. |
| 2.2 | Bulk create edges | `/LcrRoutes`, `/LcrCarrierSetEntries`, `/GoogleCustomVoices` | ⊘ | ⊘ | — | Deferred (from Tier 1 too). Swagger/live mismatch on LcrRoutes POST needs investigation of `/Lcrs/{sid}/Routes`. |
| 2.3 | RecentCalls | `/Accounts/{sid}/RecentCalls` | ☑ | ☑ | local | Paginated. **Drift**: `total`/`page`/`batch` come back as strings not integers; `batch` may be omitted. Handled via `IntField` + relaxed schema. |
| 2.4 | Alerts | `/Accounts/{sid}/Alerts` | ☑ | ☑ | local | Paginated, same envelope as RecentCalls. |
| 2.5 | Sbcs | `/Sbcs` | ☑ | ☑ | local | Done in Tier 1 (trivial GET). |
| 2.6 | Availability | `/Availability` | ☑ | ☑ | local | Done in Tier 1. |
| 2.7 | RegisteredSipUsers | `/Accounts/{sid}/RegisteredSipUsers` | ☑ | ☑ | local | Array of strings. Cross-ref Tier 3 Client-mode once UAs register. |
| 2.8 | TtsCache/Synthesize | `/Accounts/{sid}/TtsCache/Synthesize` | ⊘ | ⊘ | — | Deferred — requires real SpeechCredential. Tier 3 dependency. |
| 2.9 | WebhookSecret | `/Accounts/{sid}/WebhookSecret` | ☑ | ☑ | local | Get-only covered. Regenerate deferred (mutates live secret). |
| 2.10 | SipRealms | `/Accounts/{sid}/SipRealms/{realm}` | ⊘ | ⊘ | — | Deferred. Low priority; not a release-gate signal. |
| 2.11 | AppEnv | `/AppEnv?url=...` | ⊘ | ⊘ | — | Deferred. |
| 2.12 | Error cases | (cross-cutting) | ☑ | ☑ | local | 404 on bogus SID; delete-in-use observes cluster cascade policy (finding: jambonz.me cascades accounts). |

---

## Tier 3 — Core call-flow verbs

Each row is: place a call that triggers the verb, assert webhook events + audio/DTMF where applicable, and contract-validate the verb JSON we emit + the webhook payloads we receive.

| # | Verb | Hooks involved | Feature | Contract | Schema source | Notes |
|---|---|---|---|---|---|---|
| 3.1 | `say` | `call_hook`, `call_status_hook` | ☑ | ☐ | fern (verbs.yaml) | 6 tests covering basic, SSML, long, array, loop, synthesizer-override. |
| 3.2 | `play` | `call_hook`, `call_status_hook`, `actionHook` | ☑ | ☐ | fern (verbs.yaml) + local (actionHook) | Basic, loop, array-of-urls. `actionHook` payload fields deferred. |
| 3.3 | `pause` | `call_hook` | ☑ | ☐ | local | 1s + 3s variants; duration window + RMS silence assertion. |
| 3.4 | `gather` | `call_hook`, `actionHook` | ☑ | ☐ | local | `numDigits=4 input=[digits]` → DTMF via hand-rolled RFC 2833. |
| 3.5 | `dial` | `call_hook`, `call_status_hook`, `dial.actionHook`, optional `dial.confirmHook`, optional `dial.dtmfHook` | ☑ | ☐ | local | Two UASes on separate ports. Callee streams reference WAV, caller records, Deepgram confirms content — proves real media bridge. |
| 3.6 | `hangup` | `call_status_hook` | ☑ | ☐ | fern | Basic + WithHeaders asserting X-Custom-A/B on received BYE. |
| 3.7 | `answer` | `call_hook` | ☑ | ☐ | local | `answer + pause + hangup` flow; 200 OK in call.Sent(). |
| 3.8 | `redirect` | `actionHook` → fetched-URL returns new verbs | ☑ | ☐ | local | Second webhook fetch observed at `/action/redirect`. |
| 3.9 | `tag` | (all subsequent webhooks) | ☑ | ☐ | local | `customerData` carries tag data. Drift: `tag` REPLACES customerData, does not merge. |

### Tier 3 cross-cutting

- Every test registers a per-test verb script in the webhook server keyed by `X-Test-Id`.
- Every outbound INVITE carries `X-Test-Id`.
- Silence-on-answer default (ADR-0014) is verified as a cross-cutting test.

---

## Tier 4 — Advanced verbs

| # | Verb | Hooks involved | Feature | Contract | Schema source | Notes |
|---|---|---|---|---|---|---|
| 4.1 | `transcribe` | `transcriptionHook` | ☑ | ☐ | local | Google STT; hook payload asserted via pinned Deepgram truth. |
| 4.2 | `listen` | `actionHook`, WSS stream | ☑ | ☐ | local | WS endpoint in webhook.Server captures binary audio + opening metadata. |
| 4.3 | `conference` | `waitHook`, `enterHook` | ☑ | ☐ | local | Two legs join same room; speaker streams WAV, listener records through mix. |
| 4.4 | `enqueue` | `waitHook`, `actionHook` | ☑ | ☐ | local | Paired with dequeue; audio passes through queue bridge. |
| 4.5 | `dequeue` | `actionHook` | ☑ | ☐ | local | Dequeues waiting enqueuer, bridges + Deepgram-verified audio. |
| 4.6 | `leave` | (none) | ☑ | ☐ | local | Via waitHook returning `[leave]`; asserts post-enqueue verb runs. |
| 4.7 | `dtmf` | (none) | ☑ | ☐ | local | Outgoing RFC 2833. Single/Multi/Symbols. |
| 4.8 | `config` | (none) | ☑ | ☐ | local | Session synthesizer override proven by subsequent `say` without inline synth. |
| 4.9 | `dub` | `actionHook` | ☑ | ☐ | local | `addTrack + playOnTrack` delivers audio to us. |
| 4.10 | `alert` | (none) | ⊘ | ☐ | local | Requires UAC origination — jambonz-is-callee flow. Deferred. |
| 4.11 | `message` | `actionHook` | ☑ (skip) | ☐ | local | Verb flow authored; runs only with MESSAGE_CARRIER_TEST_{TO,FROM} env set. |
| 4.12 | `sip:refer` | `actionHook`, `eventHook` | ☑ | ☐ | local | Asserts REFER arrives at UAS with correct Refer-To. |
| 4.13 | `sip:request` | `actionHook` | ☑ | ☐ | local | Asserts INFO arrives with custom body + X-Test header. |
| 4.14 | `sip:decline` | `call_status_hook` | ⊘ | ☐ | local | Requires UAC origination — same as `alert`. Deferred. |

---

## Tier 5 — AI verbs (credential-gated)

Each row requires provider credentials; tests skip with a clear reason if absent.

| # | Verb | Creds required | Feature | Contract | Schema source | Notes |
|---|---|---|---|---|---|---|
| 5.0 | `agent` | DEEPSEEK_API_KEY (inline auth) + Deepgram (in-jambonz cred + offline STT) | ☑ | ☑ | local + upstream | 11 tests cover round-trip echo, eventHook (`user_transcript`/`llm_response`/`turn_end`), `greeting:true`, `actionHook` on end (call_sid + completion_reason + customerData), `toolHook` round-trip (function call → JSON body reply → LLM speak), `bargeIn` + `user_interruption`, `noResponseTimeout` re-prompt, `turnDetection:"krisp"`, `noiseIsolation` 3 variants (krisp/rnnoise/object). Per-test routing for hooks without customerData via `?X-Test-Id=` query param. Drift: latency.{stt_ms,eot_ms,llm_ms,tts_ms,tool_ms} + turn_end.{confidence,tool_calls} added to `schemas/callbacks/agent-turn.schema.json` with TODO upstream. |
| 5.1 | `llm` | vendor API key (OpenAI, etc.) | ☐ | ☐ | local | `eventHook`, `toolHook`, `actionHook`. Realtime connection. |
| 5.2 | `lex` | AWS access key + secret | ☐ | ☐ | local | Lex V2 session; intents + transcription. |
| 5.3 | `dialogflow` | Google service-account JSON | ☐ | ☐ | local | Welcome event; intent event; actionHook on end. |
| 5.4 | `rasa` | Rasa server URL | ☐ | ☐ | local | REST channel; prompt played; user/bot messages. |

---

## Tier 6 — Call-control PUT matrix

All variants of `PUT /Accounts/{sid}/Calls/{CallSid}`. Body shape contract-validated per variant.

| # | Action | Body key | Feature | Contract | Schema source | Notes |
|---|---|---|---|---|---|---|
| 6.1 | Redirect mid-call | `call_hook` | ☐ | ☐ | fern (calls.yaml) | |
| 6.2 | Whisper | `whisper` (verb array) | ☐ | ☐ | fern + verb schemas | |
| 6.3 | Mute / unmute | `mute_status` | ☐ | ☐ | fern | |
| 6.4 | Listen status (pause/silence/resume) | `listen_status` | ☐ | ☐ | fern | |
| 6.5 | Conference mute/hold | `conf_mute_status`, `conf_hold_status` | ☐ | ☐ | fern | |
| 6.6 | Conference participant ops (tag/coach/mute/hold) | `conferenceParticipantAction` | ☐ | ☐ | fern | |
| 6.7 | Record start/stop/pause/resume | `record` | ☐ | ☐ | fern | SIPREC vs cloud variants. |
| 6.8 | DTMF inject | `dtmf` | ☐ | ☐ | fern | |
| 6.9 | SIP in-dialog request | `sip_request` | ☐ | ☐ | fern | INFO / MESSAGE / NOTIFY. |
| 6.10 | Transcribe status | `transcribe_status` | ☐ | ☐ | fern | |
| 6.11 | Call status override | `call_status` (completed / no-answer) | ☐ | ☐ | fern | |
| 6.12 | Child call hook | `child_call_hook` | ☐ | ☐ | fern | |

---

## Tier 7 — WebSocket API

Scope: alternative to webhooks + `PUT /Calls/{sid}`. Separate test package `tests/ws/`. Same schema-validation mechanism, `async/call.yml` as source.

| # | Channel | Direction | Feature | Contract | Schema source | Notes |
|---|---|---|---|---|---|---|
| 7.1 | `callControl` session open | jambonz→us | ☐ | ☐ | fern (async) | `sessionNew`. |
| 7.2 | Verb array response | us→jambonz | ☐ | ☐ | fern (async) | `verbPayload`. |
| 7.3 | Redirect / dial / record / whisper commands | us→jambonz | ☐ | ☐ | fern (async) | Covers Tier 6 surface over WS. |
| 7.4 | Conference / listen / DTMF / TTS-stream commands | us→jambonz | ☐ | ☐ | fern (async) | |
| 7.5 | `llm` channel | bidirectional | ☐ | ☐ | fern (async) | Tool calls; events. |
| 7.6 | `tts` channel | bidirectional | ☐ | ☐ | fern (async) | Streaming TTS tokens. |

---

## Schema-source health

As of this document's creation, the gap list to fill with hand-rolled local schemas (or upstream PRs to `jambonz-fern-config`):

- Most verb action-hook payloads (`gather`, `dial`, `amd`, `recording`, `listen`, `play`, `dialogflow`, `lex`, `rasa`, `message`, `sip:refer`, `sip:request`, `redirect.statusHook`).
- 25+ verb request schemas (everything beyond `say` and `play`).
- `recording_hook`, `queue_event_hook`, `messaging_hook`, `sipRequestWithinDialogHook` body shapes.

Each gap becomes a `smoke-tester/schemas/<hook-or-verb>.json` with a top-line comment linking to the MDX page it was derived from and a `TODO: upstream to jambonz-fern-config` marker. When upstream merges the schema into `jambonz-fern-config`, the local file is deleted and the loader switches to the YAML path — this directory shrinking over time is an explicit signal of convergence.

## Related documents

- [ADR-0015](adr/0015-contract-testing.md) — contract-testing decisions.
- [ADR-0007](adr/0007-three-sip-test-modes.md) — SIP modes applied across Tiers 3–6.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — system architecture.
