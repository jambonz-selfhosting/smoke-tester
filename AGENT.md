# AGENT.md — toolbox for Claude sessions writing tests in this repo

This file is a quick-reference index of the helpers, fixtures, and conventions
this repo provides, so a Claude session can wire up a new test in minutes
without re-reading every file. Pair with [CLAUDE.md](CLAUDE.md) (mission +
non-negotiable rules) and [HANDOFF.md](HANDOFF.md) (current status).

**Decision flow when writing a new test:**

1. REST endpoint? → [tests/rest/](tests/rest/) using a `Managed*` helper.
2. Verb that just runs and ends (no callback)? → Phase-1: `placeCallTo` + inline verbs.
3. Verb with action/event hooks? → Phase-2: `claimSession` + `placeWebhookCallTo`.
4. Two-leg (caller + callee)? → `claimUAS2` + multi-leg pattern.
5. LLM/agent conversation test? → Copy `agent_test.go::TestVerb_Agent_Echo` shape.
6. Need an audio fixture? → `tts.EnsureWAV` (cached on disk) or `playFixtureURL()` (hosted).

---

## Per-test setup helpers

| Helper | Returns | When to use |
| --- | --- | --- |
| `claimUAS(t, ctx)` | `*UAS` (private SIP user, registered, inbound channel) | Every verb test that places a call. Auto-cleanup. |
| `claimUAS2(t, ctx)` | `(caller, callee *UAS)` | Multi-leg tests (`dial`, `conference`, `enqueue`). Parallelizes the two REGISTERs. |
| `claimSession(t)` | `(testID string, sess *webhook.Session)` | Phase-2 tests that need to capture webhook callbacks. |
| `provisionWebhookApp(t, ctx, suffix)` | `applicationSID string` | UAC tests dialing `sip:app-<sid>@<realm>` (e.g. `answer`, `sip:decline`). |
| `WithTimeout(t, budget)` | `context.Context` | First line of every test. Sets per-test budget + watchdog. |

## Placing calls

| Helper | Direction | Use when |
| --- | --- | --- |
| `placeCallTo(ctx, t, uas, verbs, extras...)` | Outbound from us → jambonz | Phase-1: inline `app_json` verb array, no webhook. |
| `placeWebhookCallTo(ctx, t, uas, sess, extras...)` | Outbound | Phase-2: `application_sid=webhookApp`, `tag.x_test_id` correlates webhook traffic. |
| `placeWebhookCallToNoWait(ctx, t, uas, sess)` | Outbound, returns sid only | Multi-leg: when a goroutine reads the inbound INVITE off `uas.Inbound`. |
| `uas.Stack.Invite(ctx, dest, opts)` | UAC INVITE *into* jambonz | Tests that exercise the jambonz-is-callee path (`answer`, `sip:decline`). |
| `withTimeLimit(seconds)` | `func(*CallCreate)` | Override default `time_limit` on POST /Calls. |

## Building verb scripts

| Helper | Use |
| --- | --- |
| `V("verb", "key", val, "key", val, ...)` | Build one verb as `map[string]any`. Panics on odd kv count. |
| `WithWarmup(verbs)` | Phase-1: prepends `[answer, pause 1s]` for media/DTMF stability. |
| `WithWarmupScript(script)` | Phase-2: same, for `webhook.Script`. |

## Webhook scripting (Phase-2)

| Method | Use |
| --- | --- |
| `sess.ScriptCallHook(verbs)` | Register the script jambonz fetches at call_hook. |
| `sess.ScriptActionHook(verbName, verbs)` | Reply to `/action/<verbName>` with a verb chain. |
| `sess.ScriptActionHookBody(verbName, body)` | Reply with raw bytes (e.g. tool-hook JSON). |
| `SessionURL(sess, verbName)` | Build `/action/<verb>?X-Test-Id=<id>` URL — required for hooks where jambonz drops `customerData` (eventHook, toolHook, transcribe, tag verb). |
| `SessionAckEmpty(sess, verbs...)` | Register empty `[]` action-hook responses (don't chain follow-ups). |

## Reading callbacks back

| Method | Returns | Notes |
| --- | --- | --- |
| `sess.WaitCallback(ctx)` | next callback | Blocks. |
| `sess.WaitCallbackFor(ctx, hook)` | next matching, **discards** others | DO NOT use if you also need eventHook traffic — drain first. |
| `DrainCallbacks(sess, within)` | `[]webhook.Callback` | Pulls every queued callback. Use this before any hook-by-hook filtering. |
| `cb.String("key")` / `cb.NestedString("a.b")` / `cb.NestedAny("a.b")` | typed extraction | |
| `cb.Hook` | string `"call_hook"`, `"call_status_hook"`, `"action/<verb>"` | |
| `findAgentEvents(cbs, type)` | filter by event `type` | agent verb only. |
| `statusCallbacks(t, within)` | `[]Callback` | Phase-1 status-hook drain. |

## Driving the call (UAS side)

After `placeCallTo` / `placeWebhookCallTo` returns `*jsip.Call`:

| Method | Use |
| --- | --- |
| `call.Answer()` / `call.Trying()` / `call.Ringing()` / `call.Reject(code, reason)` | UAS responses. |
| `call.SendSilence()` | Open NAT pinhole + symmetric-RTP latch. Always send before/after audio. |
| `call.SendWAV(path)` | Stream a fixture/pre-gen WAV into jambonz as the user's voice. |
| `call.SendDTMF(digits)` / `SendDTMFWithDuration` | RFC 2833 DTMF out. |
| `call.StartRecording(path)` / `call.StopRecording()` | Capture inbound audio (linear16 8kHz mono PCM). |
| `call.Hangup()` | Send BYE. |
| `call.WaitState(ctx, jsip.StateAnswered \| StateEnded)` | Block on dialog state — **prefer this over `time.Sleep`** for bridge/answer waits. |
| `call.SendInfo(ctx, contentType, body)` / `SendMessage` / `SendRefer` | In-dialog requests from us to jambonz. |

Convenience composite for "answer + record + silence + wait BYE":

```go
wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord("tag"), WithSilence())
```

## End-of-call cleanup

| Helper | Use |
| --- | --- |
| `HangupAndWaitEnded(t, ctx, call)` | BYE + drain recording. |
| `RunAudioRoundtrip(t, ctx, call, opts)` | Single-turn: answer → record → silence → arm STT → SendWAV → wait reply. Returns rec path. |

## Audio analysis (call-side)

| Method | Returns |
| --- | --- |
| `call.PCMBytesIn()` | total inbound bytes captured |
| `call.AudioDuration()` | wall-clock of recording |
| `call.RMS()` | average loudness; <50 = silent |
| `call.Codec()` | "PCMU" etc. |
| `call.ReceivedDTMF()` | `[]DTMFEvent` |
| `LongestSilenceMS(pcmPath, thresh)` | ms of longest quiet window — for SSML `<break>` checks. |

## SIP-level assertions

| Helper | Asserts |
| --- | --- |
| `RequireRecvMethods(s, call, "INVITE", "BYE")` | every named method appears in `call.Received()` |
| `RequireSentStatus(s, call, 200)` | a 200 was sent |
| `MethodsOf(call.Received())` / `StatusesOf(call.Sent())` | raw slices for custom checks |
| `call.AnsweredStatus()` / `call.EndReason()` | high-level state |
| `call.ReceivedByMethod("INFO")[0].Headers["X-Test"]` | header-level assertion |
| `call.ReceivedByMethod("INFO")[0].RawRequest.Body()` | body bytes (trim whitespace before compare; jambonz appends LF) |

## Audio-content assertions (Deepgram STT-of-recording)

Pick the strongest one your test can survive — start with strict `Contains`,
relax to `HasMost` or `KeywordCount` only when you've measured the floor:

| Helper | Semantics |
| --- | --- |
| `AssertTranscriptContains(s, ctx, wav, w1, w2, ...)` | every word must appear (strict) |
| `AssertTranscriptContainsInOrder(s, ctx, wav, w1, w2, ...)` | each word appears AFTER the previous (`say` array order) |
| `AssertTranscriptHasMost(s, ctx, wav, minHits, ws...)` | at least N of M (LLM/agent reply tests) |
| `AssertTranscriptHasAnyOf(s, ctx, wav, candidates...)` | exactly one (one-of-N tests) |
| `AssertTranscriptKeywordCount(s, ctx, wav, kw, min)` | a keyword appears ≥N times (loop tests) |
| `AssertMulawTranscriptHasMost` / `AssertMulawTranscriptContains` | same, for raw µ-law payloads (listen/stream verb's WS audio) |
| `AssertAudioDuration(s, call, min, max, tag)` | duration window + RMS sanity |
| `AssertAudioBytes(s, call, minBytes, tag)` | bytes floor + RMS |

If `DEEPGRAM_API_KEY` is unset, the transcript assertions skip cleanly with a
log line.

## Pre-generating audio fixtures

| Helper | Use |
| --- | --- |
| `tts.EnsureWAV(ctx, "testdata/<dir>", text, opts)` | Pre-gen Deepgram TTS WAV; returns path. **Cached on disk by sha** — re-runs are free. |
| `playFixtureURL()` (in `play_test.go`) | Public URL of `tests/verbs/testdata/test_audio.wav` ("The sun is shining.") served by the webhook tunnel under `/static/`. Use for `play`/`dub` verbs. |
| `playFixtureKeywords` | `["sun", "shining"]` — content words to assert against. |
| `resolveFixture(t, "test_audio.wav")` | Local fixture path for `SendWAV`. |

## Step + failure-summary tooling

Required pattern for **every** test (see [CLAUDE.md](CLAUDE.md) test-design rules):

```go
ctx := WithTimeout(t, 60*time.Second)

s := Step(t, "kebab-case-name")
if err := doThing(); err != nil {
    s.Fatalf("doThing: %v", err)   // NEVER raw t.Fatalf in tests
}
s.Done()
```

| Method | Use |
| --- | --- |
| `Step(t, name)` | open a step. Name MUST match a bullet in the test's doc-comment Steps list. |
| `s.Done()` | close it (inline, NOT via defer). |
| `s.Fatalf` / `s.Errorf` / `s.Fatal` | record failure into the end-of-run summary. |
| `s.Logf` | mid-step log without decoration. |
| `GoroutineFailf(t, label, fmt, ...)` | failure path inside a goroutine (multi-leg callee, listener, etc.). |
| `helperFatalf(t, step, fmt, ...)` | failure inside a setup helper that has no `*StepCtx` in scope. |
| `WaitFor(t, name, d)` | named sleep that emits step start/ok lines. |

## Shared timing constants

| Const | Value | Use |
| --- | --- | --- |
| `WarmupPause` | 1 (sec) | server-side `pause` injected by `WithWarmup`. |
| `RecognizerArmDelay` | 700ms | sleep after Answer+Silence before user audio (cluster STT armup). |
| `LLMReplyWindow` | 12s | wait after sending user prompt for LLM reply + TTS to flow into recording. |
| `BridgeSettleDelay` | 1500ms | for cross-leg media path stabilisation (deprecated — prefer `WaitState`). |
| `EndedDrainTimeout` | 5s | budget for `WaitState(StateEnded)`. |

## REST CRUD helpers

In [tests/rest/](tests/rest/) use the SP/account-scope `client` (built in
`restmain_test.go`). Every resource has a `Managed*(t, ctx, body)` form that
auto-registers `t.Cleanup` for delete:

```go
sid := client.ManagedApplication(t, ctx, provision.ApplicationCreate{...})
sid := client.ManagedCall(t, ctx, body)
sid := client.ManagedSIPClient(t, ctx)                  // returns sid, user, pass
sid := client.ManagedAccountSpeechCredential(...)
sid := client.ManagedVoipCarrier(...)
sid := client.ManagedSipGateway(...)
sid := client.ManagedLcr(...)
sid := client.ManagedPhoneNumber(...)
sid := client.ManagedMsTeamsTenant(...)
sid, token := client.ManagedApiKey(...)
sid := client.ManagedAccount(...)
```

Then `client.Get<Resource>(ctx, sid)` / `List<Resource>(...)` / `Update*` / `Delete*`. Every response is contract-validated automatically.

For pagination tests use `provision.RecentCallsQuery{Page, Count, Days}` /
`provision.AlertsQuery{...}`.

---

## Common test skeletons (copy-paste starters)

### Phase-1 verb test (no webhook)

```go
func TestVerb_<Name>(t *testing.T) {
    t.Parallel()
    ctx := WithTimeout(t, 30*time.Second)
    uas := claimUAS(t, ctx)

    s := Step(t, "place-call")
    call := placeCallTo(ctx, t, uas, WithWarmup([]map[string]any{
        V("<verb>", "key", "val"),
        V("hangup"),
    }))
    s.Done()

    s = Step(t, "answer-record-and-wait-end")
    wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord("<tag>"), WithSilence())
    s.Done()

    s = Step(t, "assert-content")
    AssertTranscriptContains(s, ctx, wav, "expected", "words")
    s.Done()
}
```

### Phase-2 verb test (with action/event hooks)

```go
func TestVerb_<Name>(t *testing.T) {
    t.Parallel()
    requireWebhook(t)

    ctx := WithTimeout(t, 60*time.Second)
    uas := claimUAS(t, ctx)
    _, sess := claimSession(t)

    s := Step(t, "script-verb")
    sess.ScriptCallHook(WithWarmupScript(webhook.Script{
        V("<verb>", "actionHook", SessionURL(sess, "<verb>")),
        V("hangup"),
    }))
    SessionAckEmpty(sess, "<verb>")
    s.Done()

    s = Step(t, "place-call")
    call := placeWebhookCallTo(ctx, t, uas, sess)
    s.Done()

    s = Step(t, "answer-and-wait")
    AnswerRecordAndWaitEnded(s, ctx, call, WithSilence())
    s.Done()

    s = Step(t, "assert-action-hook-fired")
    cb, err := sess.WaitCallbackFor(ctx, "action/<verb>")
    if err != nil { s.Fatalf("wait: %v", err) }
    if got := cb.NestedString("call_sid"); got == "" {
        s.Errorf("call_sid missing: %s", string(cb.Body))
    }
    s.Done()
}
```

### Multi-leg (caller speaks, callee listens)

```go
ctx := WithTimeout(t, 90*time.Second)
caller, callee := claimUAS2(t, ctx)
_, sess := claimSession(t)

// script: dial caller→callee through jambonz, OR conference/enqueue/dial verb on caller side

s := Step(t, "place-caller")
call := placeWebhookCallTo(ctx, t, caller, sess, ...)
s.Done()

answeredCh := make(chan struct{}, 1)
go func() {
    c := <-callee.Inbound
    if err := c.Answer(); err != nil {
        GoroutineFailf(t, "callee:answer", "%v", err)
        return
    }
    answeredCh <- struct{}{}
    _ = c.WaitState(ctx, jsip.StateEnded)
}()

select {
case <-answeredCh:
case <-ctx.Done(): s.Fatalf("callee never answered: %v", ctx.Err())
}
// drive caller media + assertions
```

### LLM/agent conversation test

See [tests/verbs/llm_test.go](tests/verbs/llm_test.go) and
`agent_test.go::TestVerb_Agent_Echo` — these are the strongest-assertion templates:

1. Pre-gen user prompts via `tts.EnsureWAV` (one per turn).
2. Per turn: `StartRecording → SendSilence → SendWAV → SendSilence → Sleep(LLMReplyWindow) → StopRecording`.
3. `AssertTranscriptHasMost(s, ctx, recPath, 2, contentWords(prompt)...)` per turn.
4. After all turns: `HangupAndWaitEnded` + `DrainCallbacks` + assert action/llm payload + assert event types.

Use the provided `contentWords(prompt)` helper to strip filler before assertion.

---

## Gotchas

- **Correlation drift.** Hooks where the payload doesn't carry `customerData` (eventHook, toolHook, transcribe verb, tag verb) need `SessionURL(sess, ...)` so the X-Test-Id query param routes the callback to your session. Without it, callbacks land in shared `_anon` and break under `t.Parallel()`.
- **`WaitCallbackFor` discards.** It skips non-matching callbacks. If you also want eventHook traffic, `DrainCallbacks` everything first and filter in-process.
- **First word clipping.** Always pad `RecognizerArmDelay` after `SendSilence` before `SendWAV` — STT needs ~700ms to arm.
- **NAT latch.** UAC tests must `SendSilence()` after `Answer()` to open the symmetric-RTP pinhole; otherwise jambonz's RTP can't reach you.
- **Ngrok-required tests.** Phase-2 tests `requireWebhook(t)` — skip cleanly when ngrok is down.
- **Schema-validated callbacks.** Webhook server auto-validates inbound callbacks against `schemas/callbacks/<verb>.schema.json`. A schema violation logs at error level — read `[hook=<name> err=...]` lines after a failed test.
- **One-shot Step naming.** Step names MUST match the bullet list in your test's doc comment. Ops read failures from the summary block, not from source.
- **`time.Sleep` is the last resort.** For bridge-settle / answer-wait / state changes, prefer `call.WaitState(ctx, ...)` or a goroutine signal channel.
- **Each test gets its own UAS.** Don't share `*UAS` across tests — they have private inbound channels and `t.Cleanup` lifecycle.
- **`agentEchoPrompt` vs `agentEchoTurns`.** Echo test uses multi-turn natural sentences; the others (EventHook, BargeIn, Greeting, etc.) use the legacy `agentEchoPrompt` because they assert on event payloads, not echo correctness.
